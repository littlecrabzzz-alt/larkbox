package store

import (
	"errors"
	"strings"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// DeviceKind distinguishes app registration from consent to an existing app.
type DeviceKind uint8

const (
	// DeviceRegistration creates a sandbox app after approval.
	DeviceRegistration DeviceKind = iota + 1
	// DeviceAuthorization grants an existing app access on behalf of a user.
	DeviceAuthorization
)

const (
	deviceInterval    = time.Second
	deviceExpiryGrace = time.Minute
)

// Device flow errors map to independent OAuth wire outcomes.
var (
	ErrDevicePending  = errors.New("authorization_pending")
	ErrDeviceDenied   = errors.New("access_denied")
	ErrDeviceExpired  = errors.New("expired_token")
	ErrDeviceSlowDown = errors.New("slow_down")
)

// DeviceFlow records the pending user decision and polling state.
type DeviceFlow struct {
	DeviceCode      string
	UserCode        string
	Kind            DeviceKind
	AppID           string
	Scope           string
	OpenID          string
	AuthorizationID string
	ExpiresAt       time.Time
	NextPoll        time.Time
	Interval        time.Duration
	Denied          bool
	Consumed        bool
}

// BeginDeviceFlow allocates a pair of codes for one app or registration.
func (s *Store) BeginDeviceFlow(kind DeviceKind, appID, scope string, ttl time.Duration) (DeviceFlow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == DeviceAuthorization {
		if _, ok := s.apps.items[appID]; !ok {
			return DeviceFlow{}, ErrTokenOwner
		}
	}
	now := s.Now()
	for key, flow := range s.deviceFlows {
		if !now.Before(flow.ExpiresAt) && s.deviceUserCodes[flow.UserCode] == key {
			delete(s.deviceUserCodes, flow.UserCode)
		}
		if !now.Before(flow.ExpiresAt.Add(deviceExpiryGrace)) {
			delete(s.deviceFlows, key)
		}
	}
	var deviceCode, userCode string
	for {
		deviceCode = protocol.NewID("dc_")
		fragment := strings.ToUpper(protocol.NewID("")[:8])
		userCode = fragment[:4] + "-" + fragment[4:]
		_, deviceExists := s.deviceFlows[deviceCode]
		_, userExists := s.deviceUserCodes[userCode]
		if !deviceExists && !userExists {
			break
		}
	}
	flow := DeviceFlow{
		DeviceCode: deviceCode, UserCode: userCode, AppID: appID, Scope: scope,
		Kind: kind, ExpiresAt: now.Add(ttl), Interval: deviceInterval,
	}
	s.deviceFlows[deviceCode] = flow
	s.deviceUserCodes[userCode] = deviceCode
	return flow, nil
}

// DeviceByUserCode reads the state for the confirmation page.
func (s *Store) DeviceByUserCode(code string) (DeviceFlow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	flow, ok := s.deviceFlows[s.deviceUserCodes[normalizeUserCode(code)]]
	return flow, ok && s.Now().Before(flow.ExpiresAt)
}

// DecideDeviceFlow completes the browser decision exactly once.
func (s *Store) DecideDeviceFlow(code, openID string, approve bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.deviceUserCodes[normalizeUserCode(code)]
	flow, ok := s.deviceFlows[key]
	now := s.Now()
	if !ok || !now.Before(flow.ExpiresAt) {
		return ErrDeviceExpired
	}
	if flow.Denied || flow.OpenID != "" || flow.Consumed {
		return ErrAlreadyExists
	}
	if approve {
		user, known := s.users.items[openID]
		if !known || user.Inactive {
			return ErrTokenOwner
		}
		if flow.Kind == DeviceAuthorization {
			if _, known := s.apps.items[flow.AppID]; !known {
				return ErrTokenOwner
			}
			flow.AuthorizationID = s.recordAuthorization(flow.AppID, openID, flow.Scope, now)
		}
		flow.OpenID = openID
	} else {
		flow.Denied = true
	}
	s.deviceFlows[key] = flow
	return nil
}

func normalizeUserCode(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

// pollDeviceFlow requires the store lock.
func (s *Store) pollDeviceFlow(code, appID string, kind DeviceKind, now time.Time) (DeviceFlow, error) {
	flow, ok := s.deviceFlows[code]
	switch {
	case !ok || flow.Kind != kind || flow.AppID != appID || flow.Consumed:
		return DeviceFlow{}, ErrNotFound
	case !now.Before(flow.ExpiresAt):
		return DeviceFlow{}, ErrDeviceExpired
	case flow.Denied:
		return DeviceFlow{}, ErrDeviceDenied
	case now.Before(flow.NextPoll):
		flow.Interval = min(flow.Interval+5*time.Second, time.Minute)
		flow.NextPoll = now.Add(flow.Interval)
		s.deviceFlows[code] = flow
		return DeviceFlow{}, ErrDeviceSlowDown
	case flow.OpenID == "":
		flow.NextPoll = now.Add(flow.Interval)
		s.deviceFlows[code] = flow
		return DeviceFlow{}, ErrDevicePending
	}
	user, ok := s.users.items[flow.OpenID]
	if !ok || user.Inactive {
		return DeviceFlow{}, ErrTokenOwner
	}
	return flow, nil
}

// FinishDeviceRegistration claims an approved device code and creates its app.
func (s *Store) FinishDeviceRegistration(code string) (App, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	flow, err := s.pollDeviceFlow(code, "", DeviceRegistration, s.Now())
	if err != nil {
		return App{}, "", err
	}
	var id string
	for {
		id = protocol.NewID("cli_")[:20]
		if _, exists := s.apps.items[id]; !exists {
			break
		}
	}
	app := App{
		AppID: id, AppSecret: protocol.NewID(""), BotOpenID: protocol.NewID(protocol.OpenID),
		BotName: "Larkbox CLI Bot", TenantKey: s.users.items[flow.OpenID].TenantKey,
	}
	s.apps.put(app)
	flow.Consumed = true
	s.deviceFlows[code] = flow
	return app, flow.OpenID, nil
}

// FinishDeviceAuthorization issues credentials for an approved, current consent.
func (s *Store) FinishDeviceAuthorization(code, appID string, issue func(Token, time.Time) [2]Token) ([2]Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	flow, err := s.pollDeviceFlow(code, appID, DeviceAuthorization, now)
	if err != nil {
		return [2]Token{}, err
	}
	if entry := s.authorizations[authorizationKey{appID, flow.OpenID}]; entry.ID == "" || entry.ID != flow.AuthorizationID {
		return [2]Token{}, ErrTokenRevoked
	}
	grant := Token{AppID: appID, UserOpenID: flow.OpenID, Scope: flow.Scope, AuthorizationID: flow.AuthorizationID}
	tokens := issue(grant, now)
	if err := s.saveOAuthTokens(&tokens, flow.AuthorizationID); err != nil {
		return [2]Token{}, err
	}
	flow.Consumed = true
	s.deviceFlows[code] = flow
	return tokens, nil
}
