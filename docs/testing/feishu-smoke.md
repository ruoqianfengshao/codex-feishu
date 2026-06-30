# Feishu Smoke Checklist

Use this checklist after changes that affect Feishu commands, topic routing, image handling, or Codex progress cards. The goal is to verify the actual product effect in Feishu, not only the Go unit tests.

## Preflight

- Build and restart the local service.
- Confirm the Feishu bot responds in the P2P chat.
- Use one existing Codex thread and one newly created temporary chat.
- Keep the daemon log visible while testing.

Useful local checks:

```bash
tail -f "$HOME/.codex-tg/logs/daemon.out.log"
```

## Command Surface

- `/help`
  - Shows only supported MVP commands.
  - Does not show removed commands such as `/threads`, `/workspace`, `/home`, `/newchat`, `/models`, `/lang`, or `/panelmode`.
- `/status`
  - Shows a dashboard-style card.
  - Shows language and topic mode.
  - Shows project count, temporary chat count, active, archived, and deleted chat counts.
  - Does not show current chat context as the main content.
- `/chats`
  - Shows active chats only.
  - Does not show archived or deleted chats.
  - Groups by project, with temporary chats separated.
  - Clicking a chat opens or reopens the corresponding Feishu topic without creating duplicate topics.
- `/projects`
  - Shows projects using the same visual direction as `/chats`.
  - Clicking a project shows active chats under that project.
  - Clicking new thread under a project creates the Codex chat in that project.
- `/new <prompt>`
  - Creates a temporary chat.
  - Does not require a project.
  - Creates the Feishu topic after Codex has enough title context.

## Feishu To Codex

- Send plain text in a Feishu topic.
  - The input appears in Codex Desktop for the same thread.
  - Feishu receives one Codex progress card.
  - The final answer updates the same Codex progress card when possible.
- Send an image only in a Feishu topic.
  - Codex Desktop shows the image as an image, not as local path text.
  - The bridge prompt text is not visible in Codex Desktop.
  - Feishu receives progress and final output.
- Send text plus image in a Feishu topic.
  - Codex Desktop shows both the real user text and the image.
  - No local attachment path appears in Codex Desktop or Feishu.
  - The request does not stall before Codex starts processing.

## Codex To Feishu

- Send plain text from Codex Desktop in a Feishu-opened thread.
  - Feishu first shows the desktop user input card.
  - The desktop user input card contains only the title and real user input.
  - It does not show `[项目]`, `[会话]`, `[user]`, thread IDs, or local file paths.
  - The progress card appears after the desktop user input card.
- Send image plus text from Codex Desktop.
  - Feishu shows the image.
  - Feishu also shows the user text.
  - Feishu does not show the local file path or the Codex clipboard metadata block.

## Codex Progress Card

- While running:
  - The card status says processing/running, not interrupted.
  - The collapsed log contains timeline labels such as thinking, tool call, and running.
  - The card has a Stop button.
  - The card does not have a guide/steer button.
  - The card does not have a Show context button.
- After final:
  - The same card updates to completed when possible.
  - The final answer is visible outside the progress log.
  - Running buttons disappear.
  - No duplicate final card is sent.
- For interrupted-looking intermediate snapshots:
  - Feishu should not display "interrupted" or "已中断" unless the user explicitly stopped the task and that is the final product decision.

## Repetition And Recovery

- Leave a running thread for at least one polling interval.
  - Feishu should not repeatedly send the same desktop user input card.
  - Feishu should not repeatedly send the same progress card.
  - Feishu should not repeatedly send final cards.
- Restart the service while Feishu is open.
  - Existing opened topics still sync after restart.
  - If the long connection drops, `/status` or `/repair` should recover without manual DB edits.

## Pass Criteria

Record the test run with:

- Commit or build ID.
- Feishu bot account.
- Thread title used for testing.
- Passed cases.
- Failed cases with the daemon log lines around the failure.

The release is acceptable only when command surface, text sync, image sync, progress card lifecycle, and duplicate-message checks all pass.
