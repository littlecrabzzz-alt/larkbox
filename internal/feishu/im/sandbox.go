package im

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/cardkit"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

// PrepareSandboxMessage validates content and produces the standard IM mention form.
func PrepareSandboxMessage(state *store.Store, kind, encoded string, mentionIDs, botIDs []string) (store.NewMessage, error) {
	content, _, err := validateContent(kind, encoded)
	if err != nil {
		return store.NewMessage{}, err
	}
	normalized, _ := json.Marshal(content)
	if protocol.HasAssignedMention(normalized) {
		return store.NewMessage{}, errors.New("content must not contain preassigned @_user_N mention keys")
	}
	if err := insertSandboxMentions(kind, content, mentionIDs, botIDs); err != nil {
		return store.NewMessage{}, err
	}
	if len(mentionIDs)+len(botIDs) > 0 {
		normalized, _ = json.Marshal(content)
	}
	encoded = string(normalized)
	h := handler{state: state}
	encoded, mentions := h.normalizeMentions(kind, encoded, content)
	return store.NewMessage{MsgType: kind, Content: encoded, Mentions: mentions, CardID: interactiveCardID(kind, content)}, nil
}

func interactiveCardID(kind string, content map[string]any) string {
	if kind == "interactive" {
		return cardkit.EntityReference(content)
	}
	return ""
}

func insertSandboxMentions(kind string, content map[string]any, mentionIDs, botIDs []string) error {
	if len(mentionIDs)+len(botIDs) == 0 {
		return nil
	}
	switch kind {
	case "text":
		tags := func(ids []string) string {
			parts := make([]string, 0, len(ids))
			for _, id := range ids {
				parts = append(parts, `<at user_id="`+id+`"></at>`)
			}
			return strings.Join(parts, " ")
		}
		text, _ := content["text"].(string)
		if bots := tags(botIDs); bots != "" {
			if strings.Contains(text, "@bot") {
				text = strings.ReplaceAll(text, "@bot", bots)
			} else {
				text = strings.TrimSpace(bots + " " + text)
			}
		}
		if explicit := tags(mentionIDs); explicit != "" {
			text = strings.TrimSpace(explicit + " " + text)
		}
		content["text"] = text
	case "post":
		add := func(body map[string]any) {
			row := make([]any, 0, len(mentionIDs)+len(botIDs))
			for _, ids := range [][]string{mentionIDs, botIDs} {
				for _, id := range ids {
					row = append(row, map[string]any{"tag": "at", "user_id": id})
				}
			}
			paragraphs, _ := body["content"].([]any)
			body["content"] = append([]any{row}, paragraphs...)
		}
		if _, direct := content["content"]; direct {
			add(content)
		} else {
			for _, lang := range []string{"zh_cn", "en_us", "ja_jp"} {
				if body, ok := content[lang].(map[string]any); ok {
					add(body)
				}
			}
		}
	default:
		return errors.New("mentions require a text or post message")
	}
	return nil
}

// InspectMessageData returns the IM representation and both card read modes.
func InspectMessageData(state *store.Store, m store.Message) map[string]any {
	h := handler{state: state}
	if m.MsgType == "interactive" {
		redactor := state.DiagnosticRedactor(m.AppID, m.DiagnosticRedactor)
		m.Content = string(redactor.JSON([]byte(m.Content)))
	}
	rendered := cardForRead(m, m.AppID, false)
	data := h.messageData(rendered, "open_id", false)
	data["content"] = rendered.Content
	if m.MsgType == "interactive" && !m.Deleted && json.Valid([]byte(m.Content)) {
		data["rendered_card_json"] = json.RawMessage(rendered.Content)
		data["raw_card_json"] = json.RawMessage(m.Content)
		if m.CardID != "" {
			data["card_id"] = m.CardID
		}
	}
	reactions := make([]fields, 0, len(m.Reactions))
	for _, r := range m.Reactions {
		reactions = append(reactions, fields{
			"reaction_id": r.ID, "emoji_type": r.EmojiType, "operator_open_id": r.OperatorOpenID,
			"operator_app_id": r.OperatorAppID, "created_at": r.CreatedAt,
		})
	}
	data["reactions"] = reactions
	if m.Deleted {
		data["content"] = ""
	}
	return data
}
