package docs

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/chunk"
)

func TestSplitPageLevel(t *testing.T) {
	in := `--- PAGE 1 ---

# Retention Policy

Section A: subscriber records kept 18 months.

--- PAGE 2 ---

Section B: IP logs kept 90 days.

--- PAGE 3 ---

Escalation contact is legal-ops.`
	got := Split(in)
	if len(got) != 3 {
		t.Fatalf("want 3 chunks (one per page), got %d: %+v", len(got), got)
	}
	for i, c := range got {
		if c.Page != i+1 {
			t.Errorf("chunk %d has Page=%d, want %d", i, c.Page, i+1)
		}
		if strings.Contains(c.Content, "--- PAGE") {
			t.Errorf("chunk %d leaked a page marker: %q", i, c.Content)
		}
		if strings.TrimSpace(c.Content) == "" {
			t.Errorf("chunk %d is empty", i)
		}
	}
	if !strings.Contains(got[0].Content, "18 months") {
		t.Errorf("page 1 content wrong: %q", got[0].Content)
	}
	if !strings.Contains(got[2].Content, "legal-ops") {
		t.Errorf("page 3 content wrong: %q", got[2].Content)
	}
}

// TestSplitPreamblePreserved: models sometimes emit a title line before
// the first page marker. Dropping it would lose the document's title
// from the index, which is high-value retrieval signal.
func TestSplitPreamblePreserved(t *testing.T) {
	in := `# Master Services Agreement

--- PAGE 1 ---

Body of page one.`
	got := Split(in)
	if len(got) != 1 {
		t.Fatalf("want the preamble attached to page 1, got %d chunks", len(got))
	}
	if !strings.Contains(got[0].Content, "Master Services Agreement") {
		t.Errorf("preamble lost: %+v", got)
	}
	if got[0].Page != 1 {
		t.Errorf("preamble Page = %d, want 1 (attributed to the first page)", got[0].Page)
	}
}

// TestSplitPageMarkerTolerance covers the marker variants models
// actually emit around the requested format.
func TestSplitPageMarkerTolerance(t *testing.T) {
	for _, marker := range []string{
		"--- PAGE 2 ---",
		"  --- PAGE 2 ---  ",
		"--- PAGE 2 ---------",
		"--- PAGE 2",
	} {
		t.Run(marker, func(t *testing.T) {
			in := "--- PAGE 1 ---\nfirst page body here\n" + marker + "\nsecond page body here"
			got := Split(in)
			if len(got) != 2 {
				t.Fatalf("marker %q: want 2 chunks, got %d: %+v", marker, len(got), got)
			}
			if got[1].Page != 2 {
				t.Errorf("marker %q: second chunk Page = %d, want 2", marker, got[1].Page)
			}
		})
	}
}

// TestSplitPagedSpreadsheetKeepsSheetName pins that a sheet heading
// survives inside a page marker.
//
// The prompt asks for page markers AND per-sheet "## " headings, and the
// model obliges on both — so a spreadsheet comes back with a page marker
// wrapping the sheet headings. If the page branch wins outright it
// drops the section label, leaving every chunk cited as "page 1" with no
// indication of which sheet it came from. For a multi-sheet workbook
// that's most of the useful provenance.
func TestSplitPagedSpreadsheetKeepsSheetName(t *testing.T) {
	// Two sheets inside one page marker — the observed shape.
	in := "--- PAGE 1 ---\n\n## Order_Details\n\n" +
		"| ID | Device |\n|---|---|\n| 33 | Widget |\n\n" +
		"## Region_Summary\n\n| Region | Owner |\n|---|---|\n| 0107 | Platform |\n"
	got := Split(in)
	if len(got) != 2 {
		t.Fatalf("want 2 chunks (one per sheet), got %d: %+v", len(got), got)
	}
	for i, want := range []string{"Order_Details", "Region_Summary"} {
		if got[i].Section != want {
			t.Errorf("chunk %d Section = %q, want %q", i, got[i].Section, want)
		}
		// Page attribution survives alongside the sheet name.
		if got[i].Page != 1 {
			t.Errorf("chunk %d Page = %d, want 1", i, got[i].Page)
		}
	}
}

// TestSplitSingleSheetKeepsSheetName is the case a "more than one
// section" guard would miss: a one-sheet workbook produces exactly one
// section, which is the common shape, not an edge case.
func TestSplitSingleSheetKeepsSheetName(t *testing.T) {
	in := "--- PAGE 1 ---\n\n## Order_Details\n\n" +
		"| ID | Device |\n|---|---|\n| 33 | Widget |\n"
	got := Split(in)
	if len(got) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(got))
	}
	if got[0].Section != "Order_Details" {
		t.Errorf("Section = %q, want the sheet name", got[0].Section)
	}
}

// TestSplitPagedProseHasNoSection guards the other direction: an
// ordinary paged document must not acquire a spurious section label just
// because its body happens to contain a heading mid-page.
func TestSplitPagedProseHasNoSection(t *testing.T) {
	in := "--- PAGE 1 ---\n\nRetention policy body text follows here.\n" +
		"More prose on the same page about escalation contacts.\n" +
		"--- PAGE 2 ---\n\nSecond page of ordinary prose content here.\n"
	got := Split(in)
	if len(got) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(got))
	}
	for i, c := range got {
		if c.Section != "" {
			t.Errorf("chunk %d acquired Section %q on ordinary prose", i, c.Section)
		}
		if c.Page != i+1 {
			t.Errorf("chunk %d Page = %d, want %d", i, c.Page, i+1)
		}
	}
}

func TestSplitSpreadsheetBySheet(t *testing.T) {
	in := `## Order_Details

| ID | Device |
|---|---|
| 33 | Widget |

## Region_Summary

| Region | Owner |
|---|---|
| 0107 | Platform |`
	got := Split(in)
	if len(got) != 2 {
		t.Fatalf("want 2 chunks (one per sheet), got %d: %+v", len(got), got)
	}
	if got[0].Section != "Order_Details" {
		t.Errorf("chunk 0 Section = %q", got[0].Section)
	}
	if got[1].Section != "Region_Summary" {
		t.Errorf("chunk 1 Section = %q", got[1].Section)
	}
	// The heading stays in the body: a sheet named "Retention Schedule"
	// should match a query about retention.
	if !strings.Contains(got[0].Content, "Order_Details") {
		t.Errorf("sheet heading dropped from body: %q", got[0].Content)
	}
}

// TestSplitOversizedPageSubdivides: a page over maxChunkRunes must split
// further rather than producing one giant chunk (measured retrieval
// degrades badly on very large chunks).
func TestSplitOversizedPageSubdivides(t *testing.T) {
	var b strings.Builder
	b.WriteString("--- PAGE 1 ---\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "## Section %d\n\nSome reasonably long body text for section %d that carries content about retention schedules and escalation paths.\n\n", i, i)
	}
	got := Split(b.String())
	if len(got) < 2 {
		t.Fatalf("oversized page should subdivide, got %d chunk(s)", len(got))
	}
	for i, c := range got {
		if n := len([]rune(c.Content)); n > chunk.Threshold {
			t.Errorf("chunk %d is %d runes, over the %d threshold", i, n, chunk.Threshold)
		}
		if c.Page != 1 {
			t.Errorf("chunk %d lost its page attribution: Page=%d", i, c.Page)
		}
	}
}

// TestSplitStructurelessWallOfText falls all the way through to the
// shared sliding-window chunker.
func TestSplitStructurelessWallOfText(t *testing.T) {
	in := strings.Repeat("word ", 2000) // ~10k runes, no headings, no pages
	got := Split(in)
	if len(got) < 2 {
		t.Fatalf("want multiple chunks for a 10k-rune wall, got %d", len(got))
	}
	for i, c := range got {
		if strings.TrimSpace(c.Content) == "" {
			t.Errorf("chunk %d empty", i)
		}
	}
}

func TestSplitEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n\t"} {
		if got := Split(in); got != nil {
			t.Errorf("Split(%q) = %+v, want nil", in, got)
		}
	}
}

func TestSplitNoPagesSingleChunk(t *testing.T) {
	in := "Short plain text file with no structure at all."
	got := Split(in)
	if len(got) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(got))
	}
	if got[0].Page != 0 {
		t.Errorf("Page = %d, want 0 (no page structure)", got[0].Page)
	}
	if got[0].Section != "" {
		t.Errorf("Section = %q, want empty", got[0].Section)
	}
}

func TestContextPrefix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chunk Chunk
		want  string
	}{
		{
			name:  "page",
			chunk: Chunk{Page: 7},
			want:  "[document] contract.pdf (page 7) shared by Alex in #legal on 2026-07-28",
		},
		{
			name:  "section wins over page",
			chunk: Chunk{Page: 2, Section: "Region_Summary"},
			want:  "[document] contract.pdf (section Region_Summary) shared by Alex in #legal on 2026-07-28",
		},
		{
			name:  "no page no section",
			chunk: Chunk{},
			want:  "[document] contract.pdf shared by Alex in #legal on 2026-07-28",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ContextPrefix("contract.pdf", "Alex", "legal", "2026-07-28", tc.chunk)
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestContextPrefixDegradesGracefully: channel name and actor are
// best-effort lookups in the worker, so the prefix must stay sensible
// when they're missing rather than emitting stray separators.
func TestContextPrefixDegradesGracefully(t *testing.T) {
	got := ContextPrefix("notes.pdf", "", "", "", Chunk{Page: 1})
	want := "[document] notes.pdf (page 1)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
