package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// IMActor identifies the caller for membership and ownership checks.
type IMActor struct {
	AppID   string
	OpenID  string
	IsUser  bool
	Sandbox bool
}

// ErrOutsideChat indicates that the caller cannot access the chat.
var ErrOutsideChat = errors.New("caller is outside the chat")

// ErrMessageDeleted indicates that the message has already been recalled.
var ErrMessageDeleted = errors.New("message has been recalled")

// ErrMessageCardNotFound indicates that a message references an unknown card.
var ErrMessageCardNotFound = errors.New("message card not found")

// ErrMessageUUIDConflict indicates that an app reused a live UUID for a different request.
var ErrMessageUUIDConflict = errors.New("message UUID is already in use")

// canReadChat requires the store lock.
func (s *Store) canReadChat(actor IMActor, chat Chat, requireBot bool) bool {
	if chat.ChatMode == "p2p" && chat.AppID != "" && chat.AppID != actor.AppID && (!actor.Sandbox || actor.AppID != "") {
		return false
	}
	if actor.OpenID == "" || !slices.Contains(chat.MemberIDs, actor.OpenID) {
		return false
	}
	if actor.Sandbox {
		return true
	}
	if requireBot || (chat.ChatMode != "p2p" && chat.ChatType != "public") {
		bot := s.apps.items[actor.AppID].BotOpenID
		return bot != "" && slices.Contains(chat.MemberIDs, bot)
	}
	return true
}

// IMChats snapshots all chats the actor can currently access.
func (s *Store) IMChats(actor IMActor) []Chat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	chats := make([]Chat, 0)
	for _, chat := range s.chats.items {
		if s.canReadChat(actor, chat, false) {
			chats = append(chats, cloneChat(chat))
		}
	}
	return chats
}

// IMActivity snapshots a message-order watermark and the latest visible
// activity at or before it. Later sends cannot reorder an existing cursor.
func (s *Store) IMActivity(chats []Chat, cutoff *uint64) (uint64, map[string]time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	watermark := s.messageOrder
	if cutoff != nil {
		watermark = *cutoff
	}
	latest := make(map[string]time.Time, len(chats))
	for _, chat := range chats {
		at := chat.CreatedAt
		for id := range s.chatMessages[chat.ChatID] {
			m := s.messages.items[id]
			if m.Order <= watermark && m.CreatedAt.After(at) {
				at = m.CreatedAt
			}
		}
		latest[chat.ChatID] = at
	}
	return watermark, latest
}

// IMChat gets an accessible chat by ID.
func (s *Store) IMChat(actor IMActor, id string) (Chat, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	chat, err := s.imChat(actor, id)
	return cloneChat(chat), err
}

func (s *Store) imChat(actor IMActor, id string) (Chat, error) {
	chat, ok := s.chats.items[id]
	if !ok {
		return Chat{}, ErrNotFound
	}
	if !s.canReadChat(actor, chat, false) {
		return Chat{}, ErrOutsideChat
	}
	return chat, nil
}

// ChangeIMChat commits a membership change after checking access. The callback
// works on a copy under the lock and must not call back into the store.
func (s *Store) ChangeIMChat(actor IMActor, id string, change func(*Chat) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, err := s.imChat(actor, id)
	if err != nil {
		return err
	}
	chat = cloneChat(chat)
	if err := change(&chat); err != nil {
		return err
	}
	s.chats.put(chat)
	return nil
}

// EnsureP2PChat reuses a matching seeded or API-created direct conversation.
func (s *Store) EnsureP2PChat(actor IMActor, chat Chat) (Chat, error) {
	if len(chat.MemberIDs) != 2 || chat.MemberIDs[0] == "" || chat.MemberIDs[1] == "" ||
		chat.MemberIDs[0] == chat.MemberIDs[1] || !slices.Contains(chat.MemberIDs, actor.OpenID) {
		return Chat{}, ErrOutsideChat
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	direct, found := s.resolveP2PChat(actor, chat)
	if !found {
		direct.CreatedAt = s.Now()
		s.chats.put(direct)
	}
	return cloneChat(direct), nil
}

// resolveP2PChat requires the store lock. It computes the canonical chat ID
// without creating a chat, so rejected sends leave no new conversation.
func (s *Store) resolveP2PChat(actor IMActor, chat Chat) (Chat, bool) {
	members := slices.Clone(chat.MemberIDs)
	slices.Sort(members)
	hash := sha256.Sum256([]byte(actor.AppID + "\x00" + members[0] + "\x00" + members[1]))
	chat.ChatID = fmt.Sprintf("oc_%x", hash[:16])
	if existing, ok := s.chats.items[chat.ChatID]; ok {
		return existing, true
	}
	// ponytail: seeded direct chats are few. Index seeded pairs if seed sizes grow.
	for _, key := range slices.Sorted(maps.Keys(s.chats.items)) {
		candidate := s.chats.items[key]
		if candidate.ChatMode == "p2p" && s.canReadChat(actor, candidate, false) &&
			len(candidate.MemberIDs) == 2 && slices.Contains(candidate.MemberIDs, members[0]) &&
			slices.Contains(candidate.MemberIDs, members[1]) {
			return candidate, true
		}
	}
	chat.AppID = actor.AppID
	return chat, false
}

type messageRequest struct {
	appID string
	reply bool
	uuid  string
}

type messageDedup struct {
	id      string
	target  string
	actor   IMActor
	expires time.Time
}

// NewMessage selects the target and optional reply and deduplication metadata.
type NewMessage struct {
	ChatID        string
	Peer          string
	ParentID      string
	ReplyInThread bool
	UUID          string
	MsgType       string
	Content       string
	CardID        string
	Mentions      []Mention
}

// CreateIMMessage validates the target and saves the message, reply metadata,
// and one-hour deduplication record in the same critical section.
func (s *Store) CreateIMMessage(actor IMActor, req NewMessage) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.createIMMessage(actor, req)
}

// CreateSandboxMessage commits a user message and captures its bot recipients
// before any concurrent membership change can alter the result.
func (s *Store) CreateSandboxMessage(actor IMActor, req NewMessage) (Message, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	actor.Sandbox = true
	message, err := s.createIMMessage(actor, req)
	if err != nil {
		return Message{}, nil, err
	}
	return message, s.chatBotIDs(s.chats.items[message.ChatID]), nil
}

func (s *Store) createIMMessage(actor IMActor, req NewMessage) (Message, error) {
	if actor.Sandbox {
		user, ok := s.users.items[actor.OpenID]
		if !ok || user.Inactive {
			return Message{}, ErrOutsideChat
		}
	}
	var chat Chat
	var parent Message
	var err error
	var createDirect bool
	switch {
	case req.ParentID != "":
		parent, err = s.imMessage(actor, req.ParentID)
		if err == nil && actor.Sandbox && parent.ChatID != req.ChatID {
			err = ErrOutsideChat
		}
		if err == nil && parent.Deleted {
			err = ErrMessageDeleted
		}
	case actor.Sandbox && req.Peer != "":
		bot, ok := s.apps.items[actor.AppID]
		if !ok || bot.BotOpenID != req.Peer {
			return Message{}, ErrOutsideChat
		}
		var found bool
		chat, found = s.resolveP2PChat(actor, Chat{
			ChatMode: "p2p", OwnerID: actor.OpenID, MemberIDs: []string{actor.OpenID, bot.BotOpenID},
		})
		createDirect = !found
		if found && (!s.canReadChat(actor, chat, false) || !slices.Contains(chat.MemberIDs, bot.BotOpenID)) {
			err = ErrOutsideChat
		}
	case req.Peer != "":
		if peer, ok := s.users.items[req.Peer]; !ok || peer.Inactive || actor.OpenID == req.Peer {
			err = ErrOutsideChat
		} else {
			var found bool
			chat, found = s.resolveP2PChat(actor, Chat{
				ChatMode: "p2p", OwnerID: req.Peer, MemberIDs: []string{actor.OpenID, req.Peer},
			})
			createDirect = !found
			if found && !s.canReadChat(actor, chat, false) {
				err = ErrOutsideChat
			}
		}
	default:
		chat, err = s.imChat(actor, req.ChatID)
	}
	if err != nil {
		return Message{}, err
	}

	if actor.Sandbox {
		target := chat
		if req.ParentID != "" {
			target = s.chats.items[parent.ChatID]
		}
		for _, mention := range req.Mentions {
			if !slices.Contains(target.MemberIDs, mention.OpenID) {
				return Message{}, ErrOutsideChat
			}
		}
		if card, ok := s.cards.items[req.CardID]; ok && actor.AppID == "" {
			app := s.apps.items[card.AppID]
			if slices.Contains(target.MemberIDs, app.BotOpenID) {
				actor.AppID = card.AppID
			}
		}
	}

	if req.CardID != "" {
		card, err := s.liveCard(actor.AppID, req.CardID, s.Now())
		if errors.Is(err, ErrNotFound) {
			return Message{}, ErrMessageCardNotFound
		} else if err != nil {
			return Message{}, err
		}
		if actor.Sandbox && protocol.HasAssignedMention([]byte(cardContent(card))) {
			return Message{}, errors.New("card content must not contain preassigned @_user_N mention keys")
		}
	}

	now := s.Now()
	key := messageRequest{appID: actor.AppID, reply: req.ParentID != "", uuid: req.UUID}
	target := chat.ChatID
	if key.reply {
		target = parent.ID
	}
	if req.UUID != "" {
		if !now.Before(s.nextDedupCleanup) {
			maps.DeleteFunc(s.messageUUIDs, func(_ messageRequest, record messageDedup) bool {
				return !now.Before(record.expires)
			})
			s.nextDedupCleanup = now.Add(time.Minute)
		}
		if record, exists := s.messageUUIDs[key]; exists && now.Before(record.expires) {
			if record.target != target || record.actor != actor {
				return Message{}, ErrMessageUUIDConflict
			}
			m, err := s.imMessage(actor, record.id)
			return s.renderMessage(actor.AppID, m), err
		}
	}

	m := Message{
		ID: protocol.NewID(protocol.MessageID), ChatID: chat.ChatID, AppID: actor.AppID,
		SenderOpenID: actor.OpenID, SenderIsUser: actor.IsUser, MsgType: req.MsgType,
		Content: req.Content, CardID: req.CardID, Mentions: slices.Clone(req.Mentions), CreatedAt: now, UpdatedAt: now,
	}
	switch {
	case req.ParentID != "":
		m.ChatID, m.ParentID, m.RootID = parent.ChatID, parent.ID, parent.RootID
		if m.RootID == "" {
			m.RootID = parent.ID
		}
		m.ThreadID = parent.ThreadID
		if m.ThreadID == "" && req.ReplyInThread {
			root, exists := s.messages.items[m.RootID]
			if !exists {
				return Message{}, ErrNotFound
			}
			if root.ThreadID == "" {
				root.ThreadID = protocol.NewID(protocol.ThreadID)
				s.messages.put(root)
			}
			m.ThreadID = root.ThreadID
		}
		if m.ThreadID != "" {
			m.ParentID = m.RootID
		}
	case createDirect:
		chat.CreatedAt = now
		s.chats.put(chat)
	case chat.GroupMessageType == "thread":
		m.ThreadID = protocol.NewID(protocol.ThreadID)
	}
	s.messageOrder++
	m.Order = s.messageOrder
	s.messages.put(m)
	if req.UUID != "" {
		s.messageUUIDs[key] = messageDedup{id: m.ID, target: target, actor: actor, expires: now.Add(time.Hour)}
	}
	return s.renderMessage(actor.AppID, s.messages.items[m.ID]), nil
}

// IMMessage returns a message visible to the caller.
func (s *Store) IMMessage(actor IMActor, id string) (Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.imMessage(actor, id)
	return s.renderMessage(actor.AppID, m), err
}

// IMResourceMessage also requires the app bot to be a member for user tokens
// and for public or direct conversations.
func (s *Store) IMResourceMessage(actor IMActor, id string) (Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.imMessage(actor, id)
	if err != nil {
		return Message{}, err
	}
	if !s.canReadChat(actor, s.chats.items[m.ChatID], true) {
		return Message{}, ErrOutsideChat
	}
	return cloneMessage(m), nil
}

func (s *Store) imMessage(actor IMActor, id string) (Message, error) {
	m, ok := s.messages.items[id]
	if !ok {
		return Message{}, ErrNotFound
	}
	if _, err := s.imChat(actor, m.ChatID); err != nil {
		return Message{}, err
	}
	return m, nil
}

// ChangeIMMessage commits a change to a live message. The callback works on a
// copy under the lock and must not call back into the store.
func (s *Store) ChangeIMMessage(actor IMActor, id string, change func(*Message, Chat) error) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changeIMMessage(actor, id, change)
}

// RecallIMMessage marks a message recalled and captures the receiving apps
// from the same chat membership snapshot used to authorize the mutation.
// The authorization callback runs under the lock and must not call the store.
func (s *Store) RecallIMMessage(actor IMActor, id string, authorize func(*Message, Chat) error) (Message, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.changeIMMessage(actor, id, func(m *Message, chat Chat) error {
		if err := authorize(m, chat); err != nil {
			return err
		}
		m.Deleted, m.UpdatedAt = true, s.Now()
		return nil
	})
	if err != nil {
		return Message{}, nil, err
	}
	return m, s.chatBotIDs(s.chats.items[m.ChatID]), nil
}

func (s *Store) chatBotIDs(chat Chat) []string {
	var recipients []string
	for _, app := range s.apps.items {
		if chat.ChatMode == "p2p" && chat.AppID != "" && chat.AppID != app.AppID {
			continue
		}
		if app.BotOpenID != "" && slices.Contains(chat.MemberIDs, app.BotOpenID) {
			recipients = append(recipients, app.AppID)
		}
	}
	slices.Sort(recipients)
	return recipients
}

func (s *Store) changeIMMessage(actor IMActor, id string, change func(*Message, Chat) error) (Message, error) {
	m, err := s.imMessage(actor, id)
	if err != nil {
		return Message{}, err
	}
	if m.Deleted {
		return Message{}, ErrMessageDeleted
	}
	m = cloneMessage(m)
	if err := change(&m, cloneChat(s.chats.items[m.ChatID])); err != nil {
		return Message{}, err
	}
	s.messages.put(m)
	return cloneMessage(s.messages.items[m.ID]), nil
}

// indexMessage requires the store lock.
func (s *Store) indexMessage(previous, current Message) {
	updateMessageIndex(s.chatMessages, previous.ChatID, current.ChatID, previous.ID, current.ID)
	updateMessageIndex(s.threadMessages, previous.ThreadID, current.ThreadID, previous.ID, current.ID)
}

func updateMessageIndex(index map[string]map[string]struct{}, oldKey, newKey, oldID, newID string) {
	if oldKey == newKey && oldID == newID {
		return
	}
	if oldKey != "" {
		delete(index[oldKey], oldID)
		if len(index[oldKey]) == 0 {
			delete(index, oldKey)
		}
	}
	if newKey != "" {
		if index[newKey] == nil {
			index[newKey] = make(map[string]struct{})
		}
		index[newKey][newID] = struct{}{}
	}
}

// IMHistory snapshots messages in an accessible chat or thread.
func (s *Store) IMHistory(actor IMActor, containerType, id string) ([]Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.threadMessages[id]
	if containerType == "chat" {
		if _, err := s.imChat(actor, id); err != nil {
			return nil, err
		}
		ids = s.chatMessages[id]
	} else if len(ids) == 0 {
		return nil, ErrNotFound
	}
	result := make([]Message, 0, len(ids))
	for id := range ids {
		m := s.messages.items[id]
		if containerType == "chat" {
			if m.ThreadID != "" && m.ParentID != "" {
				continue
			}
		} else if _, err := s.imChat(actor, m.ChatID); err != nil {
			return nil, err
		}
		result = append(result, s.renderMessage(actor.AppID, m))
	}
	return result, nil
}
