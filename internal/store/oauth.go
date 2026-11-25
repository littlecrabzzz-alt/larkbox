package store

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

type authorizationKey struct {
	appID  string
	openID string
}

// Authorization is the consent granted by one user to one application.
type Authorization struct {
	ID     string
	AppID  string
	OpenID string
	Scope  string
	At     time.Time
}

// SaveAuthorizationCode records consent and the code in the same commit.
func (s *Store) SaveAuthorizationCode(code Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code.Kind != OAuthCode || code.UserOpenID == "" || !s.tokenOwnersExist(code) || s.users.items[code.UserOpenID].Inactive {
		return ErrTokenOwner
	}
	code.AuthorizationID = s.recordAuthorization(code.AppID, code.UserOpenID, code.Scope, s.Now())
	s.tokens[code.Value] = code
	return nil
}

// recordAuthorization requires the store lock and valid app and user owners.
func (s *Store) recordAuthorization(appID, openID, scope string, now time.Time) string {
	key := authorizationKey{appID, openID}
	entry, exists := s.authorizations[key]
	if !exists {
		entry = Authorization{ID: protocol.NewID(""), AppID: appID, OpenID: openID}
	}
	var scopes []string
	for permission := range strings.SplitSeq(entry.Scope+" "+scope, " ") {
		if permission != "" && !slices.Contains(scopes, permission) {
			scopes = append(scopes, permission)
		}
	}
	slices.Sort(scopes)
	entry.Scope, entry.At = strings.Join(scopes, " "), now
	s.authorizations[key] = entry
	return entry.ID
}

// AuthorizedUsers lists current consent records scoped to the app.
func (s *Store) AuthorizedUsers(appID string) ([]Authorization, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.apps.items[appID]; !ok {
		return nil, ErrNotFound
	}
	entries := make([]Authorization, 0)
	for key, authorization := range s.authorizations {
		if key.appID == appID {
			entries = append(entries, authorization)
		}
	}
	slices.SortFunc(entries, func(a, b Authorization) int { return cmp.Compare(a.OpenID, b.OpenID) })
	return entries, nil
}

// RevokeAuthorization invalidates the credentials issued for the token's
// current consent record. Unknown, foreign, and previously revoked tokens
// yield the same outward result without affecting another authorization.
func (s *Store) RevokeAuthorization(value, appID string) (Authorization, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, err := s.checkTokenBinding(value, appID, "", s.Now())
	if err != nil || token.UserOpenID == "" || (token.Kind != UserToken && token.Kind != RefreshToken) {
		return Authorization{}, false
	}
	token.Revoked = true
	s.tokens[value] = token
	key := authorizationKey{token.AppID, token.UserOpenID}
	entry, exists := s.authorizations[key]
	if !exists || entry.ID != token.AuthorizationID {
		return Authorization{}, false
	}
	for value, credential := range s.tokens {
		if credential.AuthorizationID == entry.ID {
			credential.Revoked = true
			s.tokens[value] = credential
		}
	}
	delete(s.authorizations, key)
	return entry, true
}

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
		for _, seed := range s.userSeedOrder {
			if user, ok := s.users.items[seed.OpenID]; ok {
				return user, true
			}
		}
		return User{}, false
	}
}

// ExchangeOAuthGrant atomically replaces a valid authorization code or refresh
// token with new credentials. The callback validates request-specific bindings
// and builds the replacements without calling back into the store. The second
// replacement may be empty if the grant does not allow a refresh token.
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
	if err := s.saveOAuthTokens(&tokens, grant.AuthorizationID); err != nil {
		return [2]Token{}, err
	}
	grant.Consumed = true
	s.tokens[value] = grant
	return tokens, nil
}

// saveOAuthTokens requires the store lock.
func (s *Store) saveOAuthTokens(tokens *[2]Token, authorizationID string) error {
	replacements := tokens[:]
	if tokens[1].Value == "" {
		replacements = tokens[:1]
	}
	for _, token := range replacements {
		if _, exists := s.tokens[token.Value]; exists {
			return ErrAlreadyExists
		}
	}
	for i := range replacements {
		replacements[i].AuthorizationID = authorizationID
		s.tokens[replacements[i].Value] = replacements[i]
	}
	return nil
}
