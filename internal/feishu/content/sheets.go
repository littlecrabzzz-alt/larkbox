package content

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/littlecrabzzz-alt/larkbox/internal/protocol"
	"github.com/littlecrabzzz-alt/larkbox/internal/store"
)

const maxRequestedCells = 20000

var a1Cell = regexp.MustCompile(`^\$?([A-Za-z]{1,3})\$?([1-9][0-9]{0,6})$`)

func cellPosition(cell string) (int, int, bool) {
	match := a1Cell.FindStringSubmatch(cell)
	if match == nil {
		return 0, 0, false
	}
	row, _ := strconv.Atoi(match[2])
	column := 0
	for _, letter := range strings.ToUpper(match[1]) {
		column = column*26 + int(letter-'A'+1)
	}
	return row - 1, column - 1, row <= 1048576 && column <= 16384
}

func parseSheetRange(raw string) (string, store.SheetRange, bool) {
	selector, area, scoped := strings.Cut(raw, "!")
	if !scoped {
		selector, area = "", raw
	} else if selector == "" {
		return "", store.SheetRange{}, false
	}
	first, last, ranged := strings.Cut(area, ":")
	if ranged && last == "" {
		return "", store.SheetRange{}, false
	}
	row, column, startOK := cellPosition(first)
	endRow, endColumn, endOK := cellPosition(cmp.Or(last, first))
	height, width := endRow-row+1, endColumn-column+1
	if !startOK || !endOK || height <= 0 || width <= 0 || width > maxRequestedCells/height {
		return "", store.SheetRange{}, false
	}
	return strings.Trim(selector, "'"), store.SheetRange{Row: row, Column: column, Rows: height, Columns: width}, true
}

func spreadsheetData(r *http.Request, book store.Spreadsheet) fields {
	return fields{"spreadsheet_token": book.ID, "title": book.Title,
		"url": resourceURL(r, "/open-apis/sheets/v3/spreadsheets/"+book.ID)}
}

func (h handler) createSpreadsheet(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title  string `json:"title"`
		Folder string `json:"folder_token"`
	}
	if !read(w, r, &input) {
		return
	}
	if input.Folder != "" && input.Folder != store.DriveRoot {
		fail(w, protocol.ResourceNotFound)
		return
	}
	book, err := store.Commit(r.Context(), h.state, func() (store.Spreadsheet, error) {
		return h.state.CreateSpreadsheet(h.owner(r), cmp.Or(strings.TrimSpace(input.Title), "Untitled spreadsheet")), nil
	})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, fields{"spreadsheet": spreadsheetData(r, book)})
}

func (h handler) spreadsheet(w http.ResponseWriter, r *http.Request) (store.Spreadsheet, bool) {
	book, ok := h.state.Spreadsheet(h.owner(r), r.PathValue("spreadsheet_token"))
	if !ok {
		fail(w, protocol.ResourceNotFound)
	}
	return book, ok
}

func (h handler) readSpreadsheet(w http.ResponseWriter, r *http.Request) {
	if book, ok := h.spreadsheet(w, r); ok {
		reply(w, fields{"spreadsheet": spreadsheetData(r, book)})
	}
}

func (h handler) querySheets(w http.ResponseWriter, r *http.Request) {
	if book, ok := h.spreadsheet(w, r); ok {
		reply(w, fields{"sheets": []fields{{"sheet_id": book.SheetID, "title": store.DefaultSheet, "index": 0, "hidden": false,
			"grid_properties": fields{"row_count": 200, "column_count": 20, "frozen_row_count": 0, "frozen_column_count": 0}}}})
	}
}

type sheetToolInput struct {
	Workbook  string              `json:"excel_id"`
	SheetID   string              `json:"sheet_id"`
	SheetName string              `json:"sheet_name"`
	Range     string              `json:"range"`
	Ranges    []string            `json:"ranges"`
	Cells     [][]json.RawMessage `json:"cells"`
	Overwrite *bool               `json:"allow_overwrite"`
}

func readSheetToolInput(w http.ResponseWriter, r *http.Request, expected string) (sheetToolInput, bool) {
	var request struct {
		Tool  string `json:"tool_name"`
		Input string `json:"input"`
	}
	if !read(w, r, &request) {
		return sheetToolInput{}, false
	}
	var input sheetToolInput
	if request.Tool != expected || json.Unmarshal([]byte(request.Input), &input) != nil ||
		input.Workbook != r.PathValue("spreadsheet_token") || (input.SheetID != "" && input.SheetName != "") {
		fail(w, protocol.InvalidParameter)
		return sheetToolInput{}, false
	}
	return input, true
}

func toolOutput(w http.ResponseWriter, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		fail(w, protocol.InvalidParameter)
		return
	}
	reply(w, fields{"output": string(raw)})
}

func (h handler) writeSheetTool(w http.ResponseWriter, r *http.Request) {
	input, ok := readSheetToolInput(w, r, "set_cell_range")
	if !ok {
		return
	}
	book, ok := h.spreadsheet(w, r)
	if !ok {
		return
	}
	selected, area, ok := parseSheetRange(input.Range)
	if !ok || !matchesSheet(book, selected, input.SheetID, input.SheetName) || !cellMatrixFits(area, input.Cells) {
		fail(w, protocol.InvalidParameter)
		return
	}
	revision, ok := h.writeCells(w, r, book, area, input.Cells, input.Overwrite == nil || *input.Overwrite)
	if ok {
		toolOutput(w, fields{"sheet_id": book.SheetID, "updated_range": input.Range, "updated_cells": area.Rows * area.Columns, "revision": revision})
	}
}

func (h handler) readSheetTool(w http.ResponseWriter, r *http.Request) {
	input, ok := readSheetToolInput(w, r, "get_cell_ranges")
	if !ok {
		return
	}
	book, ok := h.spreadsheet(w, r)
	if !ok {
		return
	}
	ranges := make([]store.SheetRange, 0, len(input.Ranges))
	total := 0
	for _, text := range input.Ranges {
		selector, area, valid := parseSheetRange(text)
		if !valid || !matchesSheet(book, selector, input.SheetID, input.SheetName) {
			fail(w, protocol.InvalidParameter)
			return
		}
		total += area.Rows * area.Columns
		if total > maxRequestedCells {
			fail(w, protocol.InvalidParameter)
			return
		}
		ranges = append(ranges, area)
	}
	if len(ranges) == 0 {
		fail(w, protocol.InvalidParameter)
		return
	}
	cells, revision, err := h.state.ReadSheetRanges(h.owner(r), book.ID, book.SheetID, ranges)
	if err != nil {
		stateError(w, err)
		return
	}
	result := make([]fields, 0, len(cells))
	for i, matrix := range cells {
		result = append(result, fields{"range": input.Ranges[i], "cells": matrix, "values": sheetValues(matrix)})
	}
	toolOutput(w, fields{"sheet_id": book.SheetID, "sheet_name": store.DefaultSheet, "revision": revision, "ranges": result})
}

func matchesSheet(book store.Spreadsheet, selectors ...string) bool {
	for _, selection := range selectors {
		if selection != "" && selection != book.SheetID && selection != store.DefaultSheet {
			return false
		}
	}
	return true
}

func cellMatrixFits(area store.SheetRange, cells [][]json.RawMessage) bool {
	if len(cells) != area.Rows {
		return false
	}
	for _, row := range cells {
		if len(row) != area.Columns {
			return false
		}
	}
	return true
}

func sheetValues(cells [][]json.RawMessage) [][]json.RawMessage {
	result := make([][]json.RawMessage, len(cells))
	for row, line := range cells {
		result[row] = make([]json.RawMessage, len(line))
		for column, cell := range line {
			var data struct {
				Value json.RawMessage `json:"value"`
			}
			_ = json.Unmarshal(cell, &data)
			result[row][column] = data.Value
		}
	}
	return result
}

func (h handler) writeCells(w http.ResponseWriter, r *http.Request, book store.Spreadsheet, area store.SheetRange, cells [][]json.RawMessage, overwrite bool) (int64, bool) {
	revision, err := store.Commit(r.Context(), h.state, func() (int64, error) {
		return h.state.WriteSheetRange(h.owner(r), book.ID, book.SheetID, area, cells, overwrite)
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		_ = protocol.WriteError(w, protocol.APIError{Code: protocol.InvalidParameter, Message: "target range contains data", Status: http.StatusBadRequest})
		return 0, false
	}
	if err != nil {
		stateError(w, err)
		return 0, false
	}
	return revision, true
}

func (h handler) readSheetValues(w http.ResponseWriter, r *http.Request) {
	text := r.PathValue("range")
	selector, area, valid := parseSheetRange(text)
	if !valid {
		fail(w, protocol.InvalidParameter)
		return
	}
	id := r.PathValue("spreadsheet_token")
	items, revision, err := h.state.ReadSheetRanges(h.owner(r), id, selector, []store.SheetRange{area})
	if err != nil {
		stateError(w, err)
		return
	}
	reply(w, fields{"spreadsheetToken": id, "revision": revision,
		"valueRange": fields{"range": text, "majorDimension": "ROWS", "values": sheetValues(items[0])}})
}

func (h handler) writeSheetValues(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Range struct {
			Address string              `json:"range"`
			Values  [][]json.RawMessage `json:"values"`
		} `json:"valueRange"`
	}
	if !read(w, r, &input) {
		return
	}
	book, ok := h.spreadsheet(w, r)
	if !ok {
		return
	}
	selector, area, valid := parseSheetRange(input.Range.Address)
	values := input.Range.Values
	if !valid || !matchesSheet(book, selector) || !cellMatrixFits(area, values) {
		fail(w, protocol.InvalidParameter)
		return
	}
	for row, line := range values {
		for column, value := range line {
			values[row][column], _ = json.Marshal(fields{"value": value})
		}
	}
	revision, written := h.writeCells(w, r, book, area, values, true)
	if written {
		reply(w, fields{"spreadsheetToken": book.ID, "revision": revision, "updatedRange": input.Range.Address,
			"updatedRows": area.Rows, "updatedColumns": area.Columns, "updatedCells": area.Rows * area.Columns})
	}
}
