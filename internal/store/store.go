package store

import (
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
)

const defaultTraceCapacity = 256

var (
	// ErrNotFound indicates the requested entity does not exist.
	ErrNotFound = errors.New("entity not found")
	// ErrAlreadyExists indicates a create would replace an existing entity.
	ErrAlreadyExists = errors.New("entity already exists")
	// ErrStaleSequence indicates a card update did not advance the sequence.
	ErrStaleSequence = errors.New("card sequence must increase")
	// ErrTokenRevoked indicates the credential is no longer usable.
	ErrTokenRevoked = errors.New("token revoked")
	// ErrTokenExpired indicates the credential reached its expiry time.
	ErrTokenExpired = errors.New("token expired")
)

// Collection provides snapshot CRUD by primary key. Values returned to callers
// are independent of the backing store.
type Collection[T any] struct {
	mu    *sync.RWMutex
	items map[string]T
	key   func(T) string
	clone func(T) T
}

func newCollection[T any](mu *sync.RWMutex, key func(T) string, clone func(T) T) Collection[T] {
	return Collection[T]{mu: mu, items: make(map[string]T), key: key, clone: clone}
}

func (c *Collection[T]) copy(value T) T {
	if c.clone != nil {
		return c.clone(value)
	}
	return value
}

// Put creates or replaces a value at its primary key.
func (c *Collection[T]) Put(value T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[c.key(value)] = c.copy(value)
}

// Get returns an independent snapshot of the value at key.
func (c *Collection[T]) Get(key string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.items[key]
	return c.copy(value), ok
}

// List returns independent snapshots ordered by primary key.
func (c *Collection[T]) List() []T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]T, 0, len(c.items))
	for _, key := range slices.Sorted(maps.Keys(c.items)) {
		result = append(result, c.copy(c.items[key]))
	}
	return result
}

// Delete removes a value and reports whether it existed.
func (c *Collection[T]) Delete(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[key]
	delete(c.items, key)
	return ok
}

// Store owns the sandbox's in-memory state.
type Store struct {
	mu       sync.RWMutex
	now      func() time.Time
	seed     config.Config
	apps     Collection[App]
	users    Collection[User]
	chats    Collection[Chat]
	messages Collection[Message]
	tokens   Collection[Token]
	media    Collection[Media]
	cards    Collection[Card]
	traces   []Trace
	tracePos int
}

// New creates an empty store. A nil clock uses time.Now. Nonpositive trace
// capacities use the default ring capacity.
func New(now func() time.Time, traceCapacity int) *Store {
	if now == nil {
		now = time.Now
	}
	if traceCapacity <= 0 {
		traceCapacity = defaultTraceCapacity
	}
	s := &Store{now: now, traces: make([]Trace, 0, traceCapacity)}
	s.apps = newCollection(&s.mu, func(a App) string { return a.AppID }, nil)
	s.users = newCollection(&s.mu, func(u User) string { return u.OpenID }, nil)
	s.chats = newCollection(&s.mu, func(c Chat) string { return c.ChatID }, cloneChat)
	s.messages = newCollection(&s.mu, func(m Message) string { return m.ID }, cloneMessage)
	s.tokens = newCollection(&s.mu, func(t Token) string { return t.Value }, nil)
	s.media = newCollection(&s.mu, func(m Media) string { return m.Key }, cloneMedia)
	s.cards = newCollection(&s.mu, func(c Card) string { return c.ID }, cloneCard)
	return s
}

// Apps returns the application and bot collection.
func (s *Store) Apps() *Collection[App] { return &s.apps }

// Users returns the user collection.
func (s *Store) Users() *Collection[User] { return &s.users }

// Chats returns the conversation collection.
func (s *Store) Chats() *Collection[Chat] { return &s.chats }

// Messages returns the IM message collection.
func (s *Store) Messages() *Collection[Message] { return &s.messages }

// Tokens returns the credential collection.
func (s *Store) Tokens() *Collection[Token] { return &s.tokens }

// Media returns the image and file collection.
func (s *Store) Media() *Collection[Media] { return &s.media }

// Now returns the current time according to the store clock.
func (s *Store) Now() time.Time { return s.now() }

// CheckToken returns a credential if it exists and is valid at the store time.
// For expired or revoked credentials, it also returns the stored metadata.
func (s *Store) CheckToken(value string) (Token, error) {
	token, ok := s.tokens.Get(value)
	switch {
	case !ok:
		return Token{}, ErrNotFound
	case token.Revoked:
		return token, ErrTokenRevoked
	case !s.Now().Before(token.ExpiresAt):
		return token, ErrTokenExpired
	default:
		return token, nil
	}
}

// RevokeToken marks a credential as revoked without deleting its metadata.
func (s *Store) RevokeToken(value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.tokens.items[value]
	if ok {
		token.Revoked = true
		s.tokens.items[value] = token
	}
	return ok
}

// AddChatMember adds a member once and reports whether the chat exists.
func (s *Store) AddChatMember(chatID, openID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, ok := s.chats.items[chatID]
	if ok && !slices.Contains(chat.MemberIDs, openID) {
		chat.MemberIDs = append(chat.MemberIDs, openID)
		s.chats.items[chatID] = chat
	}
	return ok
}

// RemoveChatMember removes a member and reports whether the chat exists.
func (s *Store) RemoveChatMember(chatID, openID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, ok := s.chats.items[chatID]
	if ok {
		chat.MemberIDs = slices.DeleteFunc(chat.MemberIDs, func(member string) bool { return member == openID })
		s.chats.items[chatID] = chat
	}
	return ok
}

// CreateCard inserts a card with its initial sequence.
func (s *Store) CreateCard(card Card) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cards.items[card.ID]; ok {
		return ErrAlreadyExists
	}
	s.cards.items[card.ID] = cloneCard(card)
	return nil
}

// Card returns an independent snapshot of the requested CardKit card.
func (s *Store) Card(id string) (Card, bool) { return s.cards.Get(id) }

// Cards returns independent snapshots of all CardKit cards.
func (s *Store) Cards() []Card { return s.cards.List() }

// UpdateCard atomically replaces a card if its sequence has strictly advanced.
func (s *Store) UpdateCard(card Card) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.cards.items[card.ID]
	if !ok {
		return ErrNotFound
	}
	if card.Sequence <= current.Sequence {
		return ErrStaleSequence
	}
	s.cards.items[card.ID] = cloneCard(card)
	return nil
}

// DeleteCard removes the card and reports whether it existed.
func (s *Store) DeleteCard(id string) bool { return s.cards.Delete(id) }

// Seed replaces the store contents and installs the baseline for future resets.
func (s *Store) Seed(cfg config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seed.Apps = slices.Clone(cfg.Apps)
	s.seed.Users = slices.Clone(cfg.Users)
	s.seed.Chats = make([]Chat, len(cfg.Chats))
	for i, chat := range cfg.Chats {
		s.seed.Chats[i] = cloneChat(chat)
	}
	s.reset()
}

// Reset atomically restores the seed baseline and clears runtime state.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reset()
}

func (s *Store) reset() {
	clear(s.apps.items)
	clear(s.users.items)
	clear(s.chats.items)
	clear(s.messages.items)
	clear(s.tokens.items)
	clear(s.media.items)
	clear(s.cards.items)
	clear(s.traces)
	s.traces = s.traces[:0]
	s.tracePos = 0
	for _, app := range s.seed.Apps {
		s.apps.items[app.AppID] = app
	}
	for _, user := range s.seed.Users {
		s.users.items[user.OpenID] = user
	}
	for _, chat := range s.seed.Chats {
		s.chats.items[chat.ChatID] = cloneChat(chat)
	}
}

// AppendTrace records an exchange, evicting the oldest trace when full.
func (s *Store) AppendTrace(trace Trace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	trace = cloneTrace(trace)
	if len(s.traces) < cap(s.traces) {
		s.traces = append(s.traces, trace)
		return
	}
	s.traces[s.tracePos] = trace
	s.tracePos = (s.tracePos + 1) % len(s.traces)
}

// Traces returns trace snapshots in oldest-to-newest order.
func (s *Store) Traces() []Trace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Trace, len(s.traces))
	for i := range result {
		result[i] = cloneTrace(s.traces[(s.tracePos+i)%len(s.traces)])
	}
	return result
}

func cloneChat(chat Chat) Chat {
	chat.MemberIDs = slices.Clone(chat.MemberIDs)
	return chat
}

func cloneMessage(message Message) Message {
	message.Mentions = slices.Clone(message.Mentions)
	message.Reactions = slices.Clone(message.Reactions)
	return message
}

func cloneCard(card Card) Card {
	card.JSON = slices.Clone(card.JSON)
	card.Elements = maps.Clone(card.Elements)
	return card
}

func cloneMedia(media Media) Media {
	media.Bytes = slices.Clone(media.Bytes)
	return media
}

func cloneTrace(trace Trace) Trace {
	trace.RequestHeaders = trace.RequestHeaders.Clone()
	trace.ResponseHeaders = trace.ResponseHeaders.Clone()
	trace.RequestBody = slices.Clone(trace.RequestBody)
	trace.ResponseBody = slices.Clone(trace.ResponseBody)
	if trace.Code != nil {
		code := *trace.Code
		trace.Code = &code
	}
	return trace
}
