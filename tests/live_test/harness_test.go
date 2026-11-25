//go:build live

package livetest

import (
	"context"
	"net/http"
	"strings"
	"testing"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/littlecrabzzz-alt/larkbox/tests/fixturetest"
)

func TestDotEnv(t *testing.T) {
	env, err := parseEnv(strings.NewReader(`
# Repository-local settings
export LARKBOX_DOTENV_EXPORT='ignored # comment'
LARKBOX_DOTENV_EMPTY='ignored too'
LARKBOX_DOTENV_PARSED="file#value" # trailing comment
`))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LARKBOX_DOTENV_EXPORT", "exported")
	t.Setenv("LARKBOX_DOTENV_EMPTY", "")
	if env.get("LARKBOX_DOTENV_EXPORT") != "exported" || env.get("LARKBOX_DOTENV_EMPTY") != "" {
		t.Fatal("file settings took precedence over exported settings")
	}
	if env["LARKBOX_DOTENV_EXPORT"] != "ignored # comment" || env["LARKBOX_DOTENV_PARSED"] != "file#value" {
		t.Fatal("quoted file settings were not parsed")
	}
	if _, err := parseEnv(strings.NewReader("VAR='unclosed")); err == nil || strings.Contains(err.Error(), "unclosed") {
		t.Fatal("malformed-value errors must identify the line without including the value")
	}
}

func TestValidateBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		allowed   bool
	}{
		{"Feishu HTTPS", "https://open.feishu.cn", true},
		{"localhost HTTP", "http://localhost:8080", true},
		{"IPv4 loopback HTTP", "http://127.0.0.1", true},
		{"IPv6 loopback HTTP", "http://[::1]:8080", true},
		{"non-loopback HTTP", "http://open.feishu.cn", false},
		{"hostname suffix", "http://localhost.example.test", false},
		{"remote IPv4", "http://192.0.2.1:8080", false},
		{"remote IPv6", "http://[2001:db8::1]:8080", false},
		{"credentials", "https://example-user:example-password@open.feishu.cn", false},
		{"query", "https://open.feishu.cn/?x=example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBaseURL(tc.url); (err == nil) != tc.allowed {
				t.Fatalf("base URL accepted = %t, want %t", err == nil, tc.allowed)
			}
		})
	}
}

func TestLiveResponseShapes(t *testing.T) {
	t.Run("auth diagnostics", func(t *testing.T) {
		fixturetest.Envelope(t, http.StatusBadRequest, []byte(`{
			"code":99991663,"msg":"invalid token","error":{"log_id":"example","troubleshooter":"https://example.test","message":"invalid"}
		}`), http.StatusBadRequest, liveErrorShape(99991665), 99991663)
		fixturetest.Envelope(t, http.StatusBadRequest, []byte(`{"code":9499,"msg":"bad request"}`),
			http.StatusBadRequest, liveErrorShape(9499))
	})
	t.Run("app token prefixes", func(t *testing.T) {
		for _, token := range []string{"a-example", "t-example"} {
			raw := `{"code":0,"msg":"ok","expire":7200,"tenant_access_token":"t-example","app_access_token":"` + token + `"}`
			fixturetest.Envelope(t, http.StatusOK, []byte(raw), http.StatusOK, appEnvelope)
		}
	})
	t.Run("message context and text", func(t *testing.T) {
		fixturetest.Envelope(t, http.StatusOK, []byte(`{
			"code":0,"msg":"success","data":{"message_id":"om_example","msg_type":"text","chat_id":"oc_example",
			"create_time":"1234567890123","body":{"content":"{\"text\": \"larkbox differential test\"}"},
			"sender":{"id":"cli_example","id_type":"app_id","sender_type":"app"},"message_position":"100"}
		}`), http.StatusOK, messageShape)
		content := `{ "text" : "larkbox differential test" }`
		if !sameText(&larkim.MessageBody{Content: &content}, testText) {
			t.Fatal("whitespace changed the decoded text comparison")
		}
	})
}

func TestMutationCleanup(t *testing.T) {
	t.Run("fallback after incomplete scenario", func(t *testing.T) {
		called := false
		t.Run("register fallback", func(t *testing.T) {
			registerMutation(t, "test mutation", func(ctx context.Context) (*larkcore.ApiResp, error) {
				called = ctx.Err() == nil
				return &larkcore.ApiResp{StatusCode: http.StatusOK, RawBody: []byte(okData)}, nil
			}, nil)
		})
		if !called {
			t.Fatal("fallback did not run with an active context")
		}
	})
	t.Run("skip fallback after success", func(t *testing.T) {
		called := false
		t.Run("complete mutation", func(t *testing.T) {
			apply := func(context.Context) (*larkcore.ApiResp, error) {
				return &larkcore.ApiResp{StatusCode: http.StatusOK, RawBody: []byte(okData)}, nil
			}
			fallback := func(context.Context) (*larkcore.ApiResp, error) {
				called = true
				return nil, nil
			}
			registerMutation(t, "test mutation", apply, fallback)()
		})
		if called {
			t.Fatal("fallback ran after the operation succeeded")
		}
	})
}
