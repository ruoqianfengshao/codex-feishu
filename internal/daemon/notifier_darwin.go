//go:build darwin

package daemon

import (
	"context"
	"os/exec"
	"strings"
)

type osascriptNotifier struct{}

func newSystemNotifier() Notifier {
	return osascriptNotifier{}
}

func (osascriptNotifier) Notify(ctx context.Context, notification SystemNotification) error {
	title := strings.TrimSpace(notification.Title)
	if title == "" {
		title = "Codex"
	}
	message := strings.TrimSpace(notification.Message)
	if message == "" {
		message = "Codex session update"
	}
	script := `display notification ` + appleScriptQuote(message) + ` with title ` + appleScriptQuote(title)
	return exec.CommandContext(ctx, "osascript", "-e", script).Run()
}

func appleScriptQuote(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return `"` + value + `"`
}
