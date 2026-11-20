// Package docs, spreadsheet arm: parse .xlsx cells directly rather than
// asking a vision model to read a picture of a table.
package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"

	"github.com/bluesky-social/diderot/internal/attachments/files"
)

// maxSheetRows and maxSheetCols bound one sheet's contribution.
//
// A workbook is trivially large in a way a document is not: a single
// sheet can declare a million rows, and unlike a PDF there is no page
// count to reason about. These are generous for ordinary workbooks
// while keeping a pathological sheet from producing a chunk explosion. Truncation is
// recorded, never silent — a half-read inventory that claims to be
// complete is worse than one labeled partial.
const (
	maxSheetRows = 5000
	maxSheetCols = 200
	maxSheets    = 100

	// maxUnzipBytes / maxUnzipXMLBytes bound ZIP expansion. excelize's
	// defaults (16 GB / 16 MB) are effectively unbounded; these are
	// generous against a corpus whose largest workbook is 66 KB while
	// keeping a zip bomb from becoming a memory incident. Over
	// maxUnzipXMLBytes excelize spills a worksheet to a temp file rather
	// than failing, which on distroless means /tmp — acceptable, and
	// bounded by maxUnzipBytes.
	maxUnzipBytes    = 512 << 20 // 512 MiB expanded, total
	maxUnzipXMLBytes = 64 << 20  // 64 MiB per worksheet in memory
)

// SpreadsheetParser extracts .xlsx content as markdown tables.
//
// This exists because sending a spreadsheet to a rasterizing model
// destroys exactly what makes it a spreadsheet: the model returns sparse
// `| | | |` grids and drops the sheet names, and the "fix" is
// prompt-engineering a vision model into behaving like a parser. excelize gives cell values, sheet names, and
// typed data with no model call, no rasterization, and no cost.
//
// It satisfies Extractor, so the worker routes by format once and then
// treats every mechanism identically — the alternative (branching inside
// the worker) puts format knowledge in two places.
type SpreadsheetParser struct{}

// ModelID reports what produced the text. Not a model, and the column
// says so rather than lying with an empty string: an operator reading
// event_documents.extract_model needs to know which mechanism ran, and
// "did a model touch this document" is a question worth being able to
// answer from the table.
func (SpreadsheetParser) ModelID() string { return SpreadsheetMechanism }

// SpreadsheetMechanism is the extract_model value for locally parsed
// workbooks. Prefixed "local:" so a query can partition the table into
// model-touched and not.
const SpreadsheetMechanism = "local:excelize"

// Extract reads every sheet into a markdown table.
//
// Output shape matches what the splitter expects ("## <sheet name>" per
// sheet), so the chunker needs no special case — and per the revised
// boundary precedence, headings are what it splits on anyway.
//
// ctx is unused: this is pure CPU over bytes already in memory, with no
// network call to cancel, and the size guards below bound the work
// directly rather than via a deadline.
func (SpreadsheetParser) Extract(
	_ context.Context, docFormat string, _ string, data []byte,
) (*Result, error) {
	if f := strings.ToLower(strings.TrimSpace(docFormat)); f != "xlsx" {
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: SpreadsheetParser cannot read %q (excelize is OOXML-only; "+
				"legacy .xls is BIFF and out of scope)", docFormat)}
	}
	return parseSpreadsheet(data)
}

func parseSpreadsheet(data []byte) (*Result, error) {
	if len(data) == 0 {
		return nil, &files.PermanentError{Underlying: errors.New("docs: empty spreadsheet")}
	}
	// OOXML is a ZIP container, so the compressed size is not a bound on
	// the work it represents: a small .xlsx can declare gigabytes of
	// expanded XML. excelize's own limits are the right guard here —
	// they apply during unzip, before we hold anything. Defaults are
	// 16 GB / 16 MB, which for our purposes is no limit at all.
	f, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{
		UnzipSizeLimit:    maxUnzipBytes,
		UnzipXMLSizeLimit: maxUnzipXMLBytes,
		// RawCellValue false applies the cell's number format, so a date
		// reads as a date rather than a serial number. That matters for
		// retrieval: "2026-04-08" is searchable, 46121 is not.
		RawCellValue: false,
	})
	if err != nil {
		// A file that excelize cannot open will not open on a retry
		// either — a corrupt or password-protected workbook is
		// deterministic.
		return nil, &files.PermanentError{Underlying: fmt.Errorf("docs: open xlsx: %w", err)}
	}
	defer func() { _ = f.Close() }()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, &files.PermanentError{Underlying: errors.New("docs: xlsx has no sheets")}
	}
	truncated := false
	if len(sheets) > maxSheets {
		sheets = sheets[:maxSheets]
		truncated = true
	}

	var b strings.Builder
	for _, name := range sheets {
		rows, cut, err := readSheet(f, name)
		if err != nil {
			// One unreadable sheet must not lose the rest of the
			// workbook; record it in place so a reader sees the gap.
			fmt.Fprintf(&b, "## %s\n\n[sheet unreadable: %v]\n\n", name, err)
			continue
		}
		table, renderCut := renderSheet(rows)
		truncated = truncated || cut || renderCut
		if strings.TrimSpace(table) == "" {
			// Genuinely empty sheets are common in real workbooks
			// (template tabs, scratch space). Emitting a heading with no
			// body would produce a chunk carrying only a sheet name.
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n", name, table)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return nil, &files.PermanentError{
			Underlying: errors.New("docs: xlsx contained no non-empty cells")}
	}
	return &Result{
		Markdown: out,
		// No pages. A spreadsheet has sheets, and pretending otherwise
		// would put a meaningless page number in every citation.
		PageCount: 0,
		ModelID:   SpreadsheetMechanism,
		Truncated: truncated,
	}, nil
}

// readSheet reads a sheet's rows with the row and column caps applied
// DURING iteration rather than after.
//
// This is the difference between a bound and a claim. excelize's
// GetRows materializes the whole worksheet first, and it pads every row
// out to the column of its last populated cell — up to XFD (16,384). So
// a crafted workbook with one value at XFD on each of 5,001 rows is
// ~120 KB of worksheet XML (far under maxUnzipXMLBytes) that decodes to
// ~82 million string headers, over 1 GiB, before our 5,000×200 cap ever
// runs. The unzip limits bound decompressed BYTES, not decoded Go
// allocations, so they cannot catch this.
//
// Streaming with f.Rows keeps at most one wide row alive transiently and
// stops at the row cap, so the retained data is genuinely bounded.
// Reports whether anything was cut.
func readSheet(f *excelize.File, name string) ([][]string, bool, error) {
	iter, err := f.Rows(name)
	if err != nil {
		return nil, false, err
	}
	var (
		rows = make([][]string, 0, 64)
		cut  bool
	)
	for len(rows) < maxSheetRows && iter.Next() {
		row, rerr := iter.Columns()
		if rerr != nil {
			_ = iter.Close()
			return nil, false, rerr
		}
		if len(row) > maxSheetCols {
			row = row[:maxSheetCols]
			cut = true
		}
		rows = append(rows, row)
	}
	// Distinguish "hit the cap" from "reached the end at exactly the cap".
	if len(rows) == maxSheetRows && iter.Next() {
		cut = true
	}
	if err := iter.Close(); err != nil {
		return nil, false, err
	}
	return rows, cut, nil
}

// renderSheet turns rows into a markdown table, dropping entirely-empty
// rows and columns.
//
// The empty-cell pruning is not cosmetic. Excel's used-range routinely
// extends far past the real data, so a naive render emits page after
// page of `| | | |` — which is what the model path actually produced,
// and which embeds into vectors that match nothing while displacing
// real content. Reports whether it truncated.
func renderSheet(rows [][]string) (string, bool) {
	// Find the last row and column that contain anything.
	lastRow, lastCol := -1, -1
	for i, row := range rows {
		for j, cell := range row {
			if strings.TrimSpace(cell) != "" {
				if i > lastRow {
					lastRow = i
				}
				if j > lastCol {
					lastCol = j
				}
			}
		}
	}
	if lastRow < 0 {
		return "", false
	}
	truncated := false
	if lastRow >= maxSheetRows {
		lastRow = maxSheetRows - 1
		truncated = true
	}
	if lastCol >= maxSheetCols {
		lastCol = maxSheetCols - 1
		truncated = true
	}

	// Columns that are empty across every retained row carry no
	// information and are dropped, so the header row stays readable.
	keep := make([]int, 0, lastCol+1)
	for j := 0; j <= lastCol; j++ {
		for i := 0; i <= lastRow && i < len(rows); i++ {
			if j < len(rows[i]) && strings.TrimSpace(rows[i][j]) != "" {
				keep = append(keep, j)
				break
			}
		}
	}
	if len(keep) == 0 {
		return "", truncated
	}

	var b strings.Builder
	wroteHeader := false
	for i := 0; i <= lastRow && i < len(rows); i++ {
		cells := make([]string, 0, len(keep))
		any := false
		for _, j := range keep {
			v := ""
			if j < len(rows[i]) {
				v = strings.TrimSpace(rows[i][j])
			}
			if v != "" {
				any = true
			}
			cells = append(cells, escapeCell(v))
		}
		if !any {
			// Entirely-empty row: skip rather than emit a pipe skeleton.
			continue
		}
		if !wroteHeader {
			// The first non-empty row is the header, and it is emitted at
			// the FULL retained width — deliberately not trimmed like the
			// body rows below.
			//
			// In GFM the separator row defines the table's column count,
			// and consumers drop any value past it. keep[] spans every
			// column used by any row, so trimming a header like
			// {"Title", ""} down to one column while a later row has two
			// would silently discard that row's second value. Costing one
			// wide header row is the right trade for not losing data.
			fmt.Fprintf(&b, "| %s |\n", strings.Join(cells, " | "))
			sep := make([]string, len(cells))
			for k := range sep {
				sep[k] = "---"
			}
			fmt.Fprintf(&b, "| %s |\n", strings.Join(sep, " | "))
			wroteHeader = true
			continue
		}
		// Trim trailing empty cells on body rows. keep[] is computed for
		// the whole sheet, so a narrative row that occupies only column A
		// still emits an empty cell for every other retained column. On a
		// wide workbook with prose rows in column A that is thousands of
		// empty cells and hundreds of `|  |  |` skeletons — noise the
		// embedder pays for and that matches nothing. Interior empties are KEPT, because in
		// a grid they carry position: cell 5 being blank means column 5
		// has no value for this row.
		for len(cells) > 1 && cells[len(cells)-1] == "" {
			cells = cells[:len(cells)-1]
		}
		fmt.Fprintf(&b, "| %s |\n", strings.Join(cells, " | "))
	}
	return b.String(), truncated
}

// escapeCell keeps a cell's own pipes and newlines from breaking the
// table. A cell legitimately containing "a|b" would otherwise silently
// become two columns and shift every value after it.
func escapeCell(s string) string {
	// Escape backslashes BEFORE pipes. Markdown resolves backslash
	// escapes left to right, so a cell containing `a\|b` escaped naively
	// becomes `a\\|b` — which parses as a literal backslash followed by an
	// ACTIVE delimiter, splitting the row anyway and shifting every value
	// after it. Order is the whole fix.
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}
