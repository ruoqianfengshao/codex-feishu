package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mideco-tech/codex-tg/internal/appserver"
	"github.com/mideco-tech/codex-tg/internal/model"
	"github.com/mideco-tech/codex-tg/internal/msgformat"
)

const (
	detailsPageSize     = 4
	detailsToolMaxBytes = 2800
)

func (s *Service) maybeRenderFinalCard(ctx context.Context, sender Sender, target model.ObserverTarget, panel *model.ThreadPanel, thread model.Thread, snapshot *appserver.ThreadReadSnapshot) error {
	finalText, finalFP := terminalCardTextAndFP(snapshot)
	if !isTerminalStatus(snapshot.LatestTurnStatus) || strings.TrimSpace(finalText) == "" {
		return nil
	}
	if panel == nil || finalFP == "" || finalFP == panel.LastFinalNoticeFP {
		return nil
	}

	message, buttons, cardHash := s.renderFinalCard(ctx, panel.ID, thread, snapshot)
	s.logChatRenderedMessagesContainsNil(thread.ID, snapshot.LatestTurnID, "final", 0, []model.RenderedMessage{message})
	ids, err := sender.SendRenderedMessages(ctx, panel.ChatID, panel.TopicID, []model.RenderedMessage{message}, buttons, silentSendOptions())
	if err != nil {
		return err
	}
	finalMessageID := lastMessageID(ids)
	_ = s.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    panel.ChatID,
		TopicID:   panel.TopicID,
		MessageID: finalMessageID,
		ThreadID:  thread.ID,
		TurnID:    snapshot.LatestTurnID,
		EventID:   finalFP,
		CreatedAt: model.NowString(),
	})
	panel.CurrentTurnID = snapshot.LatestTurnID
	panel.Status = snapshot.LatestTurnStatus
	panel.LastFinalCardHash = cardHash
	panel.LastFinalNoticeFP = finalFP
	panel.DetailsViewJSON = model.MustJSON(model.DetailsViewState{})
	if err := s.store.UpdateThreadPanelFinalCard(ctx, panel.ID, panel.SummaryMessageID, panel.CurrentTurnID, panel.Status, panel.LastSummaryHash, panel.LastToolHash, panel.LastOutputHash, panel.LastFinalNoticeFP, panel.DetailsViewJSON, panel.LastFinalCardHash); err != nil {
		return err
	}
	if panel.ToolMessageID != 0 {
		_ = sender.DeleteMessage(ctx, panel.ChatID, panel.TopicID, panel.ToolMessageID)
	}
	if panel.OutputMessageID != 0 {
		_ = sender.DeleteMessage(ctx, panel.ChatID, panel.TopicID, panel.OutputMessageID)
	}
	return nil
}

func (s *Service) renderFinalCard(ctx context.Context, panelID int64, thread model.Thread, snapshot *appserver.ThreadReadSnapshot) (model.RenderedMessage, [][]model.ButtonSpec, string) {
	buttons := [][]model.ButtonSpec{}
	if !compactFeishuTopicCard(ctx) {
		buttons = append(buttons, []model.ButtonSpec{
			s.callbackButton(ctx, s.t(ctx, "详情", "Details"), "details_open", thread.ID, snapshot.LatestTurnID, "", map[string]any{
				"panel_id": panelID,
				"page":     0,
			}),
		})
	}
	if !compactFeishuTopicCard(ctx) {
		buttons = append(buttons, []model.ButtonSpec{
			s.callbackButton(ctx, s.t(ctx, "查看", "Show"), "show_thread", thread.ID, snapshot.LatestTurnID, "", nil),
		})
	}
	if s.finalCardShouldShowTurnOffPlan(ctx, thread.ID, snapshot) {
		buttons = append(buttons, []model.ButtonSpec{
			s.callbackButton(ctx, s.t(ctx, "关闭 Plan", "Turn off Plan"), "turn_off_plan", thread.ID, snapshot.LatestTurnID, "", map[string]any{
				"panel_id": panelID,
			}),
		})
	}
	if !compactFeishuTopicCard(ctx) {
		buttons = append(buttons, []model.ButtonSpec{
			s.callbackButton(ctx, s.t(ctx, "获取线程 ID", "Get thread id"), "get_thread_id", thread.ID, snapshot.LatestTurnID, "", nil),
		})
	}
	header := strings.Join([]string{
		s.visualHeader(ctx, s.t(ctx, "最终回复", "Final"), thread, snapshot.LatestTurnID),
		fmt.Sprintf("%s: %s", s.t(ctx, "状态", "Status"), s.t(ctx, "已处理", "Processed")),
	}, "\n")
	body, _ := terminalCardTextAndFP(snapshot)
	body = strings.TrimSpace(body)
	if line := runTimingFooter(snapshot, time.Now().UTC()); line != "" {
		if body != "" {
			body += "\n\n"
		}
		body += line
	}
	message := renderSingleMarkdownCard(header, body)
	return message, buttons, hashStrings(msgformat.HashRendered(message), flattenButtonSpecs(buttons))
}

func terminalCardTextAndFP(snapshot *appserver.ThreadReadSnapshot) (string, string) {
	if snapshot == nil {
		return "", ""
	}
	if text := strings.TrimSpace(snapshot.LatestFinalText); text != "" {
		return text, strings.TrimSpace(snapshot.LatestFinalFP)
	}
	if !isTerminalStatus(snapshot.LatestTurnStatus) {
		return "", ""
	}
	if text, fp := latestTerminalDetailText(snapshot.DetailItems); text != "" {
		return text, firstNonEmpty(fp, hashStrings(snapshot.LatestTurnID, snapshot.LatestTurnStatus, text))
	}
	if text := strings.TrimSpace(snapshot.LatestProgressText); text != "" {
		return text, hashStrings(snapshot.LatestTurnID, snapshot.LatestTurnStatus, snapshot.LatestProgressFP, text)
	}
	return "", ""
}

func latestTerminalDetailText(items []model.DetailItem) (string, string) {
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item.Kind != model.DetailItemFinal {
			continue
		}
		text := strings.TrimSpace(item.Text)
		if text != "" {
			return text, strings.TrimSpace(item.FP)
		}
	}
	return "", ""
}

func renderSingleMarkdownCard(header, markdown string) model.RenderedMessage {
	body := strings.TrimSpace(cleanNilLiteral(markdown))
	truncated := false
	for attempts := 0; attempts < 12; attempts++ {
		candidate := body
		if truncated {
			candidate = strings.TrimSpace(candidate) + "\n\n[Trimmed.]"
		}
		messages := msgformat.RenderMarkdownWithHeader(header, candidate)
		if len(messages) <= 1 {
			return firstRenderedMessage(messages)
		}
		runes := []rune(body)
		if len(runes) == 0 {
			break
		}
		next := len(runes) * 3 / 4
		if next <= 0 || next >= len(runes) {
			next = len(runes) - 1
		}
		body = string(runes[:next])
		truncated = true
	}
	text := trimOutputTail(strings.TrimSpace(header+"\n\n"+body+"\n\n[Trimmed.]"), msgformat.MessageLimit-32)
	return model.RenderedMessage{Text: text}
}

func (s *Service) handleDetailsCallback(ctx context.Context, chatID, topicID, callbackMessageID int64, route *model.CallbackRoute, payload map[string]any) (*DirectResponse, error) {
	panel, response, err := s.detailsPanelForCallback(ctx, chatID, topicID, callbackMessageID, route, payload, false)
	if err != nil {
		return nil, err
	}
	if response != nil {
		return response, nil
	}
	thread, snapshot, err := s.loadThreadPanelSnapshot(ctx, panel.ThreadID)
	if err != nil || thread == nil || snapshot == nil {
		return &DirectResponse{Text: s.t(ctx, "无法加载线程详情。请尝试 /repair 或 /show <thread>。", "Could not load thread details. Try /repair or /show <thread>.")}, nil
	}
	snapshot, ok := snapshotForPanelTurn(*thread, snapshot, panel)
	if !ok {
		return s.staleDetailsResponse(ctx), nil
	}
	state := detailsStateFromPayload(payload)
	sectionCount := len(detailSections(snapshot))
	if route != nil {
		switch route.Action {
		case "details_open":
			state = model.DetailsViewState{Page: 0}
		case "details_prev":
			if state.ToolMode {
				state.CommentaryIndex = clampInt(state.CommentaryIndex-1, 1, maxInt(1, sectionCount))
			} else {
				state.Page = clampInt(state.Page-1, 0, maxInt(0, detailsPageCount(sectionCount)-1))
			}
		case "details_next":
			if state.ToolMode {
				state.CommentaryIndex = clampInt(state.CommentaryIndex+1, 1, maxInt(1, sectionCount))
			} else {
				state.Page = clampInt(state.Page+1, 0, maxInt(0, detailsPageCount(sectionCount)-1))
			}
		case "details_tool_toggle":
			state.ToolMode = !state.ToolMode
			if state.ToolMode && state.CommentaryIndex == 0 {
				state.CommentaryIndex = firstCommentaryWithToolsOnPage(snapshot, state.Page, sectionCount)
			}
		case "details_back":
			message, buttons, cardHash := s.renderFinalCard(ctx, panel.ID, *thread, snapshot)
			targetMessageID := callbackMessageID
			if targetMessageID == 0 {
				targetMessageID = panel.SummaryMessageID
			}
			if err := s.editPanelCard(ctx, chatID, topicID, targetMessageID, message, buttons, panel, snapshot, cardHash, model.DetailsViewState{}); err != nil {
				return nil, err
			}
			return &DirectResponse{CallbackText: s.t(ctx, "最终回复卡片", "Final card.")}, nil
		}
	}
	state.Page = clampInt(state.Page, 0, maxInt(0, detailsPageCount(sectionCount)-1))
	if state.ToolMode {
		state.CommentaryIndex = clampInt(state.CommentaryIndex, 1, maxInt(1, sectionCount))
	}
	message, buttons, cardHash := s.renderDetailsCard(ctx, panel.ID, *thread, snapshot, state)
	targetMessageID := callbackMessageID
	if targetMessageID == 0 {
		targetMessageID = panel.SummaryMessageID
	}
	if err := s.editPanelCard(ctx, chatID, topicID, targetMessageID, message, buttons, panel, snapshot, cardHash, state); err != nil {
		return nil, err
	}
	return &DirectResponse{CallbackText: s.t(ctx, "详情已更新", "Details updated.")}, nil
}

func (s *Service) handleTurnOffPlanCallback(ctx context.Context, chatID, topicID, callbackMessageID int64, route *model.CallbackRoute, payload map[string]any) (*DirectResponse, error) {
	panel, response, err := s.detailsPanelForCallback(ctx, chatID, topicID, callbackMessageID, route, payload, false)
	if err != nil {
		return nil, err
	}
	if response != nil {
		return response, nil
	}
	thread, snapshot, err := s.loadThreadPanelSnapshot(ctx, panel.ThreadID)
	if err != nil || thread == nil || snapshot == nil {
		return &DirectResponse{Text: s.t(ctx, "无法加载线程详情。请尝试 /repair 或 /show <thread>。", "Could not load thread details. Try /repair or /show <thread>.")}, nil
	}
	snapshot, ok := snapshotForPanelTurn(*thread, snapshot, panel)
	if !ok {
		return s.staleDetailsResponse(ctx), nil
	}
	if err := s.setThreadCollaborationDefaultOverride(ctx, panel.ThreadID); err != nil {
		return nil, err
	}
	message, buttons, cardHash := s.renderFinalCard(ctx, panel.ID, *thread, snapshot)
	targetMessageID := callbackMessageID
	if targetMessageID == 0 {
		targetMessageID = panel.SummaryMessageID
	}
	if err := s.editPanelCard(ctx, chatID, topicID, targetMessageID, message, buttons, panel, snapshot, cardHash, model.DetailsViewState{}); err != nil {
		return nil, err
	}
	return &DirectResponse{CallbackText: s.t(ctx, "下一轮将关闭 Plan 模式。", "Plan Mode will be off for the next turn.")}, nil
}

func (s *Service) editPanelCard(ctx context.Context, chatID, topicID, messageID int64, message model.RenderedMessage, buttons [][]model.ButtonSpec, panel *model.ThreadPanel, snapshot *appserver.ThreadReadSnapshot, cardHash string, state model.DetailsViewState) error {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender == nil {
		return fmt.Errorf("message sender is not ready")
	}
	s.logChatRenderedMessagesContainsNil(panel.ThreadID, snapshot.LatestTurnID, "details", messageID, []model.RenderedMessage{message})
	if err := sender.EditRenderedMessage(ctx, chatID, topicID, messageID, message, buttons); err != nil {
		return err
	}
	_ = s.store.PutMessageRoute(ctx, model.MessageRoute{ChatID: chatID, TopicID: topicID, MessageID: messageID, ThreadID: panel.ThreadID, TurnID: snapshot.LatestTurnID, EventID: snapshot.LatestFinalFP, CreatedAt: model.NowString()})
	panel.DetailsViewJSON = model.MustJSON(state)
	panel.LastFinalCardHash = cardHash
	return s.store.UpdateThreadPanelDetails(ctx, panel.ID, panel.DetailsViewJSON, panel.LastFinalCardHash)
}

func (s *Service) renderDetailsCard(ctx context.Context, panelID int64, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, state model.DetailsViewState) (model.RenderedMessage, [][]model.ButtonSpec, string) {
	sections := detailSections(snapshot)
	totalPages := detailsPageCount(len(sections))
	if totalPages == 0 {
		totalPages = 1
	}
	state.Page = clampInt(state.Page, 0, totalPages-1)
	segments := []msgformat.Segment{msgformat.Plain(strings.Join([]string{
		s.visualHeader(ctx, s.t(ctx, "详情", "Details"), thread, snapshot.LatestTurnID),
		fmt.Sprintf("%s: %s", s.t(ctx, "状态", "Status"), readableProgressStatusLang(s.botLanguage(ctx), cardDisplayStatus(snapshot, thread))),
	}, "\n"))}

	if state.ToolMode {
		index := clampInt(state.CommentaryIndex, 1, maxInt(1, len(sections)))
		if len(sections) == 0 {
			segments = append(segments, msgformat.Plain(s.t(ctx, "\n\n这一轮没有详情条目。", "\n\nNo detail entries for this turn.")))
		} else {
			section := sections[index-1]
			segments = appendDetailSectionHeaderSegments(segments, section)
			segments = appendToolDetailSegments(segments, detailsItemsForSection(snapshot, section), detailsToolMaxBytes)
		}
	} else {
		if len(sections) == 0 {
			segments = append(segments, msgformat.Plain(s.t(ctx, "\n\n这一轮没有详情条目。", "\n\nNo detail entries for this turn.")))
		} else {
			start := state.Page * detailsPageSize
			end := minInt(start+detailsPageSize, len(sections))
			for index := start; index < end; index++ {
				segments = appendDetailSectionSummarySegments(segments, sections[index], detailsItemsForSection(snapshot, sections[index]))
			}
			segments = append(segments, msgformat.Plain(fmt.Sprintf(s.t(ctx, "\n\n第 %d/%d 页", "\n\nPage %d/%d"), state.Page+1, totalPages)))
		}
	}
	message := firstRenderedMessage(msgformat.RenderSegments(segments, msgformat.MessageLimit))
	buttons := s.detailsButtons(ctx, panelID, thread.ID, snapshot.LatestTurnID, state, len(sections))
	return message, buttons, hashStrings(msgformat.HashRendered(message), flattenButtonSpecs(buttons))
}

func appendDetailSectionHeaderSegments(segments []msgformat.Segment, section detailSection) []msgformat.Segment {
	segments = append(segments, msgformat.Plain("\n\n"+section.Title))
	if text := strings.TrimSpace(cleanNilLiteral(section.Text)); text != "" {
		segments = append(segments, msgformat.Plain("\n"), msgformat.Markdown(text))
	}
	return segments
}

func appendDetailSectionSummarySegments(segments []msgformat.Segment, section detailSection, items []model.DetailItem) []msgformat.Segment {
	segments = appendDetailSectionHeaderSegments(segments, section)
	if section.ToolOnly {
		segments = appendToolDetailSegments(segments, items, detailsToolMaxBytes)
	}
	return segments
}

func appendToolDetailSegments(segments []msgformat.Segment, items []model.DetailItem, quota int) []msgformat.Segment {
	if len(items) == 0 {
		return append(segments, msgformat.Plain("\n\nNo related tool/output entries for this commentary."))
	}
	perItem := quota / len(items)
	if perItem < 300 {
		perItem = 300
	}
	for _, item := range items {
		switch item.Kind {
		case model.DetailItemTool:
			label := strings.TrimSpace(cleanNilLiteral(item.Label))
			if label == "" {
				label = strings.TrimSpace(cleanNilLiteral(item.Text))
			}
			segments = append(segments, msgformat.Plain("\n\n[Tool]\n"))
			if label != "" {
				segments = append(segments, msgformat.Markdown("```\n"+trimHead(label, perItem)+"\n```"))
			}
			if status := strings.TrimSpace(item.Status); status != "" {
				segments = append(segments, msgformat.Plain("\nStatus: "+status))
			}
		case model.DetailItemOutput:
			output := strings.TrimSpace(cleanNilLiteral(item.Output))
			if output == "" {
				output = strings.TrimSpace(cleanNilLiteral(item.Text))
			}
			if output != "" {
				segments = append(segments, msgformat.Plain("\n\n[Output]\n"), msgformat.Markdown("```\n"+trimOutputTail(output, perItem)+"\n```"))
			}
		}
	}
	return segments
}

func (s *Service) detailsButtons(ctx context.Context, panelID int64, threadID, turnID string, state model.DetailsViewState, commentaryCount int) [][]model.ButtonSpec {
	prevPayload := map[string]any{"panel_id": panelID, "page": state.Page, "tool_mode": state.ToolMode, "commentary_index": state.CommentaryIndex}
	nextPayload := map[string]any{"panel_id": panelID, "page": state.Page, "tool_mode": state.ToolMode, "commentary_index": state.CommentaryIndex}
	rows := [][]model.ButtonSpec{{
		s.callbackButton(ctx, "<", "details_prev", threadID, turnID, "", prevPayload),
		s.callbackButton(ctx, s.t(ctx, "返回", "Back"), "details_back", threadID, turnID, "", map[string]any{"panel_id": panelID}),
		s.callbackButton(ctx, ">", "details_next", threadID, turnID, "", nextPayload),
	}}
	togglePayload := map[string]any{"panel_id": panelID, "page": state.Page, "tool_mode": state.ToolMode, "commentary_index": state.CommentaryIndex}
	if state.ToolMode {
		rows = append(rows, []model.ButtonSpec{
			s.callbackButton(ctx, s.t(ctx, "关闭工具", "Tool off"), "details_tool_toggle", threadID, turnID, "", togglePayload),
			s.callbackButton(ctx, s.t(ctx, "工具文件", "Tools file"), "details_tools_file", threadID, turnID, "", togglePayload),
		})
	} else {
		rows = append(rows, []model.ButtonSpec{s.callbackButton(ctx, s.t(ctx, "查看工具", "Tool on"), "details_tool_toggle", threadID, turnID, "", togglePayload)})
	}
	return rows
}

func (s *Service) sendDetailsToolsFile(ctx context.Context, chatID, topicID, callbackMessageID int64, route *model.CallbackRoute, payload map[string]any) (*DirectResponse, error) {
	panel, response, err := s.detailsPanelForCallback(ctx, chatID, topicID, callbackMessageID, route, payload, false)
	if err != nil {
		return nil, err
	}
	if response != nil {
		return response, nil
	}
	thread, snapshot, err := s.loadThreadPanelSnapshot(ctx, panel.ThreadID)
	if err != nil || thread == nil || snapshot == nil {
		return &DirectResponse{Text: s.t(ctx, "无法加载要导出的线程详情。", "Could not load thread details for export.")}, nil
	}
	snapshot, ok := snapshotForPanelTurn(*thread, snapshot, panel)
	if !ok {
		return s.staleDetailsResponse(ctx), nil
	}
	index := payloadInt(payload, "commentary_index")
	if index == 0 {
		index = 1
	}
	body := s.buildDetailsToolsText(*thread, snapshot, index)
	if strings.TrimSpace(body) == "" {
		return &DirectResponse{CallbackText: s.t(ctx, "没有相关工具或输出条目。", "No related tool/output entries.")}, nil
	}
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender == nil {
		return &DirectResponse{Text: s.t(ctx, "消息发送器尚未就绪。", "Message sender is not ready yet.")}, nil
	}
	fileName := fmt.Sprintf("%s-%s-details-%d.txt", sanitizeFileName(thread.ProjectName), sanitizeFileName(thread.ShortID()), index)
	caption := s.visualHeader(ctx, s.t(ctx, "详情工具", "Details tools"), *thread, snapshot.LatestTurnID)
	if _, err := sender.SendDocumentData(ctx, chatID, topicID, fileName, []byte(body), caption, silentSendOptions()); err != nil {
		return &DirectResponse{Text: fmt.Sprintf(s.t(ctx, "无法发送详情工具文件：%v", "Could not send details tools file: %v"), err)}, nil
	}
	_ = route
	return &DirectResponse{CallbackText: s.t(ctx, "工具文件已发送。", "Tools file sent.")}, nil
}

func (s *Service) sendPanelFullLogFile(ctx context.Context, chatID, topicID, callbackMessageID int64, route *model.CallbackRoute, payload map[string]any, sourceMode string) (*DirectResponse, error) {
	_ = payload
	threadID := ""
	if route != nil {
		threadID = strings.TrimSpace(route.ThreadID)
	}
	if threadID == "" {
		return s.staleDetailsResponse(ctx), nil
	}
	thread, snapshot, err := s.loadThreadPanelSnapshot(ctx, threadID)
	if err != nil || thread == nil || snapshot == nil {
		return &DirectResponse{Text: s.t(ctx, "无法加载完整日志。请尝试 /repair。", "Could not load full log. Try /repair.")}, nil
	}
	if !isTerminalStatus(cardDisplayStatus(snapshot, *thread)) {
		return &DirectResponse{CallbackText: s.t(ctx, "任务完成后才能下载完整日志。", "Full log is available after the task finishes.")}, nil
	}
	body := s.buildPanelFullLogMarkdown(ctx, *thread, snapshot)
	if strings.TrimSpace(body) == "" {
		return &DirectResponse{CallbackText: s.t(ctx, "没有可导出的日志。", "No log to export.")}, nil
	}
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender == nil {
		return &DirectResponse{Text: s.t(ctx, "消息发送器尚未就绪。", "Message sender is not ready yet.")}, nil
	}
	fileName := fmt.Sprintf("%s-%s-full-log.md", sanitizeFileName(thread.ProjectName), sanitizeFileName(thread.ShortID()))
	caption := s.visualHeader(ctx, s.t(ctx, "完整日志", "Full log"), *thread, snapshot.LatestTurnID)
	options := silentSendOptions()
	if normalizeInputSourceMode(sourceMode) == model.PanelSourceFeishuInput && callbackMessageID != 0 {
		options.FeishuReplyToMessageID = callbackMessageID
		options.FeishuReplyInThread = true
		options.FeishuCodexThreadID = thread.ID
	}
	if _, err := sender.SendDocumentData(ctx, chatID, topicID, fileName, []byte(body), caption, options); err != nil {
		return &DirectResponse{Text: fmt.Sprintf(s.t(ctx, "无法发送完整日志：%v", "Could not send full log: %v"), err)}, nil
	}
	_ = route
	return &DirectResponse{CallbackText: s.t(ctx, "完整日志已发送。", "Full log sent.")}, nil
}

func (s *Service) buildPanelFullLogMarkdown(ctx context.Context, thread model.Thread, snapshot *appserver.ThreadReadSnapshot) string {
	lines := []string{
		"# " + strings.TrimSpace(thread.Title),
		"",
		fmt.Sprintf("- %s: %s", s.t(ctx, "项目", "Project"), strings.TrimSpace(thread.ProjectName)),
		fmt.Sprintf("- %s: %s", s.t(ctx, "会话", "Thread"), thread.ID),
		fmt.Sprintf("- %s: %s", s.t(ctx, "轮次", "Turn"), strings.TrimSpace(snapshot.LatestTurnID)),
		fmt.Sprintf("- %s: %s", s.t(ctx, "状态", "Status"), readableProgressStatusLang(s.botLanguage(ctx), cardDisplayStatus(snapshot, thread))),
		fmt.Sprintf("- %s: %d", s.t(ctx, "过程日志条数", "Process log entries"), summaryLogItemCount(snapshot)),
		"",
		"## " + s.t(ctx, "过程日志", "Process log"),
	}
	for _, item := range snapshot.DetailItems {
		switch item.Kind {
		case model.DetailItemUser:
			if text := strings.TrimSpace(cleanNilLiteral(item.Text)); text != "" {
				lines = append(lines, "", "### "+s.t(ctx, "用户输入", "User input"), "", text)
			}
		case model.DetailItemCommentary:
			if text := strings.TrimSpace(cleanNilLiteral(item.Text)); text != "" {
				lines = append(lines, "", "### "+s.t(ctx, "思考中", "Thinking"), "", text)
			}
		case model.DetailItemTool:
			if text := summaryToolDetailText(item); text != "" {
				lines = append(lines, "", "### "+s.toolTimelineStatus(ctx, item), "", "```", text, "```")
			}
		case model.DetailItemOutput:
			if text := summaryOutputDetailText(item); text != "" {
				lines = append(lines, "", "### "+s.t(ctx, "工具输出", "Tool output"), "", "```", text, "```")
			}
		case model.DetailItemFinal:
			if text := strings.TrimSpace(cleanNilLiteral(item.Text)); text != "" {
				lines = append(lines, "", "## "+s.t(ctx, "最终回复", "Final answer"), "", text)
			}
		}
	}
	if finalText, _ := terminalCardTextAndFP(snapshot); strings.TrimSpace(finalText) != "" && !strings.Contains(strings.Join(lines, "\n"), finalText) {
		lines = append(lines, "", "## "+s.t(ctx, "最终回复", "Final answer"), "", strings.TrimSpace(finalText))
	}
	return strings.TrimSpace(strings.Join(lines, "\n")) + "\n"
}

func (s *Service) detailsPanelForCallback(ctx context.Context, chatID, topicID, callbackMessageID int64, route *model.CallbackRoute, payload map[string]any, requireMessageMatch bool) (*model.ThreadPanel, *DirectResponse, error) {
	panelID := payloadInt64(payload, "panel_id")
	if panelID == 0 {
		return nil, s.staleDetailsResponse(ctx), nil
	}
	panel, err := s.store.GetThreadPanelByID(ctx, panelID)
	if err != nil {
		return nil, nil, err
	}
	if panel == nil {
		return nil, s.staleDetailsResponse(ctx), nil
	}
	if panel.ChatID != chatID || panel.TopicID != topicID {
		return nil, s.staleDetailsResponse(ctx), nil
	}
	if route != nil {
		if threadID := strings.TrimSpace(route.ThreadID); threadID != "" && strings.TrimSpace(panel.ThreadID) != threadID {
			return nil, s.staleDetailsResponse(ctx), nil
		}
		if turnID := strings.TrimSpace(route.TurnID); turnID != "" && strings.TrimSpace(panel.CurrentTurnID) != turnID {
			return nil, s.staleDetailsResponse(ctx), nil
		}
	}
	if requireMessageMatch && callbackMessageID != panel.SummaryMessageID {
		return nil, s.staleDetailsResponse(ctx), nil
	}
	if !requireMessageMatch && callbackMessageID != 0 && callbackMessageID != panel.SummaryMessageID {
		if !s.callbackMessageMatchesPanel(ctx, chatID, topicID, callbackMessageID, panel) {
			return nil, s.staleDetailsResponse(ctx), nil
		}
	}
	return panel, nil, nil
}

func (s *Service) staleDetailsResponse(ctx context.Context) *DirectResponse {
	return &DirectResponse{Text: s.t(ctx, "详情面板已过期。请使用 /show <thread>。", "Details panel is stale. Use /show <thread>.")}
}

func (s *Service) callbackMessageMatchesPanel(ctx context.Context, chatID, topicID, messageID int64, panel *model.ThreadPanel) bool {
	if panel == nil || messageID == 0 {
		return false
	}
	route, err := s.store.ResolveMessageRoute(ctx, chatID, topicID, messageID)
	if err != nil || route == nil {
		return false
	}
	if strings.TrimSpace(route.ThreadID) != strings.TrimSpace(panel.ThreadID) {
		return false
	}
	routeTurnID := strings.TrimSpace(route.TurnID)
	panelTurnID := strings.TrimSpace(panel.CurrentTurnID)
	return routeTurnID == "" || panelTurnID == "" || routeTurnID == panelTurnID
}

type detailSection struct {
	Title           string
	Text            string
	CommentaryIndex int
	ToolOnly        bool
}

func (s *Service) buildDetailsToolsText(thread model.Thread, snapshot *appserver.ThreadReadSnapshot, sectionIndex int) string {
	sections := detailSections(snapshot)
	if sectionIndex < 1 || sectionIndex > len(sections) {
		return ""
	}
	section := sections[sectionIndex-1]
	lines := []string{
		s.visualFileHeader(thread, snapshot.LatestTurnID, "Details tools"),
		section.Title,
	}
	if text := strings.TrimSpace(cleanNilLiteral(section.Text)); text != "" {
		lines = append(lines, text)
	}
	items := detailsItemsForSection(snapshot, section)
	for _, item := range items {
		switch item.Kind {
		case model.DetailItemTool:
			lines = append(lines, "", "[Tool]", strings.TrimSpace(item.Label))
			if status := strings.TrimSpace(item.Status); status != "" {
				lines = append(lines, "Status: "+status)
			}
		case model.DetailItemOutput:
			output := strings.TrimSpace(item.Output)
			if output == "" {
				output = strings.TrimSpace(item.Text)
			}
			lines = append(lines, "", "[Output]", output)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func detailSections(snapshot *appserver.ThreadReadSnapshot) []detailSection {
	if snapshot == nil {
		return nil
	}
	out := []detailSection{}
	if len(detailsItemsForCommentaryIndex(snapshot, 0)) > 0 {
		out = append(out, detailSection{Title: "Tool activity", ToolOnly: true})
	}
	for index, commentary := range detailCommentaries(snapshot) {
		commentaryIndex := commentary.CommentaryIndex
		if commentaryIndex <= 0 {
			commentaryIndex = index + 1
		}
		out = append(out, detailSection{
			Title:           fmt.Sprintf("[commentary %d]", index+1),
			Text:            commentary.Text,
			CommentaryIndex: commentaryIndex,
		})
	}
	return out
}

func detailCommentaries(snapshot *appserver.ThreadReadSnapshot) []model.DetailItem {
	if snapshot == nil {
		return nil
	}
	out := []model.DetailItem{}
	for _, item := range snapshot.DetailItems {
		if item.Kind == model.DetailItemCommentary {
			out = append(out, item)
		}
	}
	if len(out) > 0 {
		return out
	}
	entries := chronologicalAgentEntries(snapshot.LatestAgentMessageEntries)
	for index, entry := range entries {
		if strings.TrimSpace(entry.Text) == "" {
			continue
		}
		out = append(out, model.DetailItem{
			ID:              entry.ID,
			Kind:            model.DetailItemCommentary,
			Phase:           entry.Phase,
			Text:            entry.Text,
			FP:              entry.FP,
			CommentaryIndex: index + 1,
		})
	}
	return out
}

func snapshotForPanelTurn(thread model.Thread, snapshot *appserver.ThreadReadSnapshot, panel *model.ThreadPanel) (*appserver.ThreadReadSnapshot, bool) {
	if snapshot == nil || panel == nil {
		return snapshot, true
	}
	turnID := strings.TrimSpace(panel.CurrentTurnID)
	if turnID == "" || strings.TrimSpace(snapshot.LatestTurnID) == turnID {
		return snapshot, true
	}
	var payload map[string]any
	if err := json.Unmarshal(snapshot.Thread.Raw, &payload); err != nil || payload == nil {
		if err := json.Unmarshal(thread.Raw, &payload); err != nil || payload == nil {
			return nil, false
		}
	}
	turns := turnsFromThreadPayload(payload)
	if len(turns) == 0 {
		return nil, false
	}
	for _, rawTurn := range turns {
		turn, _ := rawTurn.(map[string]any)
		if turn == nil || strings.TrimSpace(stringValueFromMap(turn, "id")) != turnID {
			continue
		}
		panelPayload := panelThreadPayloadForTurn(payload, turn)
		panelSnapshot := appserver.SnapshotFromThreadRead(panelPayload)
		if panelSnapshot.Thread.ID == "" {
			panelSnapshot.Thread = thread
		}
		return &panelSnapshot, true
	}
	return nil, false
}

func panelThreadPayloadForTurn(payload map[string]any, turn map[string]any) map[string]any {
	if nested, ok := payload["thread"].(map[string]any); ok && nested != nil {
		out := shallowCopyMap(payload)
		threadPayload := shallowCopyMap(nested)
		threadPayload["turns"] = []any{turn}
		out["thread"] = threadPayload
		return out
	}
	out := shallowCopyMap(payload)
	out["turns"] = []any{turn}
	return out
}

func turnsFromThreadPayload(payload map[string]any) []any {
	if payload == nil {
		return nil
	}
	if turns, ok := payload["turns"].([]any); ok {
		return turns
	}
	if nested, ok := payload["thread"].(map[string]any); ok && nested != nil {
		if turns, ok := nested["turns"].([]any); ok {
			return turns
		}
	}
	return nil
}

func shallowCopyMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func stringValueFromMap(input map[string]any, key string) string {
	value, _ := input[key].(string)
	return value
}

func detailsItemsForSection(snapshot *appserver.ThreadReadSnapshot, section detailSection) []model.DetailItem {
	if snapshot == nil {
		return nil
	}
	if section.ToolOnly {
		return detailsItemsForCommentaryIndex(snapshot, 0)
	}
	return detailsItemsForCommentaryIndex(snapshot, section.CommentaryIndex)
}

func detailsItemsForCommentary(snapshot *appserver.ThreadReadSnapshot, sectionIndex int) []model.DetailItem {
	sections := detailSections(snapshot)
	if sectionIndex < 1 || sectionIndex > len(sections) {
		return nil
	}
	return detailsItemsForSection(snapshot, sections[sectionIndex-1])
}

func detailsItemsForCommentaryIndex(snapshot *appserver.ThreadReadSnapshot, commentaryIndex int) []model.DetailItem {
	if snapshot == nil || commentaryIndex < 0 {
		return nil
	}
	out := []model.DetailItem{}
	for _, item := range snapshot.DetailItems {
		if item.CommentaryIndex == commentaryIndex && (item.Kind == model.DetailItemTool || item.Kind == model.DetailItemOutput) {
			out = append(out, item)
		}
	}
	return out
}

func firstCommentaryWithToolsOnPage(snapshot *appserver.ThreadReadSnapshot, page, commentaryCount int) int {
	if commentaryCount <= 0 {
		return 1
	}
	start := clampInt(page, 0, maxInt(0, detailsPageCount(commentaryCount)-1))*detailsPageSize + 1
	end := minInt(start+detailsPageSize-1, commentaryCount)
	for index := start; index <= end; index++ {
		if len(detailsItemsForCommentary(snapshot, index)) > 0 {
			return index
		}
	}
	return clampInt(start, 1, commentaryCount)
}

func detailsPageCount(count int) int {
	if count <= 0 {
		return 0
	}
	return (count + detailsPageSize - 1) / detailsPageSize
}

func detailsStateFromPayload(payload map[string]any) model.DetailsViewState {
	return model.DetailsViewState{
		Page:            payloadInt(payload, "page"),
		ToolMode:        payloadBool(payload, "tool_mode"),
		CommentaryIndex: payloadInt(payload, "commentary_index"),
	}
}

func payloadInt(payload map[string]any, key string) int {
	return int(payloadInt64(payload, key))
}

func payloadInt64(payload map[string]any, key string) int64 {
	if payload == nil {
		return 0
	}
	switch value := payload[key].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	case jsonNumber:
		parsed, _ := value.Int64()
		return parsed
	case string:
		var parsed int64
		_, _ = fmt.Sscanf(value, "%d", &parsed)
		return parsed
	default:
		return 0
	}
}

func payloadBool(payload map[string]any, key string) bool {
	if payload == nil {
		return false
	}
	switch value := payload[key].(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(value, "true")
	default:
		return false
	}
}

type jsonNumber interface {
	Int64() (int64, error)
}

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func trimHead(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	runes := []rune(value)
	if limit >= len(runes) {
		return value
	}
	return strings.TrimSpace(string(runes[:limit])) + "\n..."
}
