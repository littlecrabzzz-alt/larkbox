//go:build live

package livetest

import (
	"strings"
	"testing"
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
