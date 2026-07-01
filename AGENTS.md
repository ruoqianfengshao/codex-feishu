# AGENTS

Purpose: help agents work on `codex-feishu`, a Feishu-only local control bridge for
OpenAI Codex App Server.

## Repository Purpose

`codex-feishu` runs locally, connects to Codex App Server, and exposes Codex control
through a Feishu/Lark bot DM and thread topics. Feishu is the only supported
chat surface.

Keep every change safe for open-source publication: no private paths, tokens,
Feishu ids, local sessions, databases, logs, screenshots with private data, or
environment-specific credentials.

## Working Mode

- First understand nearby code, tests, and docs before editing.
- Ask focused questions when requirements or tradeoffs are unclear.
- Prefer small vertical slices with targeted tests.
- Match existing style and avoid broad refactors unless explicitly requested.
- Public docs should describe current Feishu behavior, not historical adapters.

## Core Product Rules

- Codex App Server is the runtime source of truth for threads, turns, approvals,
  live events, history, and snapshots.
- Durable identity is the Codex `threadId`.
- Feishu chat/topic/message ids are routing surfaces, not Codex identity.
- The Codex bot DM is the workspace surface for `/chats`, `/projects`,
  `/new`, `/status`, `/setting`, and `/repair`.
- A Codex thread syncs automatically after the user has opened its Feishu topic.
- Replies in a Feishu topic route to that topic's Codex thread.
- `/new <prompt>` creates a temporary chat. Project-scoped new threads are
  created from the project card.
- No global observer mode or control-room group should be reintroduced without a
  new product design.
- Fixed Feishu-visible text must support Chinese and English.

## Feishu UX Invariants

- Topic messages should be compact and high-signal.
- Codex desktop/user input should appear in the Feishu topic as user input.
- Process messages should preserve useful status markers such as thinking,
  tool calling, and running.
- Final cards should appear once and stop polling for that turn.
- Archived or deleted Codex threads should not appear in Feishu chat/project
  lists.
- Image messages must work in both directions when Feishu and Codex support the
  payload.
- Details and exports are explicit on-demand actions; avoid background document
  spam.

## Implementation Notes

- `cmd/ctr-go/`: daemon CLI entrypoint.
- `internal/appserver/`: JSON-RPC stdio client and snapshot normalization.
- `internal/config/`: env-driven config.
- `internal/daemon/`: runtime orchestration, Feishu topic panels, callbacks,
  log exports, and command handling.
- `internal/feishu/`: Feishu/Lark transport and card rendering.
- `internal/model/`: shared types.
- `internal/storage/`: SQLite schema and repositories.
- `internal/msgformat/`: shared message formatting helpers.
- `tests/`: black-box contract tests.
- `docs/`: current docs and historical cleanup plan.

## Testing

Run targeted tests first, then broader checks:

```powershell
go test ./...
go build -buildvcs=false ./...
```

For Feishu-facing behavior, unit tests are not enough when a live contour is
available. Rebuild, restart the daemon, test the changed path in Feishu, and
inspect logs or SQLite state when routing/lifecycle behavior is involved.

## Commit Hygiene

- Keep diffs focused.
- Do not mix unrelated cleanup with behavior changes.
- Before committing, check dirty tree, staged diff, tests, and secret/local data.
- Do not commit `.env`, SQLite databases, logs, local binaries, local sessions,
  private screenshots, app secrets, or user-specific paths.

## Secret Scan

Run a targeted scan before publishing:

```powershell
rg -n "APP_SECRET|FEISHU_APP_SECRET|BOT_TOKEN|password|secret|\\.session|\\.sqlite|\\.env|C:\\\\Users\\\\<private-user>" .
```
