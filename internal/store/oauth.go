package store

import "time"

// OAuthUser resolves a selector or returns the first available seeded user.
func (s *Store) OAuthUser(openID, userID string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	switch {
	case openID != "":
		user, ok := s.users.items[openID]
		return user, ok && (userID == "" || user.UserID == userID)
	case userID != "":
		var match User
		for _, user := range s.users.items {
			if user.UserID != userID {
				continue
			}
			if match.OpenID != "" {
				return User{}, false
			}
			match = user
		}
		return match, match.OpenID != ""
	default:
		for _, seed := range s.seed.Users {
			if user, ok := s.users.items[seed.OpenID]; ok {
				return user, true
			}
		}
		return User{}, false
	}
}

// ExchangeOAuthGrant atomically replaces a valid authorization code or refresh
// token with new credentials. The callback validates request-specific bindings
// and builds the replacements without calling back into the store.
func (s *Store) ExchangeOAuthGrant(value, appID string, kind TokenKind, issue func(Token, time.Time) ([2]Token, error)) ([2]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if appID == "" {
		return [2]Token{}, ErrTokenBinding
	}
	if grant, ok := s.tokens[value]; !ok || grant.Kind != kind || (kind != OAuthCode && kind != RefreshToken) {
		return [2]Token{}, ErrNotFound
	}
	now := s.Now()
	grant, err := s.checkTokenBinding(value, appID, "", now)
	if err != nil {
		return [2]Token{}, err
	}
	if grant.UserOpenID == "" {
		return [2]Token{}, ErrTokenOwner
	}
	tokens, err := issue(grant, now)
	if err != nil {
		return [2]Token{}, err
	}
	for _, token := range tokens {
		if _, exists := s.tokens[token.Value]; exists {
			return [2]Token{}, ErrAlreadyExists
		}
	}
	grant.Revoked = true
	s.tokens[value] = grant
	for _, token := range tokens {
		s.tokens[token.Value] = token
	}
	return tokens, nil
}
