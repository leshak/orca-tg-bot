package main

// Обёртка над Orca CLI. Все вызовы идут с --json; ответ — конверт
// {"ok":true,"result":...} или {"ok":false,"error":{"code","message"}}.
// Поля взяты из src/shared/runtime-*-contracts.ts репозитория stablyai/orca.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Orca struct {
	bin     string        // orca-ide на Linux, orca на macOS/Windows
	env     string        // --environment <имя> для сопряжённого Remote Orca Server (необязательно)
	timeout time.Duration // таймаут одного вызова CLI
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

func (o *Orca) run(ctx context.Context, out any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	args = append(args, "--json")
	if o.env != "" {
		args = append(args, "--environment", o.env)
	}
	cmd := exec.CommandContext(ctx, o.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// При ошибке CLI тоже печатает JSON-конверт в stdout, поэтому сначала пробуем разобрать его.
	var env orcaEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if runErr != nil {
			return fmt.Errorf("%s %s: %v: %s", o.bin, args[0], runErr, truncRunes(msg, 400))
		}
		return fmt.Errorf("%s %s: не удалось разобрать JSON: %s", o.bin, args[0], truncRunes(msg, 400))
	}
	if !env.OK {
		if env.Error != nil {
			return &OrcaError{Code: env.Error.Code, Message: env.Error.Message}
		}
		return errors.New("orca вернула ok=false без описания ошибки")
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
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
	// Где реально исполняется воркtree: "local", "ssh:<target>" или "runtime:<env>". Пусто на старых хостах.
	HostID string `json:"hostId"`
}

// Name — человекочитаемое имя воркспейса.
func (w Workspace) Name() string {
	if w.DisplayName != "" {
		return w.DisplayName
	}
	if w.Branch != "" {
		return w.Branch
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
	Kind      string `json:"kind"` // local | ssh | environment
	Name      string `json:"name"`
	ID        string `json:"id"`
	Selector  string `json:"selector"`
	Connected *bool  `json:"connected"`
}

type Repo struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	DisplayName string `json:"displayName"`
}

// ---- Команды ----

func (o *Orca) PS(ctx context.Context) ([]Workspace, error) {
	var r struct {
		Worktrees []Workspace `json:"worktrees"`
	}
	err := o.run(ctx, &r, "worktree", "ps")
	return r.Worktrees, err
}

func (o *Orca) Terminals(ctx context.Context, worktreeID string) ([]Terminal, error) {
	var r struct {
		Terminals []Terminal `json:"terminals"`
	}
	err := o.run(ctx, &r, "terminal", "list", "--worktree", "id:"+worktreeID)
	return r.Terminals, err
}

// Screen возвращает текущий отрисованный экран терминала. На старых хостах,
// где нет --screen, откатывается на накопленный поток вывода.
func (o *Orca) Screen(ctx context.Context, handle string) ([]string, error) {
	var r struct {
		Terminal struct {
			Tail []string `json:"tail"`
		} `json:"terminal"`
	}
	err := o.run(ctx, &r, "terminal", "read", "--terminal", handle, "--screen")
	var oe *OrcaError
	if errors.As(err, &oe) && oe.Code == "incompatible_runtime" {
		err = o.run(ctx, &r, "terminal", "read", "--terminal", handle, "--limit", "200")
	}
	return r.Terminal.Tail, err
}

// Send печатает текст в терминал агента и жмёт Enter; ждёт подтверждения отправки до 10 с.
func (o *Orca) Send(ctx context.Context, handle, text string) error {
	return o.run(ctx, nil, "terminal", "send", "--terminal", handle, "--text", text, "--enter", "--wait-submit", "10")
}

// Interrupt посылает прерывание (как Ctrl+C) в терминал.
func (o *Orca) Interrupt(ctx context.Context, handle string) error {
	return o.run(ctx, nil, "terminal", "send", "--terminal", handle, "--interrupt")
}

// Hosts возвращает машины, на которых этот Orca может запускать работу.
func (o *Orca) Hosts(ctx context.Context) ([]Host, error) {
	var r struct {
		Hosts []Host `json:"hosts"`
	}
	err := o.run(ctx, &r, "host", "list")
	return r.Hosts, err
}

func (o *Orca) Repos(ctx context.Context) ([]Repo, error) {
	var r struct {
		Repos []Repo `json:"repos"`
	}
	err := o.run(ctx, &r, "repo", "list")
	return r.Repos, err
}

// CreateWorktree создаёт новый воркtree и запускает в нём агента в терминале
// (--surface terminal: в нативный чат Orca CLI писать не умеет, а в терминал — умеет).
func (o *Orca) CreateWorktree(ctx context.Context, repoID, name, agent, prompt string) (string, error) {
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
	err := o.run(ctx, &r, args...)
	return r.Worktree.ID, err
}

// LaunchAgent запускает ещё одного агента в существующем воркtree.
func (o *Orca) LaunchAgent(ctx context.Context, worktreeID, agent, prompt string) error {
	args := []string{"agent", "launch", "--agent", agent, "--worktree", "id:" + worktreeID, "--surface", "terminal"}
	if prompt != "" {
		args = append(args, "--prompt", prompt)
	}
	return o.run(ctx, nil, args...)
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

func itoa(n int) string { return strconv.Itoa(n) }
