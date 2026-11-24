package store

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"
)

// ErrCardOwner indicates the card belongs to a different app.
var ErrCardOwner = errors.New("card belongs to a different app")

const cardUUIDWindow = time.Hour

type cardRequest struct {
	appID, cardID, operation, uuid string
}

// ChangeCard checks ownership, deduplicates successful requests, and applies a
// mutation under the store lock. If change fails, neither state nor sequence
// advances. The callback must not call back into the store.
func (s *Store) ChangeCard(appID, id, operation, uuid string, sequence int64, change func(*Card) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	card, err := s.ownedCard(appID, id)
	if err != nil {
		return err
	}
	key := cardRequest{appID, id, operation, uuid}
	now := s.Now()
	if uuid != "" && now.Before(s.cardUUIDs[key]) {
		return nil
	}
	if sequence <= 0 || sequence <= card.Sequence {
		return ErrStaleSequence
	}
	card = cloneCard(card)
	if err := change(&card); err != nil {
		return err
	}
	card.Sequence = sequence
	s.cards.put(card)
	if uuid != "" {
		if !now.Before(s.nextCardUUIDCleanup) {
			maps.DeleteFunc(s.cardUUIDs, func(_ cardRequest, expires time.Time) bool { return !now.Before(expires) })
			s.nextCardUUIDCleanup = now.Add(time.Minute)
		}
		s.cardUUIDs[key] = now.Add(cardUUIDWindow)
	}
	return nil
}

// ownedCard requires the store lock.
func (s *Store) ownedCard(appID, id string) (Card, error) {
	card, ok := s.cards.items[id]
	if !ok {
		return Card{}, ErrNotFound
	}
	if card.AppID != appID {
		return Card{}, ErrCardOwner
	}
	return card, nil
}

// renderMessage resolves a linked entity in the same snapshot as the message.
func (s *Store) renderMessage(m Message) Message {
	if m.CardID != "" && !m.Deleted {
		if card, err := s.ownedCard(m.AppID, m.CardID); err == nil {
			m.Content = cardContent(card)
		}
	}
	return cloneMessage(m)
}

func cardContent(card Card) string {
	if card.Type == "template" {
		return `{"type":"template","data":` + string(card.JSON) + "}"
	}
	return string(card.JSON)
}

// CardActionTarget snapshots the clicked message and operator under one lock.
func (s *Store) CardActionTarget(messageID, openID string) (Message, User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, user, err := s.cardActionTarget(messageID, openID)
	return cloneMessage(m), user, err
}

func (s *Store) cardActionTarget(messageID, openID string) (Message, User, error) {
	m, exists := s.messages.items[messageID]
	if !exists {
		return Message{}, User{}, ErrNotFound
	}
	if m.Deleted {
		return Message{}, User{}, ErrMessageDeleted
	}
	if m.MsgType != "interactive" {
		return Message{}, User{}, errors.New("message is not interactive")
	}
	user, exists := s.users.items[openID]
	chat, hasChat := s.chats.items[m.ChatID]
	if !exists || user.Inactive || !hasChat || !slices.Contains(chat.MemberIDs, openID) {
		return Message{}, User{}, ErrOutsideChat
	}
	return m, user, nil
}

// ApplyCardAction commits a validated callback card to the clicked message.
// Inline results also replace any linked entity without consuming its sequence.
// A returned entity reference relinks the message to a card owned by its app.
func (s *Store) ApplyCardAction(ctx context.Context, target Message, operator string, card Card, reference string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, _, err := s.cardActionTarget(target.ID, operator)
	if err != nil {
		return err
	}
	if current.AppID != target.AppID || current.ChatID != target.ChatID || current.Order != target.Order ||
		current.CardID != target.CardID || current.Content != target.Content || !current.UpdatedAt.Equal(target.UpdatedAt) {
		return errors.New("message changed while awaiting card action response")
	}
	switch {
	case reference != "":
		card, err = s.ownedCard(current.AppID, reference)
		if err != nil {
			return err
		}
		current.CardID = reference
	case current.CardID != "":
		previous, err := s.ownedCard(current.AppID, current.CardID)
		if err != nil {
			return err
		}
		card.ID, card.AppID, card.Sequence = previous.ID, previous.AppID, previous.Sequence
		s.cards.put(card)
	}
	current.Content = cardContent(card)
	current.Updated, current.UpdatedAt = true, s.Now()
	s.messages.put(current)
	return nil
}
