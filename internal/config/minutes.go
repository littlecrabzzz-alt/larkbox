package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Minute is a recorded meeting fixture with a transcript and owner.
type Minute struct {
	Token       string `json:"minute_token" yaml:"minute_token"`
	Title       string `json:"title" yaml:"title"`
	OwnerOpenID string `json:"owner_open_id" yaml:"owner_open_id"`
	TenantKey   string `json:"tenant_key,omitempty" yaml:"tenant_key,omitempty"`
	DurationMS  int64  `json:"duration_ms" yaml:"duration_ms"`
	Transcript  string `json:"transcript,omitempty" yaml:"transcript,omitempty"`
	Summary     string `json:"summary,omitempty" yaml:"summary,omitempty"`
}

var minuteToken = regexp.MustCompile(`^[a-z0-9]{1,128}$`)

func (cfg Config) validateMinuteKeys() error {
	if err := validateKeys("minutes", cfg.Minutes, func(entry Minute) string { return entry.Token }); err != nil {
		return err
	}
	for _, entry := range cfg.Minutes {
		if !minuteToken.MatchString(entry.Token) || strings.TrimSpace(entry.Title) == "" || entry.OwnerOpenID == "" || entry.DurationMS < 0 {
			return fmt.Errorf("minute %q requires a lowercase token, title, owner, and nonnegative duration", entry.Token)
		}
	}
	return nil
}

// ValidateMinutes checks ownership references in the combined fixture state.
func (cfg Config) ValidateMinutes() error {
	users := make(map[string]User, len(cfg.Users))
	for _, user := range cfg.Users {
		users[user.OpenID] = user
	}
	for _, entry := range cfg.Minutes {
		owner, ok := users[entry.OwnerOpenID]
		if !ok || owner.Inactive || owner.TenantKey != entry.TenantKey {
			return fmt.Errorf("minute %q requires an active owner in its tenant", entry.Token)
		}
	}
	return nil
}
