# Security Policy

## Supported Model

`codex-feishu` is a local-first Feishu bridge:

- Codex App Server runs locally.
- Feishu/Lark is the remote input and rendering surface.
- SQLite state stays on the operator machine.
- Feishu access should be restricted with app permissions and optional allowlists.

## Do Not Expose

- Do not expose Codex App Server on a public network interface.
- Do not publish Feishu app secrets, local config files, SQLite databases, logs,
  or screenshots with private data.
- Do not run the bot in broad shared chats unless access control and routing are
  explicitly reviewed.

## Reporting

For security issues, open a private advisory if GitHub Security Advisories are
enabled. Otherwise, open an issue with minimal reproduction details and without
secrets.
