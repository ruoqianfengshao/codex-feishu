# Contributing

Thanks for considering a contribution to `codex-tg`.

## Development Loop

```powershell
go test ./...
go build -buildvcs=false ./...
```

For Feishu UI, routing, card, topic, image, or lifecycle changes, also validate
the changed path against a real Feishu/Lark bot when available. Unit tests are
required, but chat UX and callback behavior need live readback.

## Pull Request Checklist

- The change keeps Codex App Server as the backend source of truth.
- Thread routing remains thread-first and does not depend on rendered card text.
- Feishu topic/panel behavior changes include focused tests.
- Details, logs, and file exports remain explicit on-demand actions.
- No `.env`, app secrets, chat ids, local databases, logs, binaries, or private
  screenshots are committed.

## Commit Hygiene

Keep commits focused. A good commit changes one behavior, test surface, or
documentation layer at a time.
