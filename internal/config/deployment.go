package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// RateLimits configures independent request budgets for each app.
type RateLimits struct {
	Profile      string `json:"profile,omitempty" yaml:"profile,omitempty"`
	QPS          int    `json:"qps,omitempty" yaml:"qps,omitempty"`
	DailyQuota   int    `json:"daily_quota,omitempty" yaml:"daily_quota,omitempty"`
	MonthlyQuota int    `json:"monthly_quota,omitempty" yaml:"monthly_quota,omitempty"`
}

// Effective supplies representative budgets for the realistic profile.
func (r RateLimits) Effective() RateLimits {
	if r.Profile == "realistic" && r.QPS == 0 && r.DailyQuota == 0 && r.MonthlyQuota == 0 {
		r.QPS, r.MonthlyQuota = 50, 10000
	}
	return r
}

func (cfg Config) validateDeployment() error {
	if cfg.SandboxToken != "" && strings.ContainsAny(cfg.SandboxToken, " \t\r\n") {
		return errors.New("sandbox_token must be a single token without whitespace")
	}
	if cfg.Pprof && cfg.SandboxToken == "" {
		return errors.New("pprof requires sandbox_token")
	}
	limits := cfg.RateLimits
	switch limits.Profile {
	case "", "unlimited":
		if limits.QPS != 0 || limits.DailyQuota != 0 || limits.MonthlyQuota != 0 {
			return errors.New("rate limit budgets require the realistic profile")
		}
	case "realistic":
		if limits.QPS < 0 || limits.DailyQuota < 0 || limits.MonthlyQuota < 0 {
			return errors.New("rate limit budgets must be nonnegative")
		}
	default:
		return errors.New("rate_limits.profile must be unlimited or realistic")
	}
	return nil
}

func (cfg *Config) applyDeploymentEnv() error {
	if path, ok := os.LookupEnv("LARKBOX_STATE_FILE"); ok {
		cfg.StateFile = path
	}
	if value, ok := os.LookupEnv("LARKBOX_SANDBOX_TOKEN"); ok {
		cfg.SandboxToken = value
	}
	for _, setting := range []struct {
		name string
		dest *bool
	}{{"LARKBOX_PPROF", &cfg.Pprof}, {"LARKBOX_JSON_LOGS", &cfg.JSONLogs}} {
		if value, ok := os.LookupEnv(setting.name); ok {
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("%s must be a boolean", setting.name)
			}
			*setting.dest = enabled
		}
	}
	if raw, ok := os.LookupEnv("LARKBOX_RATE_LIMITS"); ok {
		var limits *RateLimits
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if !json.Valid([]byte(raw)) || decoder.Decode(&limits) != nil || limits == nil {
			return errors.New("LARKBOX_RATE_LIMITS must be a JSON object with valid fields")
		}
		cfg.RateLimits = *limits
	}
	return nil
}
