package main

// Веб-интерфейс против того же фейкового Orca CLI, что и бот.

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testWebToken = "secret-token-xyz"

func setupWeb(t *testing.T) (*httptest.Server, *http.Client, string) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "orca-ide")
	if err := os.WriteFile(bin, []byte(fakeOrca), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("working"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DIR", dir)
	w := NewWeb(&Orca{bin: bin, timeout: 10 * time.Second}, "claude", testWebToken, 100*time.Millisecond, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go w.run(ctx)
	srv := httptest.NewServer(w.routes())
	t.Cleanup(func() { cancel(); srv.CloseClientConnections(); srv.Close() })

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	return srv, c, dir
}

func login(t *testing.T, srv *httptest.Server, c *http.Client) {
	resp, err := c.PostForm(srv.URL+"/login", url.Values{"token": {testWebToken}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/" || resp.StatusCode != 200 {
		t.Fatalf("после входа: %s %d", resp.Request.URL, resp.StatusCode)
	}
}

func postJSON(t *testing.T, c *http.Client, u, body string, hdr ...string) (int, map[string]string) {
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestWebAuth(t *testing.T) {
	srv, c, _ := setupWeb(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, _ := noRedirect.Get(srv.URL + "/")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("без входа / должен вести на /login, а не %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = c.Get(srv.URL + "/api/ping")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("без входа API отвечает %d", resp.StatusCode)
	}
	if code, _ := postJSON(t, c, srv.URL+"/api/send", `{}`); code != http.StatusUnauthorized {
		t.Fatalf("без входа send: %d", code)
	}

	login(t, srv, c)
	resp, _ = c.Get(srv.URL + "/api/ping")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("после входа ping: %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("нет CSP: %q", csp)
	}
	if rp := resp.Header.Get("Referrer-Policy"); rp == "no-referrer" {
		t.Errorf("при no-referrer браузер шлёт Origin: null и свои POST отклоняются")
	}

	// Свои запросы — как их шлёт браузер по HTTP: с Origin нашего адреса.
	own := srv.URL
	if code, _ := postJSON(t, c, srv.URL+"/api/key", `{"key":"rm"}`, "Origin", own); code != http.StatusBadRequest {
		t.Errorf("свой Origin не пропущен: %d", code)
	}
	if code, _ := postJSON(t, c, srv.URL+"/api/key", `{"key":"rm"}`, "Origin", "null", "Sec-Fetch-Site", "same-origin"); code != http.StatusBadRequest {
		t.Errorf("Sec-Fetch-Site: same-origin не пропущен: %d", code)
	}
	// С чужого сайта — нет, даже с cookie.
	for _, h := range [][]string{
		{"Origin", "http://evil.example"},
		{"Origin", "null"},
		{"Origin", own, "Sec-Fetch-Site", "cross-site"},
		{"Origin", "http://evil.example", "Sec-Fetch-Site", "same-site"},
	} {
		if code, _ := postJSON(t, c, srv.URL+"/api/send", `{"text":"x"}`, h...); code != http.StatusForbidden {
			t.Errorf("чужой запрос %v: %d", h, code)
		}
	}

	// Форма входа из браузера тоже несёт Origin.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/login", strings.NewReader("token="+testWebToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", own)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("вход с Origin: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestWebCommands(t *testing.T) {
	srv, c, dir := setupWeb(t)
	login(t, srv, c)
	wt := `"wt":"repo1::/srv/app/fix-login"`

	// Без терминала — пишем основному агенту.
	code, out := postJSON(t, c, srv.URL+"/api/send", `{"env":"",`+wt+`,"text":"привет"}`)
	if code != 200 || out["term"] != "term-agent" {
		t.Fatalf("send: %d %v", code, out)
	}
	code, _ = postJSON(t, c, srv.URL+"/api/key", `{"env":"",`+wt+`,"term":"term-shell","key":"esc"}`)
	if code != 200 {
		t.Fatalf("key esc: %d", code)
	}
	code, _ = postJSON(t, c, srv.URL+"/api/key", `{"env":"",`+wt+`,"key":"ctrl-c"}`)
	if code != 200 {
		t.Fatalf("key ctrl-c: %d", code)
	}
	log := calls(t, dir)
	for _, want := range []string{
		"terminal send --terminal term-agent --text привет --enter",
		"terminal send --terminal term-shell --text \x1b --json",
		"terminal send --terminal term-agent --interrupt",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("нет вызова %q в:\n%s", want, log)
		}
	}

	bad := []struct{ name, body string }{
		{"неизвестная клавиша", `{"env":"",` + wt + `,"key":"rm -rf"}`},
		{"чужой терминал", `{"env":"",` + wt + `,"term":"term-other","text":"x"}`},
		{"неизвестный рантайм", `{"env":"--evil",` + wt + `,"text":"x"}`},
		{"пустой текст", `{"env":"",` + wt + `,"text":"  "}`},
		{"ветка с минусом", `{"env":"","repo":"repo1","name":"--force"}`},
		{"неизвестный агент", `{"env":"",` + wt + `,"agent":"sh"}`},
	}
	for _, b := range bad {
		path := "/api/send"
		switch {
		case strings.Contains(b.body, `"key"`):
			path = "/api/key"
		case strings.Contains(b.body, `"repo"`):
			path = "/api/worktree"
		case strings.Contains(b.body, `"agent"`):
			path = "/api/agent"
		}
		if code, out := postJSON(t, c, srv.URL+path, b.body); code != http.StatusBadRequest {
			t.Errorf("%s: ждали 400, а %d %v", b.name, code, out)
		}
	}

	code, out = postJSON(t, c, srv.URL+"/api/worktree", `{"env":"","repo":"repo1","name":"new-feat","prompt":"сделай"}`)
	if code != 200 || out["wt"] != "repo1::/srv/app/new-feat" {
		t.Fatalf("worktree: %d %v", code, out)
	}
	if log := calls(t, dir); !strings.Contains(log, "worktree create --repo id:repo1 --name new-feat --agent claude --surface terminal --no-parent --prompt сделай") {
		t.Errorf("worktree create не вызван как надо:\n%s", log)
	}
}

func TestWebEvents(t *testing.T) {
	srv, c, _ := setupWeb(t)
	login(t, srv, c)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	q := url.Values{"env": {""}, "wt": {"repo1::/srv/app/fix-login"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events?"+q.Encode(), nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}

	got := map[string]string{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	event := ""
	for sc.Scan() && (got["state"] == "" || got["terms"] == "" || got["screen"] == "") {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			got[event] = strings.TrimPrefix(line, "data: ")
		}
	}

	var snap webSnapshot
	if err := json.Unmarshal([]byte(got["state"]), &snap); err != nil {
		t.Fatalf("state: %v: %s", err, got["state"])
	}
	byName := map[string]webProject{}
	for _, p := range snap.Projects {
		byName[p.Name] = p
	}
	app := byName["App Project"]
	if len(app.Workspaces) != 1 || app.Workspaces[0].Name != "fix-login" || app.Workspaces[0].State != "working" {
		t.Errorf("воркспейс не попал в проект App Project: %+v", app)
	}
	if app.Workspaces[0].Host != "linux-box" {
		t.Errorf("хост: %q", app.Workspaces[0].Host)
	}
	if p, ok := byName["Empty"]; !ok || len(p.Workspaces) != 0 || len(p.Repos) != 1 {
		t.Errorf("пустой проект тоже нужен (для «+ воркtree»): %+v", p)
	}
	// У старого сервера нет `project list` — проектом становится репозиторий.
	if geo := byName["geo"]; len(geo.Workspaces) != 1 || geo.Workspaces[0].Env != "env-1" {
		t.Errorf("воркспейс со старого сервера: %+v", geo)
	}

	var terms webTerms
	_ = json.Unmarshal([]byte(got["terms"]), &terms)
	if terms.Active != "term-agent" || terms.Default != "term-agent" || len(terms.Terms) != 2 {
		t.Errorf("terms: %+v", terms)
	}
	var scr webScreen
	_ = json.Unmarshal([]byte(got["screen"]), &scr)
	if scr.Handle != "term-agent" || !strings.Contains(scr.Raw, "> hello") {
		t.Errorf("screen: %+v", scr)
	}
}
