package docs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalParsersAgainstRealCorpus runs the office parsers over a
// directory of real-world office files that the operator supplies.
//
// It exists because the unit tests in this package build their own
// workbooks with excelize, and a workbook excelize wrote has a shape that
// Word, Google Sheets, and Numbers do not: merged cells, used-ranges
// padded far past the data, styles from a template, sentences split
// across a dozen formatting runs. Bugs of that kind are not caught by a
// self-generated fixture.
//
// Skipped unless DIDEROT_DOC_CORPUS points at a directory containing
// office-manifest.json plus the files it names. Such a corpus is private
// documents, so none is committed.
//
//	Run: DIDEROT_DOC_CORPUS=/path/to/corpus go test \
//		./internal/attachments/docs/ -run Corpus -v
func TestLocalParsersAgainstRealCorpus(t *testing.T) {
	dir := os.Getenv("DIDEROT_DOC_CORPUS")
	if dir == "" {
		t.Skip("DIDEROT_DOC_CORPUS unset; see the doc comment")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "office-manifest.json"))
	if err != nil {
		t.Skipf("no office-manifest.json in %s: %v", dir, err)
	}
	var manifest []struct {
		FileID   string `json:"file_id"`
		Path     string `json:"path"`
		Name     string `json:"name"`
		Mimetype string `json:"mimetype"`
		Bytes    int    `json:"bytes"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) == 0 {
		t.Fatal("empty manifest")
	}

	for _, m := range manifest {
		t.Run(m.FileID, func(t *testing.T) {
			data, err := os.ReadFile(m.Path)
			if err != nil {
				t.Skipf("missing file: %v", err)
			}
			// Route exactly the way production does: from the mimetype,
			// with the filename only as a fallback. One corpus file is a
			// .docx served under a .pdf name, so trusting the name would
			// invoke the wrong parser — which is the point of testing the
			// router here rather than hardcoding a format.
			format := FormatForMediaType(m.Mimetype, m.Name)
			var ex Extractor
			switch format {
			case "xlsx":
				ex = SpreadsheetParser{}
			case "docx":
				ex = DocumentParser{}
			default:
				t.Fatalf("no local parser for format %q (mimetype %q, name %q)",
					format, m.Mimetype, m.Name)
			}

			res, err := ex.Extract(context.Background(), format, m.Name, data)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			chunks := Split(res.Markdown)
			runes := len([]rune(res.Markdown))
			// Dumping the extraction is what makes this comparable to a
			// model arm's output for the same file, which is the whole
			// point of having a real corpus.
			if dump := os.Getenv("DIDEROT_DOC_CORPUS_DUMP"); dump != "" {
				if err := os.MkdirAll(dump, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					filepath.Join(dump, m.FileID+".md"),
					[]byte(res.Markdown), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("%s %s: %d runes, %d chunks, %d table rows, truncated=%v",
				format, m.Name, runes, len(chunks),
				strings.Count(res.Markdown, "\n|"), res.Truncated)

			// A real document must yield real text. Confident emptiness
			// is the failure mode that looks like success downstream: the
			// row gets stamped extracted, and nobody looks again.
			if runes < 200 {
				t.Errorf("only %d runes from a %d-byte file; near-empty "+
					"extraction reads as success downstream:\n%s",
					runes, m.Bytes, res.Markdown)
			}
			if len(chunks) == 0 {
				t.Error("no chunks produced")
			}
			for i, c := range chunks {
				if strings.TrimSpace(c.Content) == "" {
					t.Errorf("chunk %d is empty", i)
				}
			}
			// The empty-grid regression: real workbooks have used-ranges
			// far past their data, and a naive render emits pipe
			// skeletons that embed to vectors matching nothing.
			//
			// The assertion is on ENTIRELY-empty rows, not on any
			// occurrence of "|  |  |". Interior empty cells are kept
			// deliberately: in a grid they carry position (a blank cell
			// in column 3 means column 3 has no value for that row), and
			// a wide workbook with sparse rows has many of them. Fully
			// empty rows are the defect: Word uses tables for page
			// layout, and a naive render emits them as rows.
			for i, line := range strings.Split(res.Markdown, "\n") {
				row := strings.TrimSpace(line)
				if !strings.HasPrefix(row, "|") {
					continue
				}
				if strings.Trim(row, "| \t") == "" {
					t.Errorf("line %d is an entirely-empty table row: %q", i, row)
					break
				}
			}
			if format == "xlsx" {
				// Sheet names are the provenance the model path lost.
				if !strings.Contains(res.Markdown, "## ") {
					t.Error("no sheet headings; per-sheet provenance lost")
				}
				for _, c := range chunks {
					if c.Section == "" {
						t.Errorf("a chunk has no sheet name: %q",
							firstLine(c.Content))
					}
				}
			}
		})
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
