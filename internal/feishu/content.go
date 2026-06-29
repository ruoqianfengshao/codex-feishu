package feishu

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

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

type imageContent struct {
	ImageKey string `json:"image_key"`
}

type postContent struct {
	Post map[string]postLanguageContent `json:"post"`
}

type postLanguageContent struct {
	Title   string          `json:"title"`
	Content [][]postElement `json:"content"`
}

type postElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	ImageKey string `json:"image_key"`
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

func parseImageKeyContent(messageType, raw string) string {
	if strings.TrimSpace(messageType) != "image" {
		return ""
	}
	var payload imageContent
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.ImageKey)
}

func parsePostContent(messageType, raw string) (string, string) {
	if strings.TrimSpace(messageType) != "post" {
		return "", ""
	}
	var payload postContent
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", ""
	}
	content := payload.Post["zh_cn"]
	if len(content.Content) == 0 {
		for _, candidate := range payload.Post {
			content = candidate
			break
		}
	}
	if len(content.Content) == 0 {
		if err := json.Unmarshal([]byte(raw), &content); err != nil {
			return "", ""
		}
	}
	return parsePostLanguageContent(content)
}

func parsePostLanguageContent(content postLanguageContent) (string, string) {
	var textParts []string
	imageKey := ""
	for _, row := range content.Content {
		var rowText strings.Builder
		for _, element := range row {
			switch strings.TrimSpace(element.Tag) {
			case "text", "a", "at":
				rowText.WriteString(element.Text)
			case "img":
				if imageKey == "" {
					imageKey = strings.TrimSpace(element.ImageKey)
				}
			}
		}
		if text := strings.TrimSpace(rowText.String()); text != "" {
			textParts = append(textParts, text)
		}
	}
	return strings.TrimSpace(strings.Join(textParts, "\n")), imageKey
}

func renderPlainText(rendered model.RenderedMessage) string {
	text := strings.TrimSpace(rendered.Text)
	if text == "" || len(rendered.Entities) == 0 {
		return text
	}
	return renderTextLinks(text, rendered.Entities)
}

func renderTextLinks(text string, entities []model.MessageEntity) string {
	type linkRange struct {
		start int
		end   int
		url   string
	}
	links := []linkRange{}
	for _, entity := range entities {
		if entity.Type != "text_link" || strings.TrimSpace(entity.URL) == "" || entity.Length <= 0 {
			continue
		}
		start, ok := byteOffsetForUTF16(text, entity.Offset)
		if !ok {
			continue
		}
		end, ok := byteOffsetForUTF16(text, entity.Offset+entity.Length)
		if !ok || end <= start {
			continue
		}
		links = append(links, linkRange{start: start, end: end, url: strings.TrimSpace(entity.URL)})
	}
	if len(links) == 0 {
		return text
	}
	sort.SliceStable(links, func(i, j int) bool { return links[i].start < links[j].start })
	var out strings.Builder
	cursor := 0
	lastEnd := 0
	for _, link := range links {
		if link.start < lastEnd || link.start < cursor || link.end > len(text) {
			continue
		}
		out.WriteString(text[cursor:link.start])
		label := text[link.start:link.end]
		out.WriteString("[")
		out.WriteString(label)
		out.WriteString("](")
		out.WriteString(link.url)
		out.WriteString(")")
		cursor = link.end
		lastEnd = link.end
	}
	out.WriteString(text[cursor:])
	return out.String()
}

func byteOffsetForUTF16(text string, target int) (int, bool) {
	if target < 0 {
		return 0, false
	}
	units := 0
	for index := 0; index < len(text); {
		if units == target {
			return index, true
		}
		r, size := utf8.DecodeRuneInString(text[index:])
		if r == utf8.RuneError && size == 0 {
			break
		}
		units += len(utf16.Encode([]rune{r}))
		index += size
	}
	if units == target {
		return len(text), true
	}
	return 0, false
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
