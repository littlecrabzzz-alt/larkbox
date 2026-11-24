package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestWebhookRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"empty", nil},
		{"short", []byte("hello world")},
		{"full block", bytes.Repeat([]byte{'x'}, 16)},
		{"UTF-8 JSON", []byte(`{"type":"challenge","data":"飞书"}`)},
		{"padding-like trailing bytes", []byte{'x', 3, 2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const key = "example"
			first, err := EncryptWebhook(key, tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			second, err := EncryptWebhook(key, tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(first, second) {
				t.Fatal("same payload reused an IV")
			}
			var envelope encryptedWebhook
			if err := json.Unmarshal(first, &envelope); err != nil {
				t.Fatal(err)
			}
			raw, err := base64.StdEncoding.DecodeString(envelope.Encrypt)
			if err != nil || len(raw) != 16+(len(tc.payload)/16+1)*16 {
				t.Fatalf("invalid IV and ciphertext size: %d, %v", len(raw), err)
			}
			got, err := DecryptWebhook(key, first)
			if err != nil || !bytes.Equal(got, tc.payload) {
				t.Fatalf("DecryptWebhook = %q, %v, want %q", got, err, tc.payload)
			}
		})
	}
}

func TestDecryptWebhook(t *testing.T) {
	for _, tc := range []struct {
		name, envelope, want string
	}{
		// Published Feishu example in the event security verification guide.
		{"Feishu vector", `{"encrypt":"P37w+VZImNgPEO1RBhJ6RtKl7n6zymIbEG1pReEzghk="}`, "hello world"},
		{"invalid JSON", `{"encrypt":`, ""},
		{"missing ciphertext", `{}`, ""},
		{"invalid base64", `{"encrypt":"!"}`, ""},
		{"IV only", `{"encrypt":"AAAAAAAAAAAAAAAAAAAAAA=="}`, ""},
		{"partial block", `{"encrypt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`, ""},
		{"invalid padding", `{"encrypt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecryptWebhook("test key", []byte(tc.envelope))
			if tc.want == "" {
				if err == nil {
					t.Fatalf("unexpected plaintext: %q", got)
				}
			} else if err != nil || string(got) != tc.want {
				t.Fatalf("DecryptWebhook = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestUnpadWebhook(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want string
		ok   bool
	}{
		{"empty plaintext", bytes.Repeat([]byte{16}, 16), "", true},
		{"one byte padding", []byte{'a', 1}, "a", true},
		{"valid repeated padding", []byte{'a', 2, 2}, "a", true},
		{"empty ciphertext", nil, "", false},
		{"zero padding", []byte{0}, "", false},
		{"padding beyond data", []byte{2}, "", false},
		{"padding beyond block", bytes.Repeat([]byte{17}, 17), "", false},
		{"inconsistent padding", []byte{'a', 1, 2}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unpadWebhook(tc.in)
			if (err == nil) != tc.ok || string(got) != tc.want {
				t.Fatalf("unpadWebhook = %q, %v, want %q (ok=%t)", got, err, tc.want, tc.ok)
			}
		})
	}
}

func TestWebhookSignature(t *testing.T) {
	const signature = "86583ca69c01045893ce8c345ae8284b43941899ef7d787fe7614b34f552000f"
	const timestamp, nonce, key, body = "1700000000", "nonce42", "callback-key", `{"encrypt":"AQID"}`
	if got := WebhookSignature(timestamp, nonce, key, []byte(body)); got != signature {
		t.Fatalf("signature = %q, want %q", got, signature)
	}
	for _, tc := range []struct {
		name, signature, timestamp, nonce, key, body string
		want                                         bool
	}{
		{"valid", signature, timestamp, nonce, key, body, true},
		{"empty", "", timestamp, nonce, key, body, false},
		{"wrong signature", "f" + signature[1:], timestamp, nonce, key, body, false},
		{"wrong timestamp", signature, "1700000001", nonce, key, body, false},
		{"wrong nonce", signature, timestamp, "nonce43", key, body, false},
		{"wrong key", signature, timestamp, nonce, "other-key", body, false},
		{"body whitespace change", signature, timestamp, nonce, key, body + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyWebhookSignature(tc.signature, tc.timestamp, tc.nonce, tc.key, []byte(tc.body)); got != tc.want {
				t.Errorf("VerifyWebhookSignature = %t, want %t", got, tc.want)
			}
		})
	}
}
