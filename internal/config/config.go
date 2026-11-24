// Package config loads server settings and virtual sandbox seed data.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// App describes a virtual Feishu application and its bot.
type App struct {
	AppID             string `json:"app_id" yaml:"app_id"`
	AppSecret         string `json:"app_secret" yaml:"app_secret"`
	BotOpenID         string `json:"bot_open_id" yaml:"bot_open_id"`
	BotName           string `json:"bot_name" yaml:"bot_name"`
	VerificationToken string `json:"verification_token" yaml:"verification_token"`
	EncryptKey        string `json:"encrypt_key" yaml:"encrypt_key"`
	WebhookURL        string `json:"webhook_url" yaml:"webhook_url"`
}

// User describes a virtual Feishu user.
type User struct {
	OpenID  string `json:"open_id" yaml:"open_id"`
	UserID  string `json:"user_id" yaml:"user_id"`
	UnionID string `json:"union_id" yaml:"union_id"`
	Name    string `json:"name" yaml:"name"`
	Email   string `json:"email" yaml:"email"`
	Avatar  string `json:"avatar,omitempty" yaml:"avatar,omitempty"`
}

// Chat describes a virtual p2p or group chat.
type Chat struct {
	ChatID    string   `json:"chat_id" yaml:"chat_id"`
	ChatMode  string   `json:"chat_mode" yaml:"chat_mode"`
	Name      string   `json:"name" yaml:"name"`
	OwnerID   string   `json:"owner_id" yaml:"owner_id"`
	MemberIDs []string `json:"member_ids" yaml:"member_ids"`
}

// TokenTTLs configures the lifetime, in seconds, of each credential kind.
type TokenTTLs struct {
	Tenant  int64 `json:"tenant_access_token" yaml:"tenant_access_token"`
	App     int64 `json:"app_access_token" yaml:"app_access_token"`
	User    int64 `json:"user_access_token" yaml:"user_access_token"`
	Refresh int64 `json:"refresh_token" yaml:"refresh_token"`
	Code    int64 `json:"oauth_code" yaml:"oauth_code"`
}

// Config describes the HTTP server, token defaults, and initial state.
type Config struct {
	Addr       string    `json:"addr" yaml:"addr"`
	StrictMode bool      `json:"strict_mode" yaml:"strict_mode"`
	TokenTTLs  TokenTTLs `json:"token_ttls" yaml:"token_ttls"`
	Apps       []App     `json:"apps" yaml:"apps"`
	Users      []User    `json:"users" yaml:"users"`
	Chats      []Chat    `json:"chats" yaml:"chats"`
}

// DefaultApp returns the public fixture credentials for the built-in bot.
func DefaultApp() App {
	return App{
		AppID: "cli_larkbox", AppSecret: "larkbox-local-secret",
		BotOpenID: "ou_larkbox_bot", BotName: "Larkbox Bot",
		VerificationToken: "larkbox-local-verification", EncryptKey: "larkbox-local-encryption",
	}
}

// Default returns independent settings and demo seed data for a local server.
func Default() Config {
	bot := DefaultApp()
	alice := User{OpenID: "ou_larkbox_alice", UserID: "alice", UnionID: "on_larkbox_alice", Name: "Alice", Email: "alice@example.test"}
	bob := User{OpenID: "ou_larkbox_bob", UserID: "bob", UnionID: "on_larkbox_bob", Name: "Bob", Email: "bob@example.test"}
	return Config{
		Addr: "127.0.0.1:8080", StrictMode: true,
		TokenTTLs: TokenTTLs{Tenant: 7200, App: 7200, User: 7200, Refresh: 30 * 24 * 3600, Code: 300},
		Apps:      []App{bot}, Users: []User{alice, bob},
		Chats: []Chat{
			{ChatID: "oc_larkbox_group", ChatMode: "group", Name: "Larkbox Demo", OwnerID: alice.OpenID, MemberIDs: []string{alice.OpenID, bob.OpenID, bot.BotOpenID}},
			{ChatID: "oc_larkbox_p2p", ChatMode: "p2p", Name: "Alice and Larkbox Bot", OwnerID: alice.OpenID, MemberIDs: []string{alice.OpenID, bot.BotOpenID}},
		},
	}
}

// Load overlays file settings and LARKBOX_* environment variables on Default.
// The caller may apply flags and call Validate after all overrides are resolved.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		if err := readFile(path, &cfg); err != nil {
			return Config{}, fmt.Errorf("config file %q: %w", path, err)
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func readFile(path string, cfg *Config) (err error) {
	// #nosec G304 -- The operator selects this local file with --config.
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()

	var decode func(any) error
	if strings.EqualFold(filepath.Ext(path), ".json") {
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		decode = decoder.Decode
	} else {
		decoder := yaml.NewDecoder(file)
		decoder.KnownFields(true)
		decode = decoder.Decode
	}
	// Decode into fresh slices so JSON cannot reuse built-in seed entries.
	defaults := *cfg
	cfg.Apps, cfg.Users, cfg.Chats = nil, nil, nil
	if err := decode(&cfg); err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("expected a configuration object")
	}
	if cfg.Apps == nil {
		cfg.Apps = defaults.Apps
	}
	if cfg.Users == nil {
		cfg.Users = defaults.Users
	}
	if cfg.Chats == nil {
		cfg.Chats = defaults.Chats
	}
	var extra any
	if err := decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected a single configuration document")
	}
	return nil
}

func applyEnv(cfg *Config) error {
	if value, ok := os.LookupEnv("LARKBOX_ADDR"); ok {
		cfg.Addr = value
	}
	if value, ok := os.LookupEnv("LARKBOX_STRICT_MODE"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("LARKBOX_STRICT_MODE must be a boolean")
		}
		cfg.StrictMode = parsed
	}

	for _, item := range ttlFields(&cfg.TokenTTLs) {
		if value, ok := os.LookupEnv(item.env); ok {
			seconds, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("%s must be an integer number of seconds", item.env)
			}
			*item.seconds = seconds
		}
	}
	if err := applySeedEnv("LARKBOX_APPS", &cfg.Apps); err != nil {
		return err
	}
	if err := applySeedEnv("LARKBOX_USERS", &cfg.Users); err != nil {
		return err
	}
	return applySeedEnv("LARKBOX_CHATS", &cfg.Chats)
}

func applySeedEnv[T any](name string, dest *[]T) error {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}

	var items []T
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if !json.Valid([]byte(value)) || decoder.Decode(&items) != nil || items == nil {
		return fmt.Errorf("%s must be a JSON seed list with valid fields", name)
	}
	*dest = items
	return nil
}

type ttlField struct {
	env     string
	seconds *int64
}

func ttlFields(ttl *TokenTTLs) []ttlField {
	return []ttlField{
		{"LARKBOX_TENANT_TOKEN_TTL", &ttl.Tenant},
		{"LARKBOX_APP_TOKEN_TTL", &ttl.App},
		{"LARKBOX_USER_TOKEN_TTL", &ttl.User},
		{"LARKBOX_REFRESH_TOKEN_TTL", &ttl.Refresh},
		{"LARKBOX_OAUTH_CODE_TTL", &ttl.Code},
	}
}

// Validate checks listener settings, TTLs, and seed keys.
func (cfg Config) Validate() error {
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return fmt.Errorf("addr must be host:port: %w", err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return errors.New("addr must have a numeric port between 0 and 65535")
	}
	const maxSeconds = (1<<63 - 1) / int64(time.Second)
	for _, item := range ttlFields(&cfg.TokenTTLs) {
		if *item.seconds <= 0 || *item.seconds > maxSeconds {
			return fmt.Errorf("%s / token_ttls must be a positive duration in seconds", item.env)
		}
	}
	return cfg.validateSeeds()
}

func (cfg Config) validateSeeds() error {
	if err := validateKeys("apps", cfg.Apps, func(app App) string { return app.AppID }); err != nil {
		return err
	}
	if err := validateKeys("users", cfg.Users, func(user User) string { return user.OpenID }); err != nil {
		return err
	}
	if err := validateKeys("chats", cfg.Chats, func(chat Chat) string { return chat.ChatID }); err != nil {
		return err
	}
	for _, chat := range cfg.Chats {
		if chat.ChatMode != "p2p" && chat.ChatMode != "group" {
			return fmt.Errorf("chat %q must have chat_mode p2p or group", chat.ChatID)
		}
	}
	return nil
}

func validateKeys[T any](name string, list []T, key func(T) string) error {
	seen := make(map[string]bool, len(list))
	for _, item := range list {
		id := key(item)
		if strings.TrimSpace(id) == "" || seen[id] {
			return fmt.Errorf("%s must have unique, non-empty IDs", name)
		}
		seen[id] = true
	}
	return nil
}
