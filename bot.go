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
	WorktreeID string `json:"worktreeId"`
	Terminal   string `json:"terminal"` // handle; пусто — терминал агента выбирается автоматически
	Muted      bool   `json:"muted"`    // уведомления выключены
}

type Bot struct {
	tg        *tgClient
	orca      *Orca
	allowed   map[int64]bool
	agent     string // агент по умолчанию для /new и /agent
	stateFile string

	mu    sync.Mutex
	chats map[int64]*chatState
	ids   map[string]string // короткий id -> worktreeId (callback_data ограничен 64 байтами)
	terms map[string]string // короткий id -> handle терминала

	hostNames   map[string]string // hostId ("ssh:…") -> имя SSH-цели из `host list`
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
			for _, h := range hosts {
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
			b.hostNames, b.hostsLoaded = names, time.Now()
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
	s := esc(w.Repo) + " · <code>" + esc(w.Branch) + "</code>"
	if h := b.hostLabel(ctx, w.HostID); h != "" {
		s += " · 🖧 " + esc(h)
	}
	return s
}

// noTermText объясняет, почему писать некуда: нет терминалов или потеряна связь с хостом.
func (b *Bot) noTermText(ctx context.Context, w Workspace, term *Terminal) string {
	if w.UnverifiableTerminalCount > 0 || (term != nil && term.Verdict == "unverifiable") {
		h := b.hostLabel(ctx, w.HostID)
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
		chats: map[int64]*chatState{}, ids: map[string]string{}, terms: map[string]string{},
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

func (b *Bot) wsKey(worktreeID string) string {
	k := shortID(worktreeID)
	b.mu.Lock()
	b.ids[k] = worktreeID
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
	case "sel", "scr", "stp":
		wtID, ok := b.lookup(b.ids, key)
		if !ok {
			b.tg.answerCallback(ctx, q.ID, "Кнопка устарела, обновите /ws")
			return
		}
		b.updateChat(chatID, func(c *chatState) {
			if c.WorktreeID != wtID {
				c.Terminal = ""
			}
			c.WorktreeID = wtID
		})
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
/read [N] — экран терминала агента (последние N строк, по умолчанию 40)
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

var statusOrder = map[string]int{"permission": 0, "working": 1, "done": 2, "active": 3, "inactive": 4}

func statusEmoji(s string) string {
	switch s {
	case "working":
		return "⏳"
	case "permission":
		return "⚠️"
	case "done":
		return "✅"
	case "active":
		return "🟢"
	}
	return "⚪"
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

func (b *Bot) listWorkspaces(ctx context.Context, chatID int64) {
	all, err := b.orca.PS(ctx)
	if err != nil {
		b.replyErr(ctx, chatID, "worktree ps", err)
		return
	}
	var list []Workspace
	for _, w := range all {
		if !w.IsArchived {
			list = append(list, w)
		}
	}
	if len(list) == 0 {
		b.reply(ctx, chatID, "Воркспейсов нет. Создать: /new", nil)
		return
	}
	sort.SliceStable(list, func(i, j int) bool {
		oi, oj := statusOrder[list[i].Status], statusOrder[list[j].Status]
		if oi != oj {
			return oi < oj
		}
		return list[i].LastActivityAt > list[j].LastActivityAt
	})
	const maxItems = 25
	cur := b.chat(chatID).WorktreeID

	var sb strings.Builder
	var rows [][]Button
	var row []Button
	for i, w := range list {
		if i == maxItems {
			fmt.Fprintf(&sb, "\n…и ещё %d", len(list)-maxItems)
			break
		}
		mark := ""
		if w.WorktreeID == cur {
			mark = "👉 "
		}
		fmt.Fprintf(&sb, "%s%s <b>%s</b> — %s\n", mark, statusEmoji(w.Status), esc(w.Name()), b.whereText(ctx, w))
		if w.UnverifiableTerminalCount > 0 {
			sb.WriteString("    🔌 нет связи с хостом\n")
		}
		if w.Comment != "" {
			fmt.Fprintf(&sb, "    💬 %s\n", esc(truncRunes(w.Comment, 120)))
		}
		if a := w.MainAgent(); a != nil {
			agent := "агент"
			if a.AgentType != nil && *a.AgentType != "" {
				agent = *a.AgentType
			}
			line := fmt.Sprintf("    🤖 %s: %s", esc(agent), agentStateRu(a.State))
			if a.State == "working" && a.ToolName != nil && *a.ToolName != "" {
				line += " (" + esc(*a.ToolName) + ")"
			}
			if extra := len(w.Agents) - 1; extra > 0 {
				line += fmt.Sprintf(" +%d", extra)
			}
			sb.WriteString(line + "\n")
		}
		row = append(row, Button{Text: statusEmoji(w.Status) + " " + truncRunes(w.Name(), 28), CallbackData: "sel:" + b.wsKey(w.WorktreeID)})
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	b.reply(ctx, chatID, sb.String(), &Keyboard{InlineKeyboard: rows})
}

// resolve находит выбранный воркспейс и терминал, в который надо писать.
func (b *Bot) resolve(ctx context.Context, chatID int64) (*Workspace, *Terminal, []Terminal, error) {
	st := b.chat(chatID)
	if st.WorktreeID == "" {
		return nil, nil, nil, fmt.Errorf("воркспейс не выбран — откройте /ws")
	}
	all, err := b.orca.PS(ctx)
	if err != nil {
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
	terms, err := b.orca.Terminals(ctx, ws.WorktreeID)
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
	k := b.wsKey(ws.WorktreeID)
	return &Keyboard{InlineKeyboard: [][]Button{{
		{Text: "🖥 Экран", CallbackData: "scr:" + k},
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
	fmt.Fprintf(&sb, "%s <b>%s</b>\n%s\n", statusEmoji(ws.Status), esc(ws.Name()), b.whereText(ctx, *ws))
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
	if err := b.orca.Send(ctx, term.Handle, text); err != nil {
		b.replyErr(ctx, chatID, "terminal send", err)
		return
	}
	b.reply(ctx, chatID, "✉️ Отправлено → <b>"+esc(ws.Name())+"</b>", b.wsButtons(ws))
}

func (b *Bot) showScreen(ctx context.Context, chatID int64, n int) {
	ws, term, _, err := b.resolve(ctx, chatID)
	if err != nil {
		b.replyErr(ctx, chatID, "экран", err)
		return
	}
	if term == nil {
		b.reply(ctx, chatID, b.noTermText(ctx, *ws, term), nil)
		return
	}
	lines, err := b.orca.Screen(ctx, term.Handle)
	if err != nil {
		b.replyErr(ctx, chatID, "terminal read", err)
		return
	}
	if n <= 0 {
		n = 40
	}
	body := tailText(lines, n, 3500)
	if body == "" {
		body = "(пусто)"
	}
	b.reply(ctx, chatID, "🖥 <b>"+esc(ws.Name())+"</b> · "+esc(term.Label())+"\n<pre>"+esc(body)+"</pre>", b.wsButtons(ws))
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
	if err := b.orca.Interrupt(ctx, term.Handle); err != nil {
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
	repos, err := b.orca.Repos(ctx)
	if err != nil {
		b.replyErr(ctx, chatID, "repo list", err)
		return
	}
	if len(repos) == 0 {
		b.reply(ctx, chatID, "Репозиториев нет — добавьте их в Orca.", nil)
		return
	}
	var sb strings.Builder
	sb.WriteString("Репозитории (имя для /new):\n")
	for _, r := range repos {
		fmt.Fprintf(&sb, "• <code>%s</code> — %s\n", esc(repoName(r)), esc(r.Path))
	}
	b.reply(ctx, chatID, sb.String(), nil)
}

func repoName(r Repo) string {
	if r.DisplayName != "" {
		return r.DisplayName
	}
	return filepath.Base(r.Path)
}

func findRepo(repos []Repo, q string) (*Repo, error) {
	ql := strings.ToLower(q)
	var hits []*Repo
	for i := range repos {
		r := &repos[i]
		if strings.ToLower(repoName(*r)) == ql || strings.ToLower(filepath.Base(r.Path)) == ql || r.ID == q {
			return r, nil
		}
		if strings.Contains(strings.ToLower(repoName(*r)), ql) {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("репозиторий «%s» не найден, см. /repos", q)
	}
	return nil, fmt.Errorf("«%s» подходит к нескольким репозиториям, уточните (/repos)", q)
}

// /new <репо> <имя> [задача]
func (b *Bot) newWorktree(ctx context.Context, chatID int64, args string) {
	f := strings.Fields(args)
	if len(f) < 2 {
		b.reply(ctx, chatID, "Использование: <code>/new репо имя-ветки [задача для агента]</code>\nСписок репозиториев: /repos", nil)
		return
	}
	repos, err := b.orca.Repos(ctx)
	if err != nil {
		b.replyErr(ctx, chatID, "repo list", err)
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
	wtID, err := b.orca.CreateWorktree(ctx, repo.ID, name, b.agent, prompt)
	if err != nil {
		b.replyErr(ctx, chatID, "worktree create", err)
		return
	}
	if wtID != "" {
		b.updateChat(chatID, func(c *chatState) { c.WorktreeID = wtID; c.Terminal = "" })
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
	if err := b.orca.LaunchAgent(ctx, st.WorktreeID, agent, prompt); err != nil {
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

// Watch опрашивает worktree ps и сообщает, когда агент закончил работу или ждёт разрешения.
func (b *Bot) Watch(ctx context.Context, interval time.Duration) {
	prev := map[string]string{}
	lastNotified := map[string]time.Time{}
	first := true
	fails := 0
	down := false
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		list, err := b.orca.PS(ctx)
		if err != nil {
			fails++
			log.Printf("watch: %v", err)
			// Не шумим из-за единичного сбоя: сообщаем после трёх подряд.
			if fails == 3 && !down {
				down = true
				b.broadcast(ctx, "🔌 Orca не отвечает: <code>"+esc(truncRunes(err.Error(), 500))+"</code>", nil)
			}
		} else {
			if down {
				b.broadcast(ctx, "🔌 Orca снова на связи", nil)
			}
			fails, down = 0, false
			cur := map[string]string{}
			for _, w := range list {
				cur[w.WorktreeID] = w.Status
				p, seen := prev[w.WorktreeID]
				if first || !seen || p == w.Status {
					continue
				}
				switch {
				case w.Status == "permission":
					b.notify(ctx, w, "⚠️ <b>%s</b> ждёт разрешения")
				case p == "working" && w.Status != "working":
					// Защита от дребезга статуса между шагами агента.
					if time.Since(lastNotified[w.WorktreeID]) < 20*time.Second {
						continue
					}
					lastNotified[w.WorktreeID] = time.Now()
					b.notify(ctx, w, "✅ <b>%s</b>: агент закончил")
				}
			}
			prev, first = cur, false
		}
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
	k := b.wsKey(w.WorktreeID)
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

// tailText берёт последние n непустых в конце строк и ограничивает результат maxRunes символами с конца.
func tailText(lines []string, n, maxRunes int) string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimRight(l, " \t")
	}
	s := strings.Join(out, "\n")
	if r := []rune(s); len(r) > maxRunes {
		s = "…" + string(r[len(r)-maxRunes:])
	}
	return s
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
