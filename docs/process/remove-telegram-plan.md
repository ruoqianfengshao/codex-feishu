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

- [x] Remove Telegram default `HandleMessage` / `HandleCallback` entry points.
- [x] Keep only Feishu source entry points.
- [x] Remove Telegram run notice behavior.
- [x] Remove Telegram duplicate-user-message guards.
- [x] Remove global observer behavior that only existed for Telegram.
- [x] Remove Telegram-specific lifecycle repair/status wording.

Verification:

- [x] `rg -n "Telegram|telegram|PanelSourceGlobalObserver|GlobalObserver" internal/daemon internal/model`
  - Remaining `telegram` hits in daemon/model are module path or repo-name strings only.
- [x] `go test ./internal/daemon ./internal/model ./internal/storage ./internal/feishu`

## Phase 4: Rendering

- [x] Remove Telegram Bot API concepts from render code.
- [x] Decide whether `internal/tgformat` is still needed.
- [x] If Markdown rendering is still needed for Feishu, rename `tgformat` to a neutral package.
- [x] If it is not needed, delete `internal/tgformat`.
- [x] Ensure Feishu-facing cards use Feishu JSON/card rendering paths.

Verification:

- [x] `rg -n "tgformat|Telegram entity|Bot API|telegramify|telegram" internal/daemon internal/feishu internal/model internal/msgformat go.mod go.sum`
- [x] `go test ./internal/msgformat ./internal/daemon ./internal/feishu ./internal/model ./internal/storage`

## Phase 5: Tests

- [x] Delete Telegram-only storage/global observer test cases.
- [ ] Rewrite useful lifecycle coverage as Feishu topic tests.
- [x] Remove Telegram/global observer wording from runtime test names, fixtures, and assertions touched so far.
- [ ] Remove Telegram demo/e2e test assumptions.
- [x] Remove legacy Telegram route table and message-id column names from active storage schema.
- [x] Remove global observer and run notice storage state.

Verification:

- [x] `rg -n "Telegram|telegram|PanelSourceTelegram|telegram_origin|GlobalObserver|global_observer|RunNotice|run_notice|observer_targets|telegram_message" internal/storage internal/model internal/daemon cmd internal/feishu`
  - Remaining `telegram_message_*` hits are one-time deletion checks for old database artifacts.
- [x] `go test ./internal/storage ./internal/model ./internal/daemon ./internal/feishu`
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
