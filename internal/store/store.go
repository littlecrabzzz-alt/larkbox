package store

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
)

const defaultTraceCapacity = 256

// TraceBodyLimit caps each request and response body stored in the audit ring.
const TraceBodyLimit = 64 * 1024

var (
	// ErrNotFound indicates the requested entity does not exist.
	ErrNotFound = errors.New("entity not found")
	// ErrAlreadyExists indicates a create would replace an existing entity.
	ErrAlreadyExists = errors.New("entity already exists")
	// ErrStaleSequence indicates a card update did not advance the sequence.
	ErrStaleSequence = errors.New("card sequence must increase")
	// ErrTokenRevoked indicates the credential is no longer usable.
	ErrTokenRevoked = errors.New("token revoked")
	// ErrTokenConsumed indicates the OAuth credential was already exchanged.
	ErrTokenConsumed = errors.New("token consumed")
	// ErrTokenExpired indicates the credential reached its expiry time.
	ErrTokenExpired = errors.New("token expired")
	// ErrTokenOwner indicates a token has a missing or unknown owner.
	ErrTokenOwner = errors.New("token owner does not exist")
	// ErrTokenBinding indicates a token belongs to a different app or user.
	ErrTokenBinding = errors.New("token owner does not match expected binding")
)

// Collection provides snapshot CRUD by primary key. Values returned to callers
// are independent of the backing store.
type Collection[T any] struct {
	mu      *sync.RWMutex
	items   map[string]T
	key     func(T) string
	clone   func(T) T
	index   func(previous, current T)
	prepare func(*T)
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
	c.put(value)
}

// put requires the collection lock.
func (c *Collection[T]) put(value T) {
	key := c.key(value)
	if c.prepare != nil {
		c.prepare(&value)
	}
	if c.index != nil {
		c.index(c.items[key], value)
	}
	c.items[key] = c.copy(value)
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
	previous, ok := c.items[key]
	if ok && c.index != nil {
		var zero T
		c.index(previous, zero)
	}
	delete(c.items, key)
	return ok
}

// Store owns the sandbox's in-memory state.
type Store struct {
	mu                  sync.RWMutex
	resetGate           sync.RWMutex
	resetEpoch          uint64
	now                 func() time.Time
	seed                config.Config
	userSeedOrder       []User
	apps                Collection[App]
	users               Collection[User]
	chats               Collection[Chat]
	messages            Collection[Message]
	messageUUIDs        map[messageRequest]messageDedup
	chatMessages        map[string]map[string]struct{}
	threadMessages      map[string]map[string]struct{}
	messageOrder        uint64
	revision            uint64
	nextDedupCleanup    time.Time
	tokens              map[string]Token
	media               Collection[Media]
	cards               Collection[Card]
	cardUUIDs           map[cardRequest]cardReplay
	cardTemplates       map[templateKey]string
	latestTemplates     map[string]string
	nextCardUUIDCleanup time.Time
	traces              []Trace
	tracePos            int
	traceEpoch          uint64
}

// New creates an empty store. A nil clock uses time.Now. A supplied clock
// must support concurrent calls and must not call back into the store.
// Nonpositive trace capacities use the default ring capacity.
func New(now func() time.Time, traceCapacity int) *Store {
	if now == nil {
		now = time.Now
	}
	if traceCapacity <= 0 {
		traceCapacity = defaultTraceCapacity
	}
	s := &Store{now: now, tokens: make(map[string]Token), traces: make([]Trace, 0, traceCapacity)}
	s.messageUUIDs = make(map[messageRequest]messageDedup)
	s.chatMessages = make(map[string]map[string]struct{})
	s.threadMessages = make(map[string]map[string]struct{})
	s.apps = newCollection(&s.mu, func(a App) string { return a.AppID }, cloneApp)
	s.users = newCollection(&s.mu, func(u User) string { return u.OpenID }, nil)
	s.chats = newCollection(&s.mu, func(c Chat) string { return c.ChatID }, cloneChat)
	s.messages = newCollection(&s.mu, func(m Message) string { return m.ID }, cloneMessage)
	s.messages.index = s.indexMessage
	s.messages.prepare = func(m *Message) {
		s.revision++
		m.Revision = s.revision
	}
	s.media = newCollection(&s.mu, func(m Media) string { return m.Key }, cloneMedia)
	s.cardUUIDs = make(map[cardRequest]cardReplay)
	s.cardTemplates = make(map[templateKey]string)
	s.latestTemplates = make(map[string]string)
	s.cards = newCollection(&s.mu, func(c Card) string { return c.ID }, cloneCard)
	s.cards.prepare = func(card *Card) {
		s.revision++
		card.Revision = s.revision
	}
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

// Media returns the image and file collection.
func (s *Store) Media() *Collection[Media] { return &s.media }

// Now returns the current time according to the store clock.
func (s *Store) Now() time.Time { return s.now() }

// PutToken creates or replaces a credential whose referenced owners exist.
func (s *Store) PutToken(token Token) error {
	return s.PutTokens(token)
}

// PutTokens installs credentials together after validating every owner.
func (s *Store) PutTokens(tokens ...Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, token := range tokens {
		if !s.tokenOwnersExist(token) {
			return ErrTokenOwner
		}
	}
	for _, token := range tokens {
		s.tokens[token.Value] = token
	}
	return nil
}

// DeleteToken removes a credential and reports whether it existed.
func (s *Store) DeleteToken(value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.tokens[value]
	delete(s.tokens, value)
	return ok
}

// CheckToken returns a credential if its owners exist and it is currently valid.
// For an expired, revoked, or consumed credential, it also returns stored metadata.
// If an owner is missing, it returns only the stored kind with ErrTokenOwner.
func (s *Store) CheckToken(value string) (Token, error) {
	return s.CheckTokenBinding(value, "", "")
}

// CheckTokenBinding atomically validates a credential, its owners, and its
// expected app and user bindings. An empty expected field imposes no constraint.
// If an owner is missing, it returns only the stored kind with ErrTokenOwner.
func (s *Store) CheckTokenBinding(value, appID, userOpenID string) (Token, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkTokenBinding(value, appID, userOpenID, s.Now())
}

// checkTokenBinding requires the store lock.
func (s *Store) checkTokenBinding(value, appID, userOpenID string, now time.Time) (Token, error) {
	token, ok := s.tokens[value]
	switch {
	case !ok:
		return Token{}, ErrNotFound
	case !s.tokenOwnersExist(token):
		return Token{Kind: token.Kind}, ErrTokenOwner
	case appID != "" && token.AppID != appID, userOpenID != "" && token.UserOpenID != userOpenID:
		return Token{}, ErrTokenBinding
	case token.Revoked:
		return token, ErrTokenRevoked
	case token.Consumed:
		return token, ErrTokenConsumed
	case !now.Before(token.ExpiresAt):
		return token, ErrTokenExpired
	default:
		return token, nil
	}
}

// tokenOwnersExist requires the store lock.
func (s *Store) tokenOwnersExist(token Token) bool {
	if _, ok := s.apps.items[token.AppID]; !ok || token.AppID == "" {
		return false
	}
	if token.Kind != UserToken && token.UserOpenID == "" {
		return true
	}
	_, ok := s.users.items[token.UserOpenID]
	return ok && token.UserOpenID != ""
}

// RevokeToken marks a credential as revoked without deleting its metadata.
func (s *Store) RevokeToken(value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.tokens[value]
	if ok {
		token.Revoked = true
		s.tokens[value] = token
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
		s.chats.put(chat)
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
		s.chats.put(chat)
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
	card.CreatedAt = s.Now()
	if card.StreamingMode {
		card.StreamingSince = card.CreatedAt
	}
	s.cards.put(card)
	return nil
}

// Card returns an independent snapshot of the requested CardKit card.
func (s *Store) Card(id string) (Card, bool) { return s.cards.Get(id) }

// Cards returns independent snapshots of all CardKit cards.
func (s *Store) Cards() []Card { return s.cards.List() }

// UpdateCard atomically replaces a card if its sequence has strictly advanced.
func (s *Store) UpdateCard(card Card) error {
	return s.ChangeCard(card.AppID, card.ID, CardMutation{Sequence: card.Sequence}, func(current *Card, _ time.Time) error {
		*current = card
		return nil
	})
}

// DeleteCard removes the card and reports whether it existed.
func (s *Store) DeleteCard(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.cards.items[id]; !exists {
		return false
	}
	delete(s.cards.items, id)
	maps.DeleteFunc(s.cardUUIDs, func(key cardRequest, _ cardReplay) bool { return key.cardID == id })
	return true
}

// Seed replaces the store contents and installs the baseline for future resets.
func (s *Store) Seed(cfg config.Config) {
	s.resetGate.Lock()
	defer s.resetGate.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seed = cloneSeed(cfg)
	s.reset(s.seed)
}

// ResetTo replaces runtime state while preserving the original reset baseline.
func (s *Store) ResetTo(cfg config.Config) {
	s.resetGate.Lock()
	defer s.resetGate.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reset(cfg)
}

func cloneSeed(cfg config.Config) config.Config {
	cfg.Apps = slices.Clone(cfg.Apps)
	for i, app := range cfg.Apps {
		cfg.Apps[i] = cloneApp(app)
	}
	cfg.Users = slices.Clone(cfg.Users)
	cfg.Chats = slices.Clone(cfg.Chats)
	for i, chat := range cfg.Chats {
		cfg.Chats[i] = cloneChat(chat)
	}
	cfg.CardTemplates = slices.Clone(cfg.CardTemplates)
	return cfg
}

// Reset atomically restores the seed baseline and clears runtime state.
func (s *Store) Reset() {
	s.resetGate.Lock()
	defer s.resetGate.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reset(s.seed)
}

func (s *Store) reset(seed config.Config) {
	s.resetEpoch++
	s.cardTemplates = make(map[templateKey]string)
	s.latestTemplates = make(map[string]string)
	s.registerTemplates(seed.CardTemplates)
	clear(s.apps.items)
	clear(s.users.items)
	s.userSeedOrder = slices.Clone(seed.Users)
	clear(s.chats.items)
	clear(s.messages.items)
	clear(s.messageUUIDs)
	clear(s.chatMessages)
	clear(s.threadMessages)
	s.messageOrder = 0
	s.nextDedupCleanup = time.Time{}
	clear(s.tokens)
	clear(s.media.items)
	clear(s.cards.items)
	clear(s.cardUUIDs)
	s.nextCardUUIDCleanup = time.Time{}
	s.clearTraces()
	for _, app := range seed.Apps {
		s.apps.items[app.AppID] = cloneApp(app)
	}
	for _, user := range seed.Users {
		s.users.items[user.OpenID] = user
	}
	for _, chat := range seed.Chats {
		s.chats.items[chat.ChatID] = cloneChat(chat)
	}
}

// AppendTrace records an exchange, evicting the oldest trace when full.
func (s *Store) AppendTrace(trace Trace) {
	s.AppendTraceAt(s.TraceEpoch(), trace, Redactor{})
}

// AppendTraceAt records an exchange only if the ring has not been cleared since admission.
func (s *Store) AppendTraceAt(epoch uint64, trace Trace, redactor Redactor) {
	trace.RequestHeaders = redactor.Headers(trace.RequestHeaders)
	trace.ResponseHeaders = redactor.Headers(trace.ResponseHeaders)
	trace.RequestBody, trace.RequestBodyTruncated = redactor.captureTraceBody(trace.RequestBody, trace.RequestBodyTruncated)
	trace.ResponseBody, trace.ResponseBodyTruncated = redactor.captureTraceBody(trace.ResponseBody, trace.ResponseBodyTruncated)
	trace.Error = redactor.Replace(trace.Error)
	trace.Error = trace.Error[:min(len(trace.Error), TraceBodyLimit)]
	trace = cloneTrace(trace)
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch != s.traceEpoch {
		return
	}
	if len(s.traces) < cap(s.traces) {
		s.traces = append(s.traces, trace)
		return
	}
	s.traces[s.tracePos] = trace
	s.tracePos = (s.tracePos + 1) % len(s.traces)
}

// TraceFilter selects traces by exact app and method and by path prefix.
// A positive Limit returns the most recent matching traces.
type TraceFilter struct {
	Direction  TraceDirection
	EventType  string
	EventID    string
	Channel    string
	AppID      string
	Method     string
	PathPrefix string
	Limit      int
}

// Traces returns all trace snapshots in oldest-to-newest order.
func (s *Store) Traces() []Trace {
	return s.QueryTraces(TraceFilter{})
}

// QueryTraces returns matching snapshots in oldest-to-newest order.
func (s *Store) QueryTraces(filter TraceFilter) []Trace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Trace, 0)
	for i := range len(s.traces) {
		trace := s.traces[(s.tracePos+i)%len(s.traces)]
		if (filter.Direction != "" && trace.Direction != filter.Direction) ||
			(filter.EventType != "" && trace.EventType != filter.EventType) ||
			(filter.EventID != "" && trace.EventID != filter.EventID) ||
			(filter.Channel != "" && trace.Channel != filter.Channel) ||
			(filter.AppID != "" && trace.AppID != filter.AppID) ||
			(filter.Method != "" && trace.Method != filter.Method) ||
			!strings.HasPrefix(trace.Path, filter.PathPrefix) {
			continue
		}
		result = append(result, trace)
	}
	slices.SortStableFunc(result, func(a, b Trace) int { return a.StartedAt.Compare(b.StartedAt) })
	if filter.Limit > 0 && len(result) > filter.Limit {
		result = result[len(result)-filter.Limit:]
	}
	for i := range result {
		result[i] = cloneTrace(result[i])
	}
	return result
}

func cloneApp(app App) App {
	app.RedirectURIs = slices.Clone(app.RedirectURIs)
	return app
}

func cloneChat(chat Chat) Chat {
	chat.MemberIDs = slices.Clone(chat.MemberIDs)
	order := make(map[string]uint64, len(chat.MemberIDs))
	for _, id := range chat.MemberIDs {
		position := chat.MemberOrder[id]
		if position == 0 {
			chat.NextMemberOrder++
			position = chat.NextMemberOrder
		}
		order[id] = position
	}
	chat.MemberOrder = order
	chat.BotManagers = slices.Clone(chat.BotManagers)
	chat.I18nNames = maps.Clone(chat.I18nNames)
	chat.Labels = slices.Clone(chat.Labels)
	chat.ToolkitIDs = slices.Clone(chat.ToolkitIDs)
	chat.ChatTags = slices.Clone(chat.ChatTags)
	if chat.RestrictedModeSetting != nil {
		settings := *chat.RestrictedModeSetting
		chat.RestrictedModeSetting = &settings
	}
	return chat
}

func cloneMessage(message Message) Message {
	message.Mentions = slices.Clone(message.Mentions)
	message.Reactions = slices.Clone(message.Reactions)
	return message
}

func cloneCard(card Card) Card {
	card.JSON = slices.Clone(card.JSON)
	card.TemplateData = slices.Clone(card.TemplateData)
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
