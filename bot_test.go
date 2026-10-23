package main

// Интеграционный тест: настоящий бот против фейкового Orca CLI (shell-скрипт)
// и фейкового Telegram Bot API (httptest).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeOrca = `#!/bin/sh
echo "$@" >> "$FAKE_DIR/calls.log"
case "$1 $2" in
"worktree ps")
  st=$(cat "$FAKE_DIR/status")
  cat <<EOF
{"id":"1","ok":true,"result":{"worktrees":[{"worktreeId":"repo1::/srv/app/fix-login","repoId":"repo1","repo":"app","path":"/srv/app/fix-login","branch":"fix-login","displayName":"fix-login","workspaceStatus":"in-progress","comment":"чиню логин","unread":false,"isArchived":false,"status":"$st","hostId":"ssh:tgt-1","liveTerminalCount":1,"lastActivityAt":1,"agents":[{"paneKey":"tab1:leaf1","parentPaneKey":null,"state":"working","agentType":"claude","prompt":"fix","displayName":null,"lastAssistantMessage":"Готово: тесты <зелёные> & PR открыт","toolName":"Bash","interrupted":false,"updatedAt":1}]}],"totalCount":1,"truncated":false},"_meta":{"runtimeId":"r"}}
EOF
  ;;
"terminal list")
  v=$(cat "$FAKE_DIR/verdict" 2>/dev/null || echo live)
  echo '{"id":"2","ok":true,"result":{"terminals":[{"handle":"term-shell","worktreeId":"repo1::/srv/app/fix-login","tabId":"tab0","leafId":"leaf0","title":"zsh","connected":true,"writable":true,"verdict":"'$v'"},{"handle":"term-agent","worktreeId":"repo1::/srv/app/fix-login","tabId":"tab1","leafId":"leaf1","title":"claude","connected":true,"writable":true,"agentIdentity":"claude","verdict":"'$v'"}],"totalCount":2,"truncated":false},"_meta":{"runtimeId":"r"}}'
  ;;
"host list")
  echo '{"id":"8","ok":true,"result":{"hosts":[{"kind":"local","name":"mac-mini","id":"local","selector":"local"},{"kind":"ssh","name":"linux-box","id":"tgt-1","selector":"ssh:tgt-1","connected":true}]},"_meta":{"runtimeId":"r"}}'
  ;;
"terminal read")
  echo '{"id":"3","ok":true,"result":{"terminal":{"handle":"term-agent","status":"running","tail":["> hello","  ⎿ ok <b>","",""],"truncated":false,"nextCursor":null,"source":"screen"}},"_meta":{"runtimeId":"r"}}'
  ;;
"terminal send")
  echo '{"id":"4","ok":true,"result":{"accepted":true},"_meta":{"runtimeId":"r"}}'
  ;;
"repo list")
  echo '{"id":"5","ok":true,"result":{"repos":[{"id":"repo1","path":"/srv/app","displayName":"app"}]},"_meta":{"runtimeId":"r"}}'
  ;;
"worktree create")
  echo '{"id":"6","ok":true,"result":{"worktree":{"id":"repo1::/srv/app/new-feat"}},"_meta":{"runtimeId":"r"}}'
  ;;
"agent launch")
  echo '{"id":"7","ok":false,"error":{"code":"runtime_unavailable","message":"Orca is not running"}}'
  exit 1
  ;;
esac
`

type fakeTG struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (f *fakeTG) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var p map[string]any
	_ = json.Unmarshal(body, &p)
	if strings.HasSuffix(r.URL.Path, "/sendMessage") {
		f.mu.Lock()
		f.sent = append(f.sent, p)
		f.mu.Unlock()
	}
	_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
}

func (f *fakeTG) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeTG) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func setup(t *testing.T) (*Bot, *fakeTG, string) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "orca-ide")
	if err := os.WriteFile(bin, []byte(fakeOrca), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("working"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DIR", dir)
	f := &fakeTG{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	tg, err := newTG("TOKEN", srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	b := NewBot(tg, &Orca{bin: bin, timeout: 10 * time.Second}, map[int64]bool{42: true}, "claude", filepath.Join(dir, "state.json"))
	return b, f, dir
}

func msg(text string) Update {
	return Update{Message: &Message{Chat: Chat{ID: 42}, From: &User{ID: 42}, Text: text}}
}

func text(m map[string]any) string { s, _ := m["text"].(string); return s }

func calls(t *testing.T, dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
	return string(b)
}

func TestFlow(t *testing.T) {
	b, f, dir := setup(t)
	ctx := context.Background()

	// Чужой пользователь игнорируется.
	b.HandleUpdate(ctx, Update{Message: &Message{Chat: Chat{ID: 7}, From: &User{ID: 7}, Text: "/ws"}})
	if f.count() != 0 {
		t.Fatal("ответили чужому пользователю")
	}

	// Без выбора воркспейса текст не уходит.
	b.HandleUpdate(ctx, msg("привет"))
	if !strings.Contains(text(f.last()), "не выбран") {
		t.Fatalf("ожидали подсказку выбрать воркспейс: %q", text(f.last()))
	}

	// /ws — список и кнопка выбора.
	b.HandleUpdate(ctx, msg("/ws"))
	ws := f.last()
	if !strings.Contains(text(ws), "fix-login") || !strings.Contains(text(ws), "работает (Bash)") || !strings.Contains(text(ws), "🖧 linux-box") {
		t.Fatalf("список воркспейсов: %q", text(ws))
	}
	kb := ws["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	data := kb[0].([]any)[0].(map[string]any)["callback_data"].(string)
	if !strings.HasPrefix(data, "sel:") || len(data) > 64 {
		t.Fatalf("callback_data: %q", data)
	}

	// Нажатие кнопки выбирает воркспейс и терминал агента (по paneKey = tabId:leafId).
	b.HandleUpdate(ctx, Update{CallbackQuery: &CallbackQuery{ID: "q", From: User{ID: 42}, Message: &Message{Chat: Chat{ID: 42}}, Data: data}})
	if cur := text(f.last()); !strings.Contains(cur, "claude") || !strings.Contains(cur, "&lt;зелёные&gt;") {
		t.Fatalf("/cur: %q", cur)
	}

	// Текст уходит в терминал агента, а не в shell.
	b.HandleUpdate(ctx, msg("сделай ревью"))
	if c := calls(t, dir); !strings.Contains(c, "terminal send --terminal term-agent --text сделай ревью --enter --wait-submit 10 --json") {
		t.Fatalf("вызов send:\n%s", c)
	}

	// /send для текста со слешем.
	b.HandleUpdate(ctx, msg("/send /compact"))
	if c := calls(t, dir); !strings.Contains(c, "--text /compact --enter") {
		t.Fatalf("/send:\n%s", c)
	}

	// /read — экран в <pre>, HTML экранирован, хвостовые пустые строки срезаны.
	b.HandleUpdate(ctx, msg("/read"))
	if r := text(f.last()); !strings.Contains(r, "<pre>&gt; hello\n  ⎿ ok &lt;b&gt;</pre>") {
		t.Fatalf("/read: %q", r)
	}

	// /stop — прерывание.
	b.HandleUpdate(ctx, msg("/stop"))
	if c := calls(t, dir); !strings.Contains(c, "terminal send --terminal term-agent --interrupt --json") {
		t.Fatalf("/stop:\n%s", c)
	}

	// Ошибка Orca показывается пользователю с кодом.
	b.HandleUpdate(ctx, msg("/agent codex напиши тесты"))
	if r := text(f.last()); !strings.Contains(r, "runtime_unavailable") {
		t.Fatalf("ошибка agent launch: %q", r)
	}
	if c := calls(t, dir); !strings.Contains(c, "agent launch --agent codex --worktree id:repo1::/srv/app/fix-login --surface terminal --prompt напиши тесты --json") {
		t.Fatalf("agent launch:\n%s", c)
	}

	// /new — поиск репо по имени, задача целиком.
	b.HandleUpdate(ctx, msg("/new app new-feat добавь экспорт в CSV"))
	if c := calls(t, dir); !strings.Contains(c, "worktree create --repo id:repo1 --name new-feat --agent claude --surface terminal --no-parent --prompt добавь экспорт в CSV --json") {
		t.Fatalf("worktree create:\n%s", c)
	}
	if b.chat(42).WorktreeID != "repo1::/srv/app/new-feat" {
		t.Fatalf("после /new не выбран новый воркtree: %+v", b.chat(42))
	}

	// Обрыв SSH: ввод не отправляется, пользователю объясняют причину.
	b.updateChat(42, func(c *chatState) { c.WorktreeID = "repo1::/srv/app/fix-login"; c.Terminal = "" })
	_ = os.WriteFile(filepath.Join(dir, "verdict"), []byte("unverifiable"), 0o644)
	before := strings.Count(calls(t, dir), "terminal send")
	b.HandleUpdate(ctx, msg("ты тут?"))
	if r := text(f.last()); !strings.Contains(r, "Связь с хостом «linux-box» потеряна") {
		t.Fatalf("обрыв SSH: %q", r)
	}
	if strings.Count(calls(t, dir), "terminal send") != before {
		t.Fatal("при обрыве SSH ввод всё равно ушёл")
	}

	// Состояние сохранено на диск.
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal("state не сохранён")
	}
}

func TestWatchNotifies(t *testing.T) {
	b, f, dir := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Watch(ctx, 200*time.Millisecond)

	time.Sleep(500 * time.Millisecond)
	if f.count() != 0 {
		t.Fatal("уведомление на первом снимке")
	}
	_ = os.WriteFile(filepath.Join(dir, "status"), []byte("done"), 0o644)
	deadline := time.Now().Add(3 * time.Second)
	for f.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	n := text(f.last())
	if !strings.Contains(n, "агент закончил") || !strings.Contains(n, "PR открыт") {
		t.Fatalf("уведомление: %q", n)
	}

	// После /notify off уведомлений нет.
	b.HandleUpdate(ctx, msg("/notify off"))
	before := f.count()
	_ = os.WriteFile(filepath.Join(dir, "status"), []byte("permission"), 0o644)
	time.Sleep(700 * time.Millisecond)
	if f.count() != before {
		t.Fatalf("пришло уведомление при выключенных: %q", text(f.last()))
	}
}
