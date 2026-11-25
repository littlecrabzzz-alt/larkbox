package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// EventDelivery overrides the default transport timing. Zero values use defaults.
type EventDelivery struct {
	WebhookTimeoutMS      int64   `json:"webhook_timeout_ms,omitempty" yaml:"webhook_timeout_ms,omitempty"`
	VerificationTimeoutMS int64   `json:"verification_timeout_ms,omitempty" yaml:"verification_timeout_ms,omitempty"`
	AckTimeoutMS          int64   `json:"ack_timeout_ms,omitempty" yaml:"ack_timeout_ms,omitempty"`
	WebhookMaxAttempts    int     `json:"webhook_max_attempts,omitempty" yaml:"webhook_max_attempts,omitempty"`
	WebSocketMaxAttempts  int     `json:"websocket_max_attempts,omitempty" yaml:"websocket_max_attempts,omitempty"`
	RetryBackoffMS        []int64 `json:"retry_backoff_ms,omitempty" yaml:"retry_backoff_ms,omitempty"`
}

func (cfg *Config) applyEventEnv() error {
	if value, ok := os.LookupEnv("LARKBOX_EVENT_DELIVERY"); ok {
		raw := strings.TrimSpace(value)
		if !strings.HasPrefix(raw, "{") || !json.Valid([]byte(raw)) {
			return errors.New("LARKBOX_EVENT_DELIVERY must be a JSON object")
		}
		if err := fileDecoder([]byte(raw), true)(&cfg.EventDelivery); err != nil {
			return fmt.Errorf("LARKBOX_EVENT_DELIVERY: %w", err)
		}
	}
	if value, ok := os.LookupEnv("LARKBOX_CLOCK_START"); ok {
		start, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return errors.New("LARKBOX_CLOCK_START must be an RFC3339 timestamp")
		}
		cfg.ClockStart = &start
	}
	return nil
}

func (settings EventDelivery) validate() error {
	const maxMS = (1<<63 - 1) / int64(time.Millisecond)
	for _, timing := range []struct {
		name string
		ms   int64
	}{
		{"webhook_timeout_ms", settings.WebhookTimeoutMS},
		{"verification_timeout_ms", settings.VerificationTimeoutMS},
		{"ack_timeout_ms", settings.AckTimeoutMS},
	} {
		if timing.ms < 0 || timing.ms > maxMS {
			return fmt.Errorf("event_delivery.%s must be a nonnegative duration in milliseconds", timing.name)
		}
	}
	if settings.WebhookMaxAttempts < 0 || settings.WebSocketMaxAttempts < 0 {
		return errors.New("event_delivery attempt counts must be nonnegative")
	}
	for _, delay := range settings.RetryBackoffMS {
		if delay < 0 || delay > maxMS {
			return errors.New("event_delivery.retry_backoff_ms must contain nonnegative durations in milliseconds")
		}
	}
	return nil
}
