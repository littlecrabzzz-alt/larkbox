package store

import (
	"encoding/json"
	"slices"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// Base identifies a Base resource and its default table.
type Base struct {
	Owner                   ResourceOwner
	ID, Name, Zone, TableID string
	FieldID                 string
	records                 []BaseRecord
}

// BaseRecord is one row returned by Base v3 and Bitable v1.
type BaseRecord struct {
	ID     string                     `json:"record_id"`
	Fields map[string]json.RawMessage `json:"fields"`
}

// CreateBase saves a base with one empty table and a text field called Name.
func (s *Store) CreateBase(owner ResourceOwner, name, zone string) Base {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := Base{Owner: owner, ID: protocol.NewID("bas"), Name: name, Zone: zone,
		TableID: protocol.NewID("tbl")[:18], FieldID: protocol.NewID("fld")[:18]}
	s.content.bases[base.ID] = base
	return base
}

// Base returns resource metadata visible to the owning caller.
func (s *Store) Base(owner ResourceOwner, id string) (Base, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	base, ok := s.content.bases[id]
	if !ok || base.Owner != owner {
		return Base{}, false
	}
	base.records = nil
	return base, true
}

// CreateBaseRecords commits a batch of rows to a visible table.
func (s *Store) CreateBaseRecords(owner ResourceOwner, id, tableID string, rows []map[string]json.RawMessage) ([]BaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base, ok := s.content.bases[id]
	if !ok || base.Owner != owner || base.TableID != tableID {
		return nil, ErrNotFound
	}
	result := make([]BaseRecord, 0, len(rows))
	for _, row := range rows {
		record := BaseRecord{ID: protocol.NewID("rec")[:18], Fields: cloneFields(row)}
		base.records = append(base.records, record)
		result = append(result, BaseRecord{ID: record.ID, Fields: cloneFields(record.Fields)})
	}
	s.content.bases[id] = base
	return result, nil
}

// UpdateBaseRecord changes fields on an existing row.
func (s *Store) UpdateBaseRecord(owner ResourceOwner, id, tableID, recordID string, fields map[string]json.RawMessage) (BaseRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base, ok := s.content.bases[id]
	if !ok || base.Owner != owner || base.TableID != tableID {
		return BaseRecord{}, ErrNotFound
	}
	for _, record := range base.records {
		if record.ID == recordID {
			for name, field := range fields {
				record.Fields[name] = slices.Clone(field)
			}
			return BaseRecord{ID: record.ID, Fields: cloneFields(record.Fields)}, nil
		}
	}
	return BaseRecord{}, ErrNotFound
}

// BaseRecords returns a stable copy of the table's records in creation order.
func (s *Store) BaseRecords(owner ResourceOwner, id, tableID string) ([]BaseRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	base, ok := s.content.bases[id]
	if !ok || base.Owner != owner || base.TableID != tableID {
		return nil, false
	}
	result := make([]BaseRecord, 0, len(base.records))
	for _, record := range base.records {
		result = append(result, BaseRecord{ID: record.ID, Fields: cloneFields(record.Fields)})
	}
	return result, true
}

func cloneFields(fields map[string]json.RawMessage) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(fields))
	for name, data := range fields {
		cloned[name] = slices.Clone(data)
	}
	return cloned
}
