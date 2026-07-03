package feishu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type apiClient interface {
	RefreshAuth(ctx context.Context) error
	Send(ctx context.Context, receiveID, msgType, content string) (sentMessage, error)
	SendToOpenID(ctx context.Context, openID, msgType, content string) (sentMessage, error)
	Reply(ctx context.Context, openMessageID, msgType, content string, replyInThread bool) (sentMessage, error)
	GetChat(ctx context.Context, openChatID string) (chatInfo, error)
	CreateThreadRoom(ctx context.Context, name string, userOpenIDs []string, botAppID string, uuid string) (string, error)
	CreateWorkspaceMenu(ctx context.Context, openChatID string) error
	UpdateText(ctx context.Context, openMessageID, text string) error
	PatchCard(ctx context.Context, openMessageID, card string) error
	Delete(ctx context.Context, openMessageID string) error
	UploadFile(ctx context.Context, name string, data []byte) (string, error)
	UploadImage(ctx context.Context, data []byte) (string, error)
	DownloadImage(ctx context.Context, openMessageID, imageKey string) ([]byte, error)
}

type sentMessage struct {
	OpenMessageID string
	OpenChatID    string
	RootID        string
	ThreadID      string
}

type authRecoveryError struct {
	err error
}

func (e authRecoveryError) Error() string {
	if e.err == nil {
		return "feishu auth recovery failed"
	}
	return "feishu auth recovery failed: " + e.err.Error()
}

func (e authRecoveryError) Unwrap() error {
	return e.err
}

type chatInfo struct {
	OpenChatID       string
	Name             string
	ChatMode         string
	GroupMessageType string
}

type sdkAPIClient struct {
	appID     string
	appSecret string
	client    *lark.Client
}

func newSDKAPIClient(appID, appSecret string) *sdkAPIClient {
	return &sdkAPIClient{
		appID:     strings.TrimSpace(appID),
		appSecret: strings.TrimSpace(appSecret),
		client:    lark.NewClient(appID, appSecret),
	}
}

func (c *sdkAPIClient) RefreshAuth(ctx context.Context) error {
	if c == nil || c.client == nil {
		return errors.New("feishu api client is not configured")
	}
	resp, err := c.client.GetTenantAccessTokenBySelfBuiltApp(ctx, &larkcore.SelfBuiltTenantAccessTokenReq{
		AppID:     c.appID,
		AppSecret: c.appSecret,
	})
	if err != nil {
		return fmt.Errorf("refresh tenant access token: %w", err)
	}
	if resp == nil || !resp.Success() || strings.TrimSpace(resp.TenantAccessToken) == "" {
		code := 0
		message := ""
		requestID := ""
		if resp != nil {
			code = resp.Code
			message = resp.Msg
			requestID = resp.RequestId()
		}
		return sdkError("refresh tenant access token", code, message, requestID)
	}
	return nil
}

func (c *sdkAPIClient) Send(ctx context.Context, receiveID, msgType, content string) (sentMessage, error) {
	return c.send(ctx, "chat_id", receiveID, msgType, content)
}

func (c *sdkAPIClient) SendToOpenID(ctx context.Context, openID, msgType, content string) (sentMessage, error) {
	return c.send(ctx, "open_id", openID, msgType, content)
}

func (c *sdkAPIClient) send(ctx context.Context, receiveIDType, receiveID, msgType, content string) (sentMessage, error) {
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			MsgType(msgType).
			Content(content).
			Build()).
		Build()
	resp, err := c.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return sentMessage{}, err
	}
	if !resp.Success() {
		return sentMessage{}, sdkError("send message", resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.Data == nil {
		return sentMessage{}, nil
	}
	return sentMessage{
		OpenMessageID: value(resp.Data.MessageId),
		OpenChatID:    value(resp.Data.ChatId),
		RootID:        value(resp.Data.RootId),
		ThreadID:      value(resp.Data.ThreadId),
	}, nil
}

func (c *sdkAPIClient) Reply(ctx context.Context, openMessageID, msgType, content string, replyInThread bool) (sentMessage, error) {
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(openMessageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(msgType).
			Content(content).
			ReplyInThread(replyInThread).
			Build()).
		Build()
	resp, err := c.client.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return sentMessage{}, err
	}
	if !resp.Success() {
		return sentMessage{}, sdkError("reply message", resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.Data == nil {
		return sentMessage{}, nil
	}
	return sentMessage{
		OpenMessageID: value(resp.Data.MessageId),
		OpenChatID:    value(resp.Data.ChatId),
		RootID:        value(resp.Data.RootId),
		ThreadID:      value(resp.Data.ThreadId),
	}, nil
}

func (c *sdkAPIClient) GetChat(ctx context.Context, openChatID string) (chatInfo, error) {
	req := larkim.NewGetChatReqBuilder().
		ChatId(openChatID).
		UserIdType(larkim.UserIdTypeOpenId).
		Build()
	resp, err := c.client.Im.V1.Chat.Get(ctx, req)
	if err != nil {
		return chatInfo{}, err
	}
	if !resp.Success() {
		return chatInfo{}, sdkError("get chat", resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.Data == nil {
		return chatInfo{OpenChatID: strings.TrimSpace(openChatID)}, nil
	}
	return chatInfo{
		OpenChatID:       strings.TrimSpace(openChatID),
		Name:             value(resp.Data.Name),
		ChatMode:         value(resp.Data.ChatMode),
		GroupMessageType: value(resp.Data.GroupMessageType),
	}, nil
}

func (c *sdkAPIClient) CreateThreadRoom(ctx context.Context, name string, userOpenIDs []string, botAppID string, uuid string) (string, error) {
	body := larkim.NewCreateChatReqBodyBuilder().
		Name(name).
		UserIdList(userOpenIDs).
		BotIdList([]string{botAppID}).
		GroupMessageType(larkim.CreateChatGroupMessageTypeThread).
		ChatMode("group").
		ChatType("private").
		JoinMessageVisibility("not_anyone").
		LeaveMessageVisibility("not_anyone").
		Build()
	req := larkim.NewCreateChatReqBuilder().
		UserIdType(larkim.UserIdTypeOpenId).
		SetBotManager(true).
		Uuid(uuid).
		Body(body).
		Build()
	resp, err := c.client.Im.V1.Chat.Create(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", sdkError("create chat", resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.Data == nil {
		return "", nil
	}
	return value(resp.Data.ChatId), nil
}

func (c *sdkAPIClient) CreateWorkspaceMenu(ctx context.Context, openChatID string) error {
	menu := larkim.NewChatMenuTreeBuilder().
		ChatMenuTopLevels([]*larkim.ChatMenuTopLevel{
			chatMenuTopLevel("codex_recent", "会话", []chatMenuEntry{
				{id: "codex_chats", name: "最近会话", url: feishuMenuCommandURL("chats")},
				{id: "codex_projects", name: "项目", url: feishuMenuCommandURL("projects")},
			}),
			chatMenuTopLevel("codex_new", "新建", []chatMenuEntry{
				{id: "codex_new", name: "新建 Chat", url: feishuMenuCommandURL("new")},
			}),
			chatMenuTopLevel("codex_more", "更多", []chatMenuEntry{
				{id: "codex_status", name: "状态", url: feishuMenuCommandURL("status")},
				{id: "codex_setting", name: "设置", url: feishuMenuCommandURL("setting")},
				{id: "codex_repair", name: "修复", url: feishuMenuCommandURL("repair")},
			}),
		}).
		Build()
	req := larkim.NewCreateChatMenuTreeReqBuilder().
		ChatId(openChatID).
		Body(larkim.NewCreateChatMenuTreeReqBodyBuilder().MenuTree(menu).Build()).
		Build()
	resp, err := c.client.Im.V1.ChatMenuTree.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return sdkError("create chat menu", resp.Code, resp.Msg, resp.RequestId())
	}
	return nil
}

type chatMenuEntry struct {
	id   string
	name string
	url  string
}

func chatMenuTopLevel(id, name string, children []chatMenuEntry) *larkim.ChatMenuTopLevel {
	secondLevels := make([]*larkim.ChatMenuSecondLevel, 0, len(children))
	for _, child := range children {
		secondLevels = append(secondLevels, larkim.NewChatMenuSecondLevelBuilder().
			ChatMenuSecondLevelId(child.id).
			ChatMenuItem(chatMenuItem(child.name, child.url)).
			Build())
	}
	return larkim.NewChatMenuTopLevelBuilder().
		ChatMenuTopLevelId(id).
		ChatMenuItem(larkim.NewChatMenuItemBuilder().ActionType("NONE").Name(name).Build()).
		Children(secondLevels).
		Build()
}

func chatMenuItem(name, url string) *larkim.ChatMenuItem {
	return larkim.NewChatMenuItemBuilder().
		ActionType("REDIRECT_LINK").
		Name(name).
		RedirectLink(larkim.NewChatMenuItemRedirectLinkBuilder().CommonUrl(url).Build()).
		Build()
}

func feishuMenuCommandURL(command string) string {
	return "https://applink.feishu.cn/client/bot/open?app_id=codex-feishu&command=" + command
}

func (c *sdkAPIClient) UpdateText(ctx context.Context, openMessageID, text string) error {
	content, err := encodeTextContent(text)
	if err != nil {
		return err
	}
	req := larkim.NewUpdateMessageReqBuilder().
		MessageId(openMessageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().
			MsgType("text").
			Content(content).
			Build()).
		Build()
	resp, err := c.client.Im.V1.Message.Update(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return sdkError("update message", resp.Code, resp.Msg, resp.RequestId())
	}
	return nil
}

func (c *sdkAPIClient) PatchCard(ctx context.Context, openMessageID, card string) error {
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(openMessageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(card).
			Build()).
		Build()
	resp, err := c.client.Im.V1.Message.Patch(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return sdkError("patch message card", resp.Code, resp.Msg, resp.RequestId())
	}
	return nil
}

func (c *sdkAPIClient) Delete(ctx context.Context, openMessageID string) error {
	req := larkim.NewDeleteMessageReqBuilder().MessageId(openMessageID).Build()
	resp, err := c.client.Im.V1.Message.Delete(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return sdkError("delete message", resp.Code, resp.Msg, resp.RequestId())
	}
	return nil
}

func (c *sdkAPIClient) UploadFile(ctx context.Context, name string, data []byte) (string, error) {
	req := larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().
			FileType(fileTypeForName(name)).
			FileName(name).
			File(bytes.NewReader(data)).
			Build()).
		Build()
	resp, err := c.client.Im.V1.File.Create(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", sdkError("upload file", resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.Data == nil {
		return "", nil
	}
	return value(resp.Data.FileKey), nil
}

func (c *sdkAPIClient) UploadImage(ctx context.Context, data []byte) (string, error) {
	if len(data) == 0 {
		return "", io.ErrUnexpectedEOF
	}
	req := larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().
			ImageType(larkim.CreateImageImageTypeMessage).
			Image(bytes.NewReader(data)).
			Build()).
		Build()
	resp, err := c.client.Im.V1.Image.Create(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", sdkError("upload image", resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.Data == nil {
		return "", nil
	}
	return value(resp.Data.ImageKey), nil
}

func (c *sdkAPIClient) DownloadImage(ctx context.Context, openMessageID, imageKey string) ([]byte, error) {
	openMessageID = strings.TrimSpace(openMessageID)
	imageKey = strings.TrimSpace(imageKey)
	if openMessageID == "" {
		return nil, fmt.Errorf("feishu message id is empty")
	}
	if imageKey == "" {
		return nil, fmt.Errorf("feishu image key is empty")
	}
	resp, err := c.client.Im.V1.MessageResource.Get(ctx, larkim.NewGetMessageResourceReqBuilder().
		MessageId(openMessageID).
		FileKey(imageKey).
		Type("image").
		Build())
	if err != nil {
		return nil, err
	}
	if !resp.Success() {
		return nil, sdkError("download message image", resp.Code, resp.Msg, resp.RequestId())
	}
	if resp.File == nil {
		return nil, io.ErrUnexpectedEOF
	}
	data, err := io.ReadAll(resp.File)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}

func sdkError(action string, code int, message, requestID string) error {
	parts := []string{fmt.Sprintf("feishu %s failed", action)}
	if code != 0 {
		parts = append(parts, fmt.Sprintf("code=%d", code))
	}
	if strings.TrimSpace(message) != "" {
		parts = append(parts, strings.TrimSpace(message))
	}
	if strings.TrimSpace(requestID) != "" {
		parts = append(parts, "request_id="+strings.TrimSpace(requestID))
	}
	return fmt.Errorf("%s", strings.Join(parts, ": "))
}

func isFeishuAuthError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"code=99991663",
		"code=99991661",
		"invalid access token",
		"access token invalid",
		"access token expired",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func isFeishuAuthRecoveryError(err error) bool {
	var recoveryErr authRecoveryError
	return errors.As(err, &recoveryErr)
}

func value(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return strings.TrimSpace(*pointer)
}

func sdkLogLevel(debug bool) larkcore.LogLevel {
	if debug {
		return larkcore.LogLevelDebug
	}
	return larkcore.LogLevelInfo
}
