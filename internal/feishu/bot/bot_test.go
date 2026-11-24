package bot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestBotProfileBearerAndAudit(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state := store.New(func() time.Time { return now }, 0)
	cfg := config.Default()
	cfg.Apps[0].BotAvatarURL = "https://example.test/bot.png"
	app := cfg.Apps[0]
	withoutBot := config.App{AppID: "cli_disabled"}
	cfg.Apps = append(cfg.Apps, withoutBot)
	state.Seed(cfg)
	for _, token := range []store.Token{
		{Value: "t-bot", Kind: store.TenantToken, AppID: app.AppID, ExpiresAt: now.Add(time.Hour)},
		{Value: "a-bot", Kind: store.AppToken, AppID: app.AppID, ExpiresAt: now.Add(time.Hour)},
		{Value: "u-bot", Kind: store.UserToken, AppID: app.AppID, UserOpenID: cfg.Users[0].OpenID, ExpiresAt: now.Add(time.Hour)},
		{Value: "t-expired", Kind: store.TenantToken, AppID: app.AppID, ExpiresAt: now},
		{Value: "t-disabled", Kind: store.TenantToken, AppID: withoutBot.AppID, ExpiresAt: now.Add(time.Hour)},
	} {
		if err := state.PutToken(token); err != nil {
			t.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	RegisterRoutes(mux, state)
	handler := middleware.Audit(state, mux)
	for _, test := range []struct {
		name, authorization string
		code, status        int
	}{
		{"tenant", "Bearer t-bot", 0, http.StatusOK},
		{"missing", "", 99991661, http.StatusBadRequest},
		{"app token", "Bearer a-bot", 99991664, http.StatusBadRequest},
		{"user token", "Bearer u-bot", 99991668, http.StatusBadRequest},
		{"expired", "Bearer t-expired", 99991663, http.StatusBadRequest},
		{"unavailable bot", "Bearer t-disabled", 10014, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/open-apis/bot/v3/info", nil)
			if test.authorization != "" {
				r.Header.Set("Authorization", test.authorization)
			}
			handler.ServeHTTP(w, r)
			var wire struct {
				Code *int            `json:"code"`
				Msg  string          `json:"msg"`
				Bot  *profile        `json:"bot"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			if w.Code != test.status || wire.Code == nil || *wire.Code != test.code || len(wire.Data) != 0 {
				t.Fatalf("unexpected HTTP %d: %s", w.Code, w.Body)
			}
			traces := state.Traces()
			trace := traces[len(traces)-1]
			if trace.Code == nil || *trace.Code != test.code || trace.RequestHeaders.Get("Authorization") != "" {
				t.Fatalf("unexpected trace: %+v", trace)
			}
			if test.code == 0 && (wire.Msg != "ok" || wire.Bot == nil || wire.Bot.OpenID != app.BotOpenID ||
				wire.Bot.AppName != app.BotName || wire.Bot.AvatarURL != app.BotAvatarURL ||
				wire.Bot.ActivateStatus != 2 || wire.Bot.IPWhiteList == nil || len(wire.Bot.IPWhiteList) != 0 ||
				trace.AppID != app.AppID) {
				t.Fatalf("unexpected bot profile: %s", w.Body)
			}
		})
	}
}
