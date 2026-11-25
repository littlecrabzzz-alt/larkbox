package config

import (
	"fmt"
	"strings"
)

// ApprovalDefinition seeds a native approval with a single manual decision.
type ApprovalDefinition struct {
	Code           string `json:"approval_code" yaml:"approval_code"`
	Name           string `json:"name" yaml:"name"`
	TenantKey      string `json:"tenant_key,omitempty" yaml:"tenant_key,omitempty"`
	ApproverOpenID string `json:"approver_open_id" yaml:"approver_open_id"`
}

func (cfg Config) validateApprovalKeys() error {
	if err := validateKeys("approval_definitions", cfg.ApprovalDefinitions, func(entry ApprovalDefinition) string { return entry.Code }); err != nil {
		return err
	}
	for _, entry := range cfg.ApprovalDefinitions {
		if strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.ApproverOpenID) == "" {
			return fmt.Errorf("approval definition %q requires a name and approver_open_id", entry.Code)
		}
	}
	return nil
}

// ValidateApprovals checks that each definition has an active tenant approver.
func (cfg Config) ValidateApprovals() error {
	users := make(map[string]User, len(cfg.Users))
	for _, user := range cfg.Users {
		users[user.OpenID] = user
	}
	for _, entry := range cfg.ApprovalDefinitions {
		user, ok := users[entry.ApproverOpenID]
		if !ok || user.Inactive || user.TenantKey != entry.TenantKey {
			return fmt.Errorf("approval definition %q requires an active approver in its tenant", entry.Code)
		}
	}
	return nil
}
