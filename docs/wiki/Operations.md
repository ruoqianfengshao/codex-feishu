# Operations

Common commands:

```powershell
ctr-go service status
ctr-go service restart
ctr-go doctor
ctr-go status
ctr-go repair
```

`ctr-go doctor` is the AI-friendly preflight check. It prints JSON with:

- `health.ok`: true only when blocking checks pass.
- `health.checks[]`: stable check ids, status, message, remediation, and details.
- `health.next_actions`: concise remediation steps suitable for another agent.

Use `health.checks` before reading logs. The check starts a temporary Codex
app-server, verifies basic Codex RPCs, validates Feishu credentials, and reports
recent persisted daemon errors without sending messages to Feishu.

After Feishu-facing code changes:

1. Rebuild the binary.
2. Restart the service.
3. Test the changed command or topic flow in Feishu.
4. Check daemon logs or SQLite state if routing, topic creation, or polling is involved.

Avoid forced restarts while a Codex turn is actively running through the daemon.
Wait for the final card or stop the turn first when possible.
