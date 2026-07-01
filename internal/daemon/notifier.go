package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/mideco-tech/codex-tg/internal/appserver"
	"github.com/mideco-tech/codex-tg/internal/model"
)

const (
	systemNotificationCompleted = "completed"
	systemNotificationFailed    = "failed"
	systemNotificationApproval  = "approval"
)

type SystemNotification struct {
	Kind      string
	Title     string
	Message   string
	ThreadID  string
	TurnID    string
	RequestID string
}

type Notifier interface {
	Notify(ctx context.Context, notification SystemNotification) error
}

type noopNotifier struct{}

func (noopNotifier) Notify(context.Context, SystemNotification) error {
	return nil
}

func (s *Service) SetNotifier(notifier Notifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if notifier == nil {
		notifier = noopNotifier{}
	}
	s.notifier = notifier
}

func (s *Service) notifyPendingApproval(ctx context.Context, approval model.PendingApproval) {
	requestID := strings.TrimSpace(approval.RequestID)
	if requestID == "" {
		return
	}
	threadID := strings.TrimSpace(approval.ThreadID)
	title := "Codex needs approval"
	if strings.EqualFold(approval.PromptKind, "user_input") {
		title = "Codex needs input"
	}
	message := strings.TrimSpace(approval.Question)
	if message == "" {
		message = shortNotificationThread(threadID, approval.TurnID)
	} else if suffix := shortNotificationThread(threadID, approval.TurnID); suffix != "" {
		message = message + "\n" + suffix
	}
	message = trimSystemNotificationMessage(message)
	s.notifySystemOnce(ctx, systemNotificationKey(systemNotificationApproval, requestID), SystemNotification{
		Kind:      systemNotificationApproval,
		Title:     title,
		Message:   message,
		ThreadID:  threadID,
		TurnID:    strings.TrimSpace(approval.TurnID),
		RequestID: requestID,
	})
}

func (s *Service) notifyTerminalSnapshot(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot) {
	if snapshot == nil {
		return
	}
	status := strings.TrimSpace(snapshot.LatestTurnStatus)
	if !isTerminalTurnStatus(status) {
		return
	}
	turnID := strings.TrimSpace(snapshot.LatestTurnID)
	if strings.TrimSpace(thread.ID) == "" || turnID == "" {
		return
	}
	kind := systemNotificationCompleted
	title := "Codex session completed"
	switch strings.ToLower(status) {
	case "completed":
		kind = systemNotificationCompleted
	case "failed", "interrupted", "canceled", "cancelled":
		kind = systemNotificationFailed
		title = "Codex session failed"
	default:
		return
	}
	message := terminalNotificationMessage(thread, status, snapshot)
	s.notifySystemOnce(ctx, systemNotificationKey(kind, thread.ID, turnID, status), SystemNotification{
		Kind:     kind,
		Title:    title,
		Message:  message,
		ThreadID: strings.TrimSpace(thread.ID),
		TurnID:   turnID,
	})
}

func (s *Service) notifySystemOnce(ctx context.Context, key string, notification SystemNotification) {
	return
}

func systemNotificationKey(parts ...string) string {
	clean := make([]string, 0, len(parts)+1)
	clean = append(clean, "system_notification")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			clean = append(clean, part)
		}
	}
	return strings.Join(clean, ".")
}

func terminalNotificationMessage(thread model.Thread, status string, snapshot *appserver.ThreadReadSnapshot) string {
	label := strings.TrimSpace(thread.Label())
	if label == "" {
		label = strings.TrimSpace(thread.ID)
	}
	statusLine := "Status: " + strings.TrimSpace(status)
	if summary := strings.TrimSpace(snapshot.LatestFinalText); summary != "" {
		return trimSystemNotificationMessage(fmt.Sprintf("%s\n%s\n%s", label, statusLine, firstNotificationLine(summary)))
	}
	return trimSystemNotificationMessage(fmt.Sprintf("%s\n%s", label, statusLine))
}

func shortNotificationThread(threadID, turnID string) string {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	switch {
	case threadID != "" && turnID != "":
		return "Thread " + shortNotificationID(threadID) + " / turn " + shortNotificationID(turnID)
	case threadID != "":
		return "Thread " + shortNotificationID(threadID)
	case turnID != "":
		return "Turn " + shortNotificationID(turnID)
	default:
		return ""
	}
}

func shortNotificationID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func firstNotificationLine(text string) string {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return trimSystemNotificationMessage(line)
		}
	}
	return ""
}

func trimSystemNotificationMessage(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= 220 {
		return text
	}
	return strings.TrimSpace(string(runes[:217])) + "..."
}
