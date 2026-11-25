package config

import (
	"errors"
	"fmt"
	"strings"
)

// ContactScope limits visibility to users and department subtrees. An omitted
// scope permits the entire tenant. An empty scope has no additional visibility.
type ContactScope struct {
	DepartmentOpenIDs []string `json:"department_open_ids,omitempty" yaml:"department_open_ids,omitempty"`
	UserOpenIDs       []string `json:"user_open_ids,omitempty" yaml:"user_open_ids,omitempty"`
}

// Department describes a tenant's organizational unit in the sandbox seed.
type Department struct {
	OpenID       string `json:"open_department_id" yaml:"open_department_id"`
	ID           string `json:"department_id" yaml:"department_id"`
	ParentOpenID string `json:"parent_open_department_id,omitempty" yaml:"parent_open_department_id,omitempty"`
	TenantKey    string `json:"tenant_key,omitempty" yaml:"tenant_key,omitempty"`
	Name         string `json:"name" yaml:"name"`
	LeaderOpenID string `json:"leader_open_id,omitempty" yaml:"leader_open_id,omitempty"`
}

// ContactID selects the Feishu identifier represented in a contact response.
func (u User) ContactID(kind string) string {
	switch kind {
	case "user_id":
		return u.UserID
	case "union_id":
		return u.UnionID
	case "email":
		return u.Email
	case "open_id":
		return u.OpenID
	default:
		return ""
	}
}

func (cfg Config) validateDepartmentKeys() error {
	if err := validateKeys("departments", cfg.Departments, func(entry Department) string { return entry.OpenID }); err != nil {
		return err
	}
	seen := make(map[[2]string]bool)
	for _, dept := range cfg.Departments {
		key := [2]string{dept.TenantKey, dept.ID}
		if dept.OpenID == "0" || dept.ID == "0" || seen[key] || strings.TrimSpace(dept.ID) == "" || strings.TrimSpace(dept.Name) == "" {
			return errors.New("departments require non-root IDs, unique department_id values per tenant, and names")
		}
		seen[key] = true
	}
	return nil
}

// ValidateDirectory checks references in a complete configuration or a merged
// runtime seed. Department paths and visibility selectors stay in one tenant.
func (cfg Config) ValidateDirectory() error {
	if err := cfg.validateDepartmentKeys(); err != nil {
		return err
	}
	depts := make(map[string]Department, len(cfg.Departments))
	for _, dept := range cfg.Departments {
		depts[dept.OpenID] = dept
	}
	users := make(map[string]User, len(cfg.Users))
	for _, user := range cfg.Users {
		users[user.OpenID] = user
	}
	validDepartment := func(id, tenant string) bool {
		dept, found := depts[id]
		return id == "0" || (found && dept.TenantKey == tenant)
	}
	validUser := func(id, tenant string) bool {
		user, found := users[id]
		return found && user.TenantKey == tenant
	}
	for _, dept := range cfg.Departments {
		if dept.LeaderOpenID != "" && !validUser(dept.LeaderOpenID, dept.TenantKey) {
			return fmt.Errorf("department %q has an unknown leader", dept.OpenID)
		}
		visited := map[string]bool{dept.OpenID: true}
		for parent := dept.ParentOpenID; parent != "" && parent != "0"; parent = depts[parent].ParentOpenID {
			if visited[parent] || !validDepartment(parent, dept.TenantKey) {
				return fmt.Errorf("department %q requires a valid parent path", dept.OpenID)
			}
			visited[parent] = true
		}
	}
	checkScope := func(scope *ContactScope, tenant string) bool {
		if scope == nil {
			return true
		}
		for _, id := range scope.UserOpenIDs {
			if !validUser(id, tenant) {
				return false
			}
		}
		for _, id := range scope.DepartmentOpenIDs {
			if !validDepartment(id, tenant) {
				return false
			}
		}
		return true
	}
	for _, user := range cfg.Users {
		for _, id := range user.DepartmentOpenIDs {
			if !validDepartment(id, user.TenantKey) {
				return fmt.Errorf("user %q has an unknown department", user.OpenID)
			}
		}
		if !checkScope(user.ContactScope, user.TenantKey) {
			return fmt.Errorf("user %q has an invalid contact_scope", user.OpenID)
		}
	}
	for _, app := range cfg.Apps {
		if !checkScope(app.ContactScope, app.TenantKey) {
			return fmt.Errorf("app %q has an invalid contact_scope", app.AppID)
		}
	}
	return nil
}
