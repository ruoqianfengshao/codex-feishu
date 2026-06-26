package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/mideco-tech/codex-tg/internal/appserver"
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
	}})
	if err != nil {
		t.Fatalf("buildCard failed: %v", err)
	}
	for _, label := range []string{"停止", "追加"} {
		if !strings.Contains(card, label) {
			t.Fatalf("card = %s, want translated label %q", card, label)
		}
	}
	for _, label := range []string{"Stop", "Steer"} {
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

func TestSendRenderedMessagesRepliesInFeishuThread(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	chatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_chat")
	if err != nil {
		t.Fatalf("ResolveExternalID(chat) failed: %v", err)
	}
	rootMessageID, err := store.ResolveExternalID(ctx, namespaceMessage, "om_root")
	if err != nil {
		t.Fatalf("ResolveExternalID(root message) failed: %v", err)
	}
	if err := store.PutFeishuMessageMap(ctx, rootMessageID, "om_root", chatID, "oc_chat"); err != nil {
		t.Fatalf("PutFeishuMessageMap(root) failed: %v", err)
	}
	api := &fakeAPIClient{}
	bot := &Bot{store: store, api: api}

	ids, err := bot.SendRenderedMessages(ctx, chatID, 0, []model.RenderedMessage{{Text: "reply body"}}, nil, model.SendOptions{
		FeishuReplyToMessageID: rootMessageID,
		FeishuReplyInThread:    true,
		FeishuCodexThreadID:    "thread-1",
	})
	if err != nil {
		t.Fatalf("SendRenderedMessages failed: %v", err)
	}
	if len(ids) != 1 || ids[0] == 0 {
		t.Fatalf("ids = %#v, want one id", ids)
	}
	if api.replyCalls != 1 || api.sendCalls != 0 {
		t.Fatalf("replyCalls=%d sendCalls=%d, want one reply and no send", api.replyCalls, api.sendCalls)
	}
	if api.lastOpenMessageID != "om_root" || !api.lastReplyInThread {
		t.Fatalf("reply target=%q inThread=%v, want om_root true", api.lastOpenMessageID, api.lastReplyInThread)
	}
	topic, err := store.GetFeishuThreadTopicByCodexThread(ctx, chatID, "thread-1")
	if err != nil {
		t.Fatalf("GetFeishuThreadTopicByCodexThread failed: %v", err)
	}
	if topic == nil || topic.RootMessageID != rootMessageID || topic.FeishuThreadID != "othread_fake" {
		t.Fatalf("topic = %#v, want root and feishu thread id", topic)
	}
}

func TestEnsureThreadTopicMaterializesExistingRootWithoutThreadID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	chatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_chat")
	if err != nil {
		t.Fatalf("ResolveExternalID(chat) failed: %v", err)
	}
	rootMessageID, err := store.ResolveExternalID(ctx, namespaceMessage, "om_root")
	if err != nil {
		t.Fatalf("ResolveExternalID(root message) failed: %v", err)
	}
	if err := store.PutFeishuMessageMap(ctx, rootMessageID, "om_root", chatID, "oc_chat"); err != nil {
		t.Fatalf("PutFeishuMessageMap(root) failed: %v", err)
	}
	if err := store.UpsertFeishuThreadTopic(ctx, model.FeishuThreadTopic{
		ChatID:            chatID,
		OpenChatID:        "oc_chat",
		ThreadID:          "thread-existing-root",
		RootMessageID:     rootMessageID,
		RootOpenMessageID: "om_root",
	}); err != nil {
		t.Fatalf("UpsertFeishuThreadTopic failed: %v", err)
	}
	api := &fakeAPIClient{}
	bot := &Bot{store: store, api: api}

	topic, err := bot.EnsureThreadTopic(ctx, chatID, model.Thread{ID: "thread-existing-root", Title: "Existing Root"}, nil, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("EnsureThreadTopic failed: %v", err)
	}
	if api.replyCalls != 1 || api.sendCalls != 0 {
		t.Fatalf("replyCalls=%d sendCalls=%d, want one reply and no send", api.replyCalls, api.sendCalls)
	}
	if api.lastOpenMessageID != "om_root" || !api.lastReplyInThread {
		t.Fatalf("reply target=%q inThread=%v, want om_root true", api.lastOpenMessageID, api.lastReplyInThread)
	}
	if topic == nil || topic.FeishuThreadID != "othread_fake" {
		t.Fatalf("topic = %#v, want materialized Feishu thread id", topic)
	}
}

func TestEnsureThreadTopicCreatesControlRoomForP2PChat(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p2pChatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_p2p")
	if err != nil {
		t.Fatalf("ResolveExternalID(p2p chat) failed: %v", err)
	}
	if err := store.SetState(ctx, feishuControlUserKey(p2pChatID), "ou_user"); err != nil {
		t.Fatalf("SetState(control user) failed: %v", err)
	}
	if err := store.SetState(ctx, feishuChatModeKey(p2pChatID), "p2p"); err != nil {
		t.Fatalf("SetState(chat mode) failed: %v", err)
	}
	api := &fakeAPIClient{}
	bot := &Bot{
		cfg:   config.Config{FeishuAppID: "cli_app"},
		store: store,
		api:   api,
	}

	topic, err := bot.EnsureThreadTopic(ctx, p2pChatID, model.Thread{ID: "thread-from-dm", Title: "From DM"}, nil, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("EnsureThreadTopic failed: %v", err)
	}
	if api.createThreadRoomCalls != 1 {
		t.Fatalf("CreateThreadRoom calls = %d, want 1", api.createThreadRoomCalls)
	}
	if api.lastRoomName != feishuControlRoomName {
		t.Fatalf("room name = %q, want %q", api.lastRoomName, feishuControlRoomName)
	}
	if got := fmt.Sprint(api.lastRoomUserOpenIDs); got != "[ou_user]" {
		t.Fatalf("room users = %s, want [ou_user]", got)
	}
	roomChatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_control_room")
	if err != nil {
		t.Fatalf("ResolveExternalID(control room) failed: %v", err)
	}
	if topic == nil || topic.ChatID != roomChatID || topic.OpenChatID != "oc_control_room" {
		t.Fatalf("topic = %#v, want control room chat", topic)
	}
	if api.lastReceiveID != "oc_control_room" {
		t.Fatalf("send receive id = %q, want oc_control_room", api.lastReceiveID)
	}
	if api.replyCalls != 1 {
		t.Fatalf("replyCalls = %d, want activation reply", api.replyCalls)
	}
	if api.lastOpenMessageID != "om_fake" || api.lastMsgType != "text" || !api.lastReplyInThread {
		t.Fatalf("activation reply target=%q type=%q inThread=%v, want om_fake text true", api.lastOpenMessageID, api.lastMsgType, api.lastReplyInThread)
	}
	if got := parseTextContent("text", api.lastContent); got != `<at user_id="ou_user">你</at> 已打开会话` {
		t.Fatalf("activation content = %q, want mention", got)
	}
	topic, err = bot.EnsureThreadTopic(ctx, p2pChatID, model.Thread{ID: "thread-from-dm", Title: "From DM"}, nil, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("EnsureThreadTopic second call failed: %v", err)
	}
	if topic == nil || topic.ChatID != roomChatID {
		t.Fatalf("second topic = %#v, want control room chat", topic)
	}
	if api.replyCalls != 1 {
		t.Fatalf("replyCalls after second call = %d, want no duplicate activation", api.replyCalls)
	}
	storedRoom, err := store.GetState(ctx, feishuControlRoomKey(p2pChatID))
	if err != nil {
		t.Fatalf("GetState(control room) failed: %v", err)
	}
	if storedRoom != "oc_control_room" {
		t.Fatalf("control room state = %q, want oc_control_room", storedRoom)
	}
}

func TestRenderThreadTopicRootTextUsesCompactTitleAndProject(t *testing.T) {
	t.Parallel()

	longTitle := "实现飞书版本的 codex-tg，要求体验丝滑，功能完善，而且标题不能被截断"
	got := renderThreadTopicRootText(model.Thread{
		ID:          "thread-title",
		Title:       longTitle,
		ProjectName: "codex-tg-controller",
		Status:      "active",
	}, &appserver.ThreadReadSnapshot{LatestTurnID: "turn-123456789", LatestTurnStatus: "inProgress"}, model.PanelSourceFeishuInput)

	if !strings.HasPrefix(got, longTitle+"\nProject: codex-tg-controller\n") {
		t.Fatalf("root text = %q, want compact title then project", got)
	}
	if strings.Contains(got, "## Codex 会话") || strings.Contains(got, "Thread:") {
		t.Fatalf("root text = %q, want no legacy heading/thread line", got)
	}
	for _, want := range []string{"Status: inProgress", "Source: Feishu", "Turn: turn-123"} {
		if !strings.Contains(got, want) {
			t.Fatalf("root text missing %q:\n%s", want, got)
		}
	}
}

func TestRenderThreadTopicRootTextFallsBackToCWDBasenameForProject(t *testing.T) {
	t.Parallel()

	got := renderThreadTopicRootText(model.Thread{
		ID:     "thread-cwd",
		Title:  "Use cwd project",
		CWD:    "/Users/example/workspace/codex-tg-controller",
		Status: "idle",
	}, nil, model.PanelSourceGlobalObserver)

	if !strings.Contains(got, "Project: codex-tg-controller") {
		t.Fatalf("root text = %q, want CWD basename project", got)
	}
}

func TestControlRoomInheritsAllowedSourceChat(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p2pChatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_p2p")
	if err != nil {
		t.Fatalf("ResolveExternalID(p2p chat) failed: %v", err)
	}
	bot := &Bot{
		cfg:   config.Config{FeishuAllowedChatIDs: []string{"oc_p2p"}},
		store: store,
	}
	if err := bot.rememberControlRoomSource(ctx, "oc_control_room", p2pChatID); err != nil {
		t.Fatalf("rememberControlRoomSource failed: %v", err)
	}

	if !bot.isAllowed("ou_user", "oc_control_room", 7, 99) {
		t.Fatal("control room should inherit source p2p chat allowlist")
	}
	if bot.isAllowed("ou_user", "oc_other_room", 7, 100) {
		t.Fatal("unregistered room should not inherit source p2p chat allowlist")
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

func TestDeleteMessageIsNoopToAvoidFeishuRecallNotice(t *testing.T) {
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

	if err := bot.DeleteMessage(context.Background(), chatID, 0, messageID); err != nil {
		t.Fatalf("DeleteMessage failed: %v", err)
	}
	if api.deleteCalls != 0 {
		t.Fatalf("Delete calls = %d, want no API recall", api.deleteCalls)
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

func TestReplyToMessageIDResolvesFeishuThreadTopicRootWithoutMention(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	chatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_topic_group")
	if err != nil {
		t.Fatalf("ResolveExternalID(chat) failed: %v", err)
	}
	rootMessageID, err := store.ResolveExternalID(ctx, namespaceMessage, "om_root")
	if err != nil {
		t.Fatalf("ResolveExternalID(root) failed: %v", err)
	}
	if err := store.PutFeishuMessageMap(ctx, rootMessageID, "om_root", chatID, "oc_topic_group"); err != nil {
		t.Fatalf("PutFeishuMessageMap(root) failed: %v", err)
	}
	if err := store.UpsertFeishuThreadTopic(ctx, model.FeishuThreadTopic{
		ChatID:            chatID,
		OpenChatID:        "oc_topic_group",
		ThreadID:          "thread-topic",
		RootMessageID:     rootMessageID,
		RootOpenMessageID: "om_root",
		FeishuThreadID:    "omt_topic",
	}); err != nil {
		t.Fatalf("UpsertFeishuThreadTopic failed: %v", err)
	}
	bot := &Bot{store: store}
	message := &larkim.EventMessage{
		ChatId:   ptrString("oc_topic_group"),
		RootId:   ptrString("om_root"),
		ThreadId: ptrString("omt_topic"),
	}

	got, err := bot.replyToMessageID(ctx, message)
	if err != nil {
		t.Fatalf("replyToMessageID failed: %v", err)
	}
	if got != rootMessageID {
		t.Fatalf("replyToMessageID = %d, want topic root %d", got, rootMessageID)
	}
}

func TestUnknownFeishuTopicGroupPlainMessageShouldBeIgnored(t *testing.T) {
	t.Parallel()

	message := &larkim.EventMessage{ChatType: ptrString("topic_group")}
	if !isFeishuGroupMessage(message) {
		t.Fatal("topic_group message should be treated as group message")
	}
	if isFeishuCommand("hello") {
		t.Fatal("plain text should not be treated as command")
	}
	if !isFeishuCommand(" /threads") {
		t.Fatal("slash command should be treated as command")
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

func ptrString(value string) *string {
	return &value
}

type fakeAPIClient struct {
	lastMsgType           string
	lastContent           string
	lastReceiveID         string
	lastOpenMessageID     string
	lastText              string
	lastCard              string
	lastReplyInThread     bool
	lastRoomName          string
	lastRoomUserOpenIDs   []string
	updateTextCalls       int
	patchCardCalls        int
	sendCalls             int
	replyCalls            int
	deleteCalls           int
	createThreadRoomCalls int
	chatModes             map[string]string
}

func (f *fakeAPIClient) Send(_ context.Context, receiveID, msgType, content string) (sentMessage, error) {
	f.sendCalls++
	f.lastReceiveID = receiveID
	f.lastMsgType = msgType
	f.lastContent = content
	return sentMessage{OpenMessageID: "om_fake", OpenChatID: receiveID}, nil
}

func (f *fakeAPIClient) SendToOpenID(_ context.Context, _, msgType, content string) (sentMessage, error) {
	f.sendCalls++
	f.lastMsgType = msgType
	f.lastContent = content
	return sentMessage{OpenMessageID: "om_fake", OpenChatID: "oc_fake"}, nil
}

func (f *fakeAPIClient) Reply(_ context.Context, openMessageID, msgType, content string, replyInThread bool) (sentMessage, error) {
	f.replyCalls++
	f.lastOpenMessageID = openMessageID
	f.lastMsgType = msgType
	f.lastContent = content
	f.lastReplyInThread = replyInThread
	return sentMessage{OpenMessageID: "om_reply", OpenChatID: "oc_fake", RootID: openMessageID, ThreadID: "othread_fake"}, nil
}

func (f *fakeAPIClient) GetChat(_ context.Context, openChatID string) (chatInfo, error) {
	mode := ""
	if f.chatModes != nil {
		mode = f.chatModes[openChatID]
	}
	return chatInfo{OpenChatID: openChatID, ChatMode: mode}, nil
}

func (f *fakeAPIClient) CreateThreadRoom(_ context.Context, name string, userOpenIDs []string, _ string, _ string) (string, error) {
	f.createThreadRoomCalls++
	f.lastRoomName = name
	f.lastRoomUserOpenIDs = append([]string(nil), userOpenIDs...)
	return "oc_control_room", nil
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
	f.deleteCalls++
	return nil
}

func (f *fakeAPIClient) UploadFile(context.Context, string, []byte) (string, error) {
	return "", nil
}
