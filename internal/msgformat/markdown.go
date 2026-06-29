package msgformat

import (
	"fmt"
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
		r, size := utf8.DecodeRuneInString(markdown[i:])
		if r == utf8.RuneError && size == 0 {
			break
		}
		out.WriteRune(r)
		i += size
	}
	return out.String(), entities
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
