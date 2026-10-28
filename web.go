package main

// Веб-интерфейс для локальной сети: проекты и воркспейсы Orca, экраны терминалов,
// ввод для агентов. Обновления идут в браузер через SSE (/api/events), команды —
// обычными POST с JSON. Без внешних зависимостей.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFiles embed.FS

const sessionCookie = "orca_web"

type Web struct {
	orca        *Orca
	agent       string        // агент по умолчанию
	token       string        // WEB_TOKEN
	session     string        // значение cookie после входа: хэш токена
	poll        time.Duration // как часто опрашивать статусы, пока кто-то смотрит
	screenEvery time.Duration // как часто перечитывать открытый экран

	mu     sync.Mutex
	snap   []byte      // последний снимок проектов (JSON)
	snapAt time.Time   // когда снимок обновлялся
	list   []Workspace // воркспейсы из последнего опроса
	subs   map[chan struct{}]struct{}
	kick   chan struct{}

	projMu sync.Mutex
	projs  map[string][]Project // рантайм -> проекты; кэш
	projAt time.Time
}

func NewWeb(orca *Orca, agent, token string, poll, screenEvery time.Duration) *Web {
	h := sha256.Sum256([]byte("orca-tg-bot/web/" + token))
	return &Web{
		orca: orca, agent: agent, token: token, session: hex.EncodeToString(h[:]),
		poll: poll, screenEvery: screenEvery,
		subs: map[chan struct{}]struct{}{}, kick: make(chan struct{}, 1),
	}
}

// Serve слушает addr, пока не отменён ctx.
func (w *Web) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           w.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Запросы наследуют ctx: при остановке SSE-потоки закрываются сами.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go w.run(ctx)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ---------- маршруты и доступ ----------

func (w *Web) routes() http.Handler {
	static, _ := fs.Sub(webFiles, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /login", func(rw http.ResponseWriter, r *http.Request) {
		page, _ := fs.ReadFile(static, "login.html")
		if r.URL.Query().Get("err") != "" {
			page = bytes.Replace(page, []byte("<!--ERR-->"), []byte(`<p class="err">Неверный токен</p>`), 1)
		}
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Header().Set("Cache-Control", "no-cache")
		_, _ = rw.Write(page)
	})
	mux.HandleFunc("POST /login", w.login)
	mux.HandleFunc("POST /logout", w.logout)
	mux.HandleFunc("GET /{$}", func(rw http.ResponseWriter, r *http.Request) {
		if !w.authed(r) {
			http.Redirect(rw, r, "/login", http.StatusSeeOther)
			return
		}
		serveFile(rw, static, "index.html")
	})
	mux.HandleFunc("GET /api/ping", w.api(func(rw http.ResponseWriter, r *http.Request) error {
		writeJSON(rw, http.StatusOK, map[string]string{"ok": "pong"})
		return nil
	}))
	mux.HandleFunc("GET /api/events", w.api(w.events))
	mux.HandleFunc("POST /api/send", w.api(w.apiSend))
	mux.HandleFunc("POST /api/key", w.api(w.apiKey))
	mux.HandleFunc("POST /api/agent", w.api(w.apiAgent))
	mux.HandleFunc("POST /api/worktree", w.api(w.apiWorktree))
	return secure(mux)
}

func serveFile(rw http.ResponseWriter, fsys fs.FS, name string) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-cache")
	_, _ = rw.Write(data)
}

// secure добавляет заголовки безопасности и не пускает POST с чужих сайтов.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h := rw.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-cache")
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(rw, "чужой Origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(rw, r)
	})
}

// sameOrigin: у POST из браузера Origin должен совпадать с адресом сервера.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return r.Header.Get("Sec-Fetch-Site") == "" || r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func (w *Web) authed(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(w.session)) == 1
}

func (w *Web) login(rw http.ResponseWriter, r *http.Request) {
	tok := r.PostFormValue("token")
	if subtle.ConstantTimeCompare([]byte(tok), []byte(w.token)) != 1 {
		log.Printf("веб: неверный токен с %s", r.RemoteAddr)
		time.Sleep(time.Second) // замедляем перебор
		http.Redirect(rw, r, "/login?err=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(rw, &http.Cookie{
		Name: sessionCookie, Value: w.session, Path: "/",
		MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}

func (w *Web) logout(rw http.ResponseWriter, r *http.Request) {
	http.SetCookie(rw, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(rw, r, "/login", http.StatusSeeOther)
}

// api проверяет вход; ошибки обработчика уходят клиенту как {"error": "..."}.
func (w *Web) api(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !w.authed(r) {
			writeJSON(rw, http.StatusUnauthorized, map[string]string{"error": "нужно войти"})
			return
		}
		if err := fn(rw, r); err != nil {
			code := http.StatusBadGateway
			var be badRequest
			if errors.As(err, &be) {
				code = http.StatusBadRequest
			}
			writeJSON(rw, code, map[string]string{"error": err.Error()})
		}
	}
}

type badRequest struct{ msg string }

func (e badRequest) Error() string { return e.msg }

func bad(format string, a ...any) error { return badRequest{fmt.Sprintf(format, a...)} }

func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

// ---------- снимок проектов ----------

// Что видит браузер. Поля короткие: снимок уходит при каждом изменении.
type webAgent struct {
	Pane        string `json:"pane"`
	Type        string `json:"type,omitempty"`
	State       string `json:"state"`
	Title       string `json:"title,omitempty"`
	Tool        string `json:"tool,omitempty"`
	Message     string `json:"message,omitempty"`
	Updated     int64  `json:"updated,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
}

type webWorkspace struct {
	Env      string     `json:"env"`
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Branch   string     `json:"branch,omitempty"`
	Main     bool       `json:"main,omitempty"`
	Host     string     `json:"host,omitempty"`
	State    string     `json:"state"` // permission | working | ready | open | idle
	Comment  string     `json:"comment,omitempty"`
	Unread   bool       `json:"unread,omitempty"`
	Activity int64      `json:"activity,omitempty"`
	Lost     bool       `json:"lost,omitempty"` // нет связи с хостом
	Agents   []webAgent `json:"agents,omitempty"`
}

type webRepo struct {
	Env     string `json:"env"`
	EnvName string `json:"envName,omitempty"`
	ID      string `json:"id"`
}

type webProject struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Icon       string         `json:"icon,omitempty"`
	Repos      []webRepo      `json:"repos"`
	Workspaces []webWorkspace `json:"workspaces"`
}

type webDown struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

type webSnapshot struct {
	Agent    string       `json:"agent"`
	Agents   []string     `json:"agents"`
	Projects []webProject `json:"projects"`
	Down     []webDown    `json:"down,omitempty"`
}

func stateName(w Workspace) string {
	switch stateOf(w) {
	case stPermission:
		return "permission"
	case stWorking:
		return "working"
	case stReady:
		return "ready"
	case stOpen:
		return "open"
	}
	return "idle"
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// projects возвращает проекты всех рантаймов; список меняется редко — кэш на 30 с.
func (w *Web) projects(ctx context.Context, targets []Target) map[string][]Project {
	w.projMu.Lock()
	defer w.projMu.Unlock()
	if w.projs != nil && time.Since(w.projAt) < 30*time.Second {
		return w.projs
	}
	next := map[string][]Project{}
	for _, t := range targets {
		ps, err := w.orca.Projects(ctx, t.Env)
		if err != nil {
			// Старый хост без `project list` или сбой — берём прошлый список, иначе сгруппируем по репозиториям.
			ps = w.projs[t.Env]
		}
		next[t.Env] = ps
	}
	w.projs, w.projAt = next, time.Now()
	return next
}

func (w *Web) buildSnapshot(ctx context.Context, list []Workspace, failed []TargetError) webSnapshot {
	targets := w.orca.Targets(ctx)
	projs := w.projects(ctx, targets)

	byID := map[string]*webProject{}
	var order []*webProject
	repoProj := map[string]*webProject{} // рантайм|repoId -> проект
	get := func(id, name string) *webProject {
		if p := byID[id]; p != nil {
			return p
		}
		p := &webProject{ID: id, Name: name, Repos: []webRepo{}, Workspaces: []webWorkspace{}}
		byID[id] = p
		order = append(order, p)
		return p
	}
	for _, t := range targets {
		for _, pr := range projs[t.Env] {
			name := pr.DisplayName
			if name == "" {
				name = pr.ID
			}
			p := get(pr.ID, name)
			if p.Icon == "" && pr.RepoIcon != nil && pr.RepoIcon.Type == "image" && strings.HasPrefix(pr.RepoIcon.Src, "https://") {
				p.Icon = pr.RepoIcon.Src
			}
			for _, rid := range pr.SourceRepoIDs {
				repoProj[t.Env+"|"+rid] = p
				p.Repos = append(p.Repos, webRepo{Env: t.Env, EnvName: t.Name, ID: rid})
			}
		}
	}

	w.orca.HostLabel(ctx, "ssh:") // подгрузить имена машин
	machine := w.orca.MachineName()
	for _, ws := range list {
		if ws.IsArchived {
			continue
		}
		p := repoProj[ws.Env+"|"+ws.RepoID]
		if p == nil {
			// Проекта нет (старый хост) — проектом считаем репозиторий.
			name := ws.Repo
			if name == "" {
				name = ws.Name()
			}
			p = get("repo:"+ws.Env+"|"+ws.RepoID, name)
			if len(p.Repos) == 0 && ws.RepoID != "" {
				p.Repos = append(p.Repos, webRepo{Env: ws.Env, EnvName: ws.EnvName, ID: ws.RepoID})
			}
		}
		host := w.orca.WSHost(ctx, ws)
		if host == "" {
			host = machine
		}
		v := webWorkspace{
			Env: ws.Env, ID: ws.WorktreeID, Name: ws.Name(), Branch: ws.ShortBranch(), Main: ws.IsMainWorktree,
			Host: host, State: stateName(ws), Comment: ws.Comment, Unread: ws.Unread,
			Activity: ws.LastActivityAt, Lost: ws.UnverifiableTerminalCount > 0,
		}
		for _, a := range ws.Agents {
			title := deref(a.DisplayName)
			if title == "" {
				title = a.Prompt
			}
			v.Agents = append(v.Agents, webAgent{
				Pane: a.PaneKey, Type: deref(a.AgentType), State: a.State, Title: truncRunes(title, 200),
				Tool: deref(a.ToolName), Message: truncRunes(deref(a.LastAssistantMessage), 6000),
				Updated: a.UpdatedAt, Interrupted: a.Interrupted,
			})
		}
		p.Workspaces = append(p.Workspaces, v)
	}

	snap := webSnapshot{Agent: w.agent, Projects: make([]webProject, 0, len(order))}
	for a := range knownAgents {
		snap.Agents = append(snap.Agents, a)
	}
	sort.Strings(snap.Agents)
	for _, p := range order {
		sort.SliceStable(p.Workspaces, func(i, j int) bool {
			a, b := p.Workspaces[i], p.Workspaces[j]
			if a.Main != b.Main {
				return a.Main
			}
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		})
		snap.Projects = append(snap.Projects, *p)
	}
	sort.SliceStable(snap.Projects, func(i, j int) bool {
		return strings.ToLower(snap.Projects[i].Name) < strings.ToLower(snap.Projects[j].Name)
	})
	for _, f := range failed {
		snap.Down = append(snap.Down, webDown{Name: targetLabel(f.Target), Error: truncRunes(f.Err.Error(), 300)})
	}
	return snap
}

// run опрашивает Orca, пока открыт хоть один браузер, и рассылает изменения.
func (w *Web) run(ctx context.Context) {
	t := time.NewTicker(w.poll)
	defer t.Stop()
	for {
		w.mu.Lock()
		n := len(w.subs)
		w.mu.Unlock()
		if n > 0 {
			w.refresh(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
	}
}

func (w *Web) refresh(ctx context.Context) {
	list, failed := w.orca.PSAll(ctx)
	if ctx.Err() != nil {
		return
	}
	data, err := json.Marshal(w.buildSnapshot(ctx, list, failed))
	if err != nil {
		log.Printf("веб: снимок: %v", err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list, w.snapAt = list, time.Now()
	if bytes.Equal(data, w.snap) {
		return
	}
	w.snap = data
	for ch := range w.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (w *Web) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.subs[ch] = struct{}{}
	stale := time.Since(w.snapAt) > w.poll
	w.mu.Unlock()
	if stale {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
	return ch
}

func (w *Web) unsubscribe(ch chan struct{}) {
	w.mu.Lock()
	delete(w.subs, ch)
	w.mu.Unlock()
}

func (w *Web) current() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snap
}

// findWorkspace — воркспейс для выбора терминала агента: из последнего опроса,
// а если снимка ещё нет — прямым запросом к Orca. nil — не найден.
func (w *Web) findWorkspace(ctx context.Context, env, id string) *Workspace {
	w.mu.Lock()
	list := w.list
	w.mu.Unlock()
	if list == nil {
		list, _ = w.orca.PS(ctx, env)
	}
	for i := range list {
		if list[i].Env == env && list[i].WorktreeID == id {
			ws := list[i]
			return &ws
		}
	}
	return nil
}

// ---------- поток событий ----------

type webTerm struct {
	Handle   string `json:"handle"`
	Label    string `json:"label"`
	Agent    string `json:"agent,omitempty"`
	Pane     string `json:"pane"`
	Writable bool   `json:"writable"`
	Lost     bool   `json:"lost,omitempty"`
}

type webTerms struct {
	Terms   []webTerm `json:"terms"`
	Active  string    `json:"active"`  // какой экран показываем
	Default string    `json:"default"` // терминал основного агента
	Error   string    `json:"error,omitempty"`
}

type webScreen struct {
	Handle string `json:"handle"`
	Raw    string `json:"raw"`  // как на экране
	Tidy   string `json:"tidy"` // для узкого экрана: переносы склеены, пробелы сжаты
	Error  string `json:"error,omitempty"`
}

// events — SSE: снимок проектов («state»), а если открыт воркспейс — его терминалы
// («terms») и экран выбранного терминала («screen»). Шлём только изменения.
func (w *Web) events(rw http.ResponseWriter, r *http.Request) error {
	fl, ok := rw.(http.Flusher)
	if !ok {
		return errors.New("потоковая отдача не поддерживается")
	}
	q := r.URL.Query()
	env, wt, want := q.Get("env"), q.Get("wt"), q.Get("term")
	if wt != "" && !w.validEnv(r.Context(), env) {
		return bad("неизвестный рантайм %q", env)
	}

	h := rw.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)

	send := func(event string, data []byte) bool {
		if _, err := fmt.Fprintf(rw, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	fmt.Fprint(rw, "retry: 3000\n\n")
	fl.Flush()

	ch := w.subscribe()
	defer w.unsubscribe(ch)
	if s := w.current(); s != nil {
		send("state", s)
	}

	ctx := r.Context()
	var lastTerms, lastScreen []byte
	active := ""
	pollTerms := func() bool {
		var out webTerms
		terms, err := w.orca.Terminals(ctx, env, wt)
		if err != nil {
			out.Error = err.Error()
		}
		if ws := w.findWorkspace(ctx, env, wt); ws != nil {
			if t := PickAgentTerminal(*ws, terms); t != nil {
				out.Default = t.Handle
			}
		}
		out.Terms = []webTerm{}
		for _, t := range terms {
			out.Terms = append(out.Terms, webTerm{
				Handle: t.Handle, Label: t.Label(), Agent: t.AgentIdentity, Pane: t.TabID + ":" + t.LeafID,
				Writable: t.Writable, Lost: t.Verdict == "unverifiable",
			})
			if t.Handle == want {
				out.Active = want
			}
		}
		if out.Active == "" {
			out.Active = out.Default
			if out.Active == "" && len(terms) > 0 {
				out.Active = terms[0].Handle
			}
		}
		active = out.Active
		data, _ := json.Marshal(out)
		if bytes.Equal(data, lastTerms) {
			return true
		}
		lastTerms = data
		return send("terms", data)
	}
	pollScreen := func() bool {
		if active == "" {
			return true
		}
		out := webScreen{Handle: active}
		lines, rows, err := w.orca.Screen(ctx, env, active)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			out.Error = err.Error()
		} else {
			out.Raw = strings.TrimRight(strings.Join(lines, "\n"), "\n ")
			out.Tidy = formatScreen(lines, rows, 0, 1<<20)
		}
		data, _ := json.Marshal(out)
		if bytes.Equal(data, lastScreen) {
			return true
		}
		lastScreen = data
		return send("screen", data)
	}

	var tick <-chan time.Time
	if wt != "" {
		if !pollTerms() || !pollScreen() {
			return nil
		}
		t := time.NewTicker(w.screenEvery)
		defer t.Stop()
		tick = t.C
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for n := 1; ; {
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
			if !send("state", w.current()) {
				return nil
			}
		case <-tick:
			// Список терминалов меняется редко — раз в три такта.
			if n%3 == 0 && !pollTerms() {
				return nil
			}
			n++
			if !pollScreen() {
				return nil
			}
		case <-ping.C:
			if _, err := fmt.Fprint(rw, ": ping\n\n"); err != nil {
				return nil
			}
			fl.Flush()
		}
	}
}

// ---------- команды ----------

type webReq struct {
	Env    string `json:"env"`
	WT     string `json:"wt"`
	Term   string `json:"term"`
	Text   string `json:"text"`
	Key    string `json:"key"`
	Agent  string `json:"agent"`
	Prompt string `json:"prompt"`
	Repo   string `json:"repo"`
	Name   string `json:"name"`
}

func decode(rw http.ResponseWriter, r *http.Request) (webReq, error) {
	var req webReq
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return req, bad("нужен Content-Type: application/json")
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&req); err != nil {
		return req, bad("неверный JSON: %v", err)
	}
	return req, nil
}

func (w *Web) validEnv(ctx context.Context, env string) bool {
	for _, t := range w.orca.Targets(ctx) {
		if t.Env == env {
			return true
		}
	}
	return false
}

// terminal проверяет, что терминал принадлежит воркспейсу и в него можно писать.
// Пустой term — терминал основного агента.
func (w *Web) terminal(ctx context.Context, req webReq) (*Terminal, error) {
	if !w.validEnv(ctx, req.Env) {
		return nil, bad("неизвестный рантайм %q", req.Env)
	}
	if req.WT == "" {
		return nil, bad("не выбран воркспейс")
	}
	terms, err := w.orca.Terminals(ctx, req.Env, req.WT)
	if err != nil {
		return nil, err
	}
	var t *Terminal
	if req.Term != "" {
		for i := range terms {
			if terms[i].Handle == req.Term {
				t = &terms[i]
			}
		}
		if t == nil {
			return nil, bad("терминал закрыт или принадлежит другому воркспейсу")
		}
	} else if ws := w.findWorkspace(ctx, req.Env, req.WT); ws != nil {
		t = PickAgentTerminal(*ws, terms)
	}
	switch {
	case t == nil:
		return nil, bad("в воркспейсе нет терминала для ввода")
	case t.Verdict == "unverifiable":
		return nil, bad("связь с хостом потеряна; Orca переподключится сама, попробуйте позже")
	case !t.Writable:
		return nil, bad("терминал только для чтения")
	}
	return t, nil
}

func (w *Web) apiSend(rw http.ResponseWriter, r *http.Request) error {
	req, err := decode(rw, r)
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.Text) == "" {
		return bad("пустое сообщение")
	}
	t, err := w.terminal(r.Context(), req)
	if err != nil {
		return err
	}
	if err := w.orca.Send(r.Context(), req.Env, t.Handle, req.Text); err != nil {
		return err
	}
	writeJSON(rw, http.StatusOK, map[string]string{"ok": "sent", "term": t.Handle})
	return nil
}

// webKeys — клавиши, которые можно нажать из браузера (меню разрешений, история, отмена).
var webKeys = map[string]string{
	"esc": "\x1b", "enter": "\r", "tab": "\t", "shift-tab": "\x1b[Z", "backspace": "\x7f",
	"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
	"1": "1", "2": "2", "3": "3", "4": "4", "y": "y", "n": "n",
}

func (w *Web) apiKey(rw http.ResponseWriter, r *http.Request) error {
	req, err := decode(rw, r)
	if err != nil {
		return err
	}
	raw, ok := webKeys[req.Key]
	if !ok && req.Key != "ctrl-c" {
		return bad("неизвестная клавиша %q", req.Key)
	}
	t, err := w.terminal(r.Context(), req)
	if err != nil {
		return err
	}
	if req.Key == "ctrl-c" {
		err = w.orca.Interrupt(r.Context(), req.Env, t.Handle)
	} else {
		err = w.orca.SendKeys(r.Context(), req.Env, t.Handle, raw)
	}
	if err != nil {
		return err
	}
	writeJSON(rw, http.StatusOK, map[string]string{"ok": "sent"})
	return nil
}

func (w *Web) pickAgent(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return w.agent, nil
	}
	if !knownAgents[name] {
		return "", bad("неизвестный агент %q", name)
	}
	return name, nil
}

func (w *Web) apiAgent(rw http.ResponseWriter, r *http.Request) error {
	req, err := decode(rw, r)
	if err != nil {
		return err
	}
	if !w.validEnv(r.Context(), req.Env) || req.WT == "" {
		return bad("не выбран воркспейс")
	}
	agent, err := w.pickAgent(req.Agent)
	if err != nil {
		return err
	}
	if err := w.orca.LaunchAgent(r.Context(), req.Env, req.WT, agent, strings.TrimSpace(req.Prompt)); err != nil {
		return err
	}
	w.nudge()
	writeJSON(rw, http.StatusOK, map[string]string{"ok": "launched"})
	return nil
}

func (w *Web) apiWorktree(rw http.ResponseWriter, r *http.Request) error {
	req, err := decode(rw, r)
	if err != nil {
		return err
	}
	if !w.validEnv(r.Context(), req.Env) || req.Repo == "" {
		return bad("не выбран репозиторий")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t\n") {
		return bad("имя ветки: без пробелов и не с «-»")
	}
	agent, err := w.pickAgent(req.Agent)
	if err != nil {
		return err
	}
	id, err := w.orca.CreateWorktree(r.Context(), req.Env, req.Repo, name, agent, strings.TrimSpace(req.Prompt))
	if err != nil {
		return err
	}
	w.nudge()
	writeJSON(rw, http.StatusOK, map[string]string{"ok": "created", "env": req.Env, "wt": id})
	return nil
}

// nudge — обновить снимок сразу, не дожидаясь следующего опроса.
func (w *Web) nudge() {
	w.projMu.Lock()
	w.projAt = time.Time{}
	w.projMu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}
