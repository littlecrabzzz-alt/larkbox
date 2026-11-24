package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var hostLabel = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ParseRedirectURI validates an absolute HTTP(S) OAuth callback URL.
func ParseRedirectURI(raw string) (*url.URL, error) {
	if strings.ContainsAny(raw, "#\\") || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return nil, errors.New("callback URL must not contain a fragment, backslash, or whitespace")
	}
	callback, err := url.Parse(raw)
	if err != nil || (callback.Scheme != "http" && callback.Scheme != "https") ||
		callback.User != nil || !validCallbackHost(callback) {
		return nil, errors.New("callback URL must use HTTP or HTTPS with a valid host and port")
	}
	if _, err := url.ParseQuery(callback.RawQuery); err != nil {
		return nil, errors.New("callback URL contains an invalid query")
	}
	return callback, nil
}

func validCallbackHost(callback *url.URL) bool {
	authority, hostname := callback.Host, callback.Hostname()
	if port := callback.Port(); port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return false
		}
		authority = strings.TrimSuffix(authority, ":"+port)
	}
	if strings.HasPrefix(authority, "[") {
		ip, err := netip.ParseAddr(hostname)
		return err == nil && ip.Is6() && ip.Zone() == "" && authority == "["+hostname+"]"
	}
	domain := strings.TrimSuffix(hostname, ".")
	if authority != hostname || len(domain) > 253 {
		return false
	}
	for label := range strings.SplitSeq(domain, ".") {
		if !hostLabel.MatchString(label) {
			return false
		}
	}
	return true
}

func (cfg Config) validateRedirectURIs() error {
	for _, app := range cfg.Apps {
		for i, raw := range app.RedirectURIs {
			if _, err := ParseRedirectURI(raw); err != nil {
				return fmt.Errorf("app %q redirect_uris[%d]: %w", app.AppID, i, err)
			}
		}
	}
	return nil
}
