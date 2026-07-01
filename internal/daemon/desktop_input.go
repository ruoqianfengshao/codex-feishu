package daemon

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ruoqianfengshao/codex-feishu/internal/appserver"
	"github.com/ruoqianfengshao/codex-feishu/internal/desktopipc"
	"github.com/ruoqianfengshao/codex-feishu/internal/model"
)

type desktopInputDispatcher interface {
	LoadCompleteHistory(ctx context.Context, threadID string) (map[string]any, error)
	StartTurn(ctx context.Context, threadID string, turnStartParams map[string]any) (map[string]any, error)
	SteerTurn(ctx context.Context, threadID string, input []map[string]any, restoreMessage map[string]any) (map[string]any, error)
}

type closeableDesktopInputDispatcher interface {
	Close() error
}

const (
	desktopInputOpenRetryAttempts = 8
	desktopInputOpenRetryDelay    = 250 * time.Millisecond
)

func (s *Service) maybeSendFeishuInputViaDesktop(ctx context.Context, chatID, topicID int64, thread *model.Thread, routeTurnID, text, collaborationMode string) (map[string]any, bool, bool, string, error) {
	if s == nil || !s.cfg.OpenCodexDesktopOnFeishu || thread == nil {
		return nil, false, false, "", nil
	}
	s.mu.RLock()
	dispatcher := s.desktopInputDispatcher
	s.mu.RUnlock()
	if dispatcher == nil {
		dispatcher = desktopipc.New("", s.cfg.RequestTimeout)
	}
	opened := s.maybeOpenCodexDesktopForInput(ctx, thread.ID, model.PanelSourceFeishuInput)
	requestCtx, cancel := context.WithTimeout(ctx, desktopInputTimeout(s.cfg.RequestTimeout))
	defer cancel()
	s.logLifecycle("codex_desktop_input_attempt", lifecycleFields{
		"chat_key":      model.ChatKey(chatID, topicID),
		"source_mode":   model.PanelSourceFeishuInput,
		"thread_id":     thread.ID,
		"route_turn_id": routeTurnID,
		"text_len":      len([]rune(text)),
		"text_sha256":   shortTextHash(text),
	})
	historyResult, err := s.loadDesktopCompleteHistoryForInput(requestCtx, dispatcher, thread.ID, opened)
	if err != nil {
		if desktopInputShouldFallback(err) {
			s.logLifecycle("codex_desktop_input_unavailable", lifecycleFields{
				"thread_id": thread.ID,
				"error":     sanitizeDiagnosticString(err.Error()),
			})
			return nil, false, false, "", nil
		}
		return nil, true, false, "", err
	}
	s.logLifecycle("codex_desktop_input_history_loaded", desktopInputHistoryFields(thread.ID, historyResult))
	input := desktopInputParts(text)
	restoreMessage := desktopRestoreMessage(thread)
	steerState, _ := s.resolveArmedSteer(ctx, chatID, topicID)
	var result map[string]any
	var steerErr error
	route := ""
	switch {
	case steerState != nil && steerState.ThreadID == thread.ID && strings.TrimSpace(steerState.TurnID) != "":
		route = "armed"
		result, steerErr = dispatcher.SteerTurn(requestCtx, thread.ID, input, restoreMessage)
		if steerErr == nil {
			_ = s.store.ClearSteerState(ctx, chatID, topicID)
		}
	case strings.TrimSpace(routeTurnID) != "":
		route = "reply"
		result, steerErr = dispatcher.SteerTurn(requestCtx, thread.ID, input, restoreMessage)
	case threadLooksActiveForInput(thread) && strings.TrimSpace(thread.ActiveTurnID) != "":
		route = "active_turn"
		result, steerErr = dispatcher.SteerTurn(requestCtx, thread.ID, input, restoreMessage)
	}
	if result != nil {
		follower := desktopFollowerResult(result)
		turnID := appserverThreadTurnID(follower)
		if strings.TrimSpace(turnID) == "" {
			s.logLifecycle("codex_desktop_input_steer_empty_turn", lifecycleFields{
				"thread_id": thread.ID,
				"route":     route,
			})
			result = nil
		} else {
			s.logLifecycle("codex_desktop_input_steered", lifecycleFields{
				"thread_id": thread.ID,
				"route":     route,
				"turn_id":   turnID,
			})
			return follower, true, false, "", nil
		}
	}
	if steerErr != nil {
		s.logLifecycle("codex_desktop_input_steer_failed", lifecycleFields{
			"thread_id": thread.ID,
			"route":     route,
			"error":     sanitizeDiagnosticString(steerErr.Error()),
		})
		if desktopInputShouldFallback(steerErr) {
			return nil, false, false, "", nil
		}
		if steerFailureMeansNoActiveTurn(steerErr) {
			thread.Status = "idle"
			thread.ActiveTurnID = ""
			steerErr = nil
		}
		if threadLooksActiveForInput(thread) || steerFailureImpliesActive(steerErr) {
			return nil, true, false, "", steerErr
		}
	}
	effectiveCollaborationMode := strings.TrimSpace(collaborationMode)
	usedDefaultOverride := false
	if effectiveCollaborationMode == "" && s.threadCollaborationOverride(ctx, thread.ID) == collaborationModeDefault {
		effectiveCollaborationMode = collaborationModeDefault
		usedDefaultOverride = true
	}
	options := s.turnStartOptions(ctx, effectiveCollaborationMode, thread)
	params, err := appserver.TurnStartParams(thread.ID, text, thread.CWD, options)
	if err != nil {
		if desktopInputShouldFallback(err) {
			s.logLifecycle("codex_desktop_input_start_params_fallback", lifecycleFields{
				"thread_id": thread.ID,
				"error":     sanitizeDiagnosticString(err.Error()),
			})
			return nil, false, false, "", nil
		}
		return nil, true, false, "", err
	}
	params["input"] = input
	result, err = dispatcher.StartTurn(requestCtx, thread.ID, params)
	if err != nil {
		if desktopInputShouldFallback(err) {
			s.logLifecycle("codex_desktop_input_start_unavailable", lifecycleFields{
				"thread_id": thread.ID,
				"error":     sanitizeDiagnosticString(err.Error()),
			})
			return nil, false, false, "", nil
		}
		return nil, true, false, "", err
	}
	s.logLifecycle("codex_desktop_input_started", lifecycleFields{
		"thread_id": thread.ID,
		"turn_id":   appserverThreadTurnID(desktopFollowerResult(result)),
	})
	if usedDefaultOverride || effectiveCollaborationMode != "" {
		_ = s.clearThreadCollaborationOverride(ctx, thread.ID)
	}
	_ = s.store.ClearSteerState(ctx, chatID, topicID)
	return desktopFollowerResult(result), true, true, effectiveCollaborationMode, nil
}

func desktopInputTimeout(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return 30 * time.Second
}

func desktopInputHistoryFields(threadID string, historyResult map[string]any) lifecycleFields {
	fields := lifecycleFields{"thread_id": threadID}
	if historyResult == nil {
		return fields
	}
	if fromBroadcast, ok := historyResult["fromBroadcast"].(bool); ok {
		fields["from_broadcast"] = fromBroadcast
	}
	if ownerClientID := strings.TrimSpace(stringValueFromAny(historyResult["ownerClientId"])); ownerClientID != "" {
		fields["owner_client_id"] = ownerClientID
	}
	if revision, ok := historyResult["revision"]; ok {
		fields["revision"] = revision
	}
	return fields
}

func stringValueFromAny(value any) string {
	text, _ := value.(string)
	return text
}

func (s *Service) loadDesktopCompleteHistoryForInput(ctx context.Context, dispatcher desktopInputDispatcher, threadID string, opened bool) (map[string]any, error) {
	attempts := 1
	if opened {
		attempts = desktopInputOpenRetryAttempts
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		result, err := dispatcher.LoadCompleteHistory(ctx, threadID)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !desktopInputShouldFallback(err) || attempt == attempts {
			break
		}
		s.logLifecycle("codex_desktop_input_retry", lifecycleFields{
			"thread_id": threadID,
			"attempt":   attempt + 1,
			"error":     sanitizeDiagnosticString(err.Error()),
		})
		timer := time.NewTimer(desktopInputOpenRetryDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func desktopTextInput(text string) []map[string]any {
	return desktopInputParts(text)
}

func desktopInputParts(text string) []map[string]any {
	imagePath := desktopImagePathFromText(text)
	if imagePath == "" {
		return []map[string]any{
			{"type": "text", "text": text, "text_elements": []any{}},
		}
	}
	caption := desktopImageCaptionFromText(text)
	if caption == "" {
		return []map[string]any{
			{"type": "text", "text": codexImageAttachmentMetadataText(text, imagePath), "text_elements": []any{}},
			{"type": "localImage", "path": imagePath},
		}
	}
	return []map[string]any{
		{"type": "text", "text": caption, "text_elements": []any{}},
		{"type": "localImage", "path": imagePath},
	}
}

func desktopImagePathFromText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if path := imagePathBetween(text, `path="`, `"`); path != "" {
		return path
	}
	for _, marker := range []string{"已保存到：", "saved at:"} {
		index := strings.Index(text, marker)
		if index < 0 {
			continue
		}
		path := strings.TrimSpace(text[index+len(marker):])
		if lineEnd := strings.IndexByte(path, '\n'); lineEnd >= 0 {
			path = path[:lineEnd]
		}
		path = strings.TrimSpace(path)
		if path != "" {
			return path
		}
	}
	return ""
}

func desktopImageCaptionFromText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	for _, marker := range []string{"用户发送了一张图片，已保存到：", "The user sent an image saved at:"} {
		if strings.HasPrefix(trimmed, marker) {
			return ""
		}
	}
	if strings.Contains(trimmed, `<image `) && strings.Contains(trimmed, `path="`) {
		return ""
	}
	return trimmed
}

func codexImageAttachmentMetadataText(text, imagePath string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if request := codexImageAttachmentRequest(text); request != "" {
		return request
	}
	name := imagePath
	if slash := strings.LastIndexAny(name, `/\`); slash >= 0 {
		name = name[slash+1:]
	}
	return "\n# Files mentioned by the user:\n\n## " + name + ": " + imagePath + "\n\n## My request for Codex:\n"
}

func codexImageAttachmentRequest(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	start := -1
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if isDesktopImageRequestHeading(strings.TrimSpace(line)) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	out := make([]string, 0, len(lines)-start)
	for _, line := range lines[start:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<image ") && strings.Contains(trimmed, `path="`) {
			break
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func isDesktopImageRequestHeading(line string) bool {
	for strings.HasPrefix(line, "#") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
	}
	return strings.EqualFold(line, "My request for Codex:")
}

func imagePathBetween(text, startMarker, endMarker string) string {
	start := strings.Index(text, startMarker)
	if start < 0 {
		return ""
	}
	start += len(startMarker)
	end := strings.Index(text[start:], endMarker)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

func desktopRestoreMessage(thread *model.Thread) map[string]any {
	cwd := ""
	if thread != nil {
		cwd = strings.TrimSpace(thread.CWD)
	}
	workspaceRoots := []string{}
	if cwd != "" {
		workspaceRoots = append(workspaceRoots, cwd)
	}
	restoreMessage := map[string]any{
		"context": map[string]any{
			"workspaceRoots":    workspaceRoots,
			"collaborationMode": nil,
		},
		"responsesapiClientMetadata": map[string]any{},
	}
	if cwd != "" {
		restoreMessage["cwd"] = cwd
	}
	return restoreMessage
}

func desktopFollowerResult(payload map[string]any) map[string]any {
	if nested, ok := payload["result"].(map[string]any); ok {
		return nested
	}
	return payload
}

func desktopInputShouldFallback(err error) bool {
	return err == nil ||
		errors.Is(err, desktopipc.ErrNoClientFound) ||
		strings.Contains(strings.ToLower(err.Error()), "no-client-found") ||
		strings.Contains(strings.ToLower(err.Error()), "client-not-found") ||
		strings.Contains(strings.ToLower(err.Error()), "client-disconnected") ||
		strings.Contains(strings.ToLower(err.Error()), "webcontents-destroyed") ||
		strings.Contains(strings.ToLower(err.Error()), "webview-disposed") ||
		strings.Contains(strings.ToLower(err.Error()), "provider-disposed") ||
		strings.Contains(strings.ToLower(err.Error()), "connection refused") ||
		strings.Contains(strings.ToLower(err.Error()), "codex model is required for collaboration mode") ||
		strings.Contains(strings.ToLower(err.Error()), "i/o timeout") ||
		strings.Contains(strings.ToLower(err.Error()), "no such file") ||
		strings.Contains(strings.ToLower(err.Error()), "socket path is unavailable")
}
