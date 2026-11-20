package docs

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/chunk"
)

// newTestDocx assembles a minimal but real .docx: a ZIP whose
// word/document.xml is the body given. Real WordprocessingML, so the
// streaming parser is exercised the way a Word file exercises it.
func newTestDocx(t *testing.T, bodyXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	full := `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>` + bodyXML + `</w:body></w:document>`
	if _, err := w.Write([]byte(full)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func para(text string) string {
	return `<w:p><w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

func heading(level, text string) string {
	return `<w:p><w:pPr><w:pStyle w:val="Heading` + level + `"/></w:pPr>` +
		`<w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

func TestDocxParagraphsAndHeadings(t *testing.T) {
	data := newTestDocx(t,
		heading("1", "Master Services Agreement")+
			para("This agreement is entered into as of April 20, 2026.")+
			heading("2", "3. Payment Terms")+
			para("Net 30 from invoice date."))
	res, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", data)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Headings become markdown headings because that is precisely what
	// the revised chunker splits on.
	for _, want := range []string{
		"# Master Services Agreement",
		"## 3. Payment Terms",
		"Net 30 from invoice date.",
		"April 20, 2026",
	} {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("missing %q in:\n%s", want, res.Markdown)
		}
	}
	// A .docx has no pages: pagination is a property of whatever renders
	// it, so any page number we emitted would be invented.
	if res.PageCount != 0 {
		t.Errorf("PageCount = %d, want 0", res.PageCount)
	}
	if res.ModelID != DocumentMechanism {
		t.Errorf("ModelID = %q", res.ModelID)
	}
}

// TestDocxSplitRunsJoinCleanly is the OOXML shape that breaks naive
// concatenation: Word splits a sentence across a new run at every
// formatting change, so "Net 30 days" can be five separate w:t elements.
// Without run joining the text arrives fragmented and the retrieval
// signal is destroyed.
func TestDocxSplitRunsJoinCleanly(t *testing.T) {
	data := newTestDocx(t,
		`<w:p>`+
			`<w:r><w:t xml:space="preserve">The fee is </w:t></w:r>`+
			`<w:r><w:rPr><w:b/></w:rPr><w:t>USD 40,000</w:t></w:r>`+
			`<w:r><w:t xml:space="preserve"> per annum.</w:t></w:r>`+
			`</w:p>`)
	res, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Markdown, "The fee is USD 40,000 per annum.") {
		t.Errorf("runs not joined cleanly: %q", res.Markdown)
	}
}

func TestDocxTables(t *testing.T) {
	data := newTestDocx(t, `<w:tbl>
		<w:tr><w:tc>`+para("Milestone")+`</w:tc><w:tc>`+para("Date")+`</w:tc></w:tr>
		<w:tr><w:tc>`+para("Kickoff")+`</w:tc><w:tc>`+para("2026-05-01")+`</w:tc></w:tr>
	</w:tbl>`)
	res, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Milestone | Date |",
		"| --- | --- |",
		"| Kickoff | 2026-05-01 |",
	} {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("missing %q in:\n%s", want, res.Markdown)
		}
	}
}

// TestDocxDropsLayoutOnlyRows: Word uses tables for page layout, so real
// documents contain rows whose every cell is empty. They render as
// `|  |  |` skeletons that embed to vectors matching nothing. Found on
// the real grant agreement in the corpus, not by a unit test — this is
// the regression pin.
func TestDocxDropsLayoutOnlyRows(t *testing.T) {
	data := newTestDocx(t, `<w:tbl>
		<w:tr><w:tc>`+para("Real")+`</w:tc><w:tc>`+para("Header")+`</w:tc></w:tr>
		<w:tr><w:tc>`+para("")+`</w:tc><w:tc>`+para("")+`</w:tc></w:tr>
		<w:tr><w:tc>`+para("value")+`</w:tc><w:tc>`+para("")+`</w:tc></w:tr>
	</w:tbl>`)
	res, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", data)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(res.Markdown, "\n") {
		row := strings.TrimSpace(line)
		// Trim only pipes and whitespace, NOT dashes: "| --- | --- |" is
		// the header separator and is required for the table to render.
		if strings.HasPrefix(row, "|") && strings.Trim(row, "| \t") == "" {
			t.Errorf("line %d is an empty table row: %q", i, row)
		}
	}
	if !strings.Contains(res.Markdown, "| value |") {
		t.Errorf("trailing-empty row lost its content:\n%s", res.Markdown)
	}
}

// TestDocxExcludesDeletedText is the redline case, and the reason it
// matters is legal rather than cosmetic: w:del content is text that was
// REMOVED. Indexing it would put superseded contract terms in the corpus
// as if they were current — worse than omitting them, because a reader
// asking "what does the contract say about termination" would get the
// struck version quoted back as fact.
func TestDocxExcludesDeletedText(t *testing.T) {
	data := newTestDocx(t,
		`<w:p>`+
			`<w:r><w:t xml:space="preserve">Termination requires </w:t></w:r>`+
			`<w:del><w:r><w:delText>30 days</w:delText></w:r></w:del>`+
			`<w:ins><w:r><w:t>90 days</w:t></w:r></w:ins>`+
			`<w:r><w:t xml:space="preserve"> notice.</w:t></w:r>`+
			`</w:p>`)
	res, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Markdown, "30 days") {
		t.Errorf("deleted text leaked into the index: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "90 days") {
		t.Errorf("inserted (current) text lost: %q", res.Markdown)
	}
}

func TestDocxHeadingLevels(t *testing.T) {
	for _, tc := range []struct {
		style string
		want  int
	}{
		{"Heading1", 1},
		{"Heading3", 3},
		{"heading 2", 2},
		{"Title", 1},
		// Returns the true level; the h4 cap is applied at render time
		// (see TestDocxHeadingDepthCapped), not here.
		{"Heading7", 7},
		{"BodyText", 0},
		{"ListParagraph", 0},
		{"", 0},
		{"Heading99", 0}, // not a real level
	} {
		t.Run(tc.style, func(t *testing.T) {
			if got := headingLevel(tc.style); got != tc.want {
				t.Errorf("headingLevel(%q) = %d, want %d", tc.style, got, tc.want)
			}
		})
	}
}

func TestDocxHeadingDepthCapped(t *testing.T) {
	data := newTestDocx(t, heading("7", "12.4.3.1 Sub-sub-clause")+para("body"))
	res, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Markdown, "#### 12.4.3.1") {
		t.Errorf("heading not capped at h4:\n%s", res.Markdown)
	}
	if strings.Contains(res.Markdown, "##### ") {
		t.Errorf("h5+ emitted:\n%s", res.Markdown)
	}
}

func TestDocxRejectsWrongFormat(t *testing.T) {
	for _, f := range []string{"doc", "pdf", "xlsx", ""} {
		_, err := DocumentParser{}.Extract(context.Background(), f, "a", []byte("x"))
		if err == nil {
			t.Errorf("format %q accepted", f)
			continue
		}
		if !isPermanent(err) {
			t.Errorf("format %q gave a retryable error: %v", f, err)
		}
	}
}

func TestDocxNotAZipIsPermanent(t *testing.T) {
	_, err := DocumentParser{}.Extract(
		context.Background(), "docx", "a.docx", []byte("this is not a zip"))
	if err == nil {
		t.Fatal("non-zip accepted")
	}
	if !isPermanent(err) {
		t.Errorf("want permanent, got: %v", err)
	}
}

// TestDocxMissingMainPart covers an OOXML file that is a valid ZIP but
// not a wordprocessing document — e.g. a .xlsx renamed to .docx, which
// the corpus's mislabeled-filename case makes a live possibility.
func TestDocxMissingMainPart(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("xl/workbook.xml")
	_, _ = w.Write([]byte("<workbook/>"))
	_ = zw.Close()
	_, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", buf.Bytes())
	if err == nil {
		t.Fatal("a zip with no word/document.xml was accepted")
	}
	if !isPermanent(err) {
		t.Errorf("want permanent, got: %v", err)
	}
}

// TestDocxSuffixMatchIsExact: a crafted archive can contain an entry
// like "evil/word/document.xml". Only the canonical path counts.
func TestDocxSuffixMatchIsExact(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("evil/word/document.xml")
	_, _ = w.Write([]byte("<w:document/>"))
	_ = zw.Close()
	_, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", buf.Bytes())
	if err == nil {
		t.Fatal("a non-canonical document.xml path was accepted")
	}
}

// A docx with no text is not an error (TestDocxPicturesAreReturnedForCaptions
// says why). The worker writes no chunk for it: empty text settles the
// document as having none.
func TestDocxEmptyBodySettlesWithoutChunks(t *testing.T) {
	data := newTestDocx(t, para("")+para("   "))
	result, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", data)
	if err != nil {
		t.Fatal(err)
	}
	if reason := Reject(result.Markdown); reason != RejectEmpty {
		t.Errorf("empty text should settle as %q, got %q", RejectEmpty, reason)
	}
}

func TestDocxSplitsOnHeadings(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 4; i++ {
		b.WriteString(heading("2", "Section "+string(rune('A'+i-1))))
		b.WriteString(para(strings.Repeat(
			"Contract language about termination and payment terms. ", 12)))
	}
	data := newTestDocx(t, b.String())
	res, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", data)
	if err != nil {
		t.Fatal(err)
	}
	chunks := Split(res.Markdown)
	if len(chunks) < 2 {
		t.Fatalf("want multiple chunks from a 4-section document, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c.Content)); n > chunk.Threshold {
			t.Errorf("chunk %d is %d runes, over the %d threshold", i, n, chunk.Threshold)
		}
		if c.Page != 0 {
			t.Errorf("chunk %d has Page=%d; a docx has no pages", i, c.Page)
		}
	}
}

func TestDocxTabsAndBreaksBecomeSpaces(t *testing.T) {
	data := newTestDocx(t,
		`<w:p><w:r><w:t>Clause</w:t><w:tab/><w:t>4.1</w:t><w:br/><w:t>continued</w:t></w:r></w:p>`)
	res, err := DocumentParser{}.Extract(context.Background(), "docx", "a.docx", data)
	if err != nil {
		t.Fatal(err)
	}
	// Without this, "Clause" and "4.1" collide into one token and the
	// clause number stops being searchable.
	if !strings.Contains(res.Markdown, "Clause 4.1 continued") {
		t.Errorf("tab/br not flattened: %q", res.Markdown)
	}
}
