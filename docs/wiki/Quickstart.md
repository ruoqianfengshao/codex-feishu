# Quickstart

## 1. Create Or Connect Feishu

Use QR setup when creating a new Feishu/Lark self-built app:

```powershell
ctr-go feishu setup
ctr-go doctor
ctr-go daemon run
```

The setup command prints a one-time setup link and QR code, waits for approval
in Feishu/Lark, and writes the app id and secret to the private local config.

For an existing app:

```powershell
$env:CTR_GO_ADAPTER = "feishu"
$env:CTR_GO_FEISHU_APP_ID = "<feishu-app-id>"
$env:CTR_GO_FEISHU_APP_SECRET = "<feishu-app-secret>"
$env:CTR_GO_FEISHU_ALLOWED_OPEN_IDS = "<feishu-open-id>"
$env:CTR_GO_FEISHU_ALLOWED_CHAT_IDS = "<feishu-chat-id>"
```

The app needs bot enabled, WebSocket events enabled, message receive events,
interactive card callbacks, message send/read permissions, and file upload
permissions.

## 2. Install And Run

On macOS, prefer the release tarball over the `.pkg` installer when an AI agent
is installing for you. The tarball installs into your user directory and does
not need `sudo`:

```bash
VERSION="v0.6.4"
ARCH="$(uname -m)"
if [ "$ARCH" = "x86_64" ]; then ARCH="amd64"; fi
mkdir -p "$HOME/.local/bin"
curl -L -o /tmp/ctr-go.tar.gz \
  "https://github.com/ruoqianfengshao/codex-feishu/releases/latest/download/ctr-go_${VERSION}_darwin_${ARCH}.tar.gz"
tar -xzf /tmp/ctr-go.tar.gz -C "$HOME/.local/bin" ctr-go
"$HOME/.local/bin/ctr-go" version
```

Then configure and start the user LaunchAgent:

```bash
ctr-go service install --start --start-at-login
ctr-go feishu setup
ctr-go doctor
```

Manual/source run:

```powershell
go run ./cmd/ctr-go feishu setup
go run ./cmd/ctr-go doctor
go run ./cmd/ctr-go daemon run
```

## 3. Use Feishu

In the Codex bot DM:

```text
/help
/chats
/projects
/new <prompt>
/status
/setting
```

Open an existing chat from `/chats`, or open a project from `/projects` and use
the project card's `New thread` action. The daemon opens a Feishu topic for the
Codex thread. Continue the conversation in that topic.

`/new <prompt>` creates a temporary chat. Project-scoped new threads are only
created from project cards.

`/help` is interactive. In the bot DM it shows workspace commands; inside a
Codex topic it shows only topic-scoped commands such as `/plan`, `/goal`, and
`/stop`.

`/setting` opens a Feishu form for model, reasoning effort, and bot language.
When no local override is saved, model and reasoning values are pre-filled from
the current Codex config.

`/status` shows a dashboard-style Feishu card with KPI sections and a Feishu
chart for thread mix statistics. Change language from `/setting`, not
`/status`.

## 4. Optional Bot Menu

Feishu bot custom menus are configured manually in the Feishu/Lark developer
console. Recommended send-text menu commands:

- `/help`
- `/chats`
- `/projects`
- `/new`
- `/status`
- `/setting`
- `/repair`
