package daemon

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/mideco-tech/codex-tg/internal/appserver"
	"github.com/mideco-tech/codex-tg/internal/model"
	"github.com/mideco-tech/codex-tg/internal/msgformat"
)

const (
	threadSummaryLimit = 3
	outputMessageLimit = 3900
	steerTTL           = 10 * time.Minute
)

func (s *Service) syncThreadPanel(ctx context.Context, threadID string) {
	seen := map[string]struct{}{}
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	panels, err := s.store.ListCurrentPanelsForThread(ctx, threadID)
	if err != nil {
		return
	}
	for _, panel := range panels {
		canonicalTarget := canonicalThreadPanelTarget(ctx, sender, model.ObserverTarget{
			ChatKey: model.ChatKey(panel.ChatID, panel.TopicID),
			ChatID:  panel.ChatID,
			TopicID: panel.TopicID,
			Enabled: true,
		})
		if canonicalTarget.ChatID != panel.ChatID || canonicalTarget.TopicID != panel.TopicID {
			_ = s.store.SupersedeThreadPanel(ctx, panel.ID)
		}
		chatKey := canonicalTarget.ChatKey
		if _, ok := seen[chatKey]; ok {
			continue
		}
		seen[chatKey] = struct{}{}
		s.syncThreadPanelToTarget(ctx, *canonicalTarget, threadID, false, panel.SourceMode)
	}
}

func canonicalThreadPanelTarget(ctx context.Context, sender Sender, target model.ObserverTarget) *model.ObserverTarget {
	resolver, ok := sender.(ThreadTopicTargetResolver)
	if !ok {
		return &target
	}
	chatID, err := resolver.ResolveThreadTopicTarget(ctx, target.ChatID)
	if err != nil || chatID == 0 {
		return &target
	}
	target.ChatID = chatID
	target.TopicID = 0
	target.ChatKey = model.ChatKey(target.ChatID, target.TopicID)
	return &target
}

func (s *Service) syncThreadPanelToTarget(ctx context.Context, target model.ObserverTarget, threadID string, forceNew bool, sourceMode string) {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender == nil {
		return
	}
	if sourceMode == model.PanelSourceFeishuInput {
		ctx = withCompactFeishuTopicCard(ctx)
	}
	thread, snapshot, err := s.loadThreadPanelSnapshot(ctx, threadID)
	if err != nil || thread == nil || snapshot == nil {
		return
	}
	s.notifyTerminalSnapshot(ctx, *thread, snapshot)
	pending, _ := s.store.GetLatestPendingApprovalForThread(ctx, threadID)
	pending = pendingForSnapshot(pending, snapshot)

	s.panelMu.Lock()
	defer s.panelMu.Unlock()

	existingPanel, _ := s.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	renderSourceMode := s.effectivePanelRenderSourceMode(ctx, existingPanel, thread.ID, snapshot.LatestTurnID, sourceMode)
	effectiveForceNew := forceNew
	if isDirectInputSourceMode(sourceMode) && samePanelTurn(existingPanel, snapshot.LatestTurnID) {
		effectiveForceNew = false
	}
	threadTopic, err := s.ensureThreadTopic(ctx, sender, target, *thread, snapshot, sourceMode)
	if err != nil {
		s.setError(ctx, err)
		return
	}
	if threadTopic != nil && threadTopic.ChatID != 0 {
		target.ChatID = threadTopic.ChatID
		target.TopicID = 0
		target.ChatKey = model.ChatKey(target.ChatID, target.TopicID)
		existingPanel, _ = s.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
		renderSourceMode = s.effectivePanelRenderSourceMode(ctx, existingPanel, thread.ID, snapshot.LatestTurnID, sourceMode)
		if isDirectInputSourceMode(sourceMode) && samePanelTurn(existingPanel, snapshot.LatestTurnID) {
			effectiveForceNew = false
		}
	}
	sender = senderWithThreadTopic{Sender: sender, topic: threadTopic}
	panel, err := s.ensureCurrentPanel(ctx, sender, target, *thread, snapshot, pending, effectiveForceNew, sourceMode, renderSourceMode)
	if err != nil {
		s.setError(ctx, fmt.Errorf("ensure current panel: %w", err))
		return
	}
	if panel == nil {
		s.setError(ctx, fmt.Errorf("ensure current panel returned nil for thread %s turn %s", thread.ID, snapshot.LatestTurnID))
		return
	}
	if isDirectInputSourceMode(sourceMode) && panel.SourceMode != sourceMode && samePanelTurn(panel, snapshot.LatestTurnID) {
		panel.SourceMode = sourceMode
		_ = s.store.UpdateThreadPanelSourceMode(ctx, panel.ID, panel.SourceMode)
	}
	legacyTerminalReplay := existingPanel != nil && existingPanel.CurrentTurnID == strings.TrimSpace(snapshot.LatestTurnID) && isLegacyTerminalReplay(panel, snapshot)
	if isTerminalStatus(snapshot.LatestTurnStatus) && strings.TrimSpace(snapshot.LatestFinalFP) != "" && panel.LastFinalNoticeFP == snapshot.LatestFinalFP {
		return
	}
	if legacyTerminalReplay && snapshot.LatestFinalFP != "" {
		panel.LastFinalNoticeFP = snapshot.LatestFinalFP
		if err := s.store.UpdateThreadPanelState(ctx, panel.ID, panel.CurrentTurnID, panel.Status, panel.LastSummaryHash, panel.LastToolHash, panel.LastOutputHash, panel.LastFinalNoticeFP); err != nil {
			s.setError(ctx, err)
		}
		return
	}
	if shouldRenderFinalCardNow(panel, snapshot) {
		if err := s.maybeSendUserRequestNotice(ctx, sender, panel, *thread, snapshot, renderSourceMode); err != nil {
			s.setError(ctx, err)
			return
		}
	}
	if err := s.updateCurrentPanel(ctx, sender, panel, *thread, snapshot, pending, renderSourceMode); err != nil {
		panel, recreateErr := s.createCurrentPanel(ctx, sender, target, *thread, snapshot, pending, sourceMode, renderSourceMode)
		if recreateErr != nil || panel == nil {
			if recreateErr != nil {
				s.setError(ctx, fmt.Errorf("recreate current panel after update error %v: %w", err, recreateErr))
			} else {
				s.setError(ctx, fmt.Errorf("recreate current panel returned nil after update error: %w", err))
			}
			return
		}
		legacyTerminalReplay = false
		if err := s.updateCurrentPanel(ctx, sender, panel, *thread, snapshot, pending, renderSourceMode); err != nil {
			s.setError(ctx, err)
			return
		}
	}
}

func (s *Service) ensureThreadTopic(ctx context.Context, sender Sender, target model.ObserverTarget, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, sourceMode string) (*model.FeishuThreadTopic, error) {
	topicSender, ok := sender.(ThreadTopicSender)
	if !ok {
		return nil, nil
	}
	topic, err := topicSender.EnsureThreadTopic(ctx, target.ChatID, thread, snapshot, sourceMode)
	if err != nil || topic == nil || topic.RootMessageID == 0 {
		return topic, err
	}
	if sourceMode == model.PanelSourceFeishuInput && topic.ChatID != 0 && topic.ChatID != target.ChatID {
		_ = s.store.SupersedeCurrentThreadPanelsExcept(ctx, thread.ID, topic.ChatID, 0)
	}
	route := model.MessageRoute{
		ChatID:    target.ChatID,
		TopicID:   target.TopicID,
		MessageID: topic.RootMessageID,
		ThreadID:  thread.ID,
		TurnID:    firstNonEmpty(strings.TrimSpace(snapshot.LatestTurnID), strings.TrimSpace(thread.ActiveTurnID)),
		EventID:   "feishu.thread_topic.root." + strings.TrimSpace(thread.ID),
		CreatedAt: model.NowString(),
	}
	_ = s.store.PutMessageRoute(ctx, route)
	if topic.ChatID != 0 && topic.ChatID != target.ChatID {
		route.ChatID = topic.ChatID
		route.TopicID = 0
		_ = s.store.PutMessageRoute(ctx, route)
	}
	return topic, nil
}

type senderWithThreadTopic struct {
	Sender
	topic *model.FeishuThreadTopic
}

func (s senderWithThreadTopic) SendMessage(ctx context.Context, chatID, topicID int64, text string, buttons [][]model.ButtonSpec, options model.SendOptions) (int64, error) {
	chatID, topicID = s.target(chatID, topicID)
	return s.Sender.SendMessage(ctx, chatID, topicID, text, buttons, s.withThreadTopicOptions(options))
}

func (s senderWithThreadTopic) SendRenderedMessages(ctx context.Context, chatID, topicID int64, messages []model.RenderedMessage, buttons [][]model.ButtonSpec, options model.SendOptions) ([]int64, error) {
	chatID, topicID = s.target(chatID, topicID)
	return s.Sender.SendRenderedMessages(ctx, chatID, topicID, messages, buttons, s.withThreadTopicOptions(options))
}

func (s senderWithThreadTopic) SendDocumentData(ctx context.Context, chatID, topicID int64, fileName string, data []byte, caption string, options model.SendOptions) (int64, error) {
	chatID, topicID = s.target(chatID, topicID)
	return s.Sender.SendDocumentData(ctx, chatID, topicID, fileName, data, caption, s.withThreadTopicOptions(options))
}

func (s senderWithThreadTopic) target(chatID, topicID int64) (int64, int64) {
	if s.topic == nil || s.topic.ChatID == 0 {
		return chatID, topicID
	}
	return s.topic.ChatID, 0
}

func (s senderWithThreadTopic) withThreadTopicOptions(options model.SendOptions) model.SendOptions {
	if s.topic == nil || s.topic.RootMessageID == 0 || options.FeishuReplyToMessageID != 0 {
		return options
	}
	options.FeishuReplyToMessageID = s.topic.RootMessageID
	options.FeishuReplyInThread = true
	options.FeishuCodexThreadID = s.topic.ThreadID
	return options
}

func (s *Service) loadThreadPanelSnapshot(ctx context.Context, threadID string) (*model.Thread, *appserver.ThreadReadSnapshot, error) {
	thread, err := s.store.GetThread(ctx, threadID)
	if err != nil || thread == nil {
		return nil, nil, err
	}
	snapshotState, err := s.store.GetSnapshot(ctx, threadID)
	if err != nil || snapshotState == nil || len(snapshotState.CompactJSON) == 0 {
		return thread, nil, err
	}
	var snapshot appserver.ThreadReadSnapshot
	if err := json.Unmarshal(snapshotState.CompactJSON, &snapshot); err != nil {
		return thread, nil, err
	}
	if snapshot.Thread.ID == "" {
		snapshot.Thread = *thread
	}
	return thread, &snapshot, nil
}

func (s *Service) ensureCurrentPanel(ctx context.Context, sender Sender, target model.ObserverTarget, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, pending *model.PendingApproval, forceNew bool, sourceMode, renderSourceMode string) (*model.ThreadPanel, error) {
	panel, err := s.store.GetCurrentThreadPanel(ctx, target.ChatID, target.TopicID, thread.ID)
	if err != nil {
		return nil, err
	}
	turnID := strings.TrimSpace(snapshot.LatestTurnID)
	status := strings.TrimSpace(snapshot.LatestTurnStatus)
	if forceNew || panel == nil || panelNeedsRefresh(panel, turnID, status) {
		panel, err = s.createCurrentPanel(ctx, sender, target, thread, snapshot, pending, sourceMode, renderSourceMode)
		if err != nil {
			return nil, err
		}
	}
	return panel, nil
}

func (s *Service) effectivePanelRenderSourceMode(ctx context.Context, existingPanel *model.ThreadPanel, threadID, turnID, sourceMode string) string {
	origin := s.inputOriginTurnSource(ctx, threadID, turnID)
	if isDirectInputSourceMode(origin) {
		return origin
	}
	if isDirectInputSourceMode(sourceMode) && existingPanel != nil && strings.TrimSpace(turnID) != "" && strings.TrimSpace(existingPanel.CurrentTurnID) != strings.TrimSpace(turnID) {
		return model.PanelSourceExplicit
	}
	if isDirectInputSourceMode(sourceMode) {
		return sourceMode
	}
	return sourceMode
}

func panelNeedsRefresh(panel *model.ThreadPanel, turnID, status string) bool {
	if panel == nil {
		return true
	}
	if panel.SummaryMessageID == 0 {
		return true
	}
	if strings.TrimSpace(turnID) == "" {
		return false
	}
	if strings.TrimSpace(panel.CurrentTurnID) == "" {
		return isTerminalStatus(panel.Status)
	}
	if panel.CurrentTurnID != turnID && isTerminalStatus(panel.Status) {
		return true
	}
	return false
}

func samePanelTurn(panel *model.ThreadPanel, turnID string) bool {
	if panel == nil {
		return false
	}
	turnID = strings.TrimSpace(turnID)
	return turnID != "" && strings.TrimSpace(panel.CurrentTurnID) == turnID
}

func isLegacyTerminalReplay(panel *model.ThreadPanel, snapshot *appserver.ThreadReadSnapshot) bool {
	if panel == nil || snapshot == nil {
		return false
	}
	if panel.UserMessageID != 0 {
		return false
	}
	if strings.TrimSpace(panel.LastFinalNoticeFP) != "" || strings.TrimSpace(snapshot.LatestFinalFP) == "" {
		return false
	}
	if strings.TrimSpace(panel.CurrentTurnID) == "" || strings.TrimSpace(snapshot.LatestTurnID) == "" {
		return false
	}
	if panel.CurrentTurnID != strings.TrimSpace(snapshot.LatestTurnID) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(panel.Status), "interrupted") {
		return false
	}
	return isTerminalStatus(panel.Status) && isTerminalStatus(snapshot.LatestTurnStatus)
}

func shouldRenderFinalCardNow(panel *model.ThreadPanel, snapshot *appserver.ThreadReadSnapshot) bool {
	if panel == nil || snapshot == nil {
		return false
	}
	finalText, finalFP := terminalCardTextAndFP(snapshot)
	if !isTerminalStatus(snapshot.LatestTurnStatus) || strings.TrimSpace(finalText) == "" {
		return false
	}
	finalFP = strings.TrimSpace(finalFP)
	return finalFP != "" && finalFP != strings.TrimSpace(panel.LastFinalNoticeFP)
}

func (s *Service) createCurrentPanel(ctx context.Context, sender Sender, target model.ObserverTarget, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, pending *model.PendingApproval, sourceMode, renderSourceMode string) (*model.ThreadPanel, error) {
	planPromptMessageID, planPromptFP, err := s.sendPlanPromptNotice(ctx, sender, target, thread, effectivePlanPrompt(pending, snapshot))
	if err != nil {
		return nil, err
	}
	userMessageID, userNoticeFP, err := s.sendUserRequestNotice(ctx, sender, target, thread, snapshot, renderSourceMode)
	if err != nil {
		return nil, err
	}
	summaryMessage, summaryButtons, summaryHash := s.renderSummaryPanel(ctx, thread, snapshot, nil)
	toolText, toolHash, shouldSendTool := s.renderToolPanelIfNeeded(ctx, thread, snapshot)
	outputText, outputHash, shouldSendOutput := s.renderOutputPanelIfNeeded(ctx, thread, snapshot)
	finalNoticeFP := ""
	finalCardHash := ""
	if isTerminalStatus(cardDisplayStatus(snapshot, thread)) {
		if _, fp := terminalCardTextAndFP(snapshot); strings.TrimSpace(fp) != "" {
			finalNoticeFP = strings.TrimSpace(fp)
			finalCardHash = summaryHash
		}
	}

	s.logChatRenderedMessagesContainsNil(thread.ID, snapshot.LatestTurnID, "summary", 0, []model.RenderedMessage{summaryMessage})
	summaryIDs, err := sender.SendRenderedMessages(ctx, target.ChatID, target.TopicID, []model.RenderedMessage{summaryMessage}, summaryButtons, silentSendOptions())
	if err != nil {
		return nil, err
	}
	summaryID := lastMessageID(summaryIDs)
	var toolID int64
	if shouldSendTool {
		s.logChatRenderContainsNil(thread.ID, snapshot.LatestTurnID, "tool", 0, toolText)
		toolID, err = sender.SendMessage(ctx, target.ChatID, target.TopicID, toolText, nil, silentSendOptions())
		if err != nil {
			return nil, err
		}
	}
	var outputID int64
	if shouldSendOutput {
		s.logChatRenderContainsNil(thread.ID, snapshot.LatestTurnID, "output", 0, outputText)
		outputID, err = sender.SendMessage(ctx, target.ChatID, target.TopicID, outputText, nil, silentSendOptions())
		if err != nil {
			return nil, err
		}
	}

	panel, err := s.store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:              target.ChatID,
		TopicID:             target.TopicID,
		ProjectName:         thread.ProjectName,
		ThreadID:            thread.ID,
		SourceMode:          sourceMode,
		SummaryMessageID:    summaryID,
		ToolMessageID:       toolID,
		OutputMessageID:     outputID,
		UserMessageID:       userMessageID,
		LastUserNoticeFP:    userNoticeFP,
		CurrentTurnID:       snapshot.LatestTurnID,
		Status:              snapshot.LatestTurnStatus,
		ArchiveEnabled:      true,
		LastSummaryHash:     summaryHash,
		LastToolHash:        toolHash,
		LastOutputHash:      outputHash,
		LastFinalNoticeFP:   finalNoticeFP,
		PlanPromptMessageID: planPromptMessageID,
		LastPlanPromptFP:    planPromptFP,
		LastFinalCardHash:   finalCardHash,
	})
	if err != nil {
		return nil, err
	}
	_ = s.store.PutMessageRoute(ctx, model.MessageRoute{ChatID: target.ChatID, TopicID: target.TopicID, MessageID: summaryID, ThreadID: thread.ID, TurnID: snapshot.LatestTurnID, CreatedAt: model.NowString()})
	if toolID != 0 {
		_ = s.store.PutMessageRoute(ctx, model.MessageRoute{ChatID: target.ChatID, TopicID: target.TopicID, MessageID: toolID, ThreadID: thread.ID, TurnID: snapshot.LatestTurnID, CreatedAt: model.NowString()})
	}
	if outputID != 0 {
		_ = s.store.PutMessageRoute(ctx, model.MessageRoute{ChatID: target.ChatID, TopicID: target.TopicID, MessageID: outputID, ThreadID: thread.ID, TurnID: snapshot.LatestTurnID, CreatedAt: model.NowString()})
	}
	return panel, nil
}

func (s *Service) maybeSendUserRequestNotice(ctx context.Context, sender Sender, panel *model.ThreadPanel, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, renderSourceMode string) error {
	if panel == nil || strings.TrimSpace(snapshot.LatestUserMessageFP) == "" {
		return nil
	}
	if snapshot.LatestUserMessageFP == panel.LastUserNoticeFP {
		return nil
	}
	if !shouldSendUserRequestNotice(renderSourceMode, snapshot) || s.isDirectInputOriginTurn(ctx, thread.ID, snapshot.LatestTurnID) {
		return nil
	}
	if existing, err := s.store.FindMessageRouteByEvent(ctx, thread.ID, snapshot.LatestTurnID, snapshot.LatestUserMessageID, snapshot.LatestUserMessageFP); err == nil && existing != nil && existing.MessageID != 0 {
		panel.UserMessageID = existing.MessageID
		panel.LastUserNoticeFP = snapshot.LatestUserMessageFP
		return s.store.UpdateThreadPanelUserNotice(ctx, panel.ID, panel.UserMessageID, panel.LastUserNoticeFP)
	}
	noticeSourceMode := s.userRequestNoticeSourceMode(ctx, thread.ID, snapshot.LatestTurnID, renderSourceMode)
	if panel.UserMessageID != 0 {
		message := firstRenderedMessage(s.renderUserRequestNoticeCard(ctx, thread, snapshot, noticeSourceMode))
		s.logChatRenderedMessagesContainsNil(thread.ID, snapshot.LatestTurnID, "user", panel.UserMessageID, []model.RenderedMessage{message})
		if err := sender.EditRenderedMessage(ctx, panel.ChatID, panel.TopicID, panel.UserMessageID, message, nil); err != nil {
			s.setError(ctx, fmt.Errorf("edit user notice card: %w", err))
			return nil
		}
		_ = s.store.PutMessageRoute(ctx, model.MessageRoute{
			ChatID:    panel.ChatID,
			TopicID:   panel.TopicID,
			MessageID: panel.UserMessageID,
			ThreadID:  thread.ID,
			TurnID:    snapshot.LatestTurnID,
			ItemID:    snapshot.LatestUserMessageID,
			EventID:   snapshot.LatestUserMessageFP,
			CreatedAt: model.NowString(),
		})
		panel.LastUserNoticeFP = snapshot.LatestUserMessageFP
		return s.store.UpdateThreadPanelUserNotice(ctx, panel.ID, panel.UserMessageID, panel.LastUserNoticeFP)
	}
	target := model.ObserverTarget{
		ChatKey: model.ChatKey(panel.ChatID, panel.TopicID),
		ChatID:  panel.ChatID,
		TopicID: panel.TopicID,
		Enabled: true,
	}
	messageID, noticeFP, err := s.sendUserRequestNotice(ctx, sender, target, thread, snapshot, noticeSourceMode)
	if err != nil {
		return err
	}
	if noticeFP == "" {
		return nil
	}
	panel.UserMessageID = messageID
	panel.LastUserNoticeFP = noticeFP
	return s.store.UpdateThreadPanelUserNotice(ctx, panel.ID, panel.UserMessageID, panel.LastUserNoticeFP)
}

func (s *Service) maybeSendPlanPromptNotice(ctx context.Context, sender Sender, panel *model.ThreadPanel, thread model.Thread, prompt *model.PlanPrompt) error {
	if panel == nil || prompt == nil || strings.TrimSpace(prompt.Fingerprint) == "" || prompt.Fingerprint == panel.LastPlanPromptFP {
		return nil
	}
	target := model.ObserverTarget{
		ChatKey: model.ChatKey(panel.ChatID, panel.TopicID),
		ChatID:  panel.ChatID,
		TopicID: panel.TopicID,
		Enabled: true,
	}
	messageID, promptFP, err := s.sendPlanPromptNotice(ctx, sender, target, thread, prompt)
	if err != nil {
		return err
	}
	if promptFP == "" {
		return nil
	}
	panel.PlanPromptMessageID = messageID
	panel.LastPlanPromptFP = promptFP
	return s.store.UpdateThreadPanelPlanPrompt(ctx, panel.ID, panel.PlanPromptMessageID, panel.LastPlanPromptFP)
}

func (s *Service) sendPlanPromptNotice(ctx context.Context, sender Sender, target model.ObserverTarget, thread model.Thread, prompt *model.PlanPrompt) (int64, string, error) {
	if prompt == nil || strings.TrimSpace(prompt.Question) == "" || strings.TrimSpace(prompt.Fingerprint) == "" {
		return 0, "", nil
	}
	message, buttons, _ := s.renderPlanPromptCard(ctx, thread, prompt)
	s.logChatRenderedMessagesContainsNil(thread.ID, prompt.TurnID, "plan", 0, []model.RenderedMessage{message})
	messageIDs, err := sender.SendRenderedMessages(ctx, target.ChatID, target.TopicID, []model.RenderedMessage{message}, buttons, notifySendOptions())
	if err != nil {
		return 0, "", err
	}
	messageID := lastMessageID(messageIDs)
	_ = s.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    target.ChatID,
		TopicID:   target.TopicID,
		MessageID: messageID,
		ThreadID:  thread.ID,
		TurnID:    prompt.TurnID,
		ItemID:    firstNonEmpty(prompt.ItemID, prompt.PromptID),
		EventID:   planPromptRouteEventID(prompt),
		CreatedAt: model.NowString(),
	})
	return messageID, prompt.Fingerprint, nil
}

func planPromptRouteEventID(prompt *model.PlanPrompt) string {
	if prompt != nil && strings.TrimSpace(prompt.RequestID) != "" {
		return "plan_request:" + strings.TrimSpace(prompt.RequestID)
	}
	if prompt == nil {
		return ""
	}
	return prompt.Fingerprint
}

func (s *Service) sendUserRequestNotice(ctx context.Context, sender Sender, target model.ObserverTarget, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, sourceMode string) (int64, string, error) {
	if !shouldSendUserRequestNotice(sourceMode, snapshot) || s.isDirectInputOriginTurn(ctx, thread.ID, snapshot.LatestTurnID) {
		return 0, "", nil
	}
	if existing, err := s.store.FindMessageRouteByEvent(ctx, thread.ID, "", "", snapshot.LatestUserMessageFP); err == nil && existing != nil && existing.MessageID != 0 {
		return existing.MessageID, snapshot.LatestUserMessageFP, nil
	}
	messages := s.renderUserRequestNoticeCard(ctx, thread, snapshot, s.userRequestNoticeSourceMode(ctx, thread.ID, snapshot.LatestTurnID, sourceMode))
	s.logChatRenderedMessagesContainsNil(thread.ID, snapshot.LatestTurnID, "user", 0, messages)
	messageIDs, err := sender.SendRenderedMessages(ctx, target.ChatID, target.TopicID, messages, nil, silentSendOptions())
	if err != nil {
		return 0, "", err
	}
	canonicalMessageID := firstMessageID(messageIDs)
	for _, messageID := range messageIDs {
		_ = s.store.PutMessageRoute(ctx, model.MessageRoute{
			ChatID:    target.ChatID,
			TopicID:   target.TopicID,
			MessageID: messageID,
			ThreadID:  thread.ID,
			TurnID:    snapshot.LatestTurnID,
			ItemID:    snapshot.LatestUserMessageID,
			EventID:   snapshot.LatestUserMessageFP,
			CreatedAt: model.NowString(),
		})
	}
	return canonicalMessageID, snapshot.LatestUserMessageFP, nil
}

func (s *Service) renderUserRequestNoticeCard(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, renderSourceMode string) []model.RenderedMessage {
	messages := msgformat.RenderSegments([]msgformat.Segment{
		msgformat.Markdown(snapshot.LatestUserMessageText),
	}, msgformat.MessageLimit)
	if renderSourceMode != model.PanelSourceFeishuInput {
		for i := range messages {
			messages[i].Style = model.MessageStyleDesktopUser
		}
	}
	if len(messages) > 0 && strings.TrimSpace(snapshot.LatestUserMessageImagePath) != "" {
		messages[0].ImagePath = strings.TrimSpace(snapshot.LatestUserMessageImagePath)
	}
	return messages
}

func (s *Service) userRequestNoticeSourceMode(ctx context.Context, threadID, turnID, fallback string) string {
	if isDirectInputSourceMode(fallback) && !s.isDirectInputOriginTurn(ctx, threadID, turnID) {
		return model.PanelSourceExplicit
	}
	return fallback
}

func shouldSendUserRequestNotice(sourceMode string, snapshot *appserver.ThreadReadSnapshot) bool {
	if snapshot == nil || strings.TrimSpace(snapshot.LatestUserMessageText) == "" || strings.TrimSpace(snapshot.LatestUserMessageFP) == "" {
		return false
	}
	return true
}

func (s *Service) markChatOriginTurn(ctx context.Context, threadID, turnID string) error {
	return s.markInputOriginTurn(ctx, threadID, turnID, model.PanelSourceFeishuInput, 0, 0)
}

func (s *Service) markChatOriginTurnFromChat(ctx context.Context, threadID, turnID string, chatID, topicID int64) error {
	return s.markInputOriginTurn(ctx, threadID, turnID, model.PanelSourceFeishuInput, chatID, topicID)
}

func (s *Service) isChatOriginTurn(ctx context.Context, threadID, turnID string) bool {
	return s.inputOriginTurnSource(ctx, threadID, turnID) == model.PanelSourceFeishuInput
}

func (s *Service) markInputOriginTurn(ctx context.Context, threadID, turnID, sourceMode string, chatID, topicID int64) error {
	sourceMode = normalizeInputSourceMode(sourceMode)
	key := chatOriginTurnKey(threadID, turnID)
	if key == "" {
		return nil
	}
	err := s.store.SetState(ctx, key, sourceMode)
	s.logLifecycle("chat_origin_turn_marked", lifecycleFields{
		"chat_key":    model.ChatKey(chatID, topicID),
		"source_mode": sourceMode,
		"thread_id":   threadID,
		"turn_id":     turnID,
		"error":       err,
	})
	return err
}

func (s *Service) isDirectInputOriginTurn(ctx context.Context, threadID, turnID string) bool {
	return isDirectInputSourceMode(s.inputOriginTurnSource(ctx, threadID, turnID))
}

func (s *Service) inputOriginTurnSource(ctx context.Context, threadID, turnID string) string {
	key := chatOriginTurnKey(threadID, turnID)
	if key == "" {
		return ""
	}
	value, err := s.store.GetState(ctx, key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func chatOriginTurnKey(threadID, turnID string) string {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if threadID == "" || turnID == "" {
		return ""
	}
	return "turn_origin.chat." + threadID + "." + turnID
}

func normalizeInputSourceMode(sourceMode string) string {
	switch strings.TrimSpace(sourceMode) {
	case model.PanelSourceFeishuInput:
		return model.PanelSourceFeishuInput
	case model.PanelSourceExplicit:
		return model.PanelSourceExplicit
	default:
		return model.PanelSourceExplicit
	}
}

func isDirectInputSourceMode(sourceMode string) bool {
	switch strings.TrimSpace(sourceMode) {
	case model.PanelSourceFeishuInput:
		return true
	default:
		return false
	}
}

func (s *Service) updateCurrentPanel(ctx context.Context, sender Sender, panel *model.ThreadPanel, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, pending *model.PendingApproval, renderSourceMode string) error {
	if err := s.maybeSendUserRequestNotice(ctx, sender, panel, thread, snapshot, renderSourceMode); err != nil {
		return err
	}
	if err := s.maybeSendPlanPromptNotice(ctx, sender, panel, thread, effectivePlanPrompt(pending, snapshot)); err != nil {
		return err
	}
	summaryMessage, summaryButtons, summaryHash := s.renderSummaryPanel(ctx, thread, snapshot, pending)
	if summaryHash != panel.LastSummaryHash {
		s.logChatRenderedMessagesContainsNil(thread.ID, snapshot.LatestTurnID, "summary", panel.SummaryMessageID, []model.RenderedMessage{summaryMessage})
		if err := sender.EditRenderedMessage(ctx, panel.ChatID, panel.TopicID, panel.SummaryMessageID, summaryMessage, summaryButtons); err != nil {
			return err
		}
		panel.LastSummaryHash = summaryHash
	}

	messagesChanged := false
	toolText, toolHash, shouldShowTool := s.renderToolPanelIfNeeded(ctx, thread, snapshot)
	if !shouldShowTool {
		if panel.ToolMessageID != 0 {
			_ = sender.DeleteMessage(ctx, panel.ChatID, panel.TopicID, panel.ToolMessageID)
			panel.ToolMessageID = 0
			messagesChanged = true
		}
		panel.LastToolHash = ""
	} else if panel.ToolMessageID == 0 {
		s.logChatRenderContainsNil(thread.ID, snapshot.LatestTurnID, "tool", 0, toolText)
		toolID, err := sender.SendMessage(ctx, panel.ChatID, panel.TopicID, toolText, nil, silentSendOptions())
		if err != nil {
			return err
		}
		panel.ToolMessageID = toolID
		panel.LastToolHash = toolHash
		messagesChanged = true
		if toolID != 0 {
			_ = s.store.PutMessageRoute(ctx, model.MessageRoute{ChatID: panel.ChatID, TopicID: panel.TopicID, MessageID: toolID, ThreadID: thread.ID, TurnID: snapshot.LatestTurnID, CreatedAt: model.NowString()})
		}
	} else if toolHash != panel.LastToolHash {
		s.logChatRenderContainsNil(thread.ID, snapshot.LatestTurnID, "tool", panel.ToolMessageID, toolText)
		if err := sender.EditMessage(ctx, panel.ChatID, panel.TopicID, panel.ToolMessageID, toolText, nil); err != nil {
			return err
		}
		panel.LastToolHash = toolHash
	}

	outputText, outputHash, shouldShowOutput := s.renderOutputPanelIfNeeded(ctx, thread, snapshot)
	if !shouldShowOutput {
		if panel.OutputMessageID != 0 {
			_ = sender.DeleteMessage(ctx, panel.ChatID, panel.TopicID, panel.OutputMessageID)
			panel.OutputMessageID = 0
			messagesChanged = true
		}
		panel.LastOutputHash = ""
	} else if panel.OutputMessageID == 0 {
		s.logChatRenderContainsNil(thread.ID, snapshot.LatestTurnID, "output", 0, outputText)
		outputID, err := sender.SendMessage(ctx, panel.ChatID, panel.TopicID, outputText, nil, silentSendOptions())
		if err != nil {
			return err
		}
		panel.OutputMessageID = outputID
		panel.LastOutputHash = outputHash
		messagesChanged = true
		if outputID != 0 {
			_ = s.store.PutMessageRoute(ctx, model.MessageRoute{ChatID: panel.ChatID, TopicID: panel.TopicID, MessageID: outputID, ThreadID: thread.ID, TurnID: snapshot.LatestTurnID, CreatedAt: model.NowString()})
		}
	} else if outputHash != panel.LastOutputHash {
		s.logChatRenderContainsNil(thread.ID, snapshot.LatestTurnID, "output", panel.OutputMessageID, outputText)
		if err := sender.EditMessage(ctx, panel.ChatID, panel.TopicID, panel.OutputMessageID, outputText, nil); err != nil {
			return err
		}
		panel.LastOutputHash = outputHash
	}
	if messagesChanged {
		if err := s.store.UpdateThreadPanelMessages(ctx, panel.ID, panel.SummaryMessageID, panel.ToolMessageID, panel.OutputMessageID); err != nil {
			return err
		}
	}

	panel.CurrentTurnID = snapshot.LatestTurnID
	panel.Status = snapshot.LatestTurnStatus
	if isTerminalStatus(cardDisplayStatus(snapshot, thread)) {
		if _, fp := terminalCardTextAndFP(snapshot); strings.TrimSpace(fp) != "" {
			panel.LastFinalNoticeFP = strings.TrimSpace(fp)
			panel.LastFinalCardHash = panel.LastSummaryHash
			panel.DetailsViewJSON = model.MustJSON(model.DetailsViewState{})
			return s.store.UpdateThreadPanelFinalCard(ctx, panel.ID, panel.SummaryMessageID, panel.CurrentTurnID, panel.Status, panel.LastSummaryHash, panel.LastToolHash, panel.LastOutputHash, panel.LastFinalNoticeFP, panel.DetailsViewJSON, panel.LastFinalCardHash)
		}
	}
	return s.store.UpdateThreadPanelState(ctx, panel.ID, panel.CurrentTurnID, panel.Status, panel.LastSummaryHash, panel.LastToolHash, panel.LastOutputHash, panel.LastFinalNoticeFP)
}

func (s *Service) renderSummaryPanel(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, pending *model.PendingApproval) (model.RenderedMessage, [][]model.ButtonSpec, string) {
	pending = pendingForSnapshot(pending, snapshot)
	buttons := [][]model.ButtonSpec{}
	if !isTerminalStatus(cardDisplayStatus(snapshot, thread)) {
		buttons = append(buttons, []model.ButtonSpec{
			s.callbackButton(ctx, s.t(ctx, "停止", "Stop"), "stop_turn", thread.ID, snapshot.LatestTurnID, "", nil),
			s.callbackButton(ctx, s.t(ctx, "引导", "Steer"), "arm_steer", thread.ID, snapshot.LatestTurnID, "", nil),
			s.callbackButton(ctx, s.t(ctx, "查看上下文", "Show context"), "show_context", thread.ID, snapshot.LatestTurnID, "", nil),
		})
		if !compactFeishuTopicCard(ctx) {
			buttons = append(buttons, []model.ButtonSpec{
				s.callbackButton(ctx, s.t(ctx, "查看", "Show"), "show_thread", thread.ID, snapshot.LatestTurnID, "", nil),
				s.callbackButton(ctx, s.t(ctx, "获取线程 ID", "Get thread id"), "get_thread_id", thread.ID, snapshot.LatestTurnID, "", nil),
			})
		}
	}
	if pending != nil {
		switch pending.PromptKind {
		case "approval":
			buttons = append(buttons,
				[]model.ButtonSpec{
					s.callbackButton(ctx, s.t(ctx, "批准", "Approve"), "approve", pending.ThreadID, pending.TurnID, pending.RequestID, nil),
					s.callbackButton(ctx, s.t(ctx, "批准会话", "Approve Session"), "approve_session", pending.ThreadID, pending.TurnID, pending.RequestID, nil),
				},
				[]model.ButtonSpec{
					s.callbackButton(ctx, s.t(ctx, "拒绝", "Deny"), "deny", pending.ThreadID, pending.TurnID, pending.RequestID, nil),
					s.callbackButton(ctx, s.t(ctx, "取消", "Cancel"), "cancel", pending.ThreadID, pending.TurnID, pending.RequestID, nil),
				},
			)
		case "user_input":
			if optionRows := s.pendingInputButtons(ctx, pending); len(optionRows) > 0 {
				buttons = append(buttons, optionRows...)
			}
		}
	}
	entries := summaryPanelEntries(snapshot)
	now := time.Now().UTC()
	for {
		rendered := s.renderSummaryPanelMarkdownAt(ctx, thread, snapshot, entries, pending, now)
		if len(rendered) <= 1 {
			message := firstRenderedMessage(rendered)
			return message, buttons, hashStrings(msgformat.HashRendered(message), flattenButtonSpecs(buttons))
		}
		if len(entries) == 0 {
			message := firstRenderedMessage(rendered)
			return message, buttons, hashStrings(msgformat.HashRendered(message), flattenButtonSpecs(buttons))
		}
		entries = entries[:len(entries)-1]
	}
}

func summaryPanelEntries(snapshot *appserver.ThreadReadSnapshot) []appserver.AgentMessageEntry {
	if snapshot == nil {
		return nil
	}
	if entries := agentEntriesFromDetailCommentary(snapshot.DetailItems); len(entries) > 0 {
		return entries
	}
	return append([]appserver.AgentMessageEntry(nil), snapshot.LatestAgentMessageEntries...)
}

func agentEntriesFromDetailCommentary(items []model.DetailItem) []appserver.AgentMessageEntry {
	entries := make([]appserver.AgentMessageEntry, 0, len(items))
	for _, item := range items {
		if item.Kind != model.DetailItemCommentary {
			continue
		}
		text := strings.TrimSpace(item.Text)
		if text == "" {
			continue
		}
		entries = append([]appserver.AgentMessageEntry{{
			ID:    item.ID,
			Phase: item.Phase,
			Text:  text,
			FP:    item.FP,
		}}, entries...)
	}
	return entries
}

func (s *Service) renderSummaryPanelMarkdown(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, entries []appserver.AgentMessageEntry, pending *model.PendingApproval) []model.RenderedMessage {
	return s.renderSummaryPanelMarkdownAt(ctx, thread, snapshot, entries, pending, time.Now().UTC())
}

func (s *Service) renderSummaryPanelMarkdownAt(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, entries []appserver.AgentMessageEntry, pending *model.PendingApproval, now time.Time) []model.RenderedMessage {
	pending = pendingForSnapshot(pending, snapshot)
	status := cardDisplayStatus(snapshot, thread)
	statusText := readableProgressStatusLang(s.botLanguage(ctx), status)
	progressSegments := []msgformat.Segment{}
	if detailSegments, ok := s.summaryDetailTimelineSegments(ctx, snapshot); ok {
		progressSegments = append(progressSegments, detailSegments...)
	} else if statusLogText := s.interruptedStatusLogText(ctx, snapshot); statusLogText != "" {
		progressSegments = append(progressSegments, msgformat.Markdown(statusLogText))
	} else if len(entries) == 0 {
		progressSegments = append(progressSegments, msgformat.Markdown(s.t(ctx, "还没有 agent 消息。", "No agent messages yet.")))
	} else {
		displayEntries := chronologicalAgentEntries(entries)
		for _, message := range displayEntries {
			text := strings.TrimSpace(cleanNilLiteral(message.Text))
			if text == "" {
				continue
			}
			progressSegments = append(progressSegments,
				summaryAgentPrefix(message),
				msgformat.Markdown(text),
			)
		}
	}
	segments := []msgformat.Segment{}
	if pending != nil {
		switch pending.PromptKind {
		case "approval":
			segments = append(segments, msgformat.Plain(s.t(ctx, "\n\n[审批]\n", "\n\n[Approval]\n")), msgformat.Markdown(strings.TrimSpace(cleanNilLiteral(pending.Question))))
		case "user_input":
			segments = append(segments, msgformat.Plain(s.t(ctx, "\n\n[问题]\n", "\n\n[Question]\n")), msgformat.Markdown(strings.TrimSpace(cleanNilLiteral(pending.Question))))
		}
	} else if snapshot != nil && snapshot.PlanPrompt != nil {
		segments = append(segments, msgformat.Plain(s.t(ctx, "\n\n[Plan]\n等待输入。请回复 [Plan] 消息或使用 /reply。", "\n\n[Plan]\nWaiting for input. Reply to the [Plan] message or use /reply.")))
	}
	if line := s.runTimingFooter(ctx, snapshot, now); line != "" {
		progressSegments = append(progressSegments, msgformat.Plain("\n\n"+line))
	}
	progress := strings.TrimSpace(firstRenderedMessage(msgformat.RenderSegments(progressSegments, msgformat.MessageLimit)).Text)
	finalText, _ := terminalCardTextAndFP(snapshot)
	finalText = strings.TrimSpace(finalText)
	if finalText != "" && isTerminalStatus(status) {
		segments = append(segments, msgformat.Markdown(finalText))
	}
	displaySegments := []msgformat.Segment{msgformat.Markdown(statusText), msgformat.Plain("\n\n")}
	displaySegments = append(displaySegments, progressSegments...)
	if len(segments) > 0 {
		displaySegments = append(displaySegments, msgformat.Plain("\n\n"))
	}
	displaySegments = append(displaySegments, segments...)
	message := firstRenderedMessage(msgformat.RenderSegments(displaySegments, msgformat.MessageLimit))
	message.Style = model.MessageStyleCodexPanel
	message.CodexStatus = statusText
	message.CodexProgressMarkdown = progress
	message.CodexFinalMarkdown = finalText
	message.CodexProgressExpanded = !isTerminalStatus(status)
	if finalText == "" || !isTerminalStatus(status) {
		message.CodexFinalMarkdown = ""
	}
	return []model.RenderedMessage{message}
}

func (s *Service) summaryDetailTimelineSegments(ctx context.Context, snapshot *appserver.ThreadReadSnapshot) ([]msgformat.Segment, bool) {
	if snapshot == nil || len(snapshot.DetailItems) == 0 {
		return nil, false
	}
	segments := []msgformat.Segment{}
	for _, item := range snapshot.DetailItems {
		switch item.Kind {
		case model.DetailItemCommentary:
			text := strings.TrimSpace(cleanNilLiteral(item.Text))
			if text == "" {
				continue
			}
			segments = appendStatusTimelineBlock(segments, s.t(ctx, "思考中...", "Thinking..."), text)
		case model.DetailItemTool:
			text := summaryToolDetailText(item)
			if text == "" {
				continue
			}
			segments = appendStatusTimelineBlock(segments, s.t(ctx, "工具调用中...", "Using tools..."), text)
		case model.DetailItemOutput:
			text := summaryOutputDetailText(item)
			if text == "" {
				continue
			}
			segments = appendStatusTimelineBlock(segments, s.t(ctx, "工具调用中...", "Using tools..."), text)
		}
	}
	if len(segments) > 0 {
		return segments, true
	}
	return nil, false
}

func appendStatusTimelineBlock(segments []msgformat.Segment, status, body string) []msgformat.Segment {
	return append(segments,
		msgformat.Plain("\n\n"),
		msgformat.Markdown(status),
		msgformat.Plain("\n"),
		msgformat.Markdown(body),
	)
}

func summaryToolDetailText(item model.DetailItem) string {
	parts := []string{}
	if label := strings.TrimSpace(cleanNilLiteral(item.Label)); label != "" {
		parts = append(parts, label)
	}
	if text := strings.TrimSpace(cleanNilLiteral(item.Text)); text != "" && text != strings.TrimSpace(cleanNilLiteral(item.Label)) {
		parts = append(parts, text)
	}
	if output := strings.TrimSpace(cleanNilLiteral(item.Output)); output != "" {
		parts = append(parts, output)
	}
	return strings.Join(parts, "\n")
}

func summaryOutputDetailText(item model.DetailItem) string {
	for _, value := range []string{item.Output, item.Text, item.Label} {
		text := strings.TrimSpace(cleanNilLiteral(value))
		if text != "" {
			return text
		}
	}
	return ""
}

func (s *Service) interruptedStatusLogText(ctx context.Context, snapshot *appserver.ThreadReadSnapshot) string {
	if snapshot == nil || !strings.EqualFold(strings.TrimSpace(snapshot.LatestTurnStatus), "interrupted") || snapshotHasFinalSignal(snapshot) {
		return ""
	}
	if snapshotHasToolSignal(snapshot) {
		return s.t(ctx, "工具调用中...", "Using tools...")
	}
	if snapshotHasCommentarySignal(snapshot) {
		return s.t(ctx, "思考中...", "Thinking...")
	}
	if strings.TrimSpace(snapshot.LatestProgressText) != "" || strings.TrimSpace(snapshot.LatestProgressFP) != "" {
		return s.t(ctx, "运行中...", "Processing...")
	}
	return s.t(ctx, "运行中...", "Processing...")
}

func cardDisplayStatus(snapshot *appserver.ThreadReadSnapshot, thread model.Thread) string {
	if snapshot == nil {
		return thread.Status
	}
	status := strings.TrimSpace(snapshot.LatestTurnStatus)
	if strings.EqualFold(status, "interrupted") {
		if snapshotHasFinalSignal(snapshot) {
			return "completed"
		}
		if snapshotHasToolSignal(snapshot) {
			return "toolRunning"
		}
		if snapshotHasProgressSignal(snapshot) {
			return "inProgress"
		}
		return "inProgress"
	}
	return status
}

func snapshotHasProgressSignal(snapshot *appserver.ThreadReadSnapshot) bool {
	if snapshot == nil {
		return false
	}
	if strings.TrimSpace(snapshot.LatestProgressText) != "" || len(snapshot.LatestAgentMessageEntries) > 0 {
		return true
	}
	return snapshotHasCommentarySignal(snapshot)
}

func snapshotHasCommentarySignal(snapshot *appserver.ThreadReadSnapshot) bool {
	if snapshot == nil {
		return false
	}
	if len(snapshot.LatestAgentMessageEntries) > 0 {
		return true
	}
	for _, item := range snapshot.DetailItems {
		if item.Kind == model.DetailItemCommentary && strings.TrimSpace(item.Text) != "" {
			return true
		}
	}
	return false
}

func snapshotHasToolSignal(snapshot *appserver.ThreadReadSnapshot) bool {
	if snapshot == nil {
		return false
	}
	if strings.TrimSpace(snapshot.LatestToolID) != "" || strings.TrimSpace(snapshot.LatestToolLabel) != "" || strings.TrimSpace(snapshot.LatestToolStatus) != "" {
		return true
	}
	for _, item := range snapshot.DetailItems {
		if (item.Kind == model.DetailItemTool || item.Kind == model.DetailItemOutput) &&
			(strings.TrimSpace(item.Text) != "" ||
				strings.TrimSpace(item.Label) != "" ||
				strings.TrimSpace(item.Status) != "" ||
				strings.TrimSpace(item.Output) != "") {
			return true
		}
	}
	return false
}

func (s *Service) renderPlanPromptCard(ctx context.Context, thread model.Thread, prompt *model.PlanPrompt) (model.RenderedMessage, [][]model.ButtonSpec, string) {
	header := strings.Join([]string{
		s.visualHeader(ctx, "Plan", thread, prompt.TurnID),
		fmt.Sprintf("%s: %s", s.t(ctx, "状态", "Status"), readableStatusLang(s.botLanguage(ctx), firstNonEmpty(prompt.Status, "waiting for input"), "")),
	}, "\n")
	body := strings.TrimSpace(prompt.Question) + "\n\n" + fmt.Sprintf(s.t(ctx, "回复这条消息，或使用 /reply %s <文本>。", "Reply to this message or use /reply %s <text>."), thread.ID)
	message := renderSingleMarkdownCard(header, body)
	buttons := s.planPromptButtons(ctx, prompt)
	return message, buttons, hashStrings(msgformat.HashRendered(message), flattenButtonSpecs(buttons))
}

func (s *Service) planPromptButtons(ctx context.Context, prompt *model.PlanPrompt) [][]model.ButtonSpec {
	if prompt == nil || len(prompt.Options) == 0 {
		return nil
	}
	rows := make([][]model.ButtonSpec, 0, len(prompt.Options))
	for _, option := range prompt.Options {
		option = strings.TrimSpace(option)
		if option == "" {
			continue
		}
		rows = append(rows, []model.ButtonSpec{
			s.callbackButton(ctx, option, "answer_choice", prompt.ThreadID, prompt.TurnID, prompt.RequestID, map[string]any{"text": option}),
		})
	}
	return rows
}

func chronologicalAgentEntries(entries []appserver.AgentMessageEntry) []appserver.AgentMessageEntry {
	out := make([]appserver.AgentMessageEntry, 0, len(entries))
	for index := len(entries) - 1; index >= 0; index-- {
		out = append(out, entries[index])
	}
	return out
}

func (s *Service) renderToolPanel(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot) (string, string) {
	return s.renderToolPanelAt(ctx, thread, snapshot, time.Now().UTC())
}

func (s *Service) renderToolPanelIfNeeded(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot) (string, string, bool) {
	if current, ok := s.currentChatOriginTool(ctx, thread, snapshot); ok && strings.TrimSpace(cleanNilLiteral(current.Label)) != "" {
		text, hash := s.renderToolPanel(ctx, thread, snapshot)
		return text, hash, true
	}
	tool, ok := lastCompletedTool(snapshot)
	if !ok || strings.TrimSpace(cleanNilLiteral(tool.Label)) == "" {
		return "", "", false
	}
	text, hash := s.renderToolPanel(ctx, thread, snapshot)
	return text, hash, true
}

func (s *Service) renderToolPanelAt(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, now time.Time) (string, string) {
	header := s.visualHeader(ctx, s.t(ctx, "工具", "Tool"), thread, snapshot.LatestTurnID)
	if current, ok := s.currentChatOriginTool(ctx, thread, snapshot); ok {
		escapedHeader := html.EscapeString(header)
		renderedTool := renderToolCommandBlock(current.Label, outputMessageLimit-len(escapedHeader)-2)
		lines := []string{escapedHeader, s.t(ctx, "当前工具：", "Current tool:"), renderedTool}
		if status := strings.TrimSpace(current.Status); status != "" {
			lines = append(lines, html.EscapeString(fmt.Sprintf("%s: %s", s.t(ctx, "状态", "Status"), readableStatusLang(s.botLanguage(ctx), status, ""))))
		}
		text := strings.Join(lines, "\n")
		return text, hashStrings(text)
	}

	tool, _ := lastCompletedTool(snapshot)
	label := strings.TrimSpace(cleanNilLiteral(tool.Label))
	if label == "" {
		lines := []string{header, s.t(ctx, "还没有已完成的工具。", "No completed tool yet.")}
		text := strings.Join(lines, "\n")
		return text, hashStrings(text)
	}

	escapedHeader := html.EscapeString(header)
	renderedTool := renderToolCommandBlock(label, outputMessageLimit-len(escapedHeader)-2)
	lines := []string{escapedHeader, s.t(ctx, "最近完成的工具：", "Last completed tool:"), renderedTool}
	if status := strings.TrimSpace(tool.Status); status != "" {
		lines = append(lines, html.EscapeString(fmt.Sprintf("%s: %s", s.t(ctx, "状态", "Status"), readableStatusLang(s.botLanguage(ctx), status, ""))))
	}
	text := strings.Join(lines, "\n")
	return text, hashStrings(text)
}

func (s *Service) currentChatOriginTool(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot) (completedToolView, bool) {
	if snapshot == nil || !snapshot.LatestToolLiveCurrent {
		return completedToolView{}, false
	}
	turnID := strings.TrimSpace(snapshot.LatestTurnID)
	if turnID == "" || isTerminalStatus(snapshot.LatestTurnStatus) || terminalToolStatus(snapshot.LatestToolStatus) {
		return completedToolView{}, false
	}
	threadID := firstNonEmpty(strings.TrimSpace(thread.ID), strings.TrimSpace(snapshot.Thread.ID))
	if threadID == "" || !s.isDirectInputOriginTurn(ctx, threadID, turnID) {
		return completedToolView{}, false
	}
	label := strings.TrimSpace(cleanNilLiteral(snapshot.LatestToolLabel))
	if label == "" {
		return completedToolView{}, false
	}
	return completedToolView{
		ID:     strings.TrimSpace(snapshot.LatestToolID),
		Label:  label,
		Status: strings.TrimSpace(snapshot.LatestToolStatus),
		Output: snapshot.LatestToolOutput,
	}, true
}

func runTimingFooter(snapshot *appserver.ThreadReadSnapshot, now time.Time) string {
	if snapshot == nil {
		return ""
	}
	startedAt := parseTime(model.TimeString(snapshot.LatestTurnStartedAt))
	if startedAt.IsZero() {
		return ""
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if isTerminalStatus(snapshot.LatestTurnStatus) {
		endedAt := parseTime(model.TimeString(snapshot.LatestTurnUpdatedAt))
		if endedAt.IsZero() {
			endedAt = now
		}
		return fmt.Sprintf("Run duration: %s", formatToolDuration(endedAt.Sub(startedAt)))
	}
	return fmt.Sprintf("Run active for: %s", formatToolDuration(now.Sub(startedAt)))
}

func (s *Service) runTimingFooter(ctx context.Context, snapshot *appserver.ThreadReadSnapshot, now time.Time) string {
	line := runTimingFooter(snapshot, now)
	if normalizeBotLanguage(s.botLanguage(ctx)) == botLanguageEnglish || line == "" {
		return line
	}
	if strings.HasPrefix(line, "Run duration: ") {
		return "运行耗时：" + strings.TrimPrefix(line, "Run duration: ")
	}
	if strings.HasPrefix(line, "Run active for: ") {
		return "已运行：" + strings.TrimPrefix(line, "Run active for: ")
	}
	return line
}

func formatToolDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	seconds := int(duration.Truncate(time.Second).Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	seconds = seconds % 60
	if minutes < 60 {
		if seconds == 0 {
			return fmt.Sprintf("%dm", minutes)
		}
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	}
	hours := minutes / 60
	minutes = minutes % 60
	if hours < 48 {
		return fmt.Sprintf("%dh %02dm", hours, minutes)
	}
	days := hours / 24
	hours = hours % 24
	return fmt.Sprintf("%dd %02dh", days, hours)
}

func (s *Service) renderOutputPanel(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot) (string, string) {
	header := s.visualHeader(ctx, s.t(ctx, "输出", "Output"), thread, snapshot.LatestTurnID)
	tool, _ := lastCompletedTool(snapshot)
	output := strings.ReplaceAll(tool.Output, "\r\n", "\n")
	output = cleanNilLiteral(output)
	output = strings.TrimSpace(output)
	if output == "" {
		text := strings.Join([]string{header, s.t(ctx, "还没有已完成的工具输出。", "No completed tool output yet.")}, "\n")
		return text, hashStrings(text)
	}

	escapedHeader := html.EscapeString(header)
	prefix := strings.Join([]string{escapedHeader, s.t(ctx, "最近完成的输出：", "Last completed output:")}, "\n")
	text := strings.Join([]string{
		prefix,
		renderHTMLCodeBlockTail(trimOutputTail(output, outputMessageLimit-len(prefix)-1), outputMessageLimit-len(prefix)-1, ""),
	}, "\n")
	return text, hashStrings(text)
}

func (s *Service) renderOutputPanelIfNeeded(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot) (string, string, bool) {
	tool, ok := lastCompletedTool(snapshot)
	if !ok {
		return "", "", false
	}
	output := strings.ReplaceAll(tool.Output, "\r\n", "\n")
	output = cleanNilLiteral(output)
	if strings.TrimSpace(output) == "" {
		return "", "", false
	}
	text, hash := s.renderOutputPanel(ctx, thread, snapshot)
	return text, hash, true
}

type completedToolView struct {
	ID     string
	Label  string
	Status string
	Output string
}

func lastCompletedTool(snapshot *appserver.ThreadReadSnapshot) (completedToolView, bool) {
	if snapshot == nil {
		return completedToolView{}, false
	}
	outputByToolID := make(map[string]string)
	for _, item := range snapshot.DetailItems {
		if item.Kind != model.DetailItemOutput {
			continue
		}
		if id := strings.TrimSuffix(strings.TrimSpace(item.ID), ":output"); id != "" {
			outputByToolID[id] = item.Output
		}
	}
	for i := len(snapshot.DetailItems) - 1; i >= 0; i-- {
		item := snapshot.DetailItems[i]
		if item.Kind != model.DetailItemTool || !terminalToolStatus(item.Status) {
			continue
		}
		label := strings.TrimSpace(cleanNilLiteral(item.Label))
		if label == "" {
			continue
		}
		id := strings.TrimSpace(item.ID)
		return completedToolView{
			ID:     id,
			Label:  label,
			Status: item.Status,
			Output: outputByToolID[id],
		}, true
	}
	if terminalToolStatus(snapshot.LatestToolStatus) {
		label := strings.TrimSpace(cleanNilLiteral(snapshot.LatestToolLabel))
		if label != "" {
			return completedToolView{
				ID:     strings.TrimSpace(snapshot.LatestToolID),
				Label:  label,
				Status: snapshot.LatestToolStatus,
				Output: snapshot.LatestToolOutput,
			}, true
		}
	}
	return completedToolView{}, false
}

func renderToolCommandBlock(label string, maxLen int) string {
	tool := parseShellTool(label)
	if tool.ShellName == "" {
		return renderHTMLCodeBlockTail(label, maxLen, "")
	}
	header := fmt.Sprintf("[Shell:%s", html.EscapeString(tool.ShellName))
	if tool.DisplayName != "" {
		header += fmt.Sprintf(" (%s)", html.EscapeString(tool.DisplayName))
	} else {
		header += " (⚠️UNKNOWN SHELL⚠️)"
	}
	header += "]"
	codeBudget := maxLen - len(header) - 1
	return strings.Join([]string{
		header,
		renderHTMLCodeBlockTail(tool.Command, codeBudget, tool.Language),
	}, "\n")
}

type shellTool struct {
	ShellName   string
	DisplayName string
	Language    string
	Command     string
}

func parseShellTool(label string) shellTool {
	tokens := splitShellCommandLine(label)
	if len(tokens) < 2 {
		return shellTool{}
	}
	shellName := shellBaseName(tokens[0])
	flagIndex := -1
	for index := 1; index < len(tokens); index++ {
		if isShellCommandFlag(tokens[index]) {
			flagIndex = index
			break
		}
	}
	if flagIndex < 0 || flagIndex+1 >= len(tokens) {
		return shellTool{}
	}
	command := strings.TrimSpace(strings.Join(tokens[flagIndex+1:], " "))
	if command == "" {
		return shellTool{}
	}
	displayName, language, known := knownShell(shellName)
	if !known && !looksLikeExecutableShell(shellName) {
		return shellTool{}
	}
	return shellTool{
		ShellName:   shellName,
		DisplayName: displayName,
		Language:    language,
		Command:     command,
	}
}

func splitShellCommandLine(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	tokens := []string{}
	current := strings.Builder{}
	inQuotes := false
	escaped := false
	for _, r := range value {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\' && inQuotes:
			current.WriteRune(r)
		case r == '"':
			inQuotes = !inQuotes
		case !inQuotes && (r == ' ' || r == '\t' || r == '\n'):
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

func shellBaseName(path string) string {
	path = strings.Trim(strings.TrimSpace(path), `"`)
	path = strings.ReplaceAll(path, "/", "\\")
	if index := strings.LastIndex(path, "\\"); index >= 0 {
		return strings.TrimSpace(path[index+1:])
	}
	return path
}

func isShellCommandFlag(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "-command", "-c", "/c":
		return true
	default:
		return false
	}
}

func knownShell(shellName string) (string, string, bool) {
	switch strings.ToLower(strings.TrimSpace(shellName)) {
	case "pwsh", "pwsh.exe", "powershell", "powershell.exe":
		return "PowerShell", "powershell", true
	case "bash", "bash.exe", "sh", "sh.exe", "zsh", "zsh.exe":
		return "Bash", "bash", true
	case "cmd", "cmd.exe":
		return "Command Prompt", "batch", true
	default:
		return "", "", false
	}
}

func looksLikeExecutableShell(shellName string) bool {
	shellName = strings.TrimSpace(shellName)
	return strings.Contains(shellName, ".") || strings.HasSuffix(strings.ToLower(shellName), "sh")
}

func renderHTMLCodeBlock(content, language string) string {
	language = strings.TrimSpace(strings.ToLower(language))
	if language != "" {
		return fmt.Sprintf(`<pre><code class="language-%s">%s</code></pre>`, html.EscapeString(language), html.EscapeString(content))
	}
	return fmt.Sprintf("<pre><code>%s</code></pre>", html.EscapeString(content))
}

func renderHTMLCodeBlockTail(content string, maxLen int, language string) string {
	content = strings.TrimSpace(content)
	if maxLen <= len("<pre><code></code></pre>") {
		return renderHTMLCodeBlock("", language)
	}
	runes := []rune(content)
	if len(runes) == 0 {
		return renderHTMLCodeBlock("", language)
	}
	if block := renderHTMLCodeBlock(content, language); len(block) <= maxLen {
		return block
	}
	bestStart := len(runes)
	low, high := 0, len(runes)
	for low <= high {
		mid := (low + high) / 2
		candidate := string(runes[mid:])
		if len(renderHTMLCodeBlock(candidate, language)) <= maxLen {
			bestStart = mid
			high = mid - 1
		} else {
			low = mid + 1
		}
	}
	tail := string(runes[bestStart:])
	if bestStart > 0 {
		if newline := strings.Index(tail, "\n"); newline >= 0 && newline+1 < len(tail) {
			tail = tail[newline+1:]
		}
	}
	return renderHTMLCodeBlock(strings.TrimSpace(tail), language)
}

func formatSummaryAgentMessage(entry appserver.AgentMessageEntry) string {
	text := strings.TrimSpace(entry.Text)
	if text == "" {
		return ""
	}
	phase := strings.TrimSpace(strings.ToLower(entry.Phase))
	switch phase {
	case "", "message", "final_answer":
		return text
	default:
		return fmt.Sprintf("(%s) %s", phase, text)
	}
}

func summaryAgentLabel(entry appserver.AgentMessageEntry) string {
	phase := strings.ToLower(cleanPayloadString(entry.Phase))
	switch phase {
	case "":
		return "[agent]"
	case "final_answer":
		return "[final]"
	default:
		return fmt.Sprintf("[%s]", phase)
	}
}

func summaryAgentPrefix(entry appserver.AgentMessageEntry) msgformat.Segment {
	phase := strings.ToLower(cleanPayloadString(entry.Phase))
	switch phase {
	case "", "message", "commentary":
		return msgformat.Plain("\n\n")
	default:
		return msgformat.Plain(fmt.Sprintf("\n\n%s\n", summaryAgentLabel(entry)))
	}
}

func firstRenderedMessage(messages []model.RenderedMessage) model.RenderedMessage {
	if len(messages) == 0 {
		return model.RenderedMessage{Text: " "}
	}
	return messages[0]
}

func lastMessageID(ids []int64) int64 {
	if len(ids) == 0 {
		return 0
	}
	return ids[len(ids)-1]
}

func firstMessageID(ids []int64) int64 {
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

func trimOutputTail(output string, limit int) string {
	if limit <= 0 || len(output) <= limit {
		return output
	}
	window := output[len(output)-limit:]
	if newline := strings.Index(window, "\n"); newline >= 0 && newline+1 < len(window) {
		window = window[newline+1:]
	}
	return strings.TrimSpace(window)
}

func readableStatus(turnStatus, threadStatus string) string {
	if status := cleanPayloadString(turnStatus); status != "" {
		return status
	}
	if status := cleanPayloadString(threadStatus); status != "" {
		return status
	}
	return "idle"
}

func (s *Service) pendingInputButtons(ctx context.Context, pending *model.PendingApproval) [][]model.ButtonSpec {
	options := pendingInputOptions(pending)
	if len(options) == 0 {
		return nil
	}
	rows := make([][]model.ButtonSpec, 0, len(options))
	for _, option := range options {
		rows = append(rows, []model.ButtonSpec{
			s.callbackButton(ctx, option, "answer_choice", pending.ThreadID, pending.TurnID, pending.RequestID, map[string]any{"text": option}),
		})
	}
	return rows
}

func pendingInputOptions(pending *model.PendingApproval) []string {
	if pending == nil || strings.TrimSpace(pending.PayloadJSON) == "" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(pending.PayloadJSON), &payload); err != nil {
		return nil
	}
	return extractChoiceOptions(payload)
}

func effectivePlanPrompt(pending *model.PendingApproval, snapshot *appserver.ThreadReadSnapshot) *model.PlanPrompt {
	pending = pendingForSnapshot(pending, snapshot)
	if pending != nil && pending.PromptKind == "user_input" {
		options := pendingInputOptions(pending)
		fp := hashStrings("planPrompt", model.PromptSourceServerRequest, pending.RequestID, pending.ThreadID, pending.TurnID, pending.Question, strings.Join(options, "\x1f"))
		return &model.PlanPrompt{
			PromptID:    "request:" + pending.RequestID,
			Source:      model.PromptSourceServerRequest,
			ThreadID:    pending.ThreadID,
			TurnID:      pending.TurnID,
			ItemID:      pending.ItemID,
			RequestID:   pending.RequestID,
			Question:    firstNonEmpty(pending.Question, "Input required."),
			Options:     options,
			Fingerprint: fp,
			Status:      "waiting for input",
		}
	}
	if snapshot != nil && snapshot.PlanPrompt != nil {
		return snapshot.PlanPrompt
	}
	return nil
}

func pendingForSnapshot(pending *model.PendingApproval, snapshot *appserver.ThreadReadSnapshot) *model.PendingApproval {
	if pending == nil {
		return nil
	}
	if snapshot == nil {
		return pending
	}
	pendingTurnID := strings.TrimSpace(pending.TurnID)
	snapshotTurnID := strings.TrimSpace(snapshot.LatestTurnID)
	if pendingTurnID != "" && snapshotTurnID != "" && pendingTurnID != snapshotTurnID {
		return nil
	}
	return pending
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := cleanPayloadString(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func extractChoiceOptions(payload map[string]any) []string {
	keys := []string{"choices", "options", "suggestions", "responses"}
	for _, key := range keys {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		items, ok := raw.([]any)
		if !ok {
			continue
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			switch typed := item.(type) {
			case string:
				if text := cleanPayloadString(typed); text != "" {
					out = append(out, text)
				}
			case map[string]any:
				if text := firstPayloadString(typed, "label", "text", "value"); text != "" {
					out = append(out, text)
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if questions, ok := payload["questions"].([]any); ok {
		out := make([]string, 0)
		seen := map[string]bool{}
		for _, rawQuestion := range questions {
			question, _ := rawQuestion.(map[string]any)
			if question == nil {
				continue
			}
			options, _ := question["options"].([]any)
			for _, rawOption := range options {
				option, _ := rawOption.(map[string]any)
				if option == nil {
					continue
				}
				label := firstPayloadString(option, "label", "text", "value")
				if label == "" || seen[label] {
					continue
				}
				seen[label] = true
				out = append(out, label)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func (s *Service) armSteer(ctx context.Context, chatID, topicID int64, threadID, turnID string, panelID int64) error {
	return s.store.ArmSteerState(ctx, model.SteerState{
		ChatKey:   model.ChatKey(chatID, topicID),
		ChatID:    chatID,
		TopicID:   topicID,
		ThreadID:  threadID,
		TurnID:    turnID,
		PanelID:   panelID,
		ExpiresAt: model.TimeString(time.Now().UTC().Add(steerTTL).Format(time.RFC3339Nano)),
		CreatedAt: model.NowString(),
		UpdatedAt: model.NowString(),
	})
}

func (s *Service) resolveArmedSteer(ctx context.Context, chatID, topicID int64) (*model.SteerState, error) {
	state, err := s.store.GetSteerState(ctx, chatID, topicID)
	if err != nil || state == nil {
		return state, err
	}
	if expiresAt := parseTime(state.ExpiresAt); !expiresAt.IsZero() && time.Now().UTC().After(expiresAt) {
		_ = s.store.ClearSteerState(ctx, chatID, topicID)
		return nil, nil
	}
	return state, nil
}

func sanitizeFileName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "thread"
	}
	replacer := strings.NewReplacer("\\", "_", "/", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", " ", "_")
	return replacer.Replace(value)
}

func hashStrings(parts ...string) string {
	hasher := sha1.New()
	for _, part := range parts {
		_, _ = hasher.Write([]byte(part))
		_, _ = hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func flattenButtonSpecs(rows [][]model.ButtonSpec) string {
	out := make([]string, 0, len(rows)*2)
	for _, row := range rows {
		for _, button := range row {
			out = append(out, button.Text)
		}
	}
	return strings.Join(out, "\x1f")
}

func isTerminalStatus(status string) bool {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "completed", "interrupted", "failed", "cancelled", "canceled":
		return true
	default:
		return false
	}
}
