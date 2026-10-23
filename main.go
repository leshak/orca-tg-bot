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
	if token == "" {
		log.Fatal("не задан TELEGRAM_BOT_TOKEN")
	}
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

	poll, err := time.ParseDuration(env("POLL_INTERVAL", "5s"))
	if err != nil || poll < time.Second {
		log.Fatalf("POLL_INTERVAL: нужна длительность не меньше 1s, например 5s")
	}

	tg, err := newTG(token, env("TELEGRAM_API_URL", "https://api.telegram.org"), env("TELEGRAM_PROXY", ""))
	if err != nil {
		log.Fatal(err)
	}

	bot := NewBot(tg, orca, allowed, env("DEFAULT_AGENT", "claude"), env("STATE_FILE", "orca-tg-bot.state.json"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	var names []string
	for _, t := range orca.Targets(ctx) {
		if t.Env == "" {
			names = append(names, "local")
		} else {
			names = append(names, t.Name)
		}
	}
	log.Printf("запущен: orca=%s, рантаймы: %s, пользователей в allowlist: %d", orca.bin, strings.Join(names, ", "), len(allowed))
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
	log.Print("остановлен")
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
