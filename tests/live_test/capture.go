//go:build live

package livetest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureShape(t *testing.T, name string, status int, raw []byte, expected string) {
	t.Helper()
	dir := os.Getenv("FEISHU_LIVE_ARTIFACT_DIR")
	if dir == "" {
		return
	}
	var observed, schema map[string]any
	if json.Unmarshal([]byte(expected), &schema) != nil {
		t.Fatal("invalid capture schema")
	}
	gotJSON := json.Unmarshal(raw, &observed) == nil && observed != nil
	result := map[string]any{"contract": name, "http_status": status, "valid_json": gotJSON}
	if gotJSON {
		if code, ok := observed["code"].(float64); ok {
			result["business_code"] = code
		}
		result["fields"] = describeShape(observed, schema)
	}
	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal("encode sanitized capture")
	}
	// #nosec G703 -- The operator selects the output directory for sanitized artifacts.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal("create sanitized capture directory")
	}
	filename := strings.ReplaceAll(name, "/", "-") + ".json"
	// #nosec G703 -- The directory is operator-selected and names come from fixed fixtures.
	if err := os.WriteFile(filepath.Join(dir, filename), body, 0o600); err != nil {
		t.Fatal("write sanitized capture")
	}
}

func describeShape(observed, expected any) any {
	switch value := observed.(type) {
	case map[string]any:
		fields := map[string]any{}
		schema, _ := expected.(map[string]any)
		matched := 0
		for name, want := range schema {
			key := strings.TrimSuffix(name, "?")
			if field, ok := value[key]; ok {
				fields[key] = describeShape(field, want)
				matched++
			}
		}
		return map[string]any{"type": "object", "fields": fields, "unknown_fields": len(value) - matched}
	case []any:
		out := map[string]any{"type": "array", "items": len(value)}
		if schema, ok := expected.([]any); ok && len(schema) > 0 && len(value) > 0 {
			out["sample_shape"] = describeShape(value[0], schema[0])
		}
		return out
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	default:
		return "null"
	}
}
