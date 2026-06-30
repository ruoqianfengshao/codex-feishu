package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mideco-tech/codex-tg/internal/appserver"
	"github.com/mideco-tech/codex-tg/internal/config"
	"github.com/mideco-tech/codex-tg/internal/control"
	"github.com/mideco-tech/codex-tg/internal/desktopipc"
	"github.com/mideco-tech/codex-tg/internal/model"
	"github.com/mideco-tech/codex-tg/internal/storage"
)

type Session = control.RuntimeSession

type Sender interface {
	SendMessage(ctx context.Context, chatID, topicID int64, text string, buttons [][]model.ButtonSpec, options model.SendOptions) (int64, error)
	SendRenderedMessages(ctx context.Context, chatID, topicID int64, messages []model.RenderedMessage, buttons [][]model.ButtonSpec, options model.SendOptions) ([]int64, error)
	EditMessage(ctx context.Context, chatID, topicID, messageID int64, text string, buttons [][]model.ButtonSpec) error
	EditRenderedMessage(ctx context.Context, chatID, topicID, messageID int64, rendered model.RenderedMessage, buttons [][]model.ButtonSpec) error
	DeleteMessage(ctx context.Context, chatID, topicID, messageID int64) error
	SendDocumentData(ctx context.Context, chatID, topicID int64, fileName string, data []byte, caption string, options model.SendOptions) (int64, error)
}

type ThreadTopicSender interface {
	EnsureThreadTopic(ctx context.Context, chatID int64, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, sourceMode string) (*model.FeishuThreadTopic, error)
}

type ThreadTopicTargetResolver interface {
	ResolveThreadTopicTarget(ctx context.Context, chatID int64) (int64, error)
}

type DirectResponse struct {
	Text         string
	Sections     []model.MessageSection
	CallbackText string
	Buttons      [][]model.ButtonSpec
	ThreadID     string
	TurnID       string
	ItemID       string
	EventID      string
}

func silentSendOptions() model.SendOptions {
	return model.SendOptions{Silent: true}
}

func notifySendOptions() model.SendOptions {
	return model.SendOptions{}
}

func (s *Service) runNoticeSendOptions() model.SendOptions {
	if s != nil && s.cfg.NotifyNewRun {
		return notifySendOptions()
	}
	return silentSendOptions()
}

type Service struct {
	cfg   config.Config
	store *storage.Store

	liveFactory func() Session
	pollFactory func() Session

	appServerListen        string
	sessionMu              sync.Mutex
	mu                     sync.RWMutex
	live                   Session
	poll                   Session
	liveEvents             <-chan appserver.Event
	liveGeneration         uint64
	pollGeneration         uint64
	cancel                 context.CancelFunc
	wg                     sync.WaitGroup
	panelMu                sync.Mutex
	sender                 Sender
	notifier               Notifier
	desktopOpener          func(context.Context, string) error
	desktopInputDispatcher desktopInputDispatcher
	notificationMu         sync.Mutex
	logger                 *log.Logger
	diagnosticMu           sync.Mutex
	diagnosticWin          time.Time
	diagnosticN            int
	diagnosticBy           map[string]int
	diagnosticLast         map[string]time.Time
	now                    func() time.Time
	started                bool
	startedAt              time.Time
	ready                  bool
	phase                  string
	lastError              string
	liveConnected          bool
	pollConnected          bool
}

const (
	observerRecentThreadLimit = 50
	collaborationModeDefault  = "default"
	collaborationModePlan     = "plan"
	codexModelStateKey        = "codex.model"
	codexReasoningStateKey    = "codex.reasoning_effort"
	botLanguageStateKey       = "bot.language"
	botLanguageEnglish        = "en"
	botLanguageChinese        = "zh"
	chatOriginHotPollMax      = 75 * time.Second
	chatOriginHotPollTick     = 3 * time.Second
	appServerStdioListen      = "stdio://"
)

var (
	codexThreadIDPattern        = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	codexThreadIDExtractPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

func New(cfg config.Config) (*Service, error) {
	if err := cfg.Paths.Ensure(); err != nil {
		return nil, err
	}
	store, err := storage.Open(cfg.Paths.DBPath)
	if err != nil {
		return nil, err
	}
	service := &Service{
		cfg:                    cfg,
		store:                  store,
		appServerListen:        cfg.AppServerListen,
		notifier:               newSystemNotifier(),
		desktopOpener:          openCodexDesktopThread,
		desktopInputDispatcher: desktopipc.New("", cfg.RequestTimeout),
		logger:                 discardDiagnosticLogger(),
		diagnosticBy:           map[string]int{},
		diagnosticLast:         map[string]time.Time{},
		now:                    time.Now,
		phase:                  "created",
	}
	service.liveFactory = func() Session {
		return service.newAppServerSession()
	}
	service.pollFactory = func() Session {
		return service.newAppServerSession()
	}
	service.live = service.liveFactory()
	service.poll = service.pollFactory()
	return service, nil
}

func (s *Service) newAppServerSession() Session {
	codexBin := s.cfg.CodexBin
	listen := s.appServerListen
	cwd := s.cfg.DefaultCWD
	timeout := s.cfg.RequestTimeout
	return appserver.NewClient(codexBin, listen, cwd, timeout)
}

func (s *Service) Close() error {
	s.mu.Lock()
	cancel := s.cancel
	started := s.started
	s.started = false
	s.cancel = nil
	s.mu.Unlock()
	if started && cancel != nil {
		cancel()
	}
	s.wg.Wait()
	s.sessionMu.Lock()
	s.mu.Lock()
	live := s.live
	poll := s.poll
	desktopInputDispatcher := s.desktopInputDispatcher
	s.live = nil
	s.poll = nil
	s.liveEvents = nil
	s.liveConnected = false
	s.pollConnected = false
	s.liveGeneration++
	s.pollGeneration++
	s.mu.Unlock()
	s.sessionMu.Unlock()
	if live != nil {
		_ = live.Close()
	}
	if poll != nil {
		_ = poll.Close()
	}
	if closeable, ok := desktopInputDispatcher.(closeableDesktopInputDispatcher); ok {
		_ = closeable.Close()
	}
	return s.store.Close()
}

func (s *Service) SetSender(sender Sender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sender = sender
}

func (s *Service) Store() *storage.Store {
	return s.store
}

func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.started = true
	s.startedAt = time.Now().UTC()
	s.ready = true
	s.phase = "ready"
	s.lastError = ""
	s.liveConnected = false
	s.pollConnected = false
	s.mu.Unlock()

	_ = s.store.SetState(runCtx, "daemon.phase", "ready")
	_ = s.store.SetState(runCtx, "daemon.ready", "true")
	_ = s.store.SetState(runCtx, "daemon.started_at", s.startedAt.Format(time.RFC3339Nano))
	_ = s.store.SetState(runCtx, "daemon.last_error", "")
	s.cleanupTempArtifacts(runCtx)

	s.spawn(runCtx, s.ensureSessions)
	s.spawn(runCtx, s.indexLoop)
	s.spawn(runCtx, s.attachLoop)
	s.spawn(runCtx, s.pollLoop)
	s.spawn(runCtx, s.deliveryLoop)
	s.spawn(runCtx, s.controlLoop)
	return nil
}

func (s *Service) Doctor(ctx context.Context) (map[string]any, error) {
	backlog, _ := s.store.DeliveryQueueBacklog(ctx)
	state, _ := s.store.ListState(ctx)
	return map[string]any{
		"config":           s.cfg,
		"delivery_backlog": backlog,
		"daemon_state":     state,
	}, nil
}

func (s *Service) StatusSnapshot(ctx context.Context) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	threadCount, _ := s.store.CountThreads(ctx)
	catalog, _ := s.projectCatalog(ctx)
	backlog, _ := s.store.DeliveryQueueBacklog(ctx)
	s.mu.RLock()
	ready := s.ready
	liveConnected := s.liveConnected
	pollConnected := s.pollConnected
	startedAt := s.startedAt
	lastError := s.lastError
	s.mu.RUnlock()
	systemRows := []model.MessageSectionRow{
		{Title: localized(lang, "核心", "Core"), Trailing: readyLabelLang(lang, ready)},
		{Title: localized(lang, "运行时长", "Uptime"), Trailing: uptimeLabel(startedAt, s.now())},
	}
	startedRows := []model.MessageSectionRow{
		{Title: localized(lang, "启动时间", "Started"), Trailing: startedAtLabel(startedAt)},
	}
	if strings.TrimSpace(lastError) != "" {
		systemRows = append(systemRows, model.MessageSectionRow{Title: localized(lang, "最近错误", "Last error"), Trailing: trimPreview(lastError)})
	}
	sections := []model.MessageSection{
		{Text: localized(lang, "系统", "System"), Heading: true, Rows: systemRows},
		{Text: localized(lang, "启动", "Startup"), Heading: true, Divider: true, Rows: startedRows},
		{Text: "Codex", Heading: true, Divider: true, Rows: []model.MessageSectionRow{
			{Title: localized(lang, "实时会话", "Live session"), Trailing: onlineLabelLang(lang, liveConnected)},
			{Title: localized(lang, "轮询会话", "Poll session"), Trailing: onlineLabelLang(lang, pollConnected)},
			{Title: localized(lang, "桌面输入", "Desktop input"), Trailing: onOffLabelLang(lang, s.cfg.OpenCodexDesktopOnFeishu)},
		}},
		{Text: localized(lang, "会话", "Threads"), Heading: true, Divider: true, Rows: []model.MessageSectionRow{
			{Title: localized(lang, "缓存会话", "Cached threads"), Trailing: fmt.Sprintf("%d", threadCount)},
			{Title: localized(lang, "项目数量", "Projects"), Trailing: fmt.Sprintf("%d", len(catalog.Workspaces))},
			{Title: localized(lang, "临时会话", "Chats"), Trailing: fmt.Sprintf("%d", len(catalog.Chats))},
			{Title: localized(lang, "跟踪会话", "Tracked threads"), Trailing: fmt.Sprintf("%d", threadCount)},
			{Title: localized(lang, "发送队列", "Delivery backlog"), Trailing: fmt.Sprintf("%d", backlog)},
		}},
		{Text: "飞书", Heading: true, Divider: true, Rows: []model.MessageSectionRow{
			{Title: localized(lang, "语言", "Language"), Trailing: languageLabel(lang)},
			{Title: localized(lang, "话题模式", "Topic mode"), Trailing: localized(lang, "单聊话题", "P2P topic")},
		}, Buttons: [][]model.ButtonSpec{
			{
				s.callbackButton(ctx, selectedButtonLabel("中文", lang == botLanguageChinese), "settings_language_set", "status", "", "", map[string]any{"value": botLanguageChinese, "return": "status"}),
				s.callbackButton(ctx, selectedButtonLabel("English", lang == botLanguageEnglish), "settings_language_set", "status", "", "", map[string]any{"value": botLanguageEnglish, "return": "status"}),
			},
		}},
	}
	return &DirectResponse{Text: localized(lang, "Codex 状态总览", "Codex Remote Status"), Sections: sections}, nil
}

func readyLabel(ready bool) string {
	if ready {
		return "Ready"
	}
	return "Not ready"
}

func onlineLabel(connected bool) string {
	if connected {
		return "Online"
	}
	return "Offline"
}

func onOffLabel(enabled bool) string {
	if enabled {
		return "On"
	}
	return "Off"
}

func uptimeLabel(startedAt, now time.Time) string {
	if startedAt.IsZero() {
		return "Unknown"
	}
	return formatToolDuration(now.Sub(startedAt))
}

func startedAtLabel(startedAt time.Time) string {
	if startedAt.IsZero() {
		return "Unknown"
	}
	return startedAt.Format("2006-01-02 15:04:05")
}

func (s *Service) HandleMessageFromSource(ctx context.Context, chatID, topicID, userID int64, text string, replyToMessageID int64, sourceMode string) (*DirectResponse, error) {
	if !s.IsAllowed(userID, chatID) {
		return nil, nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return &DirectResponse{Text: s.t(ctx, "目前只支持纯文本消息。请发送文本，或使用 /context 查看路由。", "Plain text messages only right now. Send text, or use /context for routing help.")}, nil
	}
	if strings.HasPrefix(text, "/") {
		return s.handleCommandFromSource(ctx, chatID, topicID, text, replyToMessageID, sourceMode)
	}
	return s.handlePlainTextFromSource(ctx, chatID, topicID, text, replyToMessageID, sourceMode)
}

func (s *Service) HandleCallbackFromSource(ctx context.Context, chatID, topicID, messageID, userID int64, token, sourceMode string) (*DirectResponse, error) {
	if !s.IsAllowed(userID, chatID) {
		return nil, nil
	}
	route, err := s.store.GetCallbackRoute(ctx, token)
	if err != nil {
		return nil, err
	}
	if route == nil || route.Status != model.CallbackStatusActive {
		return &DirectResponse{Text: s.t(ctx, "这个按钮已过期。请使用 /show <thread> 或 /repair。", "This button is stale. Use /show <thread> or /repair.")}, nil
	}
	var payload map[string]any
	if route.PayloadJSON != "" {
		_ = json.Unmarshal([]byte(route.PayloadJSON), &payload)
	}
	switch route.Action {
	case "details_open", "details_prev", "details_next", "details_back", "details_tool_toggle":
		return s.handleDetailsCallback(ctx, chatID, topicID, messageID, route, payload)
	case "details_tools_file":
		return s.sendDetailsToolsFile(ctx, chatID, topicID, messageID, route, payload)
	case "full_log_file":
		return s.sendPanelFullLogFile(ctx, chatID, topicID, messageID, route, payload, sourceMode)
	case "turn_off_plan":
		return s.handleTurnOffPlanCallback(ctx, chatID, topicID, messageID, route, payload)
	case "workspace_overview":
		return s.workspaceOverview(ctx)
	case "workspace_threads":
		return s.threadsOverview(ctx, "")
	case "workspace_projects":
		return s.projectsOverview(ctx)
	case "workspace_status":
		response, err := s.StatusSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		response.CallbackText = s.t(ctx, "状态", "Status")
		return response, nil
	case "settings_overview":
		return s.editOrSendSettingsResponse(ctx, chatID, topicID, messageID, "Settings", s.codexSettingsOverview)
	case "settings_model_menu":
		return s.editOrSendSettingsResponse(ctx, chatID, topicID, messageID, "Model", s.codexModelMenu)
	case "settings_reasoning_menu":
		return s.editOrSendSettingsResponse(ctx, chatID, topicID, messageID, "Reasoning", s.codexReasoningMenu)
	case "settings_language_menu":
		return s.editOrSendSettingsResponse(ctx, chatID, topicID, messageID, "Language", s.botLanguageMenu)
	case "settings_model_set":
		return s.setCodexModel(ctx, chatID, topicID, messageID, payload)
	case "settings_reasoning_set":
		return s.setCodexReasoningEffort(ctx, chatID, topicID, messageID, payload)
	case "settings_language_set":
		return s.setBotLanguage(ctx, chatID, topicID, messageID, payload)
	case "projects_page":
		return s.projectsPage(ctx, chatID, topicID, messageID, payload)
	case "projects_close":
		return s.closeProjectsMenu(ctx, chatID, topicID, messageID)
	case "chats_open", "chats_page":
		return s.chatsPage(ctx, chatID, topicID, messageID, payload)
	case "chat_open":
		if normalizeInputSourceMode(sourceMode) == model.PanelSourceFeishuInput {
			s.openFeishuThreadAsync(ctx, chatID, topicID, route.ThreadID, func(openCtx context.Context) (*DirectResponse, error) {
				return s.openChatThread(openCtx, chatID, topicID, route.ThreadID, sourceMode)
			})
			return &DirectResponse{CallbackText: s.t(ctx, "正在打开话题…", "Opening topic..."), ThreadID: route.ThreadID}, nil
		}
		response, err := s.openChatThread(ctx, chatID, topicID, route.ThreadID, sourceMode)
		if err != nil {
			return nil, err
		}
		return response, nil
	case "project_open":
		return s.projectThreads(ctx, payload)
	case "project_new_thread":
		return s.armProjectNewThread(ctx, chatID, topicID, payload)
	case "project_threads":
		return s.projectThreads(ctx, payload)
	case "show_thread":
		if normalizeInputSourceMode(sourceMode) == model.PanelSourceFeishuInput {
			s.openFeishuThreadAsync(ctx, chatID, topicID, route.ThreadID, func(openCtx context.Context) (*DirectResponse, error) {
				return s.showThread(openCtx, chatID, topicID, route.ThreadID, true, sourceMode)
			})
			return &DirectResponse{CallbackText: s.t(ctx, "正在打开话题…", "Opening topic..."), ThreadID: route.ThreadID}, nil
		}
		response, err := s.showThread(ctx, chatID, topicID, route.ThreadID, true, sourceMode)
		if err != nil {
			return nil, err
		}
		return response, nil
	case "show_context":
		text, err := s.contextCard(ctx, chatID, topicID)
		if err != nil {
			return nil, err
		}
		return &DirectResponse{Text: text}, nil
	case "get_thread_id":
		return s.threadIDResponse(ctx, route.ThreadID, route.TurnID), nil
	case "reply_hint":
		return &DirectResponse{Text: fmt.Sprintf(s.t(ctx, "使用下面命令回复这个会话：\n/reply %s <text>", "Reply to this thread with:\n/reply %s <text>"), route.ThreadID)}, nil
	case "stop_turn":
		return s.interruptTurn(ctx, chatID, topicID, route.ThreadID, route.TurnID)
	case "arm_steer":
		panel, _ := s.store.GetCurrentThreadPanel(ctx, chatID, topicID, route.ThreadID)
		panelID := int64(0)
		if panel != nil {
			panelID = panel.ID
		}
		if err := s.armSteer(ctx, chatID, topicID, route.ThreadID, route.TurnID, panelID); err != nil {
			return nil, err
		}
		return &DirectResponse{CallbackText: s.t(ctx, "下一条消息会发送到这个 thread。", "The next message will go to this thread.")}, nil
	case "approve", "approve_session":
		decision := "accept"
		if route.Action == "approve_session" {
			decision = "acceptForSession"
		}
		return s.approve(ctx, route.RequestID, decision)
	case "deny", "cancel":
		decision := "decline"
		if route.Action == "cancel" {
			decision = "cancel"
		}
		return s.approve(ctx, route.RequestID, decision)
	case "answer_choice":
		return s.answerChoice(ctx, chatID, topicID, route, sourceMode)
	case "get_full_log":
		return s.sendFullLogArchive(ctx, chatID, topicID, messageID, route.ThreadID, sourceMode)
	default:
		return &DirectResponse{Text: s.t(ctx, "这个按钮暂未在 Go core 中实现。", "This button is not implemented in the Go core yet.")}, nil
	}
}

func (s *Service) feishuVisibleOpenResponse(ctx context.Context, response *DirectResponse, threadID string) *DirectResponse {
	threadID = strings.TrimSpace(threadID)
	if response == nil {
		response = &DirectResponse{ThreadID: threadID}
	}
	if strings.TrimSpace(response.Text) != "" {
		return response
	}
	if strings.TrimSpace(response.ThreadID) == "" {
		response.ThreadID = threadID
	}
	label := strings.TrimSpace(response.ThreadID)
	if thread, _ := s.store.GetThread(ctx, response.ThreadID); thread != nil {
		label = firstNonEmpty(strings.TrimSpace(thread.Title), thread.ShortID())
	}
	response.Text = strings.Join([]string{
		s.t(ctx, "已打开 Codex 会话话题", "Codex thread topic opened"),
		fmt.Sprintf("%s: %s", s.t(ctx, "会话", "Thread"), label),
		fmt.Sprintf("Thread ID: %s", response.ThreadID),
		s.t(ctx, "后续消息会在对应话题中更新；在话题里直接回复即可继续该 Codex 会话。", "Future updates will appear in the linked topic; reply in that topic to continue this Codex thread."),
	}, "\n")
	if strings.TrimSpace(response.CallbackText) == "" {
		response.CallbackText = s.t(ctx, "已打开话题", "Topic opened")
	}
	return response
}

func (s *Service) openFeishuThreadAsync(ctx context.Context, chatID, topicID int64, threadID string, open func(context.Context) (*DirectResponse, error)) {
	if open == nil {
		return
	}
	timeout := s.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	s.spawn(context.Background(), func(context.Context) {
		openCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		response, err := open(openCtx)
		if err != nil {
			s.setError(ctx, err)
			return
		}
		s.mu.RLock()
		sender := s.sender
		s.mu.RUnlock()
		visible := s.feishuVisibleOpenResponse(openCtx, response, threadID)
		if sender != nil && visible != nil && strings.TrimSpace(visible.Text) != "" {
			if _, err := sender.SendMessage(openCtx, chatID, topicID, visible.Text, visible.Buttons, model.SendOptions{Silent: true}); err != nil {
				s.setError(ctx, err)
			}
		}
	})
}

func (s *Service) RegisterDirectDelivery(ctx context.Context, chatID, topicID, messageID int64, response *DirectResponse) error {
	if response == nil || response.ThreadID == "" {
		return nil
	}
	return s.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    chatID,
		TopicID:   topicID,
		MessageID: messageID,
		ThreadID:  response.ThreadID,
		TurnID:    response.TurnID,
		ItemID:    response.ItemID,
		EventID:   response.EventID,
		CreatedAt: model.NowString(),
	})
}

func (s *Service) RequestRepair(ctx context.Context, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "manual"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_ = s.store.SetState(ctx, "repair.last_reason", reason)
	_ = s.store.SetState(ctx, "repair.last_at", now)
	s.logLifecycle("repair_requested", lifecycleFields{"reason": reason})
	return s.store.SetState(ctx, "control.repair_request", fmt.Sprintf("%s|%s", now, reason))
}

func (s *Service) IsAllowed(userID, chatID int64) bool {
	return true
}

func (s *Service) spawn(ctx context.Context, fn func(context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn(ctx)
	}()
}

func (s *Service) ensureSessions(ctx context.Context) {
	s.ensureSessionLifecycle(ctx)
	s.bootstrapTrackedState(ctx)
}

func (s *Service) ensureLiveSession(ctx context.Context) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.ensureLiveSessionLocked(ctx)
}

func (s *Service) ensureLiveSessionLocked(ctx context.Context) {
	s.mu.RLock()
	client := s.live
	connected := s.liveConnected
	s.mu.RUnlock()
	if client == nil || connected {
		return
	}
	sessionCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	started := time.Now()
	s.logLifecycle("appserver_session_start", lifecycleFields{"role": "live"})
	if err := client.Start(sessionCtx); err != nil {
		_ = s.store.SetState(ctx, "appserver.live.last_error", sanitizeDiagnosticString(err.Error()))
		s.logLifecycle("appserver_session_start_failed", lifecycleFields{
			"role":        "live",
			"duration_ms": time.Since(started).Milliseconds(),
			"error":       err,
			"stderr_tail": sanitizedStderrTail(client),
		})
		s.setError(ctx, err)
		if s.fallbackProxyAppServerToStdioLocked(ctx, "live", err) {
			s.ensureLiveSessionLocked(ctx)
		}
		return
	}
	events := client.Subscribe()
	s.mu.Lock()
	s.liveConnected = true
	s.liveEvents = events
	s.liveGeneration++
	generation := s.liveGeneration
	s.mu.Unlock()
	_ = s.store.SetState(ctx, "appserver.live_connected", "true")
	_ = s.store.SetState(ctx, "appserver.live.generation", strconv.FormatUint(generation, 10))
	_ = s.store.SetState(ctx, "appserver.live.last_started_at", time.Now().UTC().Format(time.RFC3339Nano))
	_ = s.store.SetState(ctx, "appserver.live.last_error", "")
	s.logLifecycle("appserver_session_started", lifecycleFields{
		"role":        "live",
		"generation":  generation,
		"duration_ms": time.Since(started).Milliseconds(),
	})
	s.spawn(ctx, func(loopCtx context.Context) {
		s.liveEventLoop(loopCtx, client, events, generation)
	})
}

func (s *Service) ensurePollSession(ctx context.Context) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.ensurePollSessionLocked(ctx)
}

func (s *Service) ensurePollSessionLocked(ctx context.Context) {
	s.mu.RLock()
	client := s.poll
	connected := s.pollConnected
	s.mu.RUnlock()
	if client == nil || connected {
		return
	}
	sessionCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	started := time.Now()
	s.logLifecycle("appserver_session_start", lifecycleFields{"role": "poll"})
	if err := client.Start(sessionCtx); err != nil {
		_ = s.store.SetState(ctx, "appserver.poll.last_error", sanitizeDiagnosticString(err.Error()))
		s.logLifecycle("appserver_session_start_failed", lifecycleFields{
			"role":        "poll",
			"duration_ms": time.Since(started).Milliseconds(),
			"error":       err,
			"stderr_tail": sanitizedStderrTail(client),
		})
		s.setError(ctx, err)
		if s.fallbackProxyAppServerToStdioLocked(ctx, "poll", err) {
			s.ensurePollSessionLocked(ctx)
		}
		return
	}
	s.mu.Lock()
	s.pollConnected = true
	s.pollGeneration++
	generation := s.pollGeneration
	s.mu.Unlock()
	_ = s.store.SetState(ctx, "appserver.poll_connected", "true")
	_ = s.store.SetState(ctx, "appserver.poll.generation", strconv.FormatUint(generation, 10))
	_ = s.store.SetState(ctx, "appserver.poll.last_started_at", time.Now().UTC().Format(time.RFC3339Nano))
	_ = s.store.SetState(ctx, "appserver.poll.last_error", "")
	s.logLifecycle("appserver_session_started", lifecycleFields{
		"role":        "poll",
		"generation":  generation,
		"duration_ms": time.Since(started).Milliseconds(),
	})
}

func (s *Service) fallbackProxyAppServerToStdioLocked(ctx context.Context, role string, cause error) bool {
	if s == nil || !strings.HasPrefix(strings.TrimSpace(s.appServerListen), "proxy") {
		return false
	}
	causeText := ""
	if cause != nil {
		causeText = cause.Error()
	}
	s.mu.Lock()
	oldLive := s.live
	oldPoll := s.poll
	s.appServerListen = appServerStdioListen
	s.liveConnected = false
	s.pollConnected = false
	s.live = nil
	s.poll = nil
	s.liveEvents = nil
	s.liveGeneration++
	liveGeneration := s.liveGeneration
	s.pollGeneration++
	pollGeneration := s.pollGeneration
	s.lastError = ""
	s.mu.Unlock()
	newLive := s.liveFactory()
	newPoll := s.pollFactory()
	s.mu.Lock()
	if s.live == nil {
		s.live = newLive
	} else if newLive != nil {
		_ = newLive.Close()
	}
	if s.poll == nil {
		s.poll = newPoll
	} else if newPoll != nil {
		_ = newPoll.Close()
	}
	s.mu.Unlock()
	s.logLifecycle("appserver_proxy_fallback_to_stdio", lifecycleFields{
		"role":            role,
		"cause":           sanitizeDiagnosticString(causeText),
		"live_generation": liveGeneration,
		"poll_generation": pollGeneration,
	})
	_ = s.store.SetState(ctx, "appserver.listen_effective", appServerStdioListen)
	_ = s.store.SetState(ctx, "appserver.proxy_fallback_at", time.Now().UTC().Format(time.RFC3339Nano))
	_ = s.store.SetState(ctx, "appserver.proxy_fallback_reason", sanitizeDiagnosticString(causeText))
	_ = s.store.SetState(ctx, "appserver.live_connected", "false")
	_ = s.store.SetState(ctx, "appserver.poll_connected", "false")
	if oldLive != nil {
		_ = oldLive.Close()
	}
	if oldPoll != nil && oldPoll != oldLive {
		_ = oldPoll.Close()
	}
	return true
}

func (s *Service) ensureSessionLifecycle(ctx context.Context) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.ensureLiveSessionLocked(ctx)
	s.ensurePollSessionLocked(ctx)
}

func (s *Service) liveEventLoop(ctx context.Context, live Session, ch <-chan appserver.Event, generation uint64) {
	if ch == nil || live == nil {
		return
	}
	s.logLifecycle("appserver_live_event_loop_started", lifecycleFields{
		"generation": generation,
	})
	defer func() {
		s.mu.Lock()
		currentGeneration := s.liveGeneration
		identityMatch := s.live == live
		eventsMatch := s.liveEvents == ch
		current := identityMatch && eventsMatch && currentGeneration == generation
		if current {
			s.liveConnected = false
			s.liveEvents = nil
		}
		s.mu.Unlock()
		if !current {
			s.logLifecycle("appserver_live_event_loop_stale", lifecycleFields{
				"generation":         generation,
				"current_generation": currentGeneration,
				"identity_match":     identityMatch,
				"events_match":       eventsMatch,
				"ctx_canceled":       ctx.Err() != nil,
				"stderr_tail":        sanitizedStderrTail(live),
			})
			return
		}
		_ = s.store.SetState(context.Background(), "appserver.live_connected", "false")
		_ = s.store.SetState(context.Background(), "appserver.live.last_closed_at", time.Now().UTC().Format(time.RFC3339Nano))
		s.logLifecycle("appserver_live_event_loop_closed", lifecycleFields{
			"generation":   generation,
			"ctx_canceled": ctx.Err() != nil,
			"stderr_tail":  sanitizedStderrTail(live),
		})
		if ctx.Err() == nil {
			_ = s.RequestRepair(context.Background(), "live_event_loop_closed")
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			if !s.liveEventLoopCurrent(live, ch, generation) {
				return
			}
			s.handleLiveEvent(ctx, live, event)
		}
	}
}

func (s *Service) liveEventLoopCurrent(live Session, ch <-chan appserver.Event, generation uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.live == live && s.liveEvents == ch && s.liveGeneration == generation
}

func (s *Service) handleLiveEvent(ctx context.Context, live Session, event appserver.Event) {
	if event.Channel == "transport_error" {
		err := fmt.Errorf("app-server transport error: %v", event.Params)
		_ = s.store.SetState(ctx, "appserver.live.last_error", sanitizeDiagnosticString(err.Error()))
		s.logLifecycle("appserver_transport_error", lifecycleFields{
			"params":      event.Params,
			"stderr_tail": sanitizedStderrTail(live),
		})
		s.noteSessionError(ctx, "transport_error", err)
		return
	}
	if event.Channel == "transport_closed" {
		err := fmt.Errorf("app-server transport closed: %v", event.Params)
		_ = s.store.SetState(ctx, "appserver.live.last_closed_at", time.Now().UTC().Format(time.RFC3339Nano))
		_ = s.store.SetState(ctx, "appserver.live.last_error", sanitizeDiagnosticString(err.Error()))
		s.logLifecycle("appserver_transport_closed", lifecycleFields{
			"params":      event.Params,
			"stderr_tail": sanitizedStderrTail(live),
		})
		if ctx.Err() == nil {
			s.noteSessionError(ctx, "transport_closed", err)
		}
		return
	}
	threadID := threadIDFromEvent(event)
	if threadID != "" {
		_ = s.store.MarkLiveEvent(ctx, threadID, model.NowString())
	}
	liveToolSnapshot, hasLiveToolSnapshot := appserver.ToolSnapshotFromLiveNotification(event, model.Thread{ID: threadID})
	if approval, ok := appserver.PendingApprovalFromServerRequest(event); ok {
		_ = s.store.SavePendingApproval(ctx, *approval)
		s.notifyPendingApproval(ctx, *approval)
		if approval.ThreadID != "" {
			if refreshed, err := s.refreshThread(ctx, live, approval.ThreadID); err == nil && refreshed != nil {
				_ = refreshed
			}
			s.syncThreadPanel(ctx, approval.ThreadID)
		}
		return
	}
	if strings.EqualFold(event.Method, "serverRequest/resolved") {
		if requestID := payloadMapString(event.Params, "requestId"); requestID != "" {
			_ = s.store.UpdatePendingApprovalStatus(ctx, requestID, "resolved")
			if pending, err := s.store.GetPendingApproval(ctx, requestID); err == nil && pending != nil && pending.ThreadID != "" {
				s.syncThreadPanel(ctx, pending.ThreadID)
			}
		}
		return
	}
	if threadID != "" {
		previous, _ := s.store.GetSnapshot(ctx, threadID)
		interactiveSync := s.threadNeedsLiveSync(ctx, threadID)
		if _, err := s.refreshThread(ctx, live, threadID); err != nil {
			s.noteSessionError(ctx, "live_refresh", err)
			return
		}
		if hasLiveToolSnapshot {
			_ = s.applyLiveToolSnapshot(ctx, threadID, liveToolSnapshot)
		}
		if !interactiveSync {
			_, current, err := s.loadThreadPanelSnapshot(ctx, threadID)
			if err != nil || current == nil || !snapshotHasPassiveChange(previous, current) {
				return
			}
		}
		s.syncThreadPanel(ctx, threadID)
	}
}

func (s *Service) applyLiveToolSnapshot(ctx context.Context, threadID string, liveTool appserver.ThreadReadSnapshot) bool {
	threadID = strings.TrimSpace(firstNonEmpty(threadID, liveTool.Thread.ID))
	if threadID == "" || strings.TrimSpace(liveTool.LatestToolFP) == "" {
		return false
	}
	thread, err := s.store.GetThread(ctx, threadID)
	if err != nil || thread == nil {
		return false
	}
	state, err := s.store.GetSnapshot(ctx, threadID)
	if err != nil {
		return false
	}
	var current appserver.ThreadReadSnapshot
	if state != nil && len(state.CompactJSON) > 0 {
		_ = json.Unmarshal(state.CompactJSON, &current)
	}
	if current.Thread.ID == "" {
		current.Thread = *thread
	} else {
		current.Thread = mergeThreadMetadata(current.Thread, *thread)
	}
	turnID := strings.TrimSpace(liveTool.LatestTurnID)
	if turnID == "" {
		turnID = strings.TrimSpace(current.LatestTurnID)
	}
	if turnID == "" {
		return false
	}
	if current.LatestTurnID != "" && current.LatestTurnID != turnID {
		if !isTerminalStatus(current.LatestTurnStatus) || !turnIDAfter(turnID, current.LatestTurnID) {
			return false
		}
	}
	if current.LatestTurnID == turnID && isTerminalStatus(current.LatestTurnStatus) && strings.TrimSpace(current.LatestFinalFP) != "" {
		return false
	}
	if current.LatestTurnID == turnID && liveToolIsOlderThanCurrentSameTurn(current, liveTool) {
		return false
	}
	if current.LatestTurnID == turnID &&
		sameToolSnapshot(current, liveTool) &&
		terminalToolStatus(current.LatestToolStatus) &&
		!terminalToolStatus(liveTool.LatestToolStatus) {
		return false
	}
	current.LatestTurnID = turnID
	current.LatestTurnStatus = firstNonEmpty(liveTool.LatestTurnStatus, current.LatestTurnStatus, "inProgress")
	current.Thread.ActiveTurnID = turnID
	current.Thread.Status = firstNonEmpty(liveTool.Thread.Status, current.Thread.Status, "inProgress")
	current.LatestToolID = liveTool.LatestToolID
	current.LatestToolKind = liveTool.LatestToolKind
	current.LatestToolLabel = liveTool.LatestToolLabel
	current.LatestToolStatus = liveTool.LatestToolStatus
	current.LatestToolOutput = liveTool.LatestToolOutput
	current.LatestToolFP = liveTool.LatestToolFP
	current.LatestToolLiveCurrent = liveTool.LatestToolLiveCurrent
	current.LatestProgressText = liveTool.LatestProgressText
	current.LatestProgressFP = liveTool.LatestProgressFP
	current.DetailItems = upsertLiveToolDetails(current.DetailItems, liveTool.DetailItems)

	_ = s.store.UpsertThread(ctx, current.Thread)
	next := appserver.CompactSnapshot(state, current, time.Now().UTC())
	if current.LatestTurnStatus == "inProgress" || current.WaitingOnApproval || current.WaitingOnReply {
		next.NextPollAfter = model.TimeString(time.Now().UTC().Add(s.cfg.ObserverPollInterval).Format(time.RFC3339Nano))
	}
	if err := s.store.UpsertSnapshot(ctx, threadID, next); err != nil {
		return false
	}
	s.logObserverSyncResult("live_tool", current)
	return true
}

func (s *Service) preserveChatOriginLiveCurrentTool(ctx context.Context, current *appserver.ThreadReadSnapshot, previous *model.ThreadSnapshotState) {
	if current == nil || previous == nil || len(previous.CompactJSON) == 0 {
		return
	}
	if isTerminalStatus(current.LatestTurnStatus) {
		return
	}
	var prev appserver.ThreadReadSnapshot
	if err := json.Unmarshal(previous.CompactJSON, &prev); err != nil {
		return
	}
	turnID := strings.TrimSpace(current.LatestTurnID)
	if turnID == "" || turnID != strings.TrimSpace(prev.LatestTurnID) {
		return
	}
	if !prev.LatestToolLiveCurrent || isTerminalStatus(prev.LatestTurnStatus) || terminalToolStatus(prev.LatestToolStatus) {
		return
	}
	threadID := strings.TrimSpace(firstNonEmpty(current.Thread.ID, prev.Thread.ID))
	if threadID == "" || !s.isDirectInputOriginTurn(ctx, threadID, turnID) {
		return
	}
	label := strings.TrimSpace(cleanNilLiteral(prev.LatestToolLabel))
	if label == "" || strings.TrimSpace(prev.LatestToolFP) == "" {
		return
	}
	if !shouldPreserveChatOriginLiveCurrentTool(*current, prev) {
		return
	}
	current.LatestToolID = prev.LatestToolID
	current.LatestToolKind = prev.LatestToolKind
	current.LatestToolLabel = prev.LatestToolLabel
	current.LatestToolStatus = prev.LatestToolStatus
	current.LatestToolOutput = prev.LatestToolOutput
	current.LatestToolFP = prev.LatestToolFP
	current.LatestToolLiveCurrent = prev.LatestToolLiveCurrent
	current.LatestProgressText = prev.LatestProgressText
	current.LatestProgressFP = prev.LatestProgressFP
	current.LatestToolStartedAt = prev.LatestToolStartedAt
	current.LatestToolUpdatedAt = prev.LatestToolUpdatedAt
	current.DetailItems = upsertLiveToolDetails(current.DetailItems, toolOutputDetailItems(prev.DetailItems))
}

func shouldPreserveChatOriginLiveCurrentTool(current, previous appserver.ThreadReadSnapshot) bool {
	if !snapshotHasToolEvidence(current) {
		return true
	}
	if sameToolSnapshot(current, previous) {
		return !terminalToolStatus(current.LatestToolStatus)
	}
	previousIndex := latestToolDetailIndex(previous.DetailItems, previous.LatestToolID, previous.LatestToolLabel)
	currentIndex := latestToolDetailIndex(previous.DetailItems, current.LatestToolID, current.LatestToolLabel)
	return previousIndex >= 0 && currentIndex >= 0 && currentIndex < previousIndex
}

func snapshotHasToolEvidence(snapshot appserver.ThreadReadSnapshot) bool {
	if strings.TrimSpace(cleanNilLiteral(snapshot.LatestToolID)) != "" ||
		strings.TrimSpace(cleanNilLiteral(snapshot.LatestToolLabel)) != "" ||
		strings.TrimSpace(cleanNilLiteral(snapshot.LatestToolOutput)) != "" ||
		strings.TrimSpace(snapshot.LatestToolFP) != "" {
		return true
	}
	for _, item := range snapshot.DetailItems {
		switch item.Kind {
		case model.DetailItemTool, model.DetailItemOutput:
			return true
		}
	}
	return false
}

func toolOutputDetailItems(items []model.DetailItem) []model.DetailItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]model.DetailItem, 0, len(items))
	for _, item := range items {
		switch item.Kind {
		case model.DetailItemTool, model.DetailItemOutput:
			out = append(out, item)
		}
	}
	return out
}

func turnIDAfter(candidate, current string) bool {
	candidate = strings.TrimSpace(candidate)
	current = strings.TrimSpace(current)
	if candidate == "" || current == "" || candidate == current {
		return false
	}
	if !codexThreadIDPattern.MatchString(candidate) || !codexThreadIDPattern.MatchString(current) {
		return false
	}
	return strings.Compare(candidate, current) > 0
}

func liveToolIsOlderThanCurrentSameTurn(current, liveTool appserver.ThreadReadSnapshot) bool {
	currentIndex := latestToolDetailIndex(current.DetailItems, current.LatestToolID, current.LatestToolLabel)
	liveIndex := latestToolDetailIndex(current.DetailItems, liveTool.LatestToolID, liveTool.LatestToolLabel)
	return currentIndex >= 0 && liveIndex >= 0 && liveIndex < currentIndex
}

func latestToolDetailIndex(items []model.DetailItem, toolID, label string) int {
	toolID = strings.TrimSpace(toolID)
	label = strings.TrimSpace(label)
	if toolID == "" && label == "" {
		return -1
	}
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.Kind != model.DetailItemTool {
			continue
		}
		if toolID != "" && strings.TrimSpace(item.ID) == toolID {
			return i
		}
		if toolID == "" && label != "" && strings.TrimSpace(item.Label) == label {
			return i
		}
	}
	return -1
}

func upsertLiveToolDetails(items []model.DetailItem, liveItems []model.DetailItem) []model.DetailItem {
	if len(liveItems) == 0 {
		return items
	}
	remove := map[string]struct{}{}
	for _, item := range liveItems {
		if id := strings.TrimSpace(item.ID); id != "" {
			remove[id] = struct{}{}
		}
	}
	out := make([]model.DetailItem, 0, len(items)+len(liveItems))
	for _, item := range items {
		if _, ok := remove[strings.TrimSpace(item.ID)]; ok {
			continue
		}
		out = append(out, item)
	}
	out = append(out, liveItems...)
	return out
}

func sameToolSnapshot(left, right appserver.ThreadReadSnapshot) bool {
	leftID := strings.TrimSpace(left.LatestToolID)
	rightID := strings.TrimSpace(right.LatestToolID)
	if leftID != "" && rightID != "" {
		return leftID == rightID
	}
	leftLabel := strings.TrimSpace(left.LatestToolLabel)
	rightLabel := strings.TrimSpace(right.LatestToolLabel)
	return leftLabel != "" && leftLabel == rightLabel
}

func terminalToolStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "succeeded", "failed", "interrupted", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func (s *Service) indexLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.IndexRefreshInterval)
	defer ticker.Stop()
	for {
		s.syncThreads(ctx, 200)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) attachLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.AttachRefreshInterval)
	defer ticker.Stop()
	for {
		s.attachTracked(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.ObserverPollInterval)
	defer ticker.Stop()
	for {
		s.refreshObserverIndex(ctx)
		s.pollTracked(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) refreshObserverIndex(ctx context.Context) {
}

func (s *Service) deliveryLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		s.processDeliveryBatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) controlLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		value, _ := s.store.GetState(ctx, "control.repair_request")
		if strings.TrimSpace(value) != "" {
			at, reason := parseRepairRequest(value)
			s.repairSessions(ctx, reason)
			s.logLifecycle("repair_completed", lifecycleFields{
				"reason":       reason,
				"requested_at": at,
			})
			_ = s.store.SetState(ctx, "control.repair_request", "")
		} else {
			s.reconcileSessions(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) reconcileSessions(ctx context.Context) {
	s.ensureSessionLifecycle(ctx)
}

func (s *Service) repairSessions(ctx context.Context, reason string) {
	s.sessionMu.Lock()
	s.mu.Lock()
	oldLive := s.live
	oldPoll := s.poll
	s.liveConnected = false
	s.pollConnected = false
	s.live = s.liveFactory()
	s.poll = s.pollFactory()
	s.liveEvents = nil
	s.liveGeneration++
	liveGeneration := s.liveGeneration
	s.pollGeneration++
	pollGeneration := s.pollGeneration
	s.lastError = ""
	s.mu.Unlock()
	s.logLifecycle("appserver_session_repair_start", lifecycleFields{
		"reason":          reason,
		"live_generation": liveGeneration,
		"poll_generation": pollGeneration,
	})
	if oldLive != nil {
		started := time.Now()
		err := oldLive.Close()
		s.logAppServerCall("Close", started, err, oldLive, lifecycleFields{"role": "live", "operation": "repair"})
	}
	if oldPoll != nil {
		started := time.Now()
		err := oldPoll.Close()
		s.logAppServerCall("Close", started, err, oldPoll, lifecycleFields{"role": "poll", "operation": "repair"})
	}
	rechecked, _ := s.store.MarkAllPendingApprovals(ctx, "needs_recheck")
	_ = s.store.SetState(ctx, "repair.last_rechecked", strconv.FormatInt(rechecked, 10))
	_ = s.store.SetState(ctx, "appserver.live_connected", "false")
	_ = s.store.SetState(ctx, "appserver.poll_connected", "false")
	_ = s.store.SetState(ctx, "appserver.live.generation", strconv.FormatUint(liveGeneration, 10))
	_ = s.store.SetState(ctx, "appserver.poll.generation", strconv.FormatUint(pollGeneration, 10))
	s.ensureLiveSessionLocked(ctx)
	s.ensurePollSessionLocked(ctx)
	s.sessionMu.Unlock()
	s.bootstrapTrackedState(ctx)
}

func (s *Service) bootstrapTrackedState(ctx context.Context) {
	s.syncThreads(ctx, 200)
	s.attachTracked(ctx)
	s.pollTracked(ctx)
}

func (s *Service) syncThreads(ctx context.Context, limit int) {
	s.mu.RLock()
	live := s.live
	poll := s.poll
	liveConnected := s.liveConnected
	pollConnected := s.pollConnected
	s.mu.RUnlock()
	var client Session
	if liveConnected {
		client = live
	} else if pollConnected {
		client = poll
	}
	if client == nil {
		return
	}
	if limit <= 0 {
		limit = 100
	}
	cursor := ""
	remaining := limit
	pageSize := 25
	for remaining > 0 {
		requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		result, err := client.ThreadList(requestCtx, min(pageSize, remaining), cursor)
		cancel()
		if err != nil {
			s.noteSessionError(ctx, "thread_list", err)
			return
		}
		threads := appserver.ThreadsFromList(result)
		if len(threads) == 0 {
			return
		}
		for _, thread := range threads {
			_ = s.store.UpsertThread(ctx, thread)
		}
		remaining -= len(threads)
		nextCursor, _ := result["nextCursor"].(string)
		if strings.TrimSpace(nextCursor) == "" {
			return
		}
		cursor = nextCursor
	}
}

func (s *Service) attachTracked(ctx context.Context) {
	s.mu.RLock()
	live := s.live
	connected := s.liveConnected
	s.mu.RUnlock()
	if !connected || live == nil {
		return
	}
	seen := map[string]struct{}{}
	for _, threadID := range s.currentPanelThreadIDs(ctx) {
		if _, ok := seen[threadID]; ok {
			continue
		}
		seen[threadID] = struct{}{}
		thread, err := s.store.GetThread(ctx, threadID)
		if err != nil || thread == nil {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = live.ThreadResume(requestCtx, thread.ID, thread.CWD)
		cancel()
		if err != nil {
			if isThreadArchivedError(err) {
				thread.Archived = true
				_ = s.store.UpsertThread(ctx, *thread)
				continue
			}
			s.setError(ctx, fmt.Errorf("thread_resume(bound): %w", err))
		}
	}
	for _, threadID := range s.feishuTopicThreadIDs(ctx, observerRecentThreadLimit) {
		if _, ok := seen[threadID]; ok {
			continue
		}
		seen[threadID] = struct{}{}
		thread, err := s.store.GetThread(ctx, threadID)
		if err != nil || thread == nil || thread.Archived {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = live.ThreadResume(requestCtx, thread.ID, thread.CWD)
		cancel()
		if err != nil {
			if isThreadArchivedError(err) {
				thread.Archived = true
				_ = s.store.UpsertThread(ctx, *thread)
				continue
			}
			s.setError(ctx, fmt.Errorf("thread_resume(feishu_topic): %w", err))
		}
	}
}

func (s *Service) pollTracked(ctx context.Context) {
	s.mu.RLock()
	poll := s.poll
	connected := s.pollConnected
	s.mu.RUnlock()
	if !connected || poll == nil {
		return
	}
	threads := s.trackedThreads(ctx, observerRecentThreadLimit)
	if len(threads) == 0 {
		return
	}
	for _, thread := range threads {
		snapshot, _ := s.store.GetSnapshot(ctx, thread.ID)
		catchup := s.threadNeedsCatchupPolling(ctx, thread, snapshot)
		if snapshot != nil && snapshot.LastRichLiveEventAt != "" {
			if time.Since(parseTime(snapshot.LastRichLiveEventAt)) < maxDuration(10*time.Second, s.cfg.ObserverPollInterval*2) {
				continue
			}
		}
		requestCtx, cancel := context.WithTimeout(ctx, maxDuration(10*time.Second, s.cfg.ObserverPollInterval*2))
		started := time.Now()
		payload, err := poll.ThreadRead(requestCtx, thread.ID, true)
		cancel()
		s.logAppServerCall("ThreadRead", started, err, poll, lifecycleFields{
			"operation":      "poll_tracked",
			"thread_id":      thread.ID,
			"include_turns":  true,
			"fallback_next":  err != nil,
			"poll_catchup":   catchup,
			"poll_snapshot":  snapshot != nil,
			"poll_connected": true,
		})
		if err != nil {
			requestCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
			started = time.Now()
			payload, err = poll.ThreadRead(requestCtx, thread.ID, false)
			cancel()
			s.logAppServerCall("ThreadRead", started, err, poll, lifecycleFields{
				"operation":      "poll_tracked",
				"thread_id":      thread.ID,
				"include_turns":  false,
				"poll_catchup":   catchup,
				"poll_snapshot":  snapshot != nil,
				"poll_connected": true,
			})
		}
		if err != nil {
			if isThreadNotLoadedError(err) {
				s.logThreadReadSkipped(thread.ID, "thread_not_loaded")
				continue
			}
			s.noteSessionError(ctx, "thread_read", err)
			continue
		}
		current := appserver.SnapshotFromThreadRead(payload)
		current.Thread.Raw, _ = json.Marshal(payload)
		current.Thread = mergeThreadMetadata(current.Thread, thread)
		latestSnapshot := snapshot
		if stored, err := s.store.GetSnapshot(ctx, thread.ID); err == nil && stored != nil {
			latestSnapshot = stored
		}
		gateHandled, gateDecision := s.applyChatOriginTerminalGate(ctx, "poll_tracked", &current, latestSnapshot)
		if gateHandled {
			if gateDecision.DeferredDisplayableProgress() {
				s.preserveChatOriginLiveCurrentTool(ctx, &current, latestSnapshot)
				_ = s.store.UpsertThread(ctx, current.Thread)
				nextSnapshot := s.compactDeferredProgressSnapshot(latestSnapshot, current, time.Now().UTC(), gateDecision)
				_ = s.store.UpsertSnapshot(ctx, current.Thread.ID, nextSnapshot)
				s.logObserverSyncResult("poll_tracked", current)
				if catchup || s.threadNeedsLiveSync(ctx, current.Thread.ID) || snapshotHasPassiveChange(latestSnapshot, &current) {
					s.syncThreadPanel(ctx, current.Thread.ID)
				}
			}
			continue
		}
		s.preserveChatOriginLiveCurrentTool(ctx, &current, latestSnapshot)
		_ = s.store.UpsertThread(ctx, current.Thread)
		nextSnapshot := appserver.CompactSnapshot(latestSnapshot, current, time.Now().UTC())
		if current.LatestTurnStatus == "inProgress" || current.WaitingOnApproval || current.WaitingOnReply {
			nextSnapshot.NextPollAfter = model.TimeString(time.Now().UTC().Add(s.cfg.ObserverPollInterval).Format(time.RFC3339Nano))
		} else {
			nextSnapshot.NextPollAfter = model.TimeString(time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano))
		}
		applyTerminalGateHotPolling(&nextSnapshot, gateDecision)
		_ = s.store.UpsertSnapshot(ctx, current.Thread.ID, nextSnapshot)
		s.logObserverSyncResult("poll_tracked", current)
		s.maybeLogChatOriginTerminal(ctx, current)
		if catchup || s.threadNeedsLiveSync(ctx, current.Thread.ID) || snapshotHasPassiveChange(latestSnapshot, &current) {
			s.syncThreadPanel(ctx, current.Thread.ID)
		}
	}
}

func (s *Service) compactDeferredProgressSnapshot(previous *model.ThreadSnapshotState, current appserver.ThreadReadSnapshot, polledAt time.Time, decision terminalGateDecision) model.ThreadSnapshotState {
	next := appserver.CompactSnapshot(previous, current, polledAt)
	if previous != nil {
		next.LastCompletionFP = previous.LastCompletionFP
	} else {
		next.LastCompletionFP = ""
	}
	if decision.DeferredDisplayableProgress() && strings.EqualFold(strings.TrimSpace(next.LastSeenTurnStatus), "interrupted") {
		next.LastSeenTurnStatus = "inProgress"
	}
	applyTerminalGateHotPolling(&next, decision)
	return next
}

func (s *Service) processDeliveryBatch(ctx context.Context) {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender == nil {
		return
	}
	items, err := s.store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(items) == 0 {
		return
	}
	for _, item := range items {
		var payload model.DeliveryPayload
		if err := json.Unmarshal([]byte(item.PayloadJSON), &payload); err != nil {
			_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "decode_error", err.Error())
			_ = s.store.FailDelivery(ctx, item.ID, item.RetryCount+1, time.Now().UTC().Add(s.cfg.DeliveryRetryBase), err.Error(), true)
			continue
		}
		s.logChatRenderContainsNil(payload.ThreadID, payload.TurnID, "delivery", 0, payload.Text)
		messageID, err := sender.SendMessage(ctx, item.ChatID, item.TopicID, payload.Text, payload.Buttons, silentSendOptions())
		if err != nil {
			attempt := item.RetryCount + 1
			_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "send_error", err.Error())
			dead := attempt >= s.cfg.DeliveryMaxAttempts
			backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
			_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), err.Error(), dead)
			s.setError(ctx, err)
			continue
		}
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "delivered", "")
		_ = s.store.CompleteDelivery(ctx, item.ID)
		if payload.ThreadID != "" {
			_ = s.store.PutMessageRoute(ctx, model.MessageRoute{
				ChatID:    item.ChatID,
				TopicID:   item.TopicID,
				MessageID: messageID,
				ThreadID:  payload.ThreadID,
				TurnID:    payload.TurnID,
				ItemID:    payload.ItemID,
				EventID:   payload.EventID,
				CreatedAt: model.NowString(),
			})
		}
	}
}

func (s *Service) handleCommandFromSource(ctx context.Context, chatID, topicID int64, raw string, replyToMessageID int64, sourceMode string) (*DirectResponse, error) {
	parts := strings.SplitN(strings.TrimSpace(raw), " ", 2)
	command := strings.ToLower(strings.SplitN(parts[0], "@", 2)[0])
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}
	switch command {
	case "/start":
		return s.workspaceOverview(ctx)
	case "/help":
		return &DirectResponse{Text: s.t(ctx, "命令：\n/help\n/chats [数量|搜索]\n/projects\n/new <提示词>\n/reply [--plan] <thread> <文本>\n/plan <文本>\n/plan <thread_id> <文本>\n/goal <目标>\n/goal clear\n/setting\n/status\n/repair\n/stop [thread]", "Commands:\n/help\n/chats [limit|search]\n/projects\n/new <prompt>\n/reply [--plan] <thread> <text>\n/plan <text>\n/plan <thread_id> <text>\n/goal <goal>\n/goal clear\n/setting\n/status\n/repair\n/stop [thread]")}, nil
	case "/status":
		response, err := s.StatusSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		return response, nil
	case "/context", "/whereami":
		text, err := s.contextCard(ctx, chatID, topicID)
		if err != nil {
			return nil, err
		}
		return &DirectResponse{Text: text}, nil
	case "/setting", "/settings":
		return s.codexSettingsOverview(ctx)
	case "/chats":
		return s.threadsOverview(ctx, rest)
	case "/projects":
		return s.projectsOverview(ctx)
	case "/new":
		return s.newChatCommandFromSource(ctx, chatID, topicID, rest, sourceMode)
	case "/show":
		decision, err := s.resolveRouteFromSource(ctx, chatID, topicID, rest, replyToMessageID, sourceMode)
		if err != nil {
			return nil, err
		}
		if decision.ThreadID == "" {
			return &DirectResponse{Text: s.t(ctx, "用法：/show <thread>，或回复一条会话消息。", "Usage: /show <thread> or reply to a thread message.")}, nil
		}
		return s.showThread(ctx, chatID, topicID, decision.ThreadID, true, sourceMode)
	case "/reply":
		decision, text, collaborationMode, ok, err := s.resolveInputCommand(ctx, chatID, topicID, rest, replyToMessageID, sourceMode, "", true, false)
		if err != nil {
			return nil, err
		}
		if !ok || decision.ThreadID == "" {
			return &DirectResponse{Text: s.t(ctx, "用法：/reply [--plan] <thread> <文本>", "Usage: /reply [--plan] <thread> <text>")}, nil
		}
		s.logInputInbound(sourceMode, "command_reply", chatID, topicID, replyToMessageID, decision, text, collaborationMode)
		return s.sendInputToThreadTurnFromSource(ctx, chatID, topicID, decision.ThreadID, decision.TurnID, text, collaborationMode, sourceMode)
	case "/default", "/default_mode":
		decision, text, _, ok, err := s.resolveInputCommand(ctx, chatID, topicID, rest, replyToMessageID, sourceMode, collaborationModeDefault, false, true)
		if err != nil {
			return nil, err
		}
		if !ok || decision.ThreadID == "" {
			return &DirectResponse{Text: s.t(ctx, "用法：/default <文本>、/default <thread_id> <文本>，或回复 /default <文本>。", "Usage: /default <text>, /default <thread_id> <text>, or reply with /default <text>.")}, nil
		}
		s.logInputInbound(sourceMode, "command_default", chatID, topicID, replyToMessageID, decision, text, collaborationModeDefault)
		return s.sendInputToThreadTurnFromSource(ctx, chatID, topicID, decision.ThreadID, decision.TurnID, text, collaborationModeDefault, sourceMode)
	case "/plan", "/plan_mode":
		decision, text, _, ok, err := s.resolveInputCommand(ctx, chatID, topicID, rest, replyToMessageID, sourceMode, collaborationModePlan, false, true)
		if err != nil {
			return nil, err
		}
		if !ok || decision.ThreadID == "" {
			return &DirectResponse{Text: s.t(ctx, "用法：/plan <文本>、/plan <thread_id> <文本>，或回复 /plan <文本>。", "Usage: /plan <text>, /plan <thread_id> <text>, or reply with /plan <text>.")}, nil
		}
		s.logInputInbound(sourceMode, "command_plan", chatID, topicID, replyToMessageID, decision, text, collaborationModePlan)
		return s.sendInputToThreadTurnFromSource(ctx, chatID, topicID, decision.ThreadID, decision.TurnID, text, collaborationModePlan, sourceMode)
	case "/goal":
		return s.goalCommand(ctx, chatID, topicID, rest, replyToMessageID, sourceMode)
	case "/repair":
		if err := s.RequestRepair(ctx, "chat"); err != nil {
			return nil, err
		}
		return &DirectResponse{Text: s.t(ctx, "已请求修复。App-server 会话会在后台重建。", "Repair requested. App-server sessions will be recreated in the background.")}, nil
	case "/stop":
		return s.stopThread(ctx, chatID, topicID, rest, replyToMessageID, sourceMode)
	case "/approve":
		if strings.TrimSpace(rest) == "" {
			return &DirectResponse{Text: s.t(ctx, "请使用批准按钮，或输入 /approve <request_id>。", "Use approval buttons or /approve <request_id>.")}, nil
		}
		return s.approve(ctx, rest, "accept")
	case "/deny":
		if strings.TrimSpace(rest) == "" {
			return &DirectResponse{Text: s.t(ctx, "请使用拒绝按钮，或输入 /deny <request_id>。", "Use deny button or /deny <request_id>.")}, nil
		}
		return s.approve(ctx, rest, "decline")
	default:
		return &DirectResponse{Text: s.t(ctx, "未知命令。请使用 /help。", "Unknown command. Use /help.")}, nil
	}
}

func (s *Service) handlePlainTextFromSource(ctx context.Context, chatID, topicID int64, text string, replyToMessageID int64, sourceMode string) (*DirectResponse, error) {
	if response, consumed, err := s.maybeConsumeNewThreadPromptFromSource(ctx, chatID, topicID, text, sourceMode); consumed {
		return response, err
	}
	decision, err := s.resolveRouteFromSource(ctx, chatID, topicID, "", replyToMessageID, sourceMode)
	if err != nil {
		return nil, err
	}
	if decision.ThreadID == "" {
		return s.workspaceRoutingHint(ctx), nil
	}
	s.logInputInbound(sourceMode, "plain_text", chatID, topicID, replyToMessageID, decision, text, "")
	if strings.TrimSpace(decision.RequestID) != "" {
		return s.respondUserInputRequest(ctx, decision.RequestID, text)
	}
	return s.sendInputToThreadTurnFromSource(ctx, chatID, topicID, decision.ThreadID, decision.TurnID, text, "", sourceMode)
}

func (s *Service) workspaceRoutingHint(ctx context.Context) *DirectResponse {
	lang := s.botLanguage(ctx)
	lines := []string{}
	workspaceLabel := "Workspace"
	recentLabel := "Recent chats"
	projectsLabel := "Projects"
	if lang == botLanguageEnglish {
		lines = []string{
			"No Codex thread is selected yet.",
			"Open the bot workspace, choose a recent chat or project, then reply in the topic to continue remote control.",
		}
	} else {
		lines = []string{
			"还没有选中 Codex 会话。",
			"先从机器人工作台选择最近会话或项目；进入会话话题后，直接回复即可继续远程控制。",
		}
		workspaceLabel = "显示工作台"
		recentLabel = "最近会话"
		projectsLabel = "项目"
	}
	buttons := [][]model.ButtonSpec{
		{
			s.callbackButton(ctx, workspaceLabel, "workspace_overview", "workspace", "", "", nil),
			s.callbackButton(ctx, recentLabel, "workspace_threads", "workspace", "", "", nil),
			s.callbackButton(ctx, projectsLabel, "workspace_projects", "workspace", "", "", nil),
		},
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons}
}

func (s *Service) workspaceOverview(ctx context.Context) (*DirectResponse, error) {
	threadCount, _ := s.store.CountThreads(ctx)
	backlog, _ := s.store.DeliveryQueueBacklog(ctx)
	modelValue, _ := s.store.GetState(ctx, codexModelStateKey)
	reasoningValue, _ := s.store.GetState(ctx, codexReasoningStateKey)
	lang := s.botLanguage(ctx)
	lines := s.workspaceLines(lang, threadCount, backlog, modelValue, reasoningValue)
	buttons := [][]model.ButtonSpec{
		{
			s.callbackButton(ctx, localized(lang, "最近会话", "Recent chats"), "workspace_threads", "workspace", "", "", nil),
			s.callbackButton(ctx, localized(lang, "项目", "Projects"), "workspace_projects", "workspace", "", "", nil),
		},
		{
			s.callbackButton(ctx, localized(lang, "状态", "Status"), "workspace_status", "workspace", "", "", nil),
			s.callbackButton(ctx, localized(lang, "设置", "Settings"), "settings_overview", "settings", "", "", nil),
		},
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons}, nil
}

func (s *Service) workspaceLines(lang string, threadCount, backlog int, modelValue, reasoningValue string) []string {
	if lang == botLanguageEnglish {
		return []string{
			"Codex Workspace",
			fmt.Sprintf("Cached threads: %d", threadCount),
			fmt.Sprintf("Delivery backlog: %d", backlog),
			fmt.Sprintf("Model: %s", settingValueLabel(modelValue, "Auto")),
			fmt.Sprintf("Reasoning effort: %s", settingValueLabel(reasoningValue, "Auto")),
			"",
			"Each Codex thread gets a topic reply in this bot chat. Open a topic and reply there to continue remote control.",
		}
	}
	return []string{
		"Codex 机器人工作台",
		fmt.Sprintf("缓存会话：%d", threadCount),
		fmt.Sprintf("待发送队列：%d", backlog),
		fmt.Sprintf("模型：%s", settingValueLabel(modelValue, "自动")),
		fmt.Sprintf("推理强度：%s", settingValueLabel(reasoningValue, "自动")),
		"",
		"每个 Codex thread 会在机器人单聊里对应一个会话话题；进入话题后直接回复即可继续远程控制。",
	}
}

func (s *Service) codexSettingsOverview(ctx context.Context) (*DirectResponse, error) {
	modelValue, _ := s.store.GetState(ctx, codexModelStateKey)
	reasoningValue, _ := s.store.GetState(ctx, codexReasoningStateKey)
	languageValue := s.botLanguage(ctx)
	lang := s.botLanguage(ctx)
	lines := []string{
		localized(lang, "Codex 设置", "Codex settings"),
		fmt.Sprintf("%s: %s", localized(lang, "模型", "Model"), settingValueLabel(modelValue, localized(lang, "自动", "Auto"))),
		fmt.Sprintf("%s: %s", localized(lang, "推理强度", "Reasoning effort"), settingValueLabel(reasoningValue, localized(lang, "自动", "Auto"))),
		fmt.Sprintf("%s: %s", localized(lang, "语言", "Language"), languageLabel(languageValue)),
		"",
		localized(lang, "用于从聊天适配器启动的 Codex 回合。", "Used for Codex turns started from chat adapters."),
	}
	buttons := [][]model.ButtonSpec{
		{
			s.callbackButton(ctx, localized(lang, "模型", "Model"), "settings_model_menu", "settings", "", "", nil),
			s.callbackButton(ctx, localized(lang, "推理", "Reasoning"), "settings_reasoning_menu", "settings", "", "", nil),
		},
		{
			s.callbackButton(ctx, localized(lang, "语言", "Language"), "settings_language_menu", "settings", "", "", nil),
		},
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons}, nil
}

func (s *Service) codexModelMenu(ctx context.Context) (*DirectResponse, error) {
	current, _ := s.store.GetState(ctx, codexModelStateKey)
	lang := s.botLanguage(ctx)
	models, err := s.codexModels(ctx)
	if err != nil {
		return &DirectResponse{Text: fmt.Sprintf(localized(lang, "无法加载 Codex 模型：%v", "Could not load Codex models: %v"), err)}, nil
	}
	lines := []string{
		localized(lang, "Codex 模型", "Codex model"),
		fmt.Sprintf("%s: %s", localized(lang, "当前", "Current"), settingValueLabel(current, localized(lang, "自动", "Auto"))),
	}
	buttons := [][]model.ButtonSpec{
		{s.callbackButton(ctx, selectedButtonLabel(localized(lang, "自动", "Auto"), current == ""), "settings_model_set", "settings", "", "", map[string]any{"value": ""})},
	}
	for _, option := range models {
		label := option.ID
		if label == "" {
			continue
		}
		buttons = append(buttons, []model.ButtonSpec{
			s.callbackButton(ctx, selectedButtonLabel(shortButtonLabel(label), option.ID == current), "settings_model_set", "settings", "", "", map[string]any{"value": option.ID}),
		})
	}
	buttons = append(buttons, []model.ButtonSpec{
		s.callbackButton(ctx, localized(lang, "推理", "Reasoning"), "settings_reasoning_menu", "settings", "", "", nil),
		s.callbackButton(ctx, localized(lang, "设置", "Settings"), "settings_overview", "settings", "", "", nil),
	})
	return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons}, nil
}

func (s *Service) botLanguageMenu(ctx context.Context) (*DirectResponse, error) {
	current := s.botLanguage(ctx)
	lang := current
	lines := []string{
		localized(lang, "机器人语言", "Bot language"),
		fmt.Sprintf("%s: %s", localized(lang, "当前", "Current"), languageLabel(current)),
	}
	buttons := [][]model.ButtonSpec{
		{
			s.callbackButton(ctx, selectedButtonLabel("中文", current == botLanguageChinese), "settings_language_set", "settings", "", "", map[string]any{"value": botLanguageChinese}),
			s.callbackButton(ctx, selectedButtonLabel("English", current == botLanguageEnglish), "settings_language_set", "settings", "", "", map[string]any{"value": botLanguageEnglish}),
		},
		{
			s.callbackButton(ctx, localized(lang, "设置", "Settings"), "settings_overview", "settings", "", "", nil),
		},
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons}, nil
}

func (s *Service) codexReasoningMenu(ctx context.Context) (*DirectResponse, error) {
	current, _ := s.store.GetState(ctx, codexReasoningStateKey)
	modelValue, _ := s.store.GetState(ctx, codexModelStateKey)
	lang := s.botLanguage(ctx)
	efforts := allReasoningEfforts()
	if models, err := s.codexModels(ctx); err == nil {
		if selected, ok := selectedModelOption(models, modelValue); ok && len(selected.SupportedReasoningEffort) > 0 {
			efforts = selected.SupportedReasoningEffort
		}
	}
	lines := []string{
		localized(lang, "Codex 推理强度", "Codex reasoning effort"),
		fmt.Sprintf("%s: %s", localized(lang, "当前", "Current"), settingValueLabel(current, localized(lang, "自动", "Auto"))),
		fmt.Sprintf("%s: %s", localized(lang, "模型", "Model"), settingValueLabel(modelValue, localized(lang, "自动", "Auto"))),
	}
	buttons := [][]model.ButtonSpec{
		{s.callbackButton(ctx, selectedButtonLabel(localized(lang, "自动", "Auto"), current == ""), "settings_reasoning_set", "settings", "", "", map[string]any{"value": ""})},
	}
	for index := 0; index < len(efforts); index += 2 {
		row := []model.ButtonSpec{}
		for _, effort := range efforts[index:min(index+2, len(efforts))] {
			row = append(row, s.callbackButton(ctx, selectedButtonLabel(effort, effort == current), "settings_reasoning_set", "settings", "", "", map[string]any{"value": effort}))
		}
		buttons = append(buttons, row)
	}
	buttons = append(buttons, []model.ButtonSpec{
		s.callbackButton(ctx, localized(lang, "模型", "Model"), "settings_model_menu", "settings", "", "", nil),
		s.callbackButton(ctx, localized(lang, "设置", "Settings"), "settings_overview", "settings", "", "", nil),
	})
	return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons}, nil
}

func (s *Service) setCodexModel(ctx context.Context, chatID, topicID, messageID int64, payload map[string]any) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	value := payloadMapString(payload, "value")
	if value != "" {
		models, err := s.codexModels(ctx)
		if err != nil {
			return &DirectResponse{Text: fmt.Sprintf(localized(lang, "无法校验 Codex 模型：%v", "Could not validate Codex model: %v"), err)}, nil
		}
		if _, ok := selectedModelOption(models, value); !ok {
			return &DirectResponse{CallbackText: localized(lang, "模型选项已过期。", "Model option is stale."), Text: localized(lang, "这个模型已不可用。请使用 /setting 刷新。", "This model is not available anymore. Use /setting to refresh.")}, nil
		}
	}
	if err := s.store.SetState(ctx, codexModelStateKey, value); err != nil {
		return nil, err
	}
	return s.editOrSendSettingsResponse(ctx, chatID, topicID, messageID, localized(lang, "模型已保存。", "Model saved."), func(ctx context.Context) (*DirectResponse, error) {
		return s.codexSettingsSaved(ctx, localized(lang, "模型已保存。", "Model saved."))
	})
}

func (s *Service) setCodexReasoningEffort(ctx context.Context, chatID, topicID, messageID int64, payload map[string]any) (*DirectResponse, error) {
	lang := s.botLanguage(ctx)
	value := normalizeReasoningEffort(payloadMapString(payload, "value"))
	if value != "" && !containsString(allReasoningEfforts(), value) {
		return &DirectResponse{CallbackText: localized(lang, "推理选项已过期。", "Reasoning option is stale."), Text: localized(lang, "不支持这个推理强度。请使用 /setting 刷新。", "This reasoning effort is not supported. Use /setting to refresh.")}, nil
	}
	if err := s.store.SetState(ctx, codexReasoningStateKey, value); err != nil {
		return nil, err
	}
	return s.editOrSendSettingsResponse(ctx, chatID, topicID, messageID, localized(lang, "推理强度已保存。", "Reasoning saved."), func(ctx context.Context) (*DirectResponse, error) {
		return s.codexSettingsSaved(ctx, localized(lang, "推理强度已保存。", "Reasoning effort saved."))
	})
}

func (s *Service) setBotLanguage(ctx context.Context, chatID, topicID, messageID int64, payload map[string]any) (*DirectResponse, error) {
	currentLang := s.botLanguage(ctx)
	value := normalizeBotLanguage(payloadMapString(payload, "value"))
	if value == "" {
		return &DirectResponse{CallbackText: localized(currentLang, "语言选项已过期。", "Language option is stale."), Text: localized(currentLang, "不支持这个语言。请使用 /setting 刷新。", "This language is not supported. Use /setting to refresh.")}, nil
	}
	if err := s.store.SetState(ctx, botLanguageStateKey, value); err != nil {
		return nil, err
	}
	if payloadMapString(payload, "return") == "status" {
		response, err := s.StatusSnapshot(ctx)
		if err != nil {
			return nil, err
		}
		response.CallbackText = localized(value, "语言已保存。", "Language saved.")
		return response, nil
	}
	return s.editOrSendSettingsResponse(ctx, chatID, topicID, messageID, localized(value, "语言已保存。", "Language saved."), func(ctx context.Context) (*DirectResponse, error) {
		return s.codexSettingsSaved(ctx, localized(value, "语言已保存。", "Language saved."))
	})
}

func (s *Service) codexSettingsSaved(ctx context.Context, status string) (*DirectResponse, error) {
	modelValue, _ := s.store.GetState(ctx, codexModelStateKey)
	reasoningValue, _ := s.store.GetState(ctx, codexReasoningStateKey)
	languageValue := s.botLanguage(ctx)
	lang := languageValue
	lines := []string{
		localized(lang, "Codex 设置", "Codex settings"),
		fmt.Sprintf("%s: %s", localized(lang, "模型", "Model"), settingValueLabel(modelValue, localized(lang, "自动", "Auto"))),
		fmt.Sprintf("%s: %s", localized(lang, "推理强度", "Reasoning effort"), settingValueLabel(reasoningValue, localized(lang, "自动", "Auto"))),
		fmt.Sprintf("%s: %s", localized(lang, "语言", "Language"), languageLabel(languageValue)),
	}
	if strings.TrimSpace(status) != "" {
		lines = append(lines, "", status)
	}
	lines = append(lines, localized(lang, "使用 /setting 可以再次修改。", "Use /setting to change this again."))
	return &DirectResponse{Text: strings.Join(lines, "\n")}, nil
}

func (s *Service) editOrSendSettingsResponse(ctx context.Context, chatID, topicID, messageID int64, callbackText string, renderer func(context.Context) (*DirectResponse, error)) (*DirectResponse, error) {
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

func (s *Service) codexModels(ctx context.Context) ([]appserver.ModelOption, error) {
	client := s.settingsClient()
	if client == nil {
		return nil, errors.New("app-server session is not ready yet")
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	return client.ModelList(requestCtx, false)
}

func (s *Service) settingsClient() Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.liveConnected && s.live != nil {
		return s.live
	}
	if s.pollConnected && s.poll != nil {
		return s.poll
	}
	return nil
}

func selectedModelOption(models []appserver.ModelOption, value string) (appserver.ModelOption, bool) {
	value = strings.TrimSpace(value)
	var first appserver.ModelOption
	for index, option := range models {
		if index == 0 {
			first = option
		}
		if value != "" && option.ID == value {
			return option, true
		}
		if value == "" && option.IsDefault {
			return option, true
		}
	}
	if value == "" && first.ID != "" {
		return first, true
	}
	return appserver.ModelOption{}, false
}

func allReasoningEfforts() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh"}
}

func normalizeReasoningEffort(value string) string {
	normalized := strings.TrimSpace(strings.ToLower(value))
	switch normalized {
	case "", "<nil>":
		return ""
	case "x-high", "x_high", "extra-high", "extra_high":
		return "xhigh"
	default:
		return normalized
	}
}

func normalizeBotLanguage(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case botLanguageEnglish, "english", "en-us", "en_us":
		return botLanguageEnglish
	case "", botLanguageChinese, "chinese", "zh-cn", "zh_cn", "cn":
		return botLanguageChinese
	default:
		return ""
	}
}

func (s *Service) botLanguage(ctx context.Context) string {
	value, _ := s.store.GetState(ctx, botLanguageStateKey)
	normalized := normalizeBotLanguage(value)
	if normalized == "" {
		return botLanguageChinese
	}
	return normalized
}

func languageLabel(value string) string {
	switch normalizeBotLanguage(value) {
	case botLanguageEnglish:
		return "English"
	default:
		return "中文"
	}
}

func localized(lang, zh, en string) string {
	if normalizeBotLanguage(lang) == botLanguageEnglish {
		return en
	}
	return zh
}

func (s *Service) t(ctx context.Context, zh, en string) string {
	return localized(s.botLanguage(ctx), zh, en)
}

func readyLabelLang(lang string, ready bool) string {
	if ready {
		return localized(lang, "就绪", "Ready")
	}
	return localized(lang, "未就绪", "Not ready")
}

func onlineLabelLang(lang string, connected bool) string {
	if connected {
		return localized(lang, "在线", "Online")
	}
	return localized(lang, "离线", "Offline")
}

func onOffLabelLang(lang string, enabled bool) string {
	if enabled {
		return localized(lang, "开启", "On")
	}
	return localized(lang, "关闭", "Off")
}

func readableStatusLang(lang, turnStatus, threadStatus string) string {
	status := readableStatus(turnStatus, threadStatus)
	if normalizeBotLanguage(lang) == botLanguageEnglish {
		return status
	}
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "idle":
		return "空闲"
	case "inprogress", "active", "running":
		return "运行中"
	case "toolrunning":
		return "工具调用中"
	case "completed":
		return "已完成"
	case "failed":
		return "失败"
	case "interrupted":
		return "已中断"
	case "waiting for input", "active[waitingonuserinput]":
		return "等待输入"
	case "pending":
		return "等待中"
	default:
		return status
	}
}

func readableProgressStatusLang(lang, status string) string {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "inprogress":
		return localized(lang, "运行中", "Processing")
	case "toolrunning":
		return localized(lang, "工具调用中", "Using tools")
	case "completed":
		return localized(lang, "已完成", "Completed")
	case "failed":
		return localized(lang, "失败", "Failed")
	case "interrupted":
		return localized(lang, "已中断", "Interrupted")
	default:
		return readableStatusLang(lang, status, "")
	}
}

func settingValueLabel(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func selectedButtonLabel(label string, selected bool) string {
	if selected {
		return shortButtonLabel("* " + label)
	}
	return shortButtonLabel(label)
}

func shortButtonLabel(label string) string {
	label = strings.TrimSpace(label)
	const limit = 60
	if len(label) <= limit {
		return label
	}
	return strings.TrimSpace(label[:limit-3]) + "..."
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func (s *Service) resolveInputCommand(ctx context.Context, chatID, topicID int64, rest string, replyToMessageID int64, sourceMode string, defaultCollaborationMode string, allowModeFlag bool, preferImplicitRouteForUnknownHead bool) (model.RouteDecision, string, string, bool, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return model.RouteDecision{}, "", "", false, nil
	}
	collaborationMode := defaultCollaborationMode
	if allowModeFlag {
		if next, mode, ok := consumeCollaborationModeFlag(rest); ok {
			rest = next
			collaborationMode = mode
		}
	}
	first, remainder := splitCommandHead(rest)
	if first == "" {
		return model.RouteDecision{}, "", "", false, nil
	}
	if allowModeFlag && remainder != "" {
		if next, mode, ok := consumeCollaborationModeFlag(remainder); ok {
			remainder = next
			collaborationMode = mode
		}
	}
	if remainder != "" {
		if s.shouldTreatInputHeadAsExplicitThread(ctx, first, replyToMessageID, preferImplicitRouteForUnknownHead) {
			decision, err := s.resolveRouteFromSource(ctx, chatID, topicID, first, replyToMessageID, sourceMode)
			return decision, strings.TrimSpace(remainder), collaborationMode, strings.TrimSpace(remainder) != "", err
		}
		decision, err := s.resolveRouteFromSource(ctx, chatID, topicID, "", replyToMessageID, sourceMode)
		if err != nil {
			return model.RouteDecision{}, "", "", false, err
		}
		if decision.ThreadID != "" {
			return decision, rest, collaborationMode, true, nil
		}
		if preferImplicitRouteForUnknownHead {
			return decision, "", collaborationMode, false, nil
		}
		decision, err = s.resolveRouteFromSource(ctx, chatID, topicID, first, replyToMessageID, sourceMode)
		return decision, strings.TrimSpace(remainder), collaborationMode, strings.TrimSpace(remainder) != "", err
	}
	decision, err := s.resolveRouteFromSource(ctx, chatID, topicID, "", replyToMessageID, sourceMode)
	if err != nil {
		return model.RouteDecision{}, "", "", false, err
	}
	return decision, first, collaborationMode, decision.ThreadID != "", nil
}

func (s *Service) shouldTreatInputHeadAsExplicitThread(ctx context.Context, head string, replyToMessageID int64, preferImplicitRouteForUnknownHead bool) bool {
	head = strings.TrimSpace(head)
	if head == "" {
		return false
	}
	if thread, _ := s.store.GetThread(ctx, head); thread != nil {
		return true
	}
	if codexThreadIDPattern.MatchString(head) {
		return true
	}
	return replyToMessageID == 0 && !preferImplicitRouteForUnknownHead
}

func consumeCollaborationModeFlag(rest string) (string, string, bool) {
	head, tail := splitCommandHead(rest)
	switch strings.ToLower(strings.TrimSpace(head)) {
	case "--plan", "-p":
		return strings.TrimSpace(tail), collaborationModePlan, true
	case "--default", "--code", "-d":
		return strings.TrimSpace(tail), collaborationModeDefault, true
	default:
		return rest, "", false
	}
}

func splitCommandHead(rest string) (string, string) {
	parts := strings.SplitN(strings.TrimSpace(rest), " ", 2)
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.TrimSpace(parts[1])
}

func (s *Service) sendInputToThread(ctx context.Context, chatID, topicID int64, threadID, text string) (*DirectResponse, error) {
	return s.sendInputToThreadTurnFromSource(ctx, chatID, topicID, threadID, "", text, "", model.PanelSourceFeishuInput)
}

func (s *Service) goalCommand(ctx context.Context, chatID, topicID int64, rest string, replyToMessageID int64, sourceMode string) (*DirectResponse, error) {
	decision, text, ok, err := s.resolveGoalCommand(ctx, chatID, topicID, rest, replyToMessageID, sourceMode)
	if err != nil {
		return nil, err
	}
	if !ok || decision.ThreadID == "" {
		return &DirectResponse{Text: s.t(ctx, "用法：/goal <目标>、/goal <thread_id> <目标>，或 /goal clear。", "Usage: /goal <goal>, /goal <thread_id> <goal>, or /goal clear.")}, nil
	}
	s.mu.RLock()
	live := s.live
	connected := s.liveConnected
	s.mu.RUnlock()
	if !connected || live == nil {
		return &DirectResponse{Text: s.t(ctx, "Live app-server 会话尚未就绪。请试试 /status 或 /repair。", "Live app-server session is not ready yet. Try /status or /repair.")}, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	started := time.Now()
	if strings.EqualFold(strings.TrimSpace(text), "clear") {
		_, err = live.ThreadGoalClear(requestCtx, decision.ThreadID)
		s.logAppServerCall("ThreadGoalClear", started, err, live, lifecycleFields{"thread_id": decision.ThreadID})
		if err != nil {
			return nil, err
		}
		return &DirectResponse{Text: s.t(ctx, "已清空目标。", "Goal cleared."), ThreadID: decision.ThreadID}, nil
	}
	_, err = live.ThreadGoalSet(requestCtx, decision.ThreadID, text)
	s.logAppServerCall("ThreadGoalSet", started, err, live, lifecycleFields{"thread_id": decision.ThreadID})
	if err != nil {
		return nil, err
	}
	return &DirectResponse{Text: s.t(ctx, "已设置目标。", "Goal set."), ThreadID: decision.ThreadID}, nil
}

func (s *Service) resolveGoalCommand(ctx context.Context, chatID, topicID int64, rest string, replyToMessageID int64, sourceMode string) (model.RouteDecision, string, bool, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return model.RouteDecision{}, "", false, nil
	}
	first, remainder := splitCommandHead(rest)
	if remainder != "" && s.shouldTreatInputHeadAsExplicitThread(ctx, first, replyToMessageID, true) {
		decision, err := s.resolveRouteFromSource(ctx, chatID, topicID, first, replyToMessageID, sourceMode)
		return decision, strings.TrimSpace(remainder), strings.TrimSpace(remainder) != "", err
	}
	decision, err := s.resolveRouteFromSource(ctx, chatID, topicID, "", replyToMessageID, sourceMode)
	if err != nil {
		return model.RouteDecision{}, "", false, err
	}
	if decision.ThreadID == "" {
		return decision, "", false, nil
	}
	return decision, rest, true, nil
}

func (s *Service) desktopInputHandledResponse(ctx context.Context, chatID, topicID int64, live Session, liveConnected bool, thread *model.Thread, routeTurnID, sourceMode string, desktopResult map[string]any, startedNewTurn bool, desktopCollaborationMode string) *DirectResponse {
	if thread == nil {
		return nil
	}
	threadID := thread.ID
	turn := appserverThreadTurnID(desktopResult)
	if strings.TrimSpace(turn) == "" && strings.TrimSpace(routeTurnID) != "" {
		turn = routeTurnID
	}
	if startedNewTurn {
		switch desktopCollaborationMode {
		case collaborationModePlan:
			_ = s.setThreadCollaborationMarker(ctx, threadID, "", collaborationModePlan)
		case collaborationModeDefault:
			_ = s.clearThreadCollaborationMarker(ctx, threadID)
		}
	}
	if strings.TrimSpace(turn) != "" {
		_ = s.markInputOriginTurn(ctx, threadID, turn, sourceMode, chatID, topicID)
	}
	if liveConnected && live != nil {
		if _, refreshErr := s.refreshThreadForOperation(ctx, live, threadID, "refresh_thread_after_desktop_input"); refreshErr != nil {
			s.logLifecycle("thread_refresh_failed", lifecycleFields{
				"operation": "refresh_thread_after_desktop_input",
				"thread_id": threadID,
				"turn_id":   turn,
				"error":     refreshErr,
			})
		}
	}
	if strings.TrimSpace(turn) != "" {
		s.ensureStartedTurnSnapshot(ctx, thread, turn)
	}
	explicitTarget := model.ObserverTarget{
		ChatKey: model.ChatKey(chatID, topicID),
		ChatID:  chatID,
		TopicID: topicID,
		Enabled: true,
	}
	s.syncThreadPanelToTarget(ctx, explicitTarget, threadID, true, sourceMode)
	s.logLifecycle("codex_desktop_input_dispatched", lifecycleFields{
		"chat_key":    model.ChatKey(chatID, topicID),
		"source_mode": sourceMode,
		"thread_id":   threadID,
		"turn_id":     turn,
	})
	if strings.TrimSpace(turn) != "" {
		s.startChatOriginHotPoll(ctx, threadID, turn)
	}
	return &DirectResponse{ThreadID: threadID, TurnID: turn}
}

func (s *Service) sendInputToThreadTurnFromSource(ctx context.Context, chatID, topicID int64, threadID, routeTurnID, text, collaborationMode, sourceMode string) (*DirectResponse, error) {
	sourceMode = normalizeInputSourceMode(sourceMode)
	s.logLifecycle("chat_turn_input_start", lifecycleFields{
		"chat_key":           model.ChatKey(chatID, topicID),
		"source_mode":        sourceMode,
		"thread_id":          threadID,
		"route_turn_id":      routeTurnID,
		"text_len":           len([]rune(text)),
		"text_sha256":        shortTextHash(text),
		"collaboration_mode": collaborationMode,
	})
	thread, _ := s.store.GetThread(ctx, threadID)
	if thread == nil {
		return &DirectResponse{Text: fmt.Sprintf(s.t(ctx, "未知线程：%s", "Unknown thread: %s"), threadID)}, nil
	}
	s.mu.RLock()
	live := s.live
	connected := s.liveConnected
	s.mu.RUnlock()
	if normalizeInputSourceMode(sourceMode) == model.PanelSourceFeishuInput {
		desktopResult, handled, startedNewTurn, desktopCollaborationMode, desktopErr := s.maybeSendFeishuInputViaDesktop(ctx, chatID, topicID, thread, routeTurnID, text, collaborationMode)
		if desktopErr != nil {
			s.logLifecycle("codex_desktop_input_failed", lifecycleFields{
				"thread_id": threadID,
				"error":     sanitizeDiagnosticString(desktopErr.Error()),
			})
			if threadLooksActiveForInput(thread) || steerFailureImpliesActive(desktopErr) {
				return &DirectResponse{Text: activeThreadReplyText(thread, desktopErr), ThreadID: threadID, TurnID: thread.ActiveTurnID}, nil
			}
			return nil, desktopErr
		}
		if handled {
			return s.desktopInputHandledResponse(ctx, chatID, topicID, live, connected, thread, routeTurnID, sourceMode, desktopResult, startedNewTurn, desktopCollaborationMode), nil
		}
	}
	if !connected || live == nil {
		s.logLifecycle("chat_turn_input_rejected", lifecycleFields{
			"thread_id": threadID,
			"reason":    "live_session_not_ready",
		})
		return &DirectResponse{Text: s.t(ctx, "实时 app-server 会话尚未就绪。请尝试 /status 或 /repair。", "Live app-server session is not ready yet. Try /status or /repair.")}, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	started := time.Now()
	_, err := live.ThreadResume(requestCtx, threadID, thread.CWD)
	s.logAppServerCall("ThreadResume", started, err, live, lifecycleFields{
		"thread_id": threadID,
	})
	if err != nil {
		return nil, err
	}
	if refreshed, refreshErr := s.refreshThreadForOperation(ctx, live, threadID, "refresh_thread_before_start"); refreshErr == nil && refreshed != nil {
		thread = refreshed
	} else if refreshErr != nil {
		s.logLifecycle("thread_refresh_failed", lifecycleFields{
			"operation": "refresh_thread_before_start",
			"thread_id": threadID,
			"error":     refreshErr,
		})
	}
	var result map[string]any
	var steerErr error
	steerState, _ := s.resolveArmedSteer(ctx, chatID, topicID)
	if steerState != nil && steerState.ThreadID == threadID && strings.TrimSpace(steerState.TurnID) != "" {
		started = time.Now()
		result, steerErr = live.TurnSteer(requestCtx, threadID, steerState.TurnID, text)
		s.logAppServerCall("TurnSteer", started, steerErr, live, lifecycleFields{
			"thread_id": threadID,
			"turn_id":   steerState.TurnID,
			"route":     "armed",
		})
		if steerErr == nil {
			_ = s.store.ClearSteerState(ctx, chatID, topicID)
		}
	}
	if result == nil && strings.TrimSpace(routeTurnID) != "" {
		started = time.Now()
		result, steerErr = live.TurnSteer(requestCtx, threadID, routeTurnID, text)
		s.logAppServerCall("TurnSteer", started, steerErr, live, lifecycleFields{
			"thread_id": threadID,
			"turn_id":   routeTurnID,
			"route":     "reply",
		})
	}
	if result == nil && strings.TrimSpace(routeTurnID) == "" && threadLooksActiveForInput(thread) && strings.TrimSpace(thread.ActiveTurnID) != "" {
		started = time.Now()
		result, steerErr = live.TurnSteer(requestCtx, threadID, thread.ActiveTurnID, text)
		s.logAppServerCall("TurnSteer", started, steerErr, live, lifecycleFields{
			"thread_id": threadID,
			"turn_id":   thread.ActiveTurnID,
			"route":     "active_turn",
		})
	}
	if result == nil {
		if foundTurnID := activeTurnIDFromSteerMismatch(steerErr); foundTurnID != "" {
			thread.ActiveTurnID = foundTurnID
			thread.Status = "active"
			started = time.Now()
			result, steerErr = live.TurnSteer(requestCtx, threadID, foundTurnID, text)
			s.logAppServerCall("TurnSteer", started, steerErr, live, lifecycleFields{
				"thread_id": threadID,
				"turn_id":   foundTurnID,
				"route":     "active_turn_mismatch_retry",
			})
		}
	}
	if result == nil && steerFailureMeansNoActiveTurn(steerErr) {
		if refreshed, refreshErr := s.refreshThreadForOperation(ctx, live, threadID, "refresh_thread_after_no_active_steer"); refreshErr == nil && refreshed != nil {
			thread = refreshed
		} else if refreshErr != nil {
			s.logLifecycle("thread_refresh_failed", lifecycleFields{
				"operation": "refresh_thread_after_no_active_steer",
				"thread_id": threadID,
				"turn_id":   thread.ActiveTurnID,
				"error":     refreshErr,
			})
		}
	}
	if result == nil && !steerFailureMeansNoActiveTurn(steerErr) && (threadLooksActiveForInput(thread) || steerFailureImpliesActive(steerErr)) {
		s.logLifecycle("chat_turn_input_rejected", lifecycleFields{
			"thread_id": threadID,
			"turn_id":   thread.ActiveTurnID,
			"reason":    "thread_still_active",
			"steer_err": steerErr,
		})
		return &DirectResponse{Text: activeThreadReplyText(thread, steerErr), ThreadID: threadID, TurnID: thread.ActiveTurnID}, nil
	}
	startedNewTurn := false
	effectiveCollaborationMode := strings.TrimSpace(collaborationMode)
	if result == nil {
		usedDefaultOverride := false
		if effectiveCollaborationMode == "" && s.threadCollaborationOverride(ctx, threadID) == collaborationModeDefault {
			effectiveCollaborationMode = collaborationModeDefault
			usedDefaultOverride = true
		}
		options := s.turnStartOptions(ctx, effectiveCollaborationMode, thread)
		started = time.Now()
		result, err = live.TurnStart(requestCtx, threadID, text, thread.CWD, options)
		s.logAppServerCall("TurnStart", started, err, live, lifecycleFields{
			"thread_id":           threadID,
			"returned_turn_id":    appserverThreadTurnID(result),
			"model":               options.Model,
			"reasoning_effort":    options.ReasoningEffort,
			"collaboration_mode":  options.CollaborationMode,
			"used_thread_model":   options.Model != "" && options.Model == strings.TrimSpace(thread.PreferredModel),
			"request_message_len": len([]rune(text)),
		})
		if err != nil {
			return nil, err
		}
		if usedDefaultOverride || effectiveCollaborationMode != "" {
			_ = s.clearThreadCollaborationOverride(ctx, threadID)
		}
		_ = s.store.ClearSteerState(ctx, chatID, topicID)
		startedNewTurn = true
	}
	turn := appserverThreadTurnID(result)
	if strings.TrimSpace(turn) == "" && strings.TrimSpace(routeTurnID) != "" && err == nil {
		turn = routeTurnID
	}
	if startedNewTurn {
		switch effectiveCollaborationMode {
		case collaborationModePlan:
			_ = s.setThreadCollaborationMarker(ctx, threadID, "", collaborationModePlan)
		case collaborationModeDefault:
			_ = s.clearThreadCollaborationMarker(ctx, threadID)
		}
	}
	if strings.TrimSpace(turn) != "" {
		_ = s.markInputOriginTurn(ctx, threadID, turn, sourceMode, chatID, topicID)
	}
	if _, refreshErr := s.refreshThreadForOperation(ctx, live, threadID, "refresh_thread_after_start"); refreshErr != nil {
		s.logLifecycle("thread_refresh_failed", lifecycleFields{
			"operation": "refresh_thread_after_start",
			"thread_id": threadID,
			"turn_id":   turn,
			"error":     refreshErr,
		})
	}
	if strings.TrimSpace(turn) != "" {
		s.ensureStartedTurnSnapshot(ctx, thread, turn)
	}
	explicitTarget := model.ObserverTarget{
		ChatKey: model.ChatKey(chatID, topicID),
		ChatID:  chatID,
		TopicID: topicID,
		Enabled: true,
	}
	s.syncThreadPanelToTarget(ctx, explicitTarget, threadID, true, sourceMode)
	s.logLifecycle("chat_turn_input_dispatched", lifecycleFields{
		"chat_key":    model.ChatKey(chatID, topicID),
		"source_mode": sourceMode,
		"thread_id":   threadID,
		"turn_id":     turn,
	})
	if strings.TrimSpace(turn) != "" {
		s.startChatOriginHotPoll(ctx, threadID, turn)
	}
	return &DirectResponse{ThreadID: threadID, TurnID: turn}, nil
}

func (s *Service) maybeOpenCodexDesktopForInput(ctx context.Context, threadID, sourceMode string) bool {
	if s == nil || !s.cfg.OpenCodexDesktopOnFeishu || normalizeInputSourceMode(sourceMode) != model.PanelSourceFeishuInput {
		return false
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return false
	}
	s.mu.RLock()
	opener := s.desktopOpener
	s.mu.RUnlock()
	if opener == nil {
		opener = openCodexDesktopThread
	}
	if err := opener(ctx, threadID); err != nil {
		s.logLifecycle("codex_desktop_open_failed", lifecycleFields{
			"thread_id": threadID,
			"error":     sanitizeDiagnosticString(err.Error()),
		})
		return false
	}
	s.logLifecycle("codex_desktop_opened", lifecycleFields{
		"thread_id": threadID,
	})
	return true
}

func (s *Service) turnStartOptions(ctx context.Context, collaborationMode string, thread *model.Thread) appserver.TurnStartOptions {
	modelValue, _ := s.store.GetState(ctx, codexModelStateKey)
	reasoningValue, _ := s.store.GetState(ctx, codexReasoningStateKey)
	options := appserver.TurnStartOptions{
		CollaborationMode: strings.TrimSpace(collaborationMode),
		Model:             strings.TrimSpace(modelValue),
		ReasoningEffort:   normalizeReasoningEffort(reasoningValue),
	}
	if options.Model == "" && thread != nil {
		options.Model = strings.TrimSpace(thread.PreferredModel)
	}
	return options
}

func (s *Service) ensureStartedTurnSnapshot(ctx context.Context, thread *model.Thread, turnID string) {
	turnID = strings.TrimSpace(turnID)
	if thread == nil || turnID == "" {
		return
	}
	previous, err := s.store.GetSnapshot(ctx, thread.ID)
	if err == nil && previous != nil && strings.TrimSpace(previous.LastSeenTurnID) == turnID {
		return
	}
	startedThread := *thread
	startedThread.Status = "inProgress"
	startedThread.ActiveTurnID = turnID
	if startedThread.UpdatedAt == 0 {
		startedThread.UpdatedAt = time.Now().UTC().Unix()
	}
	current := appserver.ThreadReadSnapshot{
		Thread:           startedThread,
		LatestTurnID:     turnID,
		LatestTurnStatus: "inProgress",
	}
	nextSnapshot := appserver.CompactSnapshot(previous, current, time.Now().UTC())
	nextSnapshot.NextPollAfter = model.TimeString(time.Now().UTC().Add(s.cfg.ObserverPollInterval).Format(time.RFC3339Nano))
	_ = s.store.UpsertThread(ctx, startedThread)
	_ = s.store.UpsertSnapshot(ctx, startedThread.ID, nextSnapshot)
	s.logLifecycle("chat_started_turn_snapshot_seeded", lifecycleFields{
		"thread_id": startedThread.ID,
		"turn_id":   turnID,
	})
}

func (s *Service) startChatOriginHotPoll(ctx context.Context, threadID, turnID string) {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if threadID == "" || turnID == "" {
		return
	}
	s.mu.RLock()
	started := s.started
	s.mu.RUnlock()
	if !started {
		return
	}
	s.spawn(ctx, func(ctx context.Context) {
		s.chatOriginHotPollLoop(ctx, threadID, turnID)
	})
}

func (s *Service) chatOriginHotPollLoop(ctx context.Context, threadID, turnID string) {
	timer := time.NewTimer(chatOriginHotPollMax)
	defer timer.Stop()
	ticker := time.NewTicker(chatOriginHotPollTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case <-ticker.C:
			if !s.chatOriginHotPollOnce(ctx, threadID, turnID) {
				return
			}
		}
	}
}

func (s *Service) chatOriginHotPollOnce(ctx context.Context, threadID, turnID string) bool {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if threadID == "" || turnID == "" {
		return false
	}
	s.mu.RLock()
	poll := s.poll
	connected := s.pollConnected
	s.mu.RUnlock()
	if !connected || poll == nil {
		return true
	}
	if _, err := s.refreshThreadForOperation(ctx, poll, threadID, "chat_hot_poll"); err != nil {
		s.logLifecycle("chat_hot_poll_failed", lifecycleFields{
			"thread_id": threadID,
			"turn_id":   turnID,
			"error":     err,
		})
		return true
	}
	s.syncThreadPanel(ctx, threadID)
	snapshot, err := s.store.GetSnapshot(ctx, threadID)
	if err != nil || snapshot == nil {
		return true
	}
	if strings.TrimSpace(snapshot.LastSeenTurnID) != turnID {
		return false
	}
	if isTerminalStatus(snapshot.LastSeenTurnStatus) {
		if s.threadHasDeferredTerminal(ctx, threadID, turnID) {
			return true
		}
		return false
	}
	return true
}

func (s *Service) threadHasDeferredTerminal(ctx context.Context, threadID, turnID string) bool {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if threadID == "" || turnID == "" {
		return false
	}
	state, ok, err := s.loadChatOriginEmptyInterruptedDefer(ctx, threadID, turnID, time.Now().UTC())
	if err != nil || !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(state.LastDecision), string(terminalGateDefer))
}

func threadLooksActiveForInput(thread *model.Thread) bool {
	if thread == nil {
		return false
	}
	return threadLooksActiveForPolling(*thread)
}

func steerFailureImpliesActive(err error) bool {
	if err == nil {
		return false
	}
	if steerFailureMeansNoActiveTurn(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "active turn") ||
		strings.Contains(msg, "activeturn") ||
		strings.Contains(msg, "already active") ||
		strings.Contains(msg, "in-flight") ||
		strings.Contains(msg, "not steerable")
}

func steerFailureMeansNoActiveTurn(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no active turn") ||
		strings.Contains(msg, "no active run") ||
		strings.Contains(msg, "turn is not active") ||
		strings.Contains(msg, "active turn already ended")
}

func activeTurnIDFromSteerMismatch(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "expected active turn id") || !strings.Contains(lower, "found") {
		return ""
	}
	matches := codexThreadIDExtractPattern.FindAllString(msg, -1)
	if len(matches) == 0 {
		return ""
	}
	// The app-server error lists expected first and authoritative active turn last.
	return matches[len(matches)-1]
}

func activeThreadReplyText(thread *model.Thread, steerErr error) string {
	label := "Thread"
	turnID := ""
	if thread != nil {
		label = thread.Label()
		turnID = strings.TrimSpace(thread.ActiveTurnID)
	}
	if steerErr != nil {
		if turnID != "" {
			return fmt.Sprintf("%s is already active, but Codex did not accept input for active turn %s: %v. I did not start a parallel turn.", label, turnID, steerErr)
		}
		return fmt.Sprintf("%s is already active, but Codex did not accept input: %v. I did not start a parallel turn.", label, steerErr)
	}
	if turnID != "" {
		return fmt.Sprintf("%s is already active. Reply to the current live turn card to steer turn %s, or wait for completion. I did not start a parallel turn.", label, turnID)
	}
	return fmt.Sprintf("%s is already active, but the active turn id is not available yet. Wait for completion or use /stop. I did not start a parallel turn.", label)
}

func (s *Service) stopThread(ctx context.Context, chatID, topicID int64, explicitThreadID string, replyToMessageID int64, sourceMode string) (*DirectResponse, error) {
	decision, err := s.resolveRouteFromSource(ctx, chatID, topicID, explicitThreadID, replyToMessageID, sourceMode)
	if err != nil {
		return nil, err
	}
	if decision.ThreadID == "" {
		return &DirectResponse{Text: s.t(ctx, "没有可停止的线程目标。请使用 /stop <thread>，或回复某条线程消息。", "No thread target for /stop. Use /stop <thread> or reply to a thread message.")}, nil
	}
	response, err := s.interruptTurn(ctx, chatID, topicID, decision.ThreadID, "")
	if response != nil && strings.TrimSpace(response.Text) == "" && strings.TrimSpace(response.CallbackText) != "" {
		response.Text = response.CallbackText
	}
	return response, err
}

func (s *Service) interruptTurn(ctx context.Context, chatID, topicID int64, threadID, turnID string) (*DirectResponse, error) {
	if strings.TrimSpace(threadID) == "" {
		return &DirectResponse{CallbackText: s.t(ctx, "没有可停止的线程目标。", "No thread target for stop.")}, nil
	}
	if err := s.setThreadCollaborationDefaultOverride(ctx, threadID); err != nil {
		return nil, err
	}
	thread, _ := s.store.GetThread(ctx, threadID)
	s.mu.RLock()
	live := s.live
	connected := s.liveConnected
	s.mu.RUnlock()
	if !connected || live == nil {
		return &DirectResponse{Text: s.t(ctx, "实时 app-server 会话尚未就绪。请尝试 /status 或 /repair。", "Live app-server session is not ready yet. Try /status or /repair.")}, nil
	}
	if thread != nil {
		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		started := time.Now()
		_, err := live.ThreadResume(requestCtx, threadID, thread.CWD)
		cancel()
		s.logAppServerCall("ThreadResume", started, err, live, lifecycleFields{
			"operation": "interrupt_turn",
			"thread_id": threadID,
		})
	}
	if refreshed, err := s.refreshThreadForOperation(ctx, live, threadID, "interrupt_turn_before_stop"); err == nil && refreshed != nil {
		thread = refreshed
	}
	snapshot, _ := s.store.GetSnapshot(ctx, threadID)
	latestTurnTerminal := snapshot != nil && isTerminalStatus(snapshot.LastSeenTurnStatus)
	if thread != nil && isTerminalStatus(thread.Status) {
		latestTurnTerminal = true
	}
	if thread != nil {
		if latestTurnTerminal {
			turnID = ""
		} else if threadLooksActiveForInput(thread) && strings.TrimSpace(thread.ActiveTurnID) != "" {
			turnID = thread.ActiveTurnID
		} else {
			turnID = ""
		}
	}
	if strings.TrimSpace(turnID) == "" {
		explicitTarget := model.ObserverTarget{ChatKey: model.ChatKey(chatID, topicID), ChatID: chatID, TopicID: topicID, Enabled: true}
		s.syncThreadPanelToTarget(ctx, explicitTarget, threadID, false, model.PanelSourceExplicit)
		return &DirectResponse{CallbackText: s.t(ctx, "线程已经空闲。", "Thread is already idle.")}, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	started := time.Now()
	if err := live.TurnInterrupt(requestCtx, threadID, turnID); err != nil {
		s.logAppServerCall("TurnInterrupt", started, err, live, lifecycleFields{
			"thread_id": threadID,
			"turn_id":   turnID,
		})
		return nil, err
	}
	_ = s.markChatOriginExplicitInterrupt(ctx, threadID, turnID)
	s.logAppServerCall("TurnInterrupt", started, nil, live, lifecycleFields{
		"thread_id": threadID,
		"turn_id":   turnID,
	})
	label := threadID
	if thread != nil {
		label = thread.Label()
	}
	explicitTarget := model.ObserverTarget{ChatKey: model.ChatKey(chatID, topicID), ChatID: chatID, TopicID: topicID, Enabled: true}
	s.syncThreadPanelToTarget(ctx, explicitTarget, threadID, false, model.PanelSourceExplicit)
	return &DirectResponse{CallbackText: fmt.Sprintf(s.t(ctx, "已请求中断 %s。", "Interrupt requested for %s."), label), ThreadID: threadID, TurnID: turnID}, nil
}

func (s *Service) approve(ctx context.Context, requestID, decision string) (*DirectResponse, error) {
	approval, err := s.store.GetPendingApproval(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if approval == nil {
		return &DirectResponse{Text: fmt.Sprintf(s.t(ctx, "未知审批请求：%s", "Unknown approval request: %s"), requestID)}, nil
	}
	s.mu.RLock()
	live := s.live
	connected := s.liveConnected
	s.mu.RUnlock()
	if !connected || live == nil {
		return &DirectResponse{Text: s.t(ctx, "实时 app-server 会话尚未就绪。请尝试 /repair。", "Live app-server session is not ready yet. Try /repair.")}, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := live.RespondServerRequest(requestCtx, requestID, map[string]any{"decision": decision}); err != nil {
		return nil, err
	}
	_ = s.store.UpdatePendingApprovalStatus(ctx, requestID, "resolved:"+decision)
	s.syncThreadPanel(ctx, approval.ThreadID)
	return &DirectResponse{CallbackText: fmt.Sprintf(s.t(ctx, "审批 %s 已处理。", "Approval %s resolved."), requestID), ThreadID: approval.ThreadID}, nil
}

func (s *Service) answerChoice(ctx context.Context, chatID, topicID int64, route *model.CallbackRoute, sourceMode string) (*DirectResponse, error) {
	if route == nil {
		return &DirectResponse{CallbackText: s.t(ctx, "这个按钮没有待处理问题。", "No pending question for this button.")}, nil
	}
	var payload map[string]any
	if strings.TrimSpace(route.PayloadJSON) != "" {
		_ = json.Unmarshal([]byte(route.PayloadJSON), &payload)
	}
	text := payloadMapString(payload, "text")
	if text == "" {
		return &DirectResponse{CallbackText: s.t(ctx, "回答选项为空。", "Answer option is empty.")}, nil
	}
	if strings.TrimSpace(route.RequestID) == "" {
		response, err := s.sendInputToThreadTurnFromSource(ctx, chatID, topicID, route.ThreadID, route.TurnID, text, "", sourceMode)
		if err != nil {
			return nil, err
		}
		if response == nil {
			response = &DirectResponse{}
		}
		response.CallbackText = s.t(ctx, "回答已发送。", "Answer sent.")
		return response, nil
	}
	response, err := s.respondUserInputRequest(ctx, route.RequestID, text)
	if err != nil {
		return nil, err
	}
	if response == nil {
		response = &DirectResponse{}
	}
	response.CallbackText = "Ответ отправлен."
	return response, nil
}

func (s *Service) threadsOverview(ctx context.Context, rest string) (*DirectResponse, error) {
	limit := 8
	search := ""
	if trimmed := strings.TrimSpace(rest); trimmed != "" {
		if parsed, err := strconv.Atoi(trimmed); err == nil {
			limit = parsed
		} else {
			search = trimmed
		}
	}
	threads, err := s.store.ListThreads(ctx, limit, search)
	if err != nil {
		return nil, err
	}
	threads = visibleThreadsForOverview(threads)
	lines := []string{}
	if len(threads) == 0 {
		lines = append(lines, s.t(ctx, "还没有缓存会话。请试试 /status，或等待同步。", "No cached threads yet. Try /status or wait for sync."))
		return &DirectResponse{Text: strings.Join(lines, "\n")}, nil
	}
	buttons := [][]model.ButtonSpec{}
	sections := []model.MessageSection{}
	groups := groupedThreadsForOverview(threads)
	for groupIndex, group := range groups {
		lines = append(lines, "", group.Project)
		section := model.MessageSection{Text: group.Project, Heading: true, Divider: groupIndex > 0}
		for _, thread := range group.Threads {
			label := threadOverviewLabel(thread, s.botLanguage(ctx))
			text := fmt.Sprintf("%s    %s", label, threadUpdatedAtLabel(thread.UpdatedAt, s.now))
			button := s.callbackButton(ctx, s.t(ctx, "打开", "Open"), "show_thread", thread.ID, "", "", nil)
			lines = append(lines, text)
			buttons = append(buttons, []model.ButtonSpec{button})
			section.Rows = append(section.Rows, model.MessageSectionRow{
				Title:    label,
				Trailing: threadUpdatedAtLabel(thread.UpdatedAt, s.now),
				Button:   button,
			})
		}
		sections = append(sections, section)
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Sections: sections, Buttons: buttons}, nil
}

func visibleThreadsForOverview(threads []model.Thread) []model.Thread {
	out := make([]model.Thread, 0, len(threads))
	for _, thread := range threads {
		if thread.Archived {
			continue
		}
		if threadLooksUnavailableForOverview(thread) {
			continue
		}
		out = append(out, thread)
	}
	return out
}

func threadLooksUnavailableForOverview(thread model.Thread) bool {
	if !strings.EqualFold(strings.TrimSpace(thread.Status), "notLoaded") {
		return false
	}
	title := strings.TrimSpace(thread.Title)
	if title == "" || strings.EqualFold(title, strings.TrimSpace(thread.ID)) || codexThreadIDPattern.MatchString(title) {
		return true
	}
	return false
}

func threadOverviewLabel(thread model.Thread, lang string) string {
	title := strings.TrimSpace(thread.Title)
	if title != "" && !strings.EqualFold(title, strings.TrimSpace(thread.ID)) && !codexThreadIDPattern.MatchString(title) {
		return title
	}
	if preview := trimPreview(thread.LastPreview); strings.TrimSpace(preview) != "" {
		return preview
	}
	return localized(lang, "未命名会话", "Untitled thread")
}

type threadOverviewGroup struct {
	Project string
	Threads []model.Thread
}

func groupedThreadsForOverview(threads []model.Thread) []threadOverviewGroup {
	groups := []threadOverviewGroup{}
	indexByProject := map[string]int{}
	for _, thread := range threads {
		project := strings.TrimSpace(thread.ProjectName)
		if project == "" || isCodexChatsCWD(thread.CWD) {
			project = "临时对话"
		}
		groupIndex, ok := indexByProject[project]
		if !ok {
			groupIndex = len(groups)
			indexByProject[project] = groupIndex
			groups = append(groups, threadOverviewGroup{Project: project})
		}
		groups[groupIndex].Threads = append(groups[groupIndex].Threads, thread)
	}
	return groups
}

func threadUpdatedAtLabel(updatedAt int64, now func() time.Time) string {
	if updatedAt <= 0 {
		return "unknown"
	}
	if now == nil {
		now = time.Now
	}
	updated := time.Unix(updatedAt, 0)
	if updated.IsZero() {
		return "unknown"
	}
	delta := now().Sub(updated)
	if delta < 0 {
		delta = 0
	}
	switch {
	case delta < time.Minute:
		return "刚刚"
	case delta < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(delta/time.Minute))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(delta/time.Hour))
	case delta < 48*time.Hour:
		return "昨天"
	case delta < 7*24*time.Hour:
		return fmt.Sprintf("%d 天前", int(delta/(24*time.Hour)))
	default:
		return updated.Local().Format("2006-01-02")
	}
}

func (s *Service) showThread(ctx context.Context, chatID, topicID int64, threadID string, forceNew bool, sourceMode string) (*DirectResponse, error) {
	thread, err := s.store.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if thread == nil {
		return &DirectResponse{Text: fmt.Sprintf(s.t(ctx, "未知线程：%s", "Unknown thread: %s"), threadID)}, nil
	}
	s.mu.RLock()
	live := s.live
	liveConnected := s.liveConnected
	poll := s.poll
	pollConnected := s.pollConnected
	s.mu.RUnlock()
	switch {
	case liveConnected && live != nil:
		if refreshed, refreshErr := s.refreshThread(ctx, live, threadID); refreshErr == nil && refreshed != nil {
			thread = refreshed
		}
	case pollConnected && poll != nil:
		if refreshed, refreshErr := s.refreshThread(ctx, poll, threadID); refreshErr == nil && refreshed != nil {
			thread = refreshed
		}
	}
	target := model.ObserverTarget{
		ChatKey: model.ChatKey(chatID, topicID),
		ChatID:  chatID,
		TopicID: topicID,
		Enabled: true,
	}
	if forceNew && normalizeInputSourceMode(sourceMode) == model.PanelSourceFeishuInput {
		ctx = model.WithForcedThreadTopicActivation(ctx)
	}
	s.syncThreadPanelToTarget(ctx, target, thread.ID, forceNew, sourceMode)
	return &DirectResponse{ThreadID: thread.ID}, nil
}

func (s *Service) contextCard(ctx context.Context, chatID, topicID int64) (string, error) {
	lines := []string{s.t(ctx, "当前上下文", "Current context")}
	lines = append(lines, s.t(ctx, "模式：未绑定", "Mode: Unbound"), s.t(ctx, "使用 /chats 或 /projects 选择一个线程。", "Use /chats or /projects to choose a thread."))
	return strings.Join(lines, "\n"), nil
}

func (s *Service) threadIDResponse(ctx context.Context, threadID, turnID string) *DirectResponse {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if threadID == "" {
		return &DirectResponse{Text: s.t(ctx, "这条消息没有可用的线程 ID。", "Thread ID is not available for this message.")}
	}
	responseTurnID := turnID
	if turnID == "" {
		turnID = "-"
	}
	return &DirectResponse{
		Text:     fmt.Sprintf("%s:\n%s\n\n%s:\n%s", s.t(ctx, "线程 ID", "Thread ID"), threadID, s.t(ctx, "轮次 ID", "Turn ID"), turnID),
		ThreadID: threadID,
		TurnID:   responseTurnID,
	}
}

func (s *Service) resolveRouteFromSource(ctx context.Context, chatID, topicID int64, explicitThreadID string, replyToMessageID int64, sourceMode string) (model.RouteDecision, error) {
	if strings.TrimSpace(explicitThreadID) != "" {
		return model.RouteDecision{ThreadID: strings.TrimSpace(explicitThreadID), Source: model.RouteSourceExplicit}, nil
	}
	if replyToMessageID != 0 {
		route, err := s.store.ResolveMessageRoute(ctx, chatID, topicID, replyToMessageID)
		if err != nil {
			return model.RouteDecision{}, err
		}
		if route != nil {
			return model.RouteDecision{ThreadID: route.ThreadID, TurnID: route.TurnID, RequestID: requestIDFromRouteEvent(route.EventID), Source: model.RouteSourceReply}, nil
		}
	}
	steerState, err := s.resolveArmedSteer(ctx, chatID, topicID)
	if err != nil {
		return model.RouteDecision{}, err
	}
	if steerState != nil && strings.TrimSpace(steerState.ThreadID) != "" {
		return model.RouteDecision{ThreadID: steerState.ThreadID, Source: model.RouteSourceSteer}, nil
	}
	if normalizeInputSourceMode(sourceMode) == model.PanelSourceFeishuInput {
		panel, err := s.store.GetLatestCurrentPanelForChat(ctx, chatID, topicID)
		if err != nil {
			return model.RouteDecision{}, err
		}
		if panel != nil && strings.TrimSpace(panel.ThreadID) != "" {
			return model.RouteDecision{ThreadID: panel.ThreadID, Source: model.RouteSourcePanel}, nil
		}
	}
	return model.RouteDecision{Source: model.RouteSourceNone}, nil
}

func (s *Service) respondUserInputRequest(ctx context.Context, requestID, text string) (*DirectResponse, error) {
	approval, err := s.store.GetPendingApproval(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if approval == nil {
		return &DirectResponse{Text: fmt.Sprintf(s.t(ctx, "未知输入请求：%s", "Unknown input request: %s"), requestID)}, nil
	}
	s.mu.RLock()
	live := s.live
	connected := s.liveConnected
	s.mu.RUnlock()
	if !connected || live == nil {
		return &DirectResponse{Text: s.t(ctx, "实时 app-server 会话尚未就绪。请尝试 /repair。", "Live app-server session is not ready yet. Try /repair.")}, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := live.RespondServerRequest(requestCtx, requestID, userInputResponsePayload(approval.PayloadJSON, text)); err != nil {
		return &DirectResponse{Text: fmt.Sprintf(s.t(ctx, "无法发送回答：%v", "Could not send answer: %v"), err)}, nil
	}
	_ = s.store.UpdatePendingApprovalStatus(ctx, requestID, "resolved:reply")
	s.syncThreadPanel(ctx, approval.ThreadID)
	return &DirectResponse{ThreadID: approval.ThreadID, TurnID: approval.TurnID}, nil
}

func userInputResponsePayload(payloadJSON, text string) map[string]any {
	var payload map[string]any
	if strings.TrimSpace(payloadJSON) != "" {
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
	}
	questions, _ := payload["questions"].([]any)
	if len(questions) == 0 {
		return map[string]any{
			"text":     text,
			"value":    text,
			"response": text,
			"input":    text,
		}
	}
	answers := map[string]any{}
	for _, rawQuestion := range questions {
		question, _ := rawQuestion.(map[string]any)
		if question == nil {
			continue
		}
		id := payloadMapString(question, "id")
		if id == "" {
			continue
		}
		answers[id] = map[string]any{"answers": []string{text}}
	}
	if len(answers) == 0 {
		return map[string]any{
			"text":     text,
			"value":    text,
			"response": text,
			"input":    text,
		}
	}
	return map[string]any{"answers": answers}
}

func requestIDFromRouteEvent(eventID string) string {
	eventID = strings.TrimSpace(eventID)
	if !strings.HasPrefix(eventID, "plan_request:") {
		return ""
	}
	return strings.TrimPrefix(eventID, "plan_request:")
}

func (s *Service) enqueueObserverEvent(ctx context.Context, event model.ObserverEvent) {
}

func (s *Service) trackedThreads(ctx context.Context, limit int) []model.Thread {
	seen := map[string]struct{}{}
	out := []model.Thread{}
	for _, threadID := range s.currentPanelThreadIDs(ctx) {
		thread, err := s.store.GetThread(ctx, threadID)
		if err != nil || thread == nil {
			continue
		}
		if thread.Archived {
			continue
		}
		seen[thread.ID] = struct{}{}
		out = append(out, *thread)
	}
	for _, threadID := range s.feishuTopicThreadIDs(ctx, limit) {
		if _, ok := seen[threadID]; ok {
			continue
		}
		thread, err := s.store.GetThread(ctx, threadID)
		if err != nil || thread == nil || thread.Archived {
			continue
		}
		seen[thread.ID] = struct{}{}
		out = append(out, *thread)
	}
	recent, _ := s.store.ListThreads(ctx, limit, "")
	for _, thread := range recent {
		if _, ok := seen[thread.ID]; ok {
			continue
		}
		if !threadLooksActiveForPolling(thread) {
			continue
		}
		seen[thread.ID] = struct{}{}
		out = append(out, thread)
	}
	return out
}

func (s *Service) feishuTopicThreadIDs(ctx context.Context, limit int) []string {
	topics, err := s.store.ListFeishuThreadTopics(ctx, limit)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(topics))
	for _, topic := range topics {
		threadID := strings.TrimSpace(topic.ThreadID)
		if threadID == "" {
			continue
		}
		if _, ok := seen[threadID]; ok {
			continue
		}
		seen[threadID] = struct{}{}
		out = append(out, threadID)
	}
	return out
}

func (s *Service) currentPanelThreadIDs(ctx context.Context) []string {
	threads, err := s.store.ListThreads(ctx, 100, "")
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(threads))
	for _, thread := range threads {
		panels, err := s.store.ListCurrentPanelsForThread(ctx, thread.ID)
		if err != nil || len(panels) == 0 {
			continue
		}
		track := false
		for _, panel := range panels {
			if shouldTrackCurrentPanel(panel) {
				track = true
				break
			}
		}
		if !track {
			continue
		}
		if _, ok := seen[thread.ID]; ok {
			continue
		}
		seen[thread.ID] = struct{}{}
		out = append(out, thread.ID)
	}
	return out
}

func shouldTrackCurrentPanel(panel model.ThreadPanel) bool {
	status := strings.TrimSpace(panel.Status)
	if status == "" {
		return true
	}
	return !isTerminalStatus(status)
}

func (s *Service) threadNeedsLiveSync(ctx context.Context, threadID string) bool {
	if topic, err := s.store.GetAnyFeishuThreadTopicByCodexThread(ctx, threadID); err == nil && topic != nil {
		return true
	}
	panels, err := s.store.ListCurrentPanelsForThread(ctx, threadID)
	if err != nil {
		return false
	}
	for _, panel := range panels {
		if shouldTrackCurrentPanel(panel) {
			return true
		}
	}
	return false
}

func threadLooksActiveForPolling(thread model.Thread) bool {
	status := strings.ToLower(strings.TrimSpace(thread.Status))
	if status == "" {
		return strings.TrimSpace(thread.ActiveTurnID) != ""
	}
	if status == "active" ||
		strings.HasPrefix(status, "active[") ||
		strings.Contains(status, "waitingon") ||
		strings.Contains(status, "inprogress") ||
		strings.Contains(status, "running") {
		return true
	}
	switch status {
	case "idle", "notloaded", "not_loaded", "completed", "interrupted", "failed", "cancelled", "canceled":
		return false
	default:
		return false
	}
}

func snapshotHasPassiveChange(previous *model.ThreadSnapshotState, current *appserver.ThreadReadSnapshot) bool {
	if current == nil {
		return false
	}
	if previous == nil {
		return threadLooksActiveForPolling(current.Thread) || current.WaitingOnApproval || current.WaitingOnReply
	}
	if interruptedVisibleStateChanged(previous, current) {
		return true
	}
	return len(appserver.DiffSnapshot(previous, *current)) > 0
}

func interruptedVisibleStateChanged(previous *model.ThreadSnapshotState, current *appserver.ThreadReadSnapshot) bool {
	if previous == nil || current == nil || len(previous.CompactJSON) == 0 {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(current.LatestTurnStatus), "interrupted") || snapshotHasFinalSignal(current) {
		return false
	}
	var previousSnapshot appserver.ThreadReadSnapshot
	if err := json.Unmarshal(previous.CompactJSON, &previousSnapshot); err != nil {
		return false
	}
	return interruptedVisibleFingerprint(&previousSnapshot) != interruptedVisibleFingerprint(current)
}

func interruptedVisibleFingerprint(snapshot *appserver.ThreadReadSnapshot) string {
	if snapshot == nil {
		return ""
	}
	return hashStrings(
		cardDisplayStatus(snapshot, snapshot.Thread),
		snapshot.LatestProgressFP,
		snapshot.LatestProgressText,
		snapshot.LatestToolFP,
		snapshot.LatestToolID,
		snapshot.LatestToolStatus,
		detailItemsVisibleFingerprint(snapshot.DetailItems),
		agentEntriesVisibleFingerprint(snapshot.LatestAgentMessageEntries),
	)
}

func detailItemsVisibleFingerprint(items []model.DetailItem) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, strings.Join([]string{item.ID, item.Kind, item.Phase, item.Text, item.Label, item.Status, item.Output, item.FP}, "\x00"))
	}
	return strings.Join(parts, "\x01")
}

func agentEntriesVisibleFingerprint(entries []appserver.AgentMessageEntry) string {
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		parts = append(parts, strings.Join([]string{entry.ID, entry.Phase, entry.Text, entry.FP}, "\x00"))
	}
	return strings.Join(parts, "\x01")
}

func (s *Service) threadNeedsCatchupPolling(ctx context.Context, thread model.Thread, snapshot *model.ThreadSnapshotState) bool {
	updatedAt := time.Unix(thread.UpdatedAt, 0).UTC()
	if thread.UpdatedAt <= 0 || updatedAt.IsZero() {
		return false
	}
	if time.Since(updatedAt) > s.catchupWindow() {
		return false
	}
	if snapshot == nil {
		return true
	}
	if thread.UpdatedAt > snapshot.ThreadUpdatedAt {
		return true
	}
	if snapshot.LastSeenThreadStatus != "" && !strings.EqualFold(strings.TrimSpace(snapshot.LastSeenThreadStatus), strings.TrimSpace(thread.Status)) {
		return true
	}
	if snapshot.LastSeenTurnID != "" && thread.ActiveTurnID != "" && snapshot.LastSeenTurnID != thread.ActiveTurnID {
		return true
	}
	if s.threadHasDeferredEmptyInterrupted(ctx, thread, snapshot) {
		return true
	}
	return false
}

func isThreadNotLoadedError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "thread not loaded")
}

func isThreadArchivedError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "is archived") || strings.Contains(text, "thread archived") || strings.Contains(text, "session archived")
}

func (s *Service) catchupWindow() time.Duration {
	return maxDuration(90*time.Second, s.cfg.IndexRefreshInterval*2)
}

func mergeThreadMetadata(current, fallback model.Thread) model.Thread {
	if strings.TrimSpace(current.ID) == "" {
		current.ID = fallback.ID
	}
	if strings.TrimSpace(current.Title) == "" {
		current.Title = fallback.Title
	}
	if strings.TrimSpace(current.CWD) == "" {
		current.CWD = fallback.CWD
	}
	if strings.TrimSpace(current.ProjectName) == "" {
		current.ProjectName = fallback.ProjectName
	}
	if strings.TrimSpace(current.DirectoryName) == "" {
		current.DirectoryName = fallback.DirectoryName
	}
	if current.UpdatedAt == 0 {
		current.UpdatedAt = fallback.UpdatedAt
	}
	if strings.TrimSpace(current.Status) == "" {
		current.Status = fallback.Status
	}
	if strings.TrimSpace(current.LastPreview) == "" {
		current.LastPreview = fallback.LastPreview
	}
	if strings.TrimSpace(current.ActiveTurnID) == "" && !isTerminalStatus(current.Status) {
		current.ActiveTurnID = fallback.ActiveTurnID
	}
	if strings.TrimSpace(current.PreferredModel) == "" {
		current.PreferredModel = fallback.PreferredModel
	}
	if strings.TrimSpace(current.PermissionsMode) == "" {
		current.PermissionsMode = fallback.PermissionsMode
	}
	if len(current.Raw) == 0 {
		current.Raw = fallback.Raw
	}
	if !current.Archived {
		current.Archived = fallback.Archived
	}
	return current
}

func (s *Service) refreshThread(ctx context.Context, client Session, threadID string) (*model.Thread, error) {
	return s.refreshThreadForOperation(ctx, client, threadID, "thread_read")
}

func (s *Service) refreshThreadForOperation(ctx context.Context, client Session, threadID, operation string) (*model.Thread, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	started := time.Now()
	payload, err := client.ThreadRead(requestCtx, threadID, true)
	s.logAppServerCall("ThreadRead", started, err, client, lifecycleFields{
		"operation":      operation,
		"thread_id":      threadID,
		"include_turns":  true,
		"fallback_next":  err != nil,
		"poll_connected": false,
	})
	if err != nil {
		started = time.Now()
		payload, err = client.ThreadRead(requestCtx, threadID, false)
		s.logAppServerCall("ThreadRead", started, err, client, lifecycleFields{
			"operation":      operation,
			"thread_id":      threadID,
			"include_turns":  false,
			"poll_connected": false,
		})
		if err != nil {
			return nil, err
		}
	}
	current := appserver.SnapshotFromThreadRead(payload)
	current.Thread.Raw, _ = json.Marshal(payload)
	thread := current.Thread
	if existing, _ := s.store.GetThread(ctx, threadID); existing != nil {
		thread = mergeThreadMetadata(thread, *existing)
	} else if thread.ID == "" {
		thread.ID = threadID
	}
	current.Thread = thread
	thread = current.Thread
	previous, err := s.store.GetSnapshot(ctx, threadID)
	if err != nil {
		return nil, err
	}
	gateHandled, gateDecision := s.applyChatOriginTerminalGate(ctx, operation, &current, previous)
	if gateHandled {
		if gateDecision.DeferredDisplayableProgress() {
			s.preserveChatOriginLiveCurrentTool(ctx, &current, previous)
			if err := s.store.UpsertThread(ctx, thread); err != nil {
				return nil, err
			}
			nextSnapshot := s.compactDeferredProgressSnapshot(previous, current, time.Now().UTC(), gateDecision)
			if err := s.store.UpsertSnapshot(ctx, threadID, nextSnapshot); err != nil {
				return nil, err
			}
			s.logObserverSyncResult(operation, current)
			return &thread, nil
		}
		if existing, _ := s.store.GetThread(ctx, threadID); existing != nil {
			return existing, nil
		}
		return &thread, nil
	}
	s.preserveChatOriginLiveCurrentTool(ctx, &current, previous)
	if err := s.store.UpsertThread(ctx, thread); err != nil {
		return nil, err
	}
	nextSnapshot := appserver.CompactSnapshot(previous, current, time.Now().UTC())
	if current.LatestTurnStatus == "inProgress" || current.WaitingOnApproval || current.WaitingOnReply {
		nextSnapshot.NextPollAfter = model.TimeString(time.Now().UTC().Add(s.cfg.ObserverPollInterval).Format(time.RFC3339Nano))
	}
	applyTerminalGateHotPolling(&nextSnapshot, gateDecision)
	if err := s.store.UpsertSnapshot(ctx, threadID, nextSnapshot); err != nil {
		return nil, err
	}
	s.logObserverSyncResult(operation, current)
	s.maybeLogChatOriginTerminal(ctx, current)
	return &thread, nil
}

func (s *Service) callbackButton(ctx context.Context, text, action, threadID, turnID, requestID string, payload map[string]any) model.ButtonSpec {
	token := randomToken()
	route := model.CallbackRoute{
		Token:       token,
		Action:      action,
		ThreadID:    threadID,
		TurnID:      turnID,
		RequestID:   requestID,
		Status:      model.CallbackStatusActive,
		PayloadJSON: storage.MustJSON(payload),
		CreatedAt:   model.NowString(),
	}
	_ = s.store.PutCallbackRoute(ctx, route)
	return model.ButtonSpec{Text: text, CallbackData: token}
}

func (s *Service) kickBootstrap() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		s.bootstrapTrackedState(ctx)
	}()
}

func (s *Service) noteSessionError(ctx context.Context, operation string, err error) {
	if err == nil {
		return
	}
	s.logLifecycle("appserver_session_error", lifecycleFields{
		"operation": operation,
		"error":     err,
	})
	s.setError(ctx, fmt.Errorf("%s: %w", operation, err))
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	_ = s.RequestRepair(ctx, operation)
}

func (s *Service) setError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	message := sanitizeDiagnosticString(err.Error())
	s.mu.Lock()
	s.lastError = message
	s.mu.Unlock()
	_ = s.store.SetState(ctx, "daemon.last_error", message)
}

func (s *Service) renderObserverEvent(ctx context.Context, event model.ObserverEvent) *DirectResponse {
	thread := model.Thread{ID: event.ThreadID, Title: event.ThreadTitle, ProjectName: event.ProjectName}
	lines := []string{
		s.visualHeader(ctx, s.t(ctx, "事件", "Event"), thread, event.TurnID),
		event.Text,
		"",
		fmt.Sprintf("/show %s", event.ThreadID),
		fmt.Sprintf("/reply %s <text>", event.ThreadID),
	}
	if event.NeedsApproval {
		lines = append(lines, "/status")
	}
	buttons := [][]model.ButtonSpec{{
		s.callbackButton(ctx, s.t(ctx, "查看", "Show"), "show_thread", event.ThreadID, event.TurnID, "", nil),
		s.callbackButton(ctx, s.t(ctx, "回复", "Reply"), "reply_hint", event.ThreadID, event.TurnID, "", nil),
	}}
	if event.TurnID != "" {
		buttons = append(buttons, []model.ButtonSpec{s.callbackButton(ctx, s.t(ctx, "停止", "Stop"), "stop_turn", event.ThreadID, event.TurnID, "", nil)})
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons, ThreadID: event.ThreadID, TurnID: event.TurnID, ItemID: event.ItemID, EventID: event.EventID}
}

func (s *Service) renderPendingApproval(ctx context.Context, approval model.PendingApproval) *DirectResponse {
	thread := model.Thread{ID: approval.ThreadID, Title: approval.ThreadID, ProjectName: "Codex"}
	if loaded, _ := s.store.GetThread(ctx, approval.ThreadID); loaded != nil {
		thread = *loaded
	}
	lines := []string{
		s.visualHeader(ctx, s.t(ctx, "审批", "Approval"), thread, approval.TurnID),
		strings.TrimSpace(approval.Question),
		"",
		fmt.Sprintf("/approve %s", approval.RequestID),
		fmt.Sprintf("/deny %s", approval.RequestID),
		fmt.Sprintf("/show %s", approval.ThreadID),
	}
	buttons := [][]model.ButtonSpec{
		{
			s.callbackButton(ctx, s.t(ctx, "批准", "Approve"), "approve", approval.ThreadID, approval.TurnID, approval.RequestID, nil),
			s.callbackButton(ctx, s.t(ctx, "批准会话", "Approve Session"), "approve_session", approval.ThreadID, approval.TurnID, approval.RequestID, nil),
		},
		{
			s.callbackButton(ctx, s.t(ctx, "拒绝", "Deny"), "deny", approval.ThreadID, approval.TurnID, approval.RequestID, nil),
			s.callbackButton(ctx, s.t(ctx, "取消", "Cancel"), "cancel", approval.ThreadID, approval.TurnID, approval.RequestID, nil),
		},
	}
	return &DirectResponse{Text: strings.Join(lines, "\n"), Buttons: buttons, ThreadID: approval.ThreadID, TurnID: approval.TurnID, ItemID: approval.ItemID, EventID: approval.RequestID}
}

func randomToken() string {
	var bytes [16]byte
	_, _ = rand.Read(bytes[:])
	return hex.EncodeToString(bytes[:])
}

func parseTime(value model.TimeString) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, string(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func maxDuration(left, right time.Duration) time.Duration {
	if left > right {
		return left
	}
	return right
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func threadIDFromEvent(event appserver.Event) string {
	if event.Params == nil {
		return ""
	}
	if value, ok := event.Params["threadId"].(string); ok {
		return value
	}
	if thread, ok := event.Params["thread"].(map[string]any); ok {
		if value, ok := thread["id"].(string); ok {
			return value
		}
	}
	return ""
}

func appserverThreadTurnID(payload map[string]any) string {
	turn, _ := payload["turn"].(map[string]any)
	if turn == nil {
		return ""
	}
	if id, ok := turn["id"].(string); ok {
		return id
	}
	return ""
}

func trimPreview(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 120 {
		return value
	}
	return value[:117] + "..."
}
