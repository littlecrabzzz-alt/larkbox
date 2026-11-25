// Package config loads server settings and virtual sandbox seed data.
package config

import (
	"bytes"
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

	"github.com/littlecrabzzz-alt/larkbox/internal/simclock"
)

// App describes a virtual Feishu application and its bot.
type App struct {
	TenantKey         string        `json:"tenant_key,omitempty" yaml:"tenant_key,omitempty"`
	AppID             string        `json:"app_id" yaml:"app_id"`
	AppSecret         string        `json:"app_secret" yaml:"app_secret"`
	BotOpenID         string        `json:"bot_open_id" yaml:"bot_open_id"`
	BotAvatarURL      string        `json:"bot_avatar_url,omitempty" yaml:"bot_avatar_url,omitempty"`
	BotName           string        `json:"bot_name" yaml:"bot_name"`
	VerificationToken string        `json:"verification_token" yaml:"verification_token"`
	EncryptKey        string        `json:"encrypt_key" yaml:"encrypt_key"`
	WebhookURL        string        `json:"webhook_url" yaml:"webhook_url"`
	EventTypes        []string      `json:"event_types,omitempty" yaml:"event_types,omitempty"`
	BotMenu           BotMenu       `json:"bot_menu,omitempty" yaml:"bot_menu,omitempty"`
	RedirectURIs      []string      `json:"redirect_uris,omitempty" yaml:"redirect_uris,omitempty"`
	ContactScope      *ContactScope `json:"contact_scope,omitempty" yaml:"contact_scope,omitempty"`
}

// User describes a virtual Feishu user.
type User struct {
	OpenID            string        `json:"open_id" yaml:"open_id"`
	Inactive          bool          `json:"inactive,omitempty" yaml:"inactive,omitempty"`
	UserID            string        `json:"user_id" yaml:"user_id"`
	UnionID           string        `json:"union_id" yaml:"union_id"`
	Name              string        `json:"name" yaml:"name"`
	Email             string        `json:"email" yaml:"email"`
	Avatar            string        `json:"avatar,omitempty" yaml:"avatar,omitempty"`
	EnName            string        `json:"en_name,omitempty" yaml:"en_name,omitempty"`
	AvatarThumb       string        `json:"avatar_thumb,omitempty" yaml:"avatar_thumb,omitempty"`
	AvatarMiddle      string        `json:"avatar_middle,omitempty" yaml:"avatar_middle,omitempty"`
	AvatarBig         string        `json:"avatar_big,omitempty" yaml:"avatar_big,omitempty"`
	EnterpriseEmail   string        `json:"enterprise_email,omitempty" yaml:"enterprise_email,omitempty"`
	Mobile            string        `json:"mobile,omitempty" yaml:"mobile,omitempty"`
	TenantKey         string        `json:"tenant_key,omitempty" yaml:"tenant_key,omitempty"`
	EmployeeNo        string        `json:"employee_no,omitempty" yaml:"employee_no,omitempty"`
	DepartmentOpenIDs []string      `json:"department_open_ids,omitempty" yaml:"department_open_ids,omitempty"`
	ContactScope      *ContactScope `json:"contact_scope,omitempty" yaml:"contact_scope,omitempty"`
}

// Chat describes a virtual p2p or group chat.
type Chat struct {
	ChatID          string   `json:"chat_id" yaml:"chat_id"`
	ChatMode        string   `json:"chat_mode" yaml:"chat_mode"`
	Name            string   `json:"name" yaml:"name"`
	OwnerID         string   `json:"owner_id" yaml:"owner_id"`
	MemberIDs       []string `json:"member_ids" yaml:"member_ids"`
	ChatSettings    `yaml:",inline"`
	AppID           string            `json:"-" yaml:"-"`
	CreatorID       string            `json:"-" yaml:"-"`
	BotManagers     []string          `json:"-" yaml:"-"`
	CreatedAt       time.Time         `json:"-" yaml:"-"`
	MemberOrder     map[string]uint64 `json:"-" yaml:"-"`
	NextMemberOrder uint64            `json:"-" yaml:"-"`
}

// ChatSettings are settings shared by seeded and API-created chats.
type ChatSettings struct {
	Avatar                 string                 `json:"avatar,omitempty" yaml:"avatar,omitempty"`
	Description            string                 `json:"description,omitempty" yaml:"description,omitempty"`
	I18nNames              map[string]string      `json:"i18n_names,omitempty" yaml:"i18n_names,omitempty"`
	ChatType               string                 `json:"chat_type,omitempty" yaml:"chat_type,omitempty"`
	External               bool                   `json:"external,omitempty" yaml:"external,omitempty"`
	GroupMessageType       string                 `json:"group_message_type,omitempty" yaml:"group_message_type,omitempty"`
	JoinMessageVisibility  string                 `json:"join_message_visibility,omitempty" yaml:"join_message_visibility,omitempty"`
	LeaveMessageVisibility string                 `json:"leave_message_visibility,omitempty" yaml:"leave_message_visibility,omitempty"`
	MembershipApproval     string                 `json:"membership_approval,omitempty" yaml:"membership_approval,omitempty"`
	UrgentSetting          string                 `json:"urgent_setting,omitempty" yaml:"urgent_setting,omitempty"`
	VideoConferenceSetting string                 `json:"video_conference_setting,omitempty" yaml:"video_conference_setting,omitempty"`
	EditPermission         string                 `json:"edit_permission,omitempty" yaml:"edit_permission,omitempty"`
	PinManageSetting       string                 `json:"pin_manage_setting,omitempty" yaml:"pin_manage_setting,omitempty"`
	HideMemberCountSetting string                 `json:"hide_member_count_setting,omitempty" yaml:"hide_member_count_setting,omitempty"`
	Labels                 []string               `json:"labels,omitempty" yaml:"labels,omitempty"`
	ToolkitIDs             []string               `json:"toolkit_ids,omitempty" yaml:"toolkit_ids,omitempty"`
	ChatTags               []string               `json:"chat_tags,omitempty" yaml:"chat_tags,omitempty"`
	RestrictedModeSetting  *RestrictedModeSetting `json:"restricted_mode_setting,omitempty" yaml:"restricted_mode_setting,omitempty"`
}

// RestrictedModeSetting controls sharing, screenshots, and downloads in a chat.
type RestrictedModeSetting struct {
	Status                         bool   `json:"status" yaml:"status"`
	ScreenshotHasPermissionSetting string `json:"screenshot_has_permission_setting,omitempty" yaml:"screenshot_has_permission_setting,omitempty"`
	DownloadHasPermissionSetting   string `json:"download_has_permission_setting,omitempty" yaml:"download_has_permission_setting,omitempty"`
	MessageHasPermissionSetting    string `json:"message_has_permission_setting,omitempty" yaml:"message_has_permission_setting,omitempty"`
}

// TokenTTLs configures the lifetime, in seconds, of each credential kind.
type TokenTTLs struct {
	Tenant  int64 `json:"tenant_access_token" yaml:"tenant_access_token"`
	App     int64 `json:"app_access_token" yaml:"app_access_token"`
	User    int64 `json:"user_access_token" yaml:"user_access_token"`
	Refresh int64 `json:"refresh_token" yaml:"refresh_token"`
	Code    int64 `json:"oauth_code" yaml:"oauth_code"`
}

// CardTemplate associates a template version with local Card JSON 2.0.
type CardTemplate struct {
	TemplateID  string `json:"template_id" yaml:"template_id"`
	VersionName string `json:"template_version_name,omitempty" yaml:"template_version_name,omitempty"`
	Data        string `json:"data" yaml:"data"`
}

// Config describes the HTTP server, token defaults, and initial state.
type Config struct {
	Addr                string               `json:"addr" yaml:"addr"`
	StrictMode          bool                 `json:"strict_mode" yaml:"strict_mode"`
	EventDelivery       EventDelivery        `json:"event_delivery,omitempty" yaml:"event_delivery,omitempty"`
	ClockStart          *time.Time           `json:"clock_start,omitempty" yaml:"clock_start,omitempty"`
	TokenTTLs           TokenTTLs            `json:"token_ttls" yaml:"token_ttls"`
	Apps                []App                `json:"apps" yaml:"apps"`
	Users               []User               `json:"users" yaml:"users"`
	Departments         []Department         `json:"departments,omitempty" yaml:"departments,omitempty"`
	ApprovalDefinitions []ApprovalDefinition `json:"approval_definitions,omitempty" yaml:"approval_definitions,omitempty"`
	Chats               []Chat               `json:"chats" yaml:"chats"`
	CardTemplates       []CardTemplate       `json:"card_templates,omitempty" yaml:"card_templates,omitempty"`
}

// DefaultApp returns the public fixture credentials for the built-in bot.
func DefaultApp() App {
	return App{ //nolint:gosec // G101 flags the public credentials for the local demo bot.
		AppID: "cli_larkbox", AppSecret: "larkbox-local-secret",
		BotOpenID: "ou_larkbox_bot", BotName: "Larkbox Bot",
		VerificationToken: "larkbox-local-verification", EncryptKey: "larkbox-local-encryption",
		RedirectURIs: []string{"http://127.0.0.1:8080/callback", "http://localhost:8080/callback"},
	}
}

// Default returns independent settings and demo seed data for a local server.
func Default() Config {
	bot := DefaultApp()
	dept := Department{OpenID: "od-larkbox-engineering", ID: "engineering", Name: "Engineering"}
	alice := User{OpenID: "ou_larkbox_alice", UserID: "alice", UnionID: "on_larkbox_alice", Name: "Alice", Email: "alice@example.test", DepartmentOpenIDs: []string{dept.OpenID}}
	bob := User{OpenID: "ou_larkbox_bob", UserID: "bob", UnionID: "on_larkbox_bob", Name: "Bob", Email: "bob@example.test", DepartmentOpenIDs: []string{dept.OpenID}}
	return Config{
		Addr: "127.0.0.1:8080", StrictMode: true,
		TokenTTLs: TokenTTLs{Tenant: 7200, App: 7200, User: 7200, Refresh: 30 * 24 * 3600, Code: 300},
		Apps:      []App{bot}, Users: []User{alice, bob}, Departments: []Department{dept},
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
	if err := cfg.validateRedirectURIs(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func readFile(path string, cfg *Config) error {
	// #nosec G304 -- The operator selects this local file with --config.
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	isJSON := strings.EqualFold(filepath.Ext(path), ".json")
	// Check presence separately because both typed decoders conflate null
	// and omitted slices. Keep omitted lists and allow [] to clear them.
	var fields map[string]any
	if err := fileDecoder(body, isJSON)(&fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("expected a configuration object")
	}
	for name, value := range fields {
		switch strings.ToLower(name) {
		case "apps", "users", "chats", "departments", "approval_definitions", "card_templates":
			if value == nil {
				return fmt.Errorf("%s must be a seed list; use [] to clear it", name)
			}
		}
	}

	defaults := *cfg
	cfg.Apps, cfg.Users, cfg.Chats, cfg.Departments = nil, nil, nil, nil
	decode := fileDecoder(body, isJSON)
	if err := decode(cfg); err != nil {
		return err
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
	if cfg.Departments == nil {
		cfg.Departments = defaults.Departments
	}
	var extra any
	if err := decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected a single configuration document")
	}
	return nil
}

func fileDecoder(body []byte, isJSON bool) func(any) error {
	reader := bytes.NewReader(body)
	if isJSON {
		decoder := json.NewDecoder(reader)
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		return decoder.Decode
	}
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	return decoder.Decode
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
	if err := applySeedEnv("LARKBOX_DEPARTMENTS", &cfg.Departments); err != nil {
		return err
	}
	if err := applySeedEnv("LARKBOX_APPROVAL_DEFINITIONS", &cfg.ApprovalDefinitions); err != nil {
		return err
	}
	if err := applySeedEnv("LARKBOX_CHATS", &cfg.Chats); err != nil {
		return err
	}
	if err := applySeedEnv("LARKBOX_CARD_TEMPLATES", &cfg.CardTemplates); err != nil {
		return err
	}
	return cfg.applyEventEnv()
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
	if err := cfg.EventDelivery.validate(); err != nil {
		return err
	}
	if cfg.ClockStart != nil && !simclock.ValidTime(*cfg.ClockStart) {
		return errors.New("clock_start must be nonzero and keep card expiry timestamps in the RFC3339 range")
	}
	if err := cfg.ValidateSeeds(); err != nil {
		return err
	}
	if err := cfg.ValidateDirectory(); err != nil {
		return err
	}
	return cfg.ValidateApprovals()
}

// ValidateSeeds checks seed list keys and configuration fields.
func (cfg Config) ValidateSeeds() error {
	seenTemplates := make(map[[2]string]bool, len(cfg.CardTemplates))
	for _, template := range cfg.CardTemplates {
		key := [2]string{template.TemplateID, template.VersionName}
		var doc struct {
			Schema string `json:"schema"`
		}
		if strings.TrimSpace(template.TemplateID) == "" || json.Unmarshal([]byte(template.Data), &doc) != nil || doc.Schema != "2.0" {
			return errors.New("card_templates entries require template_id and card JSON 2.0 data")
		}
		if seenTemplates[key] {
			return fmt.Errorf("duplicate card template %q version %q", template.TemplateID, template.VersionName)
		}
		seenTemplates[key] = true
	}
	if err := validateKeys("apps", cfg.Apps, func(app App) string { return app.AppID }); err != nil {
		return err
	}
	for _, app := range cfg.Apps {
		if err := app.BotMenu.Validate(); err != nil {
			return fmt.Errorf("app %q bot_menu: %w", app.AppID, err)
		}
		seen := make(map[string]bool, len(app.EventTypes))
		for _, kind := range app.EventTypes {
			if kind == "" || strings.ContainsAny(kind, " \t\r\n") || seen[kind] {
				return fmt.Errorf("app %q event_types must contain unique, nonempty event types", app.AppID)
			}
			seen[kind] = true
		}
	}
	if err := validateKeys("users", cfg.Users, func(user User) string { return user.OpenID }); err != nil {
		return err
	}
	if err := cfg.validateDepartmentKeys(); err != nil {
		return err
	}
	if err := cfg.validateApprovalKeys(); err != nil {
		return err
	}
	if err := validateKeys("chats", cfg.Chats, func(chat Chat) string { return chat.ChatID }); err != nil {
		return err
	}
	for _, chat := range cfg.Chats {
		if chat.ChatMode == "" {
			return fmt.Errorf("chat %q must have chat_mode p2p or group", chat.ChatID)
		}
		if field, description := chat.NormalizeSettings(); field != "" {
			return fmt.Errorf("chat %q has invalid %s (%s)", chat.ChatID, field, description)
		}
	}
	return cfg.validateRedirectURIs()
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
