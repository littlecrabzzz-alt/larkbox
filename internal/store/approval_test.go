package store

import (
	"errors"
	"testing"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
)

func TestApprovalReplayAcrossTenantReseed(t *testing.T) {
	cfg := config.Default()
	appID := cfg.Apps[0].AppID
	cfg.ApprovalDefinitions = []config.ApprovalDefinition{{Code: "schedule", Name: "Schedule change", ApproverOpenID: cfg.Users[1].OpenID}}
	state := New(nil, 0)
	state.Seed(cfg)
	input := ApprovalStart{DefinitionCode: "schedule", OpenID: cfg.Users[0].OpenID, Form: `[]`, UUID: "schedule-1"}
	first, err := state.StartApproval(appID, input)
	if err != nil {
		t.Fatal(err)
	}
	changed := input
	changed.Form = `[{"id":"note","type":"input","value":"Another request"}]`
	if _, err := state.StartApproval(appID, changed); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("changed payload reused its UUID: %v", err)
	}

	cfg.Apps[0].TenantKey = "reseeded"
	cfg.ApprovalDefinitions[0].TenantKey = "reseeded"
	for i := range cfg.Users {
		cfg.Users[i].TenantKey = "reseeded"
		cfg.Users[i].DepartmentOpenIDs = nil
	}
	if err := state.UpsertSeed(cfg); err != nil {
		t.Fatal(err)
	}
	second, err := state.StartApproval(appID, input)
	if err != nil || second.Instance.Code == first.Instance.Code || second.Instance.TenantKey != "reseeded" {
		t.Fatalf("tenant reseed returned the old approval instance: %+v (%v)", second.Instance, err)
	}
	if _, err := state.ReadApproval(appID, first.Instance.Code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old approval remains visible to the new tenant: %v", err)
	}
}
