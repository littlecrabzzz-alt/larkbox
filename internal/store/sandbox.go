package store

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// UpsertSeed applies a validated runtime seed without changing the reset baseline.
func (s *Store) UpsertSeed(cfg config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applySeed(cfg, true)
}

// ValidateSeed checks a replacement seed independently of the current state.
func ValidateSeed(cfg config.Config) error {
	return New(nil, 0).applySeed(cfg, false)
}

func (s *Store) applySeed(cfg config.Config, upsert bool) error {
	if err := cfg.ValidateSeeds(); err != nil {
		return err
	}
	apps, users, chats := maps.Clone(s.apps.items), maps.Clone(s.users.items), maps.Clone(s.chats.items)
	departments := maps.Clone(s.departments.items)
	for _, dept := range cfg.Departments {
		departments[dept.OpenID] = dept
	}
	definitions := maps.Clone(s.approvalDefinitions.items)
	for _, definition := range cfg.ApprovalDefinitions {
		definitions[definition.Code] = definition
	}
	for _, app := range cfg.Apps {
		if strings.TrimSpace(app.AppSecret) == "" || strings.TrimSpace(app.BotOpenID) == "" {
			return errors.New("apps require app_secret and bot_open_id")
		}
		if app.WebhookURL != "" {
			if _, err := config.ParseRedirectURI(app.WebhookURL); err != nil {
				return fmt.Errorf("invalid webhook_url for app %q: %w", app.AppID, err)
			}
		}
		apps[app.AppID] = app
	}
	for _, user := range cfg.Users {
		users[user.OpenID] = user
	}
	for _, chat := range cfg.Chats {
		if previous, ok := chats[chat.ChatID]; ok {
			chat.AppID, chat.CreatedAt = previous.AppID, previous.CreatedAt
		}
		chats[chat.ChatID] = chat
	}
	members := make(map[string]bool, len(users)+len(apps))
	if err := uniqueUsers(users, members); err != nil {
		return err
	}
	for _, app := range apps {
		if app.BotOpenID == "" {
			continue
		}
		if members[app.BotOpenID] {
			return fmt.Errorf("duplicate bot or user open_id %q: %w", app.BotOpenID, ErrAlreadyExists)
		}
		members[app.BotOpenID] = true
	}
	for _, chat := range chats {
		if err := validateSeedChat(chat, members); err != nil {
			return err
		}
	}
	directory := config.Config{
		Apps: slices.Collect(maps.Values(apps)), Users: slices.Collect(maps.Values(users)),
		Departments: slices.Collect(maps.Values(departments)), ApprovalDefinitions: slices.Collect(maps.Values(definitions)),
	}
	if err := directory.ValidateDirectory(); err != nil {
		return err
	}
	if err := directory.ValidateApprovals(); err != nil {
		return err
	}
	if upsert {
		for _, app := range cfg.Apps {
			s.apps.put(app)
		}
		for _, user := range cfg.Users {
			if _, exists := s.users.items[user.OpenID]; !exists {
				s.userSeedOrder = append(s.userSeedOrder, user)
			}
			s.users.put(user)
		}
		for _, chat := range cfg.Chats {
			s.chats.put(chats[chat.ChatID])
		}
		for _, dept := range cfg.Departments {
			s.departments.put(dept)
		}
		for _, definition := range cfg.ApprovalDefinitions {
			s.approvalDefinitions.put(definition)
		}
		s.registerTemplates(cfg.CardTemplates)
	}
	return nil
}

func uniqueUsers(users map[string]User, members map[string]bool) error {
	ids, unionIDs := make(map[string]bool), make(map[string]bool)
	for _, user := range users {
		if (user.UserID != "" && ids[user.UserID]) || (user.UnionID != "" && unionIDs[user.UnionID]) {
			return fmt.Errorf("user_id and union_id must be unique: %w", ErrAlreadyExists)
		}
		members[user.OpenID], ids[user.UserID], unionIDs[user.UnionID] = true, true, true
	}
	return nil
}

func validateSeedChat(chat Chat, known map[string]bool) error {
	seen := make(map[string]bool, len(chat.MemberIDs))
	for _, member := range chat.MemberIDs {
		if !known[member] || seen[member] {
			return fmt.Errorf("chat %q has an unknown or duplicate member", chat.ChatID)
		}
		seen[member] = true
	}
	if chat.OwnerID != "" && (!known[chat.OwnerID] || !seen[chat.OwnerID]) {
		return fmt.Errorf("chat %q owner must be a known member", chat.ChatID)
	}
	if chat.ChatMode == "p2p" && len(seen) != 2 {
		return fmt.Errorf("p2p chat %q must have exactly two members", chat.ChatID)
	}
	return nil
}

func (s *Store) registerTemplates(templates []config.CardTemplate) {
	for _, template := range templates {
		key := templateKey{id: template.TemplateID}
		versionKey := templateKey{id: template.TemplateID, version: template.VersionName}
		s.cardTemplates[versionKey] = template.Data
		// Track the latest version separately from an explicit unversioned entry.
		if versionKey != key {
			s.latestTemplates[template.TemplateID] = template.Data
		}
	}
}

// ExpireTokens expires selected credentials, or every credential for the app
// when values is empty. Every selected credential must belong to the app.
func (s *Store) ExpireTokens(appID string, values []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps.items[appID]; !ok {
		return 0, ErrNotFound
	}
	selected := make(map[string]bool, len(values))
	for _, value := range values {
		if token, ok := s.tokens[value]; !ok || token.AppID != appID {
			return 0, ErrNotFound
		}
		selected[value] = true
	}
	now := s.Now()
	count := 0
	for value, token := range s.tokens {
		if token.AppID == appID && (len(values) == 0 || selected[value]) && now.Before(token.ExpiresAt) {
			token.ExpiresAt = now
			s.tokens[value] = token
			count++
		}
	}
	return count, nil
}

// InspectMessage returns the current raw card representation of a message.
func (s *Store) InspectMessage(id string) (Message, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.messages.items[id]
	return s.renderMessage(m.CardAppID(), m), ok
}

// InspectChatMessages returns chat messages including thread replies and recalls.
func (s *Store) InspectChatMessages(id string) ([]Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.chats.items[id]; !ok {
		return nil, ErrNotFound
	}
	messages := make([]Message, 0, len(s.chatMessages[id]))
	for id := range s.chatMessages[id] {
		m := s.messages.items[id]
		if m.UpperMessageID != "" {
			continue
		}
		messages = append(messages, s.renderMessage(m.CardAppID(), m))
	}
	slices.SortFunc(messages, func(a, b Message) int {
		if a.Order == b.Order {
			return strings.Compare(a.ID, b.ID)
		}
		if a.Order < b.Order {
			return -1
		}
		return 1
	})
	return messages, nil
}

// ExpiresAt returns the CardKit entity expiry.
func (c Card) ExpiresAt() time.Time { return c.CreatedAt.Add(protocol.CardEntityLifetime) }

// StreamingExpiresAt returns the current stream expiry if streaming is enabled.
func (c Card) StreamingExpiresAt() *time.Time {
	if !c.StreamingMode || c.StreamingSince.IsZero() {
		return nil
	}
	expires := c.StreamingSince.Add(streamingWindow)
	return &expires
}
