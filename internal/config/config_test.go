package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func cleanEnvironment(t *testing.T) {
	t.Helper()
	var ttl TokenTTLs
	names := []string{"LARKBOX_ADDR", "LARKBOX_STRICT_MODE", "LARKBOX_APPS", "LARKBOX_USERS", "LARKBOX_CHATS", "LARKBOX_CARD_TEMPLATES", "LARKBOX_EVENT_DELIVERY", "LARKBOX_CLOCK_START"}
	for _, field := range ttlFields(&ttl) {
		names = append(names, field.env)
	}
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDefault(t *testing.T) {
	cleanEnvironment(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:8080" || !cfg.StrictMode || cfg.TokenTTLs.Tenant != 7200 {
		t.Fatalf("unexpected defaults: addr=%q strict=%t ttls=%+v", cfg.Addr, cfg.StrictMode, cfg.TokenTTLs)
	}
	bot := DefaultApp()
	if len(cfg.Apps) != 1 || !reflect.DeepEqual(cfg.Apps[0], bot) || len(cfg.Users) == 0 {
		t.Fatal("expected a built-in app, bot, and user")
	}
	for _, mode := range []string{"p2p", "group"} {
		if !slices.ContainsFunc(cfg.Chats, func(chat Chat) bool {
			return chat.ChatMode == mode && slices.Contains(chat.MemberIDs, bot.BotOpenID)
		}) {
			t.Errorf("missing %s chat for the built-in bot", mode)
		}
	}
	cfg.Chats[0].MemberIDs[0] = "changed"
	if Default().Chats[0].MemberIDs[0] == "changed" {
		t.Fatal("default seed slices share memory")
	}
}

func TestLoad(t *testing.T) {
	cleanEnvironment(t)
	for _, tc := range []struct {
		name, body, wantErr string
	}{
		{"config.yaml", "addr: 127.0.0.1:9000\nstrict_mode: false\ntoken_ttls:\n  tenant_access_token: 30\napps:\n  - app_id: cli_other\nusers: []\n", ""},
		{"config.json", `{"addr":"127.0.0.1:9000","strict_mode":false,"token_ttls":{"tenant_access_token":30},"apps":[{"app_id":"cli_other"}],"users":[]}`, ""},
		{"unknown.json", `{"extra":true}`, "unknown field"},
		{"unknown.yaml", "extra: true", "field extra not found"},
		{"trailing.yaml", "{}\n---\n{}", "single configuration"},
		{"trailing.json", "{} {}", "single configuration"},
		{"null.json", "null", "configuration object"},
		{"null.yaml", "null", "configuration object"},
		{"null-apps.json", `{"apps":null}`, "apps must be a seed list"},
		{"null-users.json", `{"users":null}`, "users must be a seed list"},
		{"null-chats.json", `{"chats":null}`, "chats must be a seed list"},
		{"null-apps.yaml", "apps: ~", "apps must be a seed list"},
		{"null-users.yaml", "users: null", "users must be a seed list"},
		{"null-chats.yaml", "chats:", "chats must be a seed list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Addr != "127.0.0.1:9000" || cfg.StrictMode || cfg.TokenTTLs.Tenant != 30 || cfg.TokenTTLs.App != Default().TokenTTLs.App {
				t.Fatalf("unexpected file overlay: addr=%q strict=%t ttls=%+v", cfg.Addr, cfg.StrictMode, cfg.TokenTTLs)
			}
			if len(cfg.Apps) != 1 || !reflect.DeepEqual(cfg.Apps[0], App{AppID: "cli_other"}) || len(cfg.Users) != 0 || len(cfg.Chats) == 0 {
				t.Fatal("expected supplied seed lists to replace defaults and omitted lists to inherit them")
			}
			t.Setenv("LARKBOX_ADDR", "[::1]:0")
			t.Setenv("LARKBOX_STRICT_MODE", "true")
			t.Setenv("LARKBOX_TENANT_TOKEN_TTL", "15")
			t.Setenv("LARKBOX_APPS", `[{"app_id":"cli_env","bot_name":"Environment bot"}]`)
			cfg, err = Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if cfg.Addr != "[::1]:0" || !cfg.StrictMode || cfg.TokenTTLs.Tenant != 15 || len(cfg.Apps) != 1 || !reflect.DeepEqual(cfg.Apps[0], App{AppID: "cli_env", BotName: "Environment bot"}) {
				t.Fatal("environment did not override file settings")
			}
		})
	}
}

func TestLoadInvalidEnvironment(t *testing.T) {
	cleanEnvironment(t)
	for _, tc := range []struct{ name, value string }{
		{"LARKBOX_STRICT_MODE", "maybe"},
		{"LARKBOX_OAUTH_CODE_TTL", "five"},
		{"LARKBOX_APPS", "null"},
		{"LARKBOX_USERS", "null"},
		{"LARKBOX_CHATS", "null"},
		{"LARKBOX_USERS", `[{"extra":true}]`},
		{"LARKBOX_CHATS", "[] []"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			_, err := Load("")
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("Load error = %v, want %s error", err, tc.name)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"missing port", func(cfg *Config) { cfg.Addr = "localhost" }},
		{"invalid port", func(cfg *Config) { cfg.Addr = "localhost:65536" }},
		{"zero TTL", func(cfg *Config) { cfg.TokenTTLs.Code = 0 }},
		{"overflowing TTL", func(cfg *Config) { cfg.TokenTTLs.Refresh = 1 << 62 }},
		{"duplicate app ID", func(cfg *Config) { cfg.Apps = append(cfg.Apps, cfg.Apps[0]) }},
		{"empty user ID", func(cfg *Config) { cfg.Users[0].OpenID = "" }},
		{"invalid chat mode", func(cfg *Config) { cfg.Chats[0].ChatMode = "channel" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestLoadRedirectURIs(t *testing.T) {
	cleanEnvironment(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://127.0.0.1:8080/callback", "http://localhost:8080/callback"}
	if !slices.Equal(cfg.Apps[0].RedirectURIs, want) {
		t.Fatalf("demo callbacks = %v, want %v", cfg.Apps[0].RedirectURIs, want)
	}

	path := filepath.Join(t.TempDir(), "callbacks.yaml")
	yaml := "apps:\n  - app_id: cli_test\n    redirect_uris:\n      - 'http://[::1]:49152/callback?encoded=%23'\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil || !slices.Equal(cfg.Apps[0].RedirectURIs, []string{"http://[::1]:49152/callback?encoded=%23"}) {
		t.Fatalf("expected a registered IPv6 callback from YAML, got %v", err)
	}

	for _, callback := range []string{
		"https://example.test/callback#", "https://example.test/callback#fragment",
		"https://example.test:0/callback", "https://example.test:65536/callback",
		"https://example.test:bad/callback", "https://example.test:/callback",
		"https://[example.test]/callback", "https://[::1]extra/callback", "https://::1/callback",
		"https://user@example.test/callback", "https://example..test/callback", "https://example.test/call back",
	} {
		t.Run(callback, func(t *testing.T) {
			t.Setenv("LARKBOX_APPS", `[{"app_id":"cli_test","redirect_uris":["`+callback+`"]}]`)
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "redirect_uris[0]") {
				t.Fatalf("expected invalid callback configuration, got %v", err)
			}
		})
	}
}

func TestCardTemplateSeeds(t *testing.T) {
	cleanEnvironment(t)
	path := filepath.Join(t.TempDir(), "templates.yaml")
	body := `card_templates:
  - template_id: local
    template_version_name: 1.0.0
    data: '{"schema":"2.0","body":{"elements":[]}}'
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.CardTemplates) != 1 || cfg.CardTemplates[0].TemplateID != "local" || cfg.CardTemplates[0].VersionName != "1.0.0" {
		t.Fatalf("unexpected configured templates: %+v", cfg.CardTemplates)
	}
	cfg.CardTemplates = append(cfg.CardTemplates, cfg.CardTemplates[0])
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate card template") {
		t.Fatalf("duplicate template version was accepted: %v", err)
	}
	t.Setenv("LARKBOX_CARD_TEMPLATES", `[]`)
	cfg, err = Load(path)
	if err != nil || len(cfg.CardTemplates) != 0 {
		t.Fatalf("environment did not replace template list: %+v, %v", cfg.CardTemplates, err)
	}
}

func TestEventTimingConfiguration(t *testing.T) {
	cleanEnvironment(t)
	path := filepath.Join(t.TempDir(), "timing.yaml")
	body := "clock_start: 2026-01-01T00:00:00Z\nevent_delivery:\n  webhook_timeout_ms: 150\n  retry_backoff_ms: [4, 0]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LARKBOX_CLOCK_START", "2026-02-01T00:00:00Z")
	t.Setenv("LARKBOX_EVENT_DELIVERY", `{"websocket_max_attempts":2,"verification_timeout_ms":1500}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.ClockStart == nil || cfg.ClockStart.Month() != 2 ||
		cfg.EventDelivery.WebhookTimeoutMS != 150 || cfg.EventDelivery.WebSocketMaxAttempts != 2 ||
		cfg.EventDelivery.VerificationTimeoutMS != 1500 || !slices.Equal(cfg.EventDelivery.RetryBackoffMS, []int64{4, 0}) {
		t.Fatalf("timing override lost file fields or clock: %+v", cfg.EventDelivery)
	}
	cfg.EventDelivery.RetryBackoffMS = []int64{-1}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "retry_backoff_ms") {
		t.Fatalf("negative retry delay accepted: %v", err)
	}
	t.Setenv("LARKBOX_EVENT_DELIVERY", `{"typo":1}`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown environment setting accepted: %v", err)
	}
}
