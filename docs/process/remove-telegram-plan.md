# Remove Telegram Support Plan

Objective: make this project Feishu-only. Do not preserve Telegram runtime, data compatibility, tests, or documentation.

## Ground Rules

- Remove Telegram concepts completely instead of keeping compatibility shims.
- Prefer small commits by phase.
- After each phase, update this checklist.
- Do not leave user-visible Telegram wording.
- Do not keep old Telegram data migrations unless needed to delete/drop the data.

## Phase 1: Entry Points And Config

- [x] Remove `telegram` / `tg` adapter selection from CLI.
- [x] Remove Telegram bot token configuration.
- [x] Remove Telegram allowed user/chat configuration.
- [x] Update init/status/doctor/service output to Feishu-only.
- [x] Delete or rewrite CLI tests that assert Telegram behavior.

Verification:

- [x] `rg -n "telegram|Telegram|tg" cmd internal/config`
  - Remaining hits are project/package name strings such as `codex-tg`, not Telegram adapter/config references.
- [x] `go test ./cmd/ctr-go ./internal/config`

## Phase 2: Source Modes And Runtime Naming

- [x] Delete `PanelSourceTelegramInput`.
- [x] Rename or remove `telegram_origin_*` state keys, functions, and log events.
- [x] Remove Telegram-origin hot poll naming.
- [x] Remove Telegram terminal gate naming.
- [x] Replace surviving source modes with Feishu-only or neutral names.

Verification:

- [x] `rg -n "PanelSourceTelegram|telegram_origin|TelegramOrigin|telegramOrigin" internal`
- [x] `go test ./internal/daemon ./internal/model ./internal/storage`

## Phase 3: Runtime Behavior

- [ ] Remove Telegram default `HandleMessage` / `HandleCallback` entry points.
- [ ] Keep only Feishu source entry points.
- [ ] Remove Telegram run notice behavior.
- [ ] Remove Telegram duplicate-user-message guards.
- [ ] Remove global observer behavior that only existed for Telegram.
- [ ] Remove Telegram-specific lifecycle repair/status wording.

Verification:

- [ ] `rg -n "Telegram|telegram|PanelSourceGlobalObserver|GlobalObserver" internal/daemon internal/model`
- [ ] `go test ./internal/daemon ./internal/control ./internal/appserver`

## Phase 4: Rendering

- [ ] Remove Telegram Bot API concepts from render code.
- [ ] Decide whether `internal/tgformat` is still needed.
- [ ] If Markdown rendering is still needed for Feishu, rename `tgformat` to a neutral package.
- [ ] If it is not needed, delete `internal/tgformat`.
- [ ] Ensure Feishu-facing cards use Feishu JSON/card rendering paths.

Verification:

- [ ] `rg -n "tgformat|Telegram entity|Bot API|telegram" internal`
- [ ] `go test ./internal/feishu ./internal/daemon`

## Phase 5: Tests

- [ ] Delete Telegram-only test cases.
- [ ] Rewrite useful lifecycle coverage as Feishu topic tests.
- [ ] Remove Telegram wording from test names, fixtures, and assertions.
- [ ] Remove Telegram demo/e2e test assumptions.

Verification:

- [ ] `rg -n "Telegram|telegram|PanelSourceTelegram|telegram_origin" internal tests cmd`
- [ ] `go test ./...`

## Phase 6: Documentation And Assets

- [ ] Rewrite README as Feishu-only.
- [ ] Rewrite Quickstart as Feishu-only.
- [ ] Remove Telegram wiki pages.
- [ ] Remove Telegram demo docs.
- [ ] Remove Telegram screenshots/assets.
- [ ] Update ADR/research/testing docs or delete stale Telegram-only docs.
- [ ] Update `AGENTS.md` Telegram rules to Feishu-only rules.

Verification:

- [ ] `rg -n "Telegram|telegram|tg" README.md AGENTS.md docs`

## Final Acceptance

- [ ] `rg -n "Telegram|telegram|tgformat|PanelSourceTelegram|telegram_origin|Bot API"` returns no runtime/product references.
- [ ] `go test ./...` passes.
- [ ] `go build -buildvcs=false -o /Users/vico/.local/bin/ctr-go ./cmd/ctr-go` passes.
- [ ] Restart local service.
- [ ] Feishu live check: `/chats`.
- [ ] Feishu live check: open an existing chat topic.
- [ ] Feishu live check: Codex desktop input appears in Feishu topic.
- [ ] Feishu live check: Feishu topic reply reaches Codex.
- [ ] Feishu live check: image sent from Feishu reaches Codex.
- [ ] Feishu live check: image sent from Codex appears in Feishu.
- [ ] Feishu live check: final card appears once and stops polling.
