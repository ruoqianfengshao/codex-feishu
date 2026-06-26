# codex-tg: local Codex control plane

Local control plane for OpenAI Codex App Server, built in Go, with a Feishu/Lark
adapter.

`codex-tg` watches local Codex threads, keeps durable thread identity visible,
routes operator input back to the right turn, and exposes high-signal controls
such as Plan Mode prompts, Stop, Steer, Details, Tools file, and Get full log.
Feishu/Lark is available for enterprise self-built apps through the official
WebSocket long connection.

## Why codex-tg?

- Local Codex control plane for OpenAI Codex App Server.
- Control and observe Codex threads through adapters without exposing App Server to the internet.
- Use Feishu/Lark as a high-signal notification, reply, approval, and Details surface.
- Reuse your existing Codex setup: skills, MCP servers, plugins, repo instructions, and local workflows.
- Thread-first routing keeps replies, tools, Plan Mode, Details, and Final cards attached to the right run.
- Built toward long-running local coding-agent orchestration and future router-agent workflows.

Current release: `v0.5.0`.

![codex-tg Telegram Plan Mode demo](docs/assets/telegram-plan-mode-demo.png)

The demo flow is documented in [docs/demo/telegram-plan-mode-demo.md](docs/demo/telegram-plan-mode-demo.md).

## Why It Matters

- Keep local Codex work observable and controllable without exposing Codex App Server to the internet.
- Give future router agents a stable local control surface for Codex threads, turns, approvals, events, and skills.
- Use Feishu/Lark as a low-friction notification and control surface without exposing Codex App Server.
- Preserve local-first ownership: Codex sessions, workspaces, SQLite state, and tokens stay on your machine.

## Remote Connections

Official Codex Remote Connections cover the broad mobile remote-control
workflow for Codex. `codex-tg` is not trying to replace that feature. The
project direction is a local control layer and adapter system: Feishu/Lark
consumes the Codex control core, while future adapters can use the same routing
and event contracts.

## Demo Screenshots

**1. User request and Plan Mode**

![User request and Plan Mode](docs/assets/telegram-plan-mode-demo-1-user-plan.png)

**2. Tool execution and output**

![Tool execution and output](docs/assets/telegram-plan-mode-demo-2-tool-output.png)

**3. Final answer and Details**

![Final answer and Details](docs/assets/telegram-plan-mode-demo-3-final-details.png)

## Features

- Thread-first routing over local Codex App Server.
- Global observer for foreign GUI/CLI runs, with polling fallback through `thread/read`.
- Feishu-origin live current tool rendering from App Server `item/*` events, while foreign GUI/CLI panels stay completed-tool only.
- Stable visual identity per thread: emoji marker plus project/thread/run chips.
- Explicit `New run -> [User] -> [commentary] -> [Tool] -> [Output] -> [Final]` chronology with status on the live commentary/final card.
- Low-noise notifications: only `New run` (configurable), `[Plan]`, and `[Final]` are audible; live progress and exports are sent silently.
- Plan Mode starts via `/plan` or `/reply --plan`; if a thread remains in Plan Mode, the Plan Final Card offers `Turn off Plan` and `/stop` also arms the next normal turn to leave Plan Mode. `[Plan]` prompt-cards keep reply-first routing and structured buttons when Codex provides choices.
- Final Card with Details pagination and on-demand Tools file export.
- On-demand full log archive from Codex session JSONL.
- SQLite-backed durable state for bindings, routes, callbacks, observer target, panels, and delivery metadata.
- Feishu/Lark adapter for enterprise self-built apps, using official SDK WebSocket events, text/cards, buttons, edits, deletes, and file uploads.
- macOS service installer with friendly first-run setup, user LaunchAgent management, and menu bar tray control.
- v0.5 architecture direction: adapter-independent Codex Control Plane for router agents, voice adapters, and local private APIs.
- Cross-platform Go daemon foundation for Windows, macOS, and Linux.

## Platform Status

- Windows: local daemon/runtime validation is pending after the Feishu-only adapter change.
- macOS: `v0.5.0` preserves the `v0.4.0` verified service/runtime path on macOS 26.3.1 arm64 with Go 1.26.2, and adds validated Codex Control Plane architecture, internal control interfaces, capability mapping, normalized event contracts, and notification severity policy.
- Linux: CI runs tests/builds on Ubuntu; full local daemon/runtime validation is still pending.

## Quickstart

Prerequisites:

- OpenAI Codex CLI with `codex app-server`.
- Feishu/Lark credentials: run `ctr-go feishu setup` to create an app by QR scan, or provide an existing app id and secret.

On macOS, download the latest `.pkg` from
[GitHub Releases](https://github.com/mideco-tech/codex-tg/releases/latest),
install it, then run:

```powershell
ctr-go service install --start --start-at-login
ctr-go doctor
```

`ctr-go service install` starts a friendly first-run setup wizard when required.
The same values can be passed with flags for scripted installs. It writes a
private local config file at `~/.codex-tg/config.env` by default, creates a
user LaunchAgent, and starts the daemon when `--start` is present.
If your shell uses proxy variables such as `HTTPS_PROXY` or `NO_PROXY`, the
installer preserves them in the private config so the LaunchAgent can reach the
same network without putting secrets or user ids into the plist.

For Linux, Windows, or manual macOS setup, download the latest `ctr-go` archive,
unpack it, then run:

```powershell
ctr-go init
ctr-go doctor
ctr-go daemon run
```

Use `CTR_GO_CONFIG` to point at another config file. Explicit environment
variables still override config file values.

For the Feishu/Lark adapter, the smoothest path is the scan-to-create setup:

```powershell
ctr-go feishu setup
ctr-go doctor
ctr-go daemon run
```

`ctr-go feishu setup` uses Feishu/Lark's official OAuth device registration
flow. It prints a one-time setup link and terminal QR code, waits for approval
in the mobile app, then writes `CTR_GO_ADAPTER=feishu`,
`CTR_GO_FEISHU_APP_ID`, and `CTR_GO_FEISHU_APP_SECRET` to the private local
config file. Pass `--no-qr` when your terminal cannot render QR codes, and
`--force` to overwrite an existing config.

Build from source:

```powershell
git clone https://github.com/mideco-tech/codex-tg.git
cd codex-tg
go run ./cmd/ctr-go feishu setup
go run ./cmd/ctr-go doctor
go run ./cmd/ctr-go daemon run
```

Environment-only setup remains supported when you already have an
app:

```powershell
$env:CTR_GO_ADAPTER = "feishu"
$env:CTR_GO_FEISHU_APP_ID = "<feishu-app-id>"
$env:CTR_GO_FEISHU_APP_SECRET = "<feishu-app-secret>"
# Optional allowlists.
$env:CTR_GO_FEISHU_ALLOWED_OPEN_IDS = "<feishu-open-id>"
$env:CTR_GO_FEISHU_ALLOWED_CHAT_IDS = "<feishu-chat-id>"
```

For Feishu/Lark, the app must have the bot enabled, WebSocket event
subscription enabled, message receive events subscribed, and interactive card
action callbacks enabled. The app needs message send/read and file upload
permissions appropriate for the target chats. `ctr-go feishu setup` creates the
app through the official registration flow; existing apps may still need these
capabilities checked in the developer console.
To show shortcuts above the Feishu input box, enable the bot custom menu in the
Feishu developer console and publish a new app version. Feishu exposes that menu
only in one-on-one bot chats. The smoothest setup is menu items with the "send
text message" action, using values like `/help`, `/threads`, `/projects`,
`/settings`, `/status`, and `/repair`. If you choose the "push event" action
instead, subscribe to `application.bot.menu_v6` and use event keys such as
`help`, `threads`, `projects`, `settings`, `status`, `observe_all`,
`observe_off`, or `repair`; event-based responses are sent back to the
operator's bot DM because Feishu menu events do not include a chat id.

On macOS, set `CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU=true` when you want Feishu
replies to appear in the current Codex Desktop window. The daemon first sends
Feishu input through Codex Desktop's local IPC owner window for the target
thread, then falls back to the normal App Server path if Desktop is closed, the
thread is not owned by a visible Desktop window, or local IPC is unavailable.
This does not require official Codex Remote Connections authentication.

In the selected adapter chat:

```text
/start
/observe all
/threads
/context
```

Start or continue a Codex thread from Codex GUI/CLI. `codex-tg` should create a `New run` card, a `[User]` card, live progress cards, and then send a final answer card with Details while cleaning up transient live cards.

Set `CTR_GO_NOTIFY_NEW_RUN=off` to keep `New run` visible but silent. Set `CTR_GO_NOTIFY_SYSTEM=off` to disable macOS system notifications for completion, failure, and approval prompts.

## Runtime Commands

```powershell
ctr-go init
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

Source-build equivalents:

```powershell
go run ./cmd/ctr-go init
go run ./cmd/ctr-go service install
go run ./cmd/ctr-go doctor
go run ./cmd/ctr-go status
go run ./cmd/ctr-go repair
go run ./cmd/ctr-go daemon run
```

Feishu commands:

- `/start`, `/help`
- `/threads`, `/projects`, `/new`, `/newchat`, `/newthread`, `/show`, `/bind`, `/reply`, `/plan`
- `/settings`, `/model`, `/effort`
- `/context`, `/whereami`
- `/observe all`, `/observe off`
- `/status`, `/repair`, `/stop`, `/approve`, `/deny`

`/projects` opens cached project/workspace navigation sorted by the latest
thread activity. Codex UI Chats from `Documents/Codex` are grouped under
`Chats`: the main projects view shows recent Chat previews, `Open Chats` opens
the full paginated Chat list, and choosing a Chat opens and binds its thread.
Use `New thread` in a normal project menu to create a new thread in that
project cwd. Use `/newchat <prompt>` to create a Codex UI Chat under
`Documents/Codex/<date>/<prompt-slug>`. Use `/newthread <prompt>` when you need
a thread without choosing a project or creating a Chat folder; App Server may
still attach the daemon default cwd.

## Configuration

Primary environment variables:

- `CTR_GO_HOME`
- `CTR_GO_CONFIG` (`~/.codex-tg/config.env` by default)
- `CTR_GO_ADAPTER` (`feishu`; `auto` is treated as Feishu)
- `CTR_GO_CODEX_BIN`
- `CTR_GO_APP_SERVER_LISTEN`
- `CTR_GO_FEISHU_APP_ID`
- `CTR_GO_FEISHU_APP_SECRET`
- `CTR_GO_FEISHU_ALLOWED_OPEN_IDS`
- `CTR_GO_FEISHU_ALLOWED_CHAT_IDS`
- `CTR_GO_DEFAULT_CWD`
- `CTR_GO_CODEX_CHATS_ROOT` (`~/Documents/Codex` by default)
- `CTR_GO_NOTIFY_NEW_RUN` (`true` by default; set `false`/`off`/`0` to send `New run` silently)
- `CTR_GO_NOTIFY_SYSTEM` (`true` by default; set `false`/`off`/`0` to disable macOS system notifications for completion, failure, and approval prompts)
- `CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU` (`false` by default; on macOS, set `true` to route Feishu replies through the local Codex Desktop IPC owner window when available, falling back to App Server and opening `codex://threads/<id>`)
- `CTR_GO_LOG_ENABLED` (`true` by default; set `false`/`off`/`0` to discard daemon stdout logs)
- `CTR_GO_DIAGNOSTIC_LOGS` (`true` by default; set `false`/`off`/`0` to keep normal bot logs but suppress structured `daemon_event` diagnostics)
- `CTR_GO_OBSERVER_POLL_SECONDS`
- `CTR_GO_REQUEST_TIMEOUT_SECONDS`
- `CTR_GO_PROJECTS_PROJECT_PREVIEW_LIMIT` (`7` by default)
- `CTR_GO_PROJECTS_CHAT_PREVIEW_LIMIT` (`3` by default)
- `CTR_GO_CHATS_PAGE_SIZE` (`8` by default)
- `CTR_GO_INDEX_REFRESH_SECONDS`
- `CTR_GO_ATTACH_REFRESH_SECONDS`
- `CTR_GO_DELIVERY_RETRY_SECONDS`
- `CTR_GO_DELIVERY_MAX_ATTEMPTS`

## Verification

```powershell
go test ./...
go build -buildvcs=false ./...
```

## GitHub Metadata

Suggested repository description:

```text
Local Codex control plane with a Feishu/Lark adapter. Observe, approve, steer, and route Codex App Server threads without exposing App Server publicly.
```

Suggested topics:

```text
codex feishu lark openai-codex codex-cli
codex-app-server codex-control-plane ai-agents coding-agent remote-control developer-tools
local-first go macos windows linux plan-mode agent-observer router-agent
```

## Documentation

- [Architecture](docs/wiki/Architecture.md)
- [Control Plane](docs/wiki/Control-Plane.md)
- [Quickstart](docs/wiki/Quickstart.md)
- [Telegram UX](docs/wiki/Telegram-UX.md)
- [Plan Mode](docs/wiki/Plan-Mode.md)
- [Security](docs/wiki/Security.md)
- [Operations](docs/wiki/Operations.md)
- [Demo](docs/wiki/Demo.md)
- [Changelog](CHANGELOG.md)
- [Contract matrix](docs/research/contract-matrix.md)
- [Validation notes](docs/testing/validation-notes.md)
- [ADRs](docs/adr/)

## License

Apache License 2.0. This keeps the project permissive for the community while also providing an explicit patent grant that large companies usually expect from infrastructure and developer-tooling projects.

## Operational Notes

- Feishu/Lark uses the official SDK WebSocket long connection; no public callback URL is required for message/card events.
- Do not expose Codex App Server on a public interface. `codex-tg` is designed around local/private App Server connectivity.
- Keep Feishu app secrets, SQLite databases, logs, and `.env` files out of git.
