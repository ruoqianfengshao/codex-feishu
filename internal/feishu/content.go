package feishu

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mideco-tech/codex-tg/internal/model"
)

const (
	messageLimit = 28000
	cardLimit    = 30000
)

type textContent struct {
	Text string `json:"text"`
}

type fileContent struct {
	FileKey string `json:"file_key"`
}

func encodeTextContent(text string) (string, error) {
	data, err := json.Marshal(textContent{Text: text})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func encodeFileContent(fileKey string) (string, error) {
	data, err := json.Marshal(fileContent{FileKey: fileKey})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func parseTextContent(messageType, raw string) string {
	if strings.TrimSpace(messageType) != "text" {
		return ""
	}
	var payload textContent
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return strings.TrimSpace(raw)
	}
	return strings.TrimSpace(payload.Text)
}

func renderPlainText(rendered model.RenderedMessage) string {
	return strings.TrimSpace(rendered.Text)
}

func renderPlainMessages(messages []model.RenderedMessage) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		if text := renderPlainText(message); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func splitText(text string, limit int) []string {
	if limit <= 0 {
		return []string{text}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if len(text) <= limit {
		return []string{text}
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	current := strings.Builder{}
	flush := func() {
		if current.Len() == 0 {
			return
		}
		out = append(out, strings.TrimSpace(current.String()))
		current.Reset()
	}
	for _, line := range lines {
		line = strings.TrimRight(line, " ")
		candidate := line
		if current.Len() > 0 {
			candidate = current.String() + "\n" + line
		}
		if len(candidate) <= limit {
			if current.Len() > 0 {
				current.WriteByte('\n')
			}
			current.WriteString(line)
			continue
		}
		flush()
		for len(line) > limit {
			out = append(out, strings.TrimSpace(line[:limit]))
			line = line[limit:]
		}
		if line != "" {
			current.WriteString(line)
		}
	}
	flush()
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

func fileTypeForName(name string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if ext == "" {
		return "txt"
	}
	switch ext {
	case "txt", "md", "json", "log", "zip", "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "csv":
		return ext
	default:
		return "stream"
	}
}

func checkCardSize(card string) error {
	if len(card) <= cardLimit {
		return nil
	}
	return fmt.Errorf("feishu card payload too large: %d bytes", len(card))
}
