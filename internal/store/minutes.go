package store

import (
	"maps"
	"slices"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
)

// Minutes returns the configured recording fixtures in token order.
func (s *Store) Minutes() []config.Minute {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]config.Minute, 0, len(s.work.minutes))
	for _, token := range slices.Sorted(maps.Keys(s.work.minutes)) {
		result = append(result, s.work.minutes[token])
	}
	return result
}

// Minute returns one recording visible to its active owner.
func (s *Store) Minute(owner ResourceOwner, token string) (config.Minute, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, hasMinute := s.work.minutes[token]
	app, hasApp := s.apps.items[owner.AppID]
	user, hasUser := s.users.items[owner.UserOpenID]
	if !hasMinute || !hasApp || !hasUser || user.Inactive || owner.UserOpenID != entry.OwnerOpenID ||
		entry.TenantKey != owner.TenantKey || entry.TenantKey != app.TenantKey || entry.TenantKey != user.TenantKey {
		return config.Minute{}, false
	}
	return entry, true
}
