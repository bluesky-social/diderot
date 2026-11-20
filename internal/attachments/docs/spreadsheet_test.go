package docs

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// newTestWorkbook builds a real .xlsx in memory. Building a genuine
// workbook rather than fixture bytes is deliberate: the failure this
// package exists to prevent came from a real file's shape (used-range
// padding, sheet names, typed cells), and a hand-rolled byte fixture
// can't reproduce that.
func newTestWorkbook(t *testing.T, sheets map[string][][]any, order []string) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	for i, name := range order {
		if i == 0 {
			if err := f.SetSheetName("Sheet1", name); err != nil {
				t.Fatal(err)
			}
		} else if _, err := f.NewSheet(name); err != nil {
			t.Fatal(err)
		}
		for r, row := range sheets[name] {
			for c, v := range row {
				cell, err := excelize.CoordinatesToCellName(c+1, r+1)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.SetCellValue(name, cell, v); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSpreadsheetParserBasics(t *testing.T) {
	data := newTestWorkbook(t, map[string][][]any{
		"Order_Details": {
			{"ID", "Device", "Serial"},
			{33, "Widget", "S-1029"},
			{34, "Switch", "S-1030"},
		},
		"Region_Summary": {
			{"Region", "Owner"},
			{"0107", "Platform"},
		},
	}, []string{"Order_Details", "Region_Summary"})

	res, err := SpreadsheetParser{}.Extract(context.Background(), "xlsx", "orders.xlsx", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Sheet names are the provenance a rasterizing model lost. They must
	// survive as "## " headings, which is also what the splitter cuts on.
	for _, want := range []string{
		"## Order_Details", "## Region_Summary",
		"| ID | Device | Serial |", "| 33 | Widget | S-1029 |",
		"| 0107 | Platform |",
	} {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("missing %q in:\n%s", want, res.Markdown)
		}
	}
	// A markdown table without the separator row after its header does
	// not render as a table at all.
	if !strings.Contains(res.Markdown, "| --- | --- | --- |") {
		t.Errorf("no header separator emitted:\n%s", res.Markdown)
	}
	// A spreadsheet has sheets, not pages. A page number in the citation
	// would be meaningless.
	if res.PageCount != 0 {
		t.Errorf("PageCount = %d, want 0", res.PageCount)
	}
	if res.ModelID != SpreadsheetMechanism {
		t.Errorf("ModelID = %q, want %q", res.ModelID, SpreadsheetMechanism)
	}
	if res.Truncated {
		t.Error("small workbook should not be truncated")
	}
}

// TestSpreadsheetSplitsPerSheet is the end-to-end property that matters:
// the parser's output has to chunk per sheet with the sheet name intact.
// This is the bug the live model run produced (every chunk labeled
// "page 1", sheet unknown), so it gets an assertion rather than trust.
func TestSpreadsheetSplitsPerSheet(t *testing.T) {
	data := newTestWorkbook(t, map[string][][]any{
		"Inventory": {{"ID", "Device"}, {1, "Server"}},
		"Contacts":  {{"Name", "Team"}, {"ops", "Platform"}},
	}, []string{"Inventory", "Contacts"})
	res, err := SpreadsheetParser{}.Extract(context.Background(), "xlsx", "a.xlsx", data)
	if err != nil {
		t.Fatal(err)
	}
	chunks := Split(res.Markdown)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks (one per sheet), got %d: %+v", len(chunks), chunks)
	}
	for i, want := range []string{"Inventory", "Contacts"} {
		if chunks[i].Section != want {
			t.Errorf("chunk %d Section = %q, want %q", i, chunks[i].Section, want)
		}
		if chunks[i].Page != 0 {
			t.Errorf("chunk %d Page = %d, want 0 for a spreadsheet", i, chunks[i].Page)
		}
	}
}

// TestSpreadsheetPrunesEmptyGrid pins that empty rows and columns are
// pruned. Excel's used-range extends far past the real data, so a naive render
// emits page after page of `| | | |` — vectors that match nothing while
// displacing real content.
func TestSpreadsheetPrunesEmptyGrid(t *testing.T) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	if err := f.SetCellValue("Sheet1", "A1", "Header"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetCellValue("Sheet1", "A2", "value"); err != nil {
		t.Fatal(err)
	}
	// Push the used range out to Z200 with a single distant cell, the
	// way a real workbook does after editing.
	if err := f.SetCellValue("Sheet1", "Z200", ""); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	res, err := SpreadsheetParser{}.Extract(context.Background(), "xlsx", "a.xlsx", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(res.Markdown, "\n"); n > 8 {
		t.Errorf("empty grid not pruned: %d lines\n%s", n, res.Markdown)
	}
	if strings.Contains(res.Markdown, "|  |  |") {
		t.Errorf("emitted an empty pipe skeleton:\n%s", res.Markdown)
	}
}

// TestSpreadsheetEscapesPipes: a cell containing a pipe would silently
// become two columns and shift every value after it — a wrong serial
// number attributed to the wrong asset, with no error anywhere.
func TestSpreadsheetEscapesPipes(t *testing.T) {
	data := newTestWorkbook(t, map[string][][]any{
		"S": {{"Field", "Value"}, {"range", "a|b"}, {"note", "line1\nline2"}},
	}, []string{"S"})
	res, err := SpreadsheetParser{}.Extract(context.Background(), "xlsx", "a.xlsx", data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Markdown, `a\|b`) {
		t.Errorf("pipe not escaped:\n%s", res.Markdown)
	}
	for _, line := range strings.Split(res.Markdown, "\n") {
		if strings.HasPrefix(line, "| note") && !strings.Contains(line, "line1 line2") {
			t.Errorf("newline not flattened: %q", line)
		}
	}
}

// TestSpreadsheetRejectsWrongFormat: the parser must refuse a format it
// cannot read rather than returning a confusing excelize error. Legacy
// .xls is BIFF, which excelize has no path for at all.
func TestSpreadsheetRejectsWrongFormat(t *testing.T) {
	for _, f := range []string{"xls", "pdf", "docx", ""} {
		_, err := SpreadsheetParser{}.Extract(context.Background(), f, "a", []byte("x"))
		if err == nil {
			t.Errorf("format %q was accepted", f)
			continue
		}
		if !isPermanent(err) {
			t.Errorf("format %q gave a retryable error: %v", f, err)
		}
	}
}

// TestSpreadsheetCorruptIsPermanent: a workbook excelize cannot open will
// not open on a retry either, so it must not burn the failure budget.
func TestSpreadsheetCorruptIsPermanent(t *testing.T) {
	_, err := SpreadsheetParser{}.Extract(
		context.Background(), "xlsx", "a.xlsx", []byte("PK\x03\x04 not really a workbook"))
	if err == nil {
		t.Fatal("corrupt workbook was accepted")
	}
	if !isPermanent(err) {
		t.Errorf("corrupt workbook gave a retryable error: %v", err)
	}
}

func TestSpreadsheetEmptyWorkbook(t *testing.T) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	_, err := SpreadsheetParser{}.Extract(context.Background(), "xlsx", "a.xlsx", buf.Bytes())
	if err == nil {
		t.Fatal("a workbook with no cells should error rather than write empty chunks")
	}
	if !isPermanent(err) {
		t.Errorf("empty workbook gave a retryable error: %v", err)
	}
}

func TestSpreadsheetRowTruncationIsRecorded(t *testing.T) {
	rows := make([][]any, 0, maxSheetRows+50)
	rows = append(rows, []any{"Header"})
	for i := 0; i < maxSheetRows+40; i++ {
		rows = append(rows, []any{i})
	}
	data := newTestWorkbook(t, map[string][][]any{"Big": rows}, []string{"Big"})
	res, err := SpreadsheetParser{}.Extract(context.Background(), "xlsx", "a.xlsx", data)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Error("row truncation must be recorded; a partial inventory that " +
			"claims to be complete is worse than one labeled partial")
	}
}

// TestSpreadsheetSparseWideSheetIsBounded bounds a memory vector:
// excelize's GetRows pads every row out to the
// column of its last populated cell, up to XFD (16,384). One value at a
// far column on each of many rows is a few KB of worksheet XML that
// decodes to tens of millions of string headers — the unzip limits bound
// decompressed BYTES, not decoded Go allocations, so they cannot catch
// it. Streaming with f.Rows and capping during iteration is what makes
// the 5,000x200 bound real rather than aspirational.
//
// Deliberately modest (400 rows x XFD, ~6.5M slots under GetRows) so the
// test stays fast; the mechanism is identical at 5,001 rows.
func TestSpreadsheetSparseWideSheetIsBounded(t *testing.T) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	if err := f.SetCellValue("Sheet1", "A1", "Header"); err != nil {
		t.Fatal(err)
	}
	for r := 2; r <= 400; r++ {
		cell, err := excelize.CoordinatesToCellName(16384, r)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.SetCellValue("Sheet1", cell, r); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}

	res, err := SpreadsheetParser{}.Extract(
		context.Background(), "xlsx", "sparse.xlsx", buf.Bytes())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// The column cap must actually have applied: a row rendered at full
	// XFD width would be ~49k characters by itself.
	for i, line := range strings.Split(res.Markdown, "\n") {
		if n := strings.Count(line, "|"); n > maxSheetCols+2 {
			t.Fatalf("line %d has %d cells, over the %d column cap",
				i, n, maxSheetCols)
		}
	}
	if !res.Truncated {
		t.Error("column truncation must be recorded")
	}
}

// TestSpreadsheetHeaderSeparatorSpansEveryColumn: in GFM
// the separator row defines the table's column count and consumers drop
// any value past it, so trimming trailing empties off the HEADER (as the
// body rows correctly do) silently discards data from wider rows below.
func TestSpreadsheetHeaderSeparatorSpansEveryColumn(t *testing.T) {
	data := newTestWorkbook(t, map[string][][]any{
		// Header has a trailing empty; the row below fills it.
		"S": {{"Title", ""}, {"a", "b"}},
	}, []string{"S"})
	res, err := SpreadsheetParser{}.Extract(context.Background(), "xlsx", "a.xlsx", data)
	if err != nil {
		t.Fatal(err)
	}
	var widths []int
	for _, line := range strings.Split(res.Markdown, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") {
			widths = append(widths, strings.Count(line, "|")-1)
		}
	}
	if len(widths) < 3 {
		t.Fatalf("expected header + separator + body row, got %v:\n%s",
			widths, res.Markdown)
	}
	// The separator must be at least as wide as every body row, or the
	// body row's extra value is dropped by the renderer.
	sep := widths[1]
	for i, w := range widths[2:] {
		if w > sep {
			t.Errorf("body row %d has %d columns but the separator defines %d; "+
				"the extra value would be discarded:\n%s", i, w, sep, res.Markdown)
		}
	}
	if !strings.Contains(res.Markdown, "| a | b |") {
		t.Errorf("body row lost a value:\n%s", res.Markdown)
	}
}

// TestSpreadsheetEscapesBackslashBeforePipe: Markdown
// resolves backslash escapes left to right, so escaping the pipe without
// first escaping the backslash turns `a\|b` into `a\\|b` — a literal
// backslash followed by an ACTIVE delimiter, which splits the row anyway.
func TestSpreadsheetEscapesBackslashBeforePipe(t *testing.T) {
	data := newTestWorkbook(t, map[string][][]any{
		"S": {{"Field", "Value"}, {"path", `a\|b`}, {"win", `C:\temp`}},
	}, []string{"S"})
	res, err := SpreadsheetParser{}.Extract(context.Background(), "xlsx", "a.xlsx", data)
	if err != nil {
		t.Fatal(err)
	}
	// The backslash is doubled, so the pipe's own escape survives.
	if !strings.Contains(res.Markdown, `a\\\|b`) {
		t.Errorf("backslash not escaped before pipe:\n%s", res.Markdown)
	}
	// Every row must still have the same *delimiter* count as the header.
	// Counting raw '|' bytes is wrong here — an escaped pipe is a literal
	// and not a delimiter — so count only unescaped ones, which is
	// exactly the distinction the fix is about.
	var widths []int
	for _, line := range strings.Split(res.Markdown, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		widths = append(widths, countDelimiters(line))
	}
	for i, w := range widths {
		if w != widths[0] {
			t.Errorf("row %d has %d delimiters, header has %d — the row split:\n%s",
				i, w, widths[0], res.Markdown)
		}
	}
}

// countDelimiters counts pipes that actually delimit a column, i.e.
// those preceded by an even number of backslashes. This is the same
// left-to-right rule a markdown renderer applies.
func countDelimiters(line string) int {
	n, slashes := 0, 0
	for _, r := range line {
		switch r {
		case '\\':
			slashes++
		case '|':
			if slashes%2 == 0 {
				n++
			}
			slashes = 0
		default:
			slashes = 0
		}
	}
	return n
}
