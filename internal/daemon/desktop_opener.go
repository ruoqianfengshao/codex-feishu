package daemon

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
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

func createCodexDesktopThreadWithPrompt(ctx context.Context, cwd, prompt string, projectless bool) error {
	cwd = strings.TrimSpace(cwd)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("prompt is required")
	}
	if projectless {
		return createCodexDesktopProjectlessThreadWithPrompt(ctx, prompt)
	}
	if cwd == "" {
		return fmt.Errorf("cwd is required")
	}
	values := url.Values{"prompt": []string{prompt}}
	values.Set("path", cwd)
	deepLink := "codex://new?" + values.Encode()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", deepLink)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", deepLink)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", deepLink)
	}
	if err := cmd.Run(); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("desktop prompt submission is not supported on %s", runtime.GOOS)
	}
	timer := time.NewTimer(700 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
	}
	return submitCodexDesktopPromptAfterDeepLink(ctx)
}

func createCodexDesktopProjectlessThreadWithPrompt(ctx context.Context, prompt string) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("prompt is required")
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("desktop projectless prompt submission is not supported on %s", runtime.GOOS)
	}
	return createCodexDesktopProjectlessThreadWithPromptDarwin(ctx, prompt)
}
