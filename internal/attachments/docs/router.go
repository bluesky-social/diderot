package docs

import (
	"fmt"
	"strings"

	"github.com/bluesky-social/diderot/internal/attachments/files"
)

// Router picks the extraction mechanism for a document format.
//
// It exists so the choice lives in exactly one place. The alternative —
// a switch inside the worker — puts format knowledge in two files and
// invites the failure this design already made once: a single convenient
// mechanism quietly handling every format, which is how spreadsheets
// ended up being read by a rasterizing vision model that returned sparse
// `| | | |` grids with the sheet names lost.
//
// The routing principle: **a format with recoverable structure gets a
// parser; only formats that are genuinely pictures of text get a model.**
// Parsers are free, cannot hallucinate an identifier, and preserve the
// structure that makes the content retrievable.
type Router struct {
	// PDF is the pdfium text-layer arm (*PDFParser outside tests). Nil
	// makes PDFs unroutable — there is no fallback to sending the whole
	// PDF to a model, because the arm handles the sparse-page case itself
	// and a whole-document model call is the costly path this arm exists to avoid.
	//
	// Typed as the interface rather than *PDFParser so a test can route
	// PDFs without standing up a WASM pool; the arm's own behavior is
	// tested directly in pdf_test.go.
	PDF Extractor

	spreadsheet SpreadsheetParser
	document    DocumentParser
}

// ClaimFormats returns the formats the worker may safely claim right now.
// A nil slice means every queued format, preserving the normal behavior where
// the router itself rejects an unexpected format loudly. When the PDF arm is
// absent or lacks its sparse-page model, only the fully local office formats
// are claimable: PDFs stay pending until a correctly configured process starts.
func (r *Router) ClaimFormats() []string {
	if r.pdfReady() {
		return nil
	}
	return []string{"xlsx", "docx"}
}

func (r *Router) pdfReady() bool {
	if r == nil || r.PDF == nil {
		return false
	}
	if readiness, ok := r.PDF.(interface{ SparsePageModelConfigured() bool }); ok {
		return readiness.SparsePageModelConfigured()
	}
	// Alternate/test PDF extractors are complete arms in their own right.
	return true
}

// For returns the Extractor for a format.
//
// The error is a PermanentError: an unroutable format will still be
// unroutable on a retry, so the row should settle immediately.
func (r *Router) For(docFormat string) (Extractor, error) {
	switch strings.ToLower(strings.TrimSpace(docFormat)) {
	case "xlsx":
		return r.spreadsheet, nil
	case "docx":
		return r.document, nil
	case "pdf":
		// pdfium's text layer, per page, with only the pages too sparse to
		// trust reaching a model — the arm makes that call internally, so
		// the router's job here is just "PDFs go to pdfium."
		//
		// Nil is a real deployment state (the WASM pool failed to
		// initialize). The production worker's claim filter keeps PDFs
		// pending in that state; this remains a defensive error for direct
		// callers.
		if r.PDF == nil {
			return nil, &files.PermanentError{Underlying: fmt.Errorf(
				"docs: no pdf parser configured; %q is unroutable", docFormat)}
		}
		return r.PDF, nil
	default:
		// Not a fallback to the model. Reaching here means the
		// connector's allowlist and this router disagree, which is a bug
		// in one of them — and sending an unknown format to a model
		// "just in case" is how text/* got routed to a vision model.
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: no extraction mechanism for format %q", docFormat)}
	}
}

// Mechanisms reports the mechanism id per routable format, for logging at
// boot so an operator can see what will actually run.
func (r *Router) Mechanisms() map[string]string {
	out := map[string]string{
		"xlsx": r.spreadsheet.ModelID(),
		"docx": r.document.ModelID(),
	}
	if r.PDF != nil && r.pdfReady() {
		out["pdf"] = r.PDF.ModelID()
	} else if r.PDF != nil {
		out["pdf"] = "(blocked: sparse-page model not configured)"
	} else {
		out["pdf"] = "(unrouted: no pdf parser configured)"
	}
	return out
}

// compile-time proof that both local parsers satisfy the seam. Without
// this a signature drift would only surface where the router is used.
var (
	_ Extractor = SpreadsheetParser{}
	_ Extractor = DocumentParser{}
)
