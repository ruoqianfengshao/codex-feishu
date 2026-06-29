# Quickstart

## 1. Choose an Adapter

Telegram: create a bot with BotFather and keep the token private.

Feishu/Lark: run `ctr-go feishu setup` to create an app by QR scan. It uses the
official OAuth device registration flow, waits for approval in the mobile app,
and writes the returned app id and secret to the private local config. If you
already have an enterprise self-built app, you can still configure its app id
and secret manually.

`ctr-go feishu setup` presets the created app name to `Codex`. For input-box
shortcuts in Feishu/Lark, configure the bot custom menu in the developer
console and publish a new app version. The menu is a one-on-one bot chat
feature. Recommended menu items use "send text message":

- 首页 -> `/workspace`
- 最近会话 -> `/threads`
- 项目 -> `/projects`
- 新建 Chat -> `/newchat`
- 设置 -> `/settings`
- 修复 -> `/repair`

If a menu item uses "push event", subscribe to `application.bot.menu_v6` and use
event keys such as `workspace`, `threads`, `projects`, `newchat`, `settings`, or
`repair`.

## 2. Download And Initialize

On macOS, download the latest `.pkg` from
[GitHub Releases](https://github.com/mideco-tech/codex-tg/releases/latest),
install it, then run:

```powershell
ctr-go service install --start --start-at-login
ctr-go doctor
```

`ctr-go service install` starts a friendly setup wizard when values are not
provided through flags. It writes `~/.codex-tg/config.env`, creates a user
LaunchAgent, and starts the daemon when `--start` is present.

For archive/manual setup on any OS:

```powershell
ctr-go init
ctr-go doctor
ctr-go daemon run
```

Use `CTR_GO_CONFIG` when you want a different config path. Explicit environment
variables override config file values.
When your shell uses proxy env such as `HTTPS_PROXY` or `NO_PROXY`,
`ctr-go service install` preserves those values in the private config so the
macOS LaunchAgent can reach the same network while keeping the plist limited to
`CTR_GO_CONFIG`.

For Feishu/Lark, use the QR setup instead of `ctr-go init` when creating a new
app:

```powershell
ctr-go feishu setup
ctr-go doctor
ctr-go daemon run
```

Use `ctr-go feishu setup --no-qr` when the terminal cannot render QR codes, and
`ctr-go feishu setup --force` to overwrite an existing config.

## Environment-Only Setup

```powershell
$env:CTR_GO_ADAPTER = "telegram"
$env:CTR_GO_TELEGRAM_BOT_TOKEN = "<telegram-bot-token>"
$env:CTR_GO_ALLOWED_USER_IDS = "<telegram-user-id>"
$env:CTR_GO_DEFAULT_CWD = "C:\Users\you\Projects\Codex"
$env:CTR_GO_CODEX_CHATS_ROOT = "C:\Users\you\Documents\Codex"
# Optional: set to "off" to keep New run visible but silent.
$env:CTR_GO_NOTIFY_NEW_RUN = "on"
```

For Feishu/Lark, environment-only setup remains available when you already have
an app:

```powershell
$env:CTR_GO_ADAPTER = "feishu"
$env:CTR_GO_FEISHU_APP_ID = "<feishu-app-id>"
$env:CTR_GO_FEISHU_APP_SECRET = "<feishu-app-secret>"
# Optional allowlists:
$env:CTR_GO_FEISHU_ALLOWED_OPEN_IDS = "<feishu-open-id>"
$env:CTR_GO_FEISHU_ALLOWED_CHAT_IDS = "<feishu-chat-id>"
```

## Build From Source

Source builds require Go 1.26 or newer.

```powershell
go run ./cmd/ctr-go init
go run ./cmd/ctr-go doctor
go run ./cmd/ctr-go daemon run
```

## 3. Open The Workspace

In Telegram or Feishu/Lark:

```text
/workspace
/threads
/projects
```

Open a Codex thread from the workspace or recent chats list, then reply in that
thread's chat topic to continue remote control. Telegram only uses normal
notifications for `New run`, `[Plan]`, and `[Final]`; Feishu/Lark delivery
follows the platform's chat notification behavior.

Use `/plan` or `/reply --plan` for Plan Mode. If a thread remains in Plan Mode,
press `Turn off Plan` on the Plan Final Card, or use `/stop <thread>`, then send
the next normal prompt. The bridge applies App Server Default Mode to that next
ordinary turn.

To start a new thread from Telegram, open `/projects`, choose a project, press
`New thread`, then send the first prompt as the next message. The selected
project must already exist in the cached Codex thread list.

Codex UI Chats stored under `Documents/Codex` appear under the `Chats` section
instead of as normal projects. Use `Open Chats` for the full paginated list, or
`/newchat <prompt>` to create a new Codex UI Chat under
`Documents/Codex/<date>/<prompt-slug>`.
