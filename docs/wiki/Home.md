# codex-tg Wiki

`codex-tg` is a local Codex control bridge for Feishu/Lark.

## Start Here

- [Quickstart](Quickstart.md)
- [Architecture](Architecture.md)
- [Plan Mode](Plan-Mode.md)
- [Security](Security.md)
- [Operations](Operations.md)

## Core Idea

Keep Codex local while making opened Codex threads controllable from Feishu
topics. The daemon owns Feishu connectivity, Codex App Server connectivity,
SQLite routing state, topic panels, callbacks, and delivery retries.
