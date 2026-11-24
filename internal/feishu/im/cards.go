package im

func validCard(card map[string]any) bool {
	if kind, exists := card["type"]; exists {
		data, ok := card["data"].(map[string]any)
		return kind == "template" && ok && nonemptyString(data, "template_id") &&
			optionalString(data, "template_version_name") && optionalObject(data, "template_variable")
	}
	if !optionalObject(card, "header") || !optionalObject(card, "config") {
		return false
	}
	if config, ok := card["config"].(map[string]any); ok {
		if flag, present := config["update_multi"]; present {
			if _, ok := flag.(bool); !ok {
				return false
			}
		}
	}
	if schema, exists := card["schema"]; exists {
		body, ok := card["body"].(map[string]any)
		config, _ := card["config"].(map[string]any)
		return schema == "2.0" && config["update_multi"] != false && ok && validCardElements(body["elements"])
	}
	return validCardElements(card["elements"])
}

func optionalObject(obj map[string]any, field string) bool {
	value, present := obj[field]
	_, ok := value.(map[string]any)
	return !present || ok
}

func validCardElements(value any) bool {
	elements, ok := value.([]any)
	if !ok {
		return false
	}
	for _, value := range elements {
		element, ok := value.(map[string]any)
		if !ok || !nonemptyString(element, "tag") {
			return false
		}
	}
	return true
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
