package store

import (
	"testing"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
)

func TestSnapshotPreservesExplicitEventUnsubscription(t *testing.T) {
	cfg := config.Default()
	enabled := cfg.Apps[0]
	disabled := config.App{AppID: "cli_empty_event_types", AppSecret: "local-fixture", BotOpenID: "ou_another_bot", EventTypes: []string{}}
	cfg.Apps = append(cfg.Apps, disabled)
	source := New(nil, 0)
	source.Seed(cfg)
	raw, err := source.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := DecodeSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	dest := New(nil, 0)
	dest.RestoreSnapshot(parsed)
	defaultApp, ok := dest.Apps().Get(enabled.AppID)
	if !ok || defaultApp.EventTypes != nil {
		t.Fatalf("default subscriptions changed: %v", defaultApp.EventTypes)
	}
	unsubscribed, ok := dest.Apps().Get(disabled.AppID)
	if !ok || unsubscribed.EventTypes == nil || len(unsubscribed.EventTypes) != 0 {
		t.Fatalf("explicit unsubscription was lost: %v", unsubscribed.EventTypes)
	}
}
