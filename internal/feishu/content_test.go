package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/mideco-tech/codex-tg/internal/config"
	"github.com/mideco-tech/codex-tg/internal/daemon"
	"github.com/mideco-tech/codex-tg/internal/model"
	"github.com/mideco-tech/codex-tg/internal/storage"
)

func TestEncodeAndParseTextContent(t *testing.T) {
	t.Parallel()

	content, err := encodeTextContent("hello")
	if err != nil {
		t.Fatalf("encodeTextContent failed: %v", err)
	}
	if got := parseTextContent("text", content); got != "hello" {
		t.Fatalf("parseTextContent = %q, want hello", got)
	}
	if got := parseTextContent("post", content); got != "" {
		t.Fatalf("parseTextContent(non-text) = %q, want empty", got)
	}
}

func TestBuildCardIncludesButtonCallbackData(t *testing.T) {
	t.Parallel()

	card, err := buildCard("Status", [][]model.ButtonSpec{{
		{Text: "Details", CallbackData: "cb_123"},
	}})
	if err != nil {
		t.Fatalf("buildCard failed: %v", err)
	}
	if !strings.Contains(card, "Status") || !strings.Contains(card, "cb_123") {
		t.Fatalf("card = %s", card)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
}

func TestBuildCardGroupsButtonsThreePerRow(t *testing.T) {
	t.Parallel()

	card, err := buildCard("Status", [][]model.ButtonSpec{{
		{Text: "One", CallbackData: "cb_1"},
		{Text: "Two", CallbackData: "cb_2"},
		{Text: "Three", CallbackData: "cb_3"},
	}})
	if err != nil {
		t.Fatalf("buildCard failed: %v", err)
	}
	var decoded struct {
		Elements []struct {
			Tag     string `json:"tag"`
			Layout  string `json:"layout"`
			Actions []any  `json:"actions"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
	var actionLayouts []string
	for _, element := range decoded.Elements {
		if element.Tag == "action" {
			actionLayouts = append(actionLayouts, fmt.Sprintf("%s:%d", element.Layout, len(element.Actions)))
		}
	}
	if got := fmt.Sprint(actionLayouts); got != "[trisection:3]" {
		t.Fatalf("action layouts = %s, want [trisection:3]", got)
	}
}

func TestBuildCardTranslatesCommonButtonLabels(t *testing.T) {
	t.Parallel()

	card, err := buildCard("Status", [][]model.ButtonSpec{{
		{Text: "Stop", CallbackData: "cb_stop"},
		{Text: "Steer", CallbackData: "cb_steer"},
		{Text: "Bind here", CallbackData: "cb_bind"},
	}})
	if err != nil {
		t.Fatalf("buildCard failed: %v", err)
	}
	for _, label := range []string{"停止", "追加", "绑定"} {
		if !strings.Contains(card, label) {
			t.Fatalf("card = %s, want translated label %q", card, label)
		}
	}
	for _, label := range []string{"Stop", "Steer", "Bind here"} {
		if strings.Contains(card, label) {
			t.Fatalf("card = %s, want no English label %q", card, label)
		}
	}
}

func TestBuildSectionedCardKeepsButtonsAfterEachSection(t *testing.T) {
	t.Parallel()

	card, err := buildSectionedCard([]model.MessageSection{
		{Text: "All chats"},
		{Text: "1. First", Buttons: [][]model.ButtonSpec{{{Text: "Open 1", CallbackData: "cb_1"}}}},
		{Text: "2. Second", Buttons: [][]model.ButtonSpec{{{Text: "Open 2", CallbackData: "cb_2"}}}},
	})
	if err != nil {
		t.Fatalf("buildSectionedCard failed: %v", err)
	}
	var decoded struct {
		Elements []struct {
			Tag     string `json:"tag"`
			Content string `json:"content"`
			Actions []any  `json:"actions"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
	if len(decoded.Elements) != 5 {
		t.Fatalf("elements = %#v, want markdown/action pairs after header", decoded.Elements)
	}
	if decoded.Elements[1].Content != "1. First" || decoded.Elements[2].Tag != "action" {
		t.Fatalf("first thread elements = %#v, want markdown then action", decoded.Elements[1:3])
	}
	if decoded.Elements[3].Content != "2. Second" || decoded.Elements[4].Tag != "action" {
		t.Fatalf("second thread elements = %#v, want markdown then action", decoded.Elements[3:5])
	}
}

func TestCallbackDataFromValue(t *testing.T) {
	t.Parallel()

	got := callbackDataFromValue(map[string]interface{}{"callback_data": " route-token "})
	if got != "route-token" {
		t.Fatalf("callbackDataFromValue = %q, want route-token", got)
	}
}

func TestBotMenuCommandMapsStableEventKeys(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"help":        "/help",
		"STATUS":      "/status",
		"observe_all": "/observe all",
		"observe_off": "/observe off",
	}
	for key, want := range tests {
		got, ok := botMenuCommand(key)
		if !ok {
			t.Fatalf("botMenuCommand(%q) ok = false", key)
		}
		if got != want {
			t.Fatalf("botMenuCommand(%q) = %q, want %q", key, got, want)
		}
	}
	if got, ok := botMenuCommand("default"); ok || got != "" {
		t.Fatalf("botMenuCommand(default) = %q, %v; hidden fallback must not be exposed", got, ok)
	}
}

func TestBotMenuAllowedRequiresUserScopedAllowlistWhenChatAllowlistConfigured(t *testing.T) {
	t.Parallel()

	bot := &Bot{cfg: config.Config{FeishuAllowedChatIDs: []string{"oc_allowed"}}}
	if bot.isBotMenuAllowed("ou_user", 42) {
		t.Fatal("bot menu event without chat id must not pass chat-only allowlist")
	}

	bot.cfg.FeishuAllowedOpenIDs = []string{"ou_user"}
	if !bot.isBotMenuAllowed("ou_user", 42) {
		t.Fatal("bot menu event should pass explicit Feishu open_id allowlist")
	}
}

func TestSendDocumentDataRejectsEmptyFileKey(t *testing.T) {
	t.Parallel()

	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bot := &Bot{
		store: store,
		api:   &fakeAPIClient{},
	}
	_, err = bot.SendDocumentData(context.Background(), 42, 0, "trace.log", []byte("hello"), "", model.SendOptions{})
	if err == nil {
		t.Fatal("SendDocumentData succeeded, want empty file key error")
	}
	if !strings.Contains(err.Error(), "file key") {
		t.Fatalf("SendDocumentData error = %v, want file key error", err)
	}
}

func TestSendRenderedMessagesUsesMarkdownCardWithoutButtons(t *testing.T) {
	t.Parallel()

	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	chatID, err := store.ResolveExternalID(context.Background(), namespaceChat, "oc_chat")
	if err != nil {
		t.Fatalf("ResolveExternalID failed: %v", err)
	}
	api := &fakeAPIClient{}
	bot := &Bot{store: store, api: api}

	_, err = bot.SendRenderedMessages(context.Background(), chatID, 0, []model.RenderedMessage{{
		Text: "**Done**\n\n```go\nfmt.Println(\"ok\")\n```",
	}}, nil, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendRenderedMessages failed: %v", err)
	}
	if api.lastMsgType != "interactive" {
		t.Fatalf("msgType = %q, want interactive", api.lastMsgType)
	}
	for _, want := range []string{`"tag":"markdown"`, "**Done**", "```go"} {
		if !strings.Contains(api.lastContent, want) {
			t.Fatalf("content missing %q:\n%s", want, api.lastContent)
		}
	}
}

func TestSendRenderedMessagesUsesDesktopUserHeader(t *testing.T) {
	t.Parallel()

	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	chatID, err := store.ResolveExternalID(context.Background(), namespaceChat, "oc_chat")
	if err != nil {
		t.Fatalf("ResolveExternalID failed: %v", err)
	}
	api := &fakeAPIClient{}
	bot := &Bot{store: store, api: api}

	_, err = bot.SendRenderedMessages(context.Background(), chatID, 0, []model.RenderedMessage{{
		Text:  "Desktop prompt",
		Style: model.MessageStyleDesktopUser,
	}}, nil, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendRenderedMessages failed: %v", err)
	}
	if api.lastMsgType != "interactive" {
		t.Fatalf("msgType = %q, want interactive", api.lastMsgType)
	}
	for _, want := range []string{`"header"`, `"template":"blue"`, "来自 Codex 桌面端", "Desktop prompt"} {
		if !strings.Contains(api.lastContent, want) {
			t.Fatalf("content missing %q:\n%s", want, api.lastContent)
		}
	}
}

func TestSendOpenIDMessageUsesMarkdownCardWithoutButtons(t *testing.T) {
	t.Parallel()

	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	api := &fakeAPIClient{}
	bot := &Bot{store: store, api: api}

	_, err = bot.sendOpenIDMessage(context.Background(), "ou_user", "## Status\n- Ready", nil)
	if err != nil {
		t.Fatalf("sendOpenIDMessage failed: %v", err)
	}
	if api.lastMsgType != "interactive" {
		t.Fatalf("msgType = %q, want interactive", api.lastMsgType)
	}
	if !strings.Contains(api.lastContent, `"tag":"markdown"`) || !strings.Contains(api.lastContent, "## Status") {
		t.Fatalf("content is not markdown card:\n%s", api.lastContent)
	}
}

func TestEditMessageUsesMarkdownCardWithoutButtons(t *testing.T) {
	t.Parallel()

	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	messageID, err := store.ResolveExternalID(context.Background(), namespaceMessage, "om_message")
	if err != nil {
		t.Fatalf("ResolveExternalID(message) failed: %v", err)
	}
	chatID, err := store.ResolveExternalID(context.Background(), namespaceChat, "oc_chat")
	if err != nil {
		t.Fatalf("ResolveExternalID(chat) failed: %v", err)
	}
	if err := store.PutFeishuMessageMap(context.Background(), messageID, "om_message", chatID, "oc_chat"); err != nil {
		t.Fatalf("PutFeishuMessageMap failed: %v", err)
	}
	api := &fakeAPIClient{}
	bot := &Bot{store: store, api: api}

	err = bot.EditMessage(context.Background(), chatID, 0, messageID, "## Updated\n- **Done**", nil)
	if err != nil {
		t.Fatalf("EditMessage failed: %v", err)
	}
	if api.updateTextCalls != 0 {
		t.Fatalf("UpdateText calls = %d, want 0", api.updateTextCalls)
	}
	if api.patchCardCalls != 1 {
		t.Fatalf("PatchCard calls = %d, want 1", api.patchCardCalls)
	}
	if api.lastOpenMessageID != "om_message" {
		t.Fatalf("openMessageID = %q, want om_message", api.lastOpenMessageID)
	}
	if !strings.Contains(api.lastCard, `"tag":"markdown"`) || !strings.Contains(api.lastCard, "## Updated") {
		t.Fatalf("patched content is not markdown card:\n%s", api.lastCard)
	}
}

func TestHandleMessageEventDeduplicatesFeishuRetries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := daemon.New(config.Config{
		Paths: config.Paths{
			Home:    t.TempDir(),
			DataDir: t.TempDir(),
			LogDir:  t.TempDir(),
			DBPath:  t.TempDir() + "/service.sqlite",
		},
	})
	if err != nil {
		t.Fatalf("daemon.New failed: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	api := &fakeAPIClient{}
	bot := &Bot{store: store, service: service, api: api}
	event := newTextMessageEvent("oc_chat", "om_retry", "ou_user", "/status")

	if err := bot.handleMessageEvent(ctx, event); err != nil {
		t.Fatalf("handleMessageEvent(first) failed: %v", err)
	}
	if err := bot.handleMessageEvent(ctx, event); err != nil {
		t.Fatalf("handleMessageEvent(duplicate) failed: %v", err)
	}
	if api.sendCalls != 1 {
		t.Fatalf("Send calls = %d, want one response for duplicate Feishu event", api.sendCalls)
	}
	claim, err := store.GetState(ctx, feishuInboundClaimKey("om_retry"))
	if err != nil {
		t.Fatalf("GetState(claim) failed: %v", err)
	}
	if !strings.Contains(claim, "chat=") || !strings.Contains(claim, "message=") {
		t.Fatalf("claim state = %q, want recorded chat/message", claim)
	}
}

func newTextMessageEvent(openChatID, openMessageID, openUserID, text string) *larkim.P2MessageReceiveV1 {
	messageType := "text"
	content, err := encodeTextContent(text)
	if err != nil {
		panic(err)
	}
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: &openUserID},
			},
			Message: &larkim.EventMessage{
				ChatId:      &openChatID,
				MessageId:   &openMessageID,
				MessageType: &messageType,
				Content:     &content,
			},
		},
	}
}

type fakeAPIClient struct {
	lastMsgType       string
	lastContent       string
	lastOpenMessageID string
	lastText          string
	lastCard          string
	updateTextCalls   int
	patchCardCalls    int
	sendCalls         int
}

func (f *fakeAPIClient) Send(_ context.Context, _, msgType, content string) (sentMessage, error) {
	f.sendCalls++
	f.lastMsgType = msgType
	f.lastContent = content
	return sentMessage{OpenMessageID: "om_fake", OpenChatID: "oc_fake"}, nil
}

func (f *fakeAPIClient) SendToOpenID(_ context.Context, _, msgType, content string) (sentMessage, error) {
	f.sendCalls++
	f.lastMsgType = msgType
	f.lastContent = content
	return sentMessage{OpenMessageID: "om_fake", OpenChatID: "oc_fake"}, nil
}

func (f *fakeAPIClient) UpdateText(_ context.Context, openMessageID, text string) error {
	f.lastOpenMessageID = openMessageID
	f.lastText = text
	f.updateTextCalls++
	return nil
}

func (f *fakeAPIClient) PatchCard(_ context.Context, openMessageID, card string) error {
	f.lastOpenMessageID = openMessageID
	f.lastCard = card
	f.patchCardCalls++
	return nil
}

func (f *fakeAPIClient) Delete(context.Context, string) error {
	return nil
}

func (f *fakeAPIClient) UploadFile(context.Context, string, []byte) (string, error) {
	return "", nil
}
