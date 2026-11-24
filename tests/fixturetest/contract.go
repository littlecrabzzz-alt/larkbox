// Package fixturetest asserts Feishu wire contracts for the integration suites.
package fixturetest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// Root is relative to both integration test packages.
const Root = "../../testdata/fixtures"

type fixture struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// Golden checks a fixture's HTTP status, business code, and wire shape.
// When exact is true, stable values and the contents of arrays also match.
func Golden(t *testing.T, name string, status int, body []byte, exact bool) {
	t.Helper()
	path := name + ".json"
	raw, err := os.ReadFile(filepath.Join(Root, path)) // #nosec G304 -- callers supply repository fixture names.
	if err != nil {
		t.Fatal(err)
	}
	var want fixture
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("%s: invalid fixture", name)
	}
	compare(t, name, status, body, want.Status, decode(t, want.Body), exact)
}

// Envelope checks an HTTP response with a small inline contract.
func Envelope(t *testing.T, status int, body []byte, wantStatus int, shape string) {
	t.Helper()
	compare(t, "envelope", status, body, wantStatus, decode(t, []byte(shape)), false)
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		t.Fatal("expected a JSON object")
	}
	return object
}

func compare(t *testing.T, name string, status int, body []byte, wantStatus int, want map[string]any, exact bool) {
	t.Helper()
	if status != wantStatus {
		t.Errorf("%s: HTTP status = %d, want %d", name, status, wantStatus)
	}
	got := decode(t, body)
	if code, ok := want["code"]; ok && got["code"] != code {
		// Only print numeric business codes. Unexpected bodies can carry credentials.
		number, _ := got["code"].(float64)
		t.Errorf("%s: business code = %v, want %v", name, number, code)
	}
	if err := match(name, got, want, exact); err != nil {
		t.Fatal(err)
	}
}

var dynamic = map[string]*regexp.Regexp{
	"fixture:id":        regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`),
	"fixture:micros":    regexp.MustCompile(`^[0-9]{16}$`),
	"fixture:card_id":   regexp.MustCompile(`^[0-9]{1,20}$`),
	"fixture:millis":    regexp.MustCompile(`^[0-9]{13}$`),
	"fixture:encrypt":   regexp.MustCompile(`^[A-Za-z0-9+/]{24,}={0,2}$`),
	"fixture:tenant":    regexp.MustCompile(`^t-.+$`),
	"fixture:app_token": regexp.MustCompile(`^a-.+$`),
	"fixture:nonce":     regexp.MustCompile(`^[[:xdigit:]]{32}$`),
}

func match(path string, got, want any, exact bool) error {
	if fmt.Sprintf("%T", got) != fmt.Sprintf("%T", want) {
		return fmt.Errorf("%s: got %T, want %T", path, got, want)
	}
	switch expected := want.(type) {
	case map[string]any:
		actual := got.(map[string]any)
		if len(actual) != len(expected) {
			return fmt.Errorf("%s: got %d keys, want %v", path, len(actual), slices.Sorted(maps.Keys(expected)))
		}
		for _, key := range slices.Sorted(maps.Keys(expected)) {
			value, ok := actual[key]
			if !ok {
				return fmt.Errorf("%s: missing key %q", path, key)
			}
			if err := match(path+"."+key, value, expected[key], exact); err != nil {
				return err
			}
		}
	case []any:
		actual := got.([]any)
		if exact && len(actual) != len(expected) {
			return fmt.Errorf("%s: got %d items, want %d", path, len(actual), len(expected))
		}
		if len(expected) > 0 {
			for i, value := range actual {
				if err := match(fmt.Sprintf("%s[%d]", path, i), value, expected[i%len(expected)], exact); err != nil {
					return err
				}
			}
		}
	case string:
		if pattern := dynamic[expected]; pattern != nil {
			if !pattern.MatchString(got.(string)) {
				return fmt.Errorf("%s: value does not match %s", path, expected)
			}
		} else if exact && got != want {
			return fmt.Errorf("%s: fixed string differs from fixture", path)
		}
	default:
		if exact && got != want {
			return fmt.Errorf("%s: fixed value differs from fixture", path)
		}
	}
	return nil
}

// Request makes a raw request when a generated SDK method cannot send its body
// or omit its credentials. It keeps transport details out of test logs.
func Request(t *testing.T, client *http.Client, base, method, path, token string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: cannot build request", method, path)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: transport error (%T)", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: cannot read response", method, path)
	}
	return resp.StatusCode, raw
}
