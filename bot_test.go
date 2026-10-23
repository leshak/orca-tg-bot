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
remote=""
case "$*" in *"--environment env-1"*) remote=1 ;; esac
if [ -n "$remote" ] && [ -f "$FAKE_DIR/remote_down" ]; then
  echo '{"id":"r","ok":false,"error":{"code":"runtime_unavailable","message":"server unreachable"}}'
  exit 1
fi
case "$1 $2" in
"environment list")
  echo '{"id":"e","ok":true,"result":{"environments":[{"id":"env-1","name":"devbox"}]},"_meta":{"runtimeId":"local"}}'
  ;;
"worktree ps")
  if [ -n "$remote" ]; then
    rst=$(cat "$FAKE_DIR/remote_status" 2>/dev/null || echo working)
    echo '{"id":"1r","ok":true,"result":{"worktrees":[{"worktreeId":"repo9::/home/u/geo","repoId":"repo9","repo":"geo","path":"/home/u/geo","branch":"","displayName":"geo","status":"'$rst'","hostId":"local","isArchived":false,"lastActivityAt":0,"agents":[{"paneKey":"tR:lR","parentPaneKey":null,"state":"working","agentType":"claude","lastAssistantMessage":"Геокодер готов","updatedAt":1}]}],"totalCount":1,"truncated":false},"_meta":{"runtimeId":"rr"}}'
    exit 0
  fi
  st=$(cat "$FAKE_DIR/status")
  cat <<EOF
{"id":"1","ok":true,"result":{"worktrees":[{"worktreeId":"repo1::/srv/app/fix-login","repoId":"repo1","repo":"app","path":"/srv/app/fix-login","branch":"fix-login","displayName":"fix-login","workspaceStatus":"in-progress","comment":"чиню логин","unread":false,"isArchived":false,"status":"$st","hostId":"ssh:tgt-1","liveTerminalCount":1,"lastActivityAt":1,"agents":[{"paneKey":"tab1:leaf1","parentPaneKey":null,"state":"working","agentType":"claude","prompt":"fix","displayName":null,"lastAssistantMessage":"Готово: тесты <зелёные> & PR открыт","toolName":"Bash","interrupted":false,"updatedAt":1}]}],"totalCount":1,"truncated":false},"_meta":{"runtimeId":"r"}}
EOF
  ;;
"terminal list")
  if [ -n "$remote" ]; then
    echo '{"id":"2r","ok":true,"result":{"terminals":[{"handle":"term-remote","worktreeId":"repo9::/home/u/geo","tabId":"tR","leafId":"lR","title":"claude","connected":true,"writable":true,"agentIdentity":"claude"}]},"_meta":{"runtimeId":"rr"}}'
    exit 0
  fi
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
  if [ -n "$remote" ]; then
    echo '{"id":"5r","ok":true,"result":{"repos":[{"id":"repo9","path":"/home/u/geo","displayName":"geo"},{"id":"repoA","path":"/home/u/app","displayName":"app"}]},"_meta":{"runtimeId":"rr"}}'
    exit 0
  fi
  echo '{"id":"5","ok":true,"result":{"repos":[{"id":"repo1","path":"/srv/app","displayName":"app"}]},"_meta":{"runtimeId":"r"}}'
  ;;
"worktree create")
  if [ -n "$remote" ]; then
    echo '{"id":"6r","ok":true,"result":{"worktree":{"id":"repoA::/home/u/app/x"}},"_meta":{"runtimeId":"rr"}}'
    exit 0
  fi
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
	if !strings.Contains(text(ws), "fix-login") || !strings.Contains(text(ws), "<i>работает · Bash</i>") || !strings.Contains(text(ws), "<b>🌐 linux-box</b>") {
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

	// /new — поиск репо по имени (с указанием сервера), задача целиком.
	b.HandleUpdate(ctx, msg("/new local/app new-feat добавь экспорт в CSV"))
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

// Сопряжённый сервер Orca (environment): его воркспейсы видны вместе с локальными,
// а все команды к ним уходят с --environment.
func TestRemoteEnvironment(t *testing.T) {
	b, f, dir := setup(t)
	ctx := context.Background()

	b.HandleUpdate(ctx, msg("/ws"))
	ws := f.last()
	if !strings.Contains(text(ws), "fix-login") || !strings.Contains(text(ws), "<b>geo</b>") || !strings.Contains(text(ws), "<b>🌐 devbox</b>") {
		t.Fatalf("список воркспейсов: %q", text(ws))
	}
	var data string
	for _, row := range ws["reply_markup"].(map[string]any)["inline_keyboard"].([]any) {
		for _, btn := range row.([]any) {
			if m := btn.(map[string]any); strings.Contains(m["text"].(string), "geo") {
				data = m["callback_data"].(string)
			}
		}
	}
	if data == "" {
		t.Fatal("нет кнопки удалённого воркспейса")
	}
	b.HandleUpdate(ctx, Update{CallbackQuery: &CallbackQuery{ID: "q", From: User{ID: 42}, Message: &Message{Chat: Chat{ID: 42}}, Data: data}})
	if cur := text(f.last()); !strings.Contains(cur, "Геокодер готов") || !strings.Contains(cur, "🌐 devbox") {
		t.Fatalf("/cur: %q", cur)
	}
	if st := b.chat(42); st.Env != "env-1" || st.WorktreeID != "repo9::/home/u/geo" {
		t.Fatalf("выбор: %+v", st)
	}

	b.HandleUpdate(ctx, msg("пример URL"))
	b.HandleUpdate(ctx, msg("/read"))
	b.HandleUpdate(ctx, msg("/stop"))
	c := calls(t, dir)
	for _, want := range []string{
		"terminal list --worktree id:repo9::/home/u/geo --json --environment env-1",
		"terminal send --terminal term-remote --text пример URL --enter --wait-submit 10 --json --environment env-1",
		"terminal read --terminal term-remote --screen --json --environment env-1",
		"terminal send --terminal term-remote --interrupt --json --environment env-1",
	} {
		if !strings.Contains(c, want) {
			t.Fatalf("нет вызова %q:\n%s", want, c)
		}
	}

	// /new: «app» есть и тут, и на сервере — нужно уточнение; «devbox/app» — создаётся на сервере.
	b.HandleUpdate(ctx, msg("/new app x"))
	if r := text(f.last()); !strings.Contains(r, "нескольким") {
		t.Fatalf("неоднозначное /new: %q", r)
	}
	b.HandleUpdate(ctx, msg("/new devbox/app x задача"))
	if c := calls(t, dir); !strings.Contains(c, "worktree create --repo id:repoA --name x --agent claude --surface terminal --no-parent --prompt задача --json --environment env-1") {
		t.Fatalf("worktree create на сервере:\n%s", c)
	}
	if st := b.chat(42); st.Env != "env-1" || st.WorktreeID != "repoA::/home/u/app/x" {
		t.Fatalf("после /new: %+v", st)
	}

	// Сервер недоступен: локальные воркспейсы всё равно показываются.
	_ = os.WriteFile(filepath.Join(dir, "remote_down"), nil, 0o644)
	b.HandleUpdate(ctx, msg("/ws"))
	if r := text(f.last()); !strings.Contains(r, "🌐 devbox</b>\n<blockquote>🔌 не отвечает") || !strings.Contains(r, "fix-login") {
		t.Fatalf("/ws при недоступном сервере: %q", r)
	}
}

// Уведомление о завершении агента на сопряжённом сервере.
func TestWatchRemote(t *testing.T) {
	b, f, dir := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Watch(ctx, 200*time.Millisecond)

	time.Sleep(500 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(dir, "remote_status"), []byte("done"), 0o644)
	deadline := time.Now().Add(3 * time.Second)
	for f.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := text(f.last()); !strings.Contains(n, "<b>geo</b>: агент закончил") || !strings.Contains(n, "🌐 devbox") {
		t.Fatalf("уведомление: %q", n)
	}
}
