# codex-feishu: Feishu control for local Codex

[English](README.md) | [简体中文](README.zh-CN.md)

`codex-feishu` is a local Go daemon for remotely controlling OpenAI Codex App
Server from Feishu/Lark. It keeps Codex on your machine, maps opened Codex
threads to Feishu topics, and routes replies, approvals, progress, final
answers, images, and project navigation through a Feishu bot DM.

The project name is `codex-feishu`; the command-line binary is still `ctr-go`
for now. The product is Feishu-only. Older channel adapters and observer-style
global monitoring have been removed.

This project is a fork of
[`mideco-tech/codex-tg`](https://github.com/mideco-tech/codex-tg). It has been
substantially reworked for the Feishu/Lark workflow, including the Feishu-only
adapter surface, project/thread navigation, topic cards, final-card repair,
installation health checks, and local service defaults.

## What It Does

- Creates or connects a Feishu/Lark self-built app through `ctr-go feishu setup`.
- Uses the official Feishu WebSocket long connection; no public callback URL is required.
- Shows chats and projects in the Codex bot DM.
- Opens Codex threads as Feishu topics under the bot DM.
- Automatically syncs a Codex thread after the user has opened its Feishu topic.
- Sends Codex desktop/user input, progress, tool activity, final answers, and image messages into the topic.
- Sends Feishu topic replies and images back into the matching Codex thread.
- Supports Plan Mode prompts, approvals, stop, steer, settings, status, and repair flows.
- Stores local state in SQLite under the configured `CTR_GO_HOME`, defaulting
  to `~/.codex-feishu`.

## Feishu Screenshots

These screenshots show the Feishu bot workspace, project navigation, thread
cards, settings, status, and Codex topic workflow:

<table>
  <tr>
    <td><img src="docs/images/feishu/1.jpg" alt="Feishu screenshot 1"></td>
    <td><img src="docs/images/feishu/2.jpg" alt="Feishu screenshot 2"></td>
    <td><img src="docs/images/feishu/3.jpg" alt="Feishu screenshot 3"></td>
  </tr>
  <tr>
    <td><img src="docs/images/feishu/4.jpg" alt="Feishu screenshot 4"></td>
    <td><img src="docs/images/feishu/5.jpg" alt="Feishu screenshot 5"></td>
    <td><img src="docs/images/feishu/6.jpg" alt="Feishu screenshot 6"></td>
  </tr>
  <tr>
    <td><img src="docs/images/feishu/7.jpg" alt="Feishu screenshot 7"></td>
    <td></td>
    <td></td>
  </tr>
</table>

## Quickstart

Prerequisites:

- OpenAI Codex CLI with `codex app-server`.
- Feishu/Lark access that can create or authorize an enterprise self-built app.

On macOS, use the tarball from
[GitHub Releases](https://github.com/ruoqianfengshao/codex-feishu/releases/latest)
and install it into your user bin directory. This does not need `sudo`:

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

Make sure `$HOME/.local/bin` is on `PATH`, or call the binary by absolute path.

Then configure and start the user LaunchAgent:

```bash
ctr-go service install --start --start-at-login
ctr-go feishu setup
ctr-go doctor
```

The service is installed as a user LaunchAgent and does not require `sudo`.
The installed CLI is still named `ctr-go`.

For a source build:

```powershell
git clone https://github.com/ruoqianfengshao/codex-feishu.git
cd codex-feishu
go run ./cmd/ctr-go feishu setup
go run ./cmd/ctr-go doctor
go run ./cmd/ctr-go daemon run
```

`ctr-go feishu setup` prints a one-time setup link and QR code. After approval
in Feishu/Lark, it writes the app id and secret into the private local config
file. Existing apps can be configured with environment variables instead:

```powershell
$env:CTR_GO_ADAPTER = "feishu"
$env:CTR_GO_FEISHU_APP_ID = "<feishu-app-id>"
$env:CTR_GO_FEISHU_APP_SECRET = "<feishu-app-secret>"
$env:CTR_GO_FEISHU_ALLOWED_OPEN_IDS = "<feishu-open-id>"
$env:CTR_GO_FEISHU_ALLOWED_CHAT_IDS = "<feishu-chat-id>"
```

The Feishu app must have the bot enabled, WebSocket event subscription enabled,
message receive events subscribed, and interactive card callbacks enabled. It
also needs the message, image, file, and chat permissions required for the
target chats. If a card button, image, or file upload fails, rerun
`ctr-go doctor` first and then check the Feishu app permissions.

`ctr-go doctor` emits a JSON health report under `health`. AI installers should
treat `health.ok == true` as the readiness gate. When it is false, read
`health.checks[].remediation` and `health.next_actions` before changing config.

## Daily Use

Use the Codex bot DM as the workspace:

```text
/help
/chats
/projects
/new <prompt>
/status
/setting
```

Open a chat from `/chats`, or open a project from `/projects` and choose `New
thread`. The daemon creates or reopens a Feishu topic for that Codex thread.
Continue the conversation in the topic.

`/new <prompt>` always creates a temporary Codex chat. Project-scoped new
threads should be created from the project card.

`/help` returns an interactive command card. In the bot DM it shows workspace
commands and explains which commands belong inside Codex thread topics. Inside a
topic it only shows topic-scoped commands such as `/plan`, `/goal`, and `/stop`.

`/setting` opens a Feishu form for model, reasoning effort, and bot language.
When no local override is saved, the model and reasoning dropdowns are
pre-filled from the current Codex config.

`/status` returns a dashboard-style Feishu card with health and thread KPIs,
plus a Feishu pie chart with thread mix percentages. Language switching is
intentionally kept in `/setting`, not `/status`.

Configure Feishu bot custom menu items manually in the Feishu/Lark developer
console if you want input-box shortcuts. Recommended commands:

- `/help`
- `/chats`
- `/projects`
- `/new`
- `/status`
- `/setting`
- `/repair`

## Commands

Feishu bot commands:

- `/start`
- `/help`
- `/chats [limit|search]`
- `/projects`
- `/new <prompt>`
- `/show <thread>`
- `/plan <text>`
- `/plan <thread_id> <text>`
- `/goal <goal>`
- `/goal clear`
- `/setting`
- `/status`
- `/repair`
- `/stop [thread]`
- `/approve <request_id>`
- `/deny <request_id>`

Runtime commands:

```powershell
ctr-go init
ctr-go feishu setup
ctr-go service install
ctr-go service start
ctr-go service stop
ctr-go service restart
ctr-go service status
ctr-go doctor
ctr-go status
ctr-go repair
ctr-go daemon run
```

The binary name may change in a future release; until then, automation should
call `ctr-go`.

## Configuration

Common environment variables:

- `CTR_GO_HOME` defaults to `~/.codex-feishu`
- `CTR_GO_CONFIG`
- `CTR_GO_ADAPTER` (`feishu`)
- `CTR_GO_CODEX_BIN`
- `CTR_GO_APP_SERVER_LISTEN`
- `CTR_GO_FEISHU_APP_ID`
- `CTR_GO_FEISHU_APP_SECRET`
- `CTR_GO_FEISHU_ALLOWED_OPEN_IDS`
- `CTR_GO_FEISHU_ALLOWED_CHAT_IDS`
- `CTR_GO_DEFAULT_CWD`
- `CTR_GO_CODEX_CHATS_ROOT`
- `CTR_GO_NOTIFY_SYSTEM`
- `CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU`
- `CTR_GO_LOG_ENABLED`
- `CTR_GO_DIAGNOSTIC_LOGS`
- `CTR_GO_OBSERVER_POLL_SECONDS`
- `CTR_GO_REQUEST_TIMEOUT_SECONDS`
- `CTR_GO_PROJECTS_PROJECT_PREVIEW_LIMIT`
- `CTR_GO_PROJECTS_CHAT_PREVIEW_LIMIT`
- `CTR_GO_CHATS_PAGE_SIZE`
- `CTR_GO_INDEX_REFRESH_SECONDS`
- `CTR_GO_ATTACH_REFRESH_SECONDS`
- `CTR_GO_DELIVERY_RETRY_SECONDS`
- `CTR_GO_DELIVERY_MAX_ATTEMPTS`

On macOS, set `CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU=true` when Feishu replies
should first try the current Codex Desktop IPC owner window and then fall back
to App Server.

## Verification

```powershell
go test ./...
go build -buildvcs=false ./...
```

For Feishu-facing changes, also rebuild, restart the service, and validate the
changed path in Feishu.

For installation validation on another machine:

```powershell
ctr-go doctor
ctr-go service status
```

Check `doctor.health.checks` first. It probes the Codex binary, Codex
app-server initialization, basic Codex RPCs, Feishu app credentials, and recent
daemon errors without sending test messages to Feishu.

## Documentation

- [Architecture](docs/wiki/Architecture.md)
- [Quickstart](docs/wiki/Quickstart.md)
- [Plan Mode](docs/wiki/Plan-Mode.md)
- [Security](docs/wiki/Security.md)
- [Operations](docs/wiki/Operations.md)

## Security

Do not expose Codex App Server on a public interface. Keep Feishu app secrets,
SQLite databases, logs, local config files, and private screenshots out of git.

## License

Apache License 2.0.
