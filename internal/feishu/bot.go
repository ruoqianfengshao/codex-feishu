package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/ruoqianfengshao/codex-feishu/internal/appserver"
	"github.com/ruoqianfengshao/codex-feishu/internal/config"
	"github.com/ruoqianfengshao/codex-feishu/internal/daemon"
	"github.com/ruoqianfengshao/codex-feishu/internal/model"
	"github.com/ruoqianfengshao/codex-feishu/internal/storage"
)

const (
	namespaceChat    = "feishu.chat"
	namespaceMessage = "feishu.message"
	namespaceUser    = "feishu.user"

	feishuWorkspaceRoomName = "Codex 工作台"
	feishuBotLanguageKey    = "bot.language"
)

type Bot struct {
	cfg     config.Config
	service *daemon.Service
	store   *storage.Store
	apiMu   sync.RWMutex
	api     apiClient
	apiNew  func() apiClient
	logger  *log.Logger
	wsMu    sync.Mutex
	ws      wsClient
	wsNew   func() wsClient
}

type wsClient interface {
	Start(context.Context) error
}

func NewBot(cfg config.Config, service *daemon.Service, store *storage.Store, logger *log.Logger) (*Bot, error) {
	if strings.TrimSpace(cfg.FeishuAppID) == "" || strings.TrimSpace(cfg.FeishuAppSecret) == "" {
		return nil, errors.New("CTR_GO_FEISHU_APP_ID and CTR_GO_FEISHU_APP_SECRET must be set")
	}
	if logger == nil {
		logger = log.Default()
	}
	bot := &Bot{
		cfg:     cfg,
		service: service,
		store:   store,
		logger:  logger,
	}
	bot.apiNew = func() apiClient {
		return newSDKAPIClient(cfg.FeishuAppID, cfg.FeishuAppSecret)
	}
	bot.api = bot.apiNew()
	bot.wsNew = bot.newWSClient
	bot.ws = bot.wsNew()
	return bot, nil
}

func (b *Bot) Start(ctx context.Context) error {
	if b.ws == nil {
		return errors.New("feishu websocket client is not configured")
	}
	b.logger.Printf("feishu bot ready: app_id=%s", safeAppID(b.cfg.FeishuAppID))
	return nil
}

func (b *Bot) Run(ctx context.Context) error {
	const maxDelay = 30 * time.Second
	delay := time.Second
	for {
		ws := b.currentWSClient()
		if ws == nil {
			return nil
		}
		err := ws.Start(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if b.logger != nil {
			b.logger.Printf("feishu websocket disconnected; reconnecting after %s: %v", delay, err)
		}
		if !sleepContext(ctx, delay) {
			return nil
		}
		b.rebuildWSClient()
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func (b *Bot) currentWSClient() wsClient {
	if b == nil {
		return nil
	}
	b.wsMu.Lock()
	defer b.wsMu.Unlock()
	return b.ws
}

func (b *Bot) rebuildWSClient() wsClient {
	if b == nil {
		return nil
	}
	b.wsMu.Lock()
	defer b.wsMu.Unlock()
	if b.wsNew == nil {
		b.wsNew = b.newWSClient
	}
	b.ws = b.wsNew()
	return b.ws
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (b *Bot) String() string {
	return "feishu bot"
}

func (b *Bot) currentAPI() apiClient {
	if b == nil {
		return nil
	}
	b.apiMu.RLock()
	api := b.api
	b.apiMu.RUnlock()
	return api
}

func (b *Bot) rebuildAPIClient(reason error) apiClient {
	if b == nil {
		return nil
	}
	b.apiMu.Lock()
	defer b.apiMu.Unlock()
	if b.apiNew == nil {
		b.apiNew = func() apiClient {
			return newSDKAPIClient(b.cfg.FeishuAppID, b.cfg.FeishuAppSecret)
		}
	}
	b.api = b.apiNew()
	if b.logger != nil {
		b.logger.Printf("feishu api client rebuilt after auth error: %v", reason)
	}
	return b.api
}

func (b *Bot) withRecoveredAPIClient(ctx context.Context, call func(context.Context, apiClient) (sentMessage, error)) (sentMessage, error) {
	api := b.currentAPI()
	if api == nil {
		return sentMessage{}, errors.New("feishu api client is not configured")
	}
	sent, err := call(ctx, api)
	if err == nil || !isFeishuAuthError(err) {
		return sent, err
	}
	api = b.rebuildAPIClient(err)
	if api == nil {
		return sentMessage{}, err
	}
	if refreshErr := api.RefreshAuth(ctx); refreshErr != nil {
		return sentMessage{}, authRecoveryError{err: refreshErr}
	}
	return call(ctx, api)
}

func (b *Bot) newWSClient() wsClient {
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(b.handleMessageEvent).
		OnP2CardActionTrigger(b.handleCardAction).
		OnP2BotMenuV6(b.handleBotMenu)
	return larkws.NewClient(
		b.cfg.FeishuAppID,
		b.cfg.FeishuAppSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithLogLevel(sdkLogLevel(false)),
	)
}

func (b *Bot) SendMessage(ctx context.Context, chatID, topicID int64, text string, buttons [][]model.ButtonSpec, options model.SendOptions) (int64, error) {
	chunks := splitText(strings.TrimSpace(text), messageLimit)
	if len(chunks) == 0 {
		chunks = []string{" "}
	}
	var lastID int64
	for index, chunk := range chunks {
		var id int64
		var err error
		if index == len(chunks)-1 && len(buttons) > 0 {
			id, err = b.sendCardWithOptions(ctx, chatID, chunk, buttons, options)
		} else {
			id, err = b.sendMarkdownWithOptions(ctx, chatID, chunk, options)
		}
		if err != nil {
			return 0, err
		}
		lastID = id
	}
	return lastID, nil
}

func (b *Bot) SendRenderedMessages(ctx context.Context, chatID, topicID int64, messages []model.RenderedMessage, buttons [][]model.ButtonSpec, options model.SendOptions) ([]int64, error) {
	if len(messages) == 0 {
		messages = []model.RenderedMessage{{Text: " "}}
	}
	ids := make([]int64, 0, len(messages))
	for index, message := range messages {
		if renderPlainText(message) == "" {
			message.Text = " "
		}
		var id int64
		var err error
		if index == len(messages)-1 && len(buttons) > 0 {
			id, err = b.sendRenderedCardWithOptions(ctx, chatID, message, buttons, options)
		} else {
			id, err = b.sendRenderedCardWithOptions(ctx, chatID, message, nil, options)
		}
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (b *Bot) EditMessage(ctx context.Context, chatID, topicID, messageID int64, text string, buttons [][]model.ButtonSpec) error {
	mapped, err := b.store.GetFeishuMessageByNumericID(ctx, messageID)
	if err != nil {
		return err
	}
	if mapped == nil {
		return fmt.Errorf("feishu message route not found: %d", messageID)
	}
	editCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	card, err := buildCard(text, buttons)
	if err != nil {
		return err
	}
	return b.api.PatchCard(editCtx, mapped.OpenMessageID, card)
}

func (b *Bot) EditRenderedMessage(ctx context.Context, chatID, topicID, messageID int64, rendered model.RenderedMessage, buttons [][]model.ButtonSpec) error {
	mapped, err := b.store.GetFeishuMessageByNumericID(ctx, messageID)
	if err != nil {
		return err
	}
	if mapped == nil {
		return fmt.Errorf("feishu message route not found: %d", messageID)
	}
	editCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	card, err := buildRenderedCard(rendered, buttons)
	if err != nil {
		return err
	}
	return b.api.PatchCard(editCtx, mapped.OpenMessageID, card)
}

func (b *Bot) DeleteMessage(ctx context.Context, chatID, topicID, messageID int64) error {
	return nil
}

func (b *Bot) SendDocumentData(ctx context.Context, chatID, topicID int64, fileName string, data []byte, caption string, options model.SendOptions) (int64, error) {
	if strings.TrimSpace(fileName) == "" {
		fileName = "document.txt"
	}
	uploadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	fileKey, err := b.api.UploadFile(uploadCtx, fileName, data)
	cancel()
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(fileKey) == "" {
		return 0, errors.New("feishu upload file did not return file key")
	}
	if strings.TrimSpace(caption) != "" {
		if _, err := b.sendMarkdownWithOptions(ctx, chatID, strings.TrimSpace(caption), options); err != nil {
			return 0, err
		}
	}
	content, err := encodeFileContent(fileKey)
	if err != nil {
		return 0, err
	}
	return b.sendWithOptions(ctx, chatID, "file", content, options)
}

func (b *Bot) EnsureThreadTopic(ctx context.Context, chatID int64, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, sourceMode string) (*model.FeishuThreadTopic, error) {
	threadID := strings.TrimSpace(thread.ID)
	if threadID == "" {
		return nil, nil
	}
	sourceChatID := chatID
	targetChatID, err := b.threadTopicChatID(ctx, chatID)
	if err != nil {
		return nil, err
	}
	chatID = targetChatID
	if existing, err := b.store.GetFeishuThreadTopicByCodexThread(ctx, chatID, threadID); err != nil || existing != nil {
		if err != nil || existing == nil {
			return existing, err
		}
		if existing.RootMessageID == 0 {
			return existing, nil
		}
		valid, err := b.threadTopicRootMatchesChat(ctx, existing, chatID)
		if err != nil {
			return nil, err
		}
		if !valid {
			return b.createThreadTopicRoot(ctx, sourceChatID, chatID, thread, snapshot, sourceMode)
		}
		if err := b.activateThreadTopic(ctx, sourceChatID, chatID, threadID, existing, sourceMode); err != nil {
			return nil, err
		}
		return b.store.GetFeishuThreadTopicByCodexThread(ctx, chatID, threadID)
	}
	if existing, err := b.reusableThreadTopic(ctx, threadID); err != nil || existing != nil {
		if err != nil || existing == nil {
			return existing, err
		}
		if existing.RootMessageID == 0 {
			return existing, nil
		}
		valid, err := b.threadTopicRootMatchesChat(ctx, existing, existing.ChatID)
		if err != nil {
			return nil, err
		}
		if !valid {
			return b.createThreadTopicRoot(ctx, sourceChatID, chatID, thread, snapshot, sourceMode)
		}
		if err := b.activateThreadTopic(ctx, sourceChatID, existing.ChatID, threadID, existing, sourceMode); err != nil {
			return nil, err
		}
		return b.store.GetFeishuThreadTopicByCodexThread(ctx, existing.ChatID, threadID)
	}
	return b.createThreadTopicRoot(ctx, sourceChatID, chatID, thread, snapshot, sourceMode)
}

func (b *Bot) createThreadTopicRoot(ctx context.Context, sourceChatID, chatID int64, thread model.Thread, snapshot *appserver.ThreadReadSnapshot, sourceMode string) (*model.FeishuThreadTopic, error) {
	threadID := strings.TrimSpace(thread.ID)
	openChatID, err := b.store.ExternalIDForNumeric(ctx, namespaceChat, chatID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(openChatID) == "" {
		return nil, fmt.Errorf("feishu chat mapping not found: %d", chatID)
	}
	text := renderThreadTopicRootText(thread, snapshot, sourceMode)
	rootMessageID, err := b.sendCard(ctx, chatID, text, nil)
	if err != nil {
		return nil, err
	}
	if rootMessageID == 0 {
		return nil, nil
	}
	mapped, err := b.store.GetFeishuMessageByNumericID(ctx, rootMessageID)
	if err != nil {
		return nil, err
	}
	if mapped == nil {
		return nil, fmt.Errorf("feishu root message route not found: %d", rootMessageID)
	}
	topic := model.FeishuThreadTopic{
		ChatID:            chatID,
		OpenChatID:        firstNonEmptyString(mapped.OpenChatID, openChatID),
		ThreadID:          threadID,
		RootMessageID:     rootMessageID,
		RootOpenMessageID: mapped.OpenMessageID,
		FeishuThreadID:    "",
	}
	if err := b.store.UpsertFeishuThreadTopic(ctx, topic); err != nil {
		return nil, err
	}
	stored, err := b.store.GetFeishuThreadTopicByCodexThread(ctx, chatID, threadID)
	if err != nil {
		return nil, err
	}
	if err := b.activateThreadTopic(ctx, sourceChatID, chatID, threadID, stored, sourceMode); err != nil {
		return nil, err
	}
	return b.store.GetFeishuThreadTopicByCodexThread(ctx, chatID, threadID)
}

func (b *Bot) threadTopicRootMatchesChat(ctx context.Context, topic *model.FeishuThreadTopic, chatID int64) (bool, error) {
	if topic == nil || topic.RootMessageID == 0 {
		return false, nil
	}
	mapped, err := b.store.GetFeishuMessageByNumericID(ctx, topic.RootMessageID)
	if err != nil || mapped == nil {
		return mapped != nil, err
	}
	return mapped.ChatID == chatID, nil
}

func (b *Bot) reusableThreadTopic(ctx context.Context, threadID string) (*model.FeishuThreadTopic, error) {
	existing, err := b.store.GetAnyFeishuThreadTopicByCodexThread(ctx, threadID)
	if err != nil || existing == nil {
		return existing, err
	}
	if b.isControlRoomTopic(ctx, existing) {
		return nil, nil
	}
	return existing, nil
}

func (b *Bot) isControlRoomTopic(ctx context.Context, topic *model.FeishuThreadTopic) bool {
	if topic == nil {
		return false
	}
	if strings.TrimSpace(topic.OpenChatID) != "" {
		sourceChatID, _, err := b.controlRoomSource(ctx, topic.OpenChatID)
		if err == nil && sourceChatID != 0 {
			return true
		}
	}
	openChatID, err := b.store.ExternalIDForNumeric(ctx, namespaceChat, topic.ChatID)
	if err != nil || strings.TrimSpace(openChatID) == "" {
		return false
	}
	sourceChatID, _, err := b.controlRoomSource(ctx, openChatID)
	return err == nil && sourceChatID != 0
}

func (b *Bot) ResolveThreadTopicTarget(ctx context.Context, chatID int64) (int64, error) {
	return b.threadTopicChatID(ctx, chatID)
}

func (b *Bot) activateThreadTopic(ctx context.Context, sourceChatID, topicChatID int64, threadID string, topic *model.FeishuThreadTopic, sourceMode string) error {
	if sourceMode != model.PanelSourceFeishuInput || topic == nil || topic.RootMessageID == 0 {
		return nil
	}
	if !model.ForceThreadTopicActivation(ctx) {
		activated, err := b.store.GetState(ctx, feishuThreadTopicActivatedKey(topicChatID, threadID))
		if err != nil {
			return err
		}
		if strings.TrimSpace(activated) != "" {
			return nil
		}
	}
	content, err := encodeTextContent(b.threadTopicActivationText(ctx, sourceChatID, topicChatID))
	if err != nil {
		return err
	}
	_, err = b.sendWithOptions(ctx, topicChatID, "text", content, model.SendOptions{
		FeishuReplyToMessageID: topic.RootMessageID,
		FeishuReplyInThread:    true,
		FeishuCodexThreadID:    threadID,
	})
	if err != nil {
		return err
	}
	return b.store.SetState(ctx, feishuThreadTopicActivatedKey(topicChatID, threadID), "1")
}

func (b *Bot) threadTopicActivationText(ctx context.Context, sourceChatID, topicChatID int64) string {
	lang := b.botLanguage(ctx)
	openUserID, _ := b.store.GetState(ctx, feishuControlUserKey(sourceChatID))
	if strings.TrimSpace(openUserID) == "" && sourceChatID != topicChatID {
		openUserID, _ = b.store.GetState(ctx, feishuControlUserKey(topicChatID))
	}
	if strings.TrimSpace(openUserID) == "" {
		return botText(lang, "已打开 Codex 会话话题", "Codex thread topic opened")
	}
	return fmt.Sprintf(botText(lang, `<at user_id="%s">你</at> 已打开 Codex 会话话题`, `<at user_id="%s">You</at> opened the Codex thread topic`), strings.TrimSpace(openUserID))
}

func (b *Bot) threadTopicChatID(ctx context.Context, chatID int64) (int64, error) {
	openChatID, err := b.store.ExternalIDForNumeric(ctx, namespaceChat, chatID)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(openChatID) == "" {
		return 0, fmt.Errorf("feishu chat mapping not found: %d", chatID)
	}
	isP2P := strings.HasPrefix(openChatID, "p2p:")
	if !isP2P {
		chatMode, err := b.chatMode(ctx, chatID)
		if err != nil {
			return 0, err
		}
		if chatMode == "" {
			infoCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			info, getErr := b.api.GetChat(infoCtx, openChatID)
			cancel()
			if getErr != nil {
				return chatID, nil
			}
			chatMode = normalizeChatMode(info.ChatMode)
			if err := b.rememberChatMode(ctx, chatID, chatMode); err != nil {
				return 0, err
			}
		}
		isP2P = chatMode == "p2p"
	}
	return chatID, nil
}

func (b *Bot) controlRoomUserOpenID(ctx context.Context, chatID int64, openChatID string) (string, error) {
	if strings.HasPrefix(openChatID, "p2p:") {
		return strings.TrimSpace(strings.TrimPrefix(openChatID, "p2p:")), nil
	}
	openUserID, err := b.store.GetState(ctx, feishuControlUserKey(chatID))
	if err != nil {
		return "", err
	}
	openUserID = strings.TrimSpace(openUserID)
	if openUserID == "" {
		return "", fmt.Errorf("feishu p2p operator is not known for chat %d; send any message to the bot first", chatID)
	}
	return openUserID, nil
}

func (b *Bot) ensureControlRoom(ctx context.Context, p2pChatID int64, openUserID string) (int64, error) {
	openUserID = strings.TrimSpace(openUserID)
	if openUserID == "" {
		return 0, errors.New("feishu control room user open_id is empty")
	}
	key := feishuControlRoomKey(p2pChatID)
	if existing, err := b.store.GetState(ctx, key); err != nil {
		return 0, err
	} else if strings.TrimSpace(existing) != "" {
		openChatID := strings.TrimSpace(existing)
		roomChatID, resolveErr := b.store.ResolveExternalID(ctx, namespaceChat, openChatID)
		if resolveErr != nil {
			return 0, resolveErr
		}
		if err := b.rememberControlRoomSource(ctx, openChatID, p2pChatID); err != nil {
			return 0, err
		}
		b.ensureWorkspaceMenu(ctx, openChatID)
		return roomChatID, nil
	}
	createCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	openChatID, err := b.api.CreateThreadRoom(createCtx, feishuWorkspaceRoomName, []string{openUserID}, b.cfg.FeishuAppID, feishuControlRoomUUID(openUserID))
	cancel()
	if err != nil {
		return 0, err
	}
	openChatID = strings.TrimSpace(openChatID)
	if openChatID == "" {
		return 0, errors.New("feishu create control room did not return chat id")
	}
	roomChatID, err := b.store.ResolveExternalID(ctx, namespaceChat, openChatID)
	if err != nil {
		return 0, err
	}
	if err := b.store.SetState(ctx, key, openChatID); err != nil {
		return 0, err
	}
	if err := b.rememberControlRoomSource(ctx, openChatID, p2pChatID); err != nil {
		return 0, err
	}
	b.ensureWorkspaceMenu(ctx, openChatID)
	return roomChatID, nil
}

func (b *Bot) ensureWorkspaceMenu(ctx context.Context, openChatID string) {
	openChatID = strings.TrimSpace(openChatID)
	if openChatID == "" || b.api == nil {
		return
	}
	key := feishuWorkspaceMenuKey(openChatID)
	if installed, err := b.store.GetState(ctx, key); err == nil && strings.TrimSpace(installed) != "" {
		return
	}
	menuCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err := b.api.CreateWorkspaceMenu(menuCtx, openChatID)
	cancel()
	if err != nil {
		if b.logger != nil {
			b.logger.Printf("feishu workspace menu setup skipped: chat=%s err=%v", shortHashForLog(openChatID), err)
		}
		return
	}
	_ = b.store.SetState(ctx, key, "1")
}

func (b *Bot) rememberChatMode(ctx context.Context, chatID int64, mode string) error {
	mode = normalizeChatMode(mode)
	if chatID == 0 || mode == "" {
		return nil
	}
	return b.store.SetState(ctx, feishuChatModeKey(chatID), mode)
}

func (b *Bot) chatMode(ctx context.Context, chatID int64) (string, error) {
	mode, err := b.store.GetState(ctx, feishuChatModeKey(chatID))
	if err != nil {
		return "", err
	}
	return normalizeChatMode(mode), nil
}

func (b *Bot) rememberControlRoomSource(ctx context.Context, roomOpenChatID string, p2pChatID int64) error {
	sourceOpenChatID, err := b.store.ExternalIDForNumeric(ctx, namespaceChat, p2pChatID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(roomOpenChatID) == "" || strings.TrimSpace(sourceOpenChatID) == "" {
		return nil
	}
	value := fmt.Sprintf("%d|%s", p2pChatID, strings.TrimSpace(sourceOpenChatID))
	return b.store.SetState(ctx, feishuControlRoomSourceKey(roomOpenChatID), value)
}

func (b *Bot) sendText(ctx context.Context, chatID int64, text string) (int64, error) {
	content, err := encodeTextContent(text)
	if err != nil {
		return 0, err
	}
	return b.send(ctx, chatID, "text", content)
}

func (b *Bot) sendMarkdown(ctx context.Context, chatID int64, text string) (int64, error) {
	return b.sendMarkdownWithOptions(ctx, chatID, text, model.SendOptions{})
}

func (b *Bot) sendMarkdownWithOptions(ctx context.Context, chatID int64, text string, options model.SendOptions) (int64, error) {
	return b.sendCardWithOptions(ctx, chatID, text, nil, options)
}

func (b *Bot) sendCard(ctx context.Context, chatID int64, text string, buttons [][]model.ButtonSpec) (int64, error) {
	return b.sendCardWithOptions(ctx, chatID, text, buttons, model.SendOptions{})
}

func (b *Bot) sendCardWithOptions(ctx context.Context, chatID int64, text string, buttons [][]model.ButtonSpec, options model.SendOptions) (int64, error) {
	card, err := buildCard(text, buttons)
	if err != nil {
		content, contentErr := encodeTextContent(text)
		if contentErr != nil {
			return 0, errors.Join(err, contentErr)
		}
		return b.sendWithOptions(ctx, chatID, "text", content, options)
	}
	return b.sendWithOptions(ctx, chatID, "interactive", card, options)
}

func (b *Bot) sendRenderedCard(ctx context.Context, chatID int64, message model.RenderedMessage, buttons [][]model.ButtonSpec) (int64, error) {
	return b.sendRenderedCardWithOptions(ctx, chatID, message, buttons, model.SendOptions{})
}

func (b *Bot) sendRenderedCardWithOptions(ctx context.Context, chatID int64, message model.RenderedMessage, buttons [][]model.ButtonSpec, options model.SendOptions) (int64, error) {
	message = b.withUploadedRenderedImage(ctx, message)
	card, err := buildRenderedCard(message, buttons)
	if err != nil {
		text := renderPlainText(message)
		if text == "" {
			text = " "
		}
		content, contentErr := encodeTextContent(text)
		if contentErr != nil {
			return 0, errors.Join(err, contentErr)
		}
		return b.sendWithOptions(ctx, chatID, "text", content, options)
	}
	return b.sendWithOptions(ctx, chatID, "interactive", card, options)
}

func (b *Bot) withUploadedRenderedImage(ctx context.Context, message model.RenderedMessage) model.RenderedMessage {
	if strings.TrimSpace(message.ImageKey) != "" || strings.TrimSpace(message.ImagePath) == "" || b == nil || b.api == nil {
		return message
	}
	data, err := os.ReadFile(strings.TrimSpace(message.ImagePath))
	if err != nil {
		if b.logger != nil {
			b.logger.Printf("feishu read rendered image failed: %v", err)
		}
		return message
	}
	uploadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	imageKey, err := b.api.UploadImage(uploadCtx, data)
	cancel()
	if err != nil {
		if b.logger != nil {
			b.logger.Printf("feishu upload rendered image failed: %v", err)
		}
		return message
	}
	message.ImageKey = strings.TrimSpace(imageKey)
	return message
}

func (b *Bot) sendSectionedCard(ctx context.Context, chatID int64, sections []model.MessageSection) (int64, error) {
	return b.sendSectionedCardWithOptions(ctx, chatID, sections, model.SendOptions{})
}

func (b *Bot) sendSectionedCardWithOptions(ctx context.Context, chatID int64, sections []model.MessageSection, options model.SendOptions) (int64, error) {
	card, err := buildSectionedCard(sections)
	if err != nil {
		return 0, err
	}
	messageID, err := b.sendWithOptions(ctx, chatID, "interactive", card, options)
	if err == nil || !sectionedCardHasRowsOrCharts(sections) || sectionedCardHasCharts(sections) {
		return messageID, err
	}
	if isFeishuAuthError(err) || isFeishuAuthRecoveryError(err) {
		return 0, err
	}
	b.logger.Printf("feishu json 2.0 card send failed, falling back to legacy card: %v", err)
	fallbackCard, fallbackErr := buildSectionedCardV1(sections)
	if fallbackErr != nil {
		return 0, errors.Join(err, fallbackErr)
	}
	return b.sendWithOptions(ctx, chatID, "interactive", fallbackCard, options)
}

func (b *Bot) send(ctx context.Context, chatID int64, msgType, content string) (int64, error) {
	return b.sendWithOptions(ctx, chatID, msgType, content, model.SendOptions{})
}

func (b *Bot) sendWithOptions(ctx context.Context, chatID int64, msgType, content string, options model.SendOptions) (int64, error) {
	openChatID, err := b.store.ExternalIDForNumeric(ctx, namespaceChat, chatID)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(openChatID) == "" {
		return 0, fmt.Errorf("feishu chat mapping not found: %d", chatID)
	}
	var sent sentMessage
	if options.FeishuReplyToMessageID != 0 {
		var root *model.FeishuMessageMap
		root, err = b.store.GetFeishuMessageByNumericID(ctx, options.FeishuReplyToMessageID)
		if err != nil {
			return 0, err
		}
		if root == nil || strings.TrimSpace(root.OpenMessageID) == "" {
			return 0, fmt.Errorf("feishu reply root not found: %d", options.FeishuReplyToMessageID)
		}
		sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		sent, err = b.withRecoveredAPIClient(sendCtx, func(callCtx context.Context, api apiClient) (sentMessage, error) {
			return api.Reply(callCtx, root.OpenMessageID, msgType, content, options.FeishuReplyInThread)
		})
		cancel()
	} else {
		sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		sent, err = b.withRecoveredAPIClient(sendCtx, func(callCtx context.Context, api apiClient) (sentMessage, error) {
			return api.Send(callCtx, openChatID, msgType, content)
		})
		cancel()
	}
	if err != nil {
		if b.logger != nil {
			b.logger.Printf(
				"feishu send failed: chat_id=%d msg_type=%s reply_to=%d reply_in_thread=%t content_len=%d err=%v",
				chatID,
				msgType,
				options.FeishuReplyToMessageID,
				options.FeishuReplyInThread,
				len(content),
				err,
			)
		}
		return 0, err
	}
	openMessageID := strings.TrimSpace(sent.OpenMessageID)
	if openMessageID == "" {
		return 0, nil
	}
	messageID, err := b.store.ResolveExternalID(ctx, namespaceMessage, openMessageID)
	if err != nil {
		return 0, err
	}
	mappedOpenChatID := strings.TrimSpace(sent.OpenChatID)
	if mappedOpenChatID == "" {
		mappedOpenChatID = openChatID
	}
	if err := b.store.PutFeishuMessageMap(ctx, messageID, openMessageID, chatID, mappedOpenChatID); err != nil {
		return 0, err
	}
	if options.FeishuReplyToMessageID != 0 && strings.TrimSpace(options.FeishuCodexThreadID) != "" {
		root, err := b.store.GetFeishuMessageByNumericID(ctx, options.FeishuReplyToMessageID)
		if err != nil {
			return 0, err
		}
		if root != nil {
			feishuThreadID := strings.TrimSpace(sent.ThreadID)
			if feishuThreadID == "" {
				existing, _ := b.store.GetFeishuThreadTopicByCodexThread(ctx, chatID, options.FeishuCodexThreadID)
				if existing != nil {
					feishuThreadID = existing.FeishuThreadID
				}
			}
			_ = b.store.UpsertFeishuThreadTopic(ctx, model.FeishuThreadTopic{
				ChatID:            chatID,
				OpenChatID:        firstNonEmptyString(root.OpenChatID, openChatID, mappedOpenChatID),
				ThreadID:          options.FeishuCodexThreadID,
				RootMessageID:     options.FeishuReplyToMessageID,
				RootOpenMessageID: root.OpenMessageID,
				FeishuThreadID:    feishuThreadID,
			})
		}
	}
	return messageID, nil
}

func (b *Bot) handleMessageEvent(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil {
		return nil
	}
	message := event.Event.Message
	openChatID := value(message.ChatId)
	openMessageID := value(message.MessageId)
	openUserID := senderOpenID(event.Event.Sender)
	text := parseTextContent(value(message.MessageType), value(message.Content))
	if text == "" {
		var err error
		text, err = b.imageMessageText(ctx, message)
		if err != nil {
			return err
		}
	}
	if openChatID == "" || openMessageID == "" || openUserID == "" || text == "" {
		b.logIgnoredInboundMessage(message, openChatID, openMessageID, openUserID, text)
		return nil
	}
	chatID, err := b.store.ResolveExternalID(ctx, namespaceChat, openChatID)
	if err != nil {
		return err
	}
	messageID, err := b.store.ResolveExternalID(ctx, namespaceMessage, openMessageID)
	if err != nil {
		return err
	}
	userID, err := b.store.ResolveExternalID(ctx, namespaceUser, openUserID)
	if err != nil {
		return err
	}
	if err := b.store.PutFeishuMessageMap(ctx, messageID, openMessageID, chatID, openChatID); err != nil {
		return err
	}
	if err := b.store.SetState(ctx, feishuControlUserKey(chatID), openUserID); err != nil {
		return err
	}
	if err := b.rememberChatMode(ctx, chatID, value(message.ChatType)); err != nil {
		return err
	}
	replyTo, _ := b.replyToMessageID(ctx, message)
	topicID := replyTo
	b.logInboundMessage(message, chatID, messageID, userID, replyTo, text)
	claimKey := feishuInboundClaimKey(openMessageID)
	claimed, err := b.store.SetStateIfAbsent(ctx, claimKey, fmt.Sprintf("chat=%d message=%d", chatID, messageID))
	if err != nil {
		return err
	}
	if !claimed {
		b.logDuplicateInboundMessage(message, chatID, messageID)
		return nil
	}
	if !b.isAllowed(openUserID, openChatID, userID, chatID) {
		return b.sendFailureMessage(ctx, chatID, b.t(ctx, "当前飞书用户或聊天无权控制 codex-feishu。", "This Feishu user or chat is not allowed to control codex-feishu."))
	}
	if replyTo == 0 && isFeishuGroupMessage(message) && !isFeishuCommand(text) {
		return nil
	}
	response, err := b.service.HandleMessageFromSource(ctx, chatID, topicID, userID, text, replyTo, model.PanelSourceFeishuInput)
	if err != nil {
		_ = b.store.DeleteState(ctx, claimKey)
		return b.sendFailureMessage(ctx, chatID, b.t(ctx, "本地 Go bridge 处理请求失败。请试试 /repair 或 /status。", "Request failed inside the local Go bridge. Try /repair or /status."))
	}
	return b.deliverDirectResponse(ctx, chatID, response)
}

func (b *Bot) imageMessageText(ctx context.Context, message *larkim.EventMessage) (string, error) {
	if b == nil || message == nil || b.api == nil {
		return "", nil
	}
	request, imageKey := parsePostContent(value(message.MessageType), value(message.Content))
	if imageKey == "" {
		imageKey = parseImageKeyContent(value(message.MessageType), value(message.Content))
	}
	if imageKey == "" {
		return "", nil
	}
	downloadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	data, err := b.api.DownloadImage(downloadCtx, value(message.MessageId), imageKey)
	cancel()
	if err != nil {
		return "", err
	}
	path, err := b.saveInboundImage(value(message.MessageId), imageKey, data)
	if err != nil {
		return "", err
	}
	return codexAttachmentPrompt(path, request), nil
}

func codexAttachmentPrompt(path, request string) string {
	path = strings.TrimSpace(path)
	request = strings.TrimSpace(request)
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) {
		name = "image"
	}
	return fmt.Sprintf("\n# Files mentioned by the user:\n\n## %s: %s\n\n## My request for Codex:\n%s\n<image name=[Image #1] path=\"%s\">\n</image>", name, path, request, path)
}

func (b *Bot) saveInboundImage(openMessageID, imageKey string, data []byte) (string, error) {
	if b == nil {
		return "", errors.New("feishu bot is nil")
	}
	root := filepath.Join(b.cfg.Paths.DataDir, "feishu-attachments")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	name := shortHashForLog(firstNonEmptyString(openMessageID, imageKey))
	if name == "" {
		name = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	path := filepath.Join(root, name+".jpg")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (b *Bot) handleBotMenu(ctx context.Context, event *larkapplication.P2BotMenuV6) error {
	if event == nil || event.Event == nil || event.Event.Operator == nil || event.Event.Operator.OperatorId == nil {
		return nil
	}
	openUserID := value(event.Event.Operator.OperatorId.OpenId)
	command, ok := botMenuCommand(value(event.Event.EventKey))
	if openUserID == "" || !ok {
		return nil
	}
	userID, err := b.store.ResolveExternalID(ctx, namespaceUser, openUserID)
	if err != nil {
		return err
	}
	chatID, err := b.store.ResolveExternalID(ctx, namespaceChat, "p2p:"+openUserID)
	if err != nil {
		return err
	}
	if err := b.store.SetState(ctx, feishuControlUserKey(chatID), openUserID); err != nil {
		return err
	}
	if err := b.rememberChatMode(ctx, chatID, "p2p"); err != nil {
		return err
	}
	if !b.isBotMenuAllowed(openUserID, userID) {
		return b.sendOpenIDFailureMessage(ctx, openUserID, b.t(ctx, "当前飞书用户无权控制 codex-feishu。", "This Feishu user is not allowed to control codex-feishu."))
	}
	response, err := b.service.HandleMessageFromSource(ctx, chatID, 0, userID, command, 0, model.PanelSourceFeishuInput)
	if err != nil {
		return b.sendOpenIDFailureMessage(ctx, openUserID, b.t(ctx, "本地 Go bridge 处理请求失败。请试试 /repair 或 /status。", "Request failed inside the local Go bridge. Try /repair or /status."))
	}
	return b.deliverOpenIDDirectResponse(ctx, openUserID, chatID, response)
}

func (b *Bot) handleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Context == nil || event.Event.Operator == nil {
		return nil, nil
	}
	openChatID := strings.TrimSpace(event.Event.Context.OpenChatID)
	openMessageID := strings.TrimSpace(event.Event.Context.OpenMessageID)
	openUserID := strings.TrimSpace(event.Event.Operator.OpenID)
	if openChatID == "" || openMessageID == "" || openUserID == "" {
		return nil, nil
	}
	chatID, err := b.store.ResolveExternalID(ctx, namespaceChat, openChatID)
	if err != nil {
		return nil, err
	}
	messageID, err := b.store.ResolveExternalID(ctx, namespaceMessage, openMessageID)
	if err != nil {
		return nil, err
	}
	userID, err := b.store.ResolveExternalID(ctx, namespaceUser, openUserID)
	if err != nil {
		return nil, err
	}
	if err := b.store.PutFeishuMessageMap(ctx, messageID, openMessageID, chatID, openChatID); err != nil {
		return nil, err
	}
	if err := b.store.SetState(ctx, feishuControlUserKey(chatID), openUserID); err != nil {
		return nil, err
	}
	if info, err := b.api.GetChat(ctx, openChatID); err == nil && normalizeChatMode(info.ChatMode) == "p2p" {
		if err := b.rememberChatMode(ctx, chatID, info.ChatMode); err != nil {
			return nil, err
		}
	}
	if !b.isAllowed(openUserID, openChatID, userID, chatID) {
		return toast(b.t(ctx, "无权操作。", "Not allowed.")), nil
	}
	data := ""
	actionPayload := map[string]any(nil)
	if event.Event.Action != nil {
		actionPayload = callbackPayloadFromAction(event.Event.Action)
		data = callbackDataFromValue(actionPayload)
	}
	topicID := b.callbackTopicID(ctx, chatID, messageID)
	response, err := b.service.HandleCallbackPayloadFromSource(ctx, chatID, topicID, messageID, userID, data, actionPayload, model.PanelSourceFeishuInput)
	if err != nil {
		_ = b.sendFailureMessage(ctx, chatID, b.t(ctx, "本地 Go bridge 处理请求失败。请试试 /repair 或 /status。", "Request failed inside the local Go bridge. Try /repair or /status."))
		return toast(b.t(ctx, "请求失败。", "Request failed.")), nil
	}
	if err := b.deliverDirectResponse(ctx, chatID, response); err != nil {
		return nil, err
	}
	if response != nil && response.SilentCallback {
		return nil, nil
	}
	if response != nil && strings.TrimSpace(response.CallbackText) != "" {
		return toast(response.CallbackText), nil
	}
	return toast(b.t(ctx, "完成。", "Done.")), nil
}

func (b *Bot) replyToMessageID(ctx context.Context, message *larkim.EventMessage) (int64, error) {
	openChatID := value(message.ChatId)
	if topic, err := b.feishuThreadTopicFromMessage(ctx, openChatID, message); err != nil {
		return 0, err
	} else if topic != nil && topic.RootMessageID != 0 {
		return topic.RootMessageID, nil
	}
	for _, candidate := range []string{value(message.ParentId), value(message.RootId)} {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		mapped, err := b.store.GetFeishuMessageByOpenID(ctx, candidate)
		if err != nil {
			return 0, err
		}
		if mapped != nil {
			return mapped.MessageID, nil
		}
		id, err := b.store.ResolveExternalID(ctx, namespaceMessage, candidate)
		if err != nil {
			return 0, err
		}
		return id, nil
	}
	return 0, nil
}

func (b *Bot) callbackTopicID(ctx context.Context, chatID, messageID int64) int64 {
	if messageID == 0 {
		return 0
	}
	for _, topicID := range []int64{0, messageID} {
		route, err := b.store.ResolveMessageRoute(ctx, chatID, topicID, messageID)
		if err == nil && route != nil && route.TopicID != 0 {
			return route.TopicID
		}
	}
	return 0
}

func (b *Bot) feishuThreadTopicFromMessage(ctx context.Context, openChatID string, message *larkim.EventMessage) (*model.FeishuThreadTopic, error) {
	if message == nil {
		return nil, nil
	}
	for _, candidate := range []struct {
		kind string
		id   string
	}{
		{"root", value(message.RootId)},
		{"thread", value(message.ThreadId)},
	} {
		if strings.TrimSpace(candidate.id) == "" {
			continue
		}
		var topic *model.FeishuThreadTopic
		var err error
		switch candidate.kind {
		case "root":
			topic, err = b.store.GetFeishuThreadTopicByRootOpenMessageID(ctx, openChatID, candidate.id)
		case "thread":
			topic, err = b.store.GetFeishuThreadTopicByFeishuThreadID(ctx, openChatID, candidate.id)
		}
		if err != nil || topic != nil {
			return topic, err
		}
	}
	return nil, nil
}

func (b *Bot) logInboundMessage(message *larkim.EventMessage, chatID, messageID, userID, replyTo int64, text string) {
	if b == nil || b.logger == nil || message == nil {
		return
	}
	b.logger.Printf(
		"feishu inbound: chat_id=%d message_id=%d user_id=%d reply_to=%d open_chat=%s open_message=%s parent=%s root=%s text_len=%d text_sha256=%s",
		chatID,
		messageID,
		userID,
		replyTo,
		safeIDForLog(value(message.ChatId)),
		safeIDForLog(value(message.MessageId)),
		safeIDForLog(value(message.ParentId)),
		safeIDForLog(value(message.RootId)),
		len([]rune(text)),
		shortHashForLog(text),
	)
}

func (b *Bot) logIgnoredInboundMessage(message *larkim.EventMessage, openChatID, openMessageID, openUserID, text string) {
	if b == nil || b.logger == nil || message == nil {
		return
	}
	b.logger.Printf(
		"feishu inbound ignored: open_chat_empty=%t open_message_empty=%t open_user_empty=%t text_empty=%t message_type=%s content_len=%d content_sha256=%s",
		strings.TrimSpace(openChatID) == "",
		strings.TrimSpace(openMessageID) == "",
		strings.TrimSpace(openUserID) == "",
		strings.TrimSpace(text) == "",
		strings.TrimSpace(value(message.MessageType)),
		len([]rune(value(message.Content))),
		shortHashForLog(value(message.Content)),
	)
}

func (b *Bot) logDuplicateInboundMessage(message *larkim.EventMessage, chatID, messageID int64) {
	if b == nil || b.logger == nil || message == nil {
		return
	}
	b.logger.Printf(
		"feishu inbound duplicate skipped: chat_id=%d message_id=%d open_message=%s",
		chatID,
		messageID,
		safeIDForLog(value(message.MessageId)),
	)
}

func isFeishuGroupMessage(message *larkim.EventMessage) bool {
	if message == nil {
		return false
	}
	switch normalizeChatMode(value(message.ChatType)) {
	case "group", "topic":
		return true
	default:
		return false
	}
}

func isFeishuCommand(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "/")
}

func renderThreadTopicRootText(thread model.Thread, snapshot *appserver.ThreadReadSnapshot, sourceMode string) string {
	_, _ = snapshot, sourceMode
	title := firstNonEmptyString(thread.Title, thread.LastPreview)
	if strings.TrimSpace(title) == "" {
		title = thread.ShortID()
	}
	lines := []string{title}
	lines = append(lines, "", "这个话题对应一个 Codex 会话；在这里直接回复会继续该会话。")
	return strings.Join(lines, "\n")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func shortIDForText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func feishuInboundClaimKey(openMessageID string) string {
	return "feishu.inbound." + shortHashForLog(openMessageID)
}

func feishuControlUserKey(chatID int64) string {
	return fmt.Sprintf("feishu.control_user.%d", chatID)
}

func feishuChatModeKey(chatID int64) string {
	return fmt.Sprintf("feishu.chat_mode.%d", chatID)
}

func feishuControlRoomKey(chatID int64) string {
	return fmt.Sprintf("feishu.control_room.%d", chatID)
}

func feishuControlRoomSourceKey(openChatID string) string {
	return "feishu.control_room_source." + shortHashForLog(openChatID)
}

func feishuWorkspaceMenuKey(openChatID string) string {
	return "feishu.workspace_menu." + shortHashForLog(openChatID)
}

func feishuThreadTopicActivatedKey(chatID int64, threadID string) string {
	return fmt.Sprintf("feishu.thread_topic_activated.%d.%s", chatID, shortHashForLog(threadID))
}

func feishuControlRoomUUID(openUserID string) string {
	return "codex-control-room-" + shortHashForLog(openUserID)
}

func normalizeChatMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "p2p":
		return "p2p"
	case "topic_group", "topic":
		return "topic"
	case "group":
		return "group"
	default:
		return ""
	}
}

func (b *Bot) deliverDirectResponse(ctx context.Context, chatID int64, response *daemon.DirectResponse) error {
	if response == nil || (strings.TrimSpace(response.Text) == "" && response.SettingsForm == nil) {
		return nil
	}
	messageID, err := b.sendDirectResponse(ctx, chatID, response)
	if err != nil {
		return err
	}
	return b.service.RegisterDirectDelivery(ctx, chatID, response.DeliveryTopicID, messageID, response)
}

func (b *Bot) deliverOpenIDDirectResponse(ctx context.Context, openUserID string, chatID int64, response *daemon.DirectResponse) error {
	if response == nil || (strings.TrimSpace(response.Text) == "" && response.SettingsForm == nil) {
		return nil
	}
	messageID, err := b.sendOpenIDDirectResponse(ctx, openUserID, response)
	if err != nil {
		return err
	}
	return b.service.RegisterDirectDelivery(ctx, chatID, 0, messageID, response)
}

func (b *Bot) sendDirectResponse(ctx context.Context, chatID int64, response *daemon.DirectResponse) (int64, error) {
	if response.SettingsForm != nil {
		card, err := buildSettingsFormCard(*response.SettingsForm)
		if err != nil {
			return 0, err
		}
		return b.sendWithOptions(ctx, chatID, "interactive", card, response.Options)
	}
	if len(response.Sections) == 0 {
		options := response.Options
		options.Silent = true
		return b.SendMessage(ctx, chatID, response.DeliveryTopicID, response.Text, response.Buttons, options)
	}
	return b.sendSectionedCardWithOptions(ctx, chatID, response.Sections, response.Options)
}

func (b *Bot) sendOpenIDDirectResponse(ctx context.Context, openUserID string, response *daemon.DirectResponse) (int64, error) {
	if response.SettingsForm != nil {
		card, err := buildSettingsFormCard(*response.SettingsForm)
		if err != nil {
			return 0, err
		}
		return b.sendOpenIDInteractive(ctx, openUserID, card)
	}
	if len(response.Sections) == 0 {
		return b.sendOpenIDMessage(ctx, openUserID, response.Text, response.Buttons)
	}
	return b.sendOpenIDSectionedCard(ctx, openUserID, response.Sections)
}

func (b *Bot) sendFailureMessage(ctx context.Context, chatID int64, text string) error {
	if chatID == 0 {
		b.logger.Printf("feishu handler error without chat context: %s", text)
		return nil
	}
	_, err := b.SendMessage(ctx, chatID, 0, text, nil, model.SendOptions{Silent: true})
	return err
}

func (b *Bot) sendOpenIDFailureMessage(ctx context.Context, openUserID, text string) error {
	if strings.TrimSpace(openUserID) == "" {
		b.logger.Printf("feishu handler error without user context: %s", text)
		return nil
	}
	_, err := b.sendOpenIDMessage(ctx, openUserID, text, nil)
	return err
}

func (b *Bot) sendOpenIDMessage(ctx context.Context, openUserID, text string, buttons [][]model.ButtonSpec) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		text = " "
	}
	var msgType string
	var content string
	var err error
	if len(buttons) > 0 {
		msgType = "interactive"
		content, err = buildCard(text, buttons)
	} else {
		msgType = "interactive"
		content, err = buildCard(text, nil)
		if err != nil {
			msgType = "text"
			content, err = encodeTextContent(text)
		}
	}
	if err != nil {
		return 0, err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	sent, err := b.withRecoveredAPIClient(sendCtx, func(callCtx context.Context, api apiClient) (sentMessage, error) {
		return api.SendToOpenID(callCtx, openUserID, msgType, content)
	})
	cancel()
	if err != nil {
		return 0, err
	}
	openMessageID := strings.TrimSpace(sent.OpenMessageID)
	if openMessageID == "" {
		return 0, nil
	}
	messageID, err := b.store.ResolveExternalID(ctx, namespaceMessage, openMessageID)
	if err != nil {
		return 0, err
	}
	openChatID := strings.TrimSpace(sent.OpenChatID)
	if openChatID != "" {
		chatID, err := b.store.ResolveExternalID(ctx, namespaceChat, openChatID)
		if err != nil {
			return 0, err
		}
		if err := b.store.PutFeishuMessageMap(ctx, messageID, openMessageID, chatID, openChatID); err != nil {
			return 0, err
		}
	}
	return messageID, nil
}

func (b *Bot) sendOpenIDSectionedCard(ctx context.Context, openUserID string, sections []model.MessageSection) (int64, error) {
	content, err := buildSectionedCard(sections)
	if err != nil {
		return 0, err
	}
	return b.sendOpenIDInteractive(ctx, openUserID, content)
}

func (b *Bot) sendOpenIDInteractive(ctx context.Context, openUserID, content string) (int64, error) {
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	sent, err := b.withRecoveredAPIClient(sendCtx, func(callCtx context.Context, api apiClient) (sentMessage, error) {
		return api.SendToOpenID(callCtx, openUserID, "interactive", content)
	})
	cancel()
	if err != nil {
		return 0, err
	}
	openMessageID := strings.TrimSpace(sent.OpenMessageID)
	if openMessageID == "" {
		return 0, nil
	}
	messageID, err := b.store.ResolveExternalID(ctx, namespaceMessage, openMessageID)
	if err != nil {
		return 0, err
	}
	mappedOpenChatID := strings.TrimSpace(sent.OpenChatID)
	if mappedOpenChatID == "" {
		mappedOpenChatID = "p2p:" + strings.TrimSpace(openUserID)
	}
	chatID, err := b.store.ResolveExternalID(ctx, namespaceChat, mappedOpenChatID)
	if err != nil {
		return 0, err
	}
	if err := b.store.PutFeishuMessageMap(ctx, messageID, openMessageID, chatID, mappedOpenChatID); err != nil {
		return 0, err
	}
	return messageID, nil
}

func (b *Bot) isAllowed(openUserID, openChatID string, userID, chatID int64) bool {
	if b.isAllowedDirect(openUserID, openChatID, userID, chatID) {
		return true
	}
	sourceChatID, sourceOpenChatID, err := b.controlRoomSource(context.Background(), openChatID)
	if err != nil || sourceChatID == 0 || strings.TrimSpace(sourceOpenChatID) == "" {
		return false
	}
	return b.isAllowedDirect(openUserID, sourceOpenChatID, userID, sourceChatID)
}

func (b *Bot) isAllowedDirect(openUserID, openChatID string, userID, chatID int64) bool {
	if len(b.cfg.FeishuAllowedOpenIDs) > 0 && !containsString(b.cfg.FeishuAllowedOpenIDs, openUserID) {
		return false
	}
	if len(b.cfg.FeishuAllowedChatIDs) > 0 && !containsString(b.cfg.FeishuAllowedChatIDs, openChatID) {
		return false
	}
	return true
}

func (b *Bot) controlRoomSource(ctx context.Context, roomOpenChatID string) (int64, string, error) {
	raw, err := b.store.GetState(ctx, feishuControlRoomSourceKey(roomOpenChatID))
	if err != nil {
		return 0, "", err
	}
	parts := strings.SplitN(strings.TrimSpace(raw), "|", 2)
	if len(parts) != 2 {
		return 0, "", nil
	}
	chatID, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil {
		return 0, "", nil
	}
	return chatID, strings.TrimSpace(parts[1]), nil
}

func (b *Bot) isBotMenuAllowed(openUserID string, userID int64) bool {
	if len(b.cfg.FeishuAllowedOpenIDs) > 0 {
		return containsString(b.cfg.FeishuAllowedOpenIDs, openUserID)
	}
	return len(b.cfg.FeishuAllowedChatIDs) == 0
}

func botMenuCommand(eventKey string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(eventKey)) {
	case "start":
		return "/start", true
	case "help":
		return "/help", true
	case "status":
		return "/status", true
	case "chats":
		return "/chats", true
	case "projects":
		return "/projects", true
	case "new":
		return "/new", true
	case "setting", "settings":
		return "/setting", true
	case "repair":
		return "/repair", true
	default:
		return "", false
	}
}

func senderOpenID(sender *larkim.EventSender) string {
	if sender == nil || sender.SenderId == nil {
		return ""
	}
	return value(sender.SenderId.OpenId)
}

func toast(text string) *callback.CardActionTriggerResponse {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "Done."
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "info", Content: text},
	}
}

func callbackPayloadFromAction(action *callback.CallBackAction) map[string]any {
	if action == nil {
		return nil
	}
	payload := map[string]any{}
	for key, value := range action.Value {
		payload[key] = value
	}
	if len(action.FormValue) > 0 {
		formValue := map[string]any{}
		for key, value := range action.FormValue {
			formValue[key] = value
		}
		payload["form_value"] = formValue
	}
	return payload
}

func (b *Bot) t(ctx context.Context, zh, en string) string {
	return botText(b.botLanguage(ctx), zh, en)
}

func (b *Bot) botLanguage(ctx context.Context) string {
	if b == nil || b.store == nil {
		return "zh"
	}
	value, _ := b.store.GetState(ctx, feishuBotLanguageKey)
	return normalizeBotLanguageForText(value)
}

func botText(lang, zh, en string) string {
	if normalizeBotLanguageForText(lang) == "en" {
		return en
	}
	return zh
}

func normalizeBotLanguageForText(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "en", "english", "en-us", "en_us":
		return "en"
	default:
		return "zh"
	}
}

func containsString(values []string, needle string) bool {
	needle = strings.TrimSpace(needle)
	for _, value := range values {
		if strings.TrimSpace(value) == needle {
			return true
		}
	}
	return false
}

func safeAppID(appID string) string {
	appID = strings.TrimSpace(appID)
	if len(appID) <= 8 {
		return appID
	}
	return appID[:4] + "..." + appID[len(appID)-4:]
}

func safeIDForLog(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return fmt.Sprintf("len:%d sha:%s", len(value), shortHashForLog(value))
}

func shortHashForLog(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}
