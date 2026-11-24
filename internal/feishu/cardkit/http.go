package cardkit

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
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
		card, err := parseEntity(input.Type, input.Data)
		var cardErr protocol.APIError
		if errors.As(err, &cardErr) && cardErr.Code != protocol.FieldValidation {
			err = invalid("data", cardErr.Message)
		}
		if err != nil {
			writeError(w, err)
			return
		}
		card.AppID = identity.AppID
		for {
			card.ID = protocol.NewID(protocol.CardID)
			err = state.CreateCard(card)
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
			if utf8.RuneCountInString(cardID) > 20 || utf8.RuneCountInString(elementID) > 20 {
				writeError(w, invalid("path", "Expected card and element IDs of at most 20 characters."))
				return
			}
			identity, _ := middleware.AuthFromContext(r.Context())
			err := state.ChangeCard(identity.AppID, cardID, string(route.action), uuid, req.Sequence, func(card *store.Card) error {
				if req.Sequence > math.MaxInt32 {
					return invalid("sequence", "Expected a positive 32-bit integer.")
				}
				return req.apply(card, route.action, elementID)
			})
			if err != nil {
				writeError(w, err)
				return
			}
			reply(w, struct{}{})
		})))
	}
}

func (req mutation) apply(card *store.Card, action operation, id string) error {
	var next store.Card
	var err error
	if action == fullUpdate {
		next, err = parseEntity(req.Card.Type, req.Card.Data)
	} else {
		next, err = req.edit(card, action, id)
	}
	if err != nil {
		return err
	}
	card.Type, card.JSON, card.Elements, card.StreamingMode = next.Type, next.JSON, next.Elements, next.StreamingMode
	return nil
}

func (req mutation) edit(card *store.Card, action operation, id string) (store.Card, error) {
	doc := object(string(card.JSON))
	if doc == nil || (action != updateSettings && action != batch && doc["schema"] != "2.0") {
		return store.Card{}, errUnsupportedSchema
	}
	var err error
	switch action {
	case updateSettings:
		err = changeSettings(doc, object(req.Settings))
	case streamContent:
		err = streamElement(doc, id, req.Content)
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
		err = applyBatch(card.Type, doc, req.Actions)
	}
	if err != nil {
		return store.Card{}, err
	}
	return saveEntity(card.Type, doc)
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
	var apiErr protocol.APIError
	switch {
	case errors.As(err, &apiErr):
	case errors.Is(err, store.ErrNotFound):
		apiErr = cardError(200740, "The card entity does not exist.")
	case errors.Is(err, store.ErrCardOwner):
		apiErr = errCardOwner
	case errors.Is(err, store.ErrStaleSequence):
		apiErr = errSequence
	default:
		apiErr = protocol.NewError(300120, nil)
	}
	_ = protocol.WriteError(w, apiErr)
}
