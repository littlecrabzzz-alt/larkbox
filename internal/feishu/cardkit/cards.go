package cardkit

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const (
	kindJSON     = "card_json"
	kindTemplate = "template"
)

func cardError(code protocol.Code, message string) protocol.APIError {
	return protocol.APIError{Code: code, Message: message, Status: http.StatusBadRequest}
}

var (
	errDuplicateElement  = cardError(300301, "Duplicate element_id in card component.")
	errUnsharedCard      = cardError(300302, "update_multi property is false.")
	errUnsupportedSchema = cardError(300303, "Only schema 2.0 is supported.")
	errCardOwner         = cardError(300311, "The current application does not have permission to update/use this card.")
	errElementTag        = cardError(300312, "Unable to update element tag.")
	errPatchElement      = cardError(300313, "Failed to update element properties.")
	errDeleteElement     = cardError(300314, "Failed to delete element.")
	errAddElement        = cardError(300315, "Failed to add element.")
	errSequence          = cardError(300317, "The sequence number for operating on the card did not increment consecutively.")
	errReplaceElement    = cardError(300121, "Failed to replace element.")
)

func object(encoded string) map[string]any {
	var data map[string]any
	if !decode(encoded, &data) {
		return nil
	}
	return data
}

func decode(encoded string, dst any) bool {
	if !json.Valid([]byte(encoded)) {
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.UseNumber()
	return decoder.Decode(dst) == nil
}

func invalid(field, description string) error {
	return protocol.NewError(protocol.FieldValidation, &protocol.ErrorDetails{
		FieldViolations: []protocol.FieldViolation{{Field: field, Description: description}},
	})
}

// ParseCallbackCard validates a callback's inline or template card data.
// References are resolved by the store in the same transaction as the update.
func ParseCallbackCard(state *store.Store, kind string, data json.RawMessage) (store.Card, string, error) {
	doc := object(string(data))
	switch kind {
	case "card":
		id := EntityReference(map[string]any{"type": kind, "data": doc})
		if id != "" {
			return store.Card{}, id, nil
		}
	case "raw":
		if doc != nil && doc["type"] == nil && ValidMessageCard(doc) && validCardSettings(doc) &&
			(doc["schema"] != "2.0" || validEntityCard(doc)) {
			card, err := encodeCard(kindJSON, doc)
			return card, "", err
		}
	case kindTemplate:
		card, err := instantiateCard(state, kindTemplate, string(data))
		return card, "", err
	}
	return store.Card{}, "", invalid("card", "Invalid callback card.")
}

// instantiateCard uses the local definition if the requested template is registered.
func instantiateCard(state *store.Store, kind, data string) (store.Card, error) {
	card, err := parseEntity(kind, data)
	if err != nil || kind != kindTemplate {
		return card, err
	}
	template := object(data)
	id, _ := template["template_id"].(string)
	version, _ := template["template_version_name"].(string)
	if definition := state.CardTemplate(id, version); definition != "" {
		instance, err := parseEntity(kindJSON, definition)
		if err != nil {
			return store.Card{}, err
		}
		instance.Type, instance.TemplateData = kindTemplate, card.JSON
		return instance, nil
	}
	return card, nil
}

func parseEntity(kind, encoded string) (store.Card, error) {
	doc := object(encoded)
	valid := false
	if doc != nil && validCardSettings(doc) {
		switch kind {
		case kindJSON:
			_, wrapped := doc["type"]
			if !wrapped && doc["schema"] == "2.0" {
				if err := validateEditedCard(doc); err != nil {
					return store.Card{}, err
				}
				valid = true
			}
		case kindTemplate:
			valid = doc["schema"] == nil && validTemplate(doc)
		}
	}
	if !valid {
		return store.Card{}, invalid("data", "Expected card JSON 2.0 or template data matching the card type.")
	}
	return encodeCard(kind, doc)
}

func encodeCard(kind string, doc map[string]any) (store.Card, error) {
	var elements map[string]string
	if kind == kindJSON || doc["schema"] == "2.0" {
		var err error
		elements, err = indexElements(doc)
		if err != nil {
			return store.Card{}, err
		}
	}
	config, _ := doc["config"].(map[string]any)
	streaming, _ := config["streaming_mode"].(bool)
	wire, err := json.Marshal(doc)
	return store.Card{Type: kind, JSON: wire, Elements: elements, StreamingMode: streaming}, err
}

func indexElements(doc map[string]any) (map[string]string, error) {
	elements := make(map[string]string)
	v2 := doc["schema"] == "2.0"
	var validationErr error
	count := 0
	walk(doc, func(_ elementSlot, node map[string]any) bool {
		count++
		if v2 {
			if count > 200 {
				validationErr = cardError(300305, "The number of card components exceeds 200.")
				return true
			}
			if node["tag"] == "note" || node["tag"] == "action" {
				validationErr = invalid("tag", "Component is unavailable in card JSON 2.0.")
				return true
			}
		}
		if value, present := node["element_id"]; present {
			id, ok := value.(string)
			if !ok || !elementIDPattern.MatchString(id) {
				validationErr = invalid("element_id", "Expected 1 to 20 ASCII letters, digits, or underscores, starting with a letter.")
				return true
			}
			if _, exists := elements[id]; exists {
				validationErr = errDuplicateElement
				return true
			}
			elements[id], _ = node["content"].(string)
		}
		return false
	})
	return elements, validationErr
}

func validEntityCard(doc map[string]any) bool {
	_, rootElements := doc["elements"]
	_, fallback := doc["fallback"]
	return doc["schema"] == "2.0" && !rootElements && !fallback && ValidMessageCard(doc) &&
		optionalCardCheck(doc, "body", func(value any) bool {
			body, ok := value.(map[string]any)
			return ok && optionalCardType[string](body, "direction", "horizontal_spacing", "vertical_spacing", "horizontal_align", "vertical_align", "padding")
		})
}

func validCardSettings(doc map[string]any) bool {
	return optionalCardCheck(doc, "config", validConfig) &&
		optionalCardCheck(doc, "card_link", func(value any) bool {
			link, ok := value.(map[string]any)
			return ok && optionalCardType[string](link, "url", "pc_url", "ios_url", "android_url")
		})
}

func validConfig(value any) bool {
	config, ok := value.(map[string]any)
	return ok && optionalCardType[bool](config, "streaming_mode", "update_multi", "enable_forward", "wide_screen_mode") &&
		optionalObject(config, "style") &&
		optionalCardCheck(config, "summary", func(value any) bool {
			summary, ok := value.(map[string]any)
			return ok && optionalString(summary, "content") && optionalCardCheck(summary, "i18n_content", func(value any) bool {
				return validCardLocales(value, cardString)
			})
		}) && optionalCardCheck(config, "streaming_config", validStreamingConfig)
}

func validStreamingConfig(value any) bool {
	cfg, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, field := range []string{"print_frequency_ms", "print_step"} {
		if value, present := cfg[field]; present {
			platforms, ok := value.(map[string]any)
			if !ok || platforms["default"] == nil || !optionalCardInts(platforms, 1<<31-1, "default", "android", "ios", "pc") {
				return false
			}
		}
	}
	return optionalCardCheck(cfg, "print_strategy", func(value any) bool {
		return value == "fast" || value == "delay"
	})
}

func changeSettings(doc, patch map[string]any, restart *bool) error {
	if len(patch) == 0 || !validCardSettings(patch) {
		return invalid("settings", "Expected valid config or card_link settings.")
	}
	for field, value := range patch {
		if field != "config" && field != "card_link" {
			return invalid("settings", "Unsupported setting: "+field+".")
		}
		properties := value.(map[string]any)
		if field == "config" && properties["streaming_mode"] == true {
			*restart = true
		}
		previous, _ := doc[field].(map[string]any)
		if previous == nil {
			doc[field] = value
		} else {
			mergeProperties(previous, properties)
		}
	}
	return nil
}

func mergeProperties(dst, patch map[string]any) {
	for key, value := range patch {
		if child, ok := value.(map[string]any); ok {
			if previous, ok := dst[key].(map[string]any); ok {
				mergeProperties(previous, child)
				continue
			}
		}
		dst[key] = value
	}
}

type elementSlot struct {
	parent map[string]any
	key    string
	index  int // -1 for a component stored directly in an object property
}

func (s elementSlot) list() []any {
	list, _ := s.parent[s.key].([]any)
	return list
}

func (s elementSlot) replace(node map[string]any) {
	if s.index < 0 {
		s.parent[s.key] = node
		return
	}
	s.list()[s.index] = node
}

func (s elementSlot) remove() {
	if s.index < 0 {
		delete(s.parent, s.key)
		return
	}
	s.parent[s.key] = slices.Delete(s.list(), s.index, s.index+1)
}

func walk(parent map[string]any, visit func(elementSlot, map[string]any) bool) bool {
	// Walk only structural properties so an ID in callback values, chart data,
	// template variables, or other opaque payloads does not become a card element.
	for _, key := range []string{"body", "header", "elements", "columns", "actions", "fields", "options", "extra", "text", "title", "subtitle", "alt", "label", "placeholder", "text_tag_list"} {
		switch value := parent[key].(type) {
		case map[string]any:
			if visitNode(elementSlot{parent, key, -1}, value, visit) {
				return true
			}
		case []any:
			for i, item := range value {
				if node, ok := item.(map[string]any); ok && visitNode(elementSlot{parent, key, i}, node, visit) {
					return true
				}
			}
		}
	}
	// Localized suffix tags contain the same element shape under locale keys.
	if locales, ok := parent["i18n_text_tag_list"].(map[string]any); ok {
		for _, locale := range slices.Sorted(maps.Keys(locales)) {
			nodes, _ := locales[locale].([]any)
			for i, item := range nodes {
				if node, ok := item.(map[string]any); ok && visitNode(elementSlot{locales, locale, i}, node, visit) {
					return true
				}
			}
		}
	}
	return false
}

func visitNode(slot elementSlot, node map[string]any, visit func(elementSlot, map[string]any) bool) bool {
	tag, _ := node["tag"].(string)
	return (tag != "" && visit(slot, node)) || walk(node, visit)
}

func findElement(doc map[string]any, id string) (elementSlot, map[string]any) {
	var location elementSlot
	var element map[string]any
	if id != "" {
		walk(doc, func(slot elementSlot, node map[string]any) bool {
			if node["element_id"] != id {
				return false
			}
			location, element = slot, node
			return true
		})
	}
	return location, element
}

func addElements(doc map[string]any, kind, target string, elements []any) error {
	if target != "" && !elementIDPattern.MatchString(target) {
		return invalid("target_element_id", "Invalid element ID.")
	}
	if !cardObjects(elements, true, validCardComponent) {
		return invalid("elements", "Expected a nonempty array of card components.")
	}
	slot, node := findElement(doc, target)
	switch kind {
	case "append":
		if target == "" {
			node, _ = doc["body"].(map[string]any)
			if node == nil {
				node = make(map[string]any)
				doc["body"] = node
			}
		} else {
			switch node["tag"] {
			case "column", "form", "interactive_container", "collapsible_panel":
			default:
				return errAddElement
			}
		}
		old, _ := node["elements"].([]any)
		node["elements"] = append(old, elements...)
	case "insert_before", "insert_after":
		if node == nil || slot.index < 0 {
			return errAddElement
		}
		index := slot.index
		if kind == "insert_after" {
			index++
		}
		slot.parent[slot.key] = slices.Insert(slot.list(), index, elements...)
	default:
		return invalid("type", "Expected append, insert_before, or insert_after.")
	}
	return nil
}

func updateElement(doc map[string]any, id string, patch map[string]any, partial bool) error {
	if !elementIDPattern.MatchString(id) {
		return invalid("element_id", "Invalid element ID.")
	}
	field := "element"
	failure := errReplaceElement
	if partial {
		field = "partial_element"
		failure = errPatchElement
	}
	if len(patch) == 0 {
		return invalid(field, "Expected a nonempty component object.")
	}
	slot, node := findElement(doc, id)
	if node == nil {
		return failure
	}
	if partial {
		if tag, present := patch["tag"]; present && tag != node["tag"] {
			return errElementTag
		}
		mergeProperties(node, patch)
	} else {
		if _, specified := patch["element_id"]; !specified {
			patch["element_id"] = id
		}
		slot.replace(patch)
		node = patch
	}
	if !validCardComponent(node) {
		return invalid(field, "Invalid component properties.")
	}
	return nil
}

func deleteElement(doc map[string]any, id string) error {
	if !elementIDPattern.MatchString(id) {
		return invalid("element_id", "Invalid element ID.")
	}
	slot, node := findElement(doc, id)
	if node == nil {
		return errDeleteElement
	}
	slot.remove()
	return nil
}

func streamElement(card *store.Card, doc map[string]any, id string, content *string, now time.Time) error {
	if content == nil || *content == "" || utf8.RuneCountInString(*content) > 100000 {
		return invalid("content", "Expected a string of 1 to 100000 characters.")
	}
	if card.StreamingExpired(now) {
		return cardError(200850, "Card streaming timeout.")
	}
	if !card.StreamingMode {
		return cardError(300309, "Card streaming closed.")
	}
	_, node := findElement(doc, id)
	if node == nil {
		return errPatchElement
	}
	switch node["tag"] {
	case "markdown", "plain_text":
		if card.Type == kindTemplate && node["tag"] != "markdown" {
			return cardError(300310, "Only update text.")
		}
		node["content"] = *content
		return nil
	default:
		return cardError(300310, "Only update text.")
	}
}

// applyBatch implements the five action shapes documented for batch_update.
// The input is a private decoded card snapshot owned by the current transaction.
func applyBatch(doc map[string]any, encoded string, restart *bool) error {
	var actions []struct {
		Action string `json:"action"`
		Params struct {
			Settings        map[string]any `json:"settings"`
			Type            string         `json:"type"`
			TargetElementID string         `json:"target_element_id"`
			Elements        []any          `json:"elements"`
			ElementIDs      []string       `json:"element_ids"`
			ElementID       string         `json:"element_id"`
			PartialElement  map[string]any `json:"partial_element"`
			Element         map[string]any `json:"element"`
		} `json:"params"`
	}
	if !decode(encoded, &actions) || len(actions) == 0 {
		return invalid("actions", "Expected a JSON-encoded nonempty array of actions.")
	}
	for i, action := range actions {
		p := action.Params
		var err error
		if action.Action != "partial_update_setting" && doc["schema"] != "2.0" {
			return errUnsupportedSchema
		}
		switch action.Action {
		case "partial_update_setting":
			err = changeSettings(doc, p.Settings, restart)
		case "add_elements":
			err = addElements(doc, p.Type, p.TargetElementID, p.Elements)
		case "delete_elements":
			if len(p.ElementIDs) == 0 {
				err = invalid("element_ids", "Expected a nonempty array of element IDs.")
			}
			for _, id := range p.ElementIDs {
				if err = deleteElement(doc, id); err != nil {
					break
				}
			}
		case "partial_update_element":
			err = updateElement(doc, p.ElementID, p.PartialElement, true)
		case "update_element":
			err = updateElement(doc, p.ElementID, p.Element, false)
		default:
			err = invalid("actions", fmt.Sprintf("Unsupported action at index %d.", i))
		}
		if err != nil {
			return err
		}
		if doc["schema"] == "2.0" {
			if err := validateEditedCard(doc); err != nil {
				return err
			}
			if _, err := indexElements(doc); err != nil {
				return err
			}
		}
	}
	return nil
}

func saveEntity(kind string, doc map[string]any) (store.Card, error) {
	if kind == kindJSON || doc["schema"] == "2.0" {
		if err := validateEditedCard(doc); err != nil {
			return store.Card{}, err
		}
	}
	return encodeCard(kind, doc)
}

func validateEditedCard(doc map[string]any) error {
	if doc["schema"] != "2.0" {
		return errUnsupportedSchema
	}
	if config, _ := doc["config"].(map[string]any); config["update_multi"] == false {
		return errUnsharedCard
	}
	if !validEntityCard(doc) || !validCardSettings(doc) {
		return invalid("data", "The operation would leave an invalid card.")
	}
	return nil
}
