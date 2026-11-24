package im

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"

	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func originalCardContent(w http.ResponseWriter, r *http.Request) (bool, bool) {
	if !r.URL.Query().Has("card_msg_content_type") {
		return false, true
	}
	mode, ok := queryChoice(w, r, "card_msg_content_type", "", "", "user_card_content")
	return mode == "user_card_content", ok
}

func cardForRead(m store.Message, appID string, original bool) store.Message {
	if m.MsgType != "interactive" || m.Deleted || (m.CardID != "" && m.AppID != appID) ||
		(original && m.AppID == appID) {
		return m
	}
	card := decodeContentObject(m.Content)
	if card == nil || card["type"] != nil || isRenderedCard(card) {
		// Unresolved entity/template references and already-rendered cards use
		// the available message representation.
		return m
	}
	header, _ := card["header"].(map[string]any)
	components, _ := card["elements"].([]any)
	if body, ok := card["body"].(map[string]any); ok {
		components, _ = body["elements"].([]any)
	} else if locales, ok := card["i18n_elements"].(map[string]any); ok {
		components, _ = cardLocale(locales).([]any)
	}

	rows := make([][]fields, 0, len(components))
	for _, component := range components {
		rows = append(rows, renderComponent(component)...)
	}
	payload, _ := json.Marshal(fields{"title": renderedText(header["title"]), "elements": rows})
	m.Content = string(payload)
	return m
}

func isRenderedCard(card map[string]any) bool {
	if _, title := card["title"].(string); !title {
		return false
	}
	rows, ok := card["elements"].([]any)
	if !ok || card["body"] != nil || card["header"] != nil || card["config"] != nil {
		return false
	}
	for _, row := range rows {
		if _, ok := row.([]any); !ok {
			return false
		}
	}
	return true
}

func cardLocale(locales map[string]any) any {
	for _, locale := range []string{"en_us", "zh_cn"} {
		if content, ok := locales[locale]; ok {
			return content
		}
	}
	for _, locale := range slices.Sorted(maps.Keys(locales)) {
		return locales[locale]
	}
	return nil
}

func renderedText(value any) string {
	node, _ := value.(map[string]any)
	if text, ok := node["content"].(string); ok {
		return text
	}
	for _, key := range []string{"i18n_content", "i18n"} {
		if locales, ok := node[key].(map[string]any); ok {
			text, _ := cardLocale(locales).(string)
			return text
		}
	}
	return ""
}

func renderComponent(value any) [][]fields {
	node, _ := value.(map[string]any)
	tag, _ := node["tag"].(string)
	var row []fields
	switch tag {
	case "plain_text", "lark_md", "markdown":
		row = []fields{{"tag": "text", "text": renderedText(node)}}
	case "button":
		kind, _ := node["type"].(string)
		if kind == "" {
			kind = "default"
		}
		row = []fields{{"tag": tag, "text": renderedText(node["text"]), "type": kind}}
	case "img":
		row = []fields{{"tag": tag, "image_key": node["img_key"]}}
	case "hr":
		row = []fields{{"tag": tag}}
	case "div":
		if text, ok := node["text"]; ok {
			row = append(row, fields{"tag": "text", "text": renderedText(text)})
		}
		properties, _ := node["fields"].([]any)
		for _, item := range properties {
			property, _ := item.(map[string]any)
			row = append(row, fields{"tag": "text", "text": renderedText(property["text"])})
		}
		for _, extra := range renderComponent(node["extra"]) {
			row = append(row, extra...)
		}
	case "note", "action":
		var group []fields
		for _, children := range []string{"elements", "actions"} {
			list, _ := node[children].([]any)
			for _, child := range list {
				for _, childRow := range renderComponent(child) {
					group = append(group, childRow...)
				}
			}
		}
		row = group
		if tag == "note" {
			row = []fields{{"tag": tag, "elements": group}}
		}
	case "column_set", "column", "form", "interactive_container", "collapsible_panel":
		var rows [][]fields
		for _, field := range []string{"columns", "elements"} {
			children, _ := node[field].([]any)
			for _, child := range children {
				rows = append(rows, renderComponent(child)...)
			}
		}
		return rows
	case "select_static", "multi_select_static", "overflow":
		options, _ := node["options"].([]any)
		labels := make([]string, 0, len(options))
		for _, value := range options {
			option, _ := value.(map[string]any)
			labels = append(labels, renderedText(option["text"]))
		}
		row = []fields{{"tag": tag, "options": labels}}
	case "date_picker", "picker_datetime", "picker_time":
		item := fields{"tag": tag, "placeholder": renderedText(node["placeholder"])}
		for _, field := range []string{"initial_date", "initial_datetime", "initial_time"} {
			if value, present := node[field]; present {
				item[field] = value
			}
		}
		row = []fields{item}
	}
	if len(row) == 0 {
		return nil
	}
	return [][]fields{row}
}
