//go:build !darwin

package daemon

import (
	"context"
	"fmt"
	"runtime"
)

func submitCodexDesktopPromptAfterDeepLink(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("desktop prompt submission is not supported on %s", runtime.GOOS)
}

func createCodexDesktopProjectlessThreadWithPromptDarwin(ctx context.Context, prompt string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("desktop projectless prompt submission is not supported on %s", runtime.GOOS)
}
