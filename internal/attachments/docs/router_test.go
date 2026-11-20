package docs

import (
	"slices"
	"testing"
)

// TestRouterRoutesByFormat pins the central routing decision: a format
// with recoverable structure gets a parser, and only PDFs reach a model.
// Getting this wrong is not a style issue — routing a spreadsheet to a
// rasterizing model is measured to lose the sheet names and return sparse
// `| | | |` grids.
func TestRouterRoutesByFormat(t *testing.T) {
	model := &fakeExtractor{markdown: "x"}
	r := &Router{PDF: model}
	for _, tc := range []struct {
		format string
		want   string
	}{
		{"xlsx", SpreadsheetMechanism},
		{"XLSX", SpreadsheetMechanism}, // case must not defeat routing
		{" docx ", DocumentMechanism},
		{"pdf", model.ModelID()},
	} {
		t.Run(tc.format, func(t *testing.T) {
			ex, err := r.For(tc.format)
			if err != nil {
				t.Fatalf("For(%q): %v", tc.format, err)
			}
			if got := ex.ModelID(); got != tc.want {
				t.Errorf("For(%q) -> %q, want %q", tc.format, got, tc.want)
			}
		})
	}
}

// TestRouterRejectsUnknownFormats: an unknown format must NOT fall back
// to the model. That fallback is exactly how text/* ended up being read
// by a frontier vision model — the router disagreeing with the
// connector's allowlist is a bug in one of them, and it should surface as
// a dead-lettered row rather than as a bill.
func TestRouterRejectsUnknownFormats(t *testing.T) {
	r := &Router{PDF: &fakeExtractor{markdown: "x"}}
	for _, f := range []string{"csv", "txt", "md", "html", "doc", "xls", "pptx", "", "  "} {
		if _, err := r.For(f); err == nil {
			t.Errorf("For(%q) routed an out-of-scope format", f)
		} else if !isPermanent(err) {
			t.Errorf("For(%q) gave a retryable error: %v", f, err)
		}
	}
}

// TestRouterWithoutPDFStillParsesOffice is the degraded-mode contract
// cmd/diderot depends on: a PDF arm that fails to construct (the WASM pool
// failing to initialize) must not take office-document ingest down with
// it, because those formats never needed it.
func TestRouterWithoutPDFStillParsesOffice(t *testing.T) {
	r := &Router{} // no pdf arm
	for _, f := range []string{"xlsx", "docx"} {
		if _, err := r.For(f); err != nil {
			t.Errorf("For(%q) failed with no pdf parser configured: %v", f, err)
		}
	}
	if _, err := r.For("pdf"); err == nil {
		t.Error("For(pdf) must fail with no pdf parser rather than nil-deref")
	} else if !isPermanent(err) {
		t.Errorf("For(pdf) gave a retryable error: %v", err)
	}
}

// TestRouterMechanismsReportsRealIDs: the boot log is how an operator
// learns which mechanism will run, so it must not claim a model handles
// formats that are parsed locally.
func TestRouterMechanismsReportsRealIDs(t *testing.T) {
	r := &Router{PDF: &fakeExtractor{markdown: "x"}}
	m := r.Mechanisms()
	if m["xlsx"] != SpreadsheetMechanism || m["docx"] != DocumentMechanism {
		t.Errorf("office formats misreported: %+v", m)
	}
	if m["pdf"] == SpreadsheetMechanism {
		t.Errorf("pdf attributed to the spreadsheet parser: %+v", m)
	}

	bare := (&Router{}).Mechanisms()
	if bare["pdf"] == "" {
		t.Error("unrouted pdf must be reported explicitly, not as an empty string")
	}
	if _, ok := bare["pdf"]; !ok {
		t.Error("pdf key missing entirely from the boot log's mechanism map")
	}
}

// TestRouterClaimFormatsBlocksPDFWithoutSparsePageModel pins the deployment
// safety contract: a missing credential must leave PDFs pending, not turn a
// temporary configuration problem into partial or stranded corpus state.
func TestRouterClaimFormatsBlocksPDFWithoutSparsePageModel(t *testing.T) {
	localOnlyPDF := &PDFParser{}
	r := &Router{PDF: localOnlyPDF}
	if got, want := r.ClaimFormats(), []string{"xlsx", "docx"}; !slices.Equal(got, want) {
		t.Fatalf("ClaimFormats without model = %v, want %v", got, want)
	}
	if got := r.Mechanisms()["pdf"]; got != "(blocked: sparse-page model not configured)" {
		t.Fatalf("pdf mechanism = %q, want blocked", got)
	}

	configured := &Router{PDF: &PDFParser{model: &fakeExtractor{markdown: "x"}}}
	if got := configured.ClaimFormats(); got != nil {
		t.Fatalf("ClaimFormats with model = %v, want nil (all formats)", got)
	}

	// Test/alternate PDF arms are complete extractors and do not need to expose
	// the production PDFParser's sparse-page capability method.
	alternate := &Router{PDF: &fakeExtractor{markdown: "x"}}
	if got := alternate.ClaimFormats(); got != nil {
		t.Fatalf("ClaimFormats with alternate PDF arm = %v, want nil", got)
	}
}
