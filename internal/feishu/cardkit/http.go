package cardkit

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/littlecrabzzz-alt/larkbox/internal/feishu/middleware"
	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

type cardData struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type mutation struct {
	Sequence        int64    `json:"sequence"`
	UUID            *string  `json:"uuid"`
	Card            cardData `json:"card"`
	Content         *string  `json:"content"`
	Settings        string   `json:"settings"`
	Type            string   `json:"type"`
	TargetElementID string   `json:"target_element_id"`
	Elements        string   `json:"elements"`
	Element         string   `json:"element"`
	PartialElement  string   `json:"partial_element"`
	Actions         string   `json:"actions"`
}

type operation string

const (
	fullUpdate     operation = "update_card"
	updateSettings operation = "settings"
	streamContent  operation = "content"
	add            operation = "add_element"
	replace        operation = "replace_element"
	patch          operation = "patch_element"
	remove         operation = "delete_element"
	batch          operation = "batch_update"
)

// RegisterRoutes attaches tenant-authenticated CardKit v1 endpoints.
func RegisterRoutes(mux *http.ServeMux, state *store.Store) {
	bearer := middleware.Bearer(state, middleware.Tenant, true)
	const base = "/open-apis/cardkit/v1/cards"
	mux.Handle("POST "+base, bearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input cardData
		if !readRequest(w, r, &input) {
			return
		}
		identity, _ := middleware.AuthFromContext(r.Context())
		card, err := instantiateCard(state, input.Type, input.Data)
		if err != nil {
			writeError(w, err)
			return
		}
		card.AppID = identity.AppID
		for {
			card.ID = protocol.NewID(protocol.CardID)
			err = state.Mutate(r.Context(), func() error { return state.CreateCard(card) })
			if !errors.Is(err, store.ErrAlreadyExists) {
				break
			}
		}
		if err != nil {
			writeError(w, err)
			return
		}
		reply(w, map[string]string{"card_id": card.ID})
	})))
	for _, route := range []struct {
		method, path string
		action       operation
	}{
		{"PUT", "/{card_id}", fullUpdate},
		{"PATCH", "/{card_id}/settings", updateSettings},
		{"PUT", "/{card_id}/elements/{element_id}/content", streamContent},
		{"POST", "/{card_id}/elements", add},
		{"PUT", "/{card_id}/elements/{element_id}", replace},
		{"PATCH", "/{card_id}/elements/{element_id}", patch},
		{"DELETE", "/{card_id}/elements/{element_id}", remove},
		{"POST", "/{card_id}/batch_update", batch},
	} {
		mux.Handle(route.method+" "+base+route.path, bearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req mutation
			if !readRequest(w, r, &req) {
				return
			}
			var uuid string
			if req.UUID != nil {
				uuid = *req.UUID
				if uuid == "" || utf8.RuneCountInString(uuid) > 64 {
					writeError(w, invalid("uuid", "Expected 1 to 64 characters."))
					return
				}
			}
			cardID, elementID := r.PathValue("card_id"), r.PathValue("element_id")
			if utf8.RuneCountInString(cardID) > 20 {
				writeError(w, invalid("card_id", "Expected a card ID of at most 20 characters."))
				return
			}
			if elementID != "" && !elementIDPattern.MatchString(elementID) {
				writeError(w, invalid("element_id", "Invalid element ID."))
				return
			}
			identity, _ := middleware.AuthFromContext(r.Context())
			mutation := store.CardMutation{Operation: string(route.action), UUID: uuid, Sequence: req.Sequence}
			if uuid != "" {
				mutation.Fingerprint = req.fingerprint(route.action, cardID, elementID)
			}
			err := state.Mutate(r.Context(), func() error {
				return state.ChangeCard(identity.AppID, cardID, mutation, func(card *store.Card, now time.Time) error {
					if req.Sequence > math.MaxInt32 {
						return invalid("sequence", "Expected a positive 32-bit integer.")
					}
					return req.apply(card, route.action, elementID, now)
				})
			})
			if err != nil {
				writeError(w, err)
				return
			}
			reply(w, struct{}{})
		})))
	}
}

func (req mutation) fingerprint(action operation, cardID, elementID string) [32]byte {
	var payload any
	switch action {
	case fullUpdate:
		payload = []any{req.Card.Type, canonicalJSON(req.Card.Data)}
	case updateSettings:
		payload = canonicalJSON(req.Settings)
	case streamContent:
		payload = req.Content
	case add:
		payload = []any{req.Type, req.TargetElementID, canonicalJSON(req.Elements)}
	case replace:
		payload = canonicalJSON(req.Element)
	case patch:
		payload = canonicalJSON(req.PartialElement)
	case batch:
		payload = canonicalJSON(req.Actions)
	}
	data, _ := json.Marshal([]any{cardID, elementID, action, req.Sequence, payload})
	return sha256.Sum256(data)
}

func canonicalJSON(encoded string) any {
	var value any
	if decode(encoded, &value) {
		return []any{true, value}
	}
	// Keep a malformed string distinct from valid JSON containing that string.
	return []any{false, encoded}
}

func (req mutation) apply(card *store.Card, action operation, id string, now time.Time) error {
	var next store.Card
	var err error
	if action == fullUpdate {
		if req.Card.Type != kindJSON {
			return invalid("card.type", "Expected card_json for full card updates.")
		}
		next, err = parseEntity(req.Card.Type, req.Card.Data)
	} else {
		next, err = req.edit(card, action, id, now)
	}
	if err != nil {
		return err
	}
	card.Type, card.JSON, card.Elements, card.StreamingMode = next.Type, next.JSON, next.Elements, next.StreamingMode
	card.TemplateData, card.StreamingSince = next.TemplateData, next.StreamingSince
	return nil
}

func (req mutation) edit(card *store.Card, action operation, id string, now time.Time) (store.Card, error) {
	doc := object(string(card.JSON))
	if doc == nil || (action != updateSettings && action != batch && doc["schema"] != "2.0") {
		return store.Card{}, errUnsupportedSchema
	}
	var err error
	var restart bool
	switch action {
	case updateSettings:
		err = changeSettings(doc, object(req.Settings), &restart)
	case streamContent:
		err = streamElement(card, doc, id, req.Content, now)
	case add:
		var elements []any
		if !decode(req.Elements, &elements) {
			return store.Card{}, invalid("elements", "Expected a JSON-encoded array.")
		}
		err = addElements(doc, req.Type, req.TargetElementID, elements)
	case replace:
		err = updateElement(doc, id, object(req.Element), false)
	case patch:
		err = updateElement(doc, id, object(req.PartialElement), true)
	case remove:
		err = deleteElement(doc, id)
	case batch:
		err = applyBatch(doc, req.Actions, &restart)
	}
	if err != nil {
		return store.Card{}, err
	}
	next, err := saveEntity(card.Type, doc)
	next.TemplateData = card.TemplateData
	if !restart {
		next.StreamingSince = card.StreamingSince
	}
	return next, err
}

func readRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil || len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, dst) != nil {
		writeError(w, invalid("body", "Expected a JSON object with valid field types."))
		return false
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		writeError(w, invalid("body", "Expected one JSON object."))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, data any) {
	_ = protocol.WriteJSON(w, http.StatusOK, protocol.Envelope{Code: protocol.Success, Msg: "success", Data: data})
}

func writeError(w http.ResponseWriter, err error) {
	if middleware.WriteResetConflict(w, err) {
		return
	}
	var apiErr protocol.APIError
	switch {
	case errors.As(err, &apiErr):
	case errors.Is(err, store.ErrNotFound):
		apiErr = cardError(200740, "The card entity does not exist.")
	case errors.Is(err, store.ErrCardExpired):
		apiErr = cardError(200750, "The card entity has expired.")
	case errors.Is(err, store.ErrCardUUIDConflict):
		apiErr = cardError(200770, "UUID conflict.")
	case errors.Is(err, store.ErrCardOwner):
		apiErr = errCardOwner
	case errors.Is(err, store.ErrStaleSequence):
		apiErr = errSequence
	default:
		apiErr = protocol.NewError(300120, nil)
	}
	_ = protocol.WriteError(w, apiErr)
}
