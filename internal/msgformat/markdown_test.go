package msgformat

import (
	"strings"
	"testing"

	"github.com/mideco-tech/codex-tg/internal/model"
)

func TestRenderMarkdownWithHeaderKeepsHeaderPlainAndConvertsCodeFence(t *testing.T) {
	t.Parallel()

	messages := RenderMarkdownWithHeader("[Final] [Project: Codex] [Thread: Найти *Swagger* [Stellar]]\nStatus: completed", "Run `rg`:\n\n```bash\nrg -n 'Authorization' stellar_ws.txt\n```\n\n- done")
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	message := messages[0]
	if !strings.Contains(message.Text, "[Thread: Найти *Swagger* [Stellar]]") {
		t.Fatalf("header was not preserved as plain text: %q", message.Text)
	}
	if strings.Contains(message.Text, "```") {
		t.Fatalf("rendered text still contains raw markdown fence: %q", message.Text)
	}
	if !hasEntity(message.Entities, "code", "") {
		t.Fatalf("entities = %#v, want inline code entity", message.Entities)
	}
	if !hasEntity(message.Entities, "pre", "bash") {
		t.Fatalf("entities = %#v, want bash pre entity", message.Entities)
	}
}

func TestRenderSegmentsSplitsLongMarkdown(t *testing.T) {
	t.Parallel()

	messages := RenderSegments([]Segment{
		Plain("[Final]\n\n"),
		Markdown(strings.Repeat("line with `code`\n", 500)),
	}, 512)
	if len(messages) < 2 {
		t.Fatalf("len(messages) = %d, want split messages", len(messages))
	}
	for _, message := range messages {
		if len(message.Text) == 0 {
			t.Fatal("split message text must not be empty")
		}
	}
}

func TestRenderSegmentsAutolinksBareURL(t *testing.T) {
	t.Parallel()

	messages := RenderSegments([]Segment{
		Markdown("See https://example.com/path?q=1 for details."),
	}, MessageLimit)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	if !hasEntityURL(messages[0].Entities, "https://example.com/path?q=1") {
		t.Fatalf("entities = %#v, want URL entity", messages[0].Entities)
	}
}

func TestRenderSegmentsKeepsMarkdownLinkClickable(t *testing.T) {
	t.Parallel()

	messages := RenderSegments([]Segment{
		Markdown("Open [docs](https://example.com/docs)."),
	}, MessageLimit)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	if strings.Contains(messages[0].Text, "[docs](") || !strings.Contains(messages[0].Text, "Open docs.") {
		t.Fatalf("text = %q, want markdown link label only", messages[0].Text)
	}
	if !hasEntityURL(messages[0].Entities, "https://example.com/docs") {
		t.Fatalf("entities = %#v, want markdown URL entity", messages[0].Entities)
	}
}

func TestRenderSegmentsDoesNotAutolinkCodeURL(t *testing.T) {
	t.Parallel()

	messages := RenderSegments([]Segment{
		Markdown("Run `curl https://example.com` then inspect."),
	}, MessageLimit)
	if len(messages) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(messages))
	}
	for _, entity := range messages[0].Entities {
		if entity.Type == "text_link" || entity.URL != "" {
			t.Fatalf("entities = %#v, want no URL entity inside code", messages[0].Entities)
		}
	}
	if !hasEntity(messages[0].Entities, "code", "") {
		t.Fatalf("entities = %#v, want code entity", messages[0].Entities)
	}
}

func hasEntity(entities []model.MessageEntity, entityType, language string) bool {
	for _, entity := range entities {
		if entity.Type != entityType {
			continue
		}
		if language == "" || entity.Language == language {
			return true
		}
	}
	return false
}

func hasEntityURL(entities []model.MessageEntity, url string) bool {
	for _, entity := range entities {
		if entity.Type == "text_link" && entity.URL == url {
			return true
		}
	}
	return false
}
