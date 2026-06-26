package feishu

import (
	"encoding/json"
	"strings"

	"github.com/mideco-tech/codex-tg/internal/model"
)

const maxCardButtonsPerRow = 3

func buildCard(text string, buttons [][]model.ButtonSpec) (string, error) {
	return buildSectionedCard([]model.MessageSection{{
		Text:    text,
		Buttons: buttons,
	}})
}

func buildRenderedCard(message model.RenderedMessage, buttons [][]model.ButtonSpec) (string, error) {
	return buildCardWithStyle(renderPlainText(message), buttons, message.Style)
}

func buildSectionedCard(sections []model.MessageSection) (string, error) {
	return buildSectionedCardWithStyle(sections, "")
}

func buildCardWithStyle(text string, buttons [][]model.ButtonSpec, style string) (string, error) {
	return buildSectionedCardWithStyle([]model.MessageSection{{
		Text:    text,
		Buttons: buttons,
	}}, style)
}

func buildSectionedCardWithStyle(sections []model.MessageSection, style string) (string, error) {
	if len(sections) == 0 {
		sections = []model.MessageSection{{Text: " "}}
	}
	elements := make([]map[string]any, 0, len(sections)*2)
	for _, section := range sections {
		text := strings.TrimSpace(section.Text)
		if text != "" {
			elements = append(elements, markdownElement(text))
		}
		elements = append(elements, buttonElements(section.Buttons)...)
	}
	if len(elements) == 0 {
		elements = append(elements, markdownElement(" "))
	}
	card := map[string]any{
		"config": map[string]any{
			"wide_screen_mode": true,
		},
		"elements": elements,
	}
	if header := cardHeaderForStyle(style); header != nil {
		card["header"] = header
	}
	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	out := string(data)
	if err := checkCardSize(out); err != nil {
		return "", err
	}
	return out, nil
}

func cardHeaderForStyle(style string) map[string]any {
	switch strings.TrimSpace(style) {
	case model.MessageStyleDesktopUser:
		return map[string]any{
			"template": "blue",
			"title": map[string]any{
				"tag":     "plain_text",
				"content": "来自 Codex 桌面端",
			},
		}
	default:
		return nil
	}
}

func markdownElement(text string) map[string]any {
	text = strings.TrimSpace(text)
	if text == "" {
		text = " "
	}
	return map[string]any{
		"tag":     "markdown",
		"content": text,
	}
}

func buttonElements(buttons [][]model.ButtonSpec) []map[string]any {
	flat := make([]map[string]any, 0)
	for _, row := range buttons {
		for _, button := range row {
			if label := feishuButtonLabel(button.Text); label != "" {
				flat = append(flat, map[string]any{
					"tag":  "button",
					"text": map[string]any{"tag": "plain_text", "content": label},
					"type": "default",
					"value": map[string]any{
						"callback_data": button.CallbackData,
					},
				})
			}
		}
	}
	if len(flat) == 0 {
		return nil
	}
	elements := make([]map[string]any, 0, (len(flat)+maxCardButtonsPerRow-1)/maxCardButtonsPerRow)
	for start := 0; start < len(flat); start += maxCardButtonsPerRow {
		end := min(start+maxCardButtonsPerRow, len(flat))
		element := map[string]any{
			"tag":     "action",
			"actions": flat[start:end],
		}
		switch end - start {
		case 2:
			element["layout"] = "bisected"
		case 3:
			element["layout"] = "trisection"
		}
		elements = append(elements, element)
	}
	return elements
}

func feishuButtonLabel(text string) string {
	label := strings.TrimSpace(text)
	if label == "" {
		return ""
	}
	if mapped, ok := feishuButtonLabels[label]; ok {
		return mapped
	}
	if strings.HasPrefix(label, "Open ") {
		return "打开 " + strings.TrimSpace(strings.TrimPrefix(label, "Open "))
	}
	if strings.HasPrefix(label, "Chat ") {
		return "聊天 " + strings.TrimSpace(strings.TrimPrefix(label, "Chat "))
	}
	return label
}

var feishuButtonLabels = map[string]string{
	"Approve":         "批准",
	"Approve Session": "批准会话",
	"Back":            "返回",
	"Bind here":       "绑定",
	"Cancel":          "取消",
	"Continue":        "继续",
	"Deny":            "拒绝",
	"Details":         "详情",
	"Get full log":    "日志",
	"Get thread id":   "线程ID",
	"New chat":        "新聊天",
	"New thread":      "新线程",
	"Observe here":    "观察",
	"Open Chats":      "聊天",
	"Reply":           "回复",
	"Show":            "查看",
	"Show context":    "上下文",
	"Stop":            "停止",
	"Steer":           "追加",
	"Tool on":         "工具开",
	"Turn off Plan":   "关计划",
}

func callbackDataFromValue(value map[string]interface{}) string {
	if len(value) == 0 {
		return ""
	}
	if raw, ok := value["callback_data"].(string); ok {
		return strings.TrimSpace(raw)
	}
	return ""
}
