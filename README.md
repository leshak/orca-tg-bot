# orca-tg-bot

**[English](#english)** · **[Русский](#русский)**

## English

A Telegram bot for [Orca](https://github.com/stablyai/orca): list workspaces and agents, chat with an agent, view its terminal screen, interrupt it, create worktrees, and get notified when an agent finishes or is waiting for permission.

The bot talks to Orca only through the official CLI (`orca-ide … --json`) — no custom API and no Orca cloud relay. Telegram is polled with long polling, so no inbound ports need to be opened. No external dependencies — just the Go standard library.

The bot's interface (replies and buttons) is in Russian.

There is also an optional [web interface](#web-interface) for the local network (home LAN, ZeroTier, Tailscale): projects as in Orca's sidebar, live terminal screens, input for agents. Telegram and the web interface can run together or separately.

### Commands

| Command | What it does |
|---|---|
| `/ws` | Workspaces with agent status; pick one with a button |
| `/cur` | Current selection + the agent's last reply |
| *any text* | Send it to the selected agent (Enter is pressed for you) |
| `/send /compact` | Send text that starts with a slash (Claude Code slash commands) |
| `/read [N]` | The agent's current terminal screen, tidied up for a phone: terminal line wraps are joined, separators are shortened. N — last N lines (default: the whole screen). The «🔄 Обновить» (Refresh) button redraws the same message |
| `/stop` | Interrupt the agent (Ctrl+C) |
| `/term` | Pick a specific terminal if the workspace has several |
| `/repos` | Repositories in Orca |
| `/new app fix-login fix the login` | New worktree in repo `app`, branch `fix-login`; the agent gets the task right away |
| `/agent [codex] [task]` | Another agent in the selected workspace |
| `/notify on\|off` | Notifications |

Notifications arrive when a workspace's status changes from "working" to anything else (the agent finished) and when an agent is waiting for permission. A notification includes the agent's last reply and «Ответить» (Reply) and «Экран» (Screen) buttons.

### Web interface

Set `WEB_ADDR` and `WEB_TOKEN` and the same binary serves a web page:

```bash
WEB_ADDR=10.147.17.5:8080            # listen on the ZeroTier/LAN address, not 0.0.0.0
WEB_TOKEN=$(openssl rand -hex 16)    # at least 12 characters; asked once, then a cookie
```

- **Projects** — as in Orca's sidebar (`project list` of every runtime; on older servers without it, repositories are used). Inside a project are its workspaces with status, host and agents; «+» creates a new worktree with an agent.
- **Workspace** — tabs for its terminals, the screen of the selected one refreshed about once a second, the agent's last reply, a box for messages (Enter sends on a computer; on a phone, use the button) and keys for menus and permission prompts: Esc, ↑, ↓, Enter, Tab, ⇧Tab, 1–3 and «Стоп» (Ctrl+C). «+ Агент» starts another agent.
- **Live updates** go over Server-Sent Events: the page holds one connection and reconnects by itself. Orca has no event stream, so the server polls the CLI (statuses every `WEB_POLL_INTERVAL`, default 2s, only while a page is open; the open screen every `WEB_SCREEN_INTERVAL`, default 1s) and sends only changes.
- **Telegram is optional**: without `TELEGRAM_BOT_TOKEN` only the web interface runs.

The page shows the terminal as text, without colors: that is what `terminal read --screen` returns. A full interactive terminal (mouse, arbitrary TUI) isn't possible through the CLI.

**Security.** Whoever opens the page controls the agents, and through them the machine. So the server refuses to start without `WEB_TOKEN`, the login is a long token with an `HttpOnly`, `SameSite=Strict` cookie, POST requests from other sites are rejected by `Origin`, and the page is served with a strict CSP. There is no HTTPS: listen only on a trusted network (ZeroTier and Tailscale encrypt traffic themselves) and never expose the port to the internet.

### How it works

| Bot action | Orca CLI call |
|---|---|
| list and notifications | `worktree ps` (`status` field: `working`, `permission`, `done`, …; `agents[].lastAssistantMessage`) |
| find the agent's terminal | `terminal list --worktree id:<repoId>::<path>`; the agent's terminal is the one whose `tabId:leafId` matches the agent's `paneKey` |
| message the agent | `terminal send --terminal <h> --text … --enter --wait-submit 10` |
| screen | `terminal read --terminal <h> --screen` |
| stop | `terminal send --terminal <h> --interrupt` |
| new worktree | `worktree create --repo id:… --name … --agent claude --surface terminal --no-parent --prompt …` |
| another agent | `agent launch --agent … --worktree id:… --surface terminal` |

### Where to run it

Run the bot on the same machine and as the same user as Orca (desktop app or `orca serve`): the CLI finds the running runtime through its metadata in the user's profile.

**Orca on a Mac, some worktrees on Linux over SSH.** Install the bot on the Mac next to Orca. SSH worktrees belong to the Mac runtime: `worktree ps` returns them together with local ones (with `hostId: "ssh:…"`), agent statuses arrive over SSH just like local ones, and Orca proxies `terminal send/read` through its relay on the host. You don't need a separate bot on Linux. Such worktrees are marked `🌐 <SSH target name>` in the list.

If the connection to the SSH host drops, the bot sends nothing and reports that the connection to the host is lost. The agent on the host keeps working (Orca keeps the terminals on the host and reconnects by itself). Once the connection is back, you'll get the usual completion notification.

**Orca server on a remote machine (Remote Orca Server, `orca environment list`).** If Orca runs on an SSH host as a separate server, its workspaces belong to that server rather than to the Mac runtime, and a plain `worktree ps` doesn't show them. The bot discovers such servers via `environment list` (refreshed once a minute), polls them along with the local Orca, and sends commands to their workspaces with `--environment`. They're marked `🌐 <server name>` in the list. If a server doesn't respond, local workspaces are still shown, and `/ws` shows «🔌 не отвечает» (not responding) for that server. In `/new` you can name the server explicitly: `/new devbox/repo branch task`.

The Mac must not sleep: System Settings → Energy → "Prevent automatic sleeping". For autostart, use `dev.orca-tg-bot.plist` (a LaunchAgent; instructions are inside the file).

### Installing on Linux

1. Check that the CLI can see Orca:
   ```bash
   orca-ide status --json
   orca-ide worktree ps --json
   ```
   If the command is missing, register the CLI in Orca: Settings → General → Orca CLI. On Linux the command is `orca-ide`; plain `orca` is the GNOME screen reader. On macOS it's `orca` (the bot picks the name based on the OS; `ORCA_BIN` overrides it).
2. Create a bot with @BotFather and get your id from @userinfobot.
3. Build and install:
   ```bash
   go install github.com/leshak/orca-tg-bot@latest   # the binary goes to ~/go/bin
   ```
   Or from a clone of the repo: `go build -o orca-tg-bot .`. For another architecture: `GOOS=linux GOARCH=arm64 go build -o orca-tg-bot .`
4. Fill in `~/.config/orca-tg-bot/env` following `.env.example` and start the service as described in `orca-tg-bot.service`.
5. Send the bot `/start`, then `/ws`.

`ORCA_ENVIRONMENT` sets which Orca runtimes to poll. By default (`auto`) that's this machine plus all paired servers from `orca-ide environment list`. You can list them explicitly, comma-separated: `local,devbox`; if the bot doesn't run on the machine with Orca, list just the server name.

### Caveats

- **Orca's native chat.** The CLI can only write to terminals. An agent opened as a chat (Settings → Chat UI) can't be controlled through the bot. Agents launched by the bot itself always start in a terminal (`--surface terminal`). For ones already started from the desktop app, turn off Chat UI or run agents in a terminal.
- **Access.** Whoever can message the bot effectively runs commands on the server through the agent. That's why `TELEGRAM_ALLOWED_IDS` is required; the bot ignores everyone else.
- **Multi-line text** is passed to `terminal send` as is. How a particular agent handles it (as one submission or line by line) hasn't been tested; a single line is safer.
- **Telegram not directly reachable?** Set `TELEGRAM_PROXY` (`socks5://` is supported).
- Terminal handles live within a runtime: after Orca restarts, a manual `/term` choice is reset and the bot finds the agent's terminal on its own again.

### Tests

```bash
go test -v ./...
```

The tests run the real bot against a fake `orca-ide` (a shell script answering in the runtime's format) and a fake Bot API: workspace selection, sending to the agent's terminal, `/read`, `/stop`, `/new`, Orca errors, notifications and turning them off, workspaces on a paired Orca server (`--environment`) and that server going down. The web interface is tested the same way: login and `Origin` checks, sending text and keys, input validation, grouping into projects and the SSE stream.

### License

[MIT](LICENSE)

## Русский

Telegram-бот для [Orca](https://github.com/stablyai/orca): список воркспейсов и агентов, переписка с агентом, экран терминала, прерывание, создание воркtree и уведомления, когда агент закончил или ждёт разрешения.

Бот ходит в Orca только через официальный CLI (`orca-ide … --json`), без своего API и без облачного relay Orca. Telegram опрашивается long polling, поэтому порты наружу открывать не нужно. Внешних зависимостей нет — только стандартная библиотека Go.

Есть и необязательный [веб-интерфейс](#веб-интерфейс) для локальной сети (дом, ZeroTier, Tailscale): проекты как в боковой панели Orca, живые экраны терминалов, ввод для агентов. Telegram и веб работают вместе или по отдельности.

### Команды

| Команда | Что делает |
|---|---|
| `/ws` | Воркспейсы со статусом агента, выбор кнопкой |
| `/cur` | Что выбрано + последний ответ агента |
| *любой текст* | Отправить выбранному агенту (Enter нажимается сам) |
| `/send /compact` | Отправить текст, который начинается со слеша (slash-команды Claude Code) |
| `/read [N]` | Текущий экран терминала агента, подготовленный для телефона: склеены переносы терминала, разделители укорочены. N — последние N строк (по умолчанию весь экран). Кнопка «🔄 Обновить» перерисовывает это же сообщение |
| `/stop` | Прервать агента (Ctrl+C) |
| `/term` | Выбрать конкретный терминал, если в воркспейсе их несколько |
| `/repos` | Репозитории в Orca |
| `/new app fix-login почини логин` | Новый воркtree в репо `app`, ветка `fix-login`, агент сразу получает задачу |
| `/agent [codex] [задача]` | Ещё один агент в выбранном воркспейсе |
| `/notify on\|off` | Уведомления |

Уведомления приходят, когда статус воркспейса меняется с «работает» на любой другой (агент закончил) и когда агент ждёт разрешения. В уведомлении — последний ответ агента и кнопки «Ответить» и «Экран».

### Веб-интерфейс

Задайте `WEB_ADDR` и `WEB_TOKEN` — и тот же бинарник поднимет веб-страницу:

```bash
WEB_ADDR=10.147.17.5:8080            # адрес в ZeroTier/домашней сети, не 0.0.0.0
WEB_TOKEN=$(openssl rand -hex 16)    # не короче 12 символов; спрашивается один раз, дальше cookie
```

- **Проекты** — как в боковой панели Orca (`project list` всех рантаймов; на старых серверах без него проектом считается репозиторий). Внутри — воркспейсы со статусом, машиной и агентами; «+» создаёт новый воркtree с агентом.
- **Воркспейс** — вкладки его терминалов, экран выбранного (обновляется примерно раз в секунду), последний ответ агента, поле для сообщений (на компьютере Enter отправляет, на телефоне — кнопка) и клавиши для меню и запросов разрешений: Esc, ↑, ↓, Enter, Tab, ⇧Tab, 1–3 и «Стоп» (Ctrl+C). «+ Агент» запускает ещё одного агента.
- **Обновления в реальном времени** идут через Server-Sent Events: страница держит одно соединение и сама переподключается. Потока событий у Orca нет, поэтому сервер опрашивает CLI (статусы — раз в `WEB_POLL_INTERVAL`, по умолчанию 2s, и только пока страница открыта; открытый экран — раз в `WEB_SCREEN_INTERVAL`, по умолчанию 1s) и отправляет только изменения.
- **Telegram не обязателен**: без `TELEGRAM_BOT_TOKEN` работает только веб.

Терминал показывается текстом, без цветов: так его отдаёт `terminal read --screen`. Полноценный интерактивный терминал (мышь, произвольные TUI) через CLI не сделать.

**Безопасность.** Кто открыл страницу, тот управляет агентами, а через них и машиной. Поэтому без `WEB_TOKEN` сервер не запускается, вход — по длинному токену с cookie `HttpOnly` и `SameSite=Strict`, POST-запросы с чужих сайтов отсекаются по `Origin`, страница отдаётся со строгим CSP. HTTPS нет: слушайте только доверенную сеть (ZeroTier и Tailscale шифруют трафик сами) и не открывайте порт в интернет.

### Как это устроено

| Действие бота | Вызов Orca CLI |
|---|---|
| список и уведомления | `worktree ps` (поле `status`: `working`, `permission`, `done`, …; `agents[].lastAssistantMessage`) |
| найти терминал агента | `terminal list --worktree id:<repoId>::<path>`; терминал агента — тот, у которого `tabId:leafId` совпадает с `paneKey` агента |
| написать агенту | `terminal send --terminal <h> --text … --enter --wait-submit 10` |
| экран | `terminal read --terminal <h> --screen` |
| стоп | `terminal send --terminal <h> --interrupt` |
| новый воркtree | `worktree create --repo id:… --name … --agent claude --surface terminal --no-parent --prompt …` |
| ещё агент | `agent launch --agent … --worktree id:… --surface terminal` |

### Где запускать

Бот запускается на той же машине и от того же пользователя, что и Orca (десктоп или `orca serve`): CLI находит запущенный рантайм по его метаданным в профиле пользователя.

**Orca на Mac, часть воркtree по SSH на Linux.** Ставьте бота на Mac рядом с Orca. SSH-воркtree принадлежат рантайму на Mac: `worktree ps` отдаёт их вместе с локальными (с полем `hostId: "ssh:…"`), статусы агентов приходят по SSH так же, как локальные, а `terminal send/read` Orca проксирует через свой relay на хосте. Отдельно на Linux бот не нужен. В списке у таких воркtree есть метка `🌐 <имя SSH-цели>`.

Если связь с SSH-хостом пропала, бот ничего не отправляет и пишет «связь с хостом потеряна». Агент на хосте при этом продолжает работать (Orca держит терминалы на хосте и переподключается сама). Когда связь вернётся, придёт обычное уведомление о завершении.

**Сервер Orca на удалённой машине (Remote Orca Server, `orca environment list`).** Если Orca развёрнута на SSH-хосте как отдельный сервер, его воркспейсы принадлежат уже не рантайму на Mac, а этому серверу, и обычный `worktree ps` их не показывает. Бот сам находит такие серверы через `environment list` (список обновляется раз в минуту), опрашивает их вместе с локальной Orca и шлёт команды к их воркспейсам с `--environment`. В списке они помечены `🌐 <имя сервера>`. Если сервер не отвечает, локальные воркспейсы всё равно показываются, а в `/ws` у этого сервера стоит «🔌 не отвечает». В `/new` сервер можно указать явно: `/new devbox/репо ветка задача`.

Mac не должен засыпать: Системные настройки → Энергосбережение → «Не переходить в режим сна автоматически». Для автозапуска — `dev.orca-tg-bot.plist` (LaunchAgent, инструкция внутри файла).

### Установка на Linux

1. Проверьте, что CLI видит Orca:
   ```bash
   orca-ide status --json
   orca-ide worktree ps --json
   ```
   Если команды нет — зарегистрируйте CLI в Orca: Settings → General → Orca CLI. На Linux команда называется `orca-ide`; голый `orca` — это экранный диктор GNOME. На macOS — `orca` (бот выбирает имя сам по ОС, `ORCA_BIN` переопределяет).
2. Создайте бота у @BotFather, узнайте свой id у @userinfobot.
3. Соберите и поставьте:
   ```bash
   go install github.com/leshak/orca-tg-bot@latest   # бинарник попадёт в ~/go/bin
   ```
   Или из клона репозитория: `go build -o orca-tg-bot .`. Для другой архитектуры: `GOOS=linux GOARCH=arm64 go build -o orca-tg-bot .`
4. Заполните `~/.config/orca-tg-bot/env` по образцу `.env.example` и запустите сервис по инструкции в `orca-tg-bot.service`.
5. Напишите боту `/start`, затем `/ws`.

`ORCA_ENVIRONMENT` задаёт, какие рантаймы Orca опрашивать. По умолчанию (`auto`) это эта машина и все сопряжённые серверы из `orca-ide environment list`. Можно перечислить явно через запятую: `local,devbox`, а если бот запущен не на машине с Orca — только имя сервера.

### Важно

- **Нативный чат Orca.** CLI умеет писать только в терминалы. Агент, открытый как чат (Settings → Chat UI), через бот не управляется. Агенты, которых запускает сам бот, всегда стартуют в терминале (`--surface terminal`). Для уже запущенных из десктопа — выключите Chat UI или запускайте агентов в терминале.
- **Доступ.** Кто может писать боту, тот фактически выполняет команды на сервере руками агента. Поэтому `TELEGRAM_ALLOWED_IDS` обязателен, остальным бот не отвечает.
- **Многострочный текст** передаётся в `terminal send` как есть. Как его обработает конкретный агент (одной отправкой или построчно) — не проверялось; надёжнее писать одной строкой.
- **Telegram недоступен напрямую** — задайте `TELEGRAM_PROXY` (поддерживается `socks5://`).
- Хэндлы терминалов живут в пределах рантайма: после перезапуска Orca ручной выбор `/term` сбрасывается, и бот снова сам находит терминал агента.

### Тесты

```bash
go test -v ./...
```

Тесты гоняют настоящий бот против фейкового `orca-ide` (shell-скрипт с ответами в формате рантайма) и фейкового Bot API: выбор воркспейса, отправка в терминал агента, `/read`, `/stop`, `/new`, ошибки Orca, уведомления и их отключение, воркспейсы сопряжённого сервера Orca (`--environment`) и его недоступность. Веб-интерфейс проверяется так же: вход и проверка `Origin`, отправка текста и клавиш, проверка ввода, группировка по проектам и поток SSE.

### Лицензия

[MIT](LICENSE)
