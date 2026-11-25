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

// SheetRead is a range result captured at one workbook revision.
type SheetRead struct {
	Ranges    [][][]json.RawMessage
	Revision  int64
	Truncated bool
}

// ReadSheetRanges captures rows at one revision. A positive budget limits
// the encoded cell and value payload before cells are copied out of the store.
func (s *Store) ReadSheetRanges(owner ResourceOwner, id, selector string, ranges []SheetRange, budget int) (SheetRead, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	book, ok := s.content.sheets[id]
	if !ok || book.Owner != owner || !book.matchesSelector(selector) {
		return SheetRead{}, ErrNotFound
	}
	result := SheetRead{Ranges: make([][][]json.RawMessage, 0, len(ranges)), Revision: book.Revision}
	limited := budget > 0
	for _, sel := range ranges {
		const rangeOverhead = 128
		if limited && budget < rangeOverhead {
			result.Truncated = true
			return result, nil
		}
		budget -= rangeOverhead
		rows := make([][]json.RawMessage, 0, sel.Rows)
		for row := 0; row < sel.Rows; row++ {
			if limited {
				// A cell can appear in the raw cell matrix and in its value projection.
				cost := 6
				for column := 0; column < sel.Columns && cost <= budget; column++ {
					cost += 2*max(4, len(book.cells[[2]int{sel.Row + row, sel.Column + column}])) + 2
				}
				if cost > budget {
					result.Ranges = append(result.Ranges, rows)
					result.Truncated = true
					return result, nil
				}
				budget -= cost
			}
			line := make([]json.RawMessage, sel.Columns)
			for column := range line {
				line[column] = slices.Clone(book.cells[[2]int{sel.Row + row, sel.Column + column}])
			}
			rows = append(rows, line)
		}
		result.Ranges = append(result.Ranges, rows)
	}
	return result, nil
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
