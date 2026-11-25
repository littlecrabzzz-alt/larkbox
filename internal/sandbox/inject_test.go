package sandbox_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
)

func TestInjectEventUsesConfiguredReceiverAndTrace(t *testing.T) {
	type envelope struct {
		Schema string `json:"schema"`
		Header struct {
			ID   string `json:"event_id"`
			App  string `json:"app_id"`
			Kind string `json:"event_type"`
		} `json:"header"`
		Event map[string]string `json:"event"`
	}
	received := make(chan envelope, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body envelope
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	cfg := config.Default()
	app := &cfg.Apps[0]
	app.WebhookURL, app.EncryptKey = receiver.URL, ""
	srv := sandboxServer(t, cfg)
	const route = "/_sandbox/events/inject"
	const kind = "example.custom_v1"
	raw := call(t, srv, http.MethodPost, route,
		`{"app_id":"cli_larkbox","event_type":"example.custom_v1","event":{"key":"data"}}`, http.StatusOK)
	var outcome struct {
		ID         string `json:"event_id"`
		Deliveries []struct {
			Channel string `json:"channel"`
			Status  int    `json:"status_code"`
		} `json:"deliveries"`
	}
	if err := json.Unmarshal(raw, &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.ID == "" || len(outcome.Deliveries) != 1 || outcome.Deliveries[0].Channel != "webhook" ||
		outcome.Deliveries[0].Status != http.StatusOK {
		t.Fatalf("unexpected injection outcome: %s", raw)
	}
	body := <-received
	if body.Schema != "2.0" || body.Header.ID != outcome.ID || body.Header.App != app.AppID ||
		body.Header.Kind != kind || body.Event["key"] != "data" {
		t.Fatalf("unexpected event: %+v", body)
	}
	trace := call(t, srv, http.MethodGet, "/_sandbox/traces?event_id="+outcome.ID, "", http.StatusOK)
	assertContains(t, trace, outcome.ID, kind, `"attempt":1`, `"channel":"webhook"`)
	call(t, srv, http.MethodPost, route, `{"app_id":"missing","event_type":"x","event":{}}`, http.StatusNotFound)
	call(t, srv, http.MethodPost, route, `{"app_id":"cli_larkbox","event_type":"x","event":[]}`, http.StatusBadRequest)
}
