package store

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"
)

var (
	// ErrCardOwner indicates the card belongs to a different app.
	ErrCardOwner = errors.New("card belongs to a different app")
	// ErrCardExpired indicates a CardKit entity is past its lifetime.
	ErrCardExpired = errors.New("card entity has expired")
	// ErrCardUUIDConflict indicates a live UUID belongs to a different request.
	ErrCardUUIDConflict = errors.New("card UUID conflict")
)

const (
	cardUUIDWindow  = time.Hour
	cardLifetime    = 14 * 24 * time.Hour
	streamingWindow = 10 * time.Minute
)

type cardRequest struct {
	appID, cardID, operation, uuid string
}

type cardReplay struct {
	fingerprint [32]byte
	expires     time.Time
}

// CardMutation identifies a sequenced operation and its optional replay key.
type CardMutation struct {
	Operation, UUID string
	Sequence        int64
	Fingerprint     [32]byte
}

// ChangeCard checks ownership, deduplicates successful requests, and applies a
// mutation under the store lock. The callback must not call back into the store.
func (s *Store) ChangeCard(appID, id string, req CardMutation, change func(*Card, time.Time) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.Now()
	card, err := s.liveCard(appID, id, now)
	if err != nil {
		return err
	}
	key := cardRequest{appID, id, req.Operation, req.UUID}
	if replay, exists := s.cardUUIDs[key]; req.UUID != "" && exists && now.Before(replay.expires) {
		if replay.fingerprint != req.Fingerprint {
			return ErrCardUUIDConflict
		}
		return nil
	}
	if req.Sequence <= 0 || req.Sequence <= card.Sequence {
		return ErrStaleSequence
	}
	updated := cloneCard(card)
	if err := change(&updated, now); err != nil {
		return err
	}
	updated.ID, updated.AppID, updated.CreatedAt = card.ID, card.AppID, card.CreatedAt
	updated.Sequence = req.Sequence
	if !updated.StreamingMode {
		updated.StreamingSince = time.Time{}
	} else if !card.StreamingMode || updated.StreamingSince.IsZero() {
		updated.StreamingSince = now
	}
	s.cards.put(updated)
	if req.UUID != "" {
		if !now.Before(s.nextCardUUIDCleanup) {
			maps.DeleteFunc(s.cardUUIDs, func(_ cardRequest, replay cardReplay) bool { return !now.Before(replay.expires) })
			s.nextCardUUIDCleanup = now.Add(time.Minute)
		}
		s.cardUUIDs[key] = cardReplay{req.Fingerprint, now.Add(cardUUIDWindow)}
	}
	return nil
}

// StreamingExpired reports whether a configured stream has reached its deadline.
func (c Card) StreamingExpired(now time.Time) bool {
	return c.StreamingMode && !now.Before(c.StreamingSince.Add(streamingWindow))
}

func (s *Store) liveCard(appID, id string, now time.Time) (Card, error) {
	card, err := s.ownedCard(appID, id)
	if err != nil {
		return Card{}, err
	}
	if !now.Before(card.CreatedAt.Add(cardLifetime)) {
		return Card{}, ErrCardExpired
	}
	return card, nil
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

type templateKey struct{ id, version string }

// CardTemplate returns the configured definition for a template and version.
func (s *Store) CardTemplate(id, version string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cardTemplates[templateKey{id, version}]
}

// renderMessage resolves a linked entity for its owner in the same snapshot.
func (s *Store) renderMessage(callerAppID string, m Message) Message {
	if callerAppID == m.AppID && m.CardID != "" && !m.Deleted {
		if card, err := s.ownedCard(callerAppID, m.CardID); err == nil {
			m.Content = cardContent(card)
		}
	}
	return cloneMessage(m)
}

func cardContent(card Card) string {
	if card.Type == "template" && len(card.TemplateData) == 0 {
		return `{"type":"template","data":` + string(card.JSON) + "}"
	}
	return string(card.JSON)
}

// CardActionSnapshot binds a click to the message and linked card revisions.
type CardActionSnapshot struct {
	Message      Message
	Operator     User
	cardRevision uint64
}

// CardActionTarget snapshots the clicked message, operator, and entity revision.
func (s *Store) CardActionTarget(messageID, openID string) (CardActionSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, user, err := s.cardActionTarget(messageID, openID)
	if err != nil {
		return CardActionSnapshot{}, err
	}
	target := CardActionSnapshot{Message: cloneMessage(m), Operator: user}
	if m.CardID != "" {
		card, err := s.ownedCard(m.AppID, m.CardID)
		if err != nil {
			return CardActionSnapshot{}, err
		}
		target.cardRevision = card.Revision
	}
	return target, nil
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
func (s *Store) ApplyCardAction(ctx context.Context, target CardActionSnapshot, card Card, reference string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, _, err := s.cardActionTarget(target.Message.ID, target.Operator.OpenID)
	if err != nil {
		return err
	}
	if current.Revision != target.Message.Revision {
		return errors.New("message changed while awaiting card action response")
	}
	now := s.Now()
	var previous Card
	if current.CardID != "" {
		previous, err = s.liveCard(current.AppID, current.CardID, now)
		if err != nil {
			return err
		}
		if previous.Revision != target.cardRevision {
			return errors.New("card entity changed while awaiting card action response")
		}
		if previous.StreamingMode && !previous.StreamingExpired(now) {
			return errors.New("card entity is in streaming mode")
		}
	}
	switch {
	case reference != "":
		if _, err := s.liveCard(current.AppID, reference, now); err != nil {
			return err
		}
		current.CardID = reference
		encoded, _ := json.Marshal(reference)
		current.Content = `{"type":"card","data":{"card_id":` + string(encoded) + "}}"
	case current.CardID != "":
		if card.Type == "card_json" && cardSchema(previous) == "2.0" && cardSchema(card) != "2.0" {
			return errors.New("card JSON 2.0 cannot be replaced with card JSON 1.0")
		}
		card.ID, card.AppID, card.Sequence = previous.ID, previous.AppID, previous.Sequence
		card.CreatedAt = previous.CreatedAt
		if card.StreamingMode {
			card.StreamingSince = now
		}
		s.cards.put(card)
	default:
		current.Content = cardContent(card)
	}
	current.Updated, current.UpdatedAt = true, now
	s.messages.put(current)
	return nil
}

func cardSchema(card Card) string {
	var doc struct {
		Schema string `json:"schema"`
	}
	_ = json.Unmarshal(card.JSON, &doc)
	return doc.Schema
}
