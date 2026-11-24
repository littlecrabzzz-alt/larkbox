package cardkit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/littlecrabzzz-alt/larkbox/internal/config"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const (
	cardsPath    = "/open-apis/cardkit/v1/cards"
	testCardJSON = `{"schema":"2.0","config":{"streaming_mode":true,"summary":{"content":"initial"}},"body":{"elements":[{"tag":"column_set","columns":[{"tag":"column","element_id":"container","elements":[{"tag":"markdown","element_id":"a","content":"start"},{"tag":"hr","element_id":"hr"}]}]}]}}`
)

type fixture struct {
	mux     *http.ServeMux
	state   *store.Store
	elapsed *atomic.Int64
}

func setup(t *testing.T) fixture {
	t.Helper()
	var elapsed atomic.Int64
	start := time.Now()
	cfg := config.Default()
	other := cfg.Apps[0]
	other.AppID, other.BotOpenID = "cli_other", "ou_other"
	cfg.Apps = append(cfg.Apps, other)
	state := store.New(func() time.Time { return start.Add(time.Duration(elapsed.Load())) }, 0)
	state.Seed(cfg)
	for _, token := range []store.Token{
		{Value: "t-owner", Kind: store.TenantToken, AppID: cfg.Apps[0].AppID},
		{Value: "t-other", Kind: store.TenantToken, AppID: other.AppID},
		{Value: "u-owner", Kind: store.UserToken, AppID: cfg.Apps[0].AppID, UserOpenID: cfg.Users[0].OpenID},
	} {
		token.ExpiresAt = start.Add(3 * time.Hour)
		if err := state.PutToken(token); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	RegisterRoutes(mux, state)
	return fixture{mux, state, &elapsed}
}

type response struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		CardID string `json:"card_id"`
	} `json:"data"`
}

func (f fixture) call(t *testing.T, token, method, path, body string, want int) response {
	t.Helper()
	r := httptest.NewRequest(method, cardsPath+path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	var result response
	status := http.StatusOK
	if want != 0 {
		status = http.StatusBadRequest
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != status || result.Code != want {
		t.Errorf("%s %s: HTTP %d %s, want HTTP %d code %d (decode: %v)", method, path, w.Code, w.Body, status, want, err)
	}
	return result
}

func (f fixture) create(t *testing.T, kind, doc string) string {
	t.Helper()
	r := f.call(t, "t-owner", "POST", "", fmt.Sprintf(`{"type":%q,"data":%q}`, kind, doc), 0)
	if !regexp.MustCompile(`^[0-9]{1,20}$`).MatchString(r.Data.CardID) || r.Msg != "success" {
		t.Fatalf("invalid create response: %+v", r)
	}
	return r.Data.CardID
}

func TestCreateCardValidation(t *testing.T) {
	f := setup(t)
	for _, body := range []string{
		`{"type":"card_json","data":{}}`,
		`{"type":"card_json","data":"{"}`,
		`{"type":"card_json","data":"null"}`,
		`{"type":"card_json","data":"{\"elements\":[]}"}`,
		`{"type":"template","data":"{\"template_id\":42}"}`,
		`{"type":"template","data":"{\"template_id\":\"x\",\"template_variable\":[]}"}`,
		`{"type":"bogus","data":"{}"}`,
		fmt.Sprintf(`{"type":"card_json","data":%q}`, strings.Replace(testCardJSON, `"element_id":"hr"`, `"element_id":"a"`, 1)),
		fmt.Sprintf(`{"type":"card_json","data":%q}`, strings.Replace(testCardJSON, `"streaming_mode":true`, `"streaming_mode":"yes"`, 1)),
	} {
		f.call(t, "t-owner", "POST", "", body, 99992402)
	}
	f.call(t, "", "POST", "", `{}`, 99991661)
	f.call(t, "u-owner", "POST", "", `{}`, 99991668)
	if cards := f.state.Cards(); len(cards) != 0 {
		t.Fatalf("validation created cards: %+v", cards)
	}
	id := f.create(t, "card_json", testCardJSON)
	card, _ := f.state.Card(id)
	if card.Sequence != 0 || !card.StreamingMode || card.AppID != "cli_larkbox" || card.Elements["a"] != "start" || len(card.Elements) != 3 {
		t.Fatalf("invalid initial state: %+v", card)
	}
	id = f.create(t, "template", `{"template_id":"tpl","template_version_name":"1.0.0","template_variable":{"count":1}}`)
	card, _ = f.state.Card(id)
	if card.Type != "template" || card.StreamingMode || len(card.Elements) != 0 {
		t.Fatalf("invalid template state: %+v", card)
	}
}

func TestCardMutationTransactions(t *testing.T) {
	f := setup(t)
	id := f.create(t, "card_json", testCardJSON)
	base, stream := "/"+id, "/"+id+"/elements/a/content"
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"PUT", stream, `{"sequence":0,"content":"zero"}`, 300317},
		{"PUT", stream, `{"sequence":-1,"content":"negative"}`, 300317},
		{"PUT", stream, `{"sequence":2147483648,"content":"overflow"}`, 99992402},
		{"PUT", stream, `{"sequence":1.5,"content":"float"}`, 99992402},
		{"PUT", stream, `{"sequence":2,"content":"stream 2","uuid":"retry"}`, 0},
		{"PUT", stream, `{"sequence":2,"content":"changed","uuid":"retry"}`, 0},
		{"PUT", stream, `{"sequence":2,"content":"equal"}`, 300317},
		{"PUT", stream, `{"sequence":1,"content":"lower"}`, 300317},
		{"PUT", base + "/elements/hr/content", `{"sequence":3,"content":"not text"}`, 300310},
		{"PUT", base + "/elements/missing/content", `{"sequence":3,"content":"missing"}`, 300313},
		{"PATCH", base + "/settings", `{"sequence":3,"settings":{"config":{"streaming_mode":false}}}`, 99992402},
		{"PATCH", base + "/settings", `{"sequence":3,"settings":"{\"config\":{\"summary\":false}}"}`, 99992402},
		{"PATCH", base + "/settings", `{"sequence":3,"settings":"{\"config\":{\"streaming_mode\":false}}","uuid":"retry"}`, 0},
		{"PUT", stream, `{"sequence":4,"content":"blocked"}`, 300309},
	} {
		f.call(t, "t-owner", tc.method, tc.path, tc.body, tc.code)
	}
	f.call(t, "t-other", "PUT", stream, `{"sequence":2,"content":"foreign","uuid":"retry"}`, 300311)
	f.call(t, "t-owner", "PUT", "/123/elements/a/content", `{"sequence":2,"content":"missing","uuid":"retry"}`, 200740)
	card, _ := f.state.Card(id)
	if card.Sequence != 3 || card.StreamingMode || card.Elements["a"] != "stream 2" || !strings.Contains(string(card.JSON), `"summary":{"content":"initial"}`) {
		t.Fatalf("unexpected state: %+v", card)
	}
	f.elapsed.Store(int64(time.Hour))
	f.call(t, "t-owner", "PUT", stream, `{"sequence":2,"content":"expired","uuid":"retry"}`, 300317)
	f.call(t, "t-owner", "PATCH", base+"/settings", `{"sequence":4,"settings":"{\"config\":{\"streaming_mode\":true}}"}`, 0)
	f.call(t, "t-owner", "PUT", stream, `{"sequence":5,"content":"resumed"}`, 0)
}

func TestCardElementCRUDAndBatch(t *testing.T) {
	f := setup(t)
	id := f.create(t, "card_json", testCardJSON)
	base := "/" + id
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"POST", "/elements", `{"sequence":1,"type":"append","target_element_id":"container","elements":"[{\"tag\":\"plain_text\",\"element_id\":\"a\",\"content\":\"duplicate\"}]"}`, 300301},
		{"POST", "/elements", `{"sequence":1,"type":"insert_before","target_element_id":"a","elements":"[{\"tag\":\"plain_text\",\"element_id\":\"b\",\"content\":\"before\"}]"}`, 0},
		{"PUT", "/elements/b", `{"sequence":2,"element":"{\"tag\":\"markdown\",\"content\":\"replace\"}"}`, 0},
		{"PATCH", "/elements/b", `{"sequence":3,"partial_element":"{\"tag\":\"button\"}"}`, 300312},
		{"PATCH", "/elements/b", `{"sequence":3,"partial_element":"{\"content\":\"patch\",\"element_id\":\"patched\"}"}`, 0},
		{"DELETE", "/elements/hr", `{"sequence":4}`, 0},
		{"POST", "/batch_update", `{"sequence":5,"uuid":"batch","actions":"[{\"action\":\"partial_update_element\",\"params\":{\"element_id\":\"a\",\"partial_element\":{\"content\":\"rollback\"}}},{\"action\":\"delete_elements\",\"params\":{\"element_ids\":[\"patched\",\"missing\"]}}]"}`, 300314},
	} {
		f.call(t, "t-owner", tc.method, base+tc.path, tc.body, tc.code)
	}
	card, _ := f.state.Card(id)
	if card.Sequence != 4 || card.Elements["a"] != "start" || card.Elements["patched"] != "patch" || len(card.Elements) != 3 ||
		strings.Index(string(card.JSON), `"element_id":"patched"`) > strings.Index(string(card.JSON), `"element_id":"a"`) {
		t.Fatalf("unexpected state after rollback: %s, %+v", card.JSON, card.Elements)
	}
	f.call(t, "t-owner", "POST", base+"/batch_update", `{"sequence":5,"uuid":"batch","actions":"[{\"action\":\"partial_update_setting\",\"params\":{\"settings\":{\"config\":{\"streaming_mode\":false}}}},{\"action\":\"add_elements\",\"params\":{\"type\":\"append\",\"target_element_id\":\"container\",\"elements\":[{\"tag\":\"hr\",\"element_id\":\"rule\"}]}},{\"action\":\"update_element\",\"params\":{\"element_id\":\"a\",\"element\":{\"tag\":\"markdown\",\"content\":\"batch\"}}},{\"action\":\"delete_elements\",\"params\":{\"element_ids\":[\"patched\"]}}]"}`, 0)
	card, _ = f.state.Card(id)
	if card.Sequence != 5 || card.StreamingMode || card.Elements["a"] != "batch" || len(card.Elements) != 3 {
		t.Fatalf("invalid batch state: %+v", card)
	}
	f.call(t, "t-owner", "PUT", base, `{"sequence":6,"card":{"type":"card_json","data":"null"}}`, 99992402)
	f.call(t, "t-owner", "PUT", base, fmt.Sprintf(`{"sequence":6,"card":{"type":"card_json","data":%q}}`, testCardJSON), 0)
	card, _ = f.state.Card(id)
	if card.Sequence != 6 || !card.StreamingMode || card.Elements["a"] != "start" || len(card.Elements) != 3 {
		t.Fatalf("invalid full update state: %+v", card)
	}
}

func TestConcurrentCardSequences(t *testing.T) {
	f := setup(t)
	id := f.create(t, "card_json", testCardJSON)
	const count = 40
	var accepted atomic.Int32
	start, done := make(chan struct{}), make(chan struct{}, count)
	for seq := 1; seq <= count; seq++ {
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			body := fmt.Sprintf(`{"sequence":%d,"content":"seq %d"}`, seq, seq)
			r := httptest.NewRequest(http.MethodPut, cardsPath+"/"+id+"/elements/a/content", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer t-owner")
			w := httptest.NewRecorder()
			f.mux.ServeHTTP(w, r)
			var result response
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Error(err)
			} else if w.Code == http.StatusOK && result.Code == 0 {
				accepted.Add(1)
			} else if w.Code != http.StatusBadRequest || result.Code != 300317 {
				t.Errorf("unexpected concurrent response: %d %s", w.Code, w.Body)
			}
		}()
	}
	close(start)
	for range count {
		<-done
	}
	card, _ := f.state.Card(id)
	if accepted.Load() == 0 || card.Sequence != count || card.Elements["a"] != "seq 40" {
		t.Fatalf("invalid concurrent state: %+v", card)
	}
}
