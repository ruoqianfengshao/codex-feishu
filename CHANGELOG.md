# Changelog

## Unreleased

## v0.6.14

- Added `ctr-go update` for GitHub Release self-updates with asset selection and `SHA256SUMS` verification.
- Enabled automatic codex-feishu updates for installed user LaunchAgent services by default.
- Added `/help` version information, styled update checking cards, interactive update action, and Feishu completion notice after manual updates.
- Updated install docs to resolve the latest GitHub Release tag dynamically instead of hard-coding an older version.

## v0.6.13

- Added Codex app-server lifecycle handling for `thread/archived`, `thread/deleted`, and `thread/unarchived` notifications.
- Sent Feishu topic notices when a bound Codex thread is archived, deleted, or re-enabled in Codex.
- Kept local thread visibility in sync with Codex archive/delete/unarchive lifecycle events.

## v0.6.12

- Prevented browsing existing Codex threads from `/projects`, `/chats`, or thread cards from automatically creating or activating Feishu topics.
- Kept Feishu topic activation for explicit Feishu input and already-bound thread topics.
- Added regression coverage for browsing-only thread opens and Feishu input topic activation.

## v0.6.11

- Added supervised Feishu websocket reconnects so the daemon rebuilds the long-lived callback connection after network drops.
- Made local `ctr-go repair` restart the macOS LaunchAgent when the service is loaded, giving users a recovery path when Feishu callbacks cannot reach the local daemon.

## v0.6.10

- Added Feishu OpenAPI auth recovery: when a send fails because the access token is invalid or expired, the bot rebuilds the API client, refreshes the tenant access token, and retries the original send once.
- Prevented Feishu card fallback from hiding auth recovery failures.

## v0.6.9

- Added Feishu workflow screenshots to the English and Chinese README files.
- Used a three-column screenshot layout for GitHub README rendering.

## v0.6.8

- Promoted pinned projects and pinned threads to the top of project and chat lists.
- Added pinned labels for pinned projects and pinned threads.
- Applied warmer card colors for project rows, chat rows, and new-thread entries.
- Showed `/projects` project updated times as relative time.

## v0.6.7

- Removed recent error, delivery queue, and connection details from `/status`.
- Merged thread totals and thread mix into one Threads section.
- Kept only the Feishu pie chart for thread mix and included percentages in chart labels.
- Updated README and smoke-test documentation to match the new `/status` layout.

## v0.6.6

- Listed Codex projects from the current Codex desktop project state so removed or archived projects are not shown in `/projects`.
- Showed temporary Codex chats as a separate `/projects` entry and added a projectless "New chat" action.
- Created temporary chats through the Codex desktop Quick Chat menu action so they appear as real Codex projectless chats instead of synthetic project folders.
- Kept project-scoped new threads bound to their selected workspace.
- Improved desktop input retry and app-server fallback behavior when the Codex desktop bridge is unavailable.
- Improved Feishu topic routing, image delivery, card updates, and callback handling tests.

## v0.6.5

- Restored routing for Feishu topic replies when the incoming event resolves to a non-zero topic root while existing panel and message route records were stored under the legacy `topic_id=0` key.
- Added a regression test for rootless/legacy Feishu topic route fallback so topic messages do not incorrectly fall back to the workspace "no Codex thread selected" hint.

## v0.6.4

- Reworked `/help` into interactive command cards for workspace and topic usage.
- Replaced `/setting` submenus with a Feishu form and pre-filled model and reasoning values from the current Codex config when available.
- Removed language switching from `/status`; language remains configurable from `/setting`.
- Refreshed `/status` as a dashboard-style card with KPI sections and a Feishu chart component for thread mix statistics.
- Removed the unused `/reply` command from the README command list.

## v0.6.3

- Wrote the Feishu setup QR code as a PNG image in the temp directory.
- Printed a Markdown image reference for UIs that can render local images.
- Kept the existing terminal QR code and `--no-qr` behavior.

## v0.6.2

- Kept the default macOS instructions on the release tarball and `$HOME/.local/bin`, avoiding `sudo` and password prompts.
- Updated the release workflow to use a dedicated GitHub Release action for release creation and asset upload.

## v0.6.1

- Made the default macOS install path use the release tarball and `$HOME/.local/bin` instead of the `.pkg` installer.
- Documented that the user LaunchAgent setup does not require `sudo`.
- Added architecture detection to the README install snippet for Apple Silicon and Intel Macs.

## v0.6.0

- Introduced the `codex-feishu` fork identity and focused the project on the Feishu/Lark workflow.
- Renamed the project/module from `codex-tg` to `codex-feishu`.
- Added AI-friendly `ctr-go doctor` health checks for installation validation.
- Removed local system notifications.
- Improved Feishu final-card repair and screenshot/image delivery handling.
- Updated `/projects` to filter deleted/archived threads and provide project-scoped new-thread actions.
- Updated documentation, package metadata, service labels, and local defaults for the Feishu-focused fork.
