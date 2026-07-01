# Architecture

`codex-feishu` is a local daemon with three main boundaries:

- Codex App Server client for threads, turns, snapshots, live events, approvals,
  and history.
- Feishu/Lark transport for bot DM commands, topic messages, cards, callbacks,
  files, and images.
- SQLite storage for routes, callbacks, Feishu topic mappings, snapshots,
  panels, delivery queue, and daemon state.

Codex `threadId` is the durable identity. Feishu chat, topic, and message ids
are routing surfaces.

The daemon syncs Codex threads to Feishu only after the user opens a thread
topic from the bot DM. There is no global observer mode.
