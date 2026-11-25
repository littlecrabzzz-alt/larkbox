//go:build live

package livetest

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

func TestUnknownTokenRevocationContract(t *testing.T) {
	h := newHarness(t)
	accounts := h.base
	if h.live {
		accounts = "https://accounts.feishu.cn"
	}
	// Random nonexistent credentials exercise the wire contract without
	// invalidating a credential held by the test tenant.
	for hint, value := range map[string]string{
		"access_token": protocol.NewID(protocol.UserToken), "refresh_token": protocol.NewID(protocol.RefreshToken),
	} {
		t.Run(hint, func(t *testing.T) {
			form := url.Values{"client_id": {h.appID}, "client_secret": {h.appSecret}, "token": {value}, "token_type_hint": {hint}}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, accounts+"/oauth/v1/revoke", strings.NewReader(form.Encode()))
			requireSDK(t, "build revocation request", err)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := h.http.Do(req)
			requireSDK(t, "send revocation request", err)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
			requireSDK(t, "read revocation response", err)
			if resp.StatusCode != http.StatusOK || len(body) != 0 {
				t.Fatalf("revocation returned HTTP %d with a %d-byte body, expected an empty HTTP 200", resp.StatusCode, len(body))
			}
		})
	}
}
