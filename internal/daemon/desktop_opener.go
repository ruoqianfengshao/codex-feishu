package daemon

import (
	"bytes"
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
	if err := ensureDarwinUIScripting(ctx); err != nil {
		return err
	}
	timer := time.NewTimer(700 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
	}
	if err := runOSAScript(ctx,
		"-e", `tell application "Codex" to activate`,
		"-e", `delay 0.2`,
		"-e", `tell application "System Events" to keystroke return using command down`,
	); err == nil {
		return nil
	}
	return runOSAScript(ctx,
		"-e", `tell application "Codex" to activate`,
		"-e", `delay 0.2`,
		"-e", `tell application "System Events" to keystroke return`,
	)
}

func createCodexDesktopProjectlessThreadWithPrompt(ctx context.Context, prompt string) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return fmt.Errorf("prompt is required")
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("desktop projectless prompt submission is not supported on %s", runtime.GOOS)
	}
	if err := ensureDarwinUIScripting(ctx); err != nil {
		return err
	}
	script := `
tell application "Codex" to activate
delay 0.4
tell application "System Events" to tell process "Codex"
	set clickedQuickChat to false
	repeat with menuBarItem in menu bar items of menu bar 1
		try
			repeat with menuItem in menu items of menu 1 of menuBarItem
				try
					set cmdChar to value of attribute "AXMenuItemCmdChar" of menuItem
					set cmdModifiers to value of attribute "AXMenuItemCmdModifiers" of menuItem
					if cmdChar is "N" and cmdModifiers is 2 then
						click menuItem
						set clickedQuickChat to true
						exit repeat
					end if
				end try
			end repeat
			if clickedQuickChat then exit repeat
		end try
	end repeat
	if not clickedQuickChat then
		keystroke "n" using {option down, command down}
	end if
end tell
delay 0.8
tell application "System Events" to keystroke "a" using command down
delay 0.1
tell application "System Events" to keystroke ` + appleScriptStringLiteral(prompt) + `
delay 0.1
tell application "System Events" to keystroke return using command down
`
	return runOSAScript(ctx, "-e", script)
}

func ensureDarwinUIScripting(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "osascript", "-e", `tell application "System Events" to get UI elements enabled`).CombinedOutput()
	if err != nil {
		return fmt.Errorf("check macOS UI scripting permission: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.EqualFold(strings.TrimSpace(string(out)), "true") {
		return fmt.Errorf("macOS UI scripting is disabled; enable Accessibility permission for osascript or the service host")
	}
	return nil
}

func appleScriptStringLiteral(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func runOSAScript(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "osascript", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("osascript failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
