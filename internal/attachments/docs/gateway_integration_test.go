//go:build gateway

package docs

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// TestGatewayExtractorLive sends one PDF page to a real model provider. It
// is built only with the gateway tag and skips unless the provider and the
// model are named in the environment, so it never runs in CI or a plain
// `go test ./...`:
//
//	DIDEROT_TEST_PROVIDER_URL     base URL of an OpenAI-compatible provider or gateway, with any /v1
//	DIDEROT_TEST_PROVIDER_KEY     its API key
//	DIDEROT_TEST_MODEL_DOCUMENTS  a model that reads PDF files on chat completions
//
//	go test -tags=gateway -v -count=1 ./internal/attachments/docs -run TestGatewayExtractorLive
//
// This is the one test in the package that can catch a real transport
// regression rather than a mocked one: an httptest.Server only returns
// what this file tells it to, so it can never discover that the wire
// format stopped delivering the document. A gateway can accept the
// request, answer 200, and drop the file part in translation; running
// this test is how you find that.
func TestGatewayExtractorLive(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("DIDEROT_TEST_PROVIDER_URL"))
	apiKey := strings.TrimSpace(os.Getenv("DIDEROT_TEST_PROVIDER_KEY"))
	modelID := strings.TrimSpace(os.Getenv("DIDEROT_TEST_MODEL_DOCUMENTS"))
	if baseURL == "" || apiKey == "" || modelID == "" {
		t.Skip("set DIDEROT_TEST_PROVIDER_URL, DIDEROT_TEST_PROVIDER_KEY and DIDEROT_TEST_MODEL_DOCUMENTS to run")
	}

	e, err := NewGatewayExtractor(GatewayExtractorConfig{
		BaseURL: baseURL, APIKey: apiKey, ModelID: modelID,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewGatewayExtractor: %v", err)
	}

	// A tiny, self-contained one-page PDF (no external fixture needed)
	// that draws pdfSentinel on its single page. This exercises the real
	// wire format end to end; it measures nothing about extraction
	// quality.
	pdf := []byte(minimalPDF)

	res, err := e.Extract(t.Context(), "pdf", "live-gateway-smoke-test.pdf", pdf)
	if err != nil {
		t.Fatalf("Extract against live gateway: %v", err)
	}
	// THE assertion. Everything else here can pass against a response
	// that never saw the document: a page marker is something the prompt
	// asks for and a polite denial can carry, and InputTokens is nonzero
	// even for a ~20-token denial. Only text that exists nowhere but
	// inside the PDF proves the bytes arrived.
	//
	// A token-count canary (input_tokens below some threshold) cannot do
	// this job: it depends on document size, and this very PDF legitimately
	// reports only a few hundred prompt tokens.
	//
	// This is the only assertion in the package that can tell "the
	// transport delivered the document" from "the model produced plausible
	// text," and that distinction is a property of the wire format.
	if !strings.Contains(res.Markdown, pdfSentinel) {
		t.Errorf("response does not contain the PDF's own text %q — the document may not have reached the model.\n"+
			"input_tokens=%d (an implausibly small count means the document part was dropped in translation)\ngot: %q",
			pdfSentinel, res.InputTokens, res.Markdown)
	}
	if res.PageCount == 0 {
		t.Errorf("PageCount = 0, want at least 1 page marker in the response")
	}
	if res.InputTokens == 0 {
		t.Error("InputTokens = 0, want a nonzero usage figure from the gateway")
	}
}

// pdfSentinel is the text drawn on minimalPDF's single page. It is what
// makes this test meaningful: it can only appear in the model's output if
// the document bytes actually reached the model.
const pdfSentinel = "Live gateway smoke test"

// minimalPDF is a syntactically valid, minimal one-page PDF that draws
// pdfSentinel. Good enough to prove the file part reaches the model — it
// is not a test of extraction quality.
//
// /Length is computed to match the content stream exactly. Real parsers
// are lenient about a wrong value (measured: the gateway transcribed this
// page correctly even when /Length understated the stream by 11 bytes),
// which is precisely why it is worth getting right — a malformed fixture
// that happens to work makes this test prove less than it appears to,
// and the day a parser stops being lenient the failure would look like a
// transport regression.
const minimalPDF = `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>
endobj
4 0 obj
<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>
endobj
5 0 obj
<< /Length 55 >>
stream
BT /F1 24 Tf 100 700 Td (Live gateway smoke test) Tj ET
endstream
endobj
xref
0 6
0000000000 65535 f
trailer
<< /Size 6 /Root 1 0 R >>
startxref
0
%%EOF
`
