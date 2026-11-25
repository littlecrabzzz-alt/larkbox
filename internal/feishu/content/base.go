package content

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

func bitableAPI(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/open-apis/bitable/")
}

func (h handler) createBase(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name   string `json:"name"`
		Zone   string `json:"time_zone"`
		Folder string `json:"folder_token"`
	}
	if !read(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) == "" {
		fail(w, protocol.InvalidParameter)
		return
	}
	if input.Folder != "" && input.Folder != store.DriveRoot {
		fail(w, protocol.ResourceNotFound)
		return
	}
	base, err := store.Commit(r.Context(), h.state, func() (store.Base, error) {
		return h.state.CreateBase(h.owner(r), input.Name, cmp.Or(input.Zone, "Asia/Shanghai")), nil
	})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, baseData(r, base))
}

func baseData(r *http.Request, base store.Base) fields {
	if bitableAPI(r) {
		return fields{"app": fields{"app_token": base.ID, "name": base.Name, "time_zone": base.Zone, "default_table_id": base.TableID}}
	}
	return fields{"base_token": base.ID, "name": base.Name, "time_zone": base.Zone, "default_table_id": base.TableID}
}

func (h handler) base(w http.ResponseWriter, r *http.Request) (store.Base, bool) {
	base, ok := h.state.Base(h.owner(r), r.PathValue("base_token"))
	if tableID := r.PathValue("table_id"); tableID != "" && base.TableID != tableID {
		ok = false
	}
	if !ok {
		fail(w, protocol.ResourceNotFound)
	}
	return base, ok
}

func (h handler) readBase(w http.ResponseWriter, r *http.Request) {
	if base, ok := h.base(w, r); ok {
		reply(w, baseData(r, base))
	}
}

func (h handler) listBaseTables(w http.ResponseWriter, r *http.Request) {
	if base, ok := h.base(w, r); ok {
		entry := fields{"table_id": base.TableID, "name": "Table1", "revision": 1}
		reply(w, fields{"items": []fields{entry}, "total": 1, "has_more": false, "page_token": ""})
	}
}

func (h handler) listBaseFields(w http.ResponseWriter, r *http.Request) {
	if base, ok := h.base(w, r); ok {
		kind := any("text")
		key := "fields"
		if bitableAPI(r) {
			kind, key = 1, "items"
		}
		entry := fields{"field_id": base.FieldID, "field_name": "Name", "name": "Name", "type": kind, "is_primary": true}
		reply(w, fields{key: []fields{entry}, "total": 1, "has_more": false, "page_token": ""})
	}
}

func recordFields(input map[string]json.RawMessage, base store.Base) (map[string]json.RawMessage, bool) {
	if len(input) != 1 {
		return nil, false
	}
	value, exists := input["Name"]
	if !exists {
		value, exists = input[base.FieldID]
	}
	var text string
	if !exists || json.Unmarshal(value, &text) != nil {
		return nil, false
	}
	return map[string]json.RawMessage{"Name": value}, true
}

func (h handler) createBaseRecord(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if !read(w, r, &input) {
		return
	}
	records, ok := h.createRecords(w, r, []map[string]json.RawMessage{input.Fields})
	if ok {
		replyRecord(w, r, records[0])
	}
}

func (h handler) batchCreateBaseRecords(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Create  []map[string]json.RawMessage `json:"create_records"`
		Records []struct {
			Fields map[string]json.RawMessage `json:"fields"`
		} `json:"records"`
	}
	if !read(w, r, &input) {
		return
	}
	if bitableAPI(r) {
		if input.Create != nil {
			fail(w, protocol.InvalidParameter)
			return
		}
		for _, record := range input.Records {
			input.Create = append(input.Create, record.Fields)
		}
	} else if input.Records != nil {
		fail(w, protocol.InvalidParameter)
		return
	}
	if records, ok := h.createRecords(w, r, input.Create); ok {
		reply(w, fields{"records": records})
	}
}

func (h handler) createRecords(w http.ResponseWriter, r *http.Request, rows []map[string]json.RawMessage) ([]store.BaseRecord, bool) {
	base, ok := h.base(w, r)
	if !ok {
		return nil, false
	}
	if len(rows) == 0 || len(rows) > 200 {
		fail(w, protocol.InvalidParameter)
		return nil, false
	}
	for i, row := range rows {
		normalized, valid := recordFields(row, base)
		if !valid {
			fail(w, protocol.InvalidParameter)
			return nil, false
		}
		rows[i] = normalized
	}
	records, err := store.Commit(r.Context(), h.state, func() ([]store.BaseRecord, error) {
		return h.state.CreateBaseRecords(h.owner(r), base.ID, base.TableID, rows)
	})
	if err != nil {
		stateError(w, err)
		return nil, false
	}
	return records, true
}

func (h handler) updateBaseRecord(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if !read(w, r, &input) {
		return
	}
	base, ok := h.base(w, r)
	if !ok {
		return
	}
	normalized, valid := recordFields(input.Fields, base)
	if !valid {
		fail(w, protocol.InvalidParameter)
		return
	}
	changed, err := store.Commit(r.Context(), h.state, func() (store.BaseRecord, error) {
		return h.state.UpdateBaseRecord(h.owner(r), base.ID, base.TableID, r.PathValue("record_id"), normalized)
	})
	if err != nil {
		stateError(w, err)
		return
	}
	replyRecord(w, r, changed)
}

func replyRecord(w http.ResponseWriter, r *http.Request, record store.BaseRecord) {
	if bitableAPI(r) {
		reply(w, fields{"record": record})
		return
	}
	reply(w, record)
}

type recordQuery struct {
	Keyword string   `json:"keyword"`
	Offset  int      `json:"offset"`
	Limit   int      `json:"limit"`
	Search  []string `json:"search_fields"`
	Select  []string `json:"select_fields"`
}

func (h handler) listBaseRecords(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	offset, offsetErr := strconv.Atoi(cmp.Or(query.Get("offset"), query.Get("page_token"), "0"))
	limit, limitErr := strconv.Atoi(cmp.Or(query.Get("limit"), query.Get("page_size"), "20"))
	if offsetErr != nil || limitErr != nil {
		fail(w, protocol.InvalidParameter)
		return
	}
	h.queryBaseRecords(w, r, recordQuery{Offset: offset, Limit: limit})
}

func (h handler) searchBaseRecords(w http.ResponseWriter, r *http.Request) {
	var input recordQuery
	if read(w, r, &input) {
		h.queryBaseRecords(w, r, input)
	}
}

func (h handler) queryBaseRecords(w http.ResponseWriter, r *http.Request, query recordQuery) {
	base, ok := h.base(w, r)
	if !ok {
		return
	}
	if query.Limit == 0 {
		query.Limit = 10
	}
	maximum := 2000
	if bitableAPI(r) {
		maximum = 500
	}
	valid := func(field string) bool { return field == "Name" || field == base.FieldID }
	if query.Offset < 0 || query.Limit < 1 || query.Limit > maximum ||
		slices.ContainsFunc(query.Select, func(field string) bool { return !valid(field) }) ||
		slices.ContainsFunc(query.Search, func(field string) bool { return !valid(field) }) {
		fail(w, protocol.InvalidParameter)
		return
	}
	records, ok := h.state.BaseRecords(h.owner(r), base.ID, base.TableID)
	if !ok {
		fail(w, protocol.ResourceNotFound)
		return
	}
	keyword := strings.ToLower(strings.TrimSpace(query.Keyword))
	if keyword != "" {
		records = slices.DeleteFunc(records, func(record store.BaseRecord) bool {
			var text string
			_ = json.Unmarshal(record.Fields["Name"], &text)
			return !strings.Contains(strings.ToLower(text), keyword)
		})
	}
	start := min(query.Offset, len(records))
	end := min(start+query.Limit, len(records))
	page := records[start:end]
	if !bitableAPI(r) {
		ids, values := make([]string, 0, len(page)), make([][]json.RawMessage, 0, len(page))
		for _, row := range page {
			ids = append(ids, row.ID)
			values = append(values, []json.RawMessage{row.Fields["Name"]})
		}
		reply(w, fields{
			"timezone": base.Zone, "fields": []string{"Name"}, "field_id_list": []string{base.FieldID}, "field_type_list": []string{"text"},
			"record_id_list": ids, "data": values, "total": len(records), "has_more": end < len(records),
		})
		return
	}
	next := ""
	if end < len(records) {
		next = strconv.Itoa(end)
	}
	reply(w, fields{"items": page, "total": len(records), "has_more": next != "", "page_token": next})
}
