package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ruoqianfengshao/codex-feishu/internal/appserver"
	"github.com/ruoqianfengshao/codex-feishu/internal/model"
	"github.com/ruoqianfengshao/codex-feishu/internal/msgformat"
)

type recordedMessage struct {
	chatID                int64
	topicID               int64
	messageID             int64
	text                  string
	entities              []model.MessageEntity
	style                 string
	imagePath             string
	codexStatus           string
	codexProgressMarkdown string
	codexFinalMarkdown    string
	buttons               [][]model.ButtonSpec
	options               model.SendOptions
}

type recordedDocument struct {
	chatID   int64
	topicID  int64
	fileName string
	filePath string
	data     []byte
	caption  string
	options  model.SendOptions
}

type recordingSender struct {
	messages  []recordedMessage
	documents []recordedDocument
	edits     []recordedMessage
	deletes   []recordedMessage
	editErr   error
}

func (s *recordingSender) SendMessage(ctx context.Context, chatID, topicID int64, text string, buttons [][]model.ButtonSpec, options model.SendOptions) (int64, error) {
	messageID := int64(len(s.messages) + 1)
	s.messages = append(s.messages, recordedMessage{chatID: chatID, topicID: topicID, messageID: messageID, text: text, buttons: buttons, options: options})
	return messageID, nil
}

func (s *recordingSender) SendRenderedMessages(ctx context.Context, chatID, topicID int64, messages []model.RenderedMessage, buttons [][]model.ButtonSpec, options model.SendOptions) ([]int64, error) {
	ids := make([]int64, 0, len(messages))
	for _, message := range messages {
		messageID := int64(len(s.messages) + 1)
		s.messages = append(s.messages, recordedRenderedMessage(chatID, topicID, messageID, message, buttons, options))
		ids = append(ids, messageID)
	}
	return ids, nil
}

func (s *recordingSender) EditMessage(ctx context.Context, chatID, topicID, messageID int64, text string, buttons [][]model.ButtonSpec) error {
	if s.editErr != nil {
		return s.editErr
	}
	s.edits = append(s.edits, recordedMessage{chatID: chatID, topicID: topicID, messageID: messageID, text: text, buttons: buttons})
	return nil
}

func (s *recordingSender) EditRenderedMessage(ctx context.Context, chatID, topicID, messageID int64, rendered model.RenderedMessage, buttons [][]model.ButtonSpec) error {
	if s.editErr != nil {
		return s.editErr
	}
	s.edits = append(s.edits, recordedRenderedMessage(chatID, topicID, messageID, rendered, buttons, model.SendOptions{}))
	return nil
}

func recordedRenderedMessage(chatID, topicID, messageID int64, rendered model.RenderedMessage, buttons [][]model.ButtonSpec, options model.SendOptions) recordedMessage {
	return recordedMessage{
		chatID:                chatID,
		topicID:               topicID,
		messageID:             messageID,
		text:                  rendered.Text,
		entities:              rendered.Entities,
		style:                 rendered.Style,
		imagePath:             rendered.ImagePath,
		codexStatus:           rendered.CodexStatus,
		codexProgressMarkdown: rendered.CodexProgressMarkdown,
		codexFinalMarkdown:    rendered.CodexFinalMarkdown,
		buttons:               buttons,
		options:               options,
	}
}

func (s *recordingSender) DeleteMessage(ctx context.Context, chatID, topicID, messageID int64) error {
	s.deletes = append(s.deletes, recordedMessage{chatID: chatID, topicID: topicID, messageID: messageID})
	return nil
}

func (s *recordingSender) SendDocumentData(ctx context.Context, chatID, topicID int64, fileName string, data []byte, caption string, options model.SendOptions) (int64, error) {
	s.documents = append(s.documents, recordedDocument{
		chatID:   chatID,
		topicID:  topicID,
		fileName: fileName,
		data:     append([]byte(nil), data...),
		caption:  caption,
		options:  options,
	})
	return int64(len(s.documents)), nil
}

type recordingThreadTopicSender struct {
	recordingSender
	topic              *model.FeishuThreadTopic
	canonicalByChatID  map[int64]int64
	ensureTopicChatIDs []int64
	ensureTopicThreads []model.Thread
}

func (s *recordingThreadTopicSender) EnsureThreadTopic(ctx context.Context, chatID int64, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, sourceMode string) (*model.FeishuThreadTopic, error) {
	s.ensureTopicChatIDs = append(s.ensureTopicChatIDs, chatID)
	s.ensureTopicThreads = append(s.ensureTopicThreads, thread)
	if s.topic == nil {
		s.topic = &model.FeishuThreadTopic{
			ChatID:            chatID,
			OpenChatID:        "oc_chat",
			ThreadID:          thread.ID,
			RootMessageID:     9001,
			RootOpenMessageID: "om_root",
			FeishuThreadID:    "othread_root",
		}
	}
	return s.topic, nil
}

func (s *recordingThreadTopicSender) ResolveThreadTopicTarget(ctx context.Context, chatID int64) (int64, error) {
	if s.canonicalByChatID != nil && s.canonicalByChatID[chatID] != 0 {
		return s.canonicalByChatID[chatID], nil
	}
	return chatID, nil
}

func hasRecordedEntity(entities []model.MessageEntity, entityType, language string) bool {
	for _, entity := range entities {
		if entity.Type != entityType {
			continue
		}
		if language == "" || entity.Language == language {
			return true
		}
	}
	return false
}

func hasHeaderKind(text, kind string) bool {
	firstLine := strings.SplitN(text, "\n", 2)[0]
	return strings.Contains(firstLine, "["+kind+"]")
}

func finalMessages(messages []recordedMessage) []recordedMessage {
	out := make([]recordedMessage, 0, len(messages))
	for _, message := range messages {
		if hasHeaderKind(message.text, "Final") {
			out = append(out, message)
		}
	}
	return out
}

func lastFinalMessage(t *testing.T, messages []recordedMessage) recordedMessage {
	t.Helper()
	finals := finalMessages(messages)
	if len(finals) == 0 {
		t.Fatalf("final message not found in %#v", messages)
	}
	return finals[len(finals)-1]
}

func lastCodexPanelMessage(t *testing.T, messages []recordedMessage) recordedMessage {
	t.Helper()
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].style == model.MessageStyleCodexPanel {
			return messages[index]
		}
	}
	t.Fatalf("codex panel message not found in %#v", messages)
	return recordedMessage{}
}

func lastCodexPanelEdit(t *testing.T, edits []recordedMessage) recordedMessage {
	t.Helper()
	for index := len(edits) - 1; index >= 0; index-- {
		if edits[index].style == model.MessageStyleCodexPanel {
			return edits[index]
		}
	}
	t.Fatalf("codex panel edit not found in %#v", edits)
	return recordedMessage{}
}

func buttonToken(rows [][]model.ButtonSpec, label string) string {
	for _, row := range rows {
		for _, button := range row {
			if button.Text == label {
				return button.CallbackData
			}
		}
	}
	return ""
}

func assertOnlyLatestDetailsButton(t *testing.T, rows [][]model.ButtonSpec) {
	t.Helper()
	if buttonToken(rows, "Refresh") == "" && buttonToken(rows, "手动刷新") == "" {
		t.Fatalf("buttons = %#v, want Refresh button", rows)
	}
	if buttonToken(rows, "Stop") != "" || buttonToken(rows, "停止") != "" {
		t.Fatalf("buttons = %#v, want no running buttons", rows)
	}
}

func hasThreadChip(text, threadID string) bool {
	return strings.Contains(strings.SplitN(text, "\n", 2)[0], "[T:"+visualShortID(threadID)+"]")
}

type recordingNotifier struct {
	notifications []SystemNotification
}

func (n *recordingNotifier) Notify(ctx context.Context, notification SystemNotification) error {
	n.notifications = append(n.notifications, notification)
	return nil
}

func TestSyncThreadPanelDoesNotSendCompletedSystemNotification(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.NotifySystem = true
	notifier := &recordingNotifier{}
	service.SetNotifier(notifier)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-system-complete",
		Title:       "System complete",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-system-complete",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-system-complete",
		LatestFinalText:  "All work complete.",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}

	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)
	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(notifier.notifications) != 0 {
		t.Fatalf("notifications = %#v, want none after local system notifications were removed", notifier.notifications)
	}
}

func TestSyncThreadPanelDoesNotSendFailedSystemNotification(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.NotifySystem = true
	notifier := &recordingNotifier{}
	service.SetNotifier(notifier)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-system-failed",
		Title:       "System failed",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-system-failed",
		LatestTurnStatus: "failed",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}

	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)
	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(notifier.notifications) != 0 {
		t.Fatalf("notifications = %#v, want none after local system notifications were removed", notifier.notifications)
	}
}

func TestPendingApprovalDoesNotSendSystemNotification(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.NotifySystem = true
	notifier := &recordingNotifier{}
	service.SetNotifier(notifier)
	ctx := context.Background()
	approval := model.PendingApproval{
		RequestID:  "request-system-approval",
		ThreadID:   "thread-system-approval",
		TurnID:     "turn-system-approval",
		PromptKind: "approval",
		Question:   "Allow command?",
		Status:     "pending",
		UpdatedAt:  model.NowString(),
	}

	service.notifyPendingApproval(ctx, approval)
	service.notifyPendingApproval(ctx, approval)

	if len(notifier.notifications) != 0 {
		t.Fatalf("notifications = %#v, want none after local system notifications were removed", notifier.notifications)
	}
}

func TestSystemNotificationCanBeDisabled(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	service.cfg.NotifySystem = false
	notifier := &recordingNotifier{}
	service.SetNotifier(notifier)
	ctx := context.Background()

	service.notifyPendingApproval(ctx, model.PendingApproval{
		RequestID: "request-system-disabled",
		ThreadID:  "thread-system-disabled",
		TurnID:    "turn-system-disabled",
		Question:  "Allow command?",
		Status:    "pending",
		UpdatedAt: model.NowString(),
	})

	if len(notifier.notifications) != 0 {
		t.Fatalf("notifications = %#v, want none when disabled", notifier.notifications)
	}
}

func TestSyncThreadPanelDoesNotDuplicateFinalAnswerOnRepeatedSync(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-1",
		Title:       "Observer smoke",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}

	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-1",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-fp-1",
		LatestFinalText:  "All work complete.",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceExplicit)
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceExplicit)

	finals := finalMessages(sender.messages)
	if len(finals) != 1 {
		t.Fatalf("final messages = %#v, want one standalone final card", finals)
	}
	if len(sender.messages) != 2 {
		t.Fatalf("message count = %d, want one Codex panel and one final card; messages=%#v", len(sender.messages), sender.messages)
	}
	panelMessage := lastCodexPanelMessage(t, sender.messages)
	if !strings.Contains(panelMessage.text, "Processed") && !strings.Contains(panelMessage.text, "已处理") {
		t.Fatalf("panel message = %#v, want processed status", panelMessage)
	}
	if strings.Contains(panelMessage.text, "Completed") || strings.Contains(panelMessage.text, "已完成") {
		t.Fatalf("panel message = %#v, want no raw completed status", panelMessage)
	}
	if strings.Contains(panelMessage.text, "All work complete.") || panelMessage.codexFinalMarkdown != "" {
		t.Fatalf("panel message = %#v, want no final content in progress panel", panelMessage)
	}
	if !strings.Contains(finals[0].text, "All work complete.") {
		t.Fatalf("final card = %#v, want final content", finals[0])
	}
	if len(sender.documents) != 0 {
		t.Fatalf("documents = %#v, want no tool documents for a completed turn without tool output", sender.documents)
	}
	if len(sender.deletes) != 0 {
		t.Fatalf("deletes = %#v, want retained summary/progress message", sender.deletes)
	}

	panel, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if panel == nil {
		t.Fatal("GetCurrentThreadPanel returned nil")
	}
	if panel.SourceMode != model.PanelSourceExplicit {
		t.Fatalf("panel SourceMode = %q, want explicit", panel.SourceMode)
	}

	if panel.LastFinalNoticeFP != "final-fp-1" {
		t.Fatalf("panel LastFinalNoticeFP = %q, want final-fp-1", panel.LastFinalNoticeFP)
	}
}

func TestSyncThreadPanelFormatsFinalAnswerMarkdownWithEntities(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-md-final",
		Title:       "Markdown final",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-md-final",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-md-fp",
		LatestFinalText:  "Run `rg`:\n\n```bash\nrg -n 'Authorization' stellar_ws.txt\n```",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, "explicit")

	final := lastFinalMessage(t, sender.messages)
	if strings.Contains(final.text, "```") {
		t.Fatalf("final card still contains raw markdown fence: %q", final.text)
	}
	if !hasRecordedEntity(final.entities, "code", "") {
		t.Fatalf("final card entities = %#v, want inline code entity", final.entities)
	}
	if !hasRecordedEntity(final.entities, "pre", "bash") {
		t.Fatalf("final card entities = %#v, want bash pre entity", final.entities)
	}
}

func TestSummaryPanelFormatsCommentaryMarkdownWithoutFinalLabel(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-md-summary",
		Title:       "Markdown summary",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-md-summary",
		LatestTurnStatus: "inProgress",
		LatestAgentMessageEntries: []appserver.AgentMessageEntry{{
			ID:    "agent-1",
			Phase: "commentary",
			Text:  "Checking `node`:\n\n```powershell\nnode -v\n```",
			FP:    "agent-1-fp",
		}},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, "explicit")

	if len(sender.messages) == 0 {
		t.Fatal("no summary message was sent")
	}
	summary := sender.messages[0]
	if hasHeaderKind(summary.text, "Final") {
		t.Fatalf("summary text = %q, must not label commentary as final", summary.text)
	}
	if strings.Contains(summary.text, "```") {
		t.Fatalf("summary text still contains raw markdown fence: %q", summary.text)
	}
	if !hasRecordedEntity(summary.entities, "code", "") {
		t.Fatalf("summary entities = %#v, want inline code entity", summary.entities)
	}
	if !hasRecordedEntity(summary.entities, "pre", "powershell") {
		t.Fatalf("summary entities = %#v, want powershell pre entity", summary.entities)
	}
}

func TestSummaryPanelDisplaysAgentMessagesChronologically(t *testing.T) {
	t.Parallel()

	thread := model.Thread{
		ID:          "thread-order",
		Title:       "Summary order",
		ProjectName: "Codex",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-order",
		LatestTurnStatus: "inProgress",
		LatestAgentMessageEntries: []appserver.AgentMessageEntry{
			{ID: "agent-3", Phase: "commentary", Text: "THIRD newest"},
			{ID: "agent-2", Phase: "commentary", Text: "SECOND middle"},
			{ID: "agent-1", Phase: "commentary", Text: "FIRST oldest"},
		},
	}

	service := newTestService(t)
	messages := service.renderSummaryPanelMarkdown(context.Background(), thread, snapshot, snapshot.LatestAgentMessageEntries, nil)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	text := messages[0].Text
	first := strings.Index(text, "FIRST oldest")
	second := strings.Index(text, "SECOND middle")
	third := strings.Index(text, "THIRD newest")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("summary text missing expected entries: %q", text)
	}
	if !(first < second && second < third) {
		t.Fatalf("summary text order is not chronological: %q", text)
	}
	if strings.Contains(text, "1. [commentary]") || strings.Contains(text, "2. [commentary]") || strings.Contains(text, "3. [commentary]") {
		t.Fatalf("summary text must not number commentary entries: %q", text)
	}
	if strings.Contains(text, "[commentary]") {
		t.Fatalf("summary text leaked commentary phase label: %q", text)
	}
}

func TestSummaryPanelTreatsInterruptedCommentaryAsInProgress(t *testing.T) {
	t.Parallel()

	thread := model.Thread{
		ID:          "thread-interrupted-commentary",
		Title:       "Interrupted commentary",
		ProjectName: "Codex",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-interrupted-commentary",
		LatestTurnStatus: "interrupted",
		DetailItems: []model.DetailItem{
			{ID: "agent-1", Kind: model.DetailItemCommentary, Phase: "commentary", Text: "Checking the status counters.", FP: "agent-1", CommentaryIndex: 1},
		},
	}

	service := newTestService(t)
	messages := service.renderSummaryPanelMarkdown(context.Background(), thread, snapshot, summaryPanelEntries(snapshot), nil)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	text := messages[0].Text
	for _, forbidden := range []string{"已中断", "interrupted", "[commentary]"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("summary text contains %q: %q", forbidden, text)
		}
	}
	if !strings.Contains(messages[0].CodexStatus, "Processing") || !strings.Contains(text, "Thinking...") || !strings.Contains(text, "Checking the status counters.") {
		t.Fatalf("summary text = %q, want in-progress status, thinking log, and commentary text", text)
	}
}

func TestSummaryPanelAddsInterruptedToolStatusLog(t *testing.T) {
	t.Parallel()

	thread := model.Thread{ID: "thread-interrupted-tool", Title: "Interrupted tool", ProjectName: "Codex"}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-interrupted-tool",
		LatestTurnStatus: "interrupted",
		LatestToolLabel:  "go test ./...",
		DetailItems: []model.DetailItem{
			{ID: "tool-1", Kind: model.DetailItemTool, ToolKind: "commandExecution", Label: "go test ./...", Status: "inProgress"},
		},
	}

	service := newTestService(t)
	messages := service.renderSummaryPanelMarkdown(context.Background(), thread, snapshot, nil, nil)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	text := messages[0].Text
	if !strings.Contains(text, "Ran") || !strings.Contains(text, "go test ./...") {
		t.Fatalf("summary text = %q, want command action log", text)
	}
	for _, forbidden := range []string{"已中断", "interrupted"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("summary text contains %q: %q", forbidden, text)
		}
	}
}

func TestSummaryPanelInterleavesDetailStatusLogs(t *testing.T) {
	t.Parallel()

	thread := model.Thread{ID: "thread-interleaved-status", Title: "Interleaved status", ProjectName: "Codex"}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-interleaved-status",
		LatestTurnStatus: "interrupted",
		DetailItems: []model.DetailItem{
			{ID: "agent-1", Kind: model.DetailItemCommentary, Text: "Inspecting the request.", FP: "agent-1", CommentaryIndex: 1},
			{ID: "tool-1", Kind: model.DetailItemTool, ToolKind: "commandExecution", Label: "rg -n status internal/daemon", Status: "completed", FP: "tool-1", CommentaryIndex: 1},
			{ID: "agent-2", Kind: model.DetailItemCommentary, Text: "Found the rendering branch.", FP: "agent-2", CommentaryIndex: 2},
		},
	}

	service := newTestService(t)
	messages := service.renderSummaryPanelMarkdown(context.Background(), thread, snapshot, summaryPanelEntries(snapshot), nil)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	text := messages[0].Text
	firstThinking := strings.Index(text, "Thinking...")
	firstBody := strings.Index(text, "Inspecting the request.")
	tooling := strings.Index(text, "Ran")
	toolBody := strings.Index(text, "rg -n status internal/daemon")
	secondThinking := strings.LastIndex(text, "Thinking...")
	secondBody := strings.Index(text, "Found the rendering branch.")
	if firstThinking < 0 || firstBody < 0 || tooling < 0 || toolBody < 0 || secondThinking < 0 || secondBody < 0 {
		t.Fatalf("summary text missing interleaved status timeline: %q", text)
	}
	if !(firstThinking < firstBody && firstBody < tooling && tooling < toolBody && toolBody < secondThinking && secondThinking < secondBody) {
		t.Fatalf("summary text order is wrong: %q", text)
	}
}

func TestSummaryPanelLongDetailLogShowsRecentCompleteItems(t *testing.T) {
	t.Parallel()

	thread := model.Thread{ID: "thread-long-detail-log", Title: "Long detail log", ProjectName: "Codex"}
	items := make([]model.DetailItem, 0, 1500)
	for i := 1; i <= 1500; i++ {
		items = append(items, model.DetailItem{
			ID:              fmt.Sprintf("agent-%d", i),
			Kind:            model.DetailItemCommentary,
			Text:            fmt.Sprintf("progress item %04d", i),
			FP:              fmt.Sprintf("fp-%d", i),
			CommentaryIndex: i,
		})
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-long-detail-log",
		LatestTurnStatus: "interrupted",
		DetailItems:      items,
	}

	service := newTestService(t)
	messages := service.renderSummaryPanelMarkdown(context.Background(), thread, snapshot, summaryPanelEntries(snapshot), nil)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	text := messages[0].Text
	if !strings.Contains(text, "Process log 1471-1500 / 1500") {
		t.Fatalf("summary text = %q, want recent log range", text)
	}
	if !strings.Contains(text, "progress item 1500") {
		t.Fatalf("summary text = %q, want latest log item", text)
	}
	if strings.Contains(text, "progress item 0001") || strings.Contains(text, "progress item 1400") {
		t.Fatalf("summary text = %q, want old log items omitted", text)
	}
	if strings.Contains(text, "progress item 150") && !strings.Contains(text, "progress item 1500") {
		t.Fatalf("summary text = %q, want complete recent log items only", text)
	}
}

func TestSummaryPanelDropsOversizedRecentDetailItem(t *testing.T) {
	t.Parallel()

	thread := model.Thread{ID: "thread-oversized-detail", Title: "Oversized detail", ProjectName: "Codex"}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-oversized-detail",
		LatestTurnStatus: "interrupted",
		DetailItems: []model.DetailItem{
			{ID: "agent-1", Kind: model.DetailItemCommentary, Text: "kept recent item", FP: "agent-1", CommentaryIndex: 1},
			{ID: "agent-2", Kind: model.DetailItemCommentary, Text: strings.Repeat("x", msgformat.MessageLimit+200), FP: "agent-2", CommentaryIndex: 2},
		},
	}

	service := newTestService(t)
	messages := service.renderSummaryPanelMarkdown(context.Background(), thread, snapshot, summaryPanelEntries(snapshot), nil)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	text := messages[0].Text
	if !strings.Contains(text, "kept recent item") {
		t.Fatalf("summary text = %q, want displayable recent item", text)
	}
	if strings.Contains(text, strings.Repeat("x", 100)) {
		t.Fatalf("summary text = %q, want oversized item omitted entirely", text)
	}
}

func TestSummaryPanelShowsFileChangeAsEditedAction(t *testing.T) {
	t.Parallel()

	thread := model.Thread{ID: "thread-file-change-action", Title: "File change action", ProjectName: "Codex"}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-file-change-action",
		LatestTurnStatus: "interrupted",
		DetailItems: []model.DetailItem{
			{ID: "file-1", Kind: model.DetailItemTool, ToolKind: "fileChange", Label: "File changed: internal/feishu/card.go", Status: "completed", FP: "file-1"},
		},
	}

	service := newTestService(t)
	messages := service.renderSummaryPanelMarkdown(context.Background(), thread, snapshot, summaryPanelEntries(snapshot), nil)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	text := messages[0].Text
	if !strings.Contains(text, "Edited") || !strings.Contains(text, "internal/feishu/card.go") || strings.Contains(text, "File changed:") {
		t.Fatalf("summary text = %q, want edited file action without raw prefix", text)
	}
}

func TestSummaryPanelAddsInterruptedProgressStatusLog(t *testing.T) {
	t.Parallel()

	thread := model.Thread{ID: "thread-interrupted-progress", Title: "Interrupted progress", ProjectName: "Codex"}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:             thread,
		LatestTurnID:       "turn-interrupted-progress",
		LatestTurnStatus:   "interrupted",
		LatestProgressFP:   "progress-fp",
		LatestProgressText: "Updating files.",
	}

	service := newTestService(t)
	messages := service.renderSummaryPanelMarkdown(context.Background(), thread, snapshot, nil, nil)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	text := messages[0].Text
	if !strings.Contains(text, "Processing...") || !strings.Contains(messages[0].CodexStatus, "Processing") {
		t.Fatalf("summary text = %q, want processing status log and status", text)
	}
	for _, forbidden := range []string{"已中断", "interrupted"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("summary text contains %q: %q", forbidden, text)
		}
	}
}

func TestSummaryPanelRemovesNilLiteralBeforeRendering(t *testing.T) {
	t.Parallel()

	thread := model.Thread{
		ID:          "thread-nil-summary",
		Title:       "Nil summary",
		ProjectName: "Codex",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-nil-summary",
		LatestTurnStatus: "<nil>",
		LatestAgentMessageEntries: []appserver.AgentMessageEntry{
			{ID: "agent-nil", Phase: "<nil>", Text: "Before <nil> after"},
		},
	}

	service := newTestService(t)
	messages := service.renderSummaryPanelMarkdown(context.Background(), thread, snapshot, snapshot.LatestAgentMessageEntries, nil)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	if strings.Contains(messages[0].Text, "<nil>") {
		t.Fatalf("summary text leaked nil literal: %q", messages[0].Text)
	}
	if !strings.Contains(messages[0].Text, "Before  after") {
		t.Fatalf("summary text = %q, want sanitized agent text", messages[0].Text)
	}
}

func TestSyncThreadPanelDoesNotUseDocumentDeliveryForToolOutput(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-tool",
		Title:       "Tool output smoke",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	snapshot := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-tool",
		LatestTurnStatus: "completed",
		LatestToolID:     "tool-1",
		LatestToolKind:   "commandExecution",
		LatestToolLabel:  "pwsh -Command node -v",
		LatestToolStatus: "completed",
		LatestToolOutput: "v22.22.2\n",
		LatestToolFP:     "tool-fp-1",
		DetailItems: []model.DetailItem{
			{ID: "tool-1", Kind: model.DetailItemTool, ToolKind: "commandExecution", Label: "pwsh -Command node -v", Status: "completed", FP: "tool-fp-1"},
			{ID: "tool-1:output", Kind: model.DetailItemOutput, Output: "v22.22.2\n", FP: "tool-output-fp-1"},
		},
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	compact := appserver.CompactSnapshot(nil, snapshot, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, compact); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	service.syncThreadPanelToTarget(ctx, model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}, thread.ID, false, "explicit")

	if len(sender.documents) != 0 {
		t.Fatalf("documents = %#v, want no SendDocument path for tool output", sender.documents)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("message count = %d, want one Codex panel only", len(sender.messages))
	}
	if sender.messages[0].style != model.MessageStyleCodexPanel || !strings.Contains(sender.messages[0].text, "pwsh -Command node -v") {
		t.Fatalf("message = %#v, want tool details folded into Codex panel", sender.messages[0])
	}
}

func TestFeishuInputSyncShowsFeishuSourceAndDoesNotDuplicateUserRequestNotice(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-feishu-input",
		Title:       "Feishu prompt",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-feishu-input",
		LatestTurnStatus:      "inProgress",
		LatestUserMessageID:   "user-feishu",
		LatestUserMessageText: "This was already sent in Feishu.",
		LatestUserMessageFP:   "user-feishu-fp",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	if err := service.markInputOriginTurn(ctx, thread.ID, "turn-feishu-input", model.PanelSourceFeishuInput, 123456789, 0); err != nil {
		t.Fatalf("markInputOriginTurn failed: %v", err)
	}

	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, true, model.PanelSourceFeishuInput)

	if len(sender.messages) != 1 {
		t.Fatalf("message count = %d, want summary without New run or [User] duplicate; messages=%#v", len(sender.messages), sender.messages)
	}
	if strings.Contains(sender.messages[0].text, "New run:") || strings.Contains(sender.messages[0].text, "Source: Feishu") {
		t.Fatalf("first message = %q, want no Feishu New run notice", sender.messages[0].text)
	}
	if sender.messages[0].style != model.MessageStyleCodexPanel || strings.TrimSpace(sender.messages[0].codexStatus) == "" {
		t.Fatalf("first message = %#v, want Codex panel", sender.messages[0])
	}
	for _, message := range sender.messages {
		if hasHeaderKind(message.text, "User") {
			t.Fatalf("unexpected user notice for Feishu-originated input: %#v", sender.messages)
		}
	}
	panel, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if panel == nil || panel.SourceMode != model.PanelSourceFeishuInput || panel.LastUserNoticeFP != "" {
		t.Fatalf("panel = %#v, want feishu_input with empty user notice fp", panel)
	}
}

func TestFeishuTopicSyncSendsCodexDesktopUserNoticeAndFinalAfterFeishuTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-feishu-then-desktop",
		Title:       "Feishu then desktop",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}

	feishuSnapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-feishu",
		LatestTurnStatus:      "completed",
		LatestUserMessageID:   "user-feishu",
		LatestUserMessageText: "Already visible in Feishu.",
		LatestUserMessageFP:   "user-feishu-fp",
		LatestFinalText:       "Feishu turn done.",
		LatestFinalFP:         "final-feishu-fp",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, feishuSnapshot); err != nil {
		t.Fatalf("UpsertSnapshot feishu failed: %v", err)
	}
	if err := service.markInputOriginTurn(ctx, thread.ID, "turn-feishu", model.PanelSourceFeishuInput, target.ChatID, target.TopicID); err != nil {
		t.Fatalf("markInputOriginTurn failed: %v", err)
	}
	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)
	baseMessages := len(sender.messages)

	desktopProgress := appserver.CompactSnapshot(&feishuSnapshot, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-desktop",
		LatestTurnStatus:      "inProgress",
		LatestUserMessageID:   "user-desktop",
		LatestUserMessageText: "Desktop asks Feishu to sync this.",
		LatestUserMessageFP:   "user-desktop-fp",
		LatestProgressText:    "Thinking.",
		LatestProgressFP:      "progress-desktop-fp",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, desktopProgress); err != nil {
		t.Fatalf("UpsertSnapshot desktop progress failed: %v", err)
	}
	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)

	userNoticeFound := false
	for _, message := range sender.messages[baseMessages:] {
		if strings.Contains(message.text, "Desktop asks Feishu to sync this.") && message.style == model.MessageStyleDesktopUser {
			if hasHeaderKind(message.text, "User") || strings.Contains(message.text, "[Codex]") || strings.Contains(message.text, "[T:") {
				t.Fatalf("desktop user notice text = %q, want raw user input only", message.text)
			}
			userNoticeFound = true
			break
		}
	}
	if !userNoticeFound {
		t.Fatalf("desktop user notice not found in new messages: %#v", sender.messages[baseMessages:])
	}
	if got := sender.messages[baseMessages]; got.style != model.MessageStyleDesktopUser || !strings.Contains(got.text, "Desktop asks Feishu to sync this.") {
		t.Fatalf("first new message = %#v, want desktop user notice before progress", got)
	}
	if buttonToken(sender.messages[baseMessages].buttons, "Refresh") == "" {
		t.Fatalf("desktop user notice buttons = %#v, want Refresh", sender.messages[baseMessages].buttons)
	}
	panel, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if panel == nil || panel.LastUserNoticeFP != "user-desktop-fp" {
		t.Fatalf("panel = %#v, want desktop user notice fingerprint", panel)
	}

	desktopFinal := appserver.CompactSnapshot(&desktopProgress, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-desktop",
		LatestTurnStatus:      "completed",
		LatestUserMessageID:   "user-desktop",
		LatestUserMessageText: "Desktop asks Feishu to sync this.",
		LatestUserMessageFP:   "user-desktop-fp",
		LatestFinalText:       "Desktop turn done.",
		LatestFinalFP:         "final-desktop-fp",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, desktopFinal); err != nil {
		t.Fatalf("UpsertSnapshot desktop final failed: %v", err)
	}
	beforeFinalMessages := len(sender.messages)
	beforeFinalEdits := len(sender.edits)
	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(sender.messages) != beforeFinalMessages+1 {
		t.Fatalf("messages = %#v, want standalone final card", sender.messages[beforeFinalMessages:])
	}
	if len(sender.edits) == beforeFinalEdits {
		t.Fatalf("edits = %#v, want progress panel edited to terminal state", sender.edits)
	}
	finalCard := lastFinalMessage(t, sender.messages[beforeFinalMessages:])
	if !strings.Contains(finalCard.text, "Desktop turn done.") {
		t.Fatalf("final card = %#v, want final content", finalCard)
	}
	assertOnlyLatestDetailsButton(t, finalCard.buttons)
	panel, err = service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel after final failed: %v", err)
	}
	if panel == nil || panel.LastFinalNoticeFP != "final-desktop-fp" {
		t.Fatalf("panel = %#v, want desktop final fingerprint", panel)
	}
}

func TestFeishuTopicCodexPanelProgressThenFinalEffectGuard(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-codex-panel-effect-guard",
		Title:       "Panel effect guard",
		ProjectName: "Codex",
		CWD:         `/Users/you/Projects/Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}

	progressSnapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-panel-effect-guard",
		LatestTurnStatus: "inProgress",
		DetailItems: []model.DetailItem{
			{ID: "commentary-1", Kind: model.DetailItemCommentary, Text: "Inspecting Feishu card rendering.", FP: "commentary-1", CommentaryIndex: 1},
			{ID: "tool-1", Kind: model.DetailItemTool, ToolKind: "commandExecution", Label: "go test ./internal/feishu", Status: "completed", FP: "tool-1", CommentaryIndex: 1},
			{ID: "commentary-2", Kind: model.DetailItemCommentary, Text: "Checking final card update.", FP: "commentary-2", CommentaryIndex: 2},
		},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, progressSnapshot); err != nil {
		t.Fatalf("UpsertSnapshot progress failed: %v", err)
	}

	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(sender.messages) != 1 {
		t.Fatalf("messages = %#v, want one progress panel", sender.messages)
	}
	progressPanel := lastCodexPanelMessage(t, sender.messages)
	if !strings.Contains(progressPanel.codexStatus, "Processing") {
		t.Fatalf("progress panel status = %q, want running status", progressPanel.codexStatus)
	}
	for _, want := range []string{"Thinking...", "Inspecting Feishu card rendering.", "Ran", "go test ./internal/feishu", "Checking final card update."} {
		if !strings.Contains(progressPanel.codexProgressMarkdown, want) {
			t.Fatalf("progress markdown missing %q:\n%s", want, progressPanel.codexProgressMarkdown)
		}
	}
	for _, forbidden := range []string{"已中断", "interrupted"} {
		if strings.Contains(progressPanel.codexStatus, forbidden) || strings.Contains(progressPanel.codexProgressMarkdown, forbidden) {
			t.Fatalf("progress panel leaked %q: %#v", forbidden, progressPanel)
		}
	}
	if token := buttonToken(progressPanel.buttons, "Stop"); token == "" {
		t.Fatalf("progress buttons = %#v, want Stop", progressPanel.buttons)
	}
	for _, forbidden := range []string{"Show context", "查看上下文", "Steer", "引导"} {
		if token := buttonToken(progressPanel.buttons, forbidden); token != "" {
			t.Fatalf("progress buttons = %#v, want no %q", progressPanel.buttons, forbidden)
		}
	}

	finalSnapshot := appserver.CompactSnapshot(&progressSnapshot, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-panel-effect-guard",
		LatestTurnStatus: "completed",
		LatestFinalText:  "Final answer is ready.",
		LatestFinalFP:    "final-panel-effect-guard",
		DetailItems: []model.DetailItem{
			{ID: "commentary-1", Kind: model.DetailItemCommentary, Text: "Inspecting Feishu card rendering.", FP: "commentary-1", CommentaryIndex: 1},
			{ID: "tool-1", Kind: model.DetailItemTool, ToolKind: "commandExecution", Label: "go test ./internal/feishu", Status: "completed", FP: "tool-1", CommentaryIndex: 1},
			{ID: "commentary-2", Kind: model.DetailItemCommentary, Text: "Checking final card update.", FP: "commentary-2", CommentaryIndex: 2},
			{ID: "final-1", Kind: model.DetailItemFinal, Text: "Final answer is ready.", FP: "final-panel-effect-guard", CommentaryIndex: 2},
		},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, finalSnapshot); err != nil {
		t.Fatalf("UpsertSnapshot final failed: %v", err)
	}
	beforeFinalMessages := len(sender.messages)
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(sender.messages) != beforeFinalMessages+1 {
		t.Fatalf("messages after final = %#v, want standalone final card", sender.messages[beforeFinalMessages:])
	}
	finalEdit := lastCodexPanelEdit(t, sender.edits)
	if !strings.Contains(finalEdit.codexStatus, "Processed") || strings.Contains(finalEdit.codexStatus, "Processing") || finalEdit.codexFinalMarkdown != "" {
		t.Fatalf("final edit = %#v, want processed progress panel without final content", finalEdit)
	}
	assertOnlyLatestDetailsButton(t, finalEdit.buttons)
	finalCard := lastFinalMessage(t, sender.messages[beforeFinalMessages:])
	if !strings.Contains(finalCard.text, "Final answer is ready.") {
		t.Fatalf("final card = %#v, want final content", finalCard)
	}
	for _, forbidden := range []string{"已中断", "interrupted"} {
		if strings.Contains(finalEdit.codexStatus, forbidden) || strings.Contains(finalEdit.codexProgressMarkdown, forbidden) || strings.Contains(finalCard.text, forbidden) {
			t.Fatalf("final edit leaked %q: %#v", forbidden, finalEdit)
		}
	}
}

func TestFeishuTopicSyncBackfillsLateDesktopUserNotice(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-late-desktop-user",
		Title:       "Late desktop user",
		ProjectName: "Codex",
		CWD:         `/Users/you/Projects/Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}

	feishuSnapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-feishu",
		LatestTurnStatus:      "completed",
		LatestUserMessageID:   "user-feishu",
		LatestUserMessageText: "Already visible in Feishu.",
		LatestUserMessageFP:   "user-feishu-fp",
		LatestFinalText:       "Feishu turn done.",
		LatestFinalFP:         "final-feishu-fp",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, feishuSnapshot); err != nil {
		t.Fatalf("UpsertSnapshot feishu failed: %v", err)
	}
	if err := service.markInputOriginTurn(ctx, thread.ID, "turn-feishu", model.PanelSourceFeishuInput, target.ChatID, target.TopicID); err != nil {
		t.Fatalf("markInputOriginTurn failed: %v", err)
	}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)
	baseMessages := len(sender.messages)

	desktopProgressWithoutUser := appserver.CompactSnapshot(&feishuSnapshot, appserver.ThreadReadSnapshot{
		Thread:             thread,
		LatestTurnID:       "turn-desktop-late-user",
		LatestTurnStatus:   "inProgress",
		LatestProgressText: "Thinking before user item is available.",
		LatestProgressFP:   "progress-before-user-fp",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, desktopProgressWithoutUser); err != nil {
		t.Fatalf("UpsertSnapshot desktop progress failed: %v", err)
	}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)
	if len(sender.messages) == baseMessages {
		t.Fatalf("no progress panel was sent")
	}
	for _, message := range sender.messages[baseMessages:] {
		if message.style == model.MessageStyleDesktopUser {
			t.Fatalf("unexpected desktop user notice before user item exists: %#v", sender.messages[baseMessages:])
		}
	}
	panel, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if panel == nil || panel.SourceMode != model.PanelSourceFeishuInput || panel.LastUserNoticeFP != "" {
		t.Fatalf("panel before late user = %#v, want feishu_input without user notice fp", panel)
	}

	desktopProgressWithUser := appserver.CompactSnapshot(&desktopProgressWithoutUser, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-desktop-late-user",
		LatestTurnStatus:      "inProgress",
		LatestUserMessageID:   "user-desktop-late",
		LatestUserMessageText: "This user item arrived after the progress card.",
		LatestUserMessageFP:   "user-desktop-late-fp",
		LatestProgressText:    "Thinking after user item is available.",
		LatestProgressFP:      "progress-after-user-fp",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, desktopProgressWithUser); err != nil {
		t.Fatalf("UpsertSnapshot desktop late user failed: %v", err)
	}
	beforeBackfill := len(sender.messages)
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(sender.messages) == beforeBackfill {
		t.Fatalf("no message sent for late desktop user item")
	}
	userNoticeFound := false
	for _, message := range sender.messages[beforeBackfill:] {
		if strings.Contains(message.text, "This user item arrived after the progress card.") && message.style == model.MessageStyleDesktopUser {
			userNoticeFound = true
			break
		}
	}
	if !userNoticeFound {
		t.Fatalf("late desktop user notice not found in new messages: %#v", sender.messages[beforeBackfill:])
	}
	panel, err = service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel after late user failed: %v", err)
	}
	if panel == nil || panel.LastUserNoticeFP != "user-desktop-late-fp" || panel.UserMessageID == 0 {
		t.Fatalf("panel after late user = %#v, want stored user notice", panel)
	}
}

func TestFeishuTopicSyncDoesNotDuplicateExistingDesktopUserNoticeRoute(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-existing-user-route",
		Title:       "Existing user route",
		ProjectName: "Codex",
		CWD:         `/Users/you/Projects/Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           target.ChatID,
		TopicID:          target.TopicID,
		ProjectName:      thread.ProjectName,
		ThreadID:         thread.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 88,
		CurrentTurnID:    "turn-existing-route",
		Status:           "interrupted",
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    target.ChatID,
		TopicID:   target.TopicID,
		MessageID: 777,
		ThreadID:  thread.ID,
		TurnID:    "turn-existing-route",
		ItemID:    "user-existing-route",
		EventID:   "user-existing-route-fp",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-existing-route",
		LatestTurnStatus:      "interrupted",
		LatestUserMessageID:   "user-existing-route",
		LatestUserMessageText: "Already sent once.",
		LatestUserMessageFP:   "user-existing-route-fp",
		LatestProgressText:    "Still running.",
		LatestProgressFP:      "progress-existing-route",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	for _, message := range sender.messages {
		if message.style == model.MessageStyleDesktopUser {
			t.Fatalf("messages = %#v, want no duplicate desktop user notice", sender.messages)
		}
	}
	panel, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if panel == nil || panel.UserMessageID != 777 || panel.LastUserNoticeFP != "user-existing-route-fp" {
		t.Fatalf("panel = %#v, want adopted existing user notice route", panel)
	}
}

func TestFeishuTopicSyncDoesNotDuplicateDesktopUserNoticeWhenCreatingPanel(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-existing-route-create-panel",
		Title:       "Existing route creates panel",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    123456789,
		TopicID:   0,
		MessageID: 777,
		ThreadID:  thread.ID,
		TurnID:    "older-turn-id",
		ItemID:    "older-item-id",
		EventID:   "user-existing-route-create-panel-fp",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-existing-route-create-panel",
		LatestTurnStatus:      "inProgress",
		LatestUserMessageID:   "user-existing-route-create-panel",
		LatestUserMessageText: "Already sent before panel existed.",
		LatestUserMessageFP:   "user-existing-route-create-panel-fp",
		LatestProgressText:    "Still running.",
		LatestProgressFP:      "progress-existing-route-create-panel",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	for _, message := range sender.messages {
		if message.style == model.MessageStyleDesktopUser {
			t.Fatalf("messages = %#v, want no duplicate desktop user notice while creating panel", sender.messages)
		}
	}
	panel, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if panel == nil || panel.UserMessageID != 777 || panel.LastUserNoticeFP != "user-existing-route-create-panel-fp" {
		t.Fatalf("panel = %#v, want adopted existing user notice route while creating panel", panel)
	}
}

func TestRenderDesktopUserNoticeUsesDesktopStyleWithoutVisualHeader(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	thread := model.Thread{
		ID:          "thread-desktop-user-style",
		Title:       "Desktop user style",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-desktop-user-style",
		LatestUserMessageText: "Run `go test` now.",
		LatestUserMessageFP:   "user-style-fp",
	}

	messages := service.renderUserRequestNoticeCard(context.Background(), thread, snapshot, model.PanelSourceExplicit)

	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one", messages)
	}
	if messages[0].Style != model.MessageStyleDesktopUser {
		t.Fatalf("style = %q, want desktop user style", messages[0].Style)
	}
	if strings.Contains(messages[0].Text, "[Codex]") || strings.Contains(messages[0].Text, "[User]") || strings.Contains(messages[0].Text, "[T:") {
		t.Fatalf("message text = %q, want no visual header", messages[0].Text)
	}
	if !strings.Contains(messages[0].Text, "Run go test now.") {
		t.Fatalf("message text = %q, want rendered user input", messages[0].Text)
	}
}

func TestRenderDesktopUserNoticeHidesImageAttachmentPaths(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	thread := model.Thread{
		ID:          "thread-desktop-image-style",
		Title:       "Desktop image style",
		ProjectName: "Codex",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:       thread,
		LatestTurnID: "turn-desktop-image-style",
		LatestUserMessageText: "# Files mentioned by the user:\n\n" +
			"## codex-clipboard-a1250494.png: /tmp/codex-clipboard-a1250494.png\n\n" +
			"## My request for Codex:\n" +
			"这个在飞书上显示的还不对\n" +
			"[Image: /tmp/codex-clipboard-a1250494.png]",
		LatestUserMessageImagePath: "/tmp/codex-clipboard-a1250494.png",
		LatestUserMessageFP:        "user-image-style-fp",
	}

	messages := service.renderUserRequestNoticeCard(context.Background(), thread, snapshot, model.PanelSourceExplicit)

	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one", messages)
	}
	if messages[0].Style != model.MessageStyleDesktopUser {
		t.Fatalf("style = %q, want desktop user style", messages[0].Style)
	}
	if messages[0].ImagePath != snapshot.LatestUserMessageImagePath {
		t.Fatalf("image path = %q, want attached image path", messages[0].ImagePath)
	}
	for _, forbidden := range []string{"Files mentioned by the user", "My request for Codex", "codex-clipboard-a1250494.png:", "[Image:", "/tmp/codex-clipboard-a1250494.png"} {
		if strings.Contains(messages[0].Text, forbidden) {
			t.Fatalf("message text = %q, want no attachment metadata %q", messages[0].Text, forbidden)
		}
	}
	if !strings.Contains(messages[0].Text, "这个在飞书上显示的还不对") {
		t.Fatalf("message text = %q, want real user request", messages[0].Text)
	}
}

func TestRenderDesktopUserNoticeImageOnlySendsImageWithoutPathText(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	thread := model.Thread{ID: "thread-desktop-image-only", Title: "Desktop image only", ProjectName: "Codex"}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:                     thread,
		LatestTurnID:               "turn-desktop-image-only",
		LatestUserMessageText:      "[Image: /tmp/codex-clipboard.png]",
		LatestUserMessageImagePath: "/tmp/codex-clipboard.png",
		LatestUserMessageFP:        "user-image-only-fp",
	}

	messages := service.renderUserRequestNoticeCard(context.Background(), thread, snapshot, model.PanelSourceExplicit)

	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one image message", messages)
	}
	if strings.TrimSpace(messages[0].Text) != "" {
		t.Fatalf("message text = %q, want no image path text", messages[0].Text)
	}
	if messages[0].ImagePath != snapshot.LatestUserMessageImagePath {
		t.Fatalf("image path = %q, want attached image path", messages[0].ImagePath)
	}
	if messages[0].Style != model.MessageStyleDesktopUser {
		t.Fatalf("style = %q, want desktop user style", messages[0].Style)
	}
}

func TestRenderDesktopUserNoticeHidesImageBridgePrompt(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	thread := model.Thread{ID: "thread-desktop-image-bridge", Title: "Desktop image bridge", ProjectName: "Codex"}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:                     thread,
		LatestTurnID:               "turn-desktop-image-bridge",
		LatestUserMessageText:      "请读取并分析这张图片。\n[Image: /tmp/feishu-image.jpg]",
		LatestUserMessageImagePath: "/tmp/feishu-image.jpg",
		LatestUserMessageFP:        "user-image-bridge-fp",
	}

	messages := service.renderUserRequestNoticeCard(context.Background(), thread, snapshot, model.PanelSourceExplicit)

	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one image message", messages)
	}
	if strings.TrimSpace(messages[0].Text) != "" {
		t.Fatalf("message text = %q, want bridge prompt hidden", messages[0].Text)
	}
	if messages[0].ImagePath != snapshot.LatestUserMessageImagePath {
		t.Fatalf("image path = %q, want attached image path", messages[0].ImagePath)
	}
}

func TestFeishuThreadTopicSenderRoutesPanelMessagesIntoThread(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-feishu-topic",
		Title:       "Feishu topic",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-feishu-topic",
		LatestTurnStatus: "inProgress",
		LatestAgentMessageEntries: []appserver.AgentMessageEntry{{
			Text:  "Working on it.",
			Phase: "commentary",
		}},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}

	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(sender.messages) == 0 {
		t.Fatalf("messages = %#v, want panel messages in topic", sender.messages)
	}
	for _, message := range sender.messages {
		if message.options.FeishuReplyToMessageID != 9001 || !message.options.FeishuReplyInThread || message.options.FeishuCodexThreadID != thread.ID {
			t.Fatalf("message options = %#v, want Feishu topic reply options", message.options)
		}
	}
	route, err := service.store.ResolveMessageRoute(ctx, target.ChatID, target.TopicID, 9001)
	if err != nil {
		t.Fatalf("ResolveMessageRoute(root) failed: %v", err)
	}
	if route == nil || route.ThreadID != thread.ID {
		t.Fatalf("root route = %#v, want thread route", route)
	}
}

func TestFeishuThreadTopicRouteIsStoredForActualTopicChat(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{
		topic: &model.FeishuThreadTopic{
			ChatID:        987654321,
			ThreadID:      "thread-feishu-control-room",
			RootMessageID: 9002,
		},
	}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-feishu-control-room",
		Title:       "Feishu control room",
		ProjectName: "Codex",
		CWD:         t.TempDir(),
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-feishu-control-room",
		LatestTurnStatus: "inProgress",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}

	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)

	originalRoute, err := service.store.ResolveMessageRoute(ctx, target.ChatID, target.TopicID, 9002)
	if err != nil {
		t.Fatalf("ResolveMessageRoute(original) failed: %v", err)
	}
	if originalRoute == nil || originalRoute.ThreadID != thread.ID {
		t.Fatalf("original route = %#v, want thread route", originalRoute)
	}
	topicRoute, err := service.store.ResolveMessageRoute(ctx, 987654321, 0, 9002)
	if err != nil {
		t.Fatalf("ResolveMessageRoute(topic chat) failed: %v", err)
	}
	if topicRoute == nil || topicRoute.ThreadID != thread.ID {
		t.Fatalf("topic route = %#v, want thread route", topicRoute)
	}
}

func TestFeishuThreadTopicSyncUsesCanonicalTargetOnce(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{
		topic: &model.FeishuThreadTopic{
			ChatID:        2002,
			ThreadID:      "thread-feishu-canonical",
			RootMessageID: 9003,
		},
		canonicalByChatID: map[int64]int64{
			1001: 2002,
		},
	}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-feishu-canonical",
		Title:       "Feishu canonical",
		ProjectName: "Codex",
		CWD:         t.TempDir(),
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-feishu-canonical",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-feishu-canonical",
		LatestAgentMessageEntries: []appserver.AgentMessageEntry{{
			Text:  "Done.",
			Phase: "final",
		}},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:         1001,
		TopicID:        0,
		ThreadID:       thread.ID,
		ProjectName:    thread.ProjectName,
		SourceMode:     model.PanelSourceFeishuInput,
		ArchiveEnabled: true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel(dm) failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:         2002,
		TopicID:        0,
		ThreadID:       thread.ID,
		ProjectName:    thread.ProjectName,
		SourceMode:     model.PanelSourceFeishuInput,
		ArchiveEnabled: true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel(topic) failed: %v", err)
	}

	service.syncThreadPanel(ctx, thread.ID)

	if got := fmt.Sprint(sender.ensureTopicChatIDs); got != "[2002]" {
		t.Fatalf("EnsureThreadTopic chat IDs = %s, want [2002]", got)
	}
	if len(sender.messages) == 0 {
		t.Fatalf("messages = %#v, want one canonical panel sync", sender.messages)
	}
	for _, message := range sender.messages {
		if message.chatID != 2002 {
			t.Fatalf("message chatID = %d, want canonical topic chat 2002", message.chatID)
		}
	}
	panels, err := service.store.ListCurrentPanelsForThread(ctx, thread.ID)
	if err != nil {
		t.Fatalf("ListCurrentPanelsForThread failed: %v", err)
	}
	if len(panels) != 1 || panels[0].ChatID != 2002 {
		t.Fatalf("current panels = %#v, want only canonical topic panel", panels)
	}
}

func TestFeishuInputTopicSyncDoesNotRecreatePanelFromOriginalChat(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{
		topic: &model.FeishuThreadTopic{
			ChatID:        2002,
			ThreadID:      "thread-feishu-no-duplicate",
			RootMessageID: 9004,
		},
	}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-feishu-no-duplicate",
		Title:       "Feishu no duplicate",
		ProjectName: "Codex",
		CWD:         t.TempDir(),
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-feishu-no-duplicate",
		LatestTurnStatus: "inProgress",
		LatestAgentMessageEntries: []appserver.AgentMessageEntry{{
			Text:  "Working.",
			Phase: "commentary",
		}},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(1001, 0), ChatID: 1001, TopicID: 0, Enabled: true}

	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)
	firstMessageCount := len(sender.messages)
	if firstMessageCount == 0 {
		t.Fatalf("messages = %#v, want initial panel message", sender.messages)
	}

	service.syncThreadPanelToTarget(model.WithForcedThreadTopicActivation(ctx), target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(sender.messages) != firstMessageCount {
		t.Fatalf("message count after resync = %d, want %d; messages=%#v", len(sender.messages), firstMessageCount, sender.messages)
	}
	panels, err := service.store.ListCurrentPanelsForThread(ctx, thread.ID)
	if err != nil {
		t.Fatalf("ListCurrentPanelsForThread failed: %v", err)
	}
	if len(panels) != 1 || panels[0].ChatID != 2002 {
		t.Fatalf("current panels = %#v, want one canonical topic panel", panels)
	}
}

func TestSummaryPanelUsesAllDetailCommentaryEntries(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{ID: "thread-detail-commentary-summary", Title: "Detail commentary", ProjectName: "Codex"}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-detail-commentary-summary",
		LatestTurnStatus: "interrupted",
		LatestAgentMessageEntries: []appserver.AgentMessageEntry{
			{ID: "agent-4", Phase: "commentary", Text: "Fourth"},
			{ID: "agent-3", Phase: "commentary", Text: "Third"},
			{ID: "agent-2", Phase: "commentary", Text: "Second"},
		},
		DetailItems: []model.DetailItem{
			{ID: "agent-1", Kind: model.DetailItemCommentary, Phase: "commentary", Text: "First", FP: "agent-1", CommentaryIndex: 1},
			{ID: "agent-2", Kind: model.DetailItemCommentary, Phase: "commentary", Text: "Second", FP: "agent-2", CommentaryIndex: 2},
			{ID: "agent-3", Kind: model.DetailItemCommentary, Phase: "commentary", Text: "Third", FP: "agent-3", CommentaryIndex: 3},
			{ID: "agent-4", Kind: model.DetailItemCommentary, Phase: "commentary", Text: "Fourth", FP: "agent-4", CommentaryIndex: 4},
		},
	}

	message, _, _ := service.renderSummaryPanel(ctx, thread, snapshot, nil)

	for _, want := range []string{"First", "Second", "Third", "Fourth"} {
		if !strings.Contains(message.Text, want) {
			t.Fatalf("summary = %q, want %q from detail commentary", message.Text, want)
		}
	}
}

func TestFeishuInterruptedFinalAnswerRendersFinalCard(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{
		topic: &model.FeishuThreadTopic{
			ChatID:        2002,
			ThreadID:      "thread-feishu-interrupted-final",
			RootMessageID: 9005,
		},
	}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:           "thread-feishu-interrupted-final",
		Title:        "Feishu interrupted final",
		ProjectName:  "Codex",
		CWD:          t.TempDir(),
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: "turn-feishu-interrupted-final",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-feishu-interrupted-final",
		LatestTurnStatus: "interrupted",
		LatestFinalFP:    "final-feishu-interrupted-final",
		LatestFinalText:  "Done after interrupted status.",
		DetailItems: []model.DetailItem{
			{ID: "agent-1", Kind: model.DetailItemCommentary, Phase: "commentary", Text: "Working", FP: "agent-1", CommentaryIndex: 1},
			{ID: "final-1", Kind: model.DetailItemFinal, Phase: "final_answer", Text: "Done after interrupted status.", FP: "final-feishu-interrupted-final", CommentaryIndex: 1},
		},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	if err := service.markInputOriginTurn(ctx, thread.ID, "turn-feishu-interrupted-final", model.PanelSourceFeishuInput, 1001, 0); err != nil {
		t.Fatalf("markInputOriginTurn failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(1001, 0), ChatID: 1001, TopicID: 0, Enabled: true}

	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(finalMessages(sender.messages)) != 1 {
		t.Fatalf("final messages = %#v, want standalone interrupted final card", finalMessages(sender.messages))
	}
	progress := lastCodexPanelMessage(t, sender.messages)
	if progress.codexFinalMarkdown != "" {
		t.Fatalf("progress = %#v, want no interrupted final in Codex panel", progress)
	}
	final := lastFinalMessage(t, sender.messages)
	if !strings.Contains(final.text, "Done after interrupted status.") {
		t.Fatalf("final = %#v, want interrupted final card content", final)
	}
	for _, forbidden := range []string{"状态: 已中断", "Status: Interrupted", "状态: 运行中", "Status: Processing"} {
		if strings.Contains(final.text, forbidden) {
			t.Fatalf("final text contains %q: %q", forbidden, final.text)
		}
	}
	if !strings.Contains(final.text, "已处理") && !strings.Contains(final.text, "Processed") {
		t.Fatalf("final text = %q, want processed display status", final.text)
	}
	if strings.Contains(final.text, "已完成") || strings.Contains(final.text, "Completed") {
		t.Fatalf("final text = %q, want no raw completed status", final.text)
	}
}

func TestFeishuInterruptedCommentaryDoesNotRenderFinalCard(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingThreadTopicSender{
		topic: &model.FeishuThreadTopic{
			ChatID:        2002,
			ThreadID:      "thread-feishu-interrupted-commentary",
			RootMessageID: 9005,
		},
	}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:           "thread-feishu-interrupted-commentary",
		Title:        "Feishu interrupted commentary",
		ProjectName:  "Codex",
		CWD:          t.TempDir(),
		UpdatedAt:    time.Now().UTC().Unix(),
		Status:       "active",
		ActiveTurnID: "turn-feishu-interrupted-commentary",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-feishu-interrupted-commentary",
		LatestTurnStatus: "interrupted",
		DetailItems: []model.DetailItem{
			{ID: "agent-1", Kind: model.DetailItemCommentary, Phase: "commentary", Text: "Checking logs.", FP: "agent-1", CommentaryIndex: 1},
		},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	if err := service.markInputOriginTurn(ctx, thread.ID, "turn-feishu-interrupted-commentary", model.PanelSourceFeishuInput, 1001, 0); err != nil {
		t.Fatalf("markInputOriginTurn failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(1001, 0), ChatID: 1001, TopicID: 0, Enabled: true}

	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(finalMessages(sender.messages)) != 0 {
		t.Fatalf("final message count = %d, want 0; messages=%#v", len(finalMessages(sender.messages)), sender.messages)
	}
}

func TestMarkedChatOriginTurnDoesNotDuplicateUserRequestNoticeOnObserverResync(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-marked-chat-input",
		Title:       "Marked Chat prompt",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, "turn-marked-chat"); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      thread.ProjectName,
		ThreadID:         thread.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 101,
		ToolMessageID:    102,
		OutputMessageID:  103,
		CurrentTurnID:    "turn-old",
		Status:           "completed",
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-marked-chat",
		LatestTurnStatus:      "inProgress",
		LatestUserMessageID:   "user-marked-chat",
		LatestUserMessageText: "This was sent from chat and later re-polled.",
		LatestUserMessageFP:   "user-marked-chat-fp",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	for _, message := range sender.messages {
		if hasHeaderKind(message.text, "User") {
			t.Fatalf("unexpected user notice for marked chat-origin turn: %#v", sender.messages)
		}
	}
	panel, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if panel == nil || panel.LastUserNoticeFP != "" {
		t.Fatalf("panel = %#v, want no user notice fp for marked chat-origin turn", panel)
	}
}

func TestChatInputSyncAdoptsObserverPanelForSameTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-adopt-observer-panel",
		Title:       "chat race",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      thread.ProjectName,
		ThreadID:         thread.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 101,
		ToolMessageID:    102,
		OutputMessageID:  103,
		CurrentTurnID:    "turn-chat-race",
		Status:           "inProgress",
		ArchiveEnabled:   true,
		LastSummaryHash:  "old-summary",
		LastToolHash:     "old-tool",
		LastOutputHash:   "old-output",
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	if err := service.markChatOriginTurn(ctx, thread.ID, "turn-chat-race"); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-chat-race",
		LatestTurnStatus:      "completed",
		LatestUserMessageID:   "user-chat-race",
		LatestUserMessageText: "Проверка, ответь: Test",
		LatestUserMessageFP:   "user-chat-race-fp",
		LatestFinalText:       "Test",
		LatestFinalFP:         "final-chat-race-fp",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, true, model.PanelSourceFeishuInput)

	if len(finalMessages(sender.messages)) != 1 {
		t.Fatalf("messages = %#v, want standalone final card", sender.messages)
	}
	finalEdit := lastCodexPanelEdit(t, sender.edits)
	if finalEdit.codexFinalMarkdown != "" {
		t.Fatalf("final edit = %#v, want terminal progress panel without final", finalEdit)
	}
	assertOnlyLatestDetailsButton(t, finalEdit.buttons)
	finalCard := lastFinalMessage(t, sender.messages)
	if !strings.Contains(finalCard.text, "Test") {
		t.Fatalf("final card = %#v, want final content", finalCard)
	}
	if len(sender.deletes) != 2 || sender.deletes[0].messageID != 102 || sender.deletes[1].messageID != 103 {
		t.Fatalf("deletes = %#v, want old tool/output messages deleted", sender.deletes)
	}
	panel, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if panel == nil || panel.SummaryMessageID != 101 || panel.SourceMode != model.PanelSourceFeishuInput || panel.LastFinalNoticeFP != "final-chat-race-fp" || panel.LastFinalCardHash == "" {
		t.Fatalf("panel = %#v, want adopted panel retaining summary id 101 with final fp/hash", panel)
	}
}

func TestRenderToolPanelUnwrapsQuotedPowerShellCommand(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	text, _ := service.renderToolPanel(context.Background(), model.Thread{
		ID:          "thread-tool-pwsh",
		Title:       "Find Swagger",
		ProjectName: "Codex",
	}, &appserver.ThreadReadSnapshot{
		LatestTurnID:     "turn-tool-pwsh",
		LatestToolLabel:  `"C:\Program Files\PowerShell\7\pwsh.exe" -Command "rg -n 'session/33655d2a' 'C:\Users\you\Downloads\stellar_ws.txt'"`,
		LatestToolStatus: "completed",
	})

	if !strings.Contains(text, "[Shell:pwsh.exe (PowerShell)]") {
		t.Fatalf("rendered tool = %q, want shell metadata line", text)
	}
	if strings.Contains(text, "C:\\Program Files\\PowerShell") {
		t.Fatalf("rendered tool = %q, want wrapper shell path omitted", text)
	}
	if !strings.Contains(text, `<pre><code class="language-powershell">rg -n &#39;session/33655d2a&#39; &#39;C:\Users\you\Downloads\stellar_ws.txt&#39;</code></pre>`) {
		t.Fatalf("rendered tool = %q, want inner command in powershell code block", text)
	}
}

func TestRenderToolPanelMarksUnknownShell(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	text, _ := service.renderToolPanel(context.Background(), model.Thread{
		ID:          "thread-tool-unknown",
		Title:       "Unknown shell",
		ProjectName: "Codex",
	}, &appserver.ThreadReadSnapshot{
		LatestTurnID:     "turn-tool-unknown",
		LatestToolLabel:  `hui.exe -Command "echo 1"`,
		LatestToolStatus: "completed",
	})

	if !strings.Contains(text, "[Shell:hui.exe (⚠️UNKNOWN SHELL⚠️)]") {
		t.Fatalf("rendered tool = %q, want unknown shell marker", text)
	}
	if !strings.Contains(text, "<pre><code>echo 1</code></pre>") {
		t.Fatalf("rendered tool = %q, want command in generic code block", text)
	}
}

func TestRenderToolPanelShowsLastCompletedToolInsteadOfRunningTool(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	now := time.Date(2026, time.May, 1, 23, 19, 21, 0, time.UTC)
	text, _ := service.renderToolPanelAt(context.Background(), model.Thread{
		ID:          "thread-tool-running",
		Title:       "Long command",
		ProjectName: "Codex",
	}, &appserver.ThreadReadSnapshot{
		LatestTurnID:     "turn-tool-running",
		LatestTurnStatus: "inProgress",
		LatestToolLabel:  `sleep 20; printf 'slow-command-done\n'`,
		LatestToolStatus: "running",
		DetailItems: []model.DetailItem{
			{
				ID:     "tool-prev",
				Kind:   model.DetailItemTool,
				Label:  `printf 'previous\n'`,
				Status: "completed",
			},
			{
				ID:     "tool-prev:output",
				Kind:   model.DetailItemOutput,
				Output: "previous\n",
			},
		},
	}, now)

	if !strings.Contains(text, "Last completed tool:") {
		t.Fatalf("rendered tool = %q, want last completed label", text)
	}
	if !strings.Contains(text, "previous") || !strings.Contains(text, "Status: completed") {
		t.Fatalf("rendered tool = %q, want previous completed command", text)
	}
	if strings.Contains(text, "slow-command-done") || strings.Contains(text, "Status: running") {
		t.Fatalf("rendered tool = %q, want running command hidden from Tool panel", text)
	}
	if strings.Contains(text, "Running for:") || strings.Contains(text, "Run active for:") {
		t.Fatalf("rendered tool = %q, want run timing outside Tool panel", text)
	}
}

func TestRenderToolPanelShowsChatOriginCurrentTool(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "thread-chat-current-tool",
		Title:       "Long command",
		ProjectName: "Codex",
	}
	turnID := "turn-chat-current-tool"
	if err := service.markChatOriginTurn(ctx, thread.ID, turnID); err != nil {
		t.Fatalf("markChatOriginTurn failed: %v", err)
	}
	text, _ := service.renderToolPanelAt(ctx, thread, &appserver.ThreadReadSnapshot{
		LatestTurnID:          turnID,
		LatestTurnStatus:      "inProgress",
		LatestToolLabel:       `sleep 20; printf 'slow-command-done\n'`,
		LatestToolStatus:      "running",
		LatestToolLiveCurrent: true,
		DetailItems: []model.DetailItem{
			{
				ID:     "tool-prev",
				Kind:   model.DetailItemTool,
				Label:  `printf 'previous\n'`,
				Status: "completed",
			},
			{
				ID:     "tool-prev:output",
				Kind:   model.DetailItemOutput,
				Output: "previous\n",
			},
		},
	}, time.Date(2026, time.May, 1, 23, 19, 21, 0, time.UTC))

	if !strings.Contains(text, "Current tool:") {
		t.Fatalf("rendered tool = %q, want current tool heading", text)
	}
	if !strings.Contains(text, "slow-command-done") || !strings.Contains(text, "Status: running") {
		t.Fatalf("rendered tool = %q, want running current command", text)
	}
	if strings.Contains(text, "Last completed tool:") || strings.Contains(text, "previous") {
		t.Fatalf("rendered tool = %q, want current command to take precedence", text)
	}
}

func TestRenderToolPanelKeepsForeignRunningToolHidden(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "thread-foreign-running-tool",
		Title:       "Foreign command",
		ProjectName: "Codex",
	}
	text, _ := service.renderToolPanelAt(ctx, thread, &appserver.ThreadReadSnapshot{
		LatestTurnID:          "turn-foreign-running-tool",
		LatestTurnStatus:      "inProgress",
		LatestToolLabel:       `sleep 20; printf 'slow-command-done\n'`,
		LatestToolStatus:      "running",
		LatestToolLiveCurrent: true,
		DetailItems: []model.DetailItem{
			{
				ID:     "tool-prev",
				Kind:   model.DetailItemTool,
				Label:  `printf 'previous\n'`,
				Status: "completed",
			},
		},
	}, time.Date(2026, time.May, 1, 23, 19, 21, 0, time.UTC))

	if !strings.Contains(text, "Last completed tool:") || !strings.Contains(text, "previous") {
		t.Fatalf("rendered tool = %q, want last completed foreign command", text)
	}
	if strings.Contains(text, "Current tool:") || strings.Contains(text, "slow-command-done") || strings.Contains(text, "Status: running") {
		t.Fatalf("rendered tool = %q, want foreign running command hidden", text)
	}
}

func TestRenderSummaryPanelShowsActiveRunElapsedTimeAtBottom(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	now := time.Date(2026, time.May, 1, 23, 19, 21, 0, time.UTC)
	thread := model.Thread{
		ID:          "thread-active-no-tool",
		Title:       "Waiting command",
		ProjectName: "Codex",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:              thread,
		LatestTurnID:        "turn-active-no-tool",
		LatestTurnStatus:    "inProgress",
		LatestTurnStartedAt: "2026-05-01T23:18:51Z",
	}
	messages := service.renderSummaryPanelMarkdownAt(context.Background(), thread, snapshot, nil, nil, now)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	text := messages[0].Text

	if !strings.Contains(text, "No agent messages yet.") {
		t.Fatalf("rendered summary = %q, want empty commentary state", text)
	}
	if !strings.Contains(messages[0].CodexStatus, "Processing 30s") {
		t.Fatalf("codex status = %q, want active run elapsed time", messages[0].CodexStatus)
	}
	if !strings.Contains(messages[0].CodexStatus, "Refreshed:") && !strings.Contains(messages[0].CodexStatus, "最后刷新:") {
		t.Fatalf("codex status = %q, want refresh time", messages[0].CodexStatus)
	}
	if strings.Contains(text, "Run active for:") {
		t.Fatalf("rendered summary = %q, want no run timing footer in progress body", text)
	}
}

func TestRenderSummaryPanelShowsContextCompactionDivider(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	thread := model.Thread{
		ID:          "thread-context-compaction",
		Title:       "Context compaction",
		ProjectName: "Codex",
	}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-context-compaction",
		LatestTurnStatus: "inProgress",
		DetailItems: []model.DetailItem{
			{Kind: model.DetailItemCommentary, Text: "Before compaction.", FP: "before-fp"},
			{Kind: model.DetailItemCompaction, Text: "Compacted earlier context.", FP: "compact-fp"},
			{Kind: model.DetailItemCommentary, Text: "After compaction.", FP: "after-fp"},
		},
	}

	messages := service.renderSummaryPanelMarkdownAt(context.Background(), thread, snapshot, nil, nil, time.Now().UTC())
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	text := messages[0].Text
	if !strings.Contains(text, "---------- 上下文已自动压缩 ----------") && !strings.Contains(text, "---------- Context compacted ----------") {
		t.Fatalf("rendered summary = %q, want context compaction divider", text)
	}
	if !strings.Contains(text, "Before compaction.") || !strings.Contains(text, "After compaction.") {
		t.Fatalf("rendered summary = %q, want logs around compaction", text)
	}
}

func TestRenderOutputPanelEscapesHTMLInsideCodeBlock(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	thread := model.Thread{ID: "thread-output-html", Title: "Output HTML", ProjectName: "Codex"}
	text, _ := service.renderOutputPanel(context.Background(), thread, &appserver.ThreadReadSnapshot{
		LatestTurnID:     "turn-output-html",
		LatestToolLabel:  "printf html",
		LatestToolStatus: "completed",
		LatestToolOutput: "<tag>value & more</tag>\n",
	})
	if !hasHeaderKind(text, "Output") || !strings.Contains(text, "\n<pre><code>") {
		t.Fatalf("rendered output = %q, want plain header before HTML code block", text)
	}
	if strings.Contains(text, "[T:") || strings.Contains(text, "[R:") {
		t.Fatalf("rendered output = %q, want no thread or run identity chip", text)
	}
	if strings.Contains(text, "<tag>value") {
		t.Fatalf("rendered output = %q, want escaped html", text)
	}
	if !strings.Contains(text, "&lt;tag&gt;value &amp; more&lt;/tag&gt;") {
		t.Fatalf("rendered output = %q, want escaped payload", text)
	}
}

func TestRenderOutputPanelFitsAfterHTMLEscaping(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	thread := model.Thread{ID: "thread-output-fit", Title: "Output fit", ProjectName: "Codex"}
	text, _ := service.renderOutputPanel(context.Background(), thread, &appserver.ThreadReadSnapshot{
		LatestTurnID:     "turn-output-fit",
		LatestToolLabel:  "printf fit",
		LatestToolStatus: "completed",
		LatestToolOutput: strings.Repeat("<tag>&", 2000),
	})
	if len(text) > outputMessageLimit {
		t.Fatalf("rendered output length = %d, want <= %d", len(text), outputMessageLimit)
	}
	if !hasHeaderKind(text, "Output") || !strings.Contains(text, "\n<pre><code>") {
		t.Fatalf("rendered output = %q, want plain header before HTML code block", text)
	}
	if strings.Contains(text, "[T:") || strings.Contains(text, "[R:") {
		t.Fatalf("rendered output = %q, want no thread or run identity chip", text)
	}
}

func TestLegacyTerminalPanelSeedsFinalFingerprintWithoutResending(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-legacy-terminal",
		Title:       "Legacy terminal",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}

	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-legacy",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-fp-legacy",
		LatestFinalText:  "Legacy final text.",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	panel, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           target.ChatID,
		TopicID:          target.TopicID,
		ProjectName:      thread.ProjectName,
		ThreadID:         thread.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 11,
		ToolMessageID:    12,
		OutputMessageID:  13,
		CurrentTurnID:    "turn-legacy",
		Status:           "completed",
		ArchiveEnabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}

	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	finalCard := lastFinalMessage(t, sender.messages)
	if !strings.Contains(finalCard.text, "Legacy final text.") {
		t.Fatalf("final card = %#v, want legacy final text", finalCard)
	}
	refreshed, err := service.store.GetThreadPanelByID(ctx, panel.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID failed: %v", err)
	}
	if refreshed == nil || refreshed.LastFinalNoticeFP != "final-fp-legacy" || refreshed.LastFinalCardHash == "" {
		t.Fatalf("panel = %#v, want final-fp-legacy with final card hash", refreshed)
	}
}

func TestInterruptedPanelSendsFinalWhenSameTurnCompletes(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-interrupted-then-completed",
		Title:       "Interrupted then completed",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           target.ChatID,
		TopicID:          target.TopicID,
		ProjectName:      thread.ProjectName,
		ThreadID:         thread.ID,
		SourceMode:       model.PanelSourceFeishuInput,
		SummaryMessageID: 11,
		ToolMessageID:    12,
		OutputMessageID:  13,
		CurrentTurnID:    "turn-interrupted",
		Status:           "interrupted",
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}

	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-interrupted",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-after-interrupted",
		LatestFinalText:  "Recovered final.",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	finalEdit := lastCodexPanelEdit(t, sender.edits)
	if finalEdit.codexFinalMarkdown != "" {
		t.Fatalf("final edit = %#v, want terminal progress panel without final and no running buttons", finalEdit)
	}
	assertOnlyLatestDetailsButton(t, finalEdit.buttons)
	finalCard := lastFinalMessage(t, sender.messages)
	if !strings.Contains(finalCard.text, "Recovered final.") {
		t.Fatalf("final card = %#v, want recovered final", finalCard)
	}
	refreshed, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if refreshed == nil || refreshed.LastFinalNoticeFP != "final-after-interrupted" || refreshed.Status != "completed" {
		t.Fatalf("panel = %#v, want completed with final fp", refreshed)
	}
}

func TestTerminalPanelRepairsMissingFinalCardWhenFinalNoticeWasSeeded(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-missing-final-card",
		Title:       "Missing final card",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:            target.ChatID,
		TopicID:           target.TopicID,
		ProjectName:       thread.ProjectName,
		ThreadID:          thread.ID,
		SourceMode:        model.PanelSourceFeishuInput,
		SummaryMessageID:  11,
		CurrentTurnID:     "turn-missing-final-card",
		Status:            "completed",
		ArchiveEnabled:    true,
		LastFinalNoticeFP: "final-missing-card",
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-missing-final-card",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-missing-card",
		LatestFinalText:  "Final card should be repaired.",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	finalCard := lastFinalMessage(t, sender.messages)
	if !strings.Contains(finalCard.text, "Final card should be repaired.") {
		t.Fatalf("final card = %#v, want repaired final", finalCard)
	}
	refreshed, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if refreshed == nil || refreshed.LastFinalCardHash == "" || refreshed.LastFinalNoticeFP != "final-missing-card" {
		t.Fatalf("panel = %#v, want repaired final card state", refreshed)
	}
}

func TestNewTerminalTurnAfterExistingPanelStillSendsFinal(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-new-terminal-final",
		Title:       "Fresh terminal after history",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:            target.ChatID,
		TopicID:           target.TopicID,
		ProjectName:       thread.ProjectName,
		ThreadID:          thread.ID,
		SourceMode:        model.PanelSourceFeishuInput,
		SummaryMessageID:  11,
		ToolMessageID:     12,
		OutputMessageID:   13,
		CurrentTurnID:     "turn-old",
		Status:            "completed",
		ArchiveEnabled:    true,
		LastFinalNoticeFP: "final-old",
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}

	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread: model.Thread{
			ID:          thread.ID,
			Title:       thread.Title,
			ProjectName: thread.ProjectName,
			CWD:         thread.CWD,
			UpdatedAt:   thread.UpdatedAt,
		},
		LatestTurnID:     "turn-new",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-new",
		LatestFinalText:  "NEW FINAL",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(finalMessages(sender.messages)) != 1 {
		t.Fatalf("final messages = %#v, want standalone final card", finalMessages(sender.messages))
	}
	finalCard := lastFinalMessage(t, sender.messages)
	if !strings.Contains(finalCard.text, "NEW FINAL") {
		t.Fatalf("final card = %#v, want new final", finalCard)
	}
}

func TestSyncRepairsPreviousTerminalPanelBeforeCreatingNextPanel(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}

	threadPayload := map[string]any{
		"id":     "thread-repair-previous-final",
		"name":   "Repair previous final",
		"cwd":    "/Users/example/project",
		"status": "completed",
		"turns": []any{
			map[string]any{
				"id":     "turn-old",
				"status": "completed",
				"items": []any{
					map[string]any{"id": "old-user", "type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "old prompt"}}},
					map[string]any{"id": "old-final", "type": "agentMessage", "phase": "final_answer", "text": "OLD FINAL"},
				},
			},
			map[string]any{
				"id":     "turn-new",
				"status": "inProgress",
				"items": []any{
					map[string]any{"id": "new-user", "type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "new prompt"}}},
					map[string]any{"id": "new-commentary", "type": "agentMessage", "phase": "commentary", "text": "Working."},
				},
			},
		},
	}
	latest := appserver.SnapshotFromThreadRead(threadPayload)
	latest.Thread.Raw = json.RawMessage(model.MustJSON(map[string]any{"thread": threadPayload}))
	if err := service.store.UpsertThread(ctx, latest.Thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertSnapshot(ctx, latest.Thread.ID, appserver.CompactSnapshot(nil, latest, time.Now().UTC())); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	oldPanel, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:            target.ChatID,
		TopicID:           target.TopicID,
		ProjectName:       latest.Thread.ProjectName,
		ThreadID:          latest.Thread.ID,
		SourceMode:        model.PanelSourceFeishuInput,
		SummaryMessageID:  11,
		CurrentTurnID:     "turn-old",
		Status:            "completed",
		ArchiveEnabled:    true,
		LastFinalNoticeFP: "old-final-fp-without-card",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}

	service.syncThreadPanelToTarget(ctx, target, latest.Thread.ID, false, model.PanelSourceFeishuInput)

	finalCard := lastFinalMessage(t, sender.messages)
	if !strings.Contains(finalCard.text, "OLD FINAL") {
		t.Fatalf("final card = %#v, want old final before new panel", finalCard)
	}
	route, err := service.store.FindMessageRouteByEvent(ctx, latest.Thread.ID, "turn-old", "", finalCardEventIDForTest(latest.Thread.Raw, "turn-old"))
	if err != nil {
		t.Fatalf("FindMessageRouteByEvent failed: %v", err)
	}
	if route == nil || route.MessageID == 0 {
		t.Fatalf("old final route = %#v, want recorded final route", route)
	}
	refreshedOld, err := service.store.GetThreadPanelByID(ctx, oldPanel.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID failed: %v", err)
	}
	if refreshedOld == nil || refreshedOld.LastFinalCardHash == "" {
		t.Fatalf("old panel = %#v, want final card hash", refreshedOld)
	}
	current, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, latest.Thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if current == nil || current.CurrentTurnID != "turn-new" {
		t.Fatalf("current panel = %#v, want new turn panel", current)
	}
}

func TestFinalCardUploadsLocalMarkdownImage(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := context.Background()
	thread := model.Thread{ID: "thread-final-image", Title: "Final image", ProjectName: "Codex"}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-final-image",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-image-fp",
		LatestFinalText:  "这是当前桌面截图：\n\n![当前 Codex 界面](/tmp/codex-screenshots/current-codex-ui.png)",
	}

	message, _, _ := service.renderFinalCard(ctx, 42, thread, snapshot)

	if message.ImagePath != "/tmp/codex-screenshots/current-codex-ui.png" {
		t.Fatalf("image path = %q, want local screenshot path", message.ImagePath)
	}
	if strings.Contains(message.Text, "![") || strings.Contains(message.Text, "/tmp/codex-screenshots/current-codex-ui.png") {
		t.Fatalf("final text = %q, want markdown image stripped", message.Text)
	}
	if !strings.Contains(message.Text, "这是当前桌面截图：") {
		t.Fatalf("final text = %q, want caption preserved", message.Text)
	}
}

func finalCardEventIDForTest(raw json.RawMessage, turnID string) string {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	snapshot := appserver.SnapshotFromThreadRead(payload)
	panel := &model.ThreadPanel{CurrentTurnID: turnID}
	if turnSnapshot, ok := snapshotForPanelTurn(snapshot.Thread, &snapshot, panel); ok && turnSnapshot != nil {
		return turnSnapshot.LatestFinalFP
	}
	return ""
}

func TestLatestDetailsCallbackRepairsLatestTurnFromOldCard(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "thread-latest-details-old-card",
		Title:       "Latest details repair",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	panel, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:            123456789,
		TopicID:           0,
		ProjectName:       thread.ProjectName,
		ThreadID:          thread.ID,
		SourceMode:        model.PanelSourceFeishuInput,
		SummaryMessageID:  101,
		CurrentTurnID:     "turn-old",
		Status:            "completed",
		LastSummaryHash:   "old-summary",
		LastFinalNoticeFP: "old-final-fp",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    panel.ChatID,
		TopicID:   panel.TopicID,
		MessageID: panel.SummaryMessageID,
		ThreadID:  thread.ID,
		TurnID:    "turn-old",
		EventID:   "old-summary-route",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatalf("PutMessageRoute failed: %v", err)
	}
	if err := service.store.UpsertSnapshot(ctx, thread.ID, appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:                thread,
		LatestTurnID:          "turn-new",
		LatestTurnStatus:      "completed",
		LatestUserMessageID:   "user-new",
		LatestUserMessageText: "Latest user prompt",
		LatestUserMessageFP:   "user-new-fp",
		LatestFinalText:       "LATEST FINAL",
		LatestFinalFP:         "new-final-fp",
	}, time.Now().UTC())); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	oldToken := service.callbackButton(ctx, "Refresh", "latest_details", thread.ID, "turn-old", "", nil).CallbackData
	response, err := service.HandleCallbackFromSource(ctx, panel.ChatID, panel.TopicID, panel.SummaryMessageID, panel.ChatID, oldToken, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallbackFromSource failed: %v", err)
	}
	if response == nil || !strings.Contains(response.CallbackText, "Latest status synced") {
		t.Fatalf("response = %#v, want latest repair callback", response)
	}
	final := lastFinalMessage(t, sender.messages)
	if !strings.Contains(final.text, "LATEST FINAL") || strings.Contains(final.text, "old") {
		t.Fatalf("final = %#v, want latest turn final only", final)
	}
	refreshed, err := service.store.GetCurrentThreadPanel(ctx, panel.ChatID, panel.TopicID, thread.ID)
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if refreshed == nil || refreshed.CurrentTurnID != "turn-new" || refreshed.LastFinalNoticeFP != "new-final-fp" {
		t.Fatalf("panel = %#v, want repaired latest turn", refreshed)
	}
}

func TestFinalCardDetailsCallbacksEditSameMessageAndExportToolsFile(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-details",
		Title:       "Details flow",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-details",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-details",
		LatestFinalText:  "Done.",
		DetailItems: []model.DetailItem{
			{ID: "c1", Kind: model.DetailItemCommentary, Text: "First.", CommentaryIndex: 1},
			{ID: "c2", Kind: model.DetailItemCommentary, Text: "Second with `code`.", CommentaryIndex: 2},
			{ID: "t2", Kind: model.DetailItemTool, Label: "pwsh -Command rg test", Status: "completed", CommentaryIndex: 2},
			{ID: "o2", Kind: model.DetailItemOutput, Output: "match\n", CommentaryIndex: 2},
			{ID: "c3", Kind: model.DetailItemCommentary, Text: "Third.", CommentaryIndex: 3},
			{ID: "c4", Kind: model.DetailItemCommentary, Text: "Fourth.", CommentaryIndex: 4},
			{ID: "c5", Kind: model.DetailItemCommentary, Text: "Fifth.", CommentaryIndex: 5},
			{ID: "f1", Kind: model.DetailItemFinal, Text: "Done.", CommentaryIndex: 5},
		},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, "explicit")
	finalCard := lastCodexPanelMessage(t, sender.messages)
	cardMessageID := finalCard.messageID
	panel, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil || panel == nil {
		t.Fatalf("GetCurrentThreadPanel failed: panel=%#v err=%v", panel, err)
	}
	detailsToken := service.callbackButton(ctx, "Details", "details_open", thread.ID, "turn-details", "", map[string]any{
		"panel_id": panel.ID,
		"page":     0,
	}).CallbackData

	if _, err := service.HandleCallbackFromSource(ctx, target.ChatID, target.TopicID, cardMessageID, 123456789, detailsToken, model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("HandleCallback(details) failed: %v", err)
	}
	if len(sender.edits) < 1 {
		t.Fatalf("edits = %#v, want details edit", sender.edits)
	}
	details := sender.edits[len(sender.edits)-1]
	if details.messageID != cardMessageID {
		t.Fatalf("details message id = %d, want same %d", details.messageID, cardMessageID)
	}
	if !strings.Contains(details.text, "[commentary 1]") || !strings.Contains(details.text, "[commentary 4]") || strings.Contains(details.text, "Fifth.") {
		t.Fatalf("details page text = %q, want first four commentary entries only", details.text)
	}

	nextToken := buttonToken(details.buttons, ">")
	if _, err := service.HandleCallbackFromSource(ctx, target.ChatID, target.TopicID, cardMessageID, 123456789, nextToken, model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("HandleCallback(next) failed: %v", err)
	}
	next := sender.edits[len(sender.edits)-1]
	if !strings.Contains(next.text, "[commentary 5]") || !strings.Contains(next.text, "Fifth.") {
		t.Fatalf("next details text = %q, want fifth commentary", next.text)
	}

	toolOnToken := buttonToken(details.buttons, "Tool on")
	if _, err := service.HandleCallbackFromSource(ctx, target.ChatID, target.TopicID, cardMessageID, 123456789, toolOnToken, model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("HandleCallback(tool on) failed: %v", err)
	}
	toolMode := sender.edits[len(sender.edits)-1]
	if !strings.Contains(toolMode.text, "[Tool]") || !strings.Contains(toolMode.text, "[Output]") {
		t.Fatalf("tool mode text = %q, want related tool/output", toolMode.text)
	}

	toolNextToken := buttonToken(toolMode.buttons, ">")
	if _, err := service.HandleCallbackFromSource(ctx, target.ChatID, target.TopicID, cardMessageID, 123456789, toolNextToken, model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("HandleCallback(tool next) failed: %v", err)
	}
	toolNext := sender.edits[len(sender.edits)-1]
	if !strings.Contains(toolNext.text, "[commentary 3]") || strings.Contains(toolNext.text, "[commentary 4]") {
		t.Fatalf("tool next text = %q, want exactly next commentary without skipping", toolNext.text)
	}

	fileToken := buttonToken(toolMode.buttons, "Tools file")
	if _, err := service.HandleCallbackFromSource(ctx, target.ChatID, target.TopicID, cardMessageID, 123456789, fileToken, model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("HandleCallback(tools file) failed: %v", err)
	}
	if len(sender.documents) != 1 {
		t.Fatalf("documents = %#v, want one tools file", sender.documents)
	}
	if !sender.documents[0].options.Silent {
		t.Fatalf("document options = %#v, want silent explicit export", sender.documents[0].options)
	}
	if sender.documents[0].filePath != "" {
		t.Fatalf("tools file used path %q, want in-memory document data", sender.documents[0].filePath)
	}
	if !strings.Contains(string(sender.documents[0].data), "[Details tools]") || !strings.Contains(string(sender.documents[0].data), "[Tool]") {
		t.Fatalf("tools file data = %q, want details tool content", string(sender.documents[0].data))
	}

	backToken := buttonToken(toolMode.buttons, "Back")
	if _, err := service.HandleCallbackFromSource(ctx, target.ChatID, target.TopicID, cardMessageID, 123456789, backToken, model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("HandleCallback(back) failed: %v", err)
	}
	back := sender.edits[len(sender.edits)-1]
	if !hasHeaderKind(back.text, "Final") {
		t.Fatalf("back text = %q, want final card", back.text)
	}
}

func TestFeishuTopicCardsHideShowAndThreadIDButtons(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	ctx := withCompactFeishuTopicCard(context.Background())
	thread := model.Thread{ID: "thread-feishu-compact", Title: "Feishu compact", ProjectName: "Codex"}
	snapshot := &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-feishu-compact",
		LatestTurnStatus: "inProgress",
	}

	_, summaryButtons, _ := service.renderSummaryPanel(ctx, thread, snapshot, nil)
	for _, forbidden := range []string{"Show", "Get thread id", "Show context", "查看上下文"} {
		if buttonToken(summaryButtons, forbidden) != "" {
			t.Fatalf("summary buttons = %#v, want no %q in Feishu topic card", summaryButtons, forbidden)
		}
	}

	finalSnapshot := *snapshot
	finalSnapshot.LatestTurnStatus = "completed"
	finalSnapshot.LatestFinalText = "Done."
	finalSnapshot.LatestFinalFP = "final-feishu-compact"
	_, finalButtons, _ := service.renderFinalCard(ctx, 42, thread, &finalSnapshot)
	if buttonToken(finalButtons, "Show") != "" || buttonToken(finalButtons, "Get thread id") != "" {
		t.Fatalf("final buttons = %#v, want no Show or Get thread id in Feishu topic card", finalButtons)
	}
	if buttonToken(finalButtons, "Details") != "" || buttonToken(finalButtons, "Get full log") != "" {
		t.Fatalf("final buttons = %#v, want Details and Get full log hidden", finalButtons)
	}
}

func TestFinalCardDetailsShowsToolOnlyTurnWithoutCommentary(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-tool-only-details",
		Title:       "Tool only details",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:             thread,
		LatestTurnID:       "turn-tool-only-details",
		LatestTurnStatus:   "completed",
		LatestFinalFP:      "final-tool-only",
		LatestFinalText:    "Done.",
		LatestToolID:       "cmd-sleep",
		LatestToolKind:     "commandExecution",
		LatestToolLabel:    "sleep 10",
		LatestToolStatus:   "completed",
		LatestToolFP:       "tool-sleep-completed",
		LatestProgressText: "completed: sleep 10",
		LatestProgressFP:   "progress-sleep-completed",
		DetailItems: []model.DetailItem{
			{ID: "user-1", Kind: model.DetailItemUser, Text: "Run sleep 10."},
			{ID: "cmd-sleep", Kind: model.DetailItemTool, Label: "sleep 10", Status: "completed", CommentaryIndex: 0, FP: "tool-sleep-completed"},
			{ID: "final-1", Kind: model.DetailItemFinal, Text: "Done.", CommentaryIndex: 0, FP: "final-tool-only"},
		},
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, "explicit")
	panel, err := service.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil || panel == nil {
		t.Fatalf("GetCurrentThreadPanel failed: panel=%#v err=%v", panel, err)
	}
	detailsMessage, _, _ := service.renderDetailsCard(ctx, panel.ID, thread, &appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-tool-only",
		LatestTurnStatus: "completed",
		DetailItems: []model.DetailItem{
			{ID: "t1", Kind: model.DetailItemTool, Label: "sleep 10", Status: "completed", CommentaryIndex: 0},
			{ID: "f1", Kind: model.DetailItemFinal, Text: "Done.", CommentaryIndex: 1},
		},
	}, model.DetailsViewState{})
	details := recordedMessage{text: detailsMessage.Text}
	if !strings.Contains(details.text, "Tool activity") || !strings.Contains(details.text, "sleep 10") || !strings.Contains(details.text, "Status: completed") {
		t.Fatalf("details text = %q, want tool-only command in default Details", details.text)
	}
	if strings.Contains(details.text, "No commentary entries") {
		t.Fatalf("details text = %q, want tool-only section instead of no commentary", details.text)
	}
}

func TestDetailsCallbacksUsePanelTurnInsteadOfLatestThreadTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	threadPayload := map[string]any{
		"id":     "thread-details-history",
		"name":   "Details history",
		"cwd":    `C:\Users\you\Projects\Codex`,
		"status": "completed",
		"turns": []any{
			map[string]any{
				"id":     "turn-old",
				"status": "completed",
				"items": []any{
					map[string]any{"id": "old-c1", "type": "agentMessage", "phase": "commentary", "text": "Old commentary."},
					map[string]any{"id": "old-f1", "type": "agentMessage", "phase": "final_answer", "text": "Old final."},
				},
			},
			map[string]any{
				"id":     "turn-new",
				"status": "completed",
				"items": []any{
					map[string]any{"id": "new-c1", "type": "agentMessage", "phase": "commentary", "text": "New commentary must not leak."},
					map[string]any{"id": "new-f1", "type": "agentMessage", "phase": "final_answer", "text": "New final must not leak."},
				},
			},
		},
	}
	latest := appserver.SnapshotFromThreadRead(threadPayload)
	latest.Thread.Raw = json.RawMessage(model.MustJSON(map[string]any{"thread": threadPayload}))
	if err := service.store.UpsertThread(ctx, latest.Thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertSnapshot(ctx, latest.Thread.ID, appserver.CompactSnapshot(nil, latest, time.Now().UTC())); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	panel, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:            123456789,
		TopicID:           0,
		ProjectName:       latest.Thread.ProjectName,
		ThreadID:          latest.Thread.ID,
		SourceMode:        model.PanelSourceExplicit,
		SummaryMessageID:  101,
		ToolMessageID:     102,
		OutputMessageID:   103,
		CurrentTurnID:     "turn-old",
		Status:            "completed",
		ArchiveEnabled:    true,
		LastFinalNoticeFP: "old-final-fp",
		LastFinalCardHash: "old-card-hash",
		DetailsViewJSON:   model.MustJSON(model.DetailsViewState{}),
		LastSummaryHash:   "old-card-hash",
		LastToolHash:      "old-tool",
		LastOutputHash:    "old-output",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	token := service.callbackButton(ctx, "Details", "details_open", latest.Thread.ID, "turn-old", "", map[string]any{"panel_id": panel.ID, "page": 0}).CallbackData

	if _, err := service.HandleCallbackFromSource(ctx, 123456789, 0, 101, 123456789, token, model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("HandleCallback(details) failed: %v", err)
	}
	if len(sender.edits) != 1 {
		t.Fatalf("edits = %#v, want one details edit", sender.edits)
	}
	text := sender.edits[0].text
	if !strings.Contains(text, "Old commentary.") || strings.Contains(text, "New commentary must not leak.") {
		t.Fatalf("details text = %q, want panel turn details only", text)
	}
}

func TestDetailsCallbacksStayBoundToOriginalPanelAfterNewerRunCompletes(t *testing.T) {
	t.Parallel()

	service, sender, thread, oldPanel, newPanel := setupDetailsHistoryPanels(t)
	ctx := context.Background()

	token := service.callbackButton(ctx, "Details", "details_open", thread.ID, "turn-old", "", map[string]any{"panel_id": oldPanel.ID, "page": 0}).CallbackData
	if _, err := service.HandleCallbackFromSource(ctx, oldPanel.ChatID, oldPanel.TopicID, oldPanel.SummaryMessageID, oldPanel.ChatID, token, model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("HandleCallback(details) failed: %v", err)
	}
	if len(sender.edits) != 1 {
		t.Fatalf("edits = %#v, want one old details edit", sender.edits)
	}
	details := sender.edits[0]
	if details.messageID != oldPanel.SummaryMessageID {
		t.Fatalf("details message id = %d, want old summary %d", details.messageID, oldPanel.SummaryMessageID)
	}
	if !strings.Contains(details.text, "Old commentary.") || strings.Contains(details.text, "New commentary must not leak.") {
		t.Fatalf("details text = %q, want old turn details only", details.text)
	}

	backToken := buttonToken(details.buttons, "Back")
	if backToken == "" {
		t.Fatalf("details buttons = %#v, want Back", details.buttons)
	}
	if _, err := service.HandleCallbackFromSource(ctx, oldPanel.ChatID, oldPanel.TopicID, oldPanel.SummaryMessageID, oldPanel.ChatID, backToken, model.PanelSourceFeishuInput); err != nil {
		t.Fatalf("HandleCallback(back) failed: %v", err)
	}
	if len(sender.edits) != 2 {
		t.Fatalf("edits = %#v, want details and back edits only", sender.edits)
	}
	back := sender.edits[1]
	if back.messageID != oldPanel.SummaryMessageID {
		t.Fatalf("back message id = %d, want old summary %d", back.messageID, oldPanel.SummaryMessageID)
	}
	if !hasHeaderKind(back.text, "Final") || !strings.Contains(back.text, "Old final.") || strings.Contains(back.text, "New final must not leak.") {
		t.Fatalf("back text = %q, want old final card only", back.text)
	}
	for _, edit := range sender.edits {
		if edit.messageID == newPanel.SummaryMessageID {
			t.Fatalf("new panel message was edited by old Details callback: %#v", edit)
		}
	}
	oldRoute, err := service.store.ResolveMessageRoute(ctx, oldPanel.ChatID, oldPanel.TopicID, oldPanel.SummaryMessageID)
	if err != nil {
		t.Fatalf("ResolveMessageRoute(old) failed: %v", err)
	}
	if oldRoute == nil || oldRoute.TurnID != "turn-old" {
		t.Fatalf("old message route = %#v, want turn-old", oldRoute)
	}
	newRoute, err := service.store.ResolveMessageRoute(ctx, newPanel.ChatID, newPanel.TopicID, newPanel.SummaryMessageID)
	if err != nil {
		t.Fatalf("ResolveMessageRoute(new) failed: %v", err)
	}
	if newRoute == nil || newRoute.TurnID != "turn-new" {
		t.Fatalf("new message route = %#v, want turn-new", newRoute)
	}
}

func TestDetailsCallbackWithoutPanelIDDoesNotFallbackToCurrentPanel(t *testing.T) {
	t.Parallel()

	service, sender, thread, oldPanel, _ := setupDetailsHistoryPanels(t)
	ctx := context.Background()

	token := service.callbackButton(ctx, "Details", "details_open", thread.ID, "turn-old", "", map[string]any{"page": 0}).CallbackData
	response, err := service.HandleCallbackFromSource(ctx, oldPanel.ChatID, oldPanel.TopicID, oldPanel.SummaryMessageID, oldPanel.ChatID, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(details missing panel) failed: %v", err)
	}
	if len(sender.edits) != 0 {
		t.Fatalf("edits = %#v, want no edit for missing panel_id", sender.edits)
	}
	if response == nil || !strings.Contains(response.Text, "Details panel is stale") {
		t.Fatalf("response = %#v, want stale details response", response)
	}
}

func TestDetailsCallbackRejectsMismatchedMessageID(t *testing.T) {
	t.Parallel()

	service, sender, thread, oldPanel, newPanel := setupDetailsHistoryPanels(t)
	ctx := context.Background()

	token := service.callbackButton(ctx, "Details", "details_open", thread.ID, "turn-old", "", map[string]any{"panel_id": oldPanel.ID, "page": 0}).CallbackData
	response, err := service.HandleCallbackFromSource(ctx, oldPanel.ChatID, oldPanel.TopicID, newPanel.SummaryMessageID, oldPanel.ChatID, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(details wrong message) failed: %v", err)
	}
	if len(sender.edits) != 0 {
		t.Fatalf("edits = %#v, want no edit for mismatched details message", sender.edits)
	}
	if response == nil || !strings.Contains(response.Text, "Details panel is stale") {
		t.Fatalf("response = %#v, want stale details response", response)
	}
}

func TestTurnOffPlanCallbackSetsDefaultOverrideAndEditsFinalCard(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "turn-off-plan-thread",
		Title:       "Turn off plan",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	snapshot := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-plan-final",
		LatestTurnStatus: "completed",
		LatestFinalText:  "Plan mode final.",
		LatestFinalFP:    "turn-plan-final-fp",
		DetailItems: []model.DetailItem{
			{ID: "plan-1", Kind: model.DetailItemPlan, Text: "Plan text.", CommentaryIndex: 1},
			{ID: "final-1", Kind: model.DetailItemFinal, Text: "Plan mode final.", CommentaryIndex: 1},
		},
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertSnapshot(ctx, thread.ID, appserver.CompactSnapshot(nil, snapshot, time.Now().UTC())); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	panel, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:            123456789,
		TopicID:           0,
		ProjectName:       thread.ProjectName,
		ThreadID:          thread.ID,
		SourceMode:        model.PanelSourceExplicit,
		SummaryMessageID:  101,
		CurrentTurnID:     snapshot.LatestTurnID,
		Status:            "completed",
		ArchiveEnabled:    true,
		LastFinalNoticeFP: snapshot.LatestFinalFP,
		LastFinalCardHash: "old-card-hash",
		DetailsViewJSON:   model.MustJSON(model.DetailsViewState{}),
		LastSummaryHash:   "old-card-hash",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	_, buttons, _ := service.renderFinalCard(ctx, panel.ID, thread, &snapshot)
	token := buttonToken(buttons, "Turn off Plan")
	if token == "" {
		t.Fatalf("final buttons = %#v, want Turn off Plan", buttons)
	}

	response, err := service.HandleCallbackFromSource(ctx, panel.ChatID, panel.TopicID, panel.SummaryMessageID, panel.ChatID, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(turn_off_plan) failed: %v", err)
	}
	if response == nil || !strings.Contains(response.CallbackText, "Plan Mode") {
		t.Fatalf("response = %#v, want callback text", response)
	}
	if got := service.threadCollaborationOverride(ctx, thread.ID); got != collaborationModeDefault {
		t.Fatalf("threadCollaborationOverride = %q, want default", got)
	}
	if len(sender.edits) != 1 {
		t.Fatalf("edits = %#v, want one Final Card edit", sender.edits)
	}
	edit := sender.edits[0]
	if edit.messageID != panel.SummaryMessageID {
		t.Fatalf("edit message id = %d, want %d", edit.messageID, panel.SummaryMessageID)
	}
	if buttonToken(edit.buttons, "Turn off Plan") != "" {
		t.Fatalf("edited buttons = %#v, want Turn off Plan removed", edit.buttons)
	}
}

func TestTurnOffPlanCallbackRejectsMismatchedMessageID(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()
	thread := model.Thread{
		ID:          "turn-off-plan-stale-thread",
		Title:       "Turn off plan stale",
		ProjectName: "Codex",
		CWD:         "/Users/example/project",
		Status:      "idle",
	}
	snapshot := appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-plan-final",
		LatestTurnStatus: "completed",
		LatestFinalText:  "Plan mode final.",
		LatestFinalFP:    "turn-plan-final-fp",
		DetailItems: []model.DetailItem{
			{ID: "plan-1", Kind: model.DetailItemPlan, Text: "Plan text.", CommentaryIndex: 1},
			{ID: "final-1", Kind: model.DetailItemFinal, Text: "Plan mode final.", CommentaryIndex: 1},
		},
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertSnapshot(ctx, thread.ID, appserver.CompactSnapshot(nil, snapshot, time.Now().UTC())); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	panel, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:            123456789,
		TopicID:           0,
		ProjectName:       thread.ProjectName,
		ThreadID:          thread.ID,
		SourceMode:        model.PanelSourceExplicit,
		SummaryMessageID:  101,
		CurrentTurnID:     snapshot.LatestTurnID,
		Status:            "completed",
		ArchiveEnabled:    true,
		LastFinalNoticeFP: snapshot.LatestFinalFP,
		LastFinalCardHash: "old-card-hash",
		DetailsViewJSON:   model.MustJSON(model.DetailsViewState{}),
		LastSummaryHash:   "old-card-hash",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}
	_, buttons, _ := service.renderFinalCard(ctx, panel.ID, thread, &snapshot)
	token := buttonToken(buttons, "Turn off Plan")
	if token == "" {
		t.Fatalf("final buttons = %#v, want Turn off Plan", buttons)
	}

	response, err := service.HandleCallbackFromSource(ctx, panel.ChatID, panel.TopicID, panel.SummaryMessageID+99, panel.ChatID, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(turn_off_plan stale) failed: %v", err)
	}
	if len(sender.edits) != 0 {
		t.Fatalf("edits = %#v, want no edit for stale callback", sender.edits)
	}
	if got := service.threadCollaborationOverride(ctx, thread.ID); got != "" {
		t.Fatalf("threadCollaborationOverride = %q, want unchanged", got)
	}
	if response == nil || !strings.Contains(response.Text, "Details panel is stale") {
		t.Fatalf("response = %#v, want stale response", response)
	}
}

func TestDetailsToolsFileRejectsMismatchedPanelRoute(t *testing.T) {
	t.Parallel()

	service, sender, thread, oldPanel, _ := setupDetailsHistoryPanels(t)
	ctx := context.Background()

	token := service.callbackButton(ctx, "Tools file", "details_tools_file", thread.ID, "turn-new", "", map[string]any{"panel_id": oldPanel.ID, "commentary_index": 1}).CallbackData
	response, err := service.HandleCallbackFromSource(ctx, oldPanel.ChatID, oldPanel.TopicID, oldPanel.SummaryMessageID, oldPanel.ChatID, token, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleCallback(tools file wrong turn) failed: %v", err)
	}
	if len(sender.documents) != 0 {
		t.Fatalf("documents = %#v, want no export for mismatched details route", sender.documents)
	}
	if response == nil || !strings.Contains(response.Text, "Details panel is stale") {
		t.Fatalf("response = %#v, want stale details response", response)
	}
}

func setupDetailsHistoryPanels(t *testing.T) (*Service, *recordingSender, model.Thread, *model.ThreadPanel, *model.ThreadPanel) {
	t.Helper()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	threadPayload := map[string]any{
		"id":     "thread-details-binding",
		"name":   "Details binding",
		"cwd":    `C:\Users\you\Projects\Codex`,
		"status": "completed",
		"turns": []any{
			map[string]any{
				"id":     "turn-old",
				"status": "completed",
				"items": []any{
					map[string]any{"id": "old-c1", "type": "agentMessage", "phase": "commentary", "text": "Old commentary."},
					map[string]any{"id": "old-cmd", "type": "commandExecution", "command": "printf old", "status": "completed", "aggregatedOutput": "old output\n"},
					map[string]any{"id": "old-f1", "type": "agentMessage", "phase": "final_answer", "text": "Old final."},
				},
			},
			map[string]any{
				"id":     "turn-new",
				"status": "completed",
				"items": []any{
					map[string]any{"id": "new-c1", "type": "agentMessage", "phase": "commentary", "text": "New commentary must not leak."},
					map[string]any{"id": "new-cmd", "type": "commandExecution", "command": "printf new", "status": "completed", "aggregatedOutput": "new output\n"},
					map[string]any{"id": "new-f1", "type": "agentMessage", "phase": "final_answer", "text": "New final must not leak."},
				},
			},
		},
	}
	latest := appserver.SnapshotFromThreadRead(threadPayload)
	latest.Thread.Raw = json.RawMessage(model.MustJSON(map[string]any{"thread": threadPayload}))
	if err := service.store.UpsertThread(ctx, latest.Thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	if err := service.store.UpsertSnapshot(ctx, latest.Thread.ID, appserver.CompactSnapshot(nil, latest, time.Now().UTC())); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}
	oldPanel, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:            123456789,
		TopicID:           0,
		ProjectName:       latest.Thread.ProjectName,
		ThreadID:          latest.Thread.ID,
		SourceMode:        model.PanelSourceExplicit,
		SummaryMessageID:  101,
		ToolMessageID:     102,
		OutputMessageID:   103,
		CurrentTurnID:     "turn-old",
		Status:            "completed",
		ArchiveEnabled:    true,
		LastFinalNoticeFP: "old-final-fp",
		LastFinalCardHash: "old-card-hash",
		DetailsViewJSON:   model.MustJSON(model.DetailsViewState{}),
		LastSummaryHash:   "old-card-hash",
		LastToolHash:      "old-tool",
		LastOutputHash:    "old-output",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(old) failed: %v", err)
	}
	newPanel, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:            123456789,
		TopicID:           0,
		ProjectName:       latest.Thread.ProjectName,
		ThreadID:          latest.Thread.ID,
		SourceMode:        model.PanelSourceExplicit,
		SummaryMessageID:  201,
		ToolMessageID:     202,
		OutputMessageID:   203,
		CurrentTurnID:     "turn-new",
		Status:            "completed",
		ArchiveEnabled:    true,
		LastFinalNoticeFP: "new-final-fp",
		LastFinalCardHash: "new-card-hash",
		DetailsViewJSON:   model.MustJSON(model.DetailsViewState{}),
		LastSummaryHash:   "new-card-hash",
		LastToolHash:      "new-tool",
		LastOutputHash:    "new-output",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(new) failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{ChatID: oldPanel.ChatID, TopicID: oldPanel.TopicID, MessageID: oldPanel.SummaryMessageID, ThreadID: latest.Thread.ID, TurnID: "turn-old", EventID: "old-final-fp", CreatedAt: model.NowString()}); err != nil {
		t.Fatalf("PutMessageRoute(old) failed: %v", err)
	}
	if err := service.store.PutMessageRoute(ctx, model.MessageRoute{ChatID: newPanel.ChatID, TopicID: newPanel.TopicID, MessageID: newPanel.SummaryMessageID, ThreadID: latest.Thread.ID, TurnID: "turn-new", EventID: "new-final-fp", CreatedAt: model.NowString()}); err != nil {
		t.Fatalf("PutMessageRoute(new) failed: %v", err)
	}
	return service, sender, latest.Thread, oldPanel, newPanel
}

func TestTerminalSyncAdoptsActivePanelWithoutTurnID(t *testing.T) {
	t.Parallel()

	service := newTestService(t)
	sender := &recordingSender{}
	service.SetSender(sender)
	ctx := context.Background()

	thread := model.Thread{
		ID:          "thread-adopt-turn",
		Title:       "Adopt turn",
		ProjectName: "Codex",
		CWD:         `C:\Users\you\Projects\Codex`,
		UpdatedAt:   time.Now().UTC().Unix(),
	}
	if err := service.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	target := model.ObserverTarget{ChatKey: model.ChatKey(123456789, 0), ChatID: 123456789, TopicID: 0, Enabled: true}
	if _, err := service.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           target.ChatID,
		TopicID:          target.TopicID,
		ProjectName:      thread.ProjectName,
		ThreadID:         thread.ID,
		SourceMode:       "explicit",
		SummaryMessageID: 101,
		ToolMessageID:    102,
		OutputMessageID:  103,
		CurrentTurnID:    "",
		Status:           "inProgress",
		ArchiveEnabled:   true,
	}); err != nil {
		t.Fatalf("CreateThreadPanel failed: %v", err)
	}

	snapshot := appserver.CompactSnapshot(nil, appserver.ThreadReadSnapshot{
		Thread:           thread,
		LatestTurnID:     "turn-adopted",
		LatestTurnStatus: "completed",
		LatestFinalFP:    "final-adopted",
		LatestFinalText:  "ADOPTED_OK",
	}, time.Now().UTC())
	if err := service.store.UpsertSnapshot(ctx, thread.ID, snapshot); err != nil {
		t.Fatalf("UpsertSnapshot failed: %v", err)
	}

	service.syncThreadPanelToTarget(ctx, target, thread.ID, false, model.PanelSourceFeishuInput)

	if len(finalMessages(sender.messages)) != 1 {
		t.Fatalf("messages = %#v, want standalone final card", sender.messages)
	}
	finalEdit := lastCodexPanelEdit(t, sender.edits)
	if finalEdit.codexFinalMarkdown != "" {
		t.Fatalf("final edit = %#v, want terminal progress panel without final", finalEdit)
	}
	assertOnlyLatestDetailsButton(t, finalEdit.buttons)
	finalCard := lastFinalMessage(t, sender.messages)
	if !strings.Contains(finalCard.text, "ADOPTED_OK") {
		t.Fatalf("final card = %#v, want adopted final", finalCard)
	}
	if len(sender.deletes) != 2 || sender.deletes[0].messageID != 102 || sender.deletes[1].messageID != 103 {
		t.Fatalf("deletes = %#v, want existing tool/output delete and summary retained", sender.deletes)
	}
}
