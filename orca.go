package main

// Обёртка над Orca CLI. Все вызовы идут с --json; ответ — конверт
// {"ok":true,"result":...} или {"ok":false,"error":{"code","message"}}.
// Поля взяты из src/shared/runtime-*-contracts.ts репозитория stablyai/orca.
//
// Бот работает сразу с несколькими рантаймами Orca: локальным и сопряжёнными
// серверами (Remote Orca Server, в том числе развёрнутыми Orca на SSH-хосте).
// Каждый вызов получает env: "" — локальный рантайм, иначе --environment <env>.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Target — рантайм Orca, который опрашивает бот.
type Target struct {
	Env  string // значение для --environment; "" — локальный рантайм
	Name string // имя для показа; "" у локального
}

type Orca struct {
	bin     string        // orca-ide на Linux, orca на macOS/Windows
	timeout time.Duration // таймаут одного вызова CLI

	// targets — явный список рантаймов (ORCA_ENVIRONMENT). nil — авто:
	// локальный + все сопряжённые серверы из `environment list`.
	targets []Target

	mu           sync.Mutex
	discovered   []Target
	discoveredAt time.Time

	hostNames   map[string]string // hostId ("ssh:…") -> имя SSH-цели из `host list`
	machineName string            // имя этой машины из `host list`
	hostsLoaded time.Time
}

// OrcaError — ошибка, которую вернул сам рантайм Orca (ok:false).
type OrcaError struct {
	Code    string
	Message string
}

func (e *OrcaError) Error() string { return e.Code + ": " + e.Message }

type orcaEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (o *Orca) run(ctx context.Context, env string, out any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	args = append(args, "--json")
	if env != "" {
		args = append(args, "--environment", env)
	}
	cmd := exec.CommandContext(ctx, o.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// При ошибке CLI тоже печатает JSON-конверт в stdout, поэтому сначала пробуем разобрать его.
	var envl orcaEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envl); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if runErr != nil {
			return fmt.Errorf("%s %s: %v: %s", o.bin, args[0], runErr, truncRunes(msg, 400))
		}
		return fmt.Errorf("%s %s: не удалось разобрать JSON: %s", o.bin, args[0], truncRunes(msg, 400))
	}
	if !envl.OK {
		if envl.Error != nil {
			return &OrcaError{Code: envl.Error.Code, Message: envl.Error.Message}
		}
		return errors.New("orca вернула ok=false без описания ошибки")
	}
	if out != nil && len(envl.Result) > 0 {
		if err := json.Unmarshal(envl.Result, out); err != nil {
			return fmt.Errorf("разбор result: %w", err)
		}
	}
	return nil
}

// ---- Типы (только нужные боту поля) ----

type AgentRow struct {
	PaneKey              string  `json:"paneKey"`
	ParentPaneKey        *string `json:"parentPaneKey"`
	State                string  `json:"state"` // working | blocked | waiting | done
	AgentType            *string `json:"agentType"`
	Prompt               string  `json:"prompt"`
	DisplayName          *string `json:"displayName"`
	LastAssistantMessage *string `json:"lastAssistantMessage"`
	ToolName             *string `json:"toolName"`
	Interrupted          bool    `json:"interrupted"`
	UpdatedAt            int64   `json:"updatedAt"`
}

type Workspace struct {
	WorktreeID        string `json:"worktreeId"` // "<repoId>::<путь>"
	RepoID            string `json:"repoId"`
	Repo              string `json:"repo"`
	Path              string `json:"path"`
	Branch            string `json:"branch"`
	DisplayName       string `json:"displayName"`
	WorkspaceStatus   string `json:"workspaceStatus"`
	Comment           string `json:"comment"`
	Unread            bool   `json:"unread"`
	IsArchived        bool   `json:"isArchived"`
	Status            string `json:"status"` // active | working | permission | done | inactive
	LiveTerminalCount int    `json:"liveTerminalCount"`
	// Терминалы, с хостом которых пропала связь (например, отвалился SSH). Они не мертвы.
	UnverifiableTerminalCount int        `json:"unverifiableTerminalCount"`
	LastActivityAt            int64      `json:"lastActivityAt"`
	Agents                    []AgentRow `json:"agents"`
	// Где реально исполняется воркtree относительно своего рантайма:
	// "local", "ssh:<target>" или "runtime:<env>". Пусто на старых хостах.
	HostID         string `json:"hostId"`
	IsMainWorktree bool   `json:"isMainWorktree"`

	// Какой рантайм Orca вернул воркспейс (заполняет бот, не CLI).
	Env     string `json:"-"`
	EnvName string `json:"-"`
}

// Key — уникальный ключ воркспейса среди всех рантаймов.
func (w Workspace) Key() string { return wsRefKey(w.Env, w.WorktreeID) }

func wsRefKey(env, worktreeID string) string { return env + "|" + worktreeID }

// ShortBranch — ветка без refs/heads/.
func (w Workspace) ShortBranch() string { return strings.TrimPrefix(w.Branch, "refs/heads/") }

// Name — человекочитаемое имя воркспейса.
func (w Workspace) Name() string {
	br := w.ShortBranch()
	// У главного воркtree Orca иногда подставляет в имя ветку («master») — тогда понятнее имя репозитория.
	if w.DisplayName != "" && !(w.IsMainWorktree && w.Repo != "" && w.DisplayName == br) {
		return w.DisplayName
	}
	if (w.IsMainWorktree || br == "") && w.Repo != "" {
		return w.Repo
	}
	if br != "" {
		return br
	}
	return w.Path
}

// MainAgent — основной агент (без родителя); иначе первый попавшийся.
func (w Workspace) MainAgent() *AgentRow {
	for i := range w.Agents {
		if w.Agents[i].ParentPaneKey == nil || *w.Agents[i].ParentPaneKey == "" {
			return &w.Agents[i]
		}
	}
	if len(w.Agents) > 0 {
		return &w.Agents[0]
	}
	return nil
}

type Terminal struct {
	Handle        string  `json:"handle"`
	WorktreeID    string  `json:"worktreeId"`
	TabID         string  `json:"tabId"`
	LeafID        string  `json:"leafId"`
	Title         *string `json:"title"`
	PaneTitle     string  `json:"paneTitle"`
	Connected     bool    `json:"connected"`
	Writable      bool    `json:"writable"`
	AgentIdentity string  `json:"agentIdentity"`
	Verdict       string  `json:"verdict"`
}

func (t Terminal) Label() string {
	switch {
	case t.PaneTitle != "":
		return t.PaneTitle
	case t.Title != nil && *t.Title != "":
		return *t.Title
	case t.AgentIdentity != "":
		return t.AgentIdentity
	}
	return t.Handle
}

// Host — строка из `host list`: эта машина, SSH-цели и сопряжённые серверы.
type Host struct {
	Kind        string `json:"kind"` // local | ssh | environment
	Name        string `json:"name"`
	MachineName string `json:"machineName"` // только у local
	ID          string `json:"id"`
	Selector    string `json:"selector"`
	Connected   *bool  `json:"connected"`
}

type Repo struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	DisplayName string `json:"displayName"`

	Env     string `json:"-"`
	EnvName string `json:"-"`
}

// Project — проект из боковой панели Orca (`project list`). Объединяет репозитории
// (sourceRepoIds) — в том числе один и тот же проект на разных машинах.
type Project struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	BadgeColor  string `json:"badgeColor"`
	RepoIcon    *struct {
		Type string `json:"type"`
		Src  string `json:"src"`
	} `json:"repoIcon"`
	Kind          string   `json:"kind"`
	SourceRepoIDs []string `json:"sourceRepoIds"`

	Env string `json:"-"`
}

// ---- Рантаймы ----

// Targets возвращает рантаймы, которые нужно опрашивать.
// В авто-режиме список сопряжённых серверов кэшируется на минуту.
func (o *Orca) Targets(ctx context.Context) []Target {
	if o.targets != nil {
		return o.targets
	}
	o.mu.Lock()
	if o.discovered != nil && time.Since(o.discoveredAt) < time.Minute {
		t := o.discovered
		o.mu.Unlock()
		return t
	}
	o.mu.Unlock()

	list := []Target{{}}
	var r struct {
		Environments []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"environments"`
	}
	if err := o.run(ctx, "", &r, "environment", "list"); err == nil {
		for _, e := range r.Environments {
			name := e.Name
			if name == "" {
				name = e.ID
			}
			list = append(list, Target{Env: e.ID, Name: name})
		}
	} else {
		o.mu.Lock()
		prev := o.discovered
		o.mu.Unlock()
		if prev != nil {
			// Не теряем известные серверы из-за разового сбоя.
			list = prev
		}
	}
	o.mu.Lock()
	o.discovered, o.discoveredAt = list, time.Now()
	o.mu.Unlock()
	return list
}

// TargetName — имя рантайма для показа ("" у локального).
func (o *Orca) TargetName(ctx context.Context, env string) string {
	if env == "" {
		return ""
	}
	for _, t := range o.Targets(ctx) {
		if t.Env == env {
			return t.Name
		}
	}
	return env
}

// ---- Имена машин ----

// loadHosts обновляет имена машин из `host list` не чаще раза в минуту.
// При ошибке тоже ждём минуту, чтобы не дёргать CLI на каждый запрос.
func (o *Orca) loadHosts(ctx context.Context) {
	o.mu.Lock()
	stale := time.Since(o.hostsLoaded) > time.Minute
	if stale {
		o.hostsLoaded = time.Now()
	}
	o.mu.Unlock()
	if !stale {
		return
	}
	hosts, err := o.Hosts(ctx)
	if err != nil {
		return
	}
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
	o.mu.Lock()
	o.hostNames, o.machineName = names, machine
	o.mu.Unlock()
}

// HostLabel возвращает понятное имя удалённого хоста ("" для локального).
// Если имя неизвестно, показываем сам id.
func (o *Orca) HostLabel(ctx context.Context, hostID string) string {
	if hostID == "" || hostID == "local" {
		return ""
	}
	o.loadHosts(ctx)
	o.mu.Lock()
	name := o.hostNames[hostID]
	o.mu.Unlock()
	if name != "" {
		return name
	}
	_, rest, _ := strings.Cut(hostID, ":")
	return rest
}

// MachineName — имя этой машины из `host list` ("" если неизвестно).
func (o *Orca) MachineName() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.machineName
}

// WSHost — где живёт воркспейс: имя сервера Orca и/или SSH-цели; "" для этой машины.
func (o *Orca) WSHost(ctx context.Context, w Workspace) string {
	if w.Env != "" {
		// SSH-цели удалённого рантайма по имени не разрешаем: показываем id.
		if w.HostID != "" && w.HostID != "local" {
			_, rest, _ := strings.Cut(w.HostID, ":")
			return w.EnvName + "/" + rest
		}
		return w.EnvName
	}
	return o.HostLabel(ctx, w.HostID)
}

// ---- Команды ----

func (o *Orca) PS(ctx context.Context, env string) ([]Workspace, error) {
	var r struct {
		Worktrees []Workspace `json:"worktrees"`
	}
	err := o.run(ctx, env, &r, "worktree", "ps")
	name := o.TargetName(ctx, env)
	for i := range r.Worktrees {
		r.Worktrees[i].Env, r.Worktrees[i].EnvName = env, name
	}
	return r.Worktrees, err
}

// TargetError — рантайм, который не ответил при общем опросе.
type TargetError struct {
	Target Target
	Err    error
}

// PSAll опрашивает все рантаймы параллельно. Недоступный сервер не мешает остальным.
func (o *Orca) PSAll(ctx context.Context) ([]Workspace, []TargetError) {
	targets := o.Targets(ctx)
	lists := make([][]Workspace, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			lists[i], errs[i] = o.PS(ctx, t.Env)
		}(i, t)
	}
	wg.Wait()
	var all []Workspace
	var failed []TargetError
	for i := range targets {
		if errs[i] != nil {
			failed = append(failed, TargetError{Target: targets[i], Err: errs[i]})
			continue
		}
		all = append(all, lists[i]...)
	}
	return all, failed
}

func (o *Orca) Terminals(ctx context.Context, env, worktreeID string) ([]Terminal, error) {
	var r struct {
		Terminals []Terminal `json:"terminals"`
	}
	err := o.run(ctx, env, &r, "terminal", "list", "--worktree", "id:"+worktreeID)
	return r.Terminals, err
}

// Screen возвращает текущий отрисованный экран терминала и метаданные строк
// (мягкие переносы). На старых хостах, где нет --screen, откатывается на накопленный поток вывода.
func (o *Orca) Screen(ctx context.Context, env, handle string) ([]string, []ScreenRow, error) {
	var r struct {
		Terminal struct {
			Tail     []string    `json:"tail"`
			TailRows []ScreenRow `json:"tailRows"`
		} `json:"terminal"`
	}
	err := o.run(ctx, env, &r, "terminal", "read", "--terminal", handle, "--screen")
	var oe *OrcaError
	if errors.As(err, &oe) && oe.Code == "incompatible_runtime" {
		err = o.run(ctx, env, &r, "terminal", "read", "--terminal", handle, "--limit", "200")
	}
	if len(r.Terminal.TailRows) != len(r.Terminal.Tail) {
		r.Terminal.TailRows = nil // метаданные не совпадают со строками — не доверяем им
	}
	return r.Terminal.Tail, r.Terminal.TailRows, err
}

// Send печатает текст в терминал агента и жмёт Enter; ждёт подтверждения отправки до 10 с.
func (o *Orca) Send(ctx context.Context, env, handle, text string) error {
	return o.run(ctx, env, nil, "terminal", "send", "--terminal", handle, "--text", text, "--enter", "--wait-submit", "10")
}

// Interrupt посылает прерывание (как Ctrl+C) в терминал.
func (o *Orca) Interrupt(ctx context.Context, env, handle string) error {
	return o.run(ctx, env, nil, "terminal", "send", "--terminal", handle, "--interrupt")
}

// SendKeys пишет байты в терминал как есть, без Enter: Esc, стрелки, цифры меню.
func (o *Orca) SendKeys(ctx context.Context, env, handle, raw string) error {
	return o.run(ctx, env, nil, "terminal", "send", "--terminal", handle, "--text", raw)
}

// Projects — проекты рантайма из `project list`.
func (o *Orca) Projects(ctx context.Context, env string) ([]Project, error) {
	var r struct {
		Projects []Project `json:"projects"`
	}
	err := o.run(ctx, env, &r, "project", "list")
	for i := range r.Projects {
		r.Projects[i].Env = env
	}
	return r.Projects, err
}

// Hosts возвращает машины локального рантайма: эту и её SSH-цели.
func (o *Orca) Hosts(ctx context.Context) ([]Host, error) {
	var r struct {
		Hosts []Host `json:"hosts"`
	}
	err := o.run(ctx, "", &r, "host", "list")
	return r.Hosts, err
}

func (o *Orca) Repos(ctx context.Context, env string) ([]Repo, error) {
	var r struct {
		Repos []Repo `json:"repos"`
	}
	err := o.run(ctx, env, &r, "repo", "list")
	name := o.TargetName(ctx, env)
	for i := range r.Repos {
		r.Repos[i].Env, r.Repos[i].EnvName = env, name
	}
	return r.Repos, err
}

// ReposAll — репозитории всех рантаймов; недоступные серверы пропускаются.
func (o *Orca) ReposAll(ctx context.Context) ([]Repo, []TargetError) {
	var all []Repo
	var failed []TargetError
	for _, t := range o.Targets(ctx) {
		repos, err := o.Repos(ctx, t.Env)
		if err != nil {
			failed = append(failed, TargetError{Target: t, Err: err})
			continue
		}
		all = append(all, repos...)
	}
	return all, failed
}

// CreateWorktree создаёт новый воркtree и запускает в нём агента в терминале
// (--surface terminal: в нативный чат Orca CLI писать не умеет, а в терминал — умеет).
func (o *Orca) CreateWorktree(ctx context.Context, env, repoID, name, agent, prompt string) (string, error) {
	args := []string{"worktree", "create", "--repo", "id:" + repoID, "--name", name,
		"--agent", agent, "--surface", "terminal", "--no-parent"}
	if prompt != "" {
		args = append(args, "--prompt", prompt)
	}
	var r struct {
		Worktree struct {
			ID string `json:"id"`
		} `json:"worktree"`
	}
	err := o.run(ctx, env, &r, args...)
	return r.Worktree.ID, err
}

// LaunchAgent запускает ещё одного агента в существующем воркtree.
func (o *Orca) LaunchAgent(ctx context.Context, env, worktreeID, agent, prompt string) error {
	args := []string{"agent", "launch", "--agent", agent, "--worktree", "id:" + worktreeID, "--surface", "terminal"}
	if prompt != "" {
		args = append(args, "--prompt", prompt)
	}
	return o.run(ctx, env, nil, args...)
}

// PickAgentTerminal выбирает терминал, в котором сидит основной агент воркспейса.
// Связь: paneKey агента = "<tabId>:<leafId>" терминала.
func PickAgentTerminal(ws Workspace, terms []Terminal) *Terminal {
	if a := ws.MainAgent(); a != nil {
		for i := range terms {
			if terms[i].TabID+":"+terms[i].LeafID == a.PaneKey && terms[i].Writable {
				return &terms[i]
			}
		}
	}
	for i := range terms {
		if terms[i].AgentIdentity != "" && terms[i].Writable {
			return &terms[i]
		}
	}
	for i := range terms {
		if terms[i].Writable {
			return &terms[i]
		}
	}
	return nil
}
