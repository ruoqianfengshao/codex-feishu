package msgformat

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/mideco-tech/codex-tg/internal/model"
)

const MessageLimit = 4096

type Segment struct {
	Plain    string
	Markdown string
}

func Plain(text string) Segment {
	return Segment{Plain: text}
}

func Markdown(text string) Segment {
	return Segment{Markdown: text}
}

func RenderMarkdownWithHeader(header, markdown string) []model.RenderedMessage {
	return RenderSegments([]Segment{
		Plain(strings.TrimRight(header, "\n")),
		Plain("\n\n"),
		Markdown(strings.TrimSpace(markdown)),
	}, MessageLimit)
}

func RenderSegments(segments []Segment, maxLen int) []model.RenderedMessage {
	if maxLen <= 0 {
		maxLen = MessageLimit
	}
	text := strings.Builder{}
	entities := []model.MessageEntity{}
	for _, segment := range segments {
		if segment.Plain != "" {
			text.WriteString(segment.Plain)
		}
		if strings.TrimSpace(segment.Markdown) == "" {
			continue
		}
		offset := utf16Len(text.String())
		convertedText, convertedEntities := renderMarkdownEntities(segment.Markdown)
		text.WriteString(convertedText)
		for _, entity := range convertedEntities {
			entity.Offset += offset
			entities = append(entities, entity)
		}
	}
	out := splitRenderedMessage(text.String(), entities, maxLen)
	if len(out) == 0 {
		return []model.RenderedMessage{{Text: " "}}
	}
	return out
}

func HashRendered(message model.RenderedMessage) string {
	parts := []string{message.Text}
	for _, entity := range message.Entities {
		parts = append(parts, fmt.Sprintf("%s:%d:%d:%s:%s", entity.Type, entity.Offset, entity.Length, entity.URL, entity.Language))
	}
	return strings.Join(parts, "\x00")
}

func renderMarkdownEntities(markdown string) (string, []model.MessageEntity) {
	markdown = strings.TrimSpace(markdown)
	out := strings.Builder{}
	entities := []model.MessageEntity{}
	for i := 0; i < len(markdown); {
		if strings.HasPrefix(markdown[i:], "```") {
			endOfFence := i + 3
			lineEnd := strings.IndexByte(markdown[endOfFence:], '\n')
			if lineEnd < 0 {
				out.WriteString("```")
				i += 3
				continue
			}
			language := strings.TrimSpace(markdown[endOfFence : endOfFence+lineEnd])
			bodyStart := endOfFence + lineEnd + 1
			bodyEndRel := strings.Index(markdown[bodyStart:], "```")
			if bodyEndRel < 0 {
				out.WriteString(markdown[i:])
				break
			}
			body := strings.TrimRight(markdown[bodyStart:bodyStart+bodyEndRel], "\n")
			offset := utf16Len(out.String())
			out.WriteString(body)
			entities = append(entities, model.MessageEntity{
				Type:     "pre",
				Offset:   offset,
				Length:   utf16Len(body),
				Language: language,
			})
			i = bodyStart + bodyEndRel + 3
			continue
		}
		if markdown[i] == '`' {
			endRel := strings.IndexByte(markdown[i+1:], '`')
			if endRel >= 0 {
				code := markdown[i+1 : i+1+endRel]
				offset := utf16Len(out.String())
				out.WriteString(code)
				entities = append(entities, model.MessageEntity{
					Type:   "code",
					Offset: offset,
					Length: utf16Len(code),
				})
				i += endRel + 2
				continue
			}
		}
		if markdown[i] == '[' {
			if label, link, consumed, ok := parseMarkdownLink(markdown[i:]); ok {
				offset := utf16Len(out.String())
				out.WriteString(label)
				entities = append(entities, model.MessageEntity{
					Type:   "text_link",
					Offset: offset,
					Length: utf16Len(label),
					URL:    link,
				})
				i += consumed
				continue
			}
		}
		if link, consumed, ok := parseBareURL(markdown[i:]); ok {
			offset := utf16Len(out.String())
			out.WriteString(link)
			entities = append(entities, model.MessageEntity{
				Type:   "text_link",
				Offset: offset,
				Length: utf16Len(link),
				URL:    link,
			})
			i += consumed
			continue
		}
		r, size := utf8.DecodeRuneInString(markdown[i:])
		if r == utf8.RuneError && size == 0 {
			break
		}
		out.WriteRune(r)
		i += size
	}
	return out.String(), entities
}

func parseMarkdownLink(text string) (string, string, int, bool) {
	closeLabel := strings.IndexByte(text, ']')
	if closeLabel <= 0 || closeLabel+1 >= len(text) || text[closeLabel+1] != '(' {
		return "", "", 0, false
	}
	closeURLRel := strings.IndexByte(text[closeLabel+2:], ')')
	if closeURLRel < 0 {
		return "", "", 0, false
	}
	label := text[1:closeLabel]
	link := strings.TrimSpace(text[closeLabel+2 : closeLabel+2+closeURLRel])
	if strings.TrimSpace(label) == "" || !isHTTPURL(link) {
		return "", "", 0, false
	}
	return label, link, closeLabel + 2 + closeURLRel + 1, true
}

func parseBareURL(text string) (string, int, bool) {
	if !strings.HasPrefix(text, "http://") && !strings.HasPrefix(text, "https://") {
		return "", 0, false
	}
	end := 0
	for end < len(text) {
		r, size := utf8.DecodeRuneInString(text[end:])
		if r == utf8.RuneError && size == 0 {
			break
		}
		if isURLTerminator(r) {
			break
		}
		end += size
	}
	link := strings.TrimRight(text[:end], ".,;:!?)]}")
	if !isHTTPURL(link) {
		return "", 0, false
	}
	return link, len(link), true
}

func isURLTerminator(r rune) bool {
	switch r {
	case ' ', '\n', '\r', '\t', '<', '>', '"', '\'':
		return true
	default:
		return false
	}
}

func isHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func splitRenderedMessage(text string, entities []model.MessageEntity, maxLen int) []model.RenderedMessage {
	if maxLen <= 0 || len(text) <= maxLen {
		return []model.RenderedMessage{{Text: text, Entities: entities}}
	}
	out := []model.RenderedMessage{}
	startByte := 0
	startUTF16 := 0
	for startByte < len(text) {
		endByte, endUTF16 := splitEnd(text, startByte, maxLen)
		chunkText := text[startByte:endByte]
		out = append(out, model.RenderedMessage{
			Text:     chunkText,
			Entities: entitiesForRange(entities, startUTF16, endUTF16),
		})
		startByte = endByte
		startUTF16 = endUTF16
	}
	return out
}

func splitEnd(text string, startByte, maxLen int) (int, int) {
	endByte := startByte
	units := 0
	for endByte < len(text) {
		r, size := utf8.DecodeRuneInString(text[endByte:])
		if r == utf8.RuneError && size == 0 {
			break
		}
		nextUnits := units + runeUTF16Len(r)
		if nextUnits > maxLen && endByte > startByte {
			break
		}
		endByte += size
		units = nextUnits
	}
	return endByte, utf16Len(text[:endByte])
}

func entitiesForRange(entities []model.MessageEntity, start, end int) []model.MessageEntity {
	out := []model.MessageEntity{}
	for _, entity := range entities {
		entityStart := entity.Offset
		entityEnd := entity.Offset + entity.Length
		if entityStart < start || entityEnd > end {
			continue
		}
		entity.Offset -= start
		out = append(out, entity)
	}
	return out
}

func utf16Len(text string) int {
	n := 0
	for _, r := range text {
		n += runeUTF16Len(r)
	}
	return n
}

func runeUTF16Len(r rune) int {
	if len(utf16.Encode([]rune{r})) == 2 {
		return 2
	}
	return 1
}
