package store

import (
	"encoding/json"
	"errors"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// Forwarding errors identify the source or target condition that rejected a request.
var (
	ErrForwardThreadMissing = errors.New("forward target thread does not exist")
	ErrForwardSource        = errors.New("no eligible messages to forward")
	ErrForwardType          = errors.New("message type does not support forwarding")
	ErrForwardRecalled      = errors.New("all of the messages have been recalled")
	ErrForwardRestricted    = errors.New("forwarding a restricted message is not allowed")
	ErrForwardMixedChats    = errors.New("messages are from different chats")
	ErrForwardMixedThreads  = errors.New("messages are from different topics")
	ErrForwardTooLarge      = errors.New("forwarded message bundle exceeds 1000 entries")
)

type forwardRequest struct {
	ids    []string
	merged bool
}

// ForwardIMMessages copies eligible sources into the destination as one
// transaction. A merge saves a bundle parent and independently readable children.
func (s *Store) ForwardIMMessages(actor IMActor, destination NewMessage, ids []string, merged bool) (Message, []string, bool, error) {
	destination.forward = &forwardRequest{ids: ids, merged: merged}
	s.mu.Lock()
	defer s.mu.Unlock()
	message, created, err := s.createIMMessage(actor, destination)
	return message, message.invalidSources, created, err
}

func (s *Store) prepareForward(actor IMActor, dest *Message, request forwardRequest) ([]Message, error) {
	sources := make([]Message, 0, len(request.ids))
	seen := make(map[string]struct{}, len(request.ids))
	recalled := 0
	for _, id := range request.ids {
		if _, repeat := seen[id]; repeat {
			continue
		}
		seen[id] = struct{}{}
		m, err := s.imMessage(actor, id)
		unsupported := m.UpperMessageID != "" || m.MsgType == "system" || (!request.merged && m.MsgType == "audio")
		if err != nil || m.Deleted || unsupported {
			if m.Deleted {
				recalled++
			} else if !request.merged && unsupported {
				return nil, ErrForwardType
			}
			dest.invalidSources = append(dest.invalidSources, id)
			continue
		}
		if mode := s.chats.items[m.ChatID].RestrictedModeSetting; mode != nil && mode.Status {
			return nil, ErrForwardRestricted
		}
		if m.MsgType == "interactive" {
			card := s.renderMessage(m.CardAppID(), m)
			if flag := forwardingFlags(card.Content).EnableForward; flag != nil && !*flag {
				return nil, ErrForwardRestricted
			}
		}
		if len(sources) > 0 {
			if m.ChatID != sources[0].ChatID {
				return nil, ErrForwardMixedChats
			}
			if m.ThreadID != sources[0].ThreadID {
				return nil, ErrForwardMixedThreads
			}
		}
		sources = append(sources, m)
		dest.forwardedCount += m.forwardedCount + 1
	}
	if len(sources) == 0 {
		if recalled > 0 && recalled == len(seen) {
			return nil, ErrForwardRecalled
		}
		return nil, ErrForwardSource
	}
	if request.merged {
		if dest.forwardedCount > 1000 {
			return nil, ErrForwardTooLarge
		}
		dest.MsgType, dest.Content = "merge_forward", "Merged and Forwarded Message"
		return sources, nil
	}
	source := s.renderMessage(actor.AppID, sources[0])
	dest.MsgType, dest.Content, dest.Mentions = source.MsgType, source.Content, source.Mentions
	dest.DiagnosticRedactor = source.DiagnosticRedactor
	dest.ResourceAppID = resourceOwner(source)
	dest.Forwarded = true
	dest.forwardedCount = source.forwardedCount
	children := make([]Message, 0, len(source.forwardedIDs))
	for _, id := range source.forwardedIDs {
		children = append(children, s.messages.items[id])
	}
	return children, nil
}

func (s *Store) copyForwarded(appID string, parent Message, sources []Message) []string {
	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		snapshot := s.renderMessage(appID, source)
		child := Message{
			ID: protocol.NewID(protocol.MessageID), ChatID: parent.ChatID, UpperMessageID: parent.ID,
			AppID: source.AppID, SenderOpenID: source.SenderOpenID, SenderIsUser: source.SenderIsUser,
			MsgType: source.MsgType, Content: snapshot.Content, Mentions: source.Mentions,
			CreatedAt: source.CreatedAt, UpdatedAt: source.UpdatedAt, Updated: source.Updated,
			ResourceAppID: resourceOwner(source), Forwarded: true, forwardedCount: source.forwardedCount,
			DiagnosticRedactor: snapshot.DiagnosticRedactor,
		}
		if len(source.forwardedIDs) > 0 {
			nested := make([]Message, 0, len(source.forwardedIDs))
			for _, id := range source.forwardedIDs {
				nested = append(nested, s.messages.items[id])
			}
			child.forwardedIDs = s.copyForwarded(appID, child, nested)
		}
		s.messages.put(child)
		ids = append(ids, child.ID)
	}
	return ids
}

type cardForwardFlags struct {
	EnableForward     *bool `json:"enable_forward"`
	EnableInteraction bool  `json:"enable_forward_interaction"`
}

func forwardingFlags(content string) cardForwardFlags {
	var card struct {
		Config cardForwardFlags `json:"config"`
	}
	if json.Unmarshal([]byte(content), &card) != nil {
		return cardForwardFlags{}
	}
	return card.Config
}

// CardAppID identifies the application that owns an interactive message's callbacks.
func (m Message) CardAppID() string {
	if m.Forwarded {
		return m.ResourceAppID
	}
	return m.AppID
}

// CardActionsEnabled reports whether a visible interactive message can emit a callback.
func (m Message) CardActionsEnabled() bool {
	return m.UpperMessageID == "" && (!m.Forwarded || (m.CardAppID() != "" && forwardingFlags(m.Content).EnableInteraction))
}

func resourceOwner(m Message) string {
	if m.ResourceAppID != "" {
		return m.ResourceAppID
	}
	return m.AppID
}

// IMMessageItems returns the requested message and all visible bundle children.
func (s *Store) IMMessageItems(actor IMActor, id string) ([]Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.imMessage(actor, id)
	if err != nil {
		return nil, err
	}
	result := make([]Message, 0, m.forwardedCount+1)
	var appendItems func(Message)
	appendItems = func(item Message) {
		result = append(result, s.renderMessage(actor.AppID, item))
		if !item.Deleted {
			for _, child := range item.forwardedIDs {
				appendItems(s.messages.items[child])
			}
		}
	}
	appendItems(m)
	return result, nil
}
