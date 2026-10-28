// orca-tg-bot — Telegram-бот для управления агентами в Orca (stablyai/orca) через Orca CLI.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("orca-tg-bot: ")

	token := env("TELEGRAM_BOT_TOKEN", "")
	webAddr := env("WEB_ADDR", "")
	if token == "" && webAddr == "" {
		log.Fatal("не задан ни TELEGRAM_BOT_TOKEN, ни WEB_ADDR — нечего запускать")
	}

	// На Linux CLI называется orca-ide: /usr/bin/orca — это экранный диктор GNOME.
	defBin := "orca"
	if runtime.GOOS == "linux" {
		defBin = "orca-ide"
	}
	orcaTimeout, err := time.ParseDuration(env("ORCA_TIMEOUT", "60s"))
	if err != nil {
		log.Fatalf("ORCA_TIMEOUT: %v", err)
	}
	orca := &Orca{bin: env("ORCA_BIN", defBin), timeout: orcaTimeout, targets: parseTargets(env("ORCA_ENVIRONMENT", "auto"))}
	agent := env("DEFAULT_AGENT", "claude")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var names []string
	for _, t := range orca.Targets(ctx) {
		if t.Env == "" {
			names = append(names, "local")
		} else {
			names = append(names, t.Name)
		}
	}
	log.Printf("orca=%s, рантаймы: %s", orca.bin, strings.Join(names, ", "))

	done := make(chan struct{}, 2)
	running := 0
	if webAddr != "" {
		web := newWebFromEnv(orca, agent)
		running++
		go func() {
			defer func() { done <- struct{}{} }()
			log.Printf("веб: http://%s", webAddr)
			if err := web.Serve(ctx, webAddr); err != nil {
				log.Fatalf("веб: %v", err)
			}
		}()
	}
	if token != "" {
		running++
		go func() {
			defer func() { done <- struct{}{} }()
			runTelegram(ctx, token, orca, agent)
		}()
	}
	for ; running > 0; running-- {
		<-done
	}
	log.Print("остановлен")
}

// newWebFromEnv настраивает веб-интерфейс из WEB_*.
func newWebFromEnv(orca *Orca, agent string) *Web {
	tok := env("WEB_TOKEN", "")
	if len([]rune(tok)) < 12 {
		// Веб даёт полный доступ к агентам — без длинного токена не запускаемся.
		log.Fatal("WEB_TOKEN: нужен токен не короче 12 символов, например: openssl rand -hex 16")
	}
	poll, err := time.ParseDuration(env("WEB_POLL_INTERVAL", "2s"))
	if err != nil || poll < 500*time.Millisecond {
		log.Fatal("WEB_POLL_INTERVAL: нужна длительность не меньше 500ms, например 2s")
	}
	scr, err := time.ParseDuration(env("WEB_SCREEN_INTERVAL", "1s"))
	if err != nil || scr < 200*time.Millisecond {
		log.Fatal("WEB_SCREEN_INTERVAL: нужна длительность не меньше 200ms, например 1s")
	}
	return NewWeb(orca, agent, tok, poll, scr)
}

// runTelegram — бот: уведомления и long polling до отмены ctx.
func runTelegram(ctx context.Context, token string, orca *Orca, agent string) {
	allowed := map[int64]bool{}
	for _, s := range strings.Split(env("TELEGRAM_ALLOWED_IDS", ""), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			log.Fatalf("TELEGRAM_ALLOWED_IDS: неверный id %q", s)
		}
		allowed[id] = true
	}
	if len(allowed) == 0 {
		// Бот даёт полный доступ к агентам и, значит, к серверу — без allowlist не запускаемся.
		log.Fatal("не задан TELEGRAM_ALLOWED_IDS (ваш числовой Telegram id; узнать можно у @userinfobot)")
	}

	poll, err := time.ParseDuration(env("POLL_INTERVAL", "5s"))
	if err != nil || poll < time.Second {
		log.Fatalf("POLL_INTERVAL: нужна длительность не меньше 1s, например 5s")
	}

	tg, err := newTG(token, env("TELEGRAM_API_URL", "https://api.telegram.org"), env("TELEGRAM_PROXY", ""))
	if err != nil {
		log.Fatal(err)
	}

	bot := NewBot(tg, orca, allowed, agent, env("STATE_FILE", "orca-tg-bot.state.json"))

	if err := tg.setCommands(ctx, [][2]string{
		{"ws", "Воркспейсы и агенты"},
		{"cur", "Что выбрано"},
		{"read", "Экран терминала агента"},
		{"stop", "Прервать агента"},
		{"term", "Выбрать терминал"},
		{"send", "Отправить текст со слешем"},
		{"repos", "Репозитории"},
		{"new", "Новый воркtree с агентом"},
		{"agent", "Ещё один агент в воркспейсе"},
		{"notify", "Уведомления on/off"},
		{"help", "Справка"},
	}); err != nil {
		log.Printf("setMyCommands: %v", err)
	}

	go bot.Watch(ctx, poll)

	log.Printf("telegram: пользователей в allowlist: %d", len(allowed))
	var offset int64
	for ctx.Err() == nil {
		ups, err := tg.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("getUpdates: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			// Каждое обновление — в своей горутине: вызовы CLI бывают по несколько секунд.
			go bot.HandleUpdate(ctx, u)
		}
	}
}

// parseTargets разбирает ORCA_ENVIRONMENT: "auto" (по умолчанию) — эта машина и все
// сопряжённые серверы Orca; иначе список через запятую, где "local" — локальный рантайм.
func parseTargets(v string) []Target {
	if strings.EqualFold(v, "auto") {
		return nil
	}
	var out []Target
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		switch {
		case s == "":
		case strings.EqualFold(s, "local"):
			out = append(out, Target{})
		default:
			out = append(out, Target{Env: s, Name: s})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
