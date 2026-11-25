package store

import (
	"encoding/json"
	"slices"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
)

// Spreadsheet identifies a workbook and its default sheet.
type Spreadsheet struct {
	Owner              ResourceOwner
	ID, Title, SheetID string
	Revision           int64
	cells              map[[2]int]json.RawMessage
}

// DefaultSheet is the title of the first sheet in each sandbox workbook.
const DefaultSheet = "Sheet1"

// SheetRange gives a zero-based start and the rectangular dimensions.
type SheetRange struct {
	Row, Column   int
	Rows, Columns int
}

// CreateSpreadsheet gives the new workbook one empty default sheet.
func (s *Store) CreateSpreadsheet(owner ResourceOwner, title string) Spreadsheet {
	s.mu.Lock()
	defer s.mu.Unlock()
	book := Spreadsheet{Owner: owner, ID: protocol.NewID("sht"), Title: title,
		SheetID: protocol.NewID("")[:8], Revision: 1, cells: make(map[[2]int]json.RawMessage)}
	s.content.sheets[book.ID] = book
	book.cells = nil
	return book
}

// Spreadsheet returns workbook metadata when the caller owns it.
func (s *Store) Spreadsheet(owner ResourceOwner, id string) (Spreadsheet, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	book, ok := s.content.sheets[id]
	if !ok || book.Owner != owner {
		return Spreadsheet{}, false
	}
	book.cells = nil
	return book, true
}

// ReadSheetRanges captures all selected cells at one workbook revision.
func (s *Store) ReadSheetRanges(owner ResourceOwner, id, selector string, ranges []SheetRange) ([][][]json.RawMessage, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	book, ok := s.content.sheets[id]
	if !ok || book.Owner != owner || !book.matchesSelector(selector) {
		return nil, 0, ErrNotFound
	}
	result := make([][][]json.RawMessage, 0, len(ranges))
	for _, sel := range ranges {
		cells := make([][]json.RawMessage, sel.Rows)
		for row := range cells {
			cells[row] = make([]json.RawMessage, sel.Columns)
			for column := range cells[row] {
				cells[row][column] = slices.Clone(book.cells[[2]int{sel.Row + row, sel.Column + column}])
			}
		}
		result = append(result, cells)
	}
	return result, book.Revision, nil
}

// WriteSheetRange changes the full rectangle in one commit.
func (s *Store) WriteSheetRange(owner ResourceOwner, id, selector string, sel SheetRange, cells [][]json.RawMessage, overwrite bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	book, ok := s.content.sheets[id]
	if !ok || book.Owner != owner || !book.matchesSelector(selector) {
		return 0, ErrNotFound
	}
	if !overwrite {
		for row, line := range cells {
			for column := range line {
				if _, exists := book.cells[[2]int{sel.Row + row, sel.Column + column}]; exists {
					return 0, ErrAlreadyExists
				}
			}
		}
	}
	for row, line := range cells {
		for column, cell := range line {
			address := [2]int{sel.Row + row, sel.Column + column}
			if len(cell) == 0 || string(cell) == "null" {
				delete(book.cells, address)
			} else {
				book.cells[address] = slices.Clone(cell)
			}
		}
	}
	book.Revision++
	s.content.sheets[id] = book
	return book.Revision, nil
}

func (book Spreadsheet) matchesSelector(selector string) bool {
	return selector == "" || selector == book.SheetID || selector == DefaultSheet
}
