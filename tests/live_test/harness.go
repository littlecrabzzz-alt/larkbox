//go:build live

// Package livetest runs Feishu contracts against the sandbox or a test tenant.
package livetest

import (
	"bufio"
	"context"
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
)

type harness struct {
	base, appID, appSecret, chatID, openID string
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
	if get("FEISHU_LIVE") == "1" {
		h.appID, h.appSecret = get("FEISHU_APP_ID"), get("FEISHU_APP_SECRET")
		if h.appID == "" || h.appSecret == "" {
			t.Fatal("FEISHU_LIVE=1 requires FEISHU_APP_ID and FEISHU_APP_SECRET")
		}
		h.base = get("FEISHU_BASE_URL")
		if h.base == "" {
			h.base = "https://open.feishu.cn"
		}
		base, err := url.Parse(h.base)
		if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") ||
			base.User != nil || base.RawQuery != "" || base.Fragment != "" {
			t.Fatal("FEISHU_BASE_URL must be an HTTP(S) base URL without credentials, query, or fragment")
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

func golden(t *testing.T, name string, resp *larkcore.ApiResp) {
	t.Helper()
	fixturetest.Golden(t, name, resp.StatusCode, resp.RawBody, false)
}

func expectOK(t *testing.T, resp *larkcore.ApiResp) {
	t.Helper()
	fixturetest.Envelope(t, resp.StatusCode, resp.RawBody, http.StatusOK, okData)
}

func value[T any](p *T) T {
	if p != nil {
		return *p
	}
	var zero T
	return zero
}
