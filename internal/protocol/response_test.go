package protocol

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestWriteError(t *testing.T) {
	for _, tc := range []struct {
		code   Code
		number int
		status int
	}{
		{InvalidToken, 99991661, http.StatusBadRequest},
		{ExpiredTenantToken, 99991663, http.StatusBadRequest},
		{InvalidAppToken, 99991664, http.StatusBadRequest},
		{InvalidUserToken, 99991668, http.StatusBadRequest},
		{InsufficientScope, 99991672, http.StatusForbidden},
		{RateLimited, 99991400, http.StatusTooManyRequests},
		{ResourceNotFound, 99991201, http.StatusNotFound},
		{FieldValidation, 99992402, http.StatusBadRequest},
		{IMInvalidParameter, 230001, http.StatusBadRequest},
		{IMBotOutsideChat, 230002, http.StatusBadRequest},
		{InvalidParameter, 10003, http.StatusOK},
		{WrongAppSecret, 10015, http.StatusOK},
		{InvalidOAuthCode, 20003, http.StatusOK},
		{ExpiredOAuthCode, 20004, http.StatusOK},
		{12, 12, http.StatusInternalServerError},
	} {
		t.Run(NewError(tc.code, nil).Message, func(t *testing.T) {
			w := httptest.NewRecorder()
			details := &ErrorDetails{LogID: "local-id", FieldViolations: []FieldViolation{{Field: "name", Description: "required"}}}
			if err := WriteError(w, NewError(tc.code, details)); err != nil {
				t.Fatal(err)
			}
			var got struct {
				Code  int           `json:"code"`
				Msg   string        `json:"msg"`
				Error *ErrorDetails `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || got.Code != tc.number || got.Msg == "" || !reflect.DeepEqual(got.Error, details) {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body)
			}
			if w.Header().Get("Content-Type") != "application/json; charset=utf-8" || strings.Contains(w.Body.String(), `"data"`) {
				t.Fatalf("unexpected error headers or body: %v %s", w.Header(), w.Body)
			}
		})
	}
}

func TestWriteSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		data any
		want string
	}{
		{"empty", nil, `{"code":0,"msg":"ok","data":{}}`},
		{"payload", map[string]string{"chat_id": "oc_demo"}, `{"code":0,"msg":"ok","data":{"chat_id":"oc_demo"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if err := WriteSuccess(w, tc.data); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || w.Body.String() != tc.want {
				t.Fatalf("HTTP %d: %s, want %s", w.Code, w.Body, tc.want)
			}
		})
	}
}

func TestNewID(t *testing.T) {
	for _, prefix := range []IDPrefix{OpenID, UnionID, ChatID, MessageID, ImageKey, FileKey, TenantToken, AppToken, UserToken, RefreshToken, CardID, EventID, FrameID} {
		t.Run(string(prefix), func(t *testing.T) {
			id := NewID(prefix)
			suffix, ok := strings.CutPrefix(id, string(prefix))
			decoded, err := hex.DecodeString(suffix)
			if !ok || err != nil || len(decoded) != 16 || id == NewID(prefix) {
				t.Fatalf("invalid or repeated ID %s", id)
			}
		})
	}
}
