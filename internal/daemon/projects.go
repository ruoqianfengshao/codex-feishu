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

	"github.com/ruoqianfengshao/codex-feishu/internal/appserver"
	"github.com/ruoqianfengshao/codex-feishu/internal/config"
	"github.com/ruoqianfengshao/codex-feishu/internal/model"
	"github.com/ruoqianfengshao/codex-feishu/internal/storage"
)

const (
	newThreadStateTTL                  = 15 * time.Minute
	chatsProjectName                   = "Chats"
	chatsProjectWorkspaceKey           = "chats"
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
	Kind          string `json:"kind,omitempty"`
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
	codexProjects := loadCodexProjectState(s.codexGlobalStatePath)
	if !codexProjects.matchesAnyThread(threads) {
		codexProjects = codexProjectState{}
	}
	grouped := map[string]*projectWorkspace{}
	chats := []model.Thread{}
	for _, thread := range threads {
		if !threadVisibleInProjectCatalog(thread) {
			continue
		}
		if s.isCodexChatThread(thread) || codexProjects.isProjectlessThread(thread.ID) {
			chats = append(chats, thread)
			continue
		}
		cwdKey := model.NormalizePath(thread.CWD)
		if !codexProjects.allowProject(cwdKey) {
			continue
		}
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
	if len(codexProjects.order) > 0 {
		for _, cwd := range codexProjects.order {
			if workspace := grouped[model.NormalizePath(cwd)]; workspace != nil {
				workspaces = append(workspaces, *workspace)
			}
		}
	} else {
		for _, workspace := range grouped {
			workspaces = append(workspaces, *workspace)
		}
	}
	if len(codexProjects.order) == 0 {
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
	}
	sort.Slice(chats, func(i, j int) bool {
		if chats[i].UpdatedAt != chats[j].UpdatedAt {
			return chats[i].UpdatedAt > chats[j].UpdatedAt
		}
		return strings.ToLower(chats[i].Label()) < strings.ToLower(chats[j].Label())
	})
	if len(chats) > 0 {
		latest := chats[0]
		workspaces = append(workspaces, projectWorkspace{
			Key:               chatsProjectWorkspaceKey,
			ProjectName:       chatsProjectName,
			DirectoryName:     chatsProjectName,
			CWD:               s.codexChatsRoot(),
			LatestThread:      latest.ID,
			LatestThreadLabel: threadDisplayLabel(latest),
			ThreadCount:       len(chats),
			UpdatedAt:         latest.UpdatedAt,
		})
		sort.Slice(workspaces, func(i, j int) bool {
			leftChats := isChatsProjectWorkspace(workspaces[i])
			rightChats := isChatsProjectWorkspace(workspaces[j])
			if leftChats != rightChats {
				return !leftChats
			}
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
	}
	assignProjectWorkspaceKeys(workspaces)
	return projectCatalog{Workspaces: workspaces, Chats: chats}, nil
}

type codexProjectState struct {
	hasProjectList   bool
	allowedProjects  map[string]struct{}
	order            []string
	projectlessIDs   map[string]struct{}
	workspaceRootIDs map[string]string
}

func defaultCodexGlobalStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".codex", ".codex-global-state.json")
}

func loadCodexProjectState(path string) codexProjectState {
	if strings.TrimSpace(path) == "" {
		path = defaultCodexGlobalStatePath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return codexProjectState{}
	}
	var payload struct {
		ActiveWorkspaceRoots        []string          `json:"active-workspace-roots"`
		ElectronSavedWorkspaceRoots []string          `json:"electron-saved-workspace-roots"`
		ProjectOrder                []string          `json:"project-order"`
		ProjectlessThreadIDs        []string          `json:"projectless-thread-ids"`
		ThreadWorkspaceRootHints    map[string]string `json:"thread-workspace-root-hints"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return codexProjectState{}
	}
	state := codexProjectState{
		allowedProjects:  map[string]struct{}{},
		projectlessIDs:   map[string]struct{}{},
		workspaceRootIDs: map[string]string{},
	}
	for _, id := range payload.ProjectlessThreadIDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			state.projectlessIDs[trimmed] = struct{}{}
		}
	}
	for id, root := range payload.ThreadWorkspaceRootHints {
		if strings.TrimSpace(id) != "" && strings.TrimSpace(root) != "" {
			state.workspaceRootIDs[id] = model.NormalizePath(root)
		}
	}
	for _, root := range append(payload.ProjectOrder, payload.ElectronSavedWorkspaceRoots...) {
		normalized := model.NormalizePath(root)
		if normalized == "" {
			continue
		}
		if _, ok := state.allowedProjects[normalized]; !ok {
			state.order = append(state.order, normalized)
		}
		state.allowedProjects[normalized] = struct{}{}
	}
	for _, root := range payload.ActiveWorkspaceRoots {
		normalized := model.NormalizePath(root)
		if normalized == "" {
			continue
		}
		if _, ok := state.allowedProjects[normalized]; !ok {
			state.order = append([]string{normalized}, state.order...)
		}
		state.allowedProjects[normalized] = struct{}{}
	}
	state.hasProjectList = len(state.allowedProjects) > 0
	return state
}

func (s codexProjectState) allowProject(cwd string) bool {
	if !s.hasProjectList {
		return true
	}
	_, ok := s.allowedProjects[model.NormalizePath(cwd)]
	return ok
}

func (s codexProjectState) isProjectlessThread(threadID string) bool {
	if strings.TrimSpace(threadID) == "" {
		return false
	}
	if _, ok := s.projectlessIDs[threadID]; ok {
		return true
	}
	root := s.workspaceRootIDs[threadID]
	return root != "" && isCodexChatsCWD(root)
}

func (s codexProjectState) matchesAnyThread(threads []model.Thread) bool {
	if !s.hasProjectList {
		return false
	}
	for _, thread := range threads {
		if s.allowProject(thread.CWD) {
			return true
		}
	}
	return false
}

func (s *Service) projectWorkspaces(ctx context.Context) ([]projectWorkspace, error) {
	catalog, err := s.projectCatalog(ctx)
	if err != nil {
		return nil, err
	}
	return catalog.Workspaces, nil
}

func realProjectWorkspaceCount(workspaces []projectWorkspace) int {
	count := 0
	for _, workspace := range workspaces {
		if isChatsProjectWorkspace(workspace) {
			continue
		}
		count++
	}
	return count
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
		if strings.TrimSpace(workspaces[i].Key) != "" {
			seen[workspaces[i].Key]++
			continue
		}
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

func projectWorkspaceDisplayLabel(lang string, workspace projectWorkspace) string {
	if isChatsProjectWorkspace(workspace) {
		return localized(lang, "对话", "Chats")
	}
	return firstNonEmpty(workspace.ProjectName, workspace.DirectoryName, workspace.Key, "Project")
}

func projectWorkspaceDisplayButtonLabel(lang string, index int, workspace projectWorkspace) string {
	return shortButtonLabel(fmt.Sprintf("%d. %s", index, projectWorkspaceDisplayLabel(lang, workspace)))
}

func isChatsProjectWorkspace(workspace projectWorkspace) bool {
	return strings.TrimSpace(workspace.Key) == chatsProjectWorkspaceKey || strings.EqualFold(strings.TrimSpace(workspace.ProjectName), chatsProjectName)
}

func newThreadLabelForWorkspace(lang string, workspace projectWorkspace) string {
	if isChatsProjectWorkspace(workspace) {
		return localized(lang, "新建临时对话", "New chat")
	}
	return localized(lang, "新建会话", "New thread")
}

func newThreadTrailingForWorkspace(lang string, workspace projectWorkspace) string {
	if isChatsProjectWorkspace(workspace) {
		return localized(lang, "开始一个临时对话", "Start a temporary chat")
	}
	return localized(lang, "在此项目中开始", "Start in this project")
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
	if len(catalog.Workspaces) == 0 {
		return &DirectResponse{Text: localized(lang, "还没有缓存项目。请试试 /status，或等待同步。", "No cached projects yet. Try /status or wait for sync.")}, nil
	}
	projectWorkspaces := []projectWorkspace{}
	var chatsWorkspace *projectWorkspace
	for _, workspace := range catalog.Workspaces {
		if isChatsProjectWorkspace(workspace) {
			copy := workspace
			chatsWorkspace = &copy
			continue
		}
		projectWorkspaces = append(projectWorkspaces, workspace)
	}
	lines := []string{
		localized(lang, "项目", "Projects"),
		fmt.Sprintf("%s %d/%d", localized(lang, "项目", "Projects"), len(projectWorkspaces), len(projectWorkspaces)),
	}
	buttons := [][]model.ButtonSpec{
		{
			s.callbackButton(ctx, localized(lang, "关闭", "Close"), "projects_close", "", "", "", nil),
		},
	}
	sections := []model.MessageSection{}
	projectSection := model.MessageSection{Text: localized(lang, "项目", "Projects"), Heading: true}
	for index, workspace := range projectWorkspaces {
		displayIndex := index + 1
		label := projectWorkspaceDisplayLabel(lang, workspace)
		latest := firstNonEmpty(workspace.LatestThreadLabel, workspace.LatestThread)
		trailing := fmt.Sprintf("%d chats · %s", workspace.ThreadCount, threadUpdatedAtLabel(workspace.UpdatedAt, s.now))
		lines = append(lines,
			fmt.Sprintf("%d. %s", displayIndex, label),
			fmt.Sprintf("   %s · %s", trailing, latest),
		)
		button := s.callbackButton(ctx, projectWorkspaceDisplayButtonLabel(lang, displayIndex, workspace), "project_open", "", "", "", projectWorkspacePayload(workspace))
		buttons = append(buttons, []model.ButtonSpec{button})
		row := model.MessageSectionRow{
			Title:    label,
			Trailing: fmt.Sprintf("%s · %s", trailing, latest),
			Button:   button,
		}
		projectSection.Rows = append(projectSection.Rows, row)
	}
	if len(projectSection.Rows) > 0 {
		sections = append(sections, projectSection)
	}
	if chatsWorkspace != nil {
		displayIndex := len(projectWorkspaces) + 1
		label := projectWorkspaceDisplayLabel(lang, *chatsWorkspace)
		latest := firstNonEmpty(chatsWorkspace.LatestThreadLabel, chatsWorkspace.LatestThread)
		trailing := fmt.Sprintf("%d chats · %s", chatsWorkspace.ThreadCount, threadUpdatedAtLabel(chatsWorkspace.UpdatedAt, s.now))
		lines = append(lines,
			fmt.Sprintf("%d. %s", displayIndex, label),
			fmt.Sprintf("   %s · %s", trailing, latest),
		)
		button := s.callbackButton(ctx, projectWorkspaceDisplayButtonLabel(lang, displayIndex, *chatsWorkspace), "project_open", "", "", "", projectWorkspacePayload(*chatsWorkspace))
		buttons = append(buttons, []model.ButtonSpec{button})
		chatsSection := model.MessageSection{Text: label, Heading: true, Divider: len(sections) > 0}
		chatsSection.Rows = append(chatsSection.Rows, model.MessageSectionRow{
			Title:    label,
			Trailing: fmt.Sprintf("%s · %s", trailing, latest),
			Button:   button,
		})
		if len(sections) == 0 {
			chatsSection.Divider = false
		}
		sections = append(sections, chatsSection)
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Sections: sections, Buttons: buttons}, nil
}

func threadVisibleInProjectCatalog(thread model.Thread) bool {
	if thread.Archived {
		return false
	}
	if threadMarkedArchivedOrDeletedForOverview(thread) {
		return false
	}
	if threadLooksUnavailableForOverview(thread) {
		return false
	}
	return true
}

func threadMarkedArchivedOrDeletedForOverview(thread model.Thread) bool {
	var payload map[string]any
	if len(thread.Raw) == 0 || json.Unmarshal(thread.Raw, &payload) != nil {
		return false
	}
	return payloadBoolish(payload["archived"]) ||
		payloadBoolish(payload["isArchived"]) ||
		payloadBoolish(payload["deleted"]) ||
		payloadBoolish(payload["isDeleted"]) ||
		payloadNestedBoolish(payload, "thread", "archived") ||
		payloadNestedBoolish(payload, "thread", "isArchived") ||
		payloadNestedBoolish(payload, "thread", "deleted") ||
		payloadNestedBoolish(payload, "thread", "isDeleted")
}

func payloadNestedBoolish(payload map[string]any, parent, key string) bool {
	nested, ok := payload[parent].(map[string]any)
	return ok && payloadBoolish(nested[key])
}

func payloadBoolish(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes":
			return true
		}
	case float64:
		return typed != 0
	case json.Number:
		return typed.String() != "" && typed.String() != "0"
	}
	return false
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
	if strings.EqualFold(strings.TrimSpace(requested.ProjectName), chatsProjectName) || strings.TrimSpace(requested.Key) == chatsProjectWorkspaceKey {
		catalog, err := s.projectCatalog(ctx)
		if err != nil {
			return projectWorkspace{}, false
		}
		for _, workspace := range catalog.Workspaces {
			if strings.TrimSpace(workspace.Key) == chatsProjectWorkspaceKey {
				return workspace, true
			}
		}
		return requested, strings.TrimSpace(requested.CWD) != ""
	}
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
	label := projectWorkspaceDisplayLabel(lang, workspace)
	lines := []string{
		label,
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
	label := projectWorkspaceDisplayLabel(lang, workspace)
	lines := []string{fmt.Sprintf(localized(lang, "%s 的会话", "Threads for %s"), label)}
	buttons := [][]model.ButtonSpec{}
	section := model.MessageSection{Text: label, Heading: true}
	visibleThreadCount := 0
	for _, thread := range threads {
		if !threadVisibleInProjectCatalog(thread) {
			continue
		}
		if !s.threadBelongsToProjectWorkspace(thread, workspace) {
			continue
		}
		label := threadOverviewLabel(thread, lang)
		updatedAt := threadUpdatedAtLabel(thread.UpdatedAt, s.now)
		button := s.callbackButton(ctx, localized(lang, "打开", "Open"), "show_thread", thread.ID, "", "", nil)
		lines = append(lines, fmt.Sprintf("- %s    %s", label, updatedAt))
		buttons = append(buttons, []model.ButtonSpec{button})
		visibleThreadCount++
		section.Rows = append(section.Rows, model.MessageSectionRow{
			Title:    label,
			Trailing: updatedAt,
			Button:   button,
		})
	}
	newThreadLabel := newThreadLabelForWorkspace(lang, workspace)
	newThreadButton := s.callbackButton(ctx, newThreadLabel, "project_new_thread", "", "", "", projectWorkspacePayload(workspace))
	lines = append(lines, fmt.Sprintf("- %s", newThreadLabel))
	buttons = append(buttons, []model.ButtonSpec{newThreadButton})
	section.Rows = append(section.Rows, model.MessageSectionRow{
		Title:           newThreadLabel,
		Trailing:        newThreadTrailingForWorkspace(lang, workspace),
		BackgroundStyle: "cus-4",
		Button:          newThreadButton,
	})
	if visibleThreadCount == 0 {
		lines = append(lines, localized(lang, "这个项目还没有缓存会话。", "No cached threads for this project."))
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Sections: []model.MessageSection{section}, Buttons: buttons}, nil
}

func (s *Service) threadBelongsToProjectWorkspace(thread model.Thread, workspace projectWorkspace) bool {
	if strings.EqualFold(strings.TrimSpace(workspace.ProjectName), chatsProjectName) || strings.TrimSpace(workspace.Key) == chatsProjectWorkspaceKey {
		return s.isCodexChatThread(thread)
	}
	return model.NormalizePath(thread.CWD) == model.NormalizePath(workspace.CWD)
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
		Kind:          pendingNewThreadKind(workspace),
		ExpiresAt:     time.Now().UTC().Add(newThreadStateTTL).Format(time.RFC3339Nano),
	}
	payloadBytes, _ := json.Marshal(state)
	if err := s.store.SetState(ctx, newThreadStateKey(chatID, topicID), string(payloadBytes)); err != nil {
		return nil, err
	}
	return &DirectResponse{Text: fmt.Sprintf(localized(lang, "%s 的新线程。\n请在当前聊天中发送第一条提示词；我会用这条消息创建 Codex 会话和话题。", "New thread for %s.\nSend the first prompt in this chat. I will create the Codex session and topic from that message."), projectWorkspaceDisplayLabel(lang, workspace))}, nil
}

func pendingNewThreadKind(workspace projectWorkspace) string {
	if strings.EqualFold(strings.TrimSpace(workspace.ProjectName), chatsProjectName) || strings.TrimSpace(workspace.Key) == chatsProjectWorkspaceKey {
		return chatsProjectWorkspaceKey
	}
	return ""
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
	return s.createThreadFromProjectPrompt(ctx, chatID, topicID, pendingNewThreadState{
		Kind:        chatsProjectWorkspaceKey,
		ProjectName: chatsProjectName,
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
	if strings.TrimSpace(state.Kind) == chatsProjectWorkspaceKey {
		state.ProjectName = chatsProjectName
		state.DirectoryName = chatsProjectName
		state.CWD = ""
	}
	if response, ok, err := s.createThreadFromProjectPromptViaDesktop(ctx, chatID, topicID, state, prompt, sourceMode); ok || err != nil {
		return response, err
	}
	if strings.EqualFold(strings.TrimSpace(state.ProjectName), chatsProjectName) {
		return &DirectResponse{Text: localized(lang, "无法自动在 Codex 桌面端创建临时对话。请确认已给 ctr-go/osascript 辅助功能权限后重试。", "Could not create a Codex desktop Chat automatically. Grant Accessibility permission to ctr-go/osascript and try again.")}, nil
	}
	return s.createThreadFromProjectPromptViaAppServer(ctx, chatID, topicID, state, prompt, sourceMode)
}

func (s *Service) createThreadFromProjectPromptViaDesktop(ctx context.Context, chatID, topicID int64, state pendingNewThreadState, prompt, sourceMode string) (*DirectResponse, bool, error) {
	if s == nil || !s.cfg.OpenCodexDesktopOnFeishu || normalizeInputSourceMode(sourceMode) != model.PanelSourceFeishuInput {
		return nil, false, nil
	}
	s.mu.RLock()
	live := s.live
	connected := s.liveConnected
	creator := s.desktopThreadCreator
	s.mu.RUnlock()
	if !connected || live == nil {
		return nil, false, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	if creator == nil {
		creator = createCodexDesktopThreadWithPrompt
	}
	projectless := strings.EqualFold(strings.TrimSpace(state.ProjectName), chatsProjectName)
	started := time.Now()
	err := creator(requestCtx, state.CWD, prompt, projectless)
	s.logLifecycle("codex_desktop_thread_create", lifecycleFields{
		"cwd":          state.CWD,
		"project_name": state.ProjectName,
		"projectless":  projectless,
		"error":        err,
	})
	if err != nil {
		return nil, false, nil
	}
	thread, err := s.findDesktopCreatedThread(ctx, live, state, prompt, started)
	if err != nil {
		s.logLifecycle("codex_desktop_thread_create_lookup_failed", lifecycleFields{
			"cwd":          state.CWD,
			"project_name": state.ProjectName,
			"error":        sanitizeDiagnosticString(err.Error()),
		})
		return nil, false, nil
	}
	if strings.TrimSpace(thread.ID) == "" {
		return nil, false, nil
	}
	if err := s.store.UpsertThread(ctx, thread); err != nil {
		return nil, true, err
	}
	turnID := thread.ActiveTurnID
	if strings.TrimSpace(turnID) == "" {
		turnID = latestTurnIDFromThreadRaw(thread.Raw)
	}
	if strings.TrimSpace(turnID) != "" {
		_ = s.markInputOriginTurn(ctx, thread.ID, turnID, sourceMode, chatID, topicID)
	}
	s.ensureNewChatThreadFallback(ctx, thread.ID, state)
	target := model.ObserverTarget{ChatKey: model.ChatKey(chatID, topicID), ChatID: chatID, TopicID: topicID, Enabled: true}
	s.syncThreadPanelToTarget(ctx, target, thread.ID, true, sourceMode)
	if strings.TrimSpace(turnID) != "" {
		s.startChatOriginHotPoll(ctx, thread.ID, turnID)
	}
	response := &DirectResponse{ThreadID: thread.ID, TurnID: turnID}
	return s.feishuVisibleOpenResponse(ctx, response, thread.ID), true, nil
}

func (s *Service) createThreadFromProjectPromptViaAppServer(ctx context.Context, chatID, topicID int64, state pendingNewThreadState, prompt, sourceMode string) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	sourceMode = normalizeInputSourceMode(sourceMode)
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
	if refreshed, refreshErr := s.refreshThreadForOperation(ctx, live, thread.ID, "refresh_new_thread_after_start"); refreshErr != nil {
		s.logLifecycle("thread_refresh_failed", lifecycleFields{
			"operation": "refresh_new_thread_after_start",
			"thread_id": thread.ID,
			"turn_id":   turnID,
			"error":     refreshErr,
		})
	} else if refreshed != nil {
		thread = *refreshed
	}
	s.ensureNewChatThreadFallback(ctx, thread.ID, state)
	target := model.ObserverTarget{ChatKey: model.ChatKey(chatID, topicID), ChatID: chatID, TopicID: topicID, Enabled: true}
	s.syncThreadPanelToTarget(ctx, target, thread.ID, true, sourceMode)
	if strings.TrimSpace(turnID) != "" {
		s.startChatOriginHotPoll(ctx, thread.ID, turnID)
	}
	s.maybeAdoptCodexDesktopThreadForInput(ctx, thread.ID, sourceMode)
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

func (s *Service) findDesktopCreatedThread(ctx context.Context, live Session, state pendingNewThreadState, prompt string, startedAt time.Time) (model.Thread, error) {
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for {
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		result, err := live.ThreadList(requestCtx, 25, "")
		cancel()
		if err != nil {
			lastErr = err
		} else if thread, ok := newestMatchingDesktopCreatedThread(appserver.ThreadsFromList(result), state, prompt, startedAt); ok {
			if refreshed, refreshErr := s.refreshThreadForOperation(ctx, live, thread.ID, "refresh_desktop_created_thread"); refreshErr == nil && refreshed != nil {
				thread = *refreshed
			}
			thread = mergeThreadMetadata(thread, threadFromStartPayload(map[string]any{"thread": map[string]any{"id": thread.ID, "cwd": thread.CWD}}, state))
			return thread, nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return model.Thread{}, lastErr
			}
			return model.Thread{}, fmt.Errorf("desktop-created thread was not found in thread/list")
		}
		timer := time.NewTimer(700 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return model.Thread{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func newestMatchingDesktopCreatedThread(threads []model.Thread, state pendingNewThreadState, prompt string, startedAt time.Time) (model.Thread, bool) {
	cwd := model.NormalizePath(state.CWD)
	matchChats := strings.EqualFold(strings.TrimSpace(state.ProjectName), chatsProjectName)
	prompt = strings.TrimSpace(prompt)
	startedUnix := startedAt.Add(-5 * time.Second).Unix()
	var best model.Thread
	for _, thread := range threads {
		if !matchChats && model.NormalizePath(thread.CWD) != cwd {
			continue
		}
		if thread.UpdatedAt != 0 && thread.UpdatedAt < startedUnix {
			continue
		}
		if prompt != "" && !threadMatchesFirstPrompt(thread, prompt) {
			continue
		}
		if best.ID == "" || thread.UpdatedAt > best.UpdatedAt {
			best = thread
		}
	}
	return best, best.ID != ""
}

func threadMatchesFirstPrompt(thread model.Thread, prompt string) bool {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return true
	}
	if strings.Contains(strings.TrimSpace(thread.LastPreview), prompt) || strings.Contains(strings.TrimSpace(thread.Title), prompt) {
		return true
	}
	var payload map[string]any
	if len(thread.Raw) == 0 || json.Unmarshal(thread.Raw, &payload) != nil {
		return false
	}
	raw, _ := json.Marshal(payload)
	return strings.Contains(string(raw), prompt)
}

func latestTurnIDFromThreadRaw(raw json.RawMessage) string {
	var payload map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	if nested, ok := payload["thread"].(map[string]any); ok && nested != nil {
		payload = nested
	}
	turns, _ := payload["turns"].([]any)
	if len(turns) == 0 {
		return ""
	}
	last, _ := turns[len(turns)-1].(map[string]any)
	return payloadMapString(last, "id")
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
