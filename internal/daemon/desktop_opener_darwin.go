//go:build darwin

package daemon

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation -framework ApplicationServices
#import <ApplicationServices/ApplicationServices.h>
#import <Foundation/Foundation.h>
#include <stdlib.h>

static char* runAppleScript(const char* source) {
	@autoreleasepool {
		NSString *scriptSource = [NSString stringWithUTF8String:source];
		NSAppleScript *script = [[NSAppleScript alloc] initWithSource:scriptSource];
		NSDictionary *errorInfo = nil;
		[script executeAndReturnError:&errorInfo];
		if (errorInfo == nil) {
			return NULL;
		}
		NSString *message = [[errorInfo description] copy];
		return strdup([message UTF8String]);
	}
}

static bool accessibilityTrusted(bool prompt) {
	@autoreleasepool {
		const void *keys[] = { kAXTrustedCheckOptionPrompt };
		const void *values[] = { prompt ? kCFBooleanTrue : kCFBooleanFalse };
		CFDictionaryRef options = CFDictionaryCreate(
			kCFAllocatorDefault,
			keys,
			values,
			1,
			&kCFCopyStringDictionaryKeyCallBacks,
			&kCFTypeDictionaryValueCallBacks
		);
		bool trusted = AXIsProcessTrustedWithOptions(options);
		CFRelease(options);
		return trusted;
	}
}
*/
import "C"

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unsafe"
)

func submitCodexDesktopPromptAfterDeepLink(ctx context.Context) error {
	if err := ensureAccessibilityTrusted(ctx); err != nil {
		return err
	}
	script := `
tell application "Codex" to activate
delay 0.2
tell application "System Events" to key code 36
`
	if err := runAppleScriptInProcess(ctx, script); err == nil {
		return nil
	}
	return runAppleScriptInProcess(ctx, `
tell application "Codex" to activate
delay 0.2
tell application "System Events" to keystroke return
`)
}

func createCodexDesktopProjectlessThreadWithPromptDarwin(ctx context.Context, prompt string) error {
	if err := ensureAccessibilityTrusted(ctx); err != nil {
		return err
	}
	script := `
tell application "Codex" to activate
delay 0.4
tell application "System Events" to keystroke "n" using {option down, command down}
delay 0.8
set oldClipboard to missing value
try
	set oldClipboard to the clipboard
end try
set the clipboard to ` + appleScriptStringLiteral(prompt) + `
delay 0.1
tell application "System Events"
	keystroke "a" using command down
	delay 0.05
	key code 51
	delay 0.05
	keystroke "v" using command down
	delay 0.2
	key code 36
	delay 0.2
	key code 76
	delay 0.2
	key code 36 using command down
end tell
delay 0.1
try
	if oldClipboard is not missing value then set the clipboard to oldClipboard
end try
`
	return runAppleScriptInProcess(ctx, script)
}

func ensureAccessibilityTrusted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if bool(C.accessibilityTrusted(false)) {
		return nil
	}
	return fmt.Errorf("accessibility permission is not granted to current ctr-go")
}

func runAppleScriptInProcess(ctx context.Context, script string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := 15 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return ctx.Err()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		source := C.CString(script)
		defer C.free(unsafe.Pointer(source))
		errMessage := C.runAppleScript(source)
		if errMessage == nil {
			errCh <- nil
			return
		}
		defer C.free(unsafe.Pointer(errMessage))
		message := C.GoString(errMessage)
		if strings.TrimSpace(message) == "" {
			message = "unknown AppleScript error"
		}
		errCh <- fmt.Errorf("apple script failed: %s", message)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
		return ctx.Err()
	case <-runCtx.Done():
		return fmt.Errorf("apple script timed out: %w", runCtx.Err())
	}
}

func appleScriptStringLiteral(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}
