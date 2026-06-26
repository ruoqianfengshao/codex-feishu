package daemon

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
)

func openCodexDesktopThread(ctx context.Context, threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if !codexThreadIDPattern.MatchString(threadID) {
		return fmt.Errorf("invalid Codex thread id: %s", threadID)
	}
	deepLink := "codex://threads/" + url.PathEscape(threadID)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", deepLink)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", deepLink)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", deepLink)
	}
	return cmd.Run()
}
