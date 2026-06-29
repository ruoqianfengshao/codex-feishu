# Security

`codex-tg` is local-first:

- Codex App Server should stay local/private.
- Feishu app id and secret are stored in the private local config.
- SQLite state, logs, config files, and screenshots may contain private data.
- Optional Feishu allowlists should be used for production testing.

Do not commit app secrets, local config, SQLite databases, logs, private
screenshots, or user-specific paths.
