package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// chatState — что выбрано в конкретном чате. Сохраняется в файл, чтобы пережить перезапуск.
type chatState struct {
	Env        string `json:"env,omitempty"` // рантайм Orca выбранного воркспейса; "" — локальный
	WorktreeID string `json:"worktreeId"`
	Terminal   string `json:"terminal"` // handle; пусто — терминал агента выбирается автоматически
	Muted      bool   `json:"muted"`    // уведомления выключены
}

// wsRef — воркспейс в конкретном рантайме Orca.
type wsRef struct{ Env, WorktreeID string }

type Bot struct {
	tg        *tgClient
	orca      *Orca
	allowed   map[int64]bool
	agent     string // агент по умолчанию для /new и /agent
	stateFile string

	mu    sync.Mutex
	chats map[int64]*chatState
	ids   map[string]wsRef  // короткий id -> воркспейс (callback_data ограничен 64 байтами)
	terms map[string]string // короткий id -> handle терминала

	hostNames   map[string]string // hostId ("ssh:…") -> имя SSH-цели из `host list`
	machineName string            // имя этой машины из `host list`
	hostsLoaded time.Time
}

// hostLabel возвращает понятное имя удалённого хоста ("" для локального).
// Имена берутся из `host list` и кэшируются на минуту; при ошибке показываем сам id.
func (b *Bot) hostLabel(ctx context.Context, hostID string) string {
	if hostID == "" || hostID == "local" {
		return ""
	}
	b.mu.Lock()
	stale := time.Since(b.hostsLoaded) > time.Minute
	b.mu.Unlock()
	if stale {
		if hosts, err := b.orca.Hosts(ctx); err == nil {
			names := map[string]string{}
			machine := ""
			for _, h := range hosts {
				if h.Kind == "local" && h.MachineName != "" {
					machine = strings.ReplaceAll(h.MachineName, "\u00a0", " ")
				}
				if h.Name == "" {
					continue
				}
				for _, k := range []string{h.Selector, h.ID, h.Kind + ":" + h.ID} {
					if k != "" {
						names[k] = h.Name
					}
				}
				if h.Kind == "ssh" {
					names["ssh:"+h.ID] = h.Name
				}
			}
			b.mu.Lock()
			b.hostNames, b.machineName, b.hostsLoaded = names, machine, time.Now()
			b.mu.Unlock()
		} else {
			// Не дёргаем CLI на каждое сообщение, если host list недоступен.
			b.mu.Lock()
			b.hostsLoaded = time.Now()
			b.mu.Unlock()
		}
	}
	b.mu.Lock()
	name := b.hostNames[hostID]
	b.mu.Unlock()
	if name != "" {
		return name
	}
	_, rest, _ := strings.Cut(hostID, ":")
	return rest
}

// whereText — «repo · branch» плюс хост для SSH/удалённых воркtree.
func (b *Bot) whereText(ctx context.Context, w Workspace) string {
	s := esc(w.Repo)
	if br := w.ShortBranch(); br != "" {
		s += " · <code>" + esc(br) + "</code>"
	}
	if h := b.wsHost(ctx, w); h != "" {
		s += " · 🌐 " + esc(h)
	}
	return s
}

// wsHost — где живёт воркспейс: имя сервера Orca и/или SSH-цели; "" для этой машины.
func (b *Bot) wsHost(ctx context.Context, w Workspace) string {
	if w.Env != "" {
		// SSH-цели удалённого рантайма по имени не разрешаем: показываем id.
		if w.HostID != "" && w.HostID != "local" {
			_, rest, _ := strings.Cut(w.HostID, ":")
			return w.EnvName + "/" + rest
		}
		return w.EnvName
	}
	return b.hostLabel(ctx, w.HostID)
}

// noTermText объясняет, почему писать некуда: нет терминалов или потеряна связь с хостом.
func (b *Bot) noTermText(ctx context.Context, w Workspace, term *Terminal) string {
	if w.UnverifiableTerminalCount > 0 || (term != nil && term.Verdict == "unverifiable") {
		h := b.wsHost(ctx, w)
		if h == "" {
			h = "хостом"
		} else {
			h = "хостом «" + esc(h) + "»"
		}
		return "🔌 Связь с " + h + " потеряна. Агент там, скорее всего, продолжает работать; Orca переподключится сама. Попробуйте позже."
	}
	return "⚠️ В «" + esc(w.Name()) + "» нет терминала для ввода. Запустить агента: /agent"
}

func NewBot(tg *tgClient, orca *Orca, allowed map[int64]bool, agent, stateFile string) *Bot {
	b := &Bot{
		tg: tg, orca: orca, allowed: allowed, agent: agent, stateFile: stateFile,
		chats: map[int64]*chatState{}, ids: map[string]wsRef{}, terms: map[string]string{},
	}
	b.loadState()
	return b
}

// ---------- состояние ----------

func (b *Bot) loadState() {
	if b.stateFile == "" {
		return
	}
	data, err := os.ReadFile(b.stateFile)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &b.chats); err != nil {
		log.Printf("state: не удалось прочитать %s: %v", b.stateFile, err)
	}
}

// saveState вызывается под b.mu.
func (b *Bot) saveState() {
	if b.stateFile == "" {
		return
	}
	data, _ := json.MarshalIndent(b.chats, "", "  ")
	tmp := b.stateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("state: запись: %v", err)
		return
	}
	_ = os.Rename(tmp, b.stateFile)
}

func (b *Bot) chat(id int64) chatState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.chats[id]; c != nil {
		return *c
	}
	return chatState{}
}

func (b *Bot) updateChat(id int64, fn func(*chatState)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.chats[id]
	if c == nil {
		c = &chatState{}
		b.chats[id] = c
	}
	fn(c)
	b.saveState()
}

func shortID(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:5])
}

func (b *Bot) wsKey(w Workspace) string {
	k := shortID(w.Key())
	b.mu.Lock()
	b.ids[k] = wsRef{Env: w.Env, WorktreeID: w.WorktreeID}
	b.mu.Unlock()
	return k
}

func (b *Bot) termKey(handle string) string {
	k := shortID(handle)
	b.mu.Lock()
	b.terms[k] = handle
	b.mu.Unlock()
	return k
}

func (b *Bot) lookupWS(k string) (wsRef, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.ids[k]
	return v, ok
}

func (b *Bot) lookup(m map[string]string, k string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := m[k]
	return v, ok
}

// ---------- маршрутизация ----------

func (b *Bot) HandleUpdate(ctx context.Context, u Update) {
	switch {
	case u.Message != nil && u.Message.From != nil:
		if !b.allowed[u.Message.From.ID] {
			log.Printf("игнор: сообщение от чужого пользователя id=%d @%s", u.Message.From.ID, u.Message.From.Username)
			return
		}
		b.handleMessage(ctx, u.Message)
	case u.CallbackQuery != nil:
		if !b.allowed[u.CallbackQuery.From.ID] {
			b.tg.answerCallback(ctx, u.CallbackQuery.ID, "Нет доступа")
			return
		}
		b.handleCallback(ctx, u.CallbackQuery)
	}
}

func (b *Bot) handleMessage(ctx context.Context, m *Message) {
	text := strings.TrimSpace(m.Text)
	chatID := m.Chat.ID
	if text == "" {
		return
	}
	if !strings.HasPrefix(text, "/") {
		b.sendToAgent(ctx, chatID, text)
		return
	}
	cmd, args, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@") // /ws@my_bot -> /ws
	args = strings.TrimSpace(args)

	switch cmd {
	case "/start", "/help":
		b.reply(ctx, chatID, helpText, nil)
	case "/ws":
		b.listWorkspaces(ctx, chatID)
	case "/cur":
		b.showCurrent(ctx, chatID)
	case "/read":
		n, _ := strconv.Atoi(args)
		b.showScreen(ctx, chatID, n)
	case "/stop":
		b.interrupt(ctx, chatID)
	case "/term":
		b.listTerminals(ctx, chatID)
	case "/repos":
		b.listRepos(ctx, chatID)
	case "/new":
		b.newWorktree(ctx, chatID, args)
	case "/agent":
		b.launchAgent(ctx, chatID, args)
	case "/send":
		// Для текста, который сам начинается с «/», например slash-команд Claude Code.
		if args == "" {
			b.reply(ctx, chatID, "Использование: <code>/send текст</code>", nil)
			return
		}
		b.sendToAgent(ctx, chatID, args)
	case "/notify":
		b.setNotify(ctx, chatID, args)
	default:
		b.reply(ctx, chatID, "Неизвестная команда. /help — список команд.\nЧтобы отправить агенту текст со слешем в начале, используйте <code>/send /команда</code>.", nil)
	}
}

func (b *Bot) handleCallback(ctx context.Context, q *CallbackQuery) {
	if q.Message == nil {
		return
	}
	chatID := q.Message.Chat.ID
	kind, key, _ := strings.Cut(q.Data, ":")
	switch kind {
	case "sel", "scr", "stp", "rfr":
		ref, ok := b.lookupWS(key)
		if !ok {
			b.tg.answerCallback(ctx, q.ID, "Кнопка устарела, обновите /ws")
			return
		}
		b.updateChat(chatID, func(c *chatState) {
			if c.WorktreeID != ref.WorktreeID || c.Env != ref.Env {
				c.Terminal = ""
			}
			c.Env, c.WorktreeID = ref.Env, ref.WorktreeID
		})
		if kind == "rfr" {
			b.refreshScreen(ctx, q)
			return
		}
		b.tg.answerCallback(ctx, q.ID, "")
		switch kind {
		case "sel":
			b.showCurrent(ctx, chatID)
		case "scr":
			b.showScreen(ctx, chatID, 0)
		case "stp":
			b.interrupt(ctx, chatID)
		}
	case "trm":
		handle, ok := b.lookup(b.terms, key)
		if !ok {
			b.tg.answerCallback(ctx, q.ID, "Кнопка устарела, обновите /term")
			return
		}
		b.updateChat(chatID, func(c *chatState) { c.Terminal = handle })
		b.tg.answerCallback(ctx, q.ID, "Терминал выбран")
		b.showCurrent(ctx, chatID)
	default:
		b.tg.answerCallback(ctx, q.ID, "")
	}
}

// ---------- команды ----------

const helpText = `<b>Orca из Telegram</b>

/ws — воркспейсы и их агенты, выбор кнопкой
/cur — что сейчас выбрано
<i>любой текст</i> — отправить выбранному агенту (Enter нажимается сам)
/send <i>текст</i> — отправить текст, начинающийся с «/»
/read [N] — экран терминала агента (последние N строк, по умолчанию весь экран)
/stop — прервать агента (Ctrl+C)
/term — выбрать конкретный терминал в воркспейсе
/repos — репозитории
/new <i>репо имя [задача]</i> — новый воркtree с агентом
/agent [<i>агент</i>] [<i>задача</i>] — ещё один агент в выбранном воркспейсе
/notify on|off — уведомления о завершении работы агентов`

func (b *Bot) reply(ctx context.Context, chatID int64, text string, kb *Keyboard) {
	if err := b.tg.send(ctx, chatID, text, kb); err != nil {
		log.Printf("telegram send: %v", err)
	}
}

func (b *Bot) replyErr(ctx context.Context, chatID int64, what string, err error) {
	b.reply(ctx, chatID, "❌ "+esc(what)+": <code>"+esc(truncRunes(err.Error(), 800))+"</code>", nil)
}

func agentStateRu(s string) string {
	switch s {
	case "working":
		return "работает"
	case "blocked":
		return "ждёт разрешения"
	case "waiting":
		return "ждёт ввода"
	case "done":
		return "закончил"
	}
	return s
}

// wsGroup — машина в списке /ws: эта машина, её SSH-цель или сервер Orca.
type wsGroup struct {
	key, title string
	order      int // 0 — эта машина, 1 — её SSH-цели, 2 — серверы Orca
	items      []Workspace
	err        error // сервер не ответил
}

func (b *Bot) groupOf(ctx context.Context, w Workspace) (key, title string, order int) {
	switch {
	case w.Env != "":
		return "env:" + w.Env + "|" + w.HostID, "🌐 " + esc(b.wsHost(ctx, w)), 2
	case w.HostID == "" || w.HostID == "local":
		name := "Эта машина"
		b.mu.Lock()
		if b.machineName != "" {
			name = b.machineName
		}
		b.mu.Unlock()
		return "local", "💻 " + esc(name), 0
	}
	return w.HostID, "🌐 " + esc(b.hostLabel(ctx, w.HostID)), 1
}

// wsState — состояние воркспейса для человека. Опирается прежде всего на агента:
// статус воркспейса «active» значит лишь «открыт терминал», а агент там может ждать ответа.
type wsState struct {
	rank  int    // порядок в списке: что требует внимания — выше
	emoji string // значок
	label string // для сводки: «1 работает»
}

var (
	stPermission = wsState{0, "⚠️", "ждут разрешения"}
	stWorking    = wsState{1, "⏳", "в работе"}
	stReady      = wsState{2, "✅", "ждут ответа"}
	stOpen       = wsState{3, "🟢", "открыты"}
	stIdle       = wsState{4, "💤", "спят"}
)

func stateOf(w Workspace) wsState {
	a := w.MainAgent()
	switch {
	case w.Status == "permission" || (a != nil && a.State == "blocked"):
		return stPermission
	case w.Status == "working" || (a != nil && a.State == "working"):
		return stWorking
	case w.Status == "done" || (a != nil && (a.State == "done" || a.State == "waiting")):
		return stReady
	case w.Status == "active":
		return stOpen
	}
	return stIdle
}

// ago — «5 мин назад» по времени в миллисекундах.
func ago(ms int64) string {
	if ms <= 0 {
		return ""
	}
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	}
	return fmt.Sprintf("%d дн назад", int(d.Hours()/24))
}

// agentLine — строка под воркспейсом: что делает агент. "" если агента нет.
func (b *Bot) agentLine(w Workspace) string {
	a := w.MainAgent()
	if a == nil {
		return ""
	}
	var parts []string
	// Тип агента пишем, только если он не тот, что по умолчанию: иначе это шум.
	if a.AgentType != nil && *a.AgentType != "" && *a.AgentType != b.agent {
		parts = append(parts, esc(*a.AgentType))
	}
	parts = append(parts, agentStateRu(a.State))
	if (a.State == "working" || a.State == "blocked") && a.ToolName != nil && *a.ToolName != "" {
		parts = append(parts, esc(*a.ToolName))
	} else if t := ago(a.UpdatedAt); t != "" && a.State != "working" {
		parts = append(parts, t)
	}
	if extra := len(w.Agents) - 1; extra > 0 {
		parts = append(parts, fmt.Sprintf("+%d агент.", extra))
	}
	return strings.Join(parts, " · ")
}

// subIndent — отступ под имя воркспейса: em-space + en-space ≈ ширина значка с пробелом.
const subIndent = "  "

func (b *Bot) listWorkspaces(ctx context.Context, chatID int64) {
	all, failed := b.orca.PSAll(ctx)
	if len(all) == 0 && len(failed) > 0 && failed[0].Target.Env == "" {
		b.replyErr(ctx, chatID, "worktree ps", failed[0].Err)
		return
	}
	b.hostLabel(ctx, "ssh:") // подгрузить имена машин из `host list`

	groups := map[string]*wsGroup{}
	var order []*wsGroup
	add := func(key, title string, ord int) *wsGroup {
		if g := groups[key]; g != nil {
			return g
		}
		g := &wsGroup{key: key, title: title, order: ord}
		groups[key] = g
		order = append(order, g)
		return g
	}
	counts := map[wsState]int{}
	total := 0
	for _, w := range all {
		if w.IsArchived {
			continue
		}
		total++
		counts[stateOf(w)]++
		k, t, o := b.groupOf(ctx, w)
		g := add(k, t, o)
		g.items = append(g.items, w)
	}
	for _, f := range failed {
		add("env:"+f.Target.Env+"|err", "🌐 "+esc(f.Target.Name), 2).err = f.Err
	}
	if total == 0 && len(failed) == 0 {
		b.reply(ctx, chatID, "Воркспейсов нет. Создать: /new", nil)
		return
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].order != order[j].order {
			return order[i].order < order[j].order
		}
		return order[i].title < order[j].title
	})

	st := b.chat(chatID)
	cur := wsRefKey(st.Env, st.WorktreeID)

	var sb strings.Builder
	// Сводка: сначала то, что требует внимания.
	sb.WriteString("<b>Воркспейсы</b>\n")
	var summary []string
	for _, s := range []wsState{stPermission, stWorking, stReady, stOpen, stIdle} {
		if n := counts[s]; n > 0 {
			summary = append(summary, fmt.Sprintf("%s %d %s", s.emoji, n, s.label))
		}
	}
	sb.WriteString(strings.Join(summary, " · ") + "\n")

	const maxItems = 30
	shown := 0
	var buttons []Button
	for _, g := range order {
		sb.WriteString("\n<b>" + g.title + "</b>\n")
		var lines []string
		if g.err != nil {
			lines = append(lines, "🔌 не отвечает", subIndent+"<i>"+esc(truncRunes(g.err.Error(), 120))+"</i>")
		}
		sort.SliceStable(g.items, func(i, j int) bool {
			ri, rj := stateOf(g.items[i]).rank, stateOf(g.items[j]).rank
			if ri != rj {
				return ri < rj
			}
			return g.items[i].LastActivityAt > g.items[j].LastActivityAt
		})
		var idle []Workspace
		for _, w := range g.items {
			s := stateOf(w)
			if s == stIdle && w.Key() != cur {
				idle = append(idle, w)
				continue
			}
			if shown == maxItems {
				continue
			}
			shown++
			line := s.emoji + " <b>" + esc(w.Name()) + "</b>"
			if br := w.ShortBranch(); br != "" && br != w.Name() && !(w.IsMainWorktree && (br == "main" || br == "master")) {
				line += " <code>" + esc(br) + "</code>"
			}
			if w.Key() == cur {
				line += " 👈"
			}
			lines = append(lines, line)
			if al := b.agentLine(w); al != "" {
				lines = append(lines, subIndent+"<i>"+al+"</i>")
			}
			if w.UnverifiableTerminalCount > 0 {
				lines = append(lines, subIndent+"🔌 <i>нет связи с хостом</i>")
			}
			if w.Comment != "" {
				lines = append(lines, subIndent+"💬 <i>"+esc(truncRunes(w.Comment, 100))+"</i>")
			}
			buttons = append(buttons, b.wsButton(w, cur))
		}
		if len(idle) > 0 {
			names := make([]string, len(idle))
			for i, w := range idle {
				names[i] = esc(w.Name())
				buttons = append(buttons, b.wsButton(w, cur))
			}
			lines = append(lines, stIdle.emoji+" "+strings.Join(names, " · "))
		}
		if len(lines) == 0 {
			lines = append(lines, "<i>пусто</i>")
		}
		sb.WriteString("<blockquote>" + strings.Join(lines, "\n") + "</blockquote>")
	}
	if total > shown+countIdle(all, cur) {
		fmt.Fprintf(&sb, "\n…показаны не все: %d из %d", shown, total)
	}

	var rows [][]Button
	for i := 0; i < len(buttons); i += 2 {
		rows = append(rows, buttons[i:min(i+2, len(buttons))])
	}
	b.reply(ctx, chatID, sb.String(), &Keyboard{InlineKeyboard: rows})
}

func countIdle(all []Workspace, cur string) int {
	n := 0
	for _, w := range all {
		if !w.IsArchived && stateOf(w) == stIdle && w.Key() != cur {
			n++
		}
	}
	return n
}

// wsButton — кнопка выбора воркспейса; у удалённых — имя машины, чтобы не путать одноимённые.
func (b *Bot) wsButton(w Workspace, cur string) Button {
	text := stateOf(w).emoji + " " + truncRunes(w.Name(), 24)
	if w.Env != "" {
		text += " · " + truncRunes(w.EnvName, 10)
	}
	if w.Key() == cur {
		text = "👉 " + text
	}
	return Button{Text: text, CallbackData: "sel:" + b.wsKey(w)}
}

// resolve находит выбранный воркспейс и терминал, в который надо писать.
func (b *Bot) resolve(ctx context.Context, chatID int64) (*Workspace, *Terminal, []Terminal, error) {
	st := b.chat(chatID)
	if st.WorktreeID == "" {
		return nil, nil, nil, fmt.Errorf("воркспейс не выбран — откройте /ws")
	}
	all, err := b.orca.PS(ctx, st.Env)
	if err != nil {
		if st.Env != "" {
			return nil, nil, nil, fmt.Errorf("сервер Orca «%s» не отвечает: %w", b.orca.TargetName(ctx, st.Env), err)
		}
		return nil, nil, nil, err
	}
	var ws *Workspace
	for i := range all {
		if all[i].WorktreeID == st.WorktreeID {
			ws = &all[i]
			break
		}
	}
	if ws == nil {
		return nil, nil, nil, fmt.Errorf("выбранный воркспейс больше не найден — откройте /ws")
	}
	terms, err := b.orca.Terminals(ctx, ws.Env, ws.WorktreeID)
	if err != nil {
		return ws, nil, nil, err
	}
	if st.Terminal != "" {
		for i := range terms {
			if terms[i].Handle == st.Terminal {
				return ws, &terms[i], terms, nil
			}
		}
		// Хэндлы живут в пределах рантайма: после перезапуска Orca выбор сбрасывается.
		b.updateChat(chatID, func(c *chatState) { c.Terminal = "" })
	}
	return ws, PickAgentTerminal(*ws, terms), terms, nil
}

func (b *Bot) wsButtons(ws *Workspace) *Keyboard {
	k := b.wsKey(*ws)
	return &Keyboard{InlineKeyboard: [][]Button{{
		{Text: "🖥 Экран", CallbackData: "scr:" + k},
		{Text: "⏹ Стоп", CallbackData: "stp:" + k},
	}}}
}

// screenButtons — кнопки под экраном: «Обновить» перерисовывает это же сообщение.
func (b *Bot) screenButtons(ws *Workspace) *Keyboard {
	k := b.wsKey(*ws)
	return &Keyboard{InlineKeyboard: [][]Button{{
		{Text: "🔄 Обновить", CallbackData: "rfr:" + k},
		{Text: "⏹ Стоп", CallbackData: "stp:" + k},
	}}}
}

func (b *Bot) showCurrent(ctx context.Context, chatID int64) {
	ws, term, _, err := b.resolve(ctx, chatID)
	if err != nil {
		b.replyErr(ctx, chatID, "выбор", err)
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s <b>%s</b>\n%s\n", stateOf(*ws).emoji, esc(ws.Name()), b.whereText(ctx, *ws))
	if ws.Comment != "" {
		fmt.Fprintf(&sb, "💬 %s\n", esc(truncRunes(ws.Comment, 300)))
	}
	if term == nil {
		sb.WriteString("\n" + b.noTermText(ctx, *ws, term))
		b.reply(ctx, chatID, sb.String(), nil)
		return
	}
	fmt.Fprintf(&sb, "🖥 Терминал: <code>%s</code>\n", esc(term.Label()))
	if a := ws.MainAgent(); a != nil {
		fmt.Fprintf(&sb, "🤖 %s\n", agentStateRu(a.State))
		if a.LastAssistantMessage != nil && *a.LastAssistantMessage != "" {
			fmt.Fprintf(&sb, "<blockquote expandable>%s</blockquote>\n", esc(truncRunes(*a.LastAssistantMessage, 2500)))
		}
	}
	sb.WriteString("\nПишите сообщения — они уйдут этому агенту.")
	b.reply(ctx, chatID, sb.String(), b.wsButtons(ws))
}

func (b *Bot) sendToAgent(ctx context.Context, chatID int64, text string) {
	ws, term, _, err := b.resolve(ctx, chatID)
	if err != nil {
		b.replyErr(ctx, chatID, "отправка", err)
		return
	}
	if term == nil || term.Verdict == "unverifiable" {
		b.reply(ctx, chatID, b.noTermText(ctx, *ws, term), nil)
		return
	}
	if err := b.orca.Send(ctx, ws.Env, term.Handle, text); err != nil {
		b.replyErr(ctx, chatID, "terminal send", err)
		return
	}
	b.reply(ctx, chatID, "✉️ Отправлено → <b>"+esc(ws.Name())+"</b>", b.wsButtons(ws))
}

// screenMessage собирает сообщение с экраном терминала: заголовок, время снимка и <pre>.
func (b *Bot) screenMessage(ctx context.Context, chatID int64, n int) (string, *Keyboard, error) {
	ws, term, _, err := b.resolve(ctx, chatID)
	if err != nil {
		return "", nil, err
	}
	if term == nil {
		return b.noTermText(ctx, *ws, term), nil, nil
	}
	lines, rows, err := b.orca.Screen(ctx, ws.Env, term.Handle)
	if err != nil {
		return "", nil, fmt.Errorf("terminal read: %w", err)
	}
	body := formatScreen(lines, rows, n, 3500)
	if body == "" {
		body = "(пусто)"
	}
	head := stateOf(*ws).emoji + " <b>" + esc(ws.Name()) + "</b>"
	if label := term.Label(); label != "" && label != ws.Name() {
		head += " · " + esc(truncRunes(label, 40))
	}
	head += " · <i>" + time.Now().Format("15:04:05") + "</i>"
	return head + "\n<pre>" + esc(body) + "</pre>", b.screenButtons(ws), nil
}

func (b *Bot) showScreen(ctx context.Context, chatID int64, n int) {
	text, kb, err := b.screenMessage(ctx, chatID, n)
	if err != nil {
		b.replyErr(ctx, chatID, "экран", err)
		return
	}
	b.reply(ctx, chatID, text, kb)
}

// refreshScreen перерисовывает сообщение с экраном, на котором нажали «Обновить».
func (b *Bot) refreshScreen(ctx context.Context, q *CallbackQuery) {
	chatID := q.Message.Chat.ID
	text, kb, err := b.screenMessage(ctx, chatID, 0)
	if err != nil {
		b.tg.answerCallback(ctx, q.ID, truncRunes("Ошибка: "+err.Error(), 190))
		return
	}
	if err := b.tg.edit(ctx, chatID, q.Message.MessageID, text, kb); err != nil {
		if strings.Contains(err.Error(), "message is not modified") {
			b.tg.answerCallback(ctx, q.ID, "Без изменений")
			return
		}
		// Сообщение слишком старое или удалено — шлём новое.
		log.Printf("editMessageText: %v", err)
		b.reply(ctx, chatID, text, kb)
	}
	b.tg.answerCallback(ctx, q.ID, "Обновлено")
}

func (b *Bot) interrupt(ctx context.Context, chatID int64) {
	ws, term, _, err := b.resolve(ctx, chatID)
	if err != nil {
		b.replyErr(ctx, chatID, "стоп", err)
		return
	}
	if term == nil || term.Verdict == "unverifiable" {
		b.reply(ctx, chatID, b.noTermText(ctx, *ws, term), nil)
		return
	}
	if err := b.orca.Interrupt(ctx, ws.Env, term.Handle); err != nil {
		b.replyErr(ctx, chatID, "interrupt", err)
		return
	}
	b.reply(ctx, chatID, "⏹ Прерывание отправлено → <b>"+esc(ws.Name())+"</b>", b.wsButtons(ws))
}

func (b *Bot) listTerminals(ctx context.Context, chatID int64) {
	ws, cur, terms, err := b.resolve(ctx, chatID)
	if err != nil {
		b.replyErr(ctx, chatID, "терминалы", err)
		return
	}
	if len(terms) == 0 {
		b.reply(ctx, chatID, "В «"+esc(ws.Name())+"» нет терминалов.", nil)
		return
	}
	var sb strings.Builder
	var rows [][]Button
	fmt.Fprintf(&sb, "Терминалы в <b>%s</b>:\n", esc(ws.Name()))
	for _, t := range terms {
		mark := "▫️"
		if cur != nil && t.Handle == cur.Handle {
			mark = "👉"
		}
		flags := ""
		if t.AgentIdentity != "" {
			flags += " 🤖" + esc(t.AgentIdentity)
		}
		if !t.Writable {
			flags += " (только чтение)"
		}
		fmt.Fprintf(&sb, "%s %s%s\n", mark, esc(t.Label()), flags)
		if t.Writable {
			rows = append(rows, []Button{{Text: truncRunes(t.Label(), 40), CallbackData: "trm:" + b.termKey(t.Handle)}})
		}
	}
	b.reply(ctx, chatID, sb.String(), &Keyboard{InlineKeyboard: rows})
}

func (b *Bot) listRepos(ctx context.Context, chatID int64) {
	repos, failed := b.orca.ReposAll(ctx)
	var sb strings.Builder
	for _, f := range failed {
		fmt.Fprintf(&sb, "🔌 %s не отвечает: <code>%s</code>\n", esc(targetLabel(f.Target)), esc(truncRunes(f.Err.Error(), 200)))
	}
	if len(repos) == 0 {
		b.reply(ctx, chatID, sb.String()+"Репозиториев нет — добавьте их в Orca.", nil)
		return
	}
	sb.WriteString("Репозитории (имя для /new):\n")
	for _, r := range repos {
		where := ""
		if r.Env != "" {
			where = " 🌐 " + esc(r.EnvName)
		}
		fmt.Fprintf(&sb, "• <code>%s</code>%s — %s\n", esc(repoName(r)), where, esc(r.Path))
	}
	if hasDupNames(repos) {
		sb.WriteString("\nЕсли имя есть на нескольких серверах, укажите сервер: <code>/new сервер/репо …</code>")
	}
	b.reply(ctx, chatID, sb.String(), nil)
}

func hasDupNames(repos []Repo) bool {
	seen := map[string]bool{}
	for _, r := range repos {
		n := strings.ToLower(repoName(r))
		if seen[n] {
			return true
		}
		seen[n] = true
	}
	return false
}

// targetLabel — имя рантайма для сообщений.
func targetLabel(t Target) string {
	if t.Env == "" {
		return "Orca на этой машине"
	}
	return "Orca «" + t.Name + "»"
}

func repoName(r Repo) string {
	if r.DisplayName != "" {
		return r.DisplayName
	}
	return filepath.Base(r.Path)
}

// findRepo ищет репозиторий по имени; «сервер/имя» ограничивает поиск сервером Orca.
func findRepo(repos []Repo, q string) (*Repo, error) {
	if srv, name, ok := strings.Cut(q, "/"); ok && srv != "" && name != "" {
		var sub []Repo
		for _, r := range repos {
			if strings.EqualFold(r.EnvName, srv) || (r.Env == "" && strings.EqualFold(srv, "local")) {
				sub = append(sub, r)
			}
		}
		if len(sub) > 0 {
			return findRepo(sub, name)
		}
	}
	ql := strings.ToLower(q)
	var exact, hits []*Repo
	for i := range repos {
		r := &repos[i]
		if strings.ToLower(repoName(*r)) == ql || strings.ToLower(filepath.Base(r.Path)) == ql || r.ID == q {
			exact = append(exact, r)
			continue
		}
		if strings.Contains(strings.ToLower(repoName(*r)), ql) {
			hits = append(hits, r)
		}
	}
	if len(exact) > 0 {
		hits = exact
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("репозиторий «%s» не найден, см. /repos", q)
	}
	return nil, fmt.Errorf("«%s» подходит к нескольким репозиториям, уточните (/repos; можно «сервер/репо»)", q)
}

// /new <репо> <имя> [задача]
func (b *Bot) newWorktree(ctx context.Context, chatID int64, args string) {
	f := strings.Fields(args)
	if len(f) < 2 {
		b.reply(ctx, chatID, "Использование: <code>/new репо имя-ветки [задача для агента]</code>\nСписок репозиториев: /repos", nil)
		return
	}
	repos, failed := b.orca.ReposAll(ctx)
	if len(repos) == 0 && len(failed) > 0 {
		b.replyErr(ctx, chatID, "repo list", failed[0].Err)
		return
	}
	repo, err := findRepo(repos, f[0])
	if err != nil {
		b.replyErr(ctx, chatID, "/new", err)
		return
	}
	name := f[1]
	prompt := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(args, f[0])), name))
	b.reply(ctx, chatID, "⏳ Создаю <b>"+esc(name)+"</b> в "+esc(repoName(*repo))+"…", nil)
	wtID, err := b.orca.CreateWorktree(ctx, repo.Env, repo.ID, name, b.agent, prompt)
	if err != nil {
		b.replyErr(ctx, chatID, "worktree create", err)
		return
	}
	if wtID != "" {
		b.updateChat(chatID, func(c *chatState) { c.Env, c.WorktreeID, c.Terminal = repo.Env, wtID, "" })
	}
	b.reply(ctx, chatID, "✅ Воркtree создан, агент "+esc(b.agent)+" запущен и выбран. /cur — подробности.", nil)
}

var knownAgents = map[string]bool{
	"claude": true, "codex": true, "opencode": true, "gemini": true, "grok": true,
	"pi": true, "cursor": true, "copilot": true, "amp": true, "qwen": true,
}

// /agent [агент] [задача]
func (b *Bot) launchAgent(ctx context.Context, chatID int64, args string) {
	st := b.chat(chatID)
	if st.WorktreeID == "" {
		b.reply(ctx, chatID, "Сначала выберите воркспейс: /ws", nil)
		return
	}
	agent, prompt := b.agent, args
	if first, rest, _ := strings.Cut(args, " "); knownAgents[strings.ToLower(first)] {
		agent, prompt = strings.ToLower(first), strings.TrimSpace(rest)
	}
	if err := b.orca.LaunchAgent(ctx, st.Env, st.WorktreeID, agent, prompt); err != nil {
		b.replyErr(ctx, chatID, "agent launch", err)
		return
	}
	// Новый агент — новый терминал; пусть бот выберет его заново автоматически.
	b.updateChat(chatID, func(c *chatState) { c.Terminal = "" })
	b.reply(ctx, chatID, "🚀 Агент "+esc(agent)+" запущен. Если ввод должен уйти именно ему — выберите его в /term.", nil)
}

func (b *Bot) setNotify(ctx context.Context, chatID int64, args string) {
	switch strings.ToLower(args) {
	case "on", "вкл":
		b.updateChat(chatID, func(c *chatState) { c.Muted = false })
		b.reply(ctx, chatID, "🔔 Уведомления включены", nil)
	case "off", "выкл":
		b.updateChat(chatID, func(c *chatState) { c.Muted = true })
		b.reply(ctx, chatID, "🔕 Уведомления выключены", nil)
	default:
		state := "включены"
		if b.chat(chatID).Muted {
			state = "выключены"
		}
		b.reply(ctx, chatID, "Уведомления сейчас "+state+". <code>/notify on</code> или <code>/notify off</code>", nil)
	}
}

// ---------- уведомления ----------

// Watch опрашивает worktree ps во всех рантаймах и сообщает, когда агент закончил работу или ждёт разрешения.
func (b *Bot) Watch(ctx context.Context, interval time.Duration) {
	prev := map[string]string{}    // Workspace.Key() -> статус
	prevEnv := map[string]string{} // Workspace.Key() -> рантайм
	lastNotified := map[string]time.Time{}
	fails := map[string]int{}  // рантайм -> сбоев подряд
	down := map[string]bool{}  // рантайм -> уже сообщили, что не отвечает
	known := map[string]bool{} // рантаймы, у которых уже был первый снимок
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		list, failed := b.orca.PSAll(ctx)
		failedEnv := map[string]bool{}
		for _, f := range failed {
			env := f.Target.Env
			failedEnv[env] = true
			fails[env]++
			log.Printf("watch %s: %v", targetLabel(f.Target), f.Err)
			// Не шумим из-за единичного сбоя: сообщаем после трёх подряд.
			if fails[env] == 3 && !down[env] {
				down[env] = true
				b.broadcast(ctx, "🔌 "+esc(targetLabel(f.Target))+" не отвечает: <code>"+esc(truncRunes(f.Err.Error(), 500))+"</code>", nil)
			}
		}
		for _, tg := range b.orca.Targets(ctx) {
			if failedEnv[tg.Env] {
				continue
			}
			if down[tg.Env] {
				b.broadcast(ctx, "🔌 "+esc(targetLabel(tg))+" снова на связи", nil)
			}
			fails[tg.Env], down[tg.Env] = 0, false
		}

		cur := map[string]string{}
		curEnv := map[string]string{}
		// Воркспейсы недоступных рантаймов помним как были, чтобы не пропустить переход после восстановления связи.
		for k, st := range prev {
			if failedEnv[prevEnv[k]] {
				cur[k], curEnv[k] = st, prevEnv[k]
			}
		}
		for _, w := range list {
			k := w.Key()
			cur[k], curEnv[k] = w.Status, w.Env
			p, seen := prev[k]
			if !known[w.Env] || !seen || p == w.Status {
				continue
			}
			switch {
			case w.Status == "permission":
				b.notify(ctx, w, "⚠️ <b>%s</b> ждёт разрешения")
			case p == "working" && w.Status != "working":
				// Защита от дребезга статуса между шагами агента.
				if time.Since(lastNotified[k]) < 20*time.Second {
					continue
				}
				lastNotified[k] = time.Now()
				b.notify(ctx, w, "✅ <b>%s</b>: агент закончил")
			}
		}
		for _, tg := range b.orca.Targets(ctx) {
			if !failedEnv[tg.Env] {
				known[tg.Env] = true
			}
		}
		prev, prevEnv = cur, curEnv
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *Bot) notify(ctx context.Context, w Workspace, headFmt string) {
	var sb strings.Builder
	fmt.Fprintf(&sb, headFmt, esc(w.Name()))
	sb.WriteString("\n" + b.whereText(ctx, w))
	if a := w.MainAgent(); a != nil {
		if w.Status == "permission" && a.ToolName != nil && *a.ToolName != "" {
			fmt.Fprintf(&sb, "\n🔧 %s", esc(*a.ToolName))
		}
		if a.LastAssistantMessage != nil && *a.LastAssistantMessage != "" {
			fmt.Fprintf(&sb, "\n<blockquote expandable>%s</blockquote>", esc(truncRunes(*a.LastAssistantMessage, 2500)))
		}
	}
	k := b.wsKey(w)
	kb := &Keyboard{InlineKeyboard: [][]Button{{
		{Text: "💬 Ответить", CallbackData: "sel:" + k},
		{Text: "🖥 Экран", CallbackData: "scr:" + k},
	}}}
	b.broadcast(ctx, sb.String(), kb)
}

func (b *Bot) broadcast(ctx context.Context, text string, kb *Keyboard) {
	for id := range b.allowed {
		if b.chat(id).Muted {
			continue
		}
		if err := b.tg.send(ctx, id, text, kb); err != nil {
			// Типичная причина — пользователь ещё не написал боту /start.
			log.Printf("уведомление %d: %v", id, err)
		}
	}
}

// ---------- утилиты ----------

func esc(s string) string { return html.EscapeString(s) }

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func stripTags(s string) string {
	var sb strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			sb.WriteRune(r)
		}
	}
	return html.UnescapeString(sb.String())
}
