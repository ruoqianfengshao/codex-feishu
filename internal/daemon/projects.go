package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mideco-tech/codex-tg/internal/appserver"
	"github.com/mideco-tech/codex-tg/internal/config"
	"github.com/mideco-tech/codex-tg/internal/model"
	"github.com/mideco-tech/codex-tg/internal/storage"
)

const (
	newThreadStateTTL                  = 15 * time.Minute
	chatsProjectName                   = "Chats"
	defaultProjectsProjectPreviewLimit = 7
	defaultProjectsChatPreviewLimit    = 3
	defaultChatsPageSize               = 8
	codexChatSlugMaxLen                = 48
	codexChatCWDMaxAttempts            = 1000
)

type projectWorkspace struct {
	Key               string `json:"key,omitempty"`
	ProjectName       string `json:"project_name"`
	DirectoryName     string `json:"directory_name,omitempty"`
	CWD               string `json:"cwd"`
	LatestThread      string `json:"latest_thread_id,omitempty"`
	LatestThreadLabel string `json:"latest_thread_label,omitempty"`
	ThreadCount       int    `json:"thread_count,omitempty"`
	UpdatedAt         int64  `json:"updated_at,omitempty"`
}

type pendingNewThreadState struct {
	ProjectName   string `json:"project_name"`
	DirectoryName string `json:"directory_name,omitempty"`
	CWD           string `json:"cwd"`
	ExpiresAt     string `json:"expires_at"`
}

type projectCatalog struct {
	Workspaces []projectWorkspace
	Chats      []model.Thread
}

func (s *Service) projectCatalog(ctx context.Context) (projectCatalog, error) {
	threads, err := s.store.ListThreads(ctx, 500, "")
	if err != nil {
		return projectCatalog{}, err
	}
	grouped := map[string]*projectWorkspace{}
	chats := []model.Thread{}
	for _, thread := range threads {
		if !threadVisibleInProjectCatalog(thread) {
			continue
		}
		if s.isCodexChatThread(thread) {
			chats = append(chats, thread)
			continue
		}
		cwdKey := model.NormalizePath(thread.CWD)
		if cwdKey == "" {
			cwdKey = "thread:" + thread.ID
		}
		workspace := grouped[cwdKey]
		if workspace == nil {
			projectName := strings.TrimSpace(thread.ProjectName)
			directoryName := strings.TrimSpace(thread.DirectoryName)
			if projectName == "" || directoryName == "" {
				derivedProject, derivedDirectory := model.ProjectNameFromCWD(thread.CWD)
				projectName = firstNonEmpty(projectName, derivedProject)
				directoryName = firstNonEmpty(directoryName, derivedDirectory)
			}
			workspace = &projectWorkspace{
				ProjectName:       firstNonEmpty(projectName, "Shared/General"),
				DirectoryName:     directoryName,
				CWD:               thread.CWD,
				LatestThread:      thread.ID,
				LatestThreadLabel: threadDisplayLabel(thread),
				UpdatedAt:         thread.UpdatedAt,
			}
			grouped[cwdKey] = workspace
		}
		workspace.ThreadCount++
		if thread.UpdatedAt > workspace.UpdatedAt {
			workspace.UpdatedAt = thread.UpdatedAt
			workspace.LatestThread = thread.ID
			workspace.LatestThreadLabel = threadDisplayLabel(thread)
		}
	}
	workspaces := make([]projectWorkspace, 0, len(grouped))
	for _, workspace := range grouped {
		workspaces = append(workspaces, *workspace)
	}
	sort.Slice(workspaces, func(i, j int) bool {
		if workspaces[i].UpdatedAt != workspaces[j].UpdatedAt {
			return workspaces[i].UpdatedAt > workspaces[j].UpdatedAt
		}
		leftName := strings.ToLower(workspaces[i].ProjectName)
		rightName := strings.ToLower(workspaces[j].ProjectName)
		if leftName != rightName {
			return leftName < rightName
		}
		leftCWD := strings.ToLower(model.NormalizePath(workspaces[i].CWD))
		rightCWD := strings.ToLower(model.NormalizePath(workspaces[j].CWD))
		return leftCWD < rightCWD
	})
	sort.Slice(chats, func(i, j int) bool {
		if chats[i].UpdatedAt != chats[j].UpdatedAt {
			return chats[i].UpdatedAt > chats[j].UpdatedAt
		}
		return strings.ToLower(chats[i].Label()) < strings.ToLower(chats[j].Label())
	})
	assignProjectWorkspaceKeys(workspaces)
	return projectCatalog{Workspaces: workspaces, Chats: chats}, nil
}

func (s *Service) projectWorkspaces(ctx context.Context) ([]projectWorkspace, error) {
	catalog, err := s.projectCatalog(ctx)
	if err != nil {
		return nil, err
	}
	return catalog.Workspaces, nil
}

func (s *Service) isCodexChatThread(thread model.Thread) bool {
	if isCodexChatsCWD(thread.CWD) {
		return true
	}
	if isPathUnderRoot(thread.CWD, s.codexChatsRoot()) {
		return true
	}
	return strings.TrimSpace(thread.CWD) == "" && strings.EqualFold(strings.TrimSpace(thread.ProjectName), chatsProjectName)
}

func isPathUnderRoot(value, root string) bool {
	normalized := strings.ToLower(strings.TrimRight(model.NormalizePath(value), "/"))
	normalizedRoot := strings.ToLower(strings.TrimRight(model.NormalizePath(root), "/"))
	if normalized == "" || normalizedRoot == "" {
		return false
	}
	return normalized == normalizedRoot || strings.HasPrefix(normalized, normalizedRoot+"/")
}

func isCodexChatsCWD(cwd string) bool {
	normalized := strings.ToLower(model.NormalizePath(cwd))
	if normalized == "" {
		return false
	}
	parts := strings.Split(strings.Trim(normalized, "/"), "/")
	if len(parts) >= 4 && parts[0] == "users" && parts[2] == "documents" && parts[3] == "codex" {
		return true
	}
	return len(parts) >= 5 && strings.HasSuffix(parts[0], ":") && parts[1] == "users" && parts[3] == "documents" && parts[4] == "codex"
}

func (s *Service) projectsProjectPreviewLimit() int {
	return positiveOrDefault(s.cfg.ProjectsProjectPreviewLimit, defaultProjectsProjectPreviewLimit)
}

func (s *Service) projectsChatPreviewLimit() int {
	return positiveOrDefault(s.cfg.ProjectsChatPreviewLimit, defaultProjectsChatPreviewLimit)
}

func (s *Service) chatsPageSize() int {
	return positiveOrDefault(s.cfg.ChatsPageSize, defaultChatsPageSize)
}

func positiveOrDefault(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func assignProjectWorkspaceKeys(workspaces []projectWorkspace) {
	seen := map[string]int{}
	for i := range workspaces {
		base := projectWorkspaceKeyBase(workspaces[i])
		seen[base]++
		if seen[base] == 1 {
			workspaces[i].Key = base
			continue
		}
		workspaces[i].Key = fmt.Sprintf("%s-%d", base, seen[base])
	}
}

func projectWorkspaceKeyBase(workspace projectWorkspace) string {
	source := strings.TrimSpace(firstNonEmpty(workspace.ProjectName, workspace.DirectoryName, workspace.CWD, "project"))
	source = strings.ToLower(source)
	var builder strings.Builder
	lastDash := false
	for _, r := range source {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	key := strings.Trim(builder.String(), "-")
	if key == "" {
		return "project"
	}
	return key
}

func threadDisplayLabel(thread model.Thread) string {
	return firstNonEmpty(thread.Title, thread.ShortID())
}

func projectWorkspaceButtonLabel(index int, workspace projectWorkspace) string {
	return shortButtonLabel(fmt.Sprintf("%d. %s", index, firstNonEmpty(workspace.ProjectName, workspace.DirectoryName, workspace.Key, "Project")))
}

func chatThreadButtonLabel(index int, thread model.Thread) string {
	return shortButtonLabel(fmt.Sprintf("Chat %d. %s", index, threadDisplayLabel(thread)))
}

func (s *Service) projectsOverview(ctx context.Context) (*DirectResponse, error) {
	return s.projectsOverviewPage(ctx, 0)
}

func (s *Service) projectsOverviewPage(ctx context.Context, page int) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	catalog, err := s.projectCatalog(ctx)
	if err != nil {
		return nil, err
	}
	if len(catalog.Workspaces) == 0 && len(catalog.Chats) == 0 {
		return &DirectResponse{Text: localized(lang, "还没有缓存项目。请试试 /status，或等待同步。", "No cached projects yet. Try /status or wait for sync.")}, nil
	}
	projectLimit := s.projectsProjectPreviewLimit()
	chatLimit := s.projectsChatPreviewLimit()
	projectPages := maxInt(1, (len(catalog.Workspaces)+projectLimit-1)/projectLimit)
	page = clampInt(page, 0, projectPages-1)
	projectStart := page * projectLimit
	projectEnd := min(projectStart+projectLimit, len(catalog.Workspaces))
	chatEnd := min(chatLimit, len(catalog.Chats))

	lines := []string{
		fmt.Sprintf("%s %d/%d", localized(lang, "项目", "Projects"), page+1, projectPages),
		fmt.Sprintf("%s %d/%d · %s %d/%d", localized(lang, "项目", "Projects"), maxInt(0, projectEnd-projectStart), len(catalog.Workspaces), localized(lang, "临时对话", "Chats"), chatEnd, len(catalog.Chats)),
	}
	buttons := [][]model.ButtonSpec{
		{
			s.callbackButton(ctx, "<", "projects_page", "", "", "", map[string]any{"page": page - 1}),
			s.callbackButton(ctx, localized(lang, "关闭", "Close"), "projects_close", "", "", "", nil),
			s.callbackButton(ctx, ">", "projects_page", "", "", "", map[string]any{"page": page + 1}),
		},
		{s.callbackButton(ctx, localized(lang, "打开临时对话", "Open Chats"), "chats_open", "", "", "", map[string]any{"page": 0})},
	}
	sections := []model.MessageSection{}
	if len(catalog.Workspaces) == 0 {
		lines = append(lines, "", localized(lang, "临时对话之外还没有缓存项目。", "No cached projects outside Chats."))
	} else {
		projectSection := model.MessageSection{Text: localized(lang, "项目", "Projects"), Heading: true}
		for index, workspace := range catalog.Workspaces[projectStart:projectEnd] {
			displayIndex := projectStart + index + 1
			label := firstNonEmpty(workspace.ProjectName, workspace.DirectoryName, workspace.Key, "Project")
			latest := firstNonEmpty(workspace.LatestThreadLabel, workspace.LatestThread)
			trailing := fmt.Sprintf("%d chats · %s", workspace.ThreadCount, threadUpdatedAtLabel(workspace.UpdatedAt, s.now))
			lines = append(lines,
				fmt.Sprintf("%d. %s", displayIndex, label),
				fmt.Sprintf("   %s · %s", trailing, latest),
			)
			button := s.callbackButton(ctx, projectWorkspaceButtonLabel(displayIndex, workspace), "project_open", "", "", "", projectWorkspacePayload(workspace))
			buttons = append(buttons, []model.ButtonSpec{button})
			projectSection.Rows = append(projectSection.Rows, model.MessageSectionRow{
				Title:    label,
				Trailing: fmt.Sprintf("%s · %s", trailing, latest),
				Button:   button,
			})
		}
		sections = append(sections, projectSection)
	}
	if chatEnd > 0 {
		lines = append(lines, "", localized(lang, "临时对话", "Chats"))
		chatSection := model.MessageSection{Text: localized(lang, "临时对话", "Chats"), Heading: true, Divider: len(sections) > 0}
		for index, thread := range catalog.Chats[:chatEnd] {
			displayIndex := index + 1
			label := threadOverviewLabel(thread, lang)
			lines = append(lines, fmt.Sprintf("%d. %s    %s", displayIndex, label, threadUpdatedAtLabel(thread.UpdatedAt, s.now)))
			button := s.callbackButton(ctx, chatThreadButtonLabel(displayIndex, thread), "chat_open", thread.ID, "", "", nil)
			buttons = append(buttons, []model.ButtonSpec{button})
			chatSection.Rows = append(chatSection.Rows, model.MessageSectionRow{
				Title:    label,
				Trailing: threadUpdatedAtLabel(thread.UpdatedAt, s.now),
				Button:   button,
			})
		}
		sections = append(sections, chatSection)
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Sections: sections, Buttons: buttons}, nil
}

func threadVisibleInProjectCatalog(thread model.Thread) bool {
	if thread.Archived {
		return false
	}
	if threadLooksUnavailableForOverview(thread) {
		return false
	}
	return true
}

func (s *Service) chatsOverviewPage(ctx context.Context, page int) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	catalog, err := s.projectCatalog(ctx)
	if err != nil {
		return nil, err
	}
	pageSize := s.chatsPageSize()
	pageCount := maxInt(1, (len(catalog.Chats)+pageSize-1)/pageSize)
	page = clampInt(page, 0, pageCount-1)
	start := page * pageSize
	end := min(start+pageSize, len(catalog.Chats))
	lines := []string{
		localized(lang, "临时对话", "Chats"),
		fmt.Sprintf(localized(lang, "第 %d/%d 页（显示 %d/%d）", "Page %d/%d (showing %d of %d)"), page+1, pageCount, maxInt(0, end-start), len(catalog.Chats)),
		localized(lang, "使用 /new <提示词> 创建新的 Codex UI Chat。", "Use /new <prompt> to create a new Codex UI Chat."),
	}
	buttons := [][]model.ButtonSpec{
		{
			s.callbackButton(ctx, "<", "chats_page", "", "", "", map[string]any{"page": page - 1}),
			s.callbackButton(ctx, localized(lang, "关闭", "Close"), "projects_close", "", "", "", nil),
			s.callbackButton(ctx, ">", "chats_page", "", "", "", map[string]any{"page": page + 1}),
		},
	}
	if len(catalog.Chats) == 0 {
		lines = append(lines, "", localized(lang, "还没有缓存临时对话。", "No cached Chats yet."))
		return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons}, nil
	}
	for index, thread := range catalog.Chats[start:end] {
		displayIndex := start + index + 1
		lines = append(lines, fmt.Sprintf("%d. %s | %s", displayIndex, threadDisplayLabel(thread), thread.ShortID()))
		buttons = append(buttons, []model.ButtonSpec{
			s.callbackButton(ctx, chatThreadButtonLabel(displayIndex, thread), "chat_open", thread.ID, "", "", nil),
		})
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons}, nil
}

func projectWorkspacePayload(workspace projectWorkspace) map[string]any {
	return map[string]any{
		"key":            workspace.Key,
		"project_name":   workspace.ProjectName,
		"directory_name": workspace.DirectoryName,
		"cwd":            workspace.CWD,
		"latest_thread":  workspace.LatestThread,
		"latest_label":   workspace.LatestThreadLabel,
		"thread_count":   workspace.ThreadCount,
	}
}

func projectWorkspaceFromPayload(payload map[string]any) projectWorkspace {
	return projectWorkspace{
		Key:               payloadMapString(payload, "key"),
		ProjectName:       payloadMapString(payload, "project_name"),
		DirectoryName:     payloadMapString(payload, "directory_name"),
		CWD:               payloadMapString(payload, "cwd"),
		LatestThread:      payloadMapString(payload, "latest_thread"),
		LatestThreadLabel: payloadMapString(payload, "latest_label"),
		ThreadCount:       payloadMapInt(payload, "thread_count"),
	}
}

func payloadMapInt(values map[string]any, key string) int {
	if values == nil {
		return 0
	}
	switch typed := values[key].(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		value, _ := typed.Int64()
		return int(value)
	case string:
		value, _ := strconv.Atoi(strings.TrimSpace(typed))
		return value
	default:
		return 0
	}
}

func (s *Service) projectsPage(ctx context.Context, chatID, topicID, messageID int64, payload map[string]any) (*DirectResponse, error) {
	return s.editOrSendProjectsResponse(ctx, chatID, topicID, messageID, "Projects", func(ctx context.Context) (*DirectResponse, error) {
		return s.projectsOverviewPage(ctx, payloadMapInt(payload, "page"))
	})
}

func (s *Service) chatsPage(ctx context.Context, chatID, topicID, messageID int64, payload map[string]any) (*DirectResponse, error) {
	return s.editOrSendProjectsResponse(ctx, chatID, topicID, messageID, "Chats", func(ctx context.Context) (*DirectResponse, error) {
		return s.chatsOverviewPage(ctx, payloadMapInt(payload, "page"))
	})
}

func (s *Service) closeProjectsMenu(ctx context.Context, chatID, topicID, messageID int64) (*DirectResponse, error) {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender != nil && messageID != 0 {
		if err := sender.DeleteMessage(ctx, chatID, topicID, messageID); err == nil {
			return &DirectResponse{CallbackText: s.t(ctx, "已关闭。", "Closed.")}, nil
		}
		closed := s.t(ctx, "已关闭。", "Closed.")
		if err := sender.EditMessage(ctx, chatID, topicID, messageID, closed, nil); err == nil {
			return &DirectResponse{CallbackText: closed}, nil
		}
	}
	closed := s.t(ctx, "已关闭。", "Closed.")
	return &DirectResponse{Text: closed, CallbackText: closed}, nil
}

func (s *Service) editOrSendProjectsResponse(ctx context.Context, chatID, topicID, messageID int64, callbackText string, renderer func(context.Context) (*DirectResponse, error)) (*DirectResponse, error) {
	response, err := renderer(ctx)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return &DirectResponse{CallbackText: callbackText}, nil
	}
	response.CallbackText = callbackText
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender != nil && messageID != 0 && strings.TrimSpace(response.Text) != "" {
		if err := sender.EditMessage(ctx, chatID, topicID, messageID, response.Text, response.Buttons); err == nil {
			return &DirectResponse{CallbackText: callbackText}, nil
		}
	}
	return response, nil
}

func (s *Service) projectWorkspaceFromCallback(ctx context.Context, payload map[string]any) (projectWorkspace, bool) {
	requested := projectWorkspaceFromPayload(payload)
	requestedCWD := model.NormalizePath(requested.CWD)
	if requestedCWD == "" {
		return projectWorkspace{}, false
	}
	workspaces, err := s.projectWorkspaces(ctx)
	if err != nil {
		return projectWorkspace{}, false
	}
	for _, workspace := range workspaces {
		if model.NormalizePath(workspace.CWD) == requestedCWD {
			return workspace, true
		}
	}
	return projectWorkspace{}, false
}

func (s *Service) openChatThread(ctx context.Context, chatID, topicID int64, threadID string, sourceMode string) (*DirectResponse, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return &DirectResponse{Text: s.t(ctx, "这个对话按钮已过期。请用“打开临时对话”刷新。", "This Chat button is stale. Use Open Chats to refresh.")}, nil
	}
	thread, err := s.store.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if thread == nil || !s.isCodexChatThread(*thread) {
		return &DirectResponse{Text: s.t(ctx, "这个对话按钮已过期。请用“打开临时对话”刷新。", "This Chat button is stale. Use Open Chats to refresh.")}, nil
	}
	response, err := s.showThread(ctx, chatID, topicID, threadID, true, sourceMode)
	if err != nil {
		return nil, err
	}
	if response == nil {
		response = &DirectResponse{}
	}
	response.CallbackText = "Opened Chat."
	return response, nil
}

func (s *Service) projectMenu(ctx context.Context, payload map[string]any) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	workspace, ok := s.projectWorkspaceFromCallback(ctx, payload)
	if !ok {
		return &DirectResponse{Text: localized(lang, "这个项目按钮已过期。请使用 /projects 刷新。", "This project button is stale. Use /projects to refresh.")}, nil
	}
	lines := []string{
		workspace.ProjectName,
		fmt.Sprintf("%s: %s", localized(lang, "目录", "Directory"), settingValueLabel(workspace.DirectoryName, localized(lang, "未知", "unknown"))),
		fmt.Sprintf("CWD: %s", settingValueLabel(workspace.CWD, "unknown")),
		fmt.Sprintf("%s: %d", localized(lang, "缓存会话", "Cached threads"), workspace.ThreadCount),
	}
	buttons := [][]model.ButtonSpec{
		{s.callbackButton(ctx, localized(lang, "新线程", "New thread"), "project_new_thread", "", "", "", projectWorkspacePayload(workspace))},
		{s.callbackButton(ctx, localized(lang, "会话", "Threads"), "project_threads", "", "", "", projectWorkspacePayload(workspace))},
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons}, nil
}

func (s *Service) projectThreads(ctx context.Context, payload map[string]any) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	workspace, ok := s.projectWorkspaceFromCallback(ctx, payload)
	if !ok {
		return &DirectResponse{Text: localized(lang, "这个项目按钮已过期。请使用 /projects 刷新。", "This project button is stale. Use /projects to refresh.")}, nil
	}
	threads, err := s.store.ListThreads(ctx, 500, "")
	if err != nil {
		return nil, err
	}
	lines := []string{fmt.Sprintf(localized(lang, "%s 的会话", "Threads for %s"), workspace.ProjectName)}
	buttons := [][]model.ButtonSpec{}
	section := model.MessageSection{Text: workspace.ProjectName, Heading: true}
	for _, thread := range threads {
		if !threadVisibleInProjectCatalog(thread) {
			continue
		}
		if model.NormalizePath(thread.CWD) != model.NormalizePath(workspace.CWD) {
			continue
		}
		label := threadOverviewLabel(thread, lang)
		updatedAt := threadUpdatedAtLabel(thread.UpdatedAt, s.now)
		button := s.callbackButton(ctx, localized(lang, "打开", "Open"), "show_thread", thread.ID, "", "", nil)
		lines = append(lines, fmt.Sprintf("- %s    %s", label, updatedAt))
		buttons = append(buttons, []model.ButtonSpec{button})
		section.Rows = append(section.Rows, model.MessageSectionRow{
			Title:    label,
			Trailing: updatedAt,
			Button:   button,
		})
	}
	if len(buttons) == 0 {
		lines = append(lines, localized(lang, "这个项目还没有缓存会话。", "No cached threads for this project."))
		return &DirectResponse{Text: strings.Join(lines, "\n")}, nil
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Sections: []model.MessageSection{section}, Buttons: buttons}, nil
}

func (s *Service) armProjectNewThread(ctx context.Context, chatID, topicID int64, payload map[string]any) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	workspace, ok := s.projectWorkspaceFromCallback(ctx, payload)
	if !ok {
		return &DirectResponse{Text: localized(lang, "这个项目按钮已过期。请使用 /projects 刷新。", "This project button is stale. Use /projects to refresh.")}, nil
	}
	if strings.TrimSpace(workspace.CWD) == "" {
		return &DirectResponse{Text: localized(lang, "项目 CWD 不可用。请在 Codex 识别到这个工作区后再使用 /projects。", "Project cwd is not available. Use /projects after Codex has seen this workspace.")}, nil
	}
	state := pendingNewThreadState{
		ProjectName:   workspace.ProjectName,
		DirectoryName: workspace.DirectoryName,
		CWD:           workspace.CWD,
		ExpiresAt:     time.Now().UTC().Add(newThreadStateTTL).Format(time.RFC3339Nano),
	}
	payloadBytes, _ := json.Marshal(state)
	if err := s.store.SetState(ctx, newThreadStateKey(chatID, topicID), string(payloadBytes)); err != nil {
		return nil, err
	}
	return &DirectResponse{Text: fmt.Sprintf(localized(lang, "%s 的新线程。\n请在当前聊天中发送第一条提示词；我会用这条消息创建 Codex 会话和话题。", "New thread for %s.\nSend the first prompt in this chat. I will create the Codex session and topic from that message."), workspace.ProjectName)}, nil
}

func (s *Service) createProjectThread(ctx context.Context, chatID, topicID int64, payload map[string]any, sourceMode string) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	sourceMode = normalizeInputSourceMode(sourceMode)
	workspace, ok := s.projectWorkspaceFromCallback(ctx, payload)
	if !ok {
		return &DirectResponse{Text: localized(lang, "这个项目按钮已过期。请使用 /projects 刷新。", "This project button is stale. Use /projects to refresh.")}, nil
	}
	if strings.TrimSpace(workspace.CWD) == "" {
		return &DirectResponse{Text: localized(lang, "项目 CWD 不可用。请在 Codex 识别到这个工作区后再使用 /projects。", "Project cwd is not available. Use /projects after Codex has seen this workspace.")}, nil
	}
	s.mu.RLock()
	live := s.live
	connected := s.liveConnected
	s.mu.RUnlock()
	if !connected || live == nil {
		return &DirectResponse{Text: localized(lang, "Live app-server 会话尚未就绪。请试试 /status 或 /repair。", "Live app-server session is not ready yet. Try /status or /repair.")}, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	started := time.Now()
	threadPayload, err := live.ThreadStart(requestCtx, workspace.CWD)
	s.logAppServerCall("ThreadStart", started, err, live, lifecycleFields{
		"cwd":          workspace.CWD,
		"project_name": workspace.ProjectName,
	})
	if err != nil {
		return nil, err
	}
	thread := threadFromStartPayload(threadPayload, pendingNewThreadState{
		ProjectName:   workspace.ProjectName,
		DirectoryName: workspace.DirectoryName,
		CWD:           workspace.CWD,
	})
	if strings.TrimSpace(thread.ID) == "" {
		return &DirectResponse{Text: localized(lang, "App Server 未能创建 thread：响应中没有 thread id。", "App Server could not create thread: response did not include thread id.")}, nil
	}
	if err := s.store.UpsertThread(ctx, thread); err != nil {
		return nil, err
	}
	response, err := s.showThread(ctx, chatID, topicID, thread.ID, true, sourceMode)
	if err != nil {
		return nil, err
	}
	if sourceMode == model.PanelSourceFeishuInput {
		return s.feishuVisibleOpenResponse(ctx, response, thread.ID), nil
	}
	return response, nil
}

func (s *Service) newChatCommand(ctx context.Context, chatID, topicID int64, rest string) (*DirectResponse, error) {
	return s.newChatCommandFromSource(ctx, chatID, topicID, rest, model.PanelSourceFeishuInput)
}

func (s *Service) newChatCommandFromSource(ctx context.Context, chatID, topicID int64, rest, sourceMode string) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	prompt := strings.TrimSpace(rest)
	if prompt == "" {
		return &DirectResponse{Text: localized(lang, "用法：/new <提示词>", "Usage: /new <prompt>")}, nil
	}
	cwd, directoryName, err := createCodexChatCWD(s.codexChatsRoot(), prompt, s.now())
	if err != nil {
		return &DirectResponse{Text: fmt.Sprintf(localized(lang, "无法创建 Codex Chat 文件夹：%v", "Could not create Codex Chat folder: %v"), err)}, nil
	}
	return s.createThreadFromProjectPrompt(ctx, chatID, topicID, pendingNewThreadState{
		ProjectName:   chatsProjectName,
		DirectoryName: directoryName,
		CWD:           cwd,
	}, prompt, sourceMode)
}

func (s *Service) codexChatsRoot() string {
	root := strings.TrimSpace(s.cfg.CodexChatsRoot)
	if root == "" {
		root = config.DefaultCodexChatsRoot()
	}
	return filepath.Clean(root)
}

func createCodexChatCWD(root, prompt string, now time.Time) (string, string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = config.DefaultCodexChatsRoot()
	}
	dateDir := filepath.Join(filepath.Clean(root), now.Format("2006-01-02"))
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		return "", "", err
	}
	base := codexChatSlugFromPrompt(prompt, now)
	for attempt := 0; attempt < codexChatCWDMaxAttempts; attempt++ {
		directoryName := base
		if attempt > 0 {
			directoryName = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		cwd := filepath.Join(dateDir, directoryName)
		if err := os.Mkdir(cwd, 0o755); err == nil {
			return cwd, directoryName, nil
		} else if os.IsExist(err) {
			continue
		} else {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("could not allocate unique Chat folder under %s", dateDir)
}

func codexChatSlugFromPrompt(prompt string, now time.Time) string {
	source := strings.ToLower(strings.TrimSpace(prompt))
	var builder strings.Builder
	lastDash := false
	for _, r := range source {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastDash = false
		default:
			if builder.Len() > 0 && !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if len(slug) > codexChatSlugMaxLen {
		slug = strings.Trim(slug[:codexChatSlugMaxLen], "-")
	}
	if slug == "" {
		return "chat-" + now.Format("150405")
	}
	return slug
}

func (s *Service) maybeConsumeNewThreadPrompt(ctx context.Context, chatID, topicID int64, text string) (*DirectResponse, bool, error) {
	return s.maybeConsumeNewThreadPromptFromSource(ctx, chatID, topicID, text, model.PanelSourceFeishuInput)
}

func (s *Service) maybeConsumeNewThreadPromptFromSource(ctx context.Context, chatID, topicID int64, text, sourceMode string) (*DirectResponse, bool, error) {
	lang := s.botLanguage(ctx)
	state, ok, expired, err := s.pendingNewThreadState(ctx, chatID, topicID)
	if err != nil {
		return nil, true, err
	}
	if !ok {
		return nil, false, nil
	}
	if expired {
		_ = s.store.DeleteState(ctx, newThreadStateKey(chatID, topicID))
		return &DirectResponse{Text: localized(lang, "新线程请求已过期。请重新进入 /projects 并点击新线程。", "New thread request expired. Use /projects and New thread again.")}, true, nil
	}
	_ = s.store.DeleteState(ctx, newThreadStateKey(chatID, topicID))
	response, err := s.createThreadFromProjectPrompt(ctx, chatID, topicID, state, strings.TrimSpace(text), sourceMode)
	return response, true, err
}

func (s *Service) pendingNewThreadState(ctx context.Context, chatID, topicID int64) (pendingNewThreadState, bool, bool, error) {
	raw, err := s.store.GetState(ctx, newThreadStateKey(chatID, topicID))
	if err != nil {
		return pendingNewThreadState{}, false, false, err
	}
	if strings.TrimSpace(raw) == "" {
		return pendingNewThreadState{}, false, false, nil
	}
	var state pendingNewThreadState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return pendingNewThreadState{}, true, true, nil
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, state.ExpiresAt)
	if err != nil || time.Now().UTC().After(expiresAt) {
		return state, true, true, nil
	}
	return state, true, false, nil
}

func (s *Service) createThreadFromProjectPrompt(ctx context.Context, chatID, topicID int64, state pendingNewThreadState, prompt, sourceMode string) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	sourceMode = normalizeInputSourceMode(sourceMode)
	if strings.TrimSpace(prompt) == "" {
		return &DirectResponse{Text: localized(lang, "第一条提示词为空。请重新点击新线程，并发送非空提示词。", "First prompt is empty. Use New thread again and send a non-empty prompt.")}, nil
	}
	s.mu.RLock()
	live := s.live
	connected := s.liveConnected
	s.mu.RUnlock()
	if !connected || live == nil {
		return &DirectResponse{Text: localized(lang, "Live app-server 会话尚未就绪。请试试 /status 或 /repair。", "Live app-server session is not ready yet. Try /status or /repair.")}, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	started := time.Now()
	threadPayload, err := live.ThreadStart(requestCtx, state.CWD)
	s.logAppServerCall("ThreadStart", started, err, live, lifecycleFields{
		"cwd":          state.CWD,
		"project_name": state.ProjectName,
	})
	if err != nil {
		return nil, err
	}
	thread := threadFromStartPayload(threadPayload, state)
	if strings.TrimSpace(thread.ID) == "" {
		return &DirectResponse{Text: localized(lang, "App Server 未能创建 thread：响应中没有 thread id。", "App Server could not create thread: response did not include thread id.")}, nil
	}
	if err := s.store.UpsertThread(ctx, thread); err != nil {
		return nil, err
	}

	options := s.turnStartOptions(ctx, "", &thread)
	started = time.Now()
	turnPayload, err := live.TurnStart(requestCtx, thread.ID, prompt, thread.CWD, options)
	s.logAppServerCall("TurnStart", started, err, live, lifecycleFields{
		"thread_id":        thread.ID,
		"returned_turn_id": appserverThreadTurnID(turnPayload),
		"model":            options.Model,
		"reasoning_effort": options.ReasoningEffort,
	})
	if err != nil {
		return &DirectResponse{
			Text:     fmt.Sprintf(localized(lang, "已创建 thread %s，但无法启动第一轮：%v\n请使用 /reply %s <文本> 重试。", "Created thread %s, but could not start first turn: %v\nUse /reply %s <text> to retry."), thread.ID, err, thread.ID),
			ThreadID: thread.ID,
		}, nil
	}
	turnID := appserverThreadTurnID(turnPayload)
	if strings.TrimSpace(turnID) != "" {
		thread.ActiveTurnID = turnID
		thread.Status = "inProgress"
		thread.LastPreview = prompt
		_ = s.store.UpsertThread(ctx, thread)
		_ = s.markInputOriginTurn(ctx, thread.ID, turnID, sourceMode, chatID, topicID)
		s.ensureStartedTurnSnapshot(ctx, &thread, turnID)
	}
	if _, refreshErr := s.refreshThreadForOperation(ctx, live, thread.ID, "refresh_new_thread_after_start"); refreshErr != nil {
		s.logLifecycle("thread_refresh_failed", lifecycleFields{
			"operation": "refresh_new_thread_after_start",
			"thread_id": thread.ID,
			"turn_id":   turnID,
			"error":     refreshErr,
		})
	}
	s.ensureNewChatThreadFallback(ctx, thread.ID, state)
	target := model.ObserverTarget{ChatKey: model.ChatKey(chatID, topicID), ChatID: chatID, TopicID: topicID, Enabled: true}
	s.syncThreadPanelToTarget(ctx, target, thread.ID, true, sourceMode)
	if strings.TrimSpace(turnID) != "" {
		s.startChatOriginHotPoll(ctx, thread.ID, turnID)
	}
	response := &DirectResponse{ThreadID: thread.ID, TurnID: turnID}
	if sourceMode == model.PanelSourceFeishuInput {
		return s.feishuVisibleOpenResponse(ctx, response, thread.ID), nil
	}
	return response, nil
}

func (s *Service) ensureNewChatThreadFallback(ctx context.Context, threadID string, state pendingNewThreadState) {
	if !strings.EqualFold(strings.TrimSpace(state.ProjectName), chatsProjectName) {
		return
	}
	thread, err := s.store.GetThread(ctx, threadID)
	if err != nil || thread == nil {
		return
	}
	changed := false
	if !strings.EqualFold(strings.TrimSpace(thread.ProjectName), chatsProjectName) {
		thread.ProjectName = chatsProjectName
		changed = true
	}
	if strings.TrimSpace(state.DirectoryName) != "" && thread.DirectoryName != state.DirectoryName {
		thread.DirectoryName = state.DirectoryName
		changed = true
	}
	if !changed {
		return
	}
	_ = s.store.UpsertThread(ctx, *thread)
}

func threadFromStartPayload(payload map[string]any, state pendingNewThreadState) model.Thread {
	thread := appserver.ThreadFromPayload(payload)
	if strings.TrimSpace(thread.CWD) == "" {
		thread.CWD = state.CWD
	}
	if strings.EqualFold(strings.TrimSpace(state.ProjectName), chatsProjectName) {
		thread.ProjectName = chatsProjectName
		thread.DirectoryName = firstNonEmpty(state.DirectoryName, thread.DirectoryName)
	}
	if strings.TrimSpace(thread.ProjectName) == "" {
		project, directory := model.ProjectNameFromCWD(thread.CWD)
		thread.ProjectName = firstNonEmpty(state.ProjectName, project)
		thread.DirectoryName = firstNonEmpty(state.DirectoryName, directory)
	}
	if strings.TrimSpace(thread.DirectoryName) == "" {
		_, directory := model.ProjectNameFromCWD(thread.CWD)
		thread.DirectoryName = firstNonEmpty(state.DirectoryName, directory)
	}
	if strings.TrimSpace(thread.Title) == "" {
		thread.Title = "New thread"
	}
	if thread.UpdatedAt == 0 {
		thread.UpdatedAt = time.Now().UTC().Unix()
	}
	if len(thread.Raw) == 0 {
		thread.Raw = json.RawMessage(storage.MustJSON(payload))
	}
	return thread
}

func newThreadStateKey(chatID, topicID int64) string {
	return "chat.new_thread." + model.ChatKey(chatID, topicID)
}
