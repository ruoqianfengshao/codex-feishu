package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/mideco-tech/codex-tg/internal/config"
	"github.com/mideco-tech/codex-tg/internal/daemon"
	"github.com/mideco-tech/codex-tg/internal/model"
	"github.com/mideco-tech/codex-tg/internal/storage"
)

const (
	namespaceChat    = "feishu.chat"
	namespaceMessage = "feishu.message"
	namespaceUser    = "feishu.user"
)

type Bot struct {
	cfg     config.Config
	service *daemon.Service
	store   *storage.Store
	api     apiClient
	logger  *log.Logger
	ws      wsClient
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
		api:     newSDKAPIClient(cfg.FeishuAppID, cfg.FeishuAppSecret),
		logger:  logger,
	}
	bot.ws = bot.newWSClient()
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
	if b.ws == nil {
		return nil
	}
	if err := b.ws.Start(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func (b *Bot) String() string {
	return "feishu bot"
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
			id, err = b.sendCard(ctx, chatID, chunk, buttons)
		} else {
			id, err = b.sendMarkdown(ctx, chatID, chunk)
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
			id, err = b.sendRenderedCard(ctx, chatID, message, buttons)
		} else {
			id, err = b.sendRenderedCard(ctx, chatID, message, nil)
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
	mapped, err := b.store.GetFeishuMessageByNumericID(ctx, messageID)
	if err != nil {
		return err
	}
	if mapped == nil {
		return nil
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return b.api.Delete(deleteCtx, mapped.OpenMessageID)
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
		if _, err := b.sendMarkdown(ctx, chatID, strings.TrimSpace(caption)); err != nil {
			return 0, err
		}
	}
	content, err := encodeFileContent(fileKey)
	if err != nil {
		return 0, err
	}
	return b.send(ctx, chatID, "file", content)
}

func (b *Bot) sendText(ctx context.Context, chatID int64, text string) (int64, error) {
	content, err := encodeTextContent(text)
	if err != nil {
		return 0, err
	}
	return b.send(ctx, chatID, "text", content)
}

func (b *Bot) sendMarkdown(ctx context.Context, chatID int64, text string) (int64, error) {
	return b.sendCard(ctx, chatID, text, nil)
}

func (b *Bot) sendCard(ctx context.Context, chatID int64, text string, buttons [][]model.ButtonSpec) (int64, error) {
	card, err := buildCard(text, buttons)
	if err != nil {
		content, contentErr := encodeTextContent(text)
		if contentErr != nil {
			return 0, errors.Join(err, contentErr)
		}
		return b.send(ctx, chatID, "text", content)
	}
	return b.send(ctx, chatID, "interactive", card)
}

func (b *Bot) sendRenderedCard(ctx context.Context, chatID int64, message model.RenderedMessage, buttons [][]model.ButtonSpec) (int64, error) {
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
		return b.send(ctx, chatID, "text", content)
	}
	return b.send(ctx, chatID, "interactive", card)
}

func (b *Bot) sendSectionedCard(ctx context.Context, chatID int64, sections []model.MessageSection) (int64, error) {
	card, err := buildSectionedCard(sections)
	if err != nil {
		return 0, err
	}
	return b.send(ctx, chatID, "interactive", card)
}

func (b *Bot) send(ctx context.Context, chatID int64, msgType, content string) (int64, error) {
	openChatID, err := b.store.ExternalIDForNumeric(ctx, namespaceChat, chatID)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(openChatID) == "" {
		return 0, fmt.Errorf("feishu chat mapping not found: %d", chatID)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	sent, err := b.api.Send(sendCtx, openChatID, msgType, content)
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
		mappedOpenChatID = openChatID
	}
	if err := b.store.PutFeishuMessageMap(ctx, messageID, openMessageID, chatID, mappedOpenChatID); err != nil {
		return 0, err
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
	if openChatID == "" || openMessageID == "" || openUserID == "" || text == "" {
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
	replyTo, _ := b.replyToMessageID(ctx, message)
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
		return b.sendFailureMessage(ctx, chatID, "This Feishu user or chat is not allowed to control codex-tg.")
	}
	response, err := b.service.HandleMessageFromSource(ctx, chatID, 0, userID, text, replyTo, model.PanelSourceFeishuInput)
	if err != nil {
		_ = b.store.DeleteState(ctx, claimKey)
		return b.sendFailureMessage(ctx, chatID, "Request failed inside the local Go bridge. Try /repair or /status.")
	}
	return b.deliverDirectResponse(ctx, chatID, response)
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
	if !b.isBotMenuAllowed(openUserID, userID) {
		return b.sendOpenIDFailureMessage(ctx, openUserID, "This Feishu user is not allowed to control codex-tg.")
	}
	response, err := b.service.HandleMessageFromSource(ctx, chatID, 0, userID, command, 0, model.PanelSourceFeishuInput)
	if err != nil {
		return b.sendOpenIDFailureMessage(ctx, openUserID, "Request failed inside the local Go bridge. Try /repair or /status.")
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
	if !b.isAllowed(openUserID, openChatID, userID, chatID) {
		return toast("Not allowed."), nil
	}
	data := ""
	if event.Event.Action != nil {
		data = callbackDataFromValue(event.Event.Action.Value)
	}
	response, err := b.service.HandleCallbackFromSource(ctx, chatID, 0, messageID, userID, data, model.PanelSourceFeishuInput)
	if err != nil {
		_ = b.sendFailureMessage(ctx, chatID, "Request failed inside the local Go bridge. Try /repair or /status.")
		return toast("Request failed."), nil
	}
	if err := b.deliverDirectResponse(ctx, chatID, response); err != nil {
		return nil, err
	}
	if response != nil && strings.TrimSpace(response.CallbackText) != "" {
		return toast(response.CallbackText), nil
	}
	return toast("Done."), nil
}

func (b *Bot) replyToMessageID(ctx context.Context, message *larkim.EventMessage) (int64, error) {
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

func feishuInboundClaimKey(openMessageID string) string {
	return "feishu.inbound." + shortHashForLog(openMessageID)
}

func (b *Bot) deliverDirectResponse(ctx context.Context, chatID int64, response *daemon.DirectResponse) error {
	if response == nil || strings.TrimSpace(response.Text) == "" {
		return nil
	}
	messageID, err := b.sendDirectResponse(ctx, chatID, response)
	if err != nil {
		return err
	}
	return b.service.RegisterDirectDelivery(ctx, chatID, 0, messageID, response)
}

func (b *Bot) deliverOpenIDDirectResponse(ctx context.Context, openUserID string, chatID int64, response *daemon.DirectResponse) error {
	if response == nil || strings.TrimSpace(response.Text) == "" {
		return nil
	}
	messageID, err := b.sendOpenIDDirectResponse(ctx, openUserID, response)
	if err != nil {
		return err
	}
	return b.service.RegisterDirectDelivery(ctx, chatID, 0, messageID, response)
}

func (b *Bot) sendDirectResponse(ctx context.Context, chatID int64, response *daemon.DirectResponse) (int64, error) {
	if len(response.Sections) == 0 {
		return b.SendMessage(ctx, chatID, 0, response.Text, response.Buttons, model.SendOptions{Silent: true})
	}
	return b.sendSectionedCard(ctx, chatID, response.Sections)
}

func (b *Bot) sendOpenIDDirectResponse(ctx context.Context, openUserID string, response *daemon.DirectResponse) (int64, error) {
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
	sent, err := b.api.SendToOpenID(sendCtx, openUserID, msgType, content)
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
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	sent, err := b.api.SendToOpenID(sendCtx, openUserID, "interactive", content)
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
	if len(b.cfg.FeishuAllowedOpenIDs) > 0 && !containsString(b.cfg.FeishuAllowedOpenIDs, openUserID) {
		return false
	}
	if len(b.cfg.FeishuAllowedChatIDs) > 0 && !containsString(b.cfg.FeishuAllowedChatIDs, openChatID) {
		return false
	}
	if len(b.cfg.AllowedUserIDs) > 0 && !containsInt64(b.cfg.AllowedUserIDs, userID) {
		return false
	}
	if len(b.cfg.AllowedChatIDs) > 0 && !containsInt64(b.cfg.AllowedChatIDs, chatID) {
		return false
	}
	return true
}

func (b *Bot) isBotMenuAllowed(openUserID string, userID int64) bool {
	if len(b.cfg.FeishuAllowedOpenIDs) > 0 {
		return containsString(b.cfg.FeishuAllowedOpenIDs, openUserID)
	}
	if len(b.cfg.AllowedUserIDs) > 0 {
		return containsInt64(b.cfg.AllowedUserIDs, userID)
	}
	return len(b.cfg.FeishuAllowedChatIDs) == 0 && len(b.cfg.AllowedChatIDs) == 0
}

func botMenuCommand(eventKey string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(eventKey)) {
	case "start":
		return "/start", true
	case "help":
		return "/help", true
	case "status":
		return "/status", true
	case "threads":
		return "/threads", true
	case "projects":
		return "/projects", true
	case "newchat":
		return "/newchat", true
	case "newthread":
		return "/newthread", true
	case "settings":
		return "/settings", true
	case "model":
		return "/model", true
	case "effort":
		return "/effort", true
	case "context":
		return "/context", true
	case "observe_all":
		return "/observe all", true
	case "observe_off":
		return "/observe off", true
	case "panelmode":
		return "/panelmode", true
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

func containsString(values []string, needle string) bool {
	needle = strings.TrimSpace(needle)
	for _, value := range values {
		if strings.TrimSpace(value) == needle {
			return true
		}
	}
	return false
}

func containsInt64(values []int64, needle int64) bool {
	for _, value := range values {
		if value == needle {
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
