package feishu

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type apiClient interface {
	Send(ctx context.Context, receiveID, msgType, content string) (sentMessage, error)
	SendToOpenID(ctx context.Context, openID, msgType, content string) (sentMessage, error)
	Reply(ctx context.Context, openMessageID, msgType, content string, replyInThread bool) (sentMessage, error)
	GetChat(ctx context.Context, openChatID string) (chatInfo, error)
	CreateThreadRoom(ctx context.Context, name string, userOpenIDs []string, botAppID string, uuid string) (string, error)
	UpdateText(ctx context.Context, openMessageID, text string) error
	PatchCard(ctx context.Context, openMessageID, card string) error
	Delete(ctx context.Context, openMessageID string) error
	UploadFile(ctx context.Context, name string, data []byte) (string, error)
}

type sentMessage struct {
	OpenMessageID string
	OpenChatID    string
	RootID        string
	ThreadID      string
}

type chatInfo struct {
	OpenChatID       string
	Name             string
	ChatMode         string
	GroupMessageType string
}

type sdkAPIClient struct {
	client *lark.Client
}

func newSDKAPIClient(appID, appSecret string) *sdkAPIClient {
	return &sdkAPIClient{client: lark.NewClient(appID, appSecret)}
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
