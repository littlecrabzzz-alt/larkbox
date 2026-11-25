package store

import (
	"maps"
	"slices"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
)

// Directory holds the contacts visible to one request identity.
type Directory struct {
	Users         []User
	Departments   []config.Department
	DirectChats   map[string]string
	ContactScope  *config.ContactScope
	departmentIDs map[string]string
}

// Departments returns all seeded departments for inspection by the control plane.
func (s *Store) Departments() []config.Department { return s.departments.List() }

// Directory reads visible users, departments, and the actor's known direct chats.
func (s *Store) Directory(appID, actorOpenID string) (Directory, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	app, ok := s.apps.items[appID]
	if !ok {
		return Directory{}, false
	}
	scope := app.ContactScope
	if actorOpenID != "" {
		actor, ok := s.users.items[actorOpenID]
		if !ok || actor.Inactive || actor.TenantKey != app.TenantKey {
			return Directory{}, false
		}
		scope = actor.ContactScope
	}
	result := Directory{ContactScope: cloneContactScope(scope), DirectChats: make(map[string]string), departmentIDs: make(map[string]string)}
	for _, id := range slices.Sorted(maps.Keys(s.departments.items)) {
		dept := s.departments.items[id]
		if dept.TenantKey != app.TenantKey {
			continue
		}
		result.departmentIDs[id] = dept.ID
		if s.departmentVisible(id, scope) {
			result.Departments = append(result.Departments, dept)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.users.items)) {
		user := s.users.items[id]
		if user.TenantKey == app.TenantKey && (id == actorOpenID || s.contactVisible(user, scope)) {
			result.Users = append(result.Users, cloneUser(user))
		}
	}
	if actorOpenID != "" {
		for _, id := range slices.Sorted(maps.Keys(s.chats.items)) {
			chat := s.chats.items[id]
			if chat.ChatMode != "p2p" || len(chat.MemberIDs) != 2 || !slices.Contains(chat.MemberIDs, actorOpenID) {
				continue
			}
			for _, other := range chat.MemberIDs {
				if other != actorOpenID && result.DirectChats[other] == "" {
					result.DirectChats[other] = id
				}
			}
		}
	}
	return result, true
}

func (s *Store) contactVisible(user User, scope *config.ContactScope) bool {
	if scope == nil || slices.Contains(scope.UserOpenIDs, user.OpenID) || slices.Contains(scope.DepartmentOpenIDs, "0") {
		return true
	}
	for _, id := range user.DepartmentOpenIDs {
		if s.departmentVisible(id, scope) {
			return true
		}
	}
	return false
}

func (s *Store) departmentVisible(id string, scope *config.ContactScope) bool {
	if scope == nil || slices.Contains(scope.DepartmentOpenIDs, "0") {
		return true
	}
	for steps := 0; id != "" && id != "0" && steps <= len(s.departments.items); steps++ {
		if slices.Contains(scope.DepartmentOpenIDs, id) {
			return true
		}
		id = s.departments.items[id].ParentOpenID
	}
	return false
}

// UserByID finds a visible user's open, tenant, union, or email identifier.
func (d Directory) UserByID(kind, id string) (User, bool) {
	if id == "" {
		return User{}, false
	}
	for _, user := range d.Users {
		if kind == "email" && (strings.EqualFold(user.Email, id) || strings.EqualFold(user.EnterpriseEmail, id)) || user.ContactID(kind) == id {
			return user, true
		}
	}
	return User{}, false
}

// DepartmentID returns a department identifier in the requested representation.
func (d Directory) DepartmentID(openID, kind string) string {
	if openID == "" || openID == "0" {
		return "0"
	}
	if kind == "department_id" {
		return d.departmentIDs[openID]
	}
	return openID
}
