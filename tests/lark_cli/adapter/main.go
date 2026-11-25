//go:build lark_cli_probe

// This entry point builds in the pinned upstream module, using its CLI commands
// and its public transport extension.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/larksuite/cli/cmd"
	_ "github.com/larksuite/cli/extension/credential/env"
	"github.com/larksuite/cli/extension/transport"
)

type loopback struct{ origin *url.URL }

func (l loopback) Name() string { return "larkbox-loopback" }

func (l loopback) ResolveInterceptor(context.Context) transport.Interceptor { return l }

func (l loopback) ResolveURLRewriter(context.Context) transport.URLRewriter { return l }

func (l loopback) RewriteURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !feishuEndpoint(u) {
		return raw
	}
	u.Scheme, u.Host = l.origin.Scheme, l.origin.Host
	return u.String()
}

func (l loopback) PreRoundTrip(*http.Request) func(*http.Response, error) { return nil }

func (l loopback) PreRoundTripE(r *http.Request) (func(*http.Response, error), error) {
	if feishuEndpoint(r.URL) {
		r.URL.Scheme, r.URL.Host = l.origin.Scheme, l.origin.Host
		r.Host = l.origin.Host
	}
	if r.URL.Scheme != l.origin.Scheme || r.URL.Host != l.origin.Host || r.URL.User != nil {
		return nil, errors.New("the probe only sends requests to the local sandbox")
	}
	return nil, nil
}

func feishuEndpoint(u *url.URL) bool {
	return u.User == nil && u.Scheme == "https" && (u.Host == "open.feishu.cn" || u.Host == "accounts.feishu.cn")
}

func main() {
	origin, err := url.Parse(os.Getenv("LARKBOX_CLI_ORIGIN"))
	if err != nil || origin.Scheme != "http" || origin.Hostname() != "127.0.0.1" || origin.Port() == "" ||
		origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		fmt.Fprintln(os.Stderr, "LARKBOX_CLI_ORIGIN must be http://127.0.0.1:<port>")
		os.Exit(2)
	}
	transport.Register(loopback{origin})
	os.Exit(cmd.Execute())
}
