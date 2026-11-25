//go:build live

// Package livetest runs Feishu contracts against the sandbox or a test tenant.
package livetest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/server"
	"github.com/littlecrabzzz-alt/larkbox/tests/fixturetest"
)

const (
	botInfoPath = "/open-apis/bot/v3/info"
	okData      = `{"code":0,"msg":"","data":{}}`
	liveOKData  = `{"code":0,"msg":"","data?":{}}`
	diagnostics = `"error?":{"log_id?":"","message?":"","troubleshooter?":"",
		"helps?":[{"url?":"","description?":""}],
		"field_violations?":[{"field?":"","description?":"","value?":""}],
		"permission_violations?":[{"type?":"","subject?":"","description?":""}],
		"details?":[{"key?":"","value?":""}]}`
)

type harness struct {
	base, appID, appSecret, chatID, openID string
	live                                   bool
	http                                   *http.Client
	sdk                                    *lark.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	env, err := loadEnv()
	if err != nil {
		t.Fatal(err)
	}
	get := env.get

	h := &harness{http: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	h.live = get("FEISHU_LIVE") == "1"
	if h.live {
		h.appID, h.appSecret = get("FEISHU_APP_ID"), get("FEISHU_APP_SECRET")
		if h.appID == "" || h.appSecret == "" {
			t.Fatal("FEISHU_LIVE=1 requires FEISHU_APP_ID and FEISHU_APP_SECRET")
		}
		h.base = get("FEISHU_BASE_URL")
		if h.base == "" {
			h.base = "https://open.feishu.cn"
		}
		if err := validateBaseURL(h.base); err != nil {
			t.Fatal(err)
		}
		h.chatID, h.openID = get("FEISHU_TEST_CHAT_ID"), get("FEISHU_TEST_OPEN_ID")
		t.Log("target: Feishu test tenant")
	} else {
		cfg := config.Default()
		srv := server.New(cfg)
		web := httptest.NewServer(srv.Handler())
		t.Cleanup(web.Close)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				t.Error("sandbox shutdown failed")
			}
		})
		app := cfg.Apps[0]
		h.base, h.appID, h.appSecret = web.URL, app.AppID, app.AppSecret
		h.chatID, h.openID = cfg.Chats[0].ChatID, cfg.Users[0].OpenID
		t.Log("target: local larkbox")
	}
	h.base = strings.TrimRight(h.base, "/")
	h.sdk = lark.NewClient(h.appID, h.appSecret, lark.WithOpenBaseUrl(h.base),
		lark.WithHttpClient(h.http), lark.WithEnableTokenCache(false), lark.WithLogger(quietLogger{}))
	return h
}

func validateBaseURL(raw string) error {
	base, err := url.Parse(raw)
	if err != nil || base.Hostname() == "" || (base.Scheme != "https" && base.Scheme != "http") ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return errors.New("FEISHU_BASE_URL must be an HTTP(S) base URL without credentials, query, or fragment")
	}
	switch strings.ToLower(base.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}
	if base.Scheme != "https" {
		return errors.New("FEISHU_BASE_URL requires HTTPS for non-loopback hosts")
	}
	return nil
}

type quietLogger struct{}

func (quietLogger) Debug(context.Context, ...any) {}
func (quietLogger) Info(context.Context, ...any)  {}
func (quietLogger) Warn(context.Context, ...any)  {}
func (quietLogger) Error(context.Context, ...any) {}

type dotEnv map[string]string

func (env dotEnv) get(key string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return env[key]
}

func loadEnv() (dotEnv, error) {
	file, err := os.Open("../../.env.local")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot open .env.local")
	}
	defer func() { _ = file.Close() }()
	return parseEnv(file)
}

func parseEnv(r io.Reader) (dotEnv, error) {
	env := dotEnv{}
	scanner := bufio.NewScanner(r)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf(".env.local line %d: expected NAME=value", lineNumber)
		}
		value, err := envValue(value)
		if err != nil {
			return nil, fmt.Errorf(".env.local line %d: invalid quoted value", lineNumber)
		}
		env[key] = value
	}
	if scanner.Err() != nil {
		return nil, errors.New("cannot read .env.local")
	}
	return env, nil
}

func envValue(value string) (string, error) {
	if strings.HasPrefix(value, "#") {
		return "", nil
	}
	if strings.HasPrefix(value, "'") {
		text, tail, ok := strings.Cut(value[1:], "'")
		if ok && commentOrEmpty(tail) {
			return text, nil
		}
		return "", errors.New("invalid single-quoted value")
	}
	if strings.HasPrefix(value, `"`) {
		quoted, err := strconv.QuotedPrefix(value)
		if err != nil || !commentOrEmpty(value[len(quoted):]) {
			return "", errors.New("invalid double-quoted value")
		}
		return strconv.Unquote(quoted)
	}
	if i := strings.IndexAny(value, " \t"); i >= 0 && strings.HasPrefix(strings.TrimSpace(value[i:]), "#") {
		return value[:i], nil
	}
	return value, nil
}

func commentOrEmpty(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || strings.HasPrefix(value, "#")
}

func requireSDK(t *testing.T, operation string, err error) {
	t.Helper()
	if err != nil {
		var apiErr *larkcore.CodeError
		if errors.As(err, &apiErr) {
			t.Fatalf("%s: SDK error code %d", operation, apiErr.Code)
		}
		t.Fatalf("%s: SDK failed (%T)", operation, err)
	}
}

func liveErrorShape(code int) string {
	return `{"code":` + strconv.Itoa(code) + `,"msg":"","data?":{},` + diagnostics + `}`
}

func (h *harness) checkResponse(t *testing.T, name string, resp *larkcore.ApiResp, status int, shape string) {
	t.Helper()
	h.checkWire(t, name, resp.StatusCode, resp.RawBody, status, shape)
}

func (h *harness) checkWire(t *testing.T, name string, status int, raw []byte, liveStatus int, liveShape string) {
	t.Helper()
	if h.live {
		captureShape(t, name, status, raw, liveShape)
		fixturetest.Envelope(t, status, raw, liveStatus, liveShape)
		return
	}
	fixturetest.Golden(t, name, status, raw, false)
}

func (h *harness) expectOK(t *testing.T, resp *larkcore.ApiResp) {
	t.Helper()
	shape := okData
	if h.live {
		shape = liveOKData
	}
	fixturetest.Envelope(t, resp.StatusCode, resp.RawBody, http.StatusOK, shape)
}

type mutation func(context.Context) (*larkcore.ApiResp, error)

func registerMutation(t *testing.T, operation string, apply, fallback mutation) func() *larkcore.ApiResp {
	t.Helper()
	done := false
	if fallback == nil {
		fallback = apply
	}
	t.Cleanup(func() {
		if done {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := runMutation(ctx, fallback); err != nil {
			t.Errorf("%s cleanup: %v", operation, err)
		}
	})
	return func() *larkcore.ApiResp {
		t.Helper()
		resp, err := runMutation(t.Context(), apply)
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
		done = true
		return resp
	}
}

func runMutation(ctx context.Context, apply mutation) (*larkcore.ApiResp, error) {
	response, err := apply(ctx)
	if err != nil {
		return nil, fmt.Errorf("SDK failed (%T)", err)
	}
	if response == nil {
		return nil, errors.New("missing SDK response")
	}
	var result struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(response.RawBody, &result) != nil || result.Code == nil {
		return nil, fmt.Errorf("missing business code (HTTP %d)", response.StatusCode)
	}
	if response.StatusCode != http.StatusOK || *result.Code != 0 {
		return nil, fmt.Errorf("HTTP %d, business code %d", response.StatusCode, *result.Code)
	}
	return response, nil
}

func value[T any](p *T) T {
	if p != nil {
		return *p
	}
	var zero T
	return zero
}
