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

On macOS:

```powershell
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

## 4. Optional Bot Menu

Feishu bot custom menus are configured manually in the Feishu/Lark developer
console. Recommended send-text menu commands:

- `/chats`
- `/projects`
- `/new`
- `/status`
- `/setting`
- `/repair`
