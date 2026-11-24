package im

import "slices"

func validCard(card map[string]any) bool {
	if kind, exists := card["type"]; exists {
		data, ok := card["data"].(map[string]any)
		return kind == "template" && ok && nonemptyString(data, "template_id") &&
			optionalString(data, "template_version_name") && optionalObject(data, "template_variable")
	}
	if !optionalCardCheck(card, "header", validCardHeader) || !optionalObject(card, "config") {
		return false
	}
	config, _ := card["config"].(map[string]any)
	if !optionalCardType[bool](config, "update_multi") {
		return false
	}
	schema, supplied := card["schema"]
	if !supplied {
		schema = "1.0"
	}
	switch schema {
	case "2.0":
		body, ok := card["body"].(map[string]any)
		_, localized := card["i18n_elements"]
		return !localized && config["update_multi"] != false && ok && validCardElements(body["elements"])
	case "1.0":
		if localized, supplied := card["i18n_elements"]; supplied {
			_, plain := card["elements"]
			return !plain && validCardLocales(localized, func(value any) bool {
				return cardObjects(value, true, validCardComponent)
			})
		}
		return validCardElements(card["elements"])
	default:
		return false
	}
}

func optionalObject(obj map[string]any, field string) bool {
	return optionalCardType[map[string]any](obj, field)
}

func optionalCardType[T any](obj map[string]any, names ...string) bool {
	for _, name := range names {
		if value, exists := obj[name]; exists {
			if _, ok := value.(T); !ok {
				return false
			}
		}
	}
	return true
}

func optionalCardCheck(obj map[string]any, field string, check func(any) bool) bool {
	value, present := obj[field]
	return !present || check(value)
}

func validCardLocales(value any, check func(any) bool) bool {
	locales, ok := value.(map[string]any)
	if !ok || len(locales) == 0 {
		return false
	}
	for lang, content := range locales {
		if !slices.Contains([]string{"en_us", "zh_cn", "zh_hk", "zh_tw", "ja_jp", "id_id", "vi_vn", "th_th",
			"pt_br", "es_es", "ko_kr", "de_de", "fr_fr", "it_it", "ru_ru", "ms_my"}, lang) || !check(content) {
			return false
		}
	}
	return true
}

func cardObjects(value any, nonempty bool, check func(map[string]any) bool) bool {
	items, ok := value.([]any)
	if !ok || (nonempty && len(items) == 0) {
		return false
	}
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok || !check(object) {
			return false
		}
	}
	return true
}

func validCardElements(value any) bool {
	return cardObjects(value, false, validCardComponent)
}

func cardString(value any) bool {
	_, ok := value.(string)
	return ok
}

func validCardText(value any) bool {
	text, ok := value.(map[string]any)
	if !ok {
		return false
	}
	switch text["tag"] {
	case "plain_text", "lark_md", "markdown":
		return validCardTextContent(text)
	default:
		return false
	}
}

func validCardTextContent(text map[string]any) bool {
	content, hasContent := text["content"]
	if hasContent && !cardString(content) {
		return false
	}
	for _, field := range []string{"i18n", "i18n_content"} {
		if localized, found := text[field]; found {
			if !validCardLocales(localized, cardString) {
				return false
			}
			hasContent = true
		}
	}
	return hasContent
}

func validCardHeader(value any) bool {
	header, ok := value.(map[string]any)
	return ok && optionalCardCheck(header, "title", validCardText) &&
		optionalCardCheck(header, "subtitle", validCardText) && optionalObject(header, "icon")
}

func validCardBehavior(behavior map[string]any) bool {
	switch behavior["type"] {
	case "open_url":
		return nonemptyString(behavior, "default_url") && optionalCardType[string](behavior, "pc_url", "ios_url", "android_url")
	case "callback":
		return optionalObject(behavior, "value")
	default:
		return false
	}
}

func validCardComponent(node map[string]any) bool {
	if !optionalCardType[bool](node, "disabled", "required") ||
		!optionalCardType[string](node, "element_id") || !optionalObject(node, "icon") ||
		!optionalCardCheck(node, "behaviors", func(value any) bool { return cardObjects(value, false, validCardBehavior) }) {
		return false
	}

	switch node["tag"] {
	case "markdown", "plain_text", "lark_md":
		return validCardTextContent(node)
	case "div":
		_, hasText := node["text"]
		_, hasFields := node["fields"]
		return (hasText || hasFields) && optionalCardCheck(node, "text", validCardText) &&
			optionalCardCheck(node, "fields", func(value any) bool {
				return cardObjects(value, true, func(field map[string]any) bool {
					return validCardText(field["text"]) && optionalCardType[bool](field, "is_short")
				})
			}) && optionalCardCheck(node, "extra", func(value any) bool {
			extra, ok := value.(map[string]any)
			return ok && validCardComponent(extra)
		})
	case "img":
		return nonemptyString(node, "img_key") && validCardText(node["alt"]) &&
			optionalCardType[bool](node, "preview") && optionalCardType[string](node, "mode")
	case "hr":
		return true
	case "note":
		return cardObjects(node["elements"], true, func(item map[string]any) bool {
			switch item["tag"] {
			case "img":
				return validCardComponent(item)
			case "plain_text", "lark_md":
				return validCardTextContent(item)
			default:
				return false
			}
		})
	case "column_set":
		return cardObjects(node["columns"], true, func(column map[string]any) bool {
			return column["tag"] == "column" && validCardComponent(column)
		})
	case "column":
		return optionalCardType[float64](node, "weight") &&
			optionalCardType[string](node, "width", "vertical_align") &&
			optionalCardCheck(node, "elements", validCardElements)
	case "action":
		return cardObjects(node["actions"], true, validCardComponent)
	case "form":
		return nonemptyString(node, "name") && validCardElements(node["elements"]) && hasCardSubmit(node["elements"])
	case "interactive_container":
		return cardObjects(node["behaviors"], true, validCardBehavior) && validCardElements(node["elements"])
	case "collapsible_panel":
		return validCardHeader(node["header"]) && optionalCardType[bool](node, "expanded") &&
			optionalCardCheck(node, "elements", validCardElements)
	case "button":
		return (node["text"] != nil || node["icon"] != nil) && optionalCardCheck(node, "text", validCardText) &&
			optionalCardType[string](node, "type", "url", "name", "form_action_type", "action_type") &&
			optionalObject(node, "value") && optionalObject(node, "multi_url")
	case "input", "checker", "date_picker", "picker_datetime", "picker_time", "select_person", "multi_select_person":
		return optionalCardCheck(node, "text", validCardText) && optionalCardCheck(node, "placeholder", validCardText) &&
			optionalCardType[string](node, "name")
	case "select_static", "multi_select_static", "overflow":
		return optionalCardCheck(node, "placeholder", validCardText) && cardObjects(node["options"], true, func(option map[string]any) bool {
			return optionalCardCheck(option, "text", validCardText) &&
				optionalCardType[string](option, "value", "url") && optionalObject(option, "multi_url")
		})
	case "select_img":
		return cardObjects(node["options"], true, func(option map[string]any) bool {
			return nonemptyString(option, "img_key") && optionalCardType[string](option, "value") &&
				optionalCardCheck(option, "alt", validCardText)
		})
	case "img_combination":
		return nonemptyString(node, "combination_mode") && cardObjects(node["img_list"], true, func(image map[string]any) bool {
			return nonemptyString(image, "img_key") && optionalCardCheck(image, "alt", validCardText)
		})
	case "person":
		return nonemptyString(node, "user_id")
	case "person_list":
		return cardObjects(node["persons"], true, func(person map[string]any) bool {
			return nonemptyString(person, "id")
		})
	case "chart":
		spec, ok := node["chart_spec"].(map[string]any)
		return ok && nonemptyString(spec, "type")
	case "table":
		return cardObjects(node["columns"], true, func(column map[string]any) bool {
			return nonemptyString(column, "name") && nonemptyString(column, "data_type")
		}) && cardObjects(node["rows"], false, func(map[string]any) bool { return true })
	default:
		return false
	}
}

func hasCardSubmit(value any) bool {
	elements, _ := value.([]any)
	for _, element := range elements {
		node, _ := element.(map[string]any)
		switch node["tag"] {
		case "button":
			if (node["form_action_type"] == "submit" || node["action_type"] == "form_submit") && nonemptyString(node, "name") {
				return true
			}
		case "column_set":
			if hasCardSubmit(node["columns"]) {
				return true
			}
		case "column", "interactive_container", "collapsible_panel":
			if hasCardSubmit(node["elements"]) {
				return true
			}
		case "action":
			if hasCardSubmit(node["actions"]) {
				return true
			}
		}
	}
	return false
}

func sharedCard(card map[string]any) bool {
	if !validCard(card) {
		return false
	}
	// Template references carry no config. The published template defines sharing.
	if card["type"] == "template" || card["schema"] == "2.0" {
		return true
	}
	config, _ := card["config"].(map[string]any)
	return config["update_multi"] == true
}
