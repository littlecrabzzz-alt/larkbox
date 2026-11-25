// Package cardkit implements CardKit v1 and validates interactive card content.
package cardkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// ValidMessageCard validates inline cards and template references in IM messages.
func ValidMessageCard(card map[string]any) bool {
	if field, _ := imagePickerIssue(card); field != "" {
		return false
	}
	return validMessageCardShape(card)
}

func validMessageCardShape(card map[string]any) bool {
	if kind, exists := card["type"]; exists {
		data, ok := card["data"].(map[string]any)
		return kind == "template" && ok && validTemplate(data)
	}
	if !optionalCardCheck(card, "header", validCardHeader) || !optionalObject(card, "config") {
		return false
	}
	config, _ := card["config"].(map[string]any)
	if !optionalCardType[bool](config, "update_multi", "enable_forward", "enable_forward_interaction") {
		return false
	}
	schema, supplied := card["schema"]
	if !supplied {
		schema = "1.0"
	}
	switch schema {
	case "2.0":
		_, localized := card["i18n_elements"]
		_, rootElements := card["elements"]
		if localized || rootElements || config["update_multi"] == false {
			return false
		}
		if value, present := card["body"]; present {
			body, ok := value.(map[string]any)
			return ok && validCardElements(body["elements"])
		}
		header, _ := card["header"].(map[string]any)
		return header["title"] != nil || header["subtitle"] != nil
	case "1.0":
		if _, body := card["body"]; body {
			return false
		}
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

// ValidatePreviewJSON checks inline card definitions without creating an entity.
// Card JSON 2.0 uses CardKit's stored-entity checks and addressable-element rules.
func ValidatePreviewJSON(encoded string) error {
	doc := object(encoded)
	if doc == nil || doc["type"] != nil {
		return errors.New("paste a Card JSON 1.0 or 2.0 object")
	}
	if !validCardSettings(doc) {
		return errors.New("correct the card config or card_link fields")
	}
	if field, issue := imagePickerIssue(doc); field != "" {
		return fmt.Errorf("%s — %s", field, issue)
	}
	if doc["schema"] == "2.0" {
		if err := validateEditedCard(doc); err != nil {
			if errors.Is(err, errUnsharedCard) {
				return errors.New(errUnsharedCard.Message)
			}
			return previewShapeIssue(doc)
		}
		if _, err := indexElements(doc); err != nil {
			var apiErr protocol.APIError
			if errors.As(err, &apiErr) {
				if apiErr.Details != nil && len(apiErr.Details.FieldViolations) > 0 {
					issue := apiErr.Details.FieldViolations[0]
					return fmt.Errorf("%s — %s", issue.Field, issue.Description)
				}
				return errors.New(apiErr.Message)
			}
			return err
		}
		return nil
	}
	if !validMessageCardShape(doc) {
		return previewShapeIssue(doc)
	}
	return nil
}

func imagePickerIssue(card map[string]any) (field, issue string) {
	walk(card, func(slot elementSlot, node map[string]any) bool {
		if node["tag"] != "select_img" {
			return false
		}
		if node["multi_select"] == true && !slot.inForm {
			field, issue = "multi_select", "Multiple image selection requires a form."
		} else if slot.inForm && !nonemptyString(node, "name") {
			field, issue = "name", "An image picker inside a form requires a name."
		}
		return field != ""
	})
	return field, issue
}

func previewShapeIssue(doc map[string]any) error {
	if schema, supplied := doc["schema"]; supplied && schema != "1.0" && schema != "2.0" {
		return errors.New("schema must be 1.0 or 2.0")
	}
	if !optionalCardCheck(doc, "header", validCardHeader) {
		return errors.New("correct the card header and its title, subtitle, or text tags")
	}
	path, elements := "elements", doc["elements"]
	if doc["schema"] == "2.0" {
		path = "body.elements"
		body, _ := doc["body"].(map[string]any)
		elements = body["elements"]
	}
	if components, ok := elements.([]any); ok {
		for i, raw := range components {
			if node, ok := raw.(map[string]any); !ok || !validCardComponent(node) {
				return fmt.Errorf("check the component at %s[%d] and its nested fields", path, i)
			}
		}
	}
	return fmt.Errorf("check %s and the fields allowed by the selected card schema", path)
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

func optionalCardInts(obj map[string]any, max int64, names ...string) bool {
	for _, name := range names {
		if value, exists := obj[name]; exists {
			number, ok := value.(json.Number)
			if !ok {
				return false
			}
			n, err := number.Int64()
			if err != nil || n < 1 || n > max {
				return false
			}
		}
	}
	return true
}

func validCardNumber(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	// The decoder already enforces JSON number syntax. Float64 rejects overflow.
	_, err := number.Float64()
	return err == nil
}

func validCardInputType(value any) bool {
	switch value {
	case "text", "multiline_text", "password":
		return true
	default:
		return false
	}
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

func validNestedCardElements(value any) bool {
	return cardObjects(value, false, validNestedCardComponent)
}

func validNestedCardComponent(node map[string]any) bool {
	return node["tag"] != "form" && node["tag"] != "table" && validCardComponent(node)
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

func validInputText(value any) bool {
	text, ok := value.(map[string]any)
	if !ok {
		return false
	}
	tag, supplied := text["tag"]
	return (!supplied || tag == "plain_text") && optionalString(text, "content")
}

func validCardStrings(value any) bool {
	list, ok := value.([]any)
	if !ok {
		return false
	}
	return !slices.ContainsFunc(list, func(item any) bool { return !cardString(item) })
}

func validCardPicker(node map[string]any) bool {
	return optionalCardCheck(node, "placeholder", validCardText) &&
		optionalCardType[string](node, "name", "width", "margin", "type")
}

var elementIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,19}$`)

func validHeaderTags(value any) bool {
	return cardObjects(value, false, validHeaderTag)
}

func validHeaderTag(node map[string]any) bool {
	if node["tag"] != "text_tag" || !optionalString(node, "color") {
		return false
	}
	return optionalCardCheck(node, "text", func(value any) bool {
		text, ok := value.(map[string]any)
		return ok && text["tag"] == "plain_text" && validCardTextContent(text)
	})
}

func validCardHeader(value any) bool {
	header, ok := value.(map[string]any)
	return ok && optionalCardCheck(header, "title", validCardText) &&
		optionalCardCheck(header, "subtitle", validCardText) && optionalObject(header, "icon") &&
		optionalCardCheck(header, "text_tag_list", validHeaderTags) &&
		optionalCardCheck(header, "i18n_text_tag_list", func(value any) bool { return validCardLocales(value, validHeaderTags) })
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
	case "text_tag":
		return validHeaderTag(node)
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
			return ok && validNestedCardComponent(extra)
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
		return optionalCardType[json.Number](node, "weight") &&
			optionalCardType[string](node, "width", "vertical_align") &&
			optionalCardCheck(node, "elements", validNestedCardElements)
	case "action":
		return cardObjects(node["actions"], true, validNestedCardComponent)
	case "form":
		return nonemptyString(node, "name") && validNestedCardElements(node["elements"]) && hasCardSubmit(node["elements"])
	case "interactive_container":
		return cardObjects(node["behaviors"], true, validCardBehavior) && validNestedCardElements(node["elements"])
	case "collapsible_panel":
		return validCardHeader(node["header"]) && optionalCardType[bool](node, "expanded") &&
			optionalCardCheck(node, "elements", validNestedCardElements)
	case "button":
		return (node["text"] != nil || node["icon"] != nil) && optionalCardCheck(node, "text", validCardText) &&
			optionalCardType[string](node, "type", "url", "name", "form_action_type", "action_type", "size", "width", "margin") &&
			optionalObject(node, "value") && optionalObject(node, "multi_url")
	case "input":
		return optionalCardCheck(node, "placeholder", validInputText) && optionalCardCheck(node, "label", validInputText) &&
			optionalCardCheck(node, "text", validCardText) &&
			optionalCardType[string](node, "name", "width", "margin", "default_value", "label_position") &&
			optionalCardCheck(node, "input_type", validCardInputType) &&
			optionalCardType[bool](node, "show_icon", "auto_resize") &&
			optionalCardInts(node, 1000, "max_length") &&
			optionalCardInts(node, math.MaxInt64, "rows") &&
			optionalCardCheck(node, "max_rows", validCardNumber)
	case "checker":
		return validCardPicker(node) && optionalCardCheck(node, "text", validCardText) &&
			optionalCardType[bool](node, "checked", "overall_checkable")
	case "date_picker", "picker_datetime", "picker_time":
		return validCardPicker(node) && optionalCardCheck(node, "text", validCardText) &&
			optionalCardType[string](node, "initial_date", "initial_datetime", "initial_time") &&
			optionalObject(node, "value")
	case "select_person", "multi_select_person":
		return validCardPicker(node) && optionalCardType[string](node, "initial_option") &&
			optionalCardCheck(node, "selected_values", validCardStrings) &&
			optionalCardCheck(node, "options", func(value any) bool {
				return cardObjects(value, false, func(option map[string]any) bool {
					return optionalString(option, "value")
				})
			})
	case "select_static", "multi_select_static", "overflow":
		return validCardPicker(node) && optionalCardType[string](node, "initial_option") &&
			optionalCardCheck(node, "selected_values", validCardStrings) && cardObjects(node["options"], true, func(option map[string]any) bool {
			return optionalCardCheck(option, "text", validCardText) &&
				optionalCardType[string](option, "value", "url") && optionalObject(option, "multi_url")
		})
	case "select_img":
		return optionalCardType[bool](node, "multi_select", "can_preview") &&
			optionalCardType[string](node, "name", "layout", "aspect_ratio") &&
			cardObjects(node["options"], true, func(option map[string]any) bool {
				return nonemptyString(option, "img_key") && optionalCardType[string](option, "value") &&
					optionalCardType[bool](option, "disabled") && optionalCardCheck(option, "alt", validCardText) &&
					optionalCardCheck(option, "hover_tips", validCardText) && optionalCardCheck(option, "disabled_tips", validCardText)
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

// SharedMessageCard reports whether a card permits shared IM updates.
func SharedMessageCard(card map[string]any) bool {
	if !ValidMessageCard(card) {
		return false
	}
	// Template references carry no config. The published template defines sharing.
	if card["type"] == "template" || card["schema"] == "2.0" {
		return true
	}
	config, _ := card["config"].(map[string]any)
	return config["update_multi"] == true
}

func validTemplate(data map[string]any) bool {
	return nonemptyString(data, "template_id") &&
		optionalString(data, "template_version_name") && optionalObject(data, "template_variable")
}

func nonemptyString(obj map[string]any, key string) bool {
	value, _ := obj[key].(string)
	return strings.TrimSpace(value) != ""
}

func optionalString(obj map[string]any, key string) bool {
	return optionalCardType[string](obj, key)
}

// EntityReference extracts a nonempty CardKit ID from an IM card reference.
func EntityReference(card map[string]any) string {
	if card["type"] != "card" {
		return ""
	}
	data, _ := card["data"].(map[string]any)
	if !nonemptyString(data, "card_id") {
		return ""
	}
	return data["card_id"].(string)
}
