package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/responses"

	"github.com/bluesky-social/diderot/internal/attachments/files"
)

// sharedPDFParser keeps one WASM pool for the whole package's tests.
//
// Module compilation is the expensive part (~5 MB of WASM), and it happens
// once per pool. Standing a pool up per test made the package's runtime
// dominated by pdfium init.
var (
	sharedPDFOnce sync.Once
	sharedPDF     *PDFParser
	sharedPDFErr  error
)

// testPDFParser returns a parser with the given model wired in, reusing
// the shared pool. The model is per-test, so it is swapped rather than
// baked into the shared instance.
func testPDFParser(t *testing.T, model Extractor) *PDFParser {
	t.Helper()
	sharedPDFOnce.Do(func() {
		sharedPDF, sharedPDFErr = NewPDFParser(PDFParserConfig{})
	})
	if sharedPDFErr != nil {
		t.Fatalf("NewPDFParser: %v", sharedPDFErr)
	}
	return &PDFParser{pool: sharedPDF.pool, model: model, logger: sharedPDF.logger}
}

// ---------------------------------------------------------------------
// The sandbox. This is the most important test in this file.
// ---------------------------------------------------------------------

// TestPDFiumGuestHasNoFilesystem pins the one place this arm deliberately
// diverges from the harness it was ported from.
//
// webassembly.Config.FSConfig defaults to nil, and initWithConfig then
// mounts the host's "/" into the guest (webassembly.go:111 — the project
// README says so plainly: "by default it exposes the whole disk"). The
// proving harness used that default, so pdfium ran with the entire host
// filesystem visible. Nothing about the extracted text would look
// different if that regressed, which is exactly why it needs a test.
//
// The assertion is behavioral rather than a field check: OpenDocument's
// FilePath branch calls FPDF_LoadDocument INSIDE the guest, which reaches
// the host only through the WASI mount. So loading a perfectly good PDF
// by path must FAIL, while loading the same bytes must SUCCEED. The
// second half is the test's own negative control — without it, a
// mysteriously-broken pool would pass the first half.
func TestPDFiumGuestHasNoFilesystem(t *testing.T) {
	pdf := buildTestPDF(t, []string{"Sandbox probe page."})

	dir := t.TempDir()
	path := filepath.Join(dir, "probe.pdf")
	if err := os.WriteFile(path, pdf, 0o600); err != nil {
		t.Fatal(err)
	}

	p := testPDFParser(t, nil)
	// A deadline, because acquireInstance blocks on an exhausted pool for
	// as long as its context allows.
	ctx, cancel := context.WithTimeout(context.Background(), pdfLayerTimeout)
	defer cancel()
	inst, err := p.acquireInstance(ctx)
	if err != nil {
		t.Fatalf("acquireInstance: %v", err)
	}
	defer func() { _ = inst.Close() }()

	// Control: the bytes path works, so a failure below is the sandbox
	// and not a malformed fixture.
	byDoc, err := inst.OpenDocument(&requests.OpenDocument{File: &pdf})
	if err != nil {
		t.Fatalf("OpenDocument by bytes failed; the fixture or the pool is "+
			"broken and the path assertion below would prove nothing: %v", err)
	}
	_, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: byDoc.Document})

	if _, err := inst.OpenDocument(&requests.OpenDocument{FilePath: &path}); err == nil {
		t.Fatalf("pdfium opened %s from inside the guest — the host filesystem "+
			"is mounted into the WASM sandbox. pdfiumPoolConfig must pass an "+
			"explicit empty FSConfig; a nil one mounts \"/\".", path)
	}
}

// TestPDFiumPoolConfigSetsFSConfig is the cheap companion to the
// behavioral test above: it catches the regression at the line that
// causes it, so a failure names the fix rather than the symptom.
func TestPDFiumPoolConfigSetsFSConfig(t *testing.T) {
	cfg := pdfiumPoolConfig(2)
	if cfg.FSConfig == nil {
		t.Error("pdfiumPoolConfig left FSConfig nil, which makes " +
			"webassembly.Init mount the host root into the guest")
	}
	if cfg.RuntimeConfig == nil {
		t.Error("pdfiumPoolConfig left RuntimeConfig nil, dropping both the " +
			"memory limit and WithCloseOnContextDone (which is what makes " +
			"pdfLayerTimeout enforceable)")
	}
	if cfg.ReuseWorkers {
		t.Error("ReuseWorkers must stay false: an invalidated instance's " +
			"guest memory is released, a returned one's is retained for the " +
			"life of the process")
	}
}

// TestPDFiumWASMBlobIsPinned detects a silent swap of the pdfium binary.
//
// This pins the PDFium 8165 module distributed with go-pdfium v1.21.5. The
// module has no producers section, so it doesn't record its Emscripten
// version. The audit behind the pin checked that Chrome's font substitution
// tables and Emscripten stack exports are present, that the module's go.sum
// hash matches sum.golang.org, that its imports (env and
// wasi_snapshot_preview1) and exports are the same as the previously audited
// PDFium 8111 module's, so it reaches no new host functions, and that the
// sandbox hardening tests pass. A future upgrade must not silently replace
// these bytes without another review.
func TestPDFiumWASMBlobIsPinned(t *testing.T) {
	const (
		wantSHA256 = "9113dda8d1a981133d2042837fe4692420ab362c1aa633a9a09c76c9b3f6820a"
		wantBytes  = 6001244
	)
	// The module's own copy, the one the build compiles in: `go list -m`
	// resolves it as the build would (a vendor/ tree included, were one
	// restored), and the module cache verifies it against go.sum.
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/klippa-app/go-pdfium").Output()
	if err != nil {
		t.Fatalf("locate the go-pdfium module: %v", err)
	}
	path := filepath.Join(strings.TrimSpace(string(out)), "internal", "pdfium_wasm", "pdfium.wasm")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("cannot read the pdfium blob at %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		t.Errorf("pdfium.wasm sha256 = %s, want %s — the pinned artifact "+
			"was replaced. It is not reproducible from source, so "+
			"re-audit before accepting the new bytes.", got, wantSHA256)
	}
	if n != wantBytes {
		t.Errorf("pdfium.wasm is %d bytes, want %d", n, wantBytes)
	}
}

// ---------------------------------------------------------------------
// Routing.
// ---------------------------------------------------------------------

// TestPDFDensityRoutingThreshold drives the decision directly with pages
// either side of the line. The threshold is not a tuning knob: in a
// measured document corpus about 9% of pages fall below 400 characters
// while the median page carries about 1,800, so moving it moves spend by
// an order of magnitude in one direction or loses scanned pages in the
// other.
func TestPDFDensityRoutingThreshold(t *testing.T) {
	for _, tc := range []struct {
		name       string
		chars      int
		wantSparse bool
	}{
		{"empty page", 0, true},
		{"browser chrome only", 163, true},
		{"one under", pdfDensityThreshold - 1, true},
		{"exactly at the line", pdfDensityThreshold, false},
		{"one over", pdfDensityThreshold + 1, false},
		{"median real page", 1783, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pg := pdfPage{number: 1, chars: tc.chars}
			if got := pg.sparse(); got != tc.wantSparse {
				t.Errorf("page with %d chars: sparse = %v, want %v",
					tc.chars, got, tc.wantSparse)
			}
		})
	}
}

// TestAssemblePagesOrdersByPageNumber: content must land at its own page
// number regardless of which mechanism produced it, and holes must not
// renumber what follows them.
func TestAssemblePagesOrdersByPageNumber(t *testing.T) {
	md := assemblePages(map[int]string{
		3: "third page",
		1: "first page",
		5: "fifth page",
	}, 5)
	want := "--- PAGE 1 ---\nfirst page\n\n" +
		"--- PAGE 3 ---\nthird page\n\n" +
		"--- PAGE 5 ---\nfifth page"
	if md != want {
		t.Errorf("assembled:\n%q\nwant:\n%q", md, want)
	}
	// And the splitter must agree about which page each block is.
	for _, c := range Split(md) {
		if !strings.Contains(c.Content, fmt.Sprintf("%s page", ordinal(c.Page))) {
			t.Errorf("chunk labeled page %d carries %q", c.Page, c.Content)
		}
	}
}

func ordinal(n int) string {
	switch n {
	case 1:
		return "first"
	case 3:
		return "third"
	case 5:
		return "fifth"
	}
	return "?"
}

// ---------------------------------------------------------------------
// End to end, against real pdfium.
// ---------------------------------------------------------------------

// A born-digital PDF uses its text layer without calling the model.
func TestPDFParserLayerOnlyDocument(t *testing.T) {
	pdf := buildTestPDF(t, []string{densePage("Retention is eighteen months."),
		densePage("Contact legal-ops for disclosure requests.")})
	model := &fakeExtractor{markdown: "--- PAGE 1 ---\nmodel output"}
	p := testPDFParser(t, model)

	res, err := p.Extract(context.Background(), "pdf", "policy.pdf", pdf)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if model.calls != 0 {
		t.Errorf("model called %d times for a document whose every page has a "+
			"text layer; ~90%% of real pages must not reach a model", model.calls)
	}
	if res.ModelID != PDFMechanism {
		t.Errorf("ModelID = %q, want %q", res.ModelID, PDFMechanism)
	}
	if !isLocalMechanism(res.ModelID) {
		t.Errorf("%q must read as a local mechanism", res.ModelID)
	}
	if res.PageCount != 2 || res.PagesExtracted != 2 {
		t.Errorf("PageCount/PagesExtracted = %d/%d, want 2/2",
			res.PageCount, res.PagesExtracted)
	}
	if !strings.Contains(res.Markdown, "Retention is eighteen months") {
		t.Errorf("page 1 text missing:\n%s", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "--- PAGE 2 ---") {
		t.Errorf("page 2 marker missing:\n%s", res.Markdown)
	}
}

// TestPDFParserMixedDocumentPageOrder is the assembly contract: a
// document read partly by the parser and partly by a model must come out
// in page order with each page's content at its own page number.
func TestPDFParserMixedDocumentPageOrder(t *testing.T) {
	// Pages 2 and 4 are sparse and non-adjacent, so they become two
	// separate one-page model calls in ascending page order.
	pdf := buildTestPDF(t, []string{
		densePage("Alpha clause."),
		"scan",
		densePage("Gamma clause."),
		"scan",
		densePage("Epsilon clause."),
	})
	model := &sequenceExtractor{}
	p := testPDFParser(t, model)

	res, err := p.Extract(context.Background(), "pdf", "mixed.pdf", pdf)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if model.calls != 2 {
		t.Fatalf("model called %d times, want 2 (pages 2 and 4, separately — "+
			"they are not contiguous)", model.calls)
	}
	if res.PageCount != 5 || res.PagesExtracted != 5 {
		t.Errorf("PageCount/PagesExtracted = %d/%d, want 5/5",
			res.PageCount, res.PagesExtracted)
	}
	if res.ModelID != pdfMixedMechanismPrefix+"fake-model" {
		t.Errorf("ModelID = %q, want %q — a mixed document must be honest "+
			"about having made a billed call", res.ModelID,
			pdfMixedMechanismPrefix+"fake-model")
	}
	if isLocalMechanism(res.ModelID) {
		t.Error("a mixed document must not read as local")
	}

	// The order assertion: chunk N must carry page N's content.
	byPage := map[int]string{}
	for _, c := range Split(res.Markdown) {
		byPage[c.Page] += c.Content
	}
	for page, want := range map[int]string{
		1: "Alpha clause",
		2: "MODEL-CALL-1",
		3: "Gamma clause",
		4: "MODEL-CALL-2",
		5: "Epsilon clause",
	} {
		if !strings.Contains(byPage[page], want) {
			t.Errorf("page %d carries %q, want it to contain %q",
				page, byPage[page], want)
		}
	}
	// Markers must be strictly ascending in the assembled document, or a
	// downstream citation points at the wrong page.
	last := 0
	for _, line := range strings.Split(res.Markdown, "\n") {
		if n, ok := parsePageMarker(line); ok {
			if n <= last {
				t.Errorf("page markers out of order: %d after %d", n, last)
			}
			last = n
		}
	}
}

func TestPDFParserCallsModelOncePerSparsePage(t *testing.T) {
	pdf := buildTestPDF(t, []string{
		densePage("Cover text that is plainly long enough to stand alone."),
		"scan", "scan",
		densePage("Closing text that is also plainly long enough alone."),
	})
	model := &sequenceExtractor{}
	p := testPDFParser(t, model)

	res, err := p.Extract(context.Background(), "pdf", "run.pdf", pdf)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if model.calls != 2 {
		t.Errorf("model called %d times, want one call for each sparse page", model.calls)
	}
	byPage := map[int]string{}
	for _, c := range Split(res.Markdown) {
		byPage[c.Page] += c.Content
	}
	if !strings.Contains(byPage[2], "MODEL-CALL-1") {
		t.Errorf("page 2 = %q, want first model call", byPage[2])
	}
	if !strings.Contains(byPage[3], "MODEL-CALL-2") {
		t.Errorf("page 3 = %q, want second model call", byPage[3])
	}
}

// TestSlicePagesRoundTrip: the sub-PDF handed to the model must be a real
// PDF holding exactly the requested pages, in order.
//
// This is the piece with no cheap failure signal at runtime. A
// malformed slice does not error locally — it becomes a gateway 400 (or
// worse, a plausible transcription of nothing), and the only symptom is
// sparse pages that never come back. Reading the slice back through
// pdfium is the assertion the pipeline itself cannot make.
func TestSlicePageRoundTrip(t *testing.T) {
	pdf := buildTestPDF(t, []string{
		densePage("Alpha unmistakable."),
		densePage("Bravo unmistakable."),
		densePage("Charlie unmistakable."),
		densePage("Delta unmistakable."),
		densePage("Echo unmistakable."),
	})
	p := testPDFParser(t, nil)
	ctx := context.Background()

	sub, err := p.slicePage(ctx, pdf, pdfPage{number: 2})
	if err != nil {
		t.Fatalf("slicePage: %v", err)
	}
	pages, err := p.readLayer(ctx, sub)
	if err != nil {
		t.Fatalf("the sliced sub-PDF is not readable, so the model would get "+
			"bytes the gateway rejects: %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("sub-PDF has %d pages, want 1", len(pages))
	}
	if !strings.Contains(pages[0].text, "Bravo") {
		t.Errorf("sub-page 1 should be the original's page 2: %q",
			truncateRunes(pages[0].text, 60))
	}
	for _, unwanted := range []string{"Alpha", "Charlie", "Delta", "Echo"} {
		for _, pg := range pages {
			if strings.Contains(pg.text, unwanted) {
				t.Errorf("sub-PDF leaked %q, so the model is being paid to "+
					"re-read a page the parser already handled", unwanted)
			}
		}
	}
}

// A missing model still returns usable text-layer pages and reports the gap.
func TestPDFParserWithoutModelDegrades(t *testing.T) {
	pdf := buildTestPDF(t, []string{
		densePage("This page has a perfectly good text layer."),
		"scan",
	})
	p := testPDFParser(t, nil)

	res, err := p.Extract(context.Background(), "pdf", "half.pdf", pdf)
	if err != nil {
		t.Fatalf("Extract with no model must degrade, not fail: %v", err)
	}
	if res.PageCount != 2 {
		t.Errorf("PageCount = %d, want 2", res.PageCount)
	}
	if res.PagesExtracted != 1 {
		t.Errorf("PagesExtracted = %d, want 1 — the sparse page never reached "+
			"a model and must not be counted as extracted", res.PagesExtracted)
	}
	if res.ModelID != PDFMechanism {
		t.Errorf("ModelID = %q, want %q", res.ModelID, PDFMechanism)
	}
	if !strings.Contains(res.Markdown, "perfectly good text layer") {
		t.Errorf("layer page lost:\n%s", res.Markdown)
	}
}

// A transient model failure leaves the document pending for retry.
func TestPDFParserTransientModelErrorFailsWholeDocument(t *testing.T) {
	pdf := buildTestPDF(t, []string{densePage("Readable page."), "scan"})
	model := &fakeExtractor{err: &files.TransientError{
		Underlying: errors.New("gateway returned HTTP 429")}}
	p := testPDFParser(t, model)

	_, err := p.Extract(context.Background(), "pdf", "throttled.pdf", pdf)
	if err == nil {
		t.Fatal("a transient model failure must fail the document so it retries")
	}
	if !files.IsTransient(err) {
		t.Errorf("error lost its transient classification and would bump "+
			"failure_count: %v", err)
	}
}

// TestPDFParserPermanentModelErrorCommitsPartial: the gateway rejecting
// one sub-PDF is deterministic, so retrying is guaranteed waste. Keep the
// rest of the document and let pages_extracted report the gap.
func TestPDFParserPermanentModelErrorCommitsPartial(t *testing.T) {
	pdf := buildTestPDF(t, []string{densePage("Readable page."), "scan"})
	model := &fakeExtractor{err: &files.PermanentError{
		Underlying: errors.New("gateway rejected the document with HTTP 400")}}
	p := testPDFParser(t, model)

	res, err := p.Extract(context.Background(), "pdf", "rejected.pdf", pdf)
	if err != nil {
		t.Fatalf("a permanently-rejected page must not fail the document: %v", err)
	}
	if res.PagesExtracted != 1 || res.PageCount != 2 {
		t.Errorf("PagesExtracted/PageCount = %d/%d, want 1/2",
			res.PagesExtracted, res.PageCount)
	}
	if !strings.Contains(res.Markdown, "Readable page") {
		t.Errorf("layer page lost when the model page failed:\n%s", res.Markdown)
	}
}

// TestPDFParserRejectsNonPDF / oversized / empty: the guards that keep a
// misrouted document from reaching pdfium at all.
func TestPDFParserInputGuards(t *testing.T) {
	p := testPDFParser(t, nil)
	ctx := context.Background()

	if _, err := p.Extract(ctx, "xlsx", "book.xlsx", []byte("PK\x03\x04")); err == nil {
		t.Error("PDFParser accepted a non-pdf format")
	} else if !isPermanent(err) {
		t.Errorf("wrong format gave a retryable error: %v", err)
	}
	if _, err := p.Extract(ctx, "pdf", "empty.pdf", nil); err == nil {
		t.Error("PDFParser accepted zero bytes")
	}
	if _, err := p.Extract(ctx, "pdf", "big.pdf", make([]byte, MaxDocumentBytes+1)); err == nil {
		t.Error("PDFParser accepted an oversized document")
	} else if !isPermanent(err) {
		t.Errorf("oversized document gave a retryable error: %v", err)
	}
}

// TestPDFParserGarbageIsPermanent: bytes that are not a PDF must
// dead-letter rather than retry, and must not panic out of the binding
// layer into the worker goroutine.
func TestPDFParserGarbageIsPermanent(t *testing.T) {
	p := testPDFParser(t, nil)
	junk := []byte("%PDF-1.4\nthis is not a pdf at all, not even close\n%%EOF\n")
	_, err := p.Extract(context.Background(), "pdf", "junk.pdf", junk)
	if err == nil {
		t.Fatal("garbage bytes extracted successfully")
	}
	if !isPermanent(err) {
		t.Errorf("unparseable bytes gave a retryable error, which burns five "+
			"attempts on a guaranteed re-failure: %v", err)
	}
}

// TestPDFCancelledContextIsTransient: leadership loss or pod shutdown
// cancels the context mid-document. That is not the document's fault and
// must not bump its failure count.
func TestPDFCancelledContextIsTransient(t *testing.T) {
	pdf := buildTestPDF(t, []string{densePage("Some text.")})
	p := testPDFParser(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Extract(ctx, "pdf", "cancelled.pdf", pdf); err == nil {
		t.Fatal("extraction succeeded against a cancelled context")
	} else if !files.IsTransient(err) {
		t.Errorf("cancellation must classify transient, got: %v", err)
	}
}

type pdfCancellationPool struct {
	pdfium.Pool
	instance pdfium.Pdfium
}

func (pool pdfCancellationPool) GetInstanceWithContext(context.Context) (pdfium.Pdfium, error) {
	return pool.instance, nil
}

type pdfCancellationInstance struct {
	pdfium.Pdfium
	openDocument func() (*responses.OpenDocument, error)
	kill         func() error
}

func (instance pdfCancellationInstance) OpenDocument(*requests.OpenDocument) (*responses.OpenDocument, error) {
	return instance.openDocument()
}

func (instance pdfCancellationInstance) Kill() error { return instance.kill() }

func (instance pdfCancellationInstance) Close() error { return nil }

func pdfCancellationOperations() map[string]func(context.Context, *PDFParser) error {
	return map[string]func(context.Context, *PDFParser) error{
		"read layer": func(ctx context.Context, parser *PDFParser) error {
			_, err := parser.readLayer(ctx, []byte("pdf"))
			return err
		},
		"slice page": func(ctx context.Context, parser *PDFParser) error {
			_, err := parser.slicePage(ctx, []byte("pdf"), pdfPage{number: 1})
			return err
		},
	}
}

func TestPDFCancellationWaitsForGuestExit(t *testing.T) {
	for name, operation := range pdfCancellationOperations() {
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			killed := make(chan struct{})
			release := make(chan struct{})
			releaseGuest := sync.OnceFunc(func() { close(release) })
			defer releaseGuest()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			parser := &PDFParser{pool: pdfCancellationPool{instance: pdfCancellationInstance{
				openDocument: func() (*responses.OpenDocument, error) {
					close(started)
					<-killed
					<-release
					return nil, errors.New("guest interrupted")
				},
				kill: func() error {
					close(killed)
					return nil
				},
			}}}
			finished := make(chan error, 1)
			go func() {
				finished <- operation(ctx, parser)
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("guest did not start")
			}
			cancel()
			select {
			case <-killed:
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not kill the guest")
			}
			select {
			case err := <-finished:
				t.Fatalf("parse returned before the guest exited: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			releaseGuest()
			select {
			case err := <-finished:
				if !files.IsTransient(err) || !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation must remain transient: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("parse did not finish after the guest exited")
			}
		})
	}
}

func TestPDFCancellationOverridesGuestError(t *testing.T) {
	for name, operation := range pdfCancellationOperations() {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			parser := &PDFParser{pool: pdfCancellationPool{instance: pdfCancellationInstance{
				openDocument: func() (*responses.OpenDocument, error) {
					cancel()
					return nil, errors.New("guest interrupted")
				},
				kill: func() error { return nil },
			}}}
			err := operation(ctx, parser)
			if !files.IsTransient(err) || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation must override the permanent guest error: %v", err)
			}
		})
	}
}

// TestPDFMechanismNamesFollowTheLocalConvention: "local:" is load-bearing,
// not cosmetic. isLocalMechanism keys on it to decide whether a document
// made a billed call.
func TestPDFMechanismNamesFollowTheLocalConvention(t *testing.T) {
	if !strings.HasPrefix(PDFMechanism, "local:") {
		t.Errorf("PDFMechanism = %q; a layer-only extraction is free and must "+
			"carry the local: prefix", PDFMechanism)
	}
	if strings.HasPrefix(pdfMixedMechanismPrefix, "local:") {
		t.Errorf("pdfMixedMechanismPrefix = %q; a mixed document made a billed "+
			"call and must NOT read as local", pdfMixedMechanismPrefix)
	}
	for _, m := range []string{SpreadsheetMechanism, DocumentMechanism, PDFMechanism} {
		if !isLocalMechanism(m) {
			t.Errorf("%q not recognized as local", m)
		}
	}
}

// ---------------------------------------------------------------------
// Real corpus.
// ---------------------------------------------------------------------

// TestPDFArmAgainstRealCorpus runs the pdfium arm over a directory of
// real-world PDFs that the operator supplies.
//
// Same rationale as TestLocalParsersAgainstRealCorpus: real documents have
// shapes no self-generated fixture has. Such a corpus is private documents,
// so none is committed.
//
// Runs with NO model wired in, deliberately: what the text layer alone
// delivers IS the measurement the routing decision rests on, and it costs
// nothing. Consequence worth knowing before reading a failure: a
// genuinely-scanned document has no layer at all, so Extract correctly
// returns a permanent "yielded no text" — the test asserts that rather
// than treating it as a failure, and counts the pages either way so the
// headline sparse share stays honest.
//
// Run: DIDEROT_DOC_CORPUS=/path/to/corpus go test \
//
//	./internal/attachments/docs/ -run Corpus -v
func TestPDFArmAgainstRealCorpus(t *testing.T) {
	dir := os.Getenv("DIDEROT_DOC_CORPUS")
	if dir == "" {
		t.Skip("DIDEROT_DOC_CORPUS unset; see the doc comment")
	}
	manifest, err := readCorpusManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Skipf("no pdf manifest in %s: %v", dir, err)
	}
	if len(manifest) == 0 {
		t.Fatal("empty manifest")
	}
	p := testPDFParser(t, nil)

	var totalPages, totalSparse, scannedDocs, ranDocs int
	for _, m := range manifest {
		t.Run(m.FileID, func(t *testing.T) {
			data, err := os.ReadFile(m.Path)
			if err != nil {
				t.Skipf("missing file: %v", err)
			}
			if FormatForMediaType(m.Mimetype, m.Filename) != "pdf" {
				t.Skipf("not a pdf per the router (mimetype %q)", m.Mimetype)
			}

			// Measure the layer first, independently of Extract, so the
			// per-page densities are recorded even for a document Extract
			// rightly refuses.
			pages, err := p.readLayer(context.Background(), data)
			if err != nil {
				t.Fatalf("readLayer: %v", err)
			}
			if len(pages) == 0 {
				t.Fatal("pdfium reported no pages for a real document")
			}
			sparse, anyText := 0, false
			for _, pg := range pages {
				if pg.sparse() {
					sparse++
				}
				if strings.TrimSpace(pg.text) != "" {
					anyText = true
				}
			}
			totalPages += len(pages)
			totalSparse += sparse
			ranDocs++
			t.Logf("%s: %d pages, %d sparse (%.1f%%)",
				m.Filename, len(pages), sparse,
				100*float64(sparse)/float64(len(pages)))

			res, err := p.Extract(context.Background(), "pdf", m.Filename, data)
			if !anyText {
				// No layer text anywhere: a genuinely scanned document, which
				// with no model configured has nothing to offer. It must
				// dead-letter rather than commit an empty extraction that
				// reads as success forever after. Note this is a stricter
				// condition than "every page is sparse" — a one-page document
				// with 389 characters of layer text is entirely sparse but
				// still has content worth keeping.
				scannedDocs++
				if err == nil {
					t.Fatalf("a document with no text layer at all extracted "+
						"%d runes with no model configured; confident emptiness "+
						"is the failure mode that looks like success downstream",
						len([]rune(res.Markdown)))
				}
				if !isPermanent(err) {
					t.Errorf("fully-scanned document gave a retryable error: %v", err)
				}
				t.Logf("  no text layer; correctly refused: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}

			if res.PageCount != len(pages) {
				t.Errorf("PageCount = %d but pdfium sees %d pages",
					res.PageCount, len(pages))
			}
			if res.PagesExtracted != len(pages)-sparse {
				t.Errorf("PagesExtracted = %d, want %d (pages whose layer is "+
					"dense enough to stand alone)", res.PagesExtracted,
					len(pages)-sparse)
			}
			if res.ModelID != PDFMechanism {
				t.Errorf("ModelID = %q with no model configured", res.ModelID)
			}

			// The assembly contract, against real page counts rather than a
			// fixture's: every chunk must cite a page that exists, and a
			// dense page's text must land at its own page number.
			chunks := Split(res.Markdown)
			byPage := map[int]string{}
			for _, c := range chunks {
				if c.Page > res.PageCount {
					t.Errorf("chunk cites page %d, past the document's %d",
						c.Page, res.PageCount)
				}
				byPage[c.Page] += c.Content
			}
			t.Logf("  %d runes, %d chunks", len([]rune(res.Markdown)), len(chunks))
			for _, pg := range pages {
				if pg.sparse() {
					continue
				}
				// Compare on the first substantial word of the page's layer
				// text. A full-string compare would fail on the scrubbing
				// and whitespace normalization the layer text does not get
				// but the chunk boundaries impose.
				probe := firstLongWord(pg.text)
				if probe == "" {
					continue
				}
				if !strings.Contains(byPage[pg.number], probe) {
					t.Errorf("page %d's layer text (%q…) did not land on page %d; "+
						"got %q", pg.number, probe, pg.number,
						truncateRunes(byPage[pg.number], 80))
				}
			}
		})
	}
	// The routing decision's headline number: ~90% of real pages must come
	// out of the layer with no model call. Drift here means the threshold
	// no longer matches the corpus and the cost model is wrong.
	if totalPages == 0 {
		return
	}
	share := 100 * float64(totalSparse) / float64(totalPages)
	t.Logf("corpus totals: %d pages, %d sparse (%.1f%% reach a model); "+
		"%d documents with no text layer at all", totalPages, totalSparse,
		share, scannedDocs)
	if ranDocs < len(manifest) {
		// A -run filter narrowed the set, so the aggregate is not the
		// corpus's. Asserting on it would fail on a single scanned
		// document, which is not a regression.
		t.Logf("(ran %d of %d manifest entries; skipping the aggregate "+
			"assertion)", ranDocs, len(manifest))
		return
	}
	if share > 20 {
		t.Errorf("%.1f%% of corpus pages route to a model, far over the "+
			"expected share of about 9%%; the density threshold or the extraction is wrong",
			share)
	}
}

// firstLongWord returns the first word of at least 6 characters, as a
// probe that is unlikely to appear by accident on a different page.
func firstLongWord(s string) string {
	for _, w := range strings.Fields(s) {
		w = strings.Trim(w, ".,;:()[]{}\"'|")
		if len([]rune(w)) >= 6 {
			return w
		}
	}
	return ""
}

type corpusEntry struct {
	FileID   string `json:"file_id"`
	Path     string `json:"path"`
	Filename string `json:"filename"`
	Mimetype string `json:"mimetype"`
	Bytes    int    `json:"bytes"`
}

func readCorpusManifest(path string) ([]corpusEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []corpusEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Mimetype == "" {
			// The pdf manifest predates the mimetype column the office one
			// carries; every entry in it is a PDF by construction.
			out[i].Mimetype = "application/pdf"
		}
		if out[i].Filename == "" {
			out[i].Filename = filepath.Base(out[i].Path)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------
// Fixtures.
// ---------------------------------------------------------------------

// sequenceExtractor returns a distinct, call-ordered body so a test can
// tell which sub-PDF produced which page.
type sequenceExtractor struct {
	calls int
}

func (s *sequenceExtractor) ModelID() string { return "fake-model" }

func (s *sequenceExtractor) Extract(
	context.Context, string, string, []byte,
) (*Result, error) {
	s.calls++
	md := fmt.Sprintf("--- PAGE 1 ---\nMODEL-CALL-%d", s.calls)
	return &Result{
		Markdown: md, PageCount: 1, PagesExtracted: 1, ModelID: "fake-model",
	}, nil
}

// failAfterExtractor succeeds for the first failAfter calls, then fails —
// the shape that distinguishes "nothing spent yet, retry whole" from
// "already billed, keep it."
type failAfterExtractor struct {
	failAfter    int
	err          error
	inputTokens  int
	outputTokens int
	calls        int
}

func (f *failAfterExtractor) ModelID() string { return "fake-model" }

func (f *failAfterExtractor) Extract(
	context.Context, string, string, []byte,
) (*Result, error) {
	f.calls++
	if f.calls > f.failAfter {
		return nil, f.err
	}
	return &Result{
		Markdown:       fmt.Sprintf("--- PAGE 1 ---\nMODEL-OK-%d", f.calls),
		PageCount:      1,
		PagesExtracted: 1,
		ModelID:        "fake-model",
		InputTokens:    f.inputTokens,
		OutputTokens:   f.outputTokens,
	}, nil
}

// densePage returns a page body comfortably over pdfDensityThreshold, so
// a test's intent ("this page has a real text layer") does not depend on
// counting characters by hand.
func densePage(seed string) string {
	var b strings.Builder
	for b.Len() <= pdfDensityThreshold+200 {
		b.WriteString(seed)
		b.WriteByte(' ')
	}
	return b.String()
}

// buildTestPDF writes a minimal, valid, single-font PDF with one page per
// entry.
//
// Hand-built rather than committed as a fixture because the property under
// test is the character count of each page's text layer, and a generator
// makes "dense" and "sparse" explicit at the call site. Real-document
// shapes are covered by the corpus test instead — that division is
// deliberate, since a generated PDF cannot reproduce the rasterized
// letterhead and print-to-PDF cases the corpus has.
func buildTestPDF(t *testing.T, pages []string) []byte {
	t.Helper()
	if len(pages) == 0 {
		t.Fatal("buildTestPDF needs at least one page")
	}

	// Object numbering: 1 catalog, 2 page tree, 3 font, then per page a
	// page object and a content stream.
	const (
		catalogObj = 1
		pagesObj   = 2
		fontObj    = 3
		firstPage  = 4
	)
	objects := make([]string, 0, 3+2*len(pages))

	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", firstPage+2*i)
	}
	objects = append(objects,
		fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pagesObj),
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>",
			strings.Join(kids, " "), len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	for i, body := range pages {
		pageNum := firstPage + 2*i
		contentNum := pageNum + 1
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>",
			pagesObj, fontObj, contentNum))
		stream := contentStream(body)
		objects = append(objects, fmt.Sprintf(
			"<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}

	var buf strings.Builder
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xrefAt := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, catalogObj, xrefAt)
	return []byte(buf.String())
}

// contentStream renders text as PDF text-showing operators, wrapped so
// each ~90-character line becomes its own Tj — which is also how a real
// document's text layer is laid out.
func contentStream(body string) string {
	var b strings.Builder
	b.WriteString("BT\n/F1 11 Tf\n14 TL\n40 750 Td\n")
	for _, line := range wrapText(body, 90) {
		fmt.Fprintf(&b, "(%s) Tj\nT*\n", escapePDFString(line))
	}
	b.WriteString("ET")
	return b.String()
}

func wrapText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var (
		out  []string
		cur  strings.Builder
		lead = ""
	)
	for _, w := range words {
		if cur.Len()+len(lead)+len(w) > width && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
			lead = ""
		}
		cur.WriteString(lead)
		cur.WriteString(w)
		lead = " "
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func escapePDFString(s string) string {
	s = strings.ReplaceAll(s, "\\", `\\`)
	s = strings.ReplaceAll(s, "(", `\(`)
	s = strings.ReplaceAll(s, ")", `\)`)
	return s
}

// TestBuildTestPDFProducesTheDensitiesItClaims is the fixture's own
// negative control. Every routing test above rests on densePage landing
// above the threshold and a short page landing below it; if the generator
// drifted, those tests would keep passing while asserting nothing.
func TestBuildTestPDFProducesTheDensitiesItClaims(t *testing.T) {
	pdf := buildTestPDF(t, []string{densePage("Substantial body text."), "scan"})
	p := testPDFParser(t, nil)
	pages, err := p.readLayer(context.Background(), pdf)
	if err != nil {
		t.Fatalf("readLayer: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("got %d pages, want 2", len(pages))
	}
	if pages[0].sparse() {
		t.Errorf("densePage produced only %d chars, under the %d threshold — "+
			"every routing test using it is asserting nothing",
			pages[0].chars, pdfDensityThreshold)
	}
	if !pages[1].sparse() {
		t.Errorf("the short page produced %d chars, at or over the %d "+
			"threshold", pages[1].chars, pdfDensityThreshold)
	}
	if !strings.Contains(pages[0].text, "Substantial body text") {
		t.Errorf("page 1 layer text does not contain what was written: %q",
			pages[0].text)
	}
}
