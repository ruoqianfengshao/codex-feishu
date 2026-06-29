# Operations

Common commands:

```powershell
ctr-go service status
ctr-go service restart
ctr-go doctor
ctr-go status
ctr-go repair
```

After Feishu-facing code changes:

1. Rebuild the binary.
2. Restart the service.
3. Test the changed command or topic flow in Feishu.
4. Check daemon logs or SQLite state if routing, topic creation, or polling is involved.

Avoid forced restarts while a Codex turn is actively running through the daemon.
Wait for the final card or stop the turn first when possible.
