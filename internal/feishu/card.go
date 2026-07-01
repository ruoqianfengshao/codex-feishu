package feishu

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/mideco-tech/codex-tg/internal/model"
)

const (
	maxCardButtonsPerRow = 3
	threadRowSeparator   = "╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌"
)

var markdownImagePattern = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]*)\)`)

func buildCard(text string, buttons [][]model.ButtonSpec) (string, error) {
	return buildSectionedCard([]model.MessageSection{{
		Text:    text,
		Buttons: buttons,
	}})
}

func buildRenderedCard(message model.RenderedMessage, buttons [][]model.ButtonSpec) (string, error) {
	if strings.TrimSpace(message.ImageKey) != "" {
		return buildRenderedCardWithImage(message, buttons)
	}
	message.ImagePath = ""
	if strings.TrimSpace(message.Style) == model.MessageStyleCodexPanel {
		return buildCodexPanelCard(message, buttons)
	}
	return buildCardWithStyle(renderPlainText(message), buttons, message.Style)
}

func buildCodexPanelCard(message model.RenderedMessage, buttons [][]model.ButtonSpec) (string, error) {
	status := strings.TrimSpace(message.CodexStatus)
	if status == "" {
		status = strings.TrimSpace(message.Text)
	}
	if status == "" {
		status = " "
	}
	progress := strings.TrimSpace(message.CodexProgressMarkdown)
	final := strings.TrimSpace(message.CodexFinalMarkdown)
	elements := []map[string]any{}
	if progress == "" {
		progress = status
	}
	elements = append(elements, collapsiblePanelElement(status, progress, message.CodexProgressExpanded))
	if final != "" {
		elements = append(elements, markdownElementV2(final))
	}
	elements = append(elements, buttonElementsV2(buttons)...)
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"width_mode": "fill",
		},
		"body": map[string]any{
			"direction":        "vertical",
			"padding":          "12px 12px 12px 12px",
			"vertical_spacing": "8px",
			"elements":         elements,
		},
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

func collapsiblePanelElement(title, content string, expanded bool) map[string]any {
	title = strings.TrimSpace(title)
	if title == "" {
		title = " "
	}
	content = strings.TrimSpace(content)
	if content == "" {
		content = " "
	}
	return map[string]any{
		"tag":              "collapsible_panel",
		"expanded":         expanded,
		"background_color": "grey",
		"padding":          "8px 10px 8px 10px",
		"header": map[string]any{
			"title":            markdownElementV2(title),
			"background_color": "grey",
			"vertical_align":   "center",
			"icon": map[string]any{
				"tag":   "standard_icon",
				"token": "down-small-ccm_outlined",
				"color": "",
				"size":  "16px 16px",
			},
			"icon_position":       "right",
			"icon_expanded_angle": -180,
		},
		"border": map[string]any{
			"color":         "grey",
			"corner_radius": "8px",
		},
		"elements": []map[string]any{
			markdownElementV2(content),
		},
	}
}

func buildRenderedCardWithImage(message model.RenderedMessage, buttons [][]model.ButtonSpec) (string, error) {
	elements := []map[string]any{}
	if text := strings.TrimSpace(renderPlainText(message)); text != "" {
		elements = append(elements, markdownElementV2(text))
	}
	elements = append(elements, imageElementV2(message.ImageKey))
	elements = append(elements, buttonElementsV2(buttons)...)
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"width_mode": "fill",
		},
		"body": map[string]any{
			"direction":        "vertical",
			"padding":          "12px 12px 12px 12px",
			"vertical_spacing": "8px",
			"elements":         elements,
		},
	}
	if header := cardHeaderForStyle(message.Style); header != nil {
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
	if sectionedCardHasRows(sections) {
		return buildSectionedCardV2(sections, style)
	}
	return buildSectionedCardV1WithStyle(sections, style)
}

func buildSectionedCardV1(sections []model.MessageSection) (string, error) {
	return buildSectionedCardV1WithStyle(sections, "")
}

func buildSectionedCardV1WithStyle(sections []model.MessageSection, style string) (string, error) {
	if len(sections) == 0 {
		sections = []model.MessageSection{{Text: " "}}
	}
	elements := make([]map[string]any, 0, len(sections)*2)
	for _, section := range sections {
		if section.Divider {
			elements = append(elements, dividerElement())
		}
		text := strings.TrimSpace(section.Text)
		if text != "" {
			if section.Heading {
				elements = append(elements, markdownElement(fmt.Sprintf("**<font size=\"x-large\">%s</font>**", text)))
			} else {
				elements = append(elements, markdownElement(text))
			}
		}
		elements = append(elements, rowElements(section.Rows)...)
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

func sectionedCardHasRows(sections []model.MessageSection) bool {
	for _, section := range sections {
		if len(section.Rows) > 0 {
			return true
		}
	}
	return false
}

func buildSectionedCardV2(sections []model.MessageSection, style string) (string, error) {
	elements := make([]map[string]any, 0, len(sections)*3)
	if sectionsLookLikeDashboard(sections) {
		elements = dashboardCardElements(sections)
	} else {
		for _, section := range sections {
			if section.Divider {
				elements = append(elements, dividerElement())
			}
			if sectionHasMetricRows(section) {
				elements = append(elements, dashboardSectionElements(section)...)
				elements = append(elements, buttonElementsV2(section.Buttons)...)
				continue
			}
			text := strings.TrimSpace(section.Text)
			if text != "" {
				if section.Heading {
					elements = append(elements, projectHeadingElement(text))
				} else {
					elements = append(elements, markdownElementV2(text))
				}
			}
			elements = append(elements, rowElementsV2(section.Rows)...)
			elements = append(elements, buttonElementsV2(section.Buttons)...)
		}
	}
	if len(elements) == 0 {
		elements = append(elements, markdownElementV2(" "))
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"width_mode": "fill",
			"style": map[string]any{
				"color": map[string]any{
					"cus-0": map[string]any{
						"light_mode": "rgba(230,241,251,1.000000)",
						"dark_mode":  "rgba(32,54,73,1.000000)",
					},
					"cus-1": map[string]any{
						"light_mode": "rgba(184,216,240,1.000000)",
						"dark_mode":  "rgba(55,88,115,1.000000)",
					},
					"cus-2": map[string]any{
						"light_mode": "rgba(246,247,249,1.000000)",
						"dark_mode":  "rgba(42,45,50,1.000000)",
					},
					"cus-3": map[string]any{
						"light_mode": "rgba(255,224,224,1.000000)",
						"dark_mode":  "rgba(83,43,43,1.000000)",
					},
				},
			},
		},
		"body": map[string]any{
			"direction":        "vertical",
			"padding":          "12px 12px 12px 12px",
			"vertical_spacing": "8px",
			"elements":         elements,
		},
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
				"content": "来自 Codex 桌面端用户输入",
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
	text = sanitizeMarkdownImages(text)
	return map[string]any{
		"tag":     "markdown",
		"content": text,
	}
}

func markdownElementV2(text string) map[string]any {
	text = strings.TrimSpace(text)
	if text == "" {
		text = " "
	}
	text = sanitizeMarkdownImages(text)
	return map[string]any{
		"tag":     "markdown",
		"content": text,
	}
}

func sanitizeMarkdownImages(text string) string {
	return markdownImagePattern.ReplaceAllString(text, `[$1]($2)`)
}

func plainTextElementV2(text string) map[string]any {
	text = strings.TrimSpace(text)
	if text == "" {
		text = " "
	}
	return map[string]any{
		"tag":     "plain_text",
		"content": text,
	}
}

func projectHeadingElement(text string) map[string]any {
	element := markdownElementV2(text)
	element["text_size"] = "xx-large"
	element["margin"] = "8px 0px 6px 0px"
	return element
}

func imageElementV2(imageKey string) map[string]any {
	return map[string]any{
		"tag":     "img",
		"img_key": strings.TrimSpace(imageKey),
		"alt": map[string]any{
			"tag":     "plain_text",
			"content": "Image",
		},
		"mode":    "fit_horizontal",
		"preview": true,
	}
}

func markdownElementWithMargin(text, margin string) map[string]any {
	element := markdownElement(text)
	if strings.TrimSpace(margin) != "" {
		element["margin"] = margin
	}
	return element
}

func dividerElement() map[string]any {
	return map[string]any{"tag": "hr"}
}

func rowElementsV2(rows []model.MessageSectionRow) []map[string]any {
	if len(rows) == 0 {
		return nil
	}
	if rowsAreInteractive(rows) {
		return interactiveRowElements(rows)
	}
	return metricRowElements(rows)
}

func rowsAreInteractive(rows []model.MessageSectionRow) bool {
	for _, row := range rows {
		if strings.TrimSpace(row.Button.CallbackData) != "" {
			return true
		}
	}
	return false
}

func sectionsLookLikeDashboard(sections []model.MessageSection) bool {
	if len(sections) < 2 {
		return false
	}
	for _, section := range sections {
		if len(section.Rows) == 0 || rowsAreInteractive(section.Rows) {
			return false
		}
	}
	return true
}

func dashboardCardElements(sections []model.MessageSection) []map[string]any {
	elements := []map[string]any{
		mapWith(mapWith(markdownElementV2("**Codex Status**"), "text_size", "heading"), "margin", "2px 0px 12px 0px"),
	}
	for _, section := range sections {
		elements = append(elements, dashboardKPISectionElements(section)...)
		elements = append(elements, buttonElementsV2(section.Buttons)...)
	}
	return elements
}

func dashboardKPISectionElements(section model.MessageSection) []map[string]any {
	title := strings.TrimSpace(section.Text)
	elements := make([]map[string]any, 0, 1+(len(section.Rows)+2)/3)
	if title != "" {
		elements = append(elements, mapWith(mapWith(markdownElementV2("**"+title+"**"), "text_size", "heading"), "margin", "8px 0px 8px 0px"))
	}
	for start := 0; start < len(section.Rows); start += 3 {
		end := min(start+3, len(section.Rows))
		elements = append(elements, dashboardKPIRow(section.Rows[start:end]))
	}
	return elements
}

func dashboardKPIRow(rows []model.MessageSectionRow) map[string]any {
	columns := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		title := strings.TrimSpace(row.Title)
		if title == "" {
			title = " "
		}
		value := strings.TrimSpace(row.Trailing)
		if value == "" {
			value = " "
		}
		columns = append(columns, map[string]any{
			"tag":            "column",
			"width":          "weighted",
			"weight":         1,
			"vertical_align": "top",
			"elements": []map[string]any{{
				"tag":              "interactive_container",
				"width":            "fill",
				"background_style": "cus-2",
				"corner_radius":    "8px",
				"padding":          "14px 10px 14px 10px",
				"vertical_spacing": "6px",
				"elements": []map[string]any{
					markdownElementV2(fmt.Sprintf("<font color='grey'>%s</font>", title)),
					mapWith(markdownElementV2(value), "text_size", "xx-large"),
				},
			}},
		})
	}
	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          "none",
		"horizontal_spacing": "10px",
		"vertical_align":     "top",
		"margin":             "0px 0px 10px 0px",
		"columns":            columns,
	}
}

func sectionHasMetricRows(section model.MessageSection) bool {
	return len(section.Rows) > 0 && !rowsAreInteractive(section.Rows)
}

func dashboardSectionElements(section model.MessageSection) []map[string]any {
	elements := make([]map[string]any, 0, 1+(len(section.Rows)+1)/2)
	text := strings.TrimSpace(section.Text)
	if text != "" {
		header := plainTextElementV2(text)
		header["text_size"] = "heading"
		header["text_color"] = "default"
		header["margin"] = "4px 0px 2px 0px"
		elements = append(elements, header)
	}
	elements = append(elements, metricRowElements(section.Rows)...)
	return elements
}

func interactiveRowElements(rows []model.MessageSectionRow) []map[string]any {
	elements := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		title := strings.TrimSpace(row.Title)
		if title == "" {
			title = " "
		}
		trailing := strings.TrimSpace(row.Trailing)
		if trailing == "" {
			trailing = " "
		}
		columns := []map[string]any{
			{
				"tag":              "column",
				"width":            "weighted",
				"weight":           1,
				"vertical_spacing": "2px",
				"vertical_align":   "center",
				"elements": []map[string]any{
					mapWith(markdownElementV2(title), "text_size", "heading"),
					markdownElementV2(fmt.Sprintf("<font color='grey'>%s</font>", trailing)),
				},
			},
			{
				"tag":              "column",
				"width":            "auto",
				"vertical_spacing": "0px",
				"vertical_align":   "center",
				"elements": []map[string]any{
					mapWith(markdownElementV2("›"), "text_size", "xx-large"),
				},
			},
		}
		elements = append(elements, map[string]any{
			"tag":              "interactive_container",
			"width":            "fill",
			"background_style": "cus-0",
			"has_border":       true,
			"border_color":     "cus-1",
			"corner_radius":    "8px",
			"padding":          "10px 12px 10px 12px",
			"vertical_spacing": "0px",
			"elements": []map[string]any{{
				"tag":            "column_set",
				"flex_mode":      "none",
				"vertical_align": "center",
				"columns":        columns,
			}},
			"behaviors": []map[string]any{
				{
					"type": "callback",
					"value": map[string]any{
						"callback_data": row.Button.CallbackData,
					},
				},
			},
		})
	}
	return elements
}

func metricRowElements(rows []model.MessageSectionRow) []map[string]any {
	elements := make([]map[string]any, 0, (len(rows)+1)/2)
	for start := 0; start < len(rows); start += 2 {
		end := min(start+2, len(rows))
		columns := make([]map[string]any, 0, 2)
		for _, row := range rows[start:end] {
			title := strings.TrimSpace(row.Title)
			if title == "" {
				title = " "
			}
			trailing := strings.TrimSpace(row.Trailing)
			if trailing == "" {
				trailing = " "
			}
			columns = append(columns, map[string]any{
				"tag":              "column",
				"width":            "weighted",
				"weight":           1,
				"background_style": "cus-0",
				"has_border":       true,
				"border_color":     "cus-1",
				"corner_radius":    "8px",
				"padding":          "12px 14px 12px 14px",
				"vertical_spacing": "4px",
				"vertical_align":   "top",
				"elements": []map[string]any{
					mapWith(mapWith(plainTextElementV2(title), "text_size", "caption"), "text_color", "grey"),
					mapWith(plainTextElementV2(trailing), "text_size", "xx-large"),
				},
			})
		}
		if len(columns) == 1 {
			columns = append(columns, map[string]any{
				"tag":    "column",
				"width":  "weighted",
				"weight": 1,
				"elements": []map[string]any{
					markdownElementV2(" "),
				},
			})
		}
		elements = append(elements, map[string]any{
			"tag":                "column_set",
			"flex_mode":          "none",
			"horizontal_spacing": "8px",
			"vertical_align":     "top",
			"columns":            columns,
		})
	}
	return elements
}

func mapWith(element map[string]any, key string, value any) map[string]any {
	element[key] = value
	return element
}

func rowElements(rows []model.MessageSectionRow) []map[string]any {
	if len(rows) == 0 {
		return nil
	}
	elements := make([]map[string]any, 0, len(rows))
	for index, row := range rows {
		if index > 0 {
			elements = append(elements, markdownElementWithMargin(threadRowSeparator, "2px 0px 8px 0px"))
		}
		title := strings.TrimSpace(row.Title)
		if title == "" {
			title = " "
		}
		trailing := strings.TrimSpace(row.Trailing)
		if trailing == "" {
			trailing = " "
		}
		buttonLabel := feishuButtonLabel(row.Button.Text)
		elements = append(elements, markdownElementWithMargin(title, "0px 0px 2px 0px"))
		columns := []map[string]any{{
			"tag":            "column",
			"width":          "weighted",
			"weight":         1,
			"vertical_align": "center",
			"elements": []map[string]any{
				markdownElement(trailing),
			},
		}}
		if buttonLabel != "" {
			columns = append(columns, map[string]any{
				"tag":            "column",
				"width":          "auto",
				"vertical_align": "center",
				"elements": []map[string]any{
					{
						"tag":  "button",
						"text": map[string]any{"tag": "plain_text", "content": buttonLabel},
						"type": "default",
						"value": map[string]any{
							"callback_data": row.Button.CallbackData,
						},
					},
				},
			})
		}
		elements = append(elements, map[string]any{
			"tag":            "column_set",
			"flex_mode":      "none",
			"margin":         "0px 0px 12px 0px",
			"vertical_align": "center",
			"columns":        columns,
		})
	}
	return elements
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

func buttonElementsV2(buttons [][]model.ButtonSpec) []map[string]any {
	elements := make([]map[string]any, 0, len(buttons))
	for _, row := range buttons {
		columns := make([]map[string]any, 0, len(row))
		for _, button := range row {
			label := feishuButtonLabel(button.Text)
			if label == "" {
				continue
			}
			columns = append(columns, map[string]any{
				"tag":            "column",
				"width":          "weighted",
				"weight":         1,
				"vertical_align": "center",
				"elements": []map[string]any{
					{
						"tag":  "button",
						"text": map[string]any{"tag": "plain_text", "content": label},
						"type": "default",
						"value": map[string]any{
							"callback_data": button.CallbackData,
						},
					},
				},
			})
		}
		if len(columns) == 0 {
			continue
		}
		elements = append(elements, map[string]any{
			"tag":                "column_set",
			"flex_mode":          "none",
			"horizontal_spacing": "8px",
			"vertical_align":     "center",
			"margin":             "0px 0px 10px 0px",
			"columns":            columns,
		})
	}
	return elements
}

func feishuButtonLabel(text string) string {
	label := strings.TrimSpace(text)
	if label == "" {
		return ""
	}
	return label
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
