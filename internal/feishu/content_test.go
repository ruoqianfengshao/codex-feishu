package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/ruoqianfengshao/codex-feishu/internal/appserver"
	"github.com/ruoqianfengshao/codex-feishu/internal/config"
	"github.com/ruoqianfengshao/codex-feishu/internal/daemon"
	"github.com/ruoqianfengshao/codex-feishu/internal/model"
	"github.com/ruoqianfengshao/codex-feishu/internal/storage"
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

func TestBuildSettingsFormCard(t *testing.T) {
	t.Parallel()

	card, err := buildSettingsFormCard(model.SettingsForm{
		Title:          "Codex settings",
		SubmitText:     "Apply settings",
		SubmitToken:    "submit-token",
		ModelLabel:     "Model",
		ModelValue:     "gpt-5",
		ModelOptions:   []model.SelectOption{{Text: "Auto", Value: ""}, {Text: "gpt-5", Value: "gpt-5"}},
		ReasoningLabel: "Reasoning effort",
		ReasoningValue: "low",
		ReasoningOptions: []model.SelectOption{
			{Text: "Auto", Value: ""},
			{Text: "low", Value: "low"},
		},
		LanguageLabel:   "Language",
		LanguageValue:   "en",
		LanguageOptions: []model.SelectOption{{Text: "中文", Value: "zh"}, {Text: "English", Value: "en"}},
	})
	if err != nil {
		t.Fatalf("buildSettingsFormCard failed: %v", err)
	}
	for _, want := range []string{`"tag":"form"`, `"tag":"select_static"`, `"name":"model"`, `"name":"reasoning"`, `"name":"language"`, `"action_type":"form_submit"`, `"callback_data":"submit-token"`} {
		if !strings.Contains(card, want) {
			t.Fatalf("settings form card missing %s:\n%s", want, card)
		}
	}
	if err := json.Unmarshal([]byte(card), &map[string]any{}); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
	var decoded struct {
		Body struct {
			Elements []struct {
				Elements []map[string]any `json:"elements"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
	modelSelect := decoded.Body.Elements[0].Elements[1]
	if got, ok := modelSelect["initial_option"].(string); !ok || got != "gpt-5" {
		t.Fatalf("initial_option = %#v, want string gpt-5", modelSelect["initial_option"])
	}
}

func TestImageMessageTextDownloadsImageAndReturnsLocalPath(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	api := &fakeAPIClient{downloadedImages: map[string][]byte{"img_test": []byte("fake image bytes")}}
	bot := &Bot{
		cfg: config.Config{Paths: config.Paths{DataDir: dataDir}},
		api: api,
	}
	event := newImageMessageEvent("oc_chat", "om_image", "ou_user", "img_test")

	text, err := bot.imageMessageText(ctx, event.Event.Message)
	if err != nil {
		t.Fatalf("imageMessageText failed: %v", err)
	}
	if !strings.Contains(text, "feishu-attachments") || !strings.Contains(text, "Files mentioned by the user") || !strings.Contains(text, "My request for Codex") {
		t.Fatalf("text = %q, want Codex attachment prompt", text)
	}
	start := strings.Index(text, `path="`)
	if start < 0 {
		t.Fatalf("text = %q, want image path attribute", text)
	}
	start += len(`path="`)
	end := strings.Index(text[start:], `"`)
	if end < 0 {
		t.Fatalf("text = %q, want closed image path attribute", text)
	}
	path := text[start : start+end]
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", path, err)
	}
	if string(data) != "fake image bytes" {
		t.Fatalf("saved image = %q, want fake image bytes", string(data))
	}
	if api.downloadImageMessageIDs[0] != "om_image" || api.downloadImageKeys[0] != "img_test" {
		t.Fatalf("download image calls = messages %#v keys %#v, want om_image/img_test", api.downloadImageMessageIDs, api.downloadImageKeys)
	}
}

func TestBotRunReconnectsWebSocketAfterDisconnect(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &fakeWSClient{err: fmt.Errorf("temporary websocket disconnect")}
	secondStarted := make(chan struct{})
	second := &fakeWSClient{started: secondStarted}
	created := 0
	bot := &Bot{logger: log.New(io.Discard, "", 0)}
	bot.wsNew = func() wsClient {
		created++
		if created == 1 {
			return first
		}
		return second
	}
	bot.ws = bot.wsNew()

	done := make(chan error, 1)
	go func() {
		done <- bot.Run(ctx)
	}()

	select {
	case <-secondStarted:
	case err := <-done:
		t.Fatalf("Run returned before reconnect: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for websocket reconnect")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error after cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if first.starts != 1 || second.starts != 1 {
		t.Fatalf("starts first=%d second=%d, want 1/1", first.starts, second.starts)
	}
}

func TestPostMessageTextDownloadsImageAndKeepsRequestText(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	api := &fakeAPIClient{downloadedImages: map[string][]byte{"img_post": []byte("post image bytes")}}
	bot := &Bot{
		cfg: config.Config{Paths: config.Paths{DataDir: dataDir}},
		api: api,
	}
	event := newPostImageMessageEvent("oc_chat", "om_post", "ou_user", "帮我看看这个甜品", "img_post")

	text, err := bot.imageMessageText(ctx, event.Event.Message)
	if err != nil {
		t.Fatalf("imageMessageText failed: %v", err)
	}
	if !strings.Contains(text, "帮我看看这个甜品") || !strings.Contains(text, "My request for Codex") || !strings.Contains(text, "feishu-attachments") {
		t.Fatalf("text = %q, want Codex attachment prompt with request text", text)
	}
	if api.downloadImageMessageIDs[0] != "om_post" || api.downloadImageKeys[0] != "img_post" {
		t.Fatalf("download image calls = messages %#v keys %#v, want om_post/img_post", api.downloadImageMessageIDs, api.downloadImageKeys)
	}
}

func TestDirectPostMessageTextDownloadsImageAndKeepsRequestText(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	api := &fakeAPIClient{downloadedImages: map[string][]byte{"img_direct": []byte("direct post image bytes")}}
	bot := &Bot{
		cfg: config.Config{Paths: config.Paths{DataDir: dataDir}},
		api: api,
	}
	event := newDirectPostImageMessageEvent("oc_chat", "om_direct", "ou_user", "图片加文字测试", "img_direct")

	text, err := bot.imageMessageText(ctx, event.Event.Message)
	if err != nil {
		t.Fatalf("imageMessageText failed: %v", err)
	}
	if !strings.Contains(text, "图片加文字测试") || !strings.Contains(text, "My request for Codex") || !strings.Contains(text, "feishu-attachments") {
		t.Fatalf("text = %q, want Codex attachment prompt with request text", text)
	}
	if api.downloadImageMessageIDs[0] != "om_direct" || api.downloadImageKeys[0] != "img_direct" {
		t.Fatalf("download image calls = messages %#v keys %#v, want om_direct/img_direct", api.downloadImageMessageIDs, api.downloadImageKeys)
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

func TestBuildCardKeepsProvidedButtonLabels(t *testing.T) {
	t.Parallel()

	card, err := buildCard("Status", [][]model.ButtonSpec{{
		{Text: "Stop", CallbackData: "cb_stop"},
		{Text: "Steer", CallbackData: "cb_steer"},
	}})
	if err != nil {
		t.Fatalf("buildCard failed: %v", err)
	}
	for _, label := range []string{"Stop", "Steer"} {
		if !strings.Contains(card, label) {
			t.Fatalf("card = %s, want provided label %q", card, label)
		}
	}
}

func TestBuildCodexPanelCardUsesDocumentedCollapsiblePanelFields(t *testing.T) {
	t.Parallel()

	card, err := buildRenderedCard(model.RenderedMessage{
		Style:                 model.MessageStyleCodexPanel,
		Text:                  "Running",
		CodexStatus:           "已处理 5m 58s",
		CodexProgressMarkdown: "思考中...\n\n工具调用中...",
		CodexProgressExpanded: true,
	}, nil)
	if err != nil {
		t.Fatalf("buildRenderedCard failed: %v", err)
	}
	if strings.Contains(card, "background_style") {
		t.Fatalf("card = %s, want no unsupported background_style on collapsible_panel", card)
	}

	var decoded struct {
		Body struct {
			Elements []struct {
				Tag             string `json:"tag"`
				BackgroundColor string `json:"background_color"`
				Header          struct {
					BackgroundColor string `json:"background_color"`
					IconPosition    string `json:"icon_position"`
					IconAngle       int    `json:"icon_expanded_angle"`
					Title           struct {
						Tag     string `json:"tag"`
						Content string `json:"content"`
					} `json:"title"`
					Icon struct {
						Tag   string `json:"tag"`
						Token string `json:"token"`
						Size  string `json:"size"`
					} `json:"icon"`
				} `json:"header"`
				Border struct {
					Color        string `json:"color"`
					CornerRadius string `json:"corner_radius"`
				} `json:"border"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
	var panelFound bool
	for _, element := range decoded.Body.Elements {
		if element.Tag != "collapsible_panel" {
			continue
		}
		panelFound = true
		if element.BackgroundColor != "grey" || element.Header.BackgroundColor != "grey" || element.Border.Color != "grey" || element.Border.CornerRadius != "8px" {
			t.Fatalf("collapsible panel = %#v, want documented color/border fields", element)
		}
		if element.Header.Title.Tag != "markdown" || element.Header.Title.Content != "已处理 5m 58s" {
			t.Fatalf("collapsible panel header title = %#v, want markdown title under header", element.Header.Title)
		}
		if element.Header.Icon.Tag != "standard_icon" || element.Header.Icon.Token != "down-small-ccm_outlined" || element.Header.Icon.Size != "16px 16px" || element.Header.IconPosition != "right" || element.Header.IconAngle != -180 {
			t.Fatalf("collapsible panel header icon = %#v position=%q angle=%d, want documented arrow icon", element.Header.Icon, element.Header.IconPosition, element.Header.IconAngle)
		}
	}
	if !panelFound {
		t.Fatalf("card = %s, want collapsible_panel element", card)
	}
}

func TestBuildCodexPanelCardKeepsProgressAndFinalStructure(t *testing.T) {
	t.Parallel()

	card, err := buildRenderedCard(model.RenderedMessage{
		Style:                 model.MessageStyleCodexPanel,
		Text:                  "Running",
		CodexStatus:           "已处理 44s",
		CodexProgressMarkdown: "思考中...\n\n检查状态。\n\n工具调用中...\n\n读取日志。",
		CodexFinalMarkdown:    "最终回复内容。",
		CodexProgressExpanded: false,
	}, [][]model.ButtonSpec{{
		{Text: "Stop", CallbackData: "stop"},
	}})
	if err != nil {
		t.Fatalf("buildRenderedCard failed: %v", err)
	}
	for _, forbidden := range []string{"来自 Codex", "background_style"} {
		if strings.Contains(card, forbidden) {
			t.Fatalf("card contains %q:\n%s", forbidden, card)
		}
	}

	var decoded struct {
		Header map[string]any `json:"header"`
		Body   struct {
			Elements []struct {
				Tag      string `json:"tag"`
				Expanded bool   `json:"expanded"`
				Header   struct {
					Title struct {
						Content string `json:"content"`
					} `json:"title"`
				} `json:"header"`
				Elements []struct {
					Tag     string `json:"tag"`
					Content string `json:"content"`
				} `json:"elements"`
				Content string `json:"content"`
				Columns []struct {
					Elements []struct {
						Tag  string `json:"tag"`
						Text struct {
							Content string `json:"content"`
						} `json:"text"`
					} `json:"elements"`
				} `json:"columns"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
	if decoded.Header != nil {
		t.Fatalf("header = %#v, want Codex panel without a card heading", decoded.Header)
	}
	if len(decoded.Body.Elements) != 3 {
		t.Fatalf("body elements = %#v, want progress panel, final markdown, action row", decoded.Body.Elements)
	}
	panel := decoded.Body.Elements[0]
	if panel.Tag != "collapsible_panel" || panel.Expanded {
		t.Fatalf("first element = %#v, want collapsed progress panel", panel)
	}
	if panel.Header.Title.Content != "已处理 44s" {
		t.Fatalf("panel title = %q, want status text", panel.Header.Title.Content)
	}
	if len(panel.Elements) != 1 || !strings.Contains(panel.Elements[0].Content, "思考中") || !strings.Contains(panel.Elements[0].Content, "工具调用中") {
		t.Fatalf("panel elements = %#v, want progress timeline inside collapsible panel", panel.Elements)
	}
	final := decoded.Body.Elements[1]
	if final.Tag != "markdown" || !strings.Contains(final.Content, "最终回复内容") {
		t.Fatalf("second element = %#v, want final markdown outside progress panel", final)
	}
	actions := decoded.Body.Elements[2]
	if actions.Tag != "column_set" || len(actions.Columns) != 1 {
		t.Fatalf("third element = %#v, want one running button column", actions)
	}
	var labels []string
	for _, column := range actions.Columns {
		for _, element := range column.Elements {
			if element.Tag == "button" {
				labels = append(labels, element.Text.Content)
			}
		}
	}
	if strings.Join(labels, ",") != "Stop" {
		t.Fatalf("button labels = %#v, want Stop", labels)
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

func TestBuildSectionedCardRendersThreadRowsAsInteractiveContainersV2(t *testing.T) {
	t.Parallel()

	card, err := buildSectionedCard([]model.MessageSection{
		{Text: "All chats"},
		{
			Text:    "codex-feishu",
			Heading: true,
			Rows: []model.MessageSectionRow{
				{
					Title:    "Fix topic binding",
					Trailing: "2 小时前",
					Button:   model.ButtonSpec{Text: "Open", CallbackData: "cb_open"},
				},
				{
					Title:           "Second thread",
					Trailing:        "昨天",
					BackgroundStyle: "cus-4",
					BorderColor:     "cus-7",
					Button:          model.ButtonSpec{Text: "Open", CallbackData: "cb_second"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("buildSectionedCard failed: %v", err)
	}
	var decoded struct {
		Schema string `json:"schema"`
		Config struct {
			Style struct {
				Color map[string]struct {
					LightMode string `json:"light_mode"`
				} `json:"color"`
			} `json:"style"`
		} `json:"config"`
		Body struct {
			Elements []struct {
				Tag             string `json:"tag"`
				Content         string `json:"content"`
				TextSize        string `json:"text_size"`
				BackgroundStyle string `json:"background_style"`
				BorderColor     string `json:"border_color"`
				Behaviors       []struct {
					Type  string            `json:"type"`
					Value map[string]string `json:"value"`
				} `json:"behaviors"`
				Elements []struct {
					Tag     string `json:"tag"`
					Columns []struct {
						Elements []struct {
							Tag      string `json:"tag"`
							Content  string `json:"content"`
							TextSize string `json:"text_size"`
						} `json:"elements"`
					} `json:"columns"`
				} `json:"elements"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
	if decoded.Schema != "2.0" {
		t.Fatalf("schema = %q, want 2.0", decoded.Schema)
	}
	if decoded.Config.Style.Color["cus-0"].LightMode != "rgba(247,235,221,1.000000)" {
		t.Fatalf("custom colors = %#v, want #F7EBDD equivalent", decoded.Config.Style.Color)
	}
	elements := decoded.Body.Elements
	if len(elements) != 4 {
		t.Fatalf("elements = %#v, want header, project title, two interactive rows", elements)
	}
	if elements[1].Content != "codex-feishu" || elements[1].TextSize != "xx-large" {
		t.Fatalf("project title = %#v, want styled project heading", elements[1])
	}
	first := elements[2]
	if first.Tag != "interactive_container" || len(first.Behaviors) != 1 || first.Behaviors[0].Type != "callback" || first.Behaviors[0].Value["callback_data"] != "cb_open" {
		t.Fatalf("first row = %#v, want callback interactive container", first)
	}
	if first.BackgroundStyle != "cus-0" || first.BorderColor != "cus-1" {
		t.Fatalf("first row style = %#v, want custom light container", first)
	}
	if len(first.Elements) != 1 || len(first.Elements[0].Columns) != 2 {
		t.Fatalf("first row elements = %#v, want two-column content", first.Elements)
	}
	left := first.Elements[0].Columns[0]
	if len(left.Elements) < 2 || left.Elements[0].Content != "Fix topic binding" || left.Elements[0].TextSize != "heading" || left.Elements[1].Content != "<font color='grey'>2 小时前</font>" {
		t.Fatalf("left column = %#v, want title and grey time", left)
	}
	second := elements[3]
	if second.Tag != "interactive_container" || second.Behaviors[0].Value["callback_data"] != "cb_second" {
		t.Fatalf("second row = %#v, want second callback interactive container", second)
	}
	if second.BackgroundStyle != "cus-4" || second.BorderColor != "cus-7" || decoded.Config.Style.Color["cus-7"].LightMode != "rgba(214,230,207,1.000000)" {
		t.Fatalf("second row style = %#v colors=%#v, want explicit chat border", second, decoded.Config.Style.Color)
	}
}

func TestBuildSectionedCardRendersMetricRowsAsDashboardV2(t *testing.T) {
	t.Parallel()

	card, err := buildSectionedCard([]model.MessageSection{
		{
			Text:    "系统",
			Heading: true,
			Rows: []model.MessageSectionRow{
				{Title: "Core", Trailing: "Ready"},
				{Title: "Uptime", Trailing: "1h 02m"},
				{Title: "Started", Trailing: "2026-06-28 09:00:00"},
			},
		},
		{
			Text:    "Codex",
			Heading: true,
			Rows: []model.MessageSectionRow{
				{Title: "Live session", Trailing: "Online"},
			},
			Buttons: [][]model.ButtonSpec{
				{{Text: "中文", CallbackData: "zh"}, {Text: "English", CallbackData: "en"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("buildSectionedCard failed: %v", err)
	}
	var decoded struct {
		Schema string `json:"schema"`
		Body   struct {
			Elements []struct {
				Tag      string `json:"tag"`
				Content  string `json:"content"`
				TextSize string `json:"text_size"`
				Columns  []struct {
					Width    string `json:"width"`
					Elements []struct {
						Tag             string `json:"tag"`
						BackgroundStyle string `json:"background_style"`
						Behaviors       []any  `json:"behaviors"`
						Elements        []struct {
							Tag      string `json:"tag"`
							Content  string `json:"content"`
							TextSize string `json:"text_size"`
						} `json:"elements"`
					} `json:"elements"`
				} `json:"columns"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
	if decoded.Schema != "2.0" {
		t.Fatalf("schema = %q, want JSON 2.0 dashboard card", decoded.Schema)
	}
	if len(decoded.Body.Elements) != 6 {
		t.Fatalf("elements = %#v, want title, two section headings, KPI rows, and button row", decoded.Body.Elements)
	}
	if decoded.Body.Elements[0].Tag != "markdown" || decoded.Body.Elements[0].Content != "**Codex Status**" || decoded.Body.Elements[0].TextSize != "heading" {
		t.Fatalf("dashboard title = %#v, want card title", decoded.Body.Elements[0])
	}
	for _, element := range decoded.Body.Elements {
		if element.Tag == "action" {
			t.Fatalf("dashboard JSON 2.0 card contains unsupported action element: %#v", decoded.Body.Elements)
		}
	}
	if decoded.Body.Elements[1].Tag != "markdown" || decoded.Body.Elements[1].Content != "**系统**" {
		t.Fatalf("first section heading = %#v, want system heading", decoded.Body.Elements[1])
	}
	firstRow := decoded.Body.Elements[2]
	if firstRow.Tag != "column_set" || len(firstRow.Columns) != 3 {
		t.Fatalf("first KPI row = %#v, want three KPI columns", firstRow)
	}
	firstMetric := firstRow.Columns[0]
	if firstMetric.Width != "weighted" || len(firstMetric.Elements) != 1 || firstMetric.Elements[0].Tag != "interactive_container" || firstMetric.Elements[0].BackgroundStyle != "cus-2" || len(firstMetric.Elements[0].Behaviors) != 0 {
		t.Fatalf("first metric column = %#v, want independent non-clickable KPI container", firstMetric)
	}
	if len(firstMetric.Elements[0].Elements) < 2 || firstMetric.Elements[0].Elements[0].Content != "<font color='grey'>Core</font>" || firstMetric.Elements[0].Elements[1].Content != "Ready" || firstMetric.Elements[0].Elements[1].TextSize != "xx-large" {
		t.Fatalf("first metric content = %#v, want label and large value", firstMetric.Elements[0].Elements)
	}
	if decoded.Body.Elements[3].Tag != "markdown" || decoded.Body.Elements[3].Content != "**Codex**" {
		t.Fatalf("second section heading = %#v, want Codex heading", decoded.Body.Elements[3])
	}
	secondRow := decoded.Body.Elements[4]
	if secondRow.Tag != "column_set" || len(secondRow.Columns) != 1 || len(secondRow.Columns[0].Elements) != 1 || secondRow.Columns[0].Elements[0].BackgroundStyle != "cus-2" {
		t.Fatalf("second KPI row = %#v, want KPI card for lower section", secondRow)
	}
}

func TestBuildSectionedCardRendersChartComponentV2(t *testing.T) {
	t.Parallel()

	card, err := buildSectionedCard([]model.MessageSection{
		{
			Text:    "Thread mix",
			Heading: true,
			Rows: []model.MessageSectionRow{
				{Title: "Projects", Trailing: "60%"},
				{Title: "Chats", Trailing: "40%"},
			},
			Chart: &model.MessageChart{
				ElementID:   "thread_mix",
				AspectRatio: "4:3",
				ColorTheme:  "brand",
				Spec: map[string]any{
					"type": "pie",
					"data": map[string]any{
						"values": []map[string]any{
							{"type": "Projects", "value": 3},
							{"type": "Chats", "value": 2},
						},
					},
					"valueField":    "value",
					"categoryField": "type",
				},
			},
		},
		{
			Text:    "Codex",
			Heading: true,
			Rows: []model.MessageSectionRow{
				{Title: "Live session", Trailing: "Online"},
			},
		},
	})
	if err != nil {
		t.Fatalf("buildSectionedCard failed: %v", err)
	}
	var decoded struct {
		Schema string `json:"schema"`
		Body   struct {
			Elements []struct {
				Tag         string         `json:"tag"`
				ElementID   string         `json:"element_id"`
				AspectRatio string         `json:"aspect_ratio"`
				ColorTheme  string         `json:"color_theme"`
				ChartSpec   map[string]any `json:"chart_spec"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
	if decoded.Schema != "2.0" {
		t.Fatalf("schema = %q, want JSON 2.0 card", decoded.Schema)
	}
	chartIndex := -1
	for index := range decoded.Body.Elements {
		if decoded.Body.Elements[index].Tag == "chart" {
			chartIndex = index
			break
		}
	}
	if chartIndex < 0 {
		t.Fatalf("elements = %#v, want chart component", decoded.Body.Elements)
	}
	chart := decoded.Body.Elements[chartIndex]
	if chart.ElementID != "thread_mix" || chart.AspectRatio != "4:3" || chart.ColorTheme != "brand" || chart.ChartSpec["type"] != "pie" {
		t.Fatalf("chart = %#v, want configured pie chart", chart)
	}
}

func TestBuildSectionedCardAddsDividerBeforeLaterHeadingSections(t *testing.T) {
	t.Parallel()

	card, err := buildSectionedCard([]model.MessageSection{
		{Text: "All chats"},
		{Text: "project-a", Heading: true},
		{Text: "project-b", Heading: true, Divider: true},
	})
	if err != nil {
		t.Fatalf("buildSectionedCard failed: %v", err)
	}
	var decoded struct {
		Elements []struct {
			Tag     string `json:"tag"`
			Content string `json:"content"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(card), &decoded); err != nil {
		t.Fatalf("card JSON invalid: %v", err)
	}
	if len(decoded.Elements) != 4 {
		t.Fatalf("elements = %#v, want header, project-a, divider, project-b", decoded.Elements)
	}
	if decoded.Elements[2].Tag != "hr" {
		t.Fatalf("divider element = %#v, want hr", decoded.Elements[2])
	}
	if decoded.Elements[3].Content != "**<font size=\"x-large\">project-b</font>**" {
		t.Fatalf("project-b title = %q, want x-large bold markdown", decoded.Elements[3].Content)
	}
}

func TestBuildSectionedCardKeepsHelpTextOutOfMetricRows(t *testing.T) {
	t.Parallel()

	card, err := buildSectionedCard([]model.MessageSection{
		{Text: "Common entry points", Heading: true},
		{Rows: []model.MessageSectionRow{
			{Title: "/projects", Trailing: "Choose chats by project", Button: model.ButtonSpec{Text: "Run", CallbackData: "cb_projects"}},
		}},
		{Text: "`/plan <text>`: use inside a Codex chat topic"},
		{Text: "Settings", Heading: true},
		{Rows: []model.MessageSectionRow{
			{Title: "/status", Trailing: "Show service status", Button: model.ButtonSpec{Text: "Run", CallbackData: "cb_status"}},
		}},
	})
	if err != nil {
		t.Fatalf("buildSectionedCard failed: %v", err)
	}
	if got := strings.Count(card, `"tag":"interactive_container"`); got != 2 {
		t.Fatalf("interactive containers = %d, want 2:\n%s", got, card)
	}
	if strings.Contains(card, `"background_style":"cus-2"`) {
		t.Fatalf("card rendered metric/dashboard rows unexpectedly:\n%s", card)
	}
}

func TestWorkspaceMenuShape(t *testing.T) {
	t.Parallel()

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

	if len(menu.ChatMenuTopLevels) != 3 {
		t.Fatalf("top level menu count = %d, want 3", len(menu.ChatMenuTopLevels))
	}
	if got := value(menu.ChatMenuTopLevels[0].ChatMenuItem.Name); got != "会话" {
		t.Fatalf("first menu name = %q, want 会话", got)
	}
	if got := value(menu.ChatMenuTopLevels[0].Children[0].ChatMenuItem.Name); got != "最近会话" {
		t.Fatalf("first child name = %q, want 最近会话", got)
	}
	if got := value(menu.ChatMenuTopLevels[0].Children[0].ChatMenuItem.RedirectLink.CommonUrl); !strings.Contains(got, "command=chats") {
		t.Fatalf("first child URL = %q, want chats command", got)
	}
	if got := value(menu.ChatMenuTopLevels[1].Children[0].ChatMenuItem.RedirectLink.CommonUrl); !strings.Contains(got, "command=new") {
		t.Fatalf("new child URL = %q, want new command", got)
	}
	if got := value(menu.ChatMenuTopLevels[2].Children[1].ChatMenuItem.RedirectLink.CommonUrl); !strings.Contains(got, "command=setting") {
		t.Fatalf("settings child URL = %q, want setting command", got)
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
		"chats":    "/chats",
		"help":     "/help",
		"new":      "/new",
		"projects": "/projects",
		"setting":  "/setting",
		"STATUS":   "/status",
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
	if got, ok := botMenuCommand("newthread"); ok || got != "" {
		t.Fatalf("botMenuCommand(newthread) = %q, %t; want removed", got, ok)
	}
	for _, removed := range []string{"workspace", "home", "threads", "newchat", "language", "lang", "model", "effort"} {
		if got, ok := botMenuCommand(removed); ok || got != "" {
			t.Fatalf("botMenuCommand(%s) = %q, %t; want removed", removed, got, ok)
		}
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

func TestSendRenderedMessagesRendersTextLinkEntitiesAsMarkdownLinks(t *testing.T) {
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
		Text: "Open docs",
		Entities: []model.MessageEntity{{
			Type:   "text_link",
			Offset: 5,
			Length: 4,
			URL:    "https://example.com/docs",
		}},
	}}, nil, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendRenderedMessages failed: %v", err)
	}
	if !strings.Contains(api.lastContent, "[docs](https://example.com/docs)") {
		t.Fatalf("content = %s, want markdown link", api.lastContent)
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
	for _, want := range []string{`"header"`, `"template":"blue"`, "来自 Codex 桌面端用户输入", "Desktop prompt"} {
		if !strings.Contains(api.lastContent, want) {
			t.Fatalf("content missing %q:\n%s", want, api.lastContent)
		}
	}
}

func TestSendRenderedMessagesUploadsAndRendersImage(t *testing.T) {
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
	imagePath := t.TempDir() + "/prompt.jpg"
	if err := os.WriteFile(imagePath, []byte("fake image bytes"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	api := &fakeAPIClient{uploadedImageKey: "img_uploaded"}
	bot := &Bot{store: store, api: api}

	_, err = bot.SendRenderedMessages(context.Background(), chatID, 0, []model.RenderedMessage{{
		Text:      "Desktop image prompt",
		Style:     model.MessageStyleDesktopUser,
		ImagePath: imagePath,
	}}, nil, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendRenderedMessages failed: %v", err)
	}
	if got, want := string(api.uploadedImageData), "fake image bytes"; got != want {
		t.Fatalf("uploaded image data = %q, want %q", got, want)
	}
	for _, want := range []string{`"tag":"img"`, `"img_key":"img_uploaded"`, "Desktop image prompt"} {
		if !strings.Contains(api.lastContent, want) {
			t.Fatalf("content missing %q:\n%s", want, api.lastContent)
		}
	}
}

func TestSendRenderedMessagesFallsBackWhenImageUploadReturnsNoKey(t *testing.T) {
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
	imagePath := t.TempDir() + "/prompt.jpg"
	if err := os.WriteFile(imagePath, []byte("fake image bytes"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	api := &fakeAPIClient{forceEmptyUploadedImageKey: true}
	bot := &Bot{store: store, api: api}

	_, err = bot.SendRenderedMessages(context.Background(), chatID, 0, []model.RenderedMessage{{
		Text:      "Desktop image prompt",
		ImagePath: imagePath,
	}}, nil, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendRenderedMessages failed: %v", err)
	}
	if strings.Contains(api.lastContent, `"tag":"img"`) || strings.Contains(api.lastContent, `"img_key"`) {
		t.Fatalf("content = %s, want no invalid image element", api.lastContent)
	}
	if !strings.Contains(api.lastContent, "Desktop image prompt") {
		t.Fatalf("content = %s, want text fallback", api.lastContent)
	}
}

func TestSendRenderedMessagesSanitizesMarkdownImageSyntax(t *testing.T) {
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
		Text: "Example: `![shot](/tmp/current.png)`",
	}}, nil, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendRenderedMessages failed: %v", err)
	}
	if strings.Contains(api.lastContent, `![shot](/tmp/current.png)`) {
		t.Fatalf("content = %s, want markdown image syntax sanitized", api.lastContent)
	}
	if strings.Contains(api.lastContent, `"tag":"img"`) || strings.Contains(api.lastContent, `"img_key"`) {
		t.Fatalf("content = %s, want no image element", api.lastContent)
	}
	if !strings.Contains(api.lastContent, `[shot](/tmp/current.png)`) {
		t.Fatalf("content = %s, want image syntax rendered as link text", api.lastContent)
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

func TestEnsureThreadTopicRecreatesRootWhenStoredRootBelongsToAnotherChat(t *testing.T) {
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
	otherChatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_other")
	if err != nil {
		t.Fatalf("ResolveExternalID(other chat) failed: %v", err)
	}
	rootMessageID, err := store.ResolveExternalID(ctx, namespaceMessage, "om_other_root")
	if err != nil {
		t.Fatalf("ResolveExternalID(root message) failed: %v", err)
	}
	if err := store.PutFeishuMessageMap(ctx, rootMessageID, "om_other_root", otherChatID, "oc_other"); err != nil {
		t.Fatalf("PutFeishuMessageMap(root) failed: %v", err)
	}
	if err := store.UpsertFeishuThreadTopic(ctx, model.FeishuThreadTopic{
		ChatID:            p2pChatID,
		OpenChatID:        "oc_p2p",
		ThreadID:          "thread-mismatched-root",
		RootMessageID:     rootMessageID,
		RootOpenMessageID: "om_other_root",
	}); err != nil {
		t.Fatalf("UpsertFeishuThreadTopic failed: %v", err)
	}
	if err := store.SetState(ctx, feishuChatModeKey(p2pChatID), "p2p"); err != nil {
		t.Fatalf("SetState(chat mode) failed: %v", err)
	}
	api := &fakeAPIClient{}
	bot := &Bot{store: store, api: api}

	topic, err := bot.EnsureThreadTopic(ctx, p2pChatID, model.Thread{ID: "thread-mismatched-root", Title: "Mismatched"}, nil, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("EnsureThreadTopic failed: %v", err)
	}
	if api.sendCalls != 1 {
		t.Fatalf("sendCalls = %d, want new root message in p2p chat", api.sendCalls)
	}
	if api.lastReceiveID != "oc_p2p" {
		t.Fatalf("send receive id = %q, want oc_p2p", api.lastReceiveID)
	}
	if topic == nil || topic.ChatID != p2pChatID || topic.RootMessageID == rootMessageID {
		t.Fatalf("topic = %#v, want recreated p2p root", topic)
	}
	mapped, err := store.GetFeishuMessageByNumericID(ctx, topic.RootMessageID)
	if err != nil {
		t.Fatalf("GetFeishuMessageByNumericID failed: %v", err)
	}
	if mapped == nil || mapped.ChatID != p2pChatID {
		t.Fatalf("mapped root = %#v, want p2p chat root", mapped)
	}
}

func TestEnsureThreadTopicCreatesP2PThreadReply(t *testing.T) {
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
	if api.createThreadRoomCalls != 0 {
		t.Fatalf("CreateThreadRoom calls = %d, want 0", api.createThreadRoomCalls)
	}
	if api.createWorkspaceMenuCalls != 0 {
		t.Fatalf("CreateWorkspaceMenu calls = %d, want 0", api.createWorkspaceMenuCalls)
	}
	if topic == nil || topic.ChatID != p2pChatID || topic.OpenChatID != "oc_p2p" {
		t.Fatalf("topic = %#v, want p2p chat", topic)
	}
	if api.lastReceiveID != "oc_p2p" {
		t.Fatalf("send receive id = %q, want oc_p2p", api.lastReceiveID)
	}
	if api.replyCalls != 1 {
		t.Fatalf("replyCalls = %d, want activation reply", api.replyCalls)
	}
	if api.lastOpenMessageID != "om_fake" || api.lastMsgType != "text" || !api.lastReplyInThread {
		t.Fatalf("activation reply target=%q type=%q inThread=%v, want om_fake text true", api.lastOpenMessageID, api.lastMsgType, api.lastReplyInThread)
	}
	if got := parseTextContent("text", api.lastContent); got != `<at user_id="ou_user">你</at> 已打开 Codex 会话话题` {
		t.Fatalf("activation content = %q, want mention", got)
	}
	topic, err = bot.EnsureThreadTopic(ctx, p2pChatID, model.Thread{ID: "thread-from-dm", Title: "From DM"}, nil, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("EnsureThreadTopic second call failed: %v", err)
	}
	if topic == nil || topic.ChatID != p2pChatID {
		t.Fatalf("second topic = %#v, want p2p chat", topic)
	}
	if api.replyCalls != 1 {
		t.Fatalf("replyCalls after second call = %d, want no duplicate activation", api.replyCalls)
	}
	topic, err = bot.EnsureThreadTopic(model.WithSilentThreadTopicActivation(model.WithForcedThreadTopicActivation(ctx)), p2pChatID, model.Thread{ID: "thread-from-dm", Title: "From DM"}, nil, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("EnsureThreadTopic silent activation failed: %v", err)
	}
	if topic == nil || topic.ChatID != p2pChatID {
		t.Fatalf("silent topic = %#v, want p2p chat", topic)
	}
	if api.replyCalls != 1 {
		t.Fatalf("replyCalls after silent activation = %d, want no activation reply", api.replyCalls)
	}
	topic, err = bot.EnsureThreadTopic(model.WithForcedThreadTopicActivation(ctx), p2pChatID, model.Thread{ID: "thread-from-dm", Title: "From DM"}, nil, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("EnsureThreadTopic forced activation failed: %v", err)
	}
	if topic == nil || topic.ChatID != p2pChatID {
		t.Fatalf("forced topic = %#v, want p2p chat", topic)
	}
	if api.replyCalls != 2 {
		t.Fatalf("replyCalls after forced activation = %d, want duplicate activation for user open", api.replyCalls)
	}
	if api.createWorkspaceMenuCalls != 0 {
		t.Fatalf("CreateWorkspaceMenu calls after second call = %d, want no menu setup", api.createWorkspaceMenuCalls)
	}
	storedRoom, err := store.GetState(ctx, feishuControlRoomKey(p2pChatID))
	if err != nil {
		t.Fatalf("GetState(control room) failed: %v", err)
	}
	if storedRoom != "" {
		t.Fatalf("control room state = %q, want empty", storedRoom)
	}
}

func TestEnsureThreadTopicReusesExistingTopicAcrossChats(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	existingChatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_existing")
	if err != nil {
		t.Fatalf("ResolveExternalID(existing chat) failed: %v", err)
	}
	sourceChatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_source")
	if err != nil {
		t.Fatalf("ResolveExternalID(source chat) failed: %v", err)
	}
	rootMessageID, err := store.ResolveExternalID(ctx, namespaceMessage, "om_existing_root")
	if err != nil {
		t.Fatalf("ResolveExternalID(root message) failed: %v", err)
	}
	if err := store.PutFeishuMessageMap(ctx, rootMessageID, "om_existing_root", existingChatID, "oc_existing"); err != nil {
		t.Fatalf("PutFeishuMessageMap(root) failed: %v", err)
	}
	if err := store.SetState(ctx, feishuChatModeKey(sourceChatID), "group"); err != nil {
		t.Fatalf("SetState(source chat mode) failed: %v", err)
	}
	if err := store.SetState(ctx, feishuControlUserKey(sourceChatID), "ou_user"); err != nil {
		t.Fatalf("SetState(control user) failed: %v", err)
	}
	if err := store.UpsertFeishuThreadTopic(ctx, model.FeishuThreadTopic{
		ChatID:            existingChatID,
		OpenChatID:        "oc_existing",
		ThreadID:          "thread-cross-chat",
		RootMessageID:     rootMessageID,
		RootOpenMessageID: "om_existing_root",
		FeishuThreadID:    "othread_existing",
	}); err != nil {
		t.Fatalf("UpsertFeishuThreadTopic failed: %v", err)
	}
	api := &fakeAPIClient{}
	bot := &Bot{store: store, api: api}

	topic, err := bot.EnsureThreadTopic(ctx, sourceChatID, model.Thread{ID: "thread-cross-chat", Title: "Cross Chat"}, nil, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("EnsureThreadTopic failed: %v", err)
	}
	if topic == nil || topic.ChatID != existingChatID || topic.RootMessageID != rootMessageID {
		t.Fatalf("topic = %#v, want existing cross-chat topic", topic)
	}
	if api.sendCalls != 0 {
		t.Fatalf("sendCalls = %d, want no duplicate root card", api.sendCalls)
	}
	if api.replyCalls != 1 || api.lastOpenMessageID != "om_existing_root" || !api.lastReplyInThread {
		t.Fatalf("activation reply calls=%d target=%q inThread=%v, want existing topic activation", api.replyCalls, api.lastOpenMessageID, api.lastReplyInThread)
	}
}

func TestEnsureThreadTopicDoesNotReuseControlRoomTopicAcrossChats(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	controlRoomChatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_control_room")
	if err != nil {
		t.Fatalf("ResolveExternalID(control room) failed: %v", err)
	}
	p2pChatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_p2p")
	if err != nil {
		t.Fatalf("ResolveExternalID(p2p chat) failed: %v", err)
	}
	rootMessageID, err := store.ResolveExternalID(ctx, namespaceMessage, "om_control_room_root")
	if err != nil {
		t.Fatalf("ResolveExternalID(root message) failed: %v", err)
	}
	if err := store.PutFeishuMessageMap(ctx, rootMessageID, "om_control_room_root", controlRoomChatID, "oc_control_room"); err != nil {
		t.Fatalf("PutFeishuMessageMap(root) failed: %v", err)
	}
	if err := store.SetState(ctx, feishuChatModeKey(p2pChatID), "p2p"); err != nil {
		t.Fatalf("SetState(p2p chat mode) failed: %v", err)
	}
	if err := store.SetState(ctx, feishuControlUserKey(p2pChatID), "ou_user"); err != nil {
		t.Fatalf("SetState(control user) failed: %v", err)
	}
	if err := botRememberControlRoomSourceForTest(ctx, store, "oc_control_room", p2pChatID); err != nil {
		t.Fatalf("remember control room source failed: %v", err)
	}
	if err := store.UpsertFeishuThreadTopic(ctx, model.FeishuThreadTopic{
		ChatID:            controlRoomChatID,
		OpenChatID:        "oc_control_room",
		ThreadID:          "thread-control-room",
		RootMessageID:     rootMessageID,
		RootOpenMessageID: "om_control_room_root",
		FeishuThreadID:    "othread_control_room",
	}); err != nil {
		t.Fatalf("UpsertFeishuThreadTopic failed: %v", err)
	}
	api := &fakeAPIClient{}
	bot := &Bot{cfg: config.Config{FeishuAppID: "cli_app"}, store: store, api: api}

	topic, err := bot.EnsureThreadTopic(ctx, p2pChatID, model.Thread{ID: "thread-control-room", Title: "Control Room"}, nil, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("EnsureThreadTopic failed: %v", err)
	}
	if topic == nil || topic.ChatID != p2pChatID || topic.RootMessageID == rootMessageID {
		t.Fatalf("topic = %#v, want new p2p topic instead of control room topic", topic)
	}
	if api.sendCalls != 1 {
		t.Fatalf("sendCalls = %d, want new p2p root card", api.sendCalls)
	}
	if api.replyCalls != 1 || api.lastOpenMessageID != "om_fake" {
		t.Fatalf("activation reply calls=%d target=%q, want activation on new p2p root", api.replyCalls, api.lastOpenMessageID)
	}
}

func botRememberControlRoomSourceForTest(ctx context.Context, store *storage.Store, roomOpenChatID string, p2pChatID int64) error {
	sourceOpenChatID, err := store.ExternalIDForNumeric(ctx, namespaceChat, p2pChatID)
	if err != nil {
		return err
	}
	return store.SetState(ctx, feishuControlRoomSourceKey(roomOpenChatID), fmt.Sprintf("%d|%s", p2pChatID, sourceOpenChatID))
}

func TestRenderThreadTopicRootTextUsesOnlyTitle(t *testing.T) {
	t.Parallel()

	longTitle := "实现飞书版本的 codex-feishu，要求体验丝滑，功能完善，而且标题不能被截断"
	got := renderThreadTopicRootText(model.Thread{
		ID:          "thread-title",
		Title:       longTitle,
		ProjectName: "codex-feishu-controller",
		Status:      "active",
	}, &appserver.ThreadReadSnapshot{LatestTurnID: "turn-123456789", LatestTurnStatus: "inProgress"}, model.PanelSourceFeishuInput)

	if got != longTitle+"\n\n这个话题对应一个 Codex 会话；在这里直接回复会继续该会话。" {
		t.Fatalf("root text = %q, want title only", got)
	}
	for _, unwanted := range []string{"## Codex 会话", "Thread:", "Project:", "codex-feishu-controller", "inProgress", "Feishu", "turn-123"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("root text = %q, want no %q", got, unwanted)
		}
	}
}

func TestRenderThreadTopicRootTextDoesNotIncludeCWDBasename(t *testing.T) {
	t.Parallel()

	got := renderThreadTopicRootText(model.Thread{
		ID:     "thread-cwd",
		Title:  "Use cwd project",
		CWD:    "/Users/example/workspace/codex-feishu-controller",
		Status: "idle",
	}, nil, model.PanelSourceFeishuInput)

	if got != "Use cwd project\n\n这个话题对应一个 Codex 会话；在这里直接回复会继续该会话。" {
		t.Fatalf("root text = %q, want title only", got)
	}
	if strings.Contains(got, "codex-feishu-controller") || strings.Contains(got, "Project:") {
		t.Fatalf("root text = %q, want no cwd/project metadata", got)
	}
}

func TestRenderThreadTopicRootTextFallsBackToLastPreview(t *testing.T) {
	t.Parallel()

	got := renderThreadTopicRootText(model.Thread{
		ID:          "thread-preview",
		LastPreview: "测试一下项目里的新会话",
		ProjectName: "codex-tg-controller",
		Status:      "inProgress",
	}, &appserver.ThreadReadSnapshot{LatestTurnID: "turn-123456789", LatestTurnStatus: "inProgress"}, model.PanelSourceFeishuInput)

	if got != "测试一下项目里的新会话\n\n这个话题对应一个 Codex 会话；在这里直接回复会继续该会话。" {
		t.Fatalf("root text = %q, want first prompt preview only", got)
	}
	if strings.Contains(got, "New thread") || strings.Contains(got, "Project:") || strings.Contains(got, "codex-tg-controller") {
		t.Fatalf("root text = %q, want no placeholder or project metadata", got)
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

func TestSettingsCardActionReturnsNoToastAfterEdit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "data", "state.sqlite")
	cfg := config.Config{
		Paths: config.Paths{
			Home:    root,
			DataDir: filepath.Join(root, "data"),
			LogDir:  filepath.Join(root, "logs"),
			DBPath:  dbPath,
		},
	}
	service, err := daemon.New(cfg)
	if err != nil {
		t.Fatalf("daemon.New failed: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("storage.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SetState(ctx, feishuBotLanguageKey, "en"); err != nil {
		t.Fatalf("SetState(language) failed: %v", err)
	}
	api := &fakeAPIClient{chatModes: map[string]string{"oc_chat": "p2p"}}
	bot := &Bot{store: store, api: api, service: service, logger: log.New(io.Discard, "", 0)}
	service.SetSender(bot)
	chatID, err := store.ResolveExternalID(ctx, namespaceChat, "oc_chat")
	if err != nil {
		t.Fatalf("ResolveExternalID(chat) failed: %v", err)
	}
	userID, err := store.ResolveExternalID(ctx, namespaceUser, "ou_user")
	if err != nil {
		t.Fatalf("ResolveExternalID(user) failed: %v", err)
	}
	if chatID == 0 || userID == 0 {
		t.Fatalf("chatID=%d userID=%d, want resolved IDs", chatID, userID)
	}
	response, err := service.HandleMessageFromSource(ctx, chatID, 0, userID, "/setting", 0, model.PanelSourceFeishuInput)
	if err != nil {
		t.Fatalf("HandleMessageFromSource(/setting) failed: %v", err)
	}
	if response.SettingsForm == nil || response.SettingsForm.SubmitToken == "" {
		t.Fatalf("settings response = %#v, want settings form", response)
	}
	cardResponse, err := bot.handleCardAction(ctx, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Context:  &callback.Context{OpenChatID: "oc_chat", OpenMessageID: "om_settings"},
			Operator: &callback.Operator{OpenID: "ou_user"},
			Action: &callback.CallBackAction{
				Value: map[string]interface{}{"callback_data": response.SettingsForm.SubmitToken},
				FormValue: map[string]interface{}{
					"model":     "",
					"reasoning": "low",
					"language":  "en",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction failed: %v", err)
	}
	if cardResponse != nil {
		t.Fatalf("card response = %#v, want no toast after settings edit", cardResponse)
	}
	if api.patchCardCalls != 1 || !strings.Contains(api.lastCard, "Settings applied.") || !strings.Contains(api.lastCard, "Reasoning effort: low") {
		t.Fatalf("patched card calls=%d card=%s, want applied settings summary", api.patchCardCalls, api.lastCard)
	}
	if api.sendCalls != 0 {
		t.Fatalf("sendCalls = %d, want no fallback message", api.sendCalls)
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
	bot := &Bot{store: store, service: service, api: api, logger: log.New(io.Discard, "", 0)}
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

func TestTopicHelpRepliesInThread(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
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
	store := service.Store()
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
	api := &fakeAPIClient{}
	bot := &Bot{store: store, service: service, api: api, logger: log.New(io.Discard, "", 0)}
	event := newTextMessageEvent("oc_topic_group", "om_help", "ou_user", "/help")
	event.Event.Message.RootId = ptrString("om_root")
	event.Event.Message.ThreadId = ptrString("omt_topic")

	if err := bot.handleMessageEvent(ctx, event); err != nil {
		t.Fatalf("handleMessageEvent failed: %v", err)
	}
	if api.replyCalls != 1 || api.sendCalls != 0 {
		t.Fatalf("replyCalls=%d sendCalls=%d, want one thread reply", api.replyCalls, api.sendCalls)
	}
	if api.lastOpenMessageID != "om_root" || !api.lastReplyInThread {
		t.Fatalf("reply target=%q inThread=%v, want om_root true", api.lastOpenMessageID, api.lastReplyInThread)
	}
	if api.lastMsgType != "interactive" {
		t.Fatalf("msgType = %q, want interactive", api.lastMsgType)
	}
	for _, want := range []string{"/plan", "/goal", "/stop"} {
		if !strings.Contains(api.lastContent, want) {
			t.Fatalf("topic help card missing %q:\n%s", want, api.lastContent)
		}
	}
	for _, hidden := range []string{"/projects", "/new"} {
		if strings.Contains(api.lastContent, hidden) {
			t.Fatalf("topic help card exposes workspace command %q:\n%s", hidden, api.lastContent)
		}
	}
}

func TestHelpSendRebuildsAPIClientAfterAuthError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
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
	store := service.Store()
	firstAPI := &fakeAPIClient{sendErrs: []error{fmt.Errorf("feishu send message failed: code=99991663: Invalid access token for authorization")}}
	secondAPI := &fakeAPIClient{}
	rebuilds := 0
	bot := &Bot{store: store, service: service, api: firstAPI, logger: log.New(io.Discard, "", 0)}
	bot.apiNew = func() apiClient {
		rebuilds++
		return secondAPI
	}
	event := newTextMessageEvent("oc_help", "om_help", "ou_user", "/help")

	if err := bot.handleMessageEvent(ctx, event); err != nil {
		t.Fatalf("handleMessageEvent failed: %v", err)
	}
	if rebuilds != 1 {
		t.Fatalf("rebuilds = %d, want 1", rebuilds)
	}
	if firstAPI.sendCalls != 1 {
		t.Fatalf("firstAPI.sendCalls = %d, want failed first send", firstAPI.sendCalls)
	}
	if secondAPI.refreshAuthCalls != 1 {
		t.Fatalf("secondAPI.refreshAuthCalls = %d, want 1", secondAPI.refreshAuthCalls)
	}
	if secondAPI.sendCalls != 1 || secondAPI.lastMsgType != "interactive" {
		t.Fatalf("secondAPI sendCalls=%d msgType=%q, want one interactive retry", secondAPI.sendCalls, secondAPI.lastMsgType)
	}
	if !strings.Contains(secondAPI.lastContent, "/projects") || !strings.Contains(secondAPI.lastContent, "/setting") {
		t.Fatalf("help card missing workspace commands:\n%s", secondAPI.lastContent)
	}
}

func TestHelpSendReturnsRefreshErrorAfterAuthError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
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
	store := service.Store()
	firstAPI := &fakeAPIClient{sendErrs: []error{fmt.Errorf("feishu send message failed: code=99991663: Invalid access token for authorization")}}
	secondAPI := &fakeAPIClient{refreshAuthErr: fmt.Errorf("refresh tenant access token: rejected")}
	bot := &Bot{store: store, service: service, api: firstAPI, logger: log.New(io.Discard, "", 0)}
	bot.apiNew = func() apiClient {
		return secondAPI
	}
	event := newTextMessageEvent("oc_help", "om_help", "ou_user", "/help")

	err = bot.handleMessageEvent(ctx, event)
	if err == nil || !strings.Contains(err.Error(), "refresh tenant access token") {
		t.Fatalf("handleMessageEvent error = %v, want refresh failure", err)
	}
	if secondAPI.refreshAuthCalls != 1 {
		t.Fatalf("secondAPI.refreshAuthCalls = %d, want 1", secondAPI.refreshAuthCalls)
	}
	if secondAPI.sendCalls != 0 {
		t.Fatalf("secondAPI.sendCalls = %d, want no retry after refresh failure", secondAPI.sendCalls)
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
	if !isFeishuCommand(" /chats") {
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

func newImageMessageEvent(openChatID, openMessageID, openUserID, imageKey string) *larkim.P2MessageReceiveV1 {
	messageType := "image"
	content, err := json.Marshal(imageContent{ImageKey: imageKey})
	if err != nil {
		panic(err)
	}
	contentText := string(content)
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: &openUserID},
			},
			Message: &larkim.EventMessage{
				ChatId:      &openChatID,
				MessageId:   &openMessageID,
				MessageType: &messageType,
				Content:     &contentText,
			},
		},
	}
}

func newPostImageMessageEvent(openChatID, openMessageID, openUserID, text, imageKey string) *larkim.P2MessageReceiveV1 {
	messageType := "post"
	content, err := json.Marshal(postContent{Post: map[string]postLanguageContent{
		"zh_cn": {
			Content: [][]postElement{{
				{Tag: "text", Text: text},
				{Tag: "img", ImageKey: imageKey},
			}},
		},
	}})
	if err != nil {
		panic(err)
	}
	contentText := string(content)
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: &openUserID},
			},
			Message: &larkim.EventMessage{
				ChatId:      &openChatID,
				MessageId:   &openMessageID,
				MessageType: &messageType,
				Content:     &contentText,
			},
		},
	}
}

func newDirectPostImageMessageEvent(openChatID, openMessageID, openUserID, text, imageKey string) *larkim.P2MessageReceiveV1 {
	messageType := "post"
	content, err := json.Marshal(postLanguageContent{
		Content: [][]postElement{{
			{Tag: "text", Text: text},
			{Tag: "img", ImageKey: imageKey},
		}},
	})
	if err != nil {
		panic(err)
	}
	contentText := string(content)
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: &openUserID},
			},
			Message: &larkim.EventMessage{
				ChatId:      &openChatID,
				MessageId:   &openMessageID,
				MessageType: &messageType,
				Content:     &contentText,
			},
		},
	}
}

func ptrString(value string) *string {
	return &value
}

type fakeWSClient struct {
	started chan struct{}
	err     error
	starts  int
}

func (f *fakeWSClient) Start(ctx context.Context) error {
	f.starts++
	if f.started != nil {
		close(f.started)
	}
	if f.err != nil {
		return f.err
	}
	<-ctx.Done()
	return ctx.Err()
}

type fakeAPIClient struct {
	lastMsgType                string
	lastContent                string
	lastReceiveID              string
	lastOpenMessageID          string
	lastText                   string
	lastCard                   string
	lastReplyInThread          bool
	lastRoomName               string
	lastRoomUserOpenIDs        []string
	updateTextCalls            int
	patchCardCalls             int
	createWorkspaceMenuCalls   int
	sendCalls                  int
	replyCalls                 int
	deleteCalls                int
	createThreadRoomCalls      int
	refreshAuthCalls           int
	chatModes                  map[string]string
	downloadedImages           map[string][]byte
	downloadImageMessageIDs    []string
	downloadImageKeys          []string
	uploadedImageData          []byte
	uploadedImageKey           string
	forceEmptyUploadedImageKey bool
	sendErrs                   []error
	replyErrs                  []error
	refreshAuthErr             error
}

func (f *fakeAPIClient) RefreshAuth(context.Context) error {
	f.refreshAuthCalls++
	return f.refreshAuthErr
}

func (f *fakeAPIClient) Send(_ context.Context, receiveID, msgType, content string) (sentMessage, error) {
	f.sendCalls++
	f.lastReceiveID = receiveID
	f.lastMsgType = msgType
	f.lastContent = content
	if len(f.sendErrs) > 0 {
		err := f.sendErrs[0]
		f.sendErrs = f.sendErrs[1:]
		return sentMessage{}, err
	}
	return sentMessage{OpenMessageID: "om_fake", OpenChatID: receiveID}, nil
}

func (f *fakeAPIClient) SendToOpenID(_ context.Context, _, msgType, content string) (sentMessage, error) {
	f.sendCalls++
	f.lastMsgType = msgType
	f.lastContent = content
	if len(f.sendErrs) > 0 {
		err := f.sendErrs[0]
		f.sendErrs = f.sendErrs[1:]
		return sentMessage{}, err
	}
	return sentMessage{OpenMessageID: "om_fake", OpenChatID: "oc_fake"}, nil
}

func (f *fakeAPIClient) Reply(_ context.Context, openMessageID, msgType, content string, replyInThread bool) (sentMessage, error) {
	f.replyCalls++
	f.lastOpenMessageID = openMessageID
	f.lastMsgType = msgType
	f.lastContent = content
	f.lastReplyInThread = replyInThread
	if len(f.replyErrs) > 0 {
		err := f.replyErrs[0]
		f.replyErrs = f.replyErrs[1:]
		return sentMessage{}, err
	}
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

func (f *fakeAPIClient) CreateWorkspaceMenu(context.Context, string) error {
	f.createWorkspaceMenuCalls++
	return nil
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

func (f *fakeAPIClient) UploadImage(_ context.Context, data []byte) (string, error) {
	f.uploadedImageData = append([]byte(nil), data...)
	if f.forceEmptyUploadedImageKey {
		return "", nil
	}
	if f.uploadedImageKey != "" {
		return f.uploadedImageKey, nil
	}
	return "img_fake", nil
}

func (f *fakeAPIClient) DownloadImage(_ context.Context, openMessageID, imageKey string) ([]byte, error) {
	f.downloadImageMessageIDs = append(f.downloadImageMessageIDs, openMessageID)
	f.downloadImageKeys = append(f.downloadImageKeys, imageKey)
	if f.downloadedImages != nil {
		if data, ok := f.downloadedImages[imageKey]; ok {
			return data, nil
		}
	}
	return nil, fmt.Errorf("image %s not found", imageKey)
}
