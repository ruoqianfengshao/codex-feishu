# Plan Mode

Plan Mode can be started from Feishu with:

```text
/plan <text>
/plan <thread_id> <text>
/reply --plan <thread_id> <text>
```

When Codex asks for a Plan Mode choice or approval, the daemon sends a routeable
Feishu card. Replying to the card or clicking its buttons routes the answer
back to the matching Codex request.

If a thread remains in Plan Mode after a plan turn, use the card controls or
`/stop <thread>` before sending the next normal prompt.
