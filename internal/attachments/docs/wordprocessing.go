// Package docs, wordprocessing arm: read .docx paragraphs out of the
// OOXML directly rather than through a rasterizing model.
package docs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bluesky-social/diderot/internal/attachments/files"
)

// DocumentMechanism is the extract_model value for locally parsed .docx.
const DocumentMechanism = "local:ooxml"

// maxDocumentPartBytes bounds the decompressed word/document.xml we will
// hold and parse.
//
// This is the guard that matters for .docx, because the compressed size
// is not a bound on the work: OOXML is a ZIP container and a small file
// can declare gigabytes of expanded XML. The largest real document part
// in the corpus is a 3.3 MB redline's, so 128 MiB is generous by ~40x
// while keeping a zip bomb from becoming a memory incident.
//
// Go's encoding/xml is immune to entity expansion (billion laughs) and
// to XXE — it does not resolve external entities and does not expand
// custom entity declarations — which is why this needs a size bound
// rather than the entity-hardening a Python equivalent would need.
const maxDocumentPartBytes = 128 << 20

// maxDocxZipEntries caps directory entries examined. A zip with millions
// of tiny entries is cheap to build and expensive to walk.
const maxDocxZipEntries = 10000

// DocumentParser extracts .docx text as markdown.
//
// Parsing rather than rasterizing is the whole point: a contract redline
// is structured text, and the structure (headings, tables, numbered
// clauses) is retrieval signal. A vision model reading a picture of a
// contract loses the heading hierarchy and can silently "correct" a
// clause number, which for a legal corpus is the worst available
// failure. encoding/xml costs nothing and cannot hallucinate.
type DocumentParser struct{}

// ModelID reports the mechanism, not a model. See SpreadsheetParser.
func (DocumentParser) ModelID() string { return DocumentMechanism }

// Extract reads the main document part and renders it as markdown.
//
// ctx is unused: pure CPU over bytes already in memory, bounded by the
// size guards above rather than by a deadline.
func (DocumentParser) Extract(
	_ context.Context, docFormat string, _ string, data []byte,
) (*Result, error) {
	if f := strings.ToLower(strings.TrimSpace(docFormat)); f != "docx" {
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: DocumentParser cannot read %q (OOXML only; legacy .doc is "+
				"OLE2/BIFF with no word/document.xml and is out of scope)", docFormat)}
	}
	return parseDocx(data)
}

func parseDocx(data []byte) (*Result, error) {
	if len(data) == 0 {
		return nil, &files.PermanentError{Underlying: errors.New("docs: empty document")}
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		// Not a readable ZIP: deterministic, so retrying is pure waste.
		return nil, &files.PermanentError{
			Underlying: fmt.Errorf("docs: open docx: %w", err)}
	}
	if len(zr.File) > maxDocxZipEntries {
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: docx has %d zip entries, over the %d cap",
			len(zr.File), maxDocxZipEntries)}
	}

	var part *zip.File
	for _, f := range zr.File {
		// Exact match only. A crafted archive can contain several
		// entries whose names merely end in the right suffix; the OOXML
		// main part has one canonical path.
		if f.Name == "word/document.xml" {
			part = f
			break
		}
	}
	if part == nil {
		return nil, &files.PermanentError{Underlying: errors.New(
			"docs: no word/document.xml (not a wordprocessing OOXML file)")}
	}
	// UncompressedSize64 is attacker-controlled metadata, so it is a
	// cheap pre-check, not the enforcement. The io.LimitReader below is
	// what actually bounds what we hold.
	if part.UncompressedSize64 > maxDocumentPartBytes {
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: word/document.xml declares %d bytes, over the %d cap",
			part.UncompressedSize64, maxDocumentPartBytes)}
	}
	rc, err := part.Open()
	if err != nil {
		return nil, &files.PermanentError{
			Underlying: fmt.Errorf("docs: open document part: %w", err)}
	}
	defer func() { _ = rc.Close() }()

	// +1 so a part that decompresses to exactly the cap is detectable as
	// over-limit rather than silently truncated at the boundary.
	limited := io.LimitReader(rc, maxDocumentPartBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		// A truncated or corrupt deflate stream is deterministic.
		return nil, &files.PermanentError{
			Underlying: fmt.Errorf("docs: read document part: %w", err)}
	}
	if int64(len(body)) > maxDocumentPartBytes {
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: word/document.xml exceeds the %d-byte cap (declared size lied)",
			int64(maxDocumentPartBytes))}
	}

	md, err := renderDocxBody(body)
	if err != nil {
		return nil, &files.PermanentError{Underlying: err}
	}
	// A Word file with no text is not an error: people paste screenshots
	// into one and send that. Its pictures are returned for the caption
	// queue, and the worker settles the document as having no text.
	return &Result{
		Markdown: md,
		Pictures: docxPictures(zr),
		// No pages. Pagination in a .docx is a rendering property of the
		// consuming application, not data in the file, so any page
		// number we emitted would be invented.
		PageCount: 0,
		ModelID:   DocumentMechanism,
	}, nil
}

// renderDocxBody streams word/document.xml and emits markdown.
//
// Streaming rather than unmarshalling into a struct tree is deliberate:
// WordprocessingML is deeply nested and a contract's body would build a
// large object graph for no benefit, since all we need is text in
// document order plus a few structural signals.
//
// What it understands, and why each earns its complexity:
//
//   - `w:p` paragraph boundaries — without these the whole document is
//     one run-on line and the splitter has nothing to cut on.
//   - `w:pStyle` heading levels — these become markdown headings, which
//     is precisely what the revised chunker splits on.
//   - `w:tbl` / `w:tr` / `w:tc` tables — a contract's payment terms and
//     an inventory's rows both live in tables.
//   - `w:tab` and `w:br` — flattened to spaces so a tabbed clause
//     doesn't collide into one token.
//
// Deliberately ignored: revision marks, comments, footnotes, headers and
// footers. A redline's `w:del` content is *removed* text; including it
// would put superseded terms in the index as if current, which for a
// contract is worse than omitting them.
func renderDocxBody(body []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var (
		out       strings.Builder
		para      strings.Builder
		style     string
		inDeleted int // depth of w:del we are inside; skip that text
		inTable   bool
		row       []string
		cell      strings.Builder
		inCell    bool
		rowIdx    int
	)

	flushPara := func() {
		text := collapseSpaces(para.String())
		para.Reset()
		s := style
		style = ""
		if text == "" {
			return
		}
		if lvl := headingLevel(s); lvl > 0 {
			// Cap at h4: deeper nesting in a contract is clause
			// numbering, not document structure, and h5/h6 would
			// fragment the chunker for no retrieval gain.
			if lvl > 4 {
				lvl = 4
			}
			fmt.Fprintf(&out, "\n%s %s\n\n", strings.Repeat("#", lvl), text)
			return
		}
		out.WriteString(text)
		out.WriteString("\n\n")
	}
	flushRow := func() {
		if len(row) == 0 {
			return
		}
		// Word documents use tables for page layout, so a real .docx is
		// full of rows whose every cell is empty — the grant agreement in
		// the corpus produced 5 of them. They render as `|  |  |`
		// skeletons, which embed to vectors matching nothing. Dropping
		// the row entirely is correct: unlike a spreadsheet, a
		// wordprocessing table row carries no positional meaning.
		allEmpty := true
		for _, c := range row {
			if strings.TrimSpace(c) != "" {
				allEmpty = false
				break
			}
		}
		if allEmpty {
			row = nil
			return
		}
		// Trailing empties are layout padding for the same reason.
		for len(row) > 1 && strings.TrimSpace(row[len(row)-1]) == "" {
			row = row[:len(row)-1]
		}
		fmt.Fprintf(&out, "| %s |\n", strings.Join(row, " | "))
		if rowIdx == 0 {
			sep := make([]string, len(row))
			for i := range sep {
				sep[i] = "---"
			}
			fmt.Fprintf(&out, "| %s |\n", strings.Join(sep, " | "))
		}
		rowIdx++
		row = nil
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docs: parse document.xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "del":
				inDeleted++
			case "p":
				if !inTable {
					para.Reset()
				}
			case "pStyle":
				for _, a := range t.Attr {
					if a.Name.Local == "val" {
						style = a.Value
					}
				}
			case "tbl":
				inTable = true
				rowIdx = 0
				out.WriteString("\n")
			case "tr":
				row = nil
			case "tc":
				inCell = true
				cell.Reset()
			case "tab":
				target := &para
				if inCell {
					target = &cell
				}
				target.WriteByte(' ')
			case "br":
				target := &para
				if inCell {
					target = &cell
				}
				target.WriteByte(' ')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "del":
				if inDeleted > 0 {
					inDeleted--
				}
			case "p":
				if !inTable {
					flushPara()
				} else if inCell {
					// Multi-paragraph cell: keep it one cell.
					cell.WriteByte(' ')
				}
			case "tc":
				row = append(row, escapeCell(collapseSpaces(cell.String())))
				cell.Reset()
				inCell = false
			case "tr":
				flushRow()
			case "tbl":
				inTable = false
				out.WriteString("\n")
			}
		case xml.CharData:
			if inDeleted > 0 {
				continue
			}
			if inCell {
				cell.Write(t)
			} else {
				para.Write(t)
			}
		}
	}
	flushPara()
	return strings.TrimSpace(collapseBlankLines(out.String())), nil
}

// headingLevel maps a WordprocessingML style name to a markdown heading
// level, or 0 for body text.
//
// Word style ids are localized and template-dependent ("Heading1",
// "heading 1", "Titre1"), so this matches on the numeric suffix after a
// recognized prefix rather than on an exact table of names.
func headingLevel(style string) int {
	s := strings.ToLower(strings.ReplaceAll(style, " ", ""))
	if s == "title" {
		return 1
	}
	if !strings.HasPrefix(s, "heading") {
		return 0
	}
	n := 0
	for _, r := range s[len("heading"):] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 9 {
			return 0
		}
	}
	return n
}

// collapseSpaces flattens runs of whitespace to single spaces and trims.
// OOXML splits a sentence across many w:t runs whenever formatting
// changes mid-sentence, so naive concatenation produces ragged spacing.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f', ' ':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		default:
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

// collapseBlankLines squeezes 3+ newlines to 2 so paragraph flushing
// around headings and tables doesn't leave large gaps.
func collapseBlankLines(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}
