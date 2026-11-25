package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/event"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func TestSnapshotRoundTripAndRetryRecovery(t *testing.T) {
	at := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.ClockStart = &at
	cfg.SandboxToken = "snapshot-operator-fixture"
	cfg.StateFile = filepath.Join(t.TempDir(), "sandbox.json")
	var attempts atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer peer.Close()
	cfg.Apps[0].WebhookURL = peer.URL
	app, alice := cfg.Apps[0], cfg.Users[0]
	const bearer = "t-snapshot-fixture" // #nosec G101 -- Synthetic in-process credential.
	const artifact = "persistent test attachment"
	first := New(cfg)
	if err := first.LoadCheckpoint(); err != nil {
		t.Fatal(err)
	}
	state := first.Store()
	if err := state.PutToken(store.Token{Value: bearer, Kind: store.TenantToken, AppID: app.AppID, ExpiresAt: at.Add(30 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveAuthorizationCode(store.Token{
		Value: "snapshot-auth-grant", AppID: app.AppID, TenantKey: app.TenantKey, Kind: store.OAuthCode,
		UserOpenID: alice.OpenID, Scope: "im:message", ExpiresAt: at.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	owner := store.ResourceOwner{AppID: app.AppID, TenantKey: app.TenantKey}
	state.Media().Put(store.Media{Key: "file_snapshot", AppID: app.AppID, Bytes: []byte(artifact), Kind: "file"})
	if err := state.CreateCard(store.Card{ID: "1000000000000000000", AppID: app.AppID, Type: "card_json",
		JSON: json.RawMessage(`{"schema":"2.0","body":{"elements":[]}}`), Sequence: 7, StreamingMode: true}); err != nil {
		t.Fatal(err)
	}
	const chat = "oc_larkbox_group"
	state.Messages().Put(store.Message{ID: "om_snapshot", ChatID: chat, AppID: app.AppID, SenderOpenID: app.BotOpenID,
		Content: `{"text":"persisted message"}`, MsgType: "text", CreatedAt: at})
	book := state.CreateSpreadsheet(owner, "Restored workbook")
	if _, err := state.WriteSheetRange(owner, book.ID, book.SheetID, store.SheetRange{Rows: 1, Columns: 1},
		[][]json.RawMessage{{json.RawMessage(`{"value":37}`)}}, false); err != nil {
		t.Fatal(err)
	}
	const requestID = "req_checkpoint_probe"
	first.Events().Enqueue(store.WithRequestID(t.Context(), requestID), app.AppID, "im.message.receive_v1", at, map[string]any{"probe": true})
	awaitMetrics(t, first, func(m event.Metrics) bool { return m.RetryWaiters == 1 && m.Failures == 1 })
	initial := state.QueryTraces(store.TraceFilter{RequestID: requestID})
	if len(initial) != 1 || initial[0].EventID == "" || initial[0].Attempt != 1 {
		t.Fatalf("failed callback missing before shutdown: %+v", initial)
	}
	shutdownSnapshotServer(t, first)

	info, err := os.Stat(cfg.StateFile)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("checkpoint did not have private permissions: %v, %v", info, err)
	}
	second := New(cfg)
	defer shutdownSnapshotServer(t, second)
	if err := second.LoadCheckpoint(); err != nil {
		t.Fatal(err)
	}
	key := "Bearer " + cfg.SandboxToken
	got := deploymentRequest(second, http.MethodGet, "/open-apis/bot/v3/info", "Bearer "+bearer, "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"code":0`) {
		t.Fatalf("restored token was not usable: %s", got.Body)
	}
	if media, ok := second.Store().Media().Get("file_snapshot"); !ok || string(media.Bytes) != artifact {
		t.Fatalf("uploaded bytes not restored: %+v", media)
	}
	if card, ok := second.Store().Card("1000000000000000000"); !ok || card.Sequence != 7 || !card.StreamingMode || len(card.TemplateData) != 0 {
		t.Fatalf("card state not restored: %+v", card)
	}
	read := deploymentRequest(second, http.MethodGet, "/open-apis/im/v1/messages?container_id_type=chat&container_id="+chat, "Bearer "+bearer, "")
	if read.Code != 200 || !strings.Contains(read.Body.String(), "persisted message") {
		t.Fatalf("chat history index was not restored: %d %s", read.Code, read.Body)
	}
	if grants, err := second.Store().AuthorizedUsers(app.AppID); err != nil || len(grants) != 1 || grants[0].OpenID != alice.OpenID {
		t.Fatalf("restored grants = %+v, %v", grants, err)
	}
	cells, err := second.Store().ReadSheetRanges(owner, book.ID, book.SheetID, []store.SheetRange{{Rows: 1, Columns: 1}}, 0)
	if err != nil || len(cells.Ranges) != 1 || len(cells.Ranges[0]) != 1 || string(cells.Ranges[0][0][0]) != `{"value":37}` {
		t.Fatalf("spreadsheet cells not restored: %+v, %v", cells, err)
	}
	if attempts.Load() != 1 {
		t.Fatal("retry fired before the restored clock reached its due time")
	}
	if advanced := deploymentRequest(second, http.MethodPost, "/_sandbox/clock/advance", key, `{"by":"15s"}`); advanced.Code != 200 {
		t.Fatal(advanced.Body.String())
	}
	awaitMetrics(t, second, func(m event.Metrics) bool { return m.Pending == 0 && m.Attempts == 1 })
	delivered := second.Store().QueryTraces(store.TraceFilter{RequestID: requestID})
	if len(delivered) != 1 || delivered[0].Attempt != 2 || delivered[0].EventID != initial[0].EventID || delivered[0].Status != http.StatusNoContent {
		t.Fatalf("retry identity or attempt lost across restart: %+v", delivered)
	}

	path := "/_sandbox/snapshot"
	if rejected := deploymentRequest(second, http.MethodGet, path, "Bearer "+bearer, ""); rejected.Code != http.StatusUnauthorized {
		t.Fatalf("Feishu token accessed a private snapshot: %d", rejected.Code)
	}
	exported := deploymentRequest(second, http.MethodGet, path, key, "")
	if exported.Code != http.StatusOK || exported.Header().Get("Content-Disposition") == "" || !json.Valid(exported.Body.Bytes()) {
		t.Fatalf("export failed: %d", exported.Code)
	}
	unrecognized := bytes.Replace(exported.Body.Bytes(), []byte(`"version":1`), []byte(`"version":99`), 1)
	if rejected := deploymentRequest(second, http.MethodPost, path, key, string(unrecognized)); rejected.Code != http.StatusBadRequest {
		t.Fatalf("invalid version accepted: %d", rejected.Code)
	}
	if gone := deploymentRequest(second, http.MethodPost, "/_sandbox/reset", key, "{}"); gone.Code != http.StatusOK {
		t.Fatal(gone.Body.String())
	}
	if stale := deploymentRequest(second, http.MethodGet, "/open-apis/bot/v3/info", "Bearer "+bearer, ""); stale.Code != http.StatusBadRequest {
		t.Fatal("reset preserved a runtime token")
	}
	if imported := deploymentRequest(second, http.MethodPost, path, key, exported.Body.String()); imported.Code != 200 {
		t.Fatal(imported.Body.String())
	}
	if _, err := second.Store().CheckToken(bearer); err != nil {
		t.Fatalf("import lost the still-valid token: %v", err)
	}
	if advanced := deploymentRequest(second, http.MethodPost, "/_sandbox/clock/advance", key, `{"by":"15s"}`); advanced.Code != 200 {
		t.Fatal(advanced.Body.String())
	}
	if expired := deploymentRequest(second, http.MethodGet, "/open-apis/bot/v3/info", "Bearer "+bearer, ""); expired.Code != http.StatusBadRequest || !strings.Contains(expired.Body.String(), `"code":99991663`) {
		t.Fatalf("token did not expire at its original deadline: %d %s", expired.Code, expired.Body)
	}
	if attempts.Load() != 2 {
		t.Fatalf("completed delivery replayed after import: %d calls", attempts.Load())
	}
}

func awaitMetrics(t *testing.T, srv *Server, check func(event.Metrics) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check(srv.Events().Metrics()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("event dispatcher did not reach the expected state: %+v", srv.Events().Metrics())
}

func shutdownSnapshotServer(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
