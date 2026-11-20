// Package docs, PDF arm: pdfium's own text layer per page, with only the
// pages whose layer is too thin to trust reaching a model.
package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/bluesky-social/diderot/internal/attachments/files"
)

// PDFMechanism identifies PDFs extracted entirely from their text layer.
const PDFMechanism = "local:pdfium"

// pdfMixedMechanismPrefix identifies PDFs with at least one model-extracted page.
const pdfMixedMechanismPrefix = "pdfium+"

// Pages below this text-layer character count go to the model. The corpus
// median was 1,783; routing on image objects mostly selected logos.
const pdfDensityThreshold = 400

// pdfLayerTimeout bounds both pool acquisition and guest execution.
const pdfLayerTimeout = 2 * time.Minute

// pdfMemoryLimitPages gives each WASM instance 256 MiB of linear memory.
const pdfMemoryLimitPages = 4096

// PDFParserConfig configures the pdfium arm.
type PDFParserConfig struct {
	// Model transcribes sparse pages. Nil remains legal so construction can
	// report its capabilities, but Router.ClaimFormats prevents the production
	// worker from claiming PDFs in that state. Direct callers still receive the
	// text-layer-only result for diagnostics and focused tests.
	Model Extractor
	// Instances caps concurrent WASM instances. Defaults to 2, matching
	// Config.Concurrency's default — one instance per worker goroutine,
	// since a document holds an instance only for its parse.
	Instances int
	Logger    *slog.Logger
}

// PDFParser uses pdfium text layers and sends only sparse pages to the model.
type PDFParser struct {
	pool   pdfium.Pool
	model  Extractor
	logger *slog.Logger
}

// pdfiumPoolConfig removes the library's default host-root mount, bounds
// memory, and makes guest execution cancellable.
func pdfiumPoolConfig(instances int) webassembly.Config {
	return webassembly.Config{
		// Instances are cheap to recreate and may retain large guest memory.
		MinIdle:  0,
		MaxIdle:  0,
		MaxTotal: instances,
		// A nil FSConfig would mount the host root into the guest.
		FSConfig: wazero.NewFSConfig(),
		RuntimeConfig: wazero.NewRuntimeConfig().
			WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
			WithMemoryLimitPages(pdfMemoryLimitPages).
			// Allows instance.Kill() to interrupt guest code.
			WithCloseOnContextDone(true),
	}
}

// NewPDFParser initializes the WASM pool. The compile happens here, once,
// so a construction failure is a boot-time problem rather than a
// per-document one.
func NewPDFParser(cfg PDFParserConfig) (*PDFParser, error) {
	if cfg.Instances <= 0 {
		cfg.Instances = 2
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	pool, err := webassembly.Init(pdfiumPoolConfig(cfg.Instances))
	if err != nil {
		return nil, fmt.Errorf("docs: init pdfium wasm pool: %w", err)
	}
	return &PDFParser{pool: pool, model: cfg.Model, logger: cfg.Logger}, nil
}

// Close tears down the WASM pool and its runtime.
func (p *PDFParser) Close() error {
	if p == nil || p.pool == nil {
		return nil
	}
	return p.pool.Close()
}

// ModelID reports the mechanism this arm will use, for the boot log and
// for Router.Mechanisms. Per-document provenance is on the Result, which
// says "local:pdfium" for a document that never needed a model even when
// one is configured.
func (p *PDFParser) ModelID() string {
	if p.model == nil {
		return PDFMechanism
	}
	return pdfMixedMechanismPrefix + p.model.ModelID()
}

// SparsePageModelConfigured reports whether this arm can completely process a
// PDF. PDFium remains the first stage for every page, but claiming PDFs without
// this model would terminally settle sparse/scanned pages based on deployment
// configuration rather than document contents.
func (p *PDFParser) SparsePageModelConfigured() bool {
	return p != nil && p.model != nil
}

// pdfPage is one page's layer measurement.
type pdfPage struct {
	number int
	text   string
	chars  int
}

// sparse reports whether this page's layer is too thin to be trusted as
// the page's content.
func (p pdfPage) sparse() bool { return p.chars < pdfDensityThreshold }

// Extract reads the text layer and sends only sparse pages to the model.
func (p *PDFParser) Extract(
	ctx context.Context, docFormat string, filename string, data []byte,
) (*Result, error) {
	return p.extract(ctx, docFormat, filename, data, nil)
}

// ExtractWithCheckpoints persists each model page before starting the next.
func (p *PDFParser) ExtractWithCheckpoints(
	ctx context.Context,
	docFormat, filename string,
	data []byte,
	checkpoints PageCheckpointStore,
) (*Result, error) {
	return p.extract(ctx, docFormat, filename, data, checkpoints)
}

func (p *PDFParser) extract(
	ctx context.Context,
	docFormat, filename string,
	data []byte,
	checkpoints PageCheckpointStore,
) (*Result, error) {
	if f := strings.ToLower(strings.TrimSpace(docFormat)); f != "pdf" {
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: PDFParser cannot read %q", docFormat)}
	}
	if len(data) == 0 {
		return nil, &files.PermanentError{Underlying: errors.New("docs: empty pdf")}
	}
	if len(data) > MaxDocumentBytes {
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: %d bytes exceeds the %d-byte limit", len(data), MaxDocumentBytes)}
	}

	pages, err := p.readLayer(ctx, data)
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, &files.PermanentError{Underlying: errors.New("docs: pdf has no pages")}
	}

	// bodies is keyed by 1-based page number. A page absent from the map
	// produced nothing at all; a page present with empty text is treated
	// the same way by assemblePages.
	bodies := make(map[int]string, len(pages))
	// extracted counts pages whose content we are willing to call the
	// page's content: a dense layer, or a model transcription. A sparse
	// page that fell back to its own thin layer is deliberately NOT
	// counted — that is the signal that makes the row findable as
	// missing_pages.
	extracted := 0
	var sparse []pdfPage
	for _, pg := range pages {
		if pg.sparse() {
			sparse = append(sparse, pg)
			continue
		}
		bodies[pg.number] = strings.TrimSpace(pg.text)
		extracted++
	}

	var (
		usedModel   bool
		costUSD     float64
		costLimited bool
		truncated   bool
	)
	if len(sparse) > 0 {
		mr, err := p.transcribeSparse(ctx, filename, data, sparse, bodies, checkpoints)
		if err != nil {
			return nil, err
		}
		usedModel = mr.calls > 0
		truncated = mr.truncated
		extracted += mr.pages
		costUSD = mr.costUSD
		costLimited = mr.costLimited
	}

	// Sparse pages the model didn't cover keep their thin layer text: a
	// page of browser chrome is still better in the index than a hole,
	// and it is exactly the comparison signal the hallucination canary
	// wants. It does not count toward extracted (see above).
	for _, pg := range sparse {
		if strings.TrimSpace(bodies[pg.number]) == "" {
			if t := strings.TrimSpace(pg.text); t != "" {
				bodies[pg.number] = t
			}
		}
	}

	markdown := assemblePages(bodies, len(pages))
	if strings.TrimSpace(markdown) == "" {
		// Nothing at all. Reachable only two ways, and both are config or
		// transport problems rather than properties of the document: no
		// model is wired in, or every sparse page was permanently rejected. A
		// working model path always returns something for a scanned page —
		// even "[illegible]" — so this is not the "blank scan" case, which
		// reaches Reject() as RejectEmpty instead.
		//
		// Permanent, so it dead-letters loudly rather than retrying: a
		// second pass over the same bytes with the same config produces the
		// same nothing. Same outcome the router gave every PDF when no
		// model extractor was configured, before this arm existed.
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: pdf yielded no text (%d pages, %d sparse, model configured: %v)",
			len(pages), len(sparse), p.model != nil)}
	}

	mechanism := PDFMechanism
	if usedModel {
		mechanism = pdfMixedMechanismPrefix + p.model.ModelID()
	}

	p.logger.Info("docs: pdf text layer",
		"filename", filename, "pages", len(pages),
		"layer_pages", len(pages)-len(sparse), "sparse_pages", len(sparse),
		"pages_extracted", extracted,
		"mechanism", mechanism)

	return &Result{
		Markdown:       markdown,
		PageCount:      len(pages),
		PagesExtracted: extracted,
		ModelID:        mechanism,
		Truncated:      truncated,
		CostUSD:        costUSD,
		CostLimited:    costLimited,
	}, nil
}

type modelRun struct {
	calls       int
	pages       int
	costUSD     float64
	costLimited bool
	truncated   bool
}

// transcribeSparse calls the model once per sparse page. Each successful call
// is checkpointed before the next page starts.
func (p *PDFParser) transcribeSparse(
	ctx context.Context, filename string, data []byte,
	sparse []pdfPage, bodies map[int]string,
	checkpoints PageCheckpointStore,
) (modelRun, error) {
	var run modelRun
	if p.model == nil {
		p.logger.Warn("docs: pdf has sparse pages but no model extractor; "+
			"keeping the thin text layer and settling partial",
			"filename", filename, "sparse_pages", len(sparse))
		return run, nil
	}
	model := p.model
	if snapshotter, ok := model.(interface{ Snapshot() Extractor }); ok {
		model = snapshotter.Snapshot()
	}
	checkpointIdentity := model.ModelID()
	if identified, ok := model.(interface{ CheckpointIdentity() string }); ok {
		checkpointIdentity = identified.CheckpointIdentity()
	}

	saved := make(map[int]PageCheckpoint)
	if checkpoints != nil {
		pages, err := checkpoints.Load(ctx, checkpointIdentity)
		if err != nil {
			return run, &files.TransientError{Underlying: fmt.Errorf(
				"load extraction checkpoints: %w", err)}
		}
		for _, page := range pages {
			saved[page.PageNumber] = page
		}
	}

	for _, pg := range sparse {
		if err := ctx.Err(); err != nil {
			return run, &files.TransientError{Underlying: err}
		}

		if page, ok := saved[pg.number]; ok {
			p.applyPage(filename, pg, page, bodies, &run, true)
			continue
		}

		if run.costUSD >= maxDocumentCostUSD {
			run.costLimited = true
			p.logger.Warn("docs: per-document extraction cost limit reached",
				"filename", filename, "spent_usd", run.costUSD,
				"limit_usd", maxDocumentCostUSD,
				"first_unreached_page", pg.number)
			break
		}

		sub, err := p.slicePage(ctx, data, pg)
		if err != nil {
			if files.IsTransient(err) {
				return run, err
			}
			p.logger.Warn("docs: could not slice sparse page; skipping",
				"filename", filename, "page", pg.number, "err", err)
			continue
		}

		res, err := model.Extract(ctx, "pdf", filename, sub)
		if err != nil {
			if files.IsTransient(err) {
				return run, fmt.Errorf("pdf sparse page %d: %w", pg.number, err)
			}
			p.logger.Warn("docs: model rejected a sparse page; skipping",
				"filename", filename, "page", pg.number, "err", err)
			continue
		}
		page := PageCheckpoint{
			PageNumber: pg.number,
			Markdown:   res.Markdown,
			CostUSD: extractionCostUSD(
				model.ModelID(), res.InputTokens, res.OutputTokens),
			Truncated: res.Truncated,
		}
		if checkpoints != nil {
			persisted, saveErr := checkpoints.Save(ctx, checkpointIdentity, page)
			if saveErr != nil {
				p.logger.Error("docs: extraction checkpoint write failed; settling partial",
					"filename", filename, "page", pg.number, "err", saveErr)
				p.applyPage(filename, pg, page, bodies, &run, false)
				break
			}
			page = persisted
		}
		p.applyPage(filename, pg, page, bodies, &run, false)
	}
	return run, nil
}

func (p *PDFParser) applyPage(
	filename string,
	pg pdfPage,
	page PageCheckpoint,
	bodies map[int]string,
	run *modelRun,
	resumed bool,
) {
	run.calls++
	run.costUSD += page.CostUSD
	run.truncated = run.truncated || page.Truncated

	body := singlePageBody(page.Markdown)
	if body != "" {
		bodies[pg.number] = body
		run.pages++
	}

	p.logger.Info("docs: pdf sparse page transcribed",
		"filename", filename, "page", pg.number,
		"layer_chars", pg.chars,
		"model_chars", len([]rune(bodies[pg.number])),
		"resumed", resumed)
}

func singlePageBody(markdown string) string {
	var lines []string
	for _, line := range strings.Split(markdown, "\n") {
		if _, ok := parsePageMarker(line); !ok {
			lines = append(lines, line)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// assemblePages renders the per-page bodies as one markdown document with
// the same "--- PAGE N ---" markers the splitter keys on, in page order.
//
// Pages with no content are skipped entirely rather than emitted as a
// bare marker: the marker carries its own number, so skipping a page does
// not renumber anything after it, and an empty page block would only
// produce a chunk of whitespace the splitter has to drop anyway.
func assemblePages(bodies map[int]string, pageCount int) string {
	var b strings.Builder
	for n := 1; n <= pageCount; n++ {
		body := strings.TrimSpace(bodies[n])
		if body == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, PageMarker+"\n%s", n, body)
	}
	return b.String()
}

// acquireInstance borrows a WASM instance under the caller's deadline.
//
// GetInstanceWithContext rather than GetInstance(timeout): the pool is
// configured BlockWhenExhausted, and GetInstance roots its wait at
// context.Background() (webassembly.go:262), so a cancelled extraction —
// leadership loss, pod shutdown, an expiring document deadline — could not
// shorten the wait. Worse, the borrow happens BEFORE the select that would
// otherwise observe cancellation, so the ctx.Done() branch is unreachable
// while it blocks.
//
// Transient: a busy pool or a pool closing during shutdown says nothing
// about the document.
func (p *PDFParser) acquireInstance(ctx context.Context) (pdfium.Pdfium, error) {
	inst, err := p.pool.GetInstanceWithContext(ctx)
	if err != nil {
		return nil, &files.TransientError{Underlying: fmt.Errorf(
			"docs: pdfium instance: %w", err)}
	}
	return inst, nil
}

// readLayer measures every page's text layer in one guest session.
//
// The pdfium work runs on its own goroutine so ctx cancellation and
// pdfLayerTimeout can be enforced: Kill() cancels the worker context,
// which the WithCloseOnContextDone runtime turns into an abort of the
// in-flight guest call. Without that a looping parse would hold one of
// two worker slots until the process restarts.
func (p *PDFParser) readLayer(ctx context.Context, data []byte) ([]pdfPage, error) {
	return runPDFium(ctx, p, "reading the document", func(instance pdfium.Pdfium) ([]pdfPage, error) {
		return readPageLayers(instance, data)
	})
}

func runPDFium[Output any](ctx context.Context, parser *PDFParser, action string, operation func(pdfium.Pdfium) (Output, error)) (Output, error) {
	var empty Output
	ctx, cancel := context.WithTimeout(ctx, pdfLayerTimeout)
	defer cancel()

	instance, err := parser.acquireInstance(ctx)
	if err != nil {
		return empty, err
	}

	type outcome struct {
		value Output
		err   error
	}
	resultCh := make(chan outcome, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				resultCh <- outcome{err: &files.PermanentError{Underlying: fmt.Errorf(
					"docs: pdfium panicked %s: %v", action, recovered)}}
			}
		}()
		value, operationErr := operation(instance)
		resultCh <- outcome{value: value, err: operationErr}
	}()

	select {
	case <-ctx.Done():
		_ = instance.Kill()
		<-resultCh
	case result := <-resultCh:
		_ = instance.Close()
		if ctx.Err() == nil {
			return result.value, result.err
		}
	}
	return empty, &files.TransientError{Underlying: fmt.Errorf(
		"docs: pdfium interrupted %s: %w", action, ctx.Err())}
}

// readPageLayers runs the actual pdfium calls. Bytes in, never a path —
// which is what lets the guest run with no filesystem at all.
func readPageLayers(inst pdfium.Pdfium, data []byte) ([]pdfPage, error) {
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		// A file pdfium cannot open will not open on a retry: corrupt,
		// encrypted, or not a PDF at all.
		return nil, &files.PermanentError{Underlying: fmt.Errorf("docs: open pdf: %w", err)}
	}
	defer func() {
		_, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
	}()

	pc, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return nil, &files.PermanentError{Underlying: fmt.Errorf("docs: pdf page count: %w", err)}
	}

	pages := make([]pdfPage, 0, pc.PageCount)
	for i := 0; i < pc.PageCount; i++ {
		pg := pdfPage{number: i + 1}
		// A page whose text cannot be read is not a document-level
		// failure: it becomes a sparse page and routes to the model, which
		// is the right answer for a page pdfium can't decode anyway.
		if pt, terr := inst.GetPageText(&requests.GetPageText{
			Page: requests.Page{ByIndex: &requests.PageByIndex{
				Document: doc.Document, Index: i}},
		}); terr == nil {
			// pdfium emits CRLF line breaks. The model path returns bare LF, and these two
			// streams are merged into one document, so normalizing here is
			// what keeps a chunk's line endings from depending on which
			// mechanism read its page. Carriage returns would otherwise be
			// embedded and rendered as-is in retrieval output.
			pg.text = strings.ReplaceAll(pt.Text, "\r\n", "\n")
			pg.chars = len([]rune(strings.TrimSpace(pg.text)))
		}
		pages = append(pages, pg)
	}
	return pages, nil
}

// slicePage builds a one-page PDF for a sparse page.
//
// This avoids sending the whole document and asking the model to
// transcribe one page, which would pay the full input cost each time and
// trust the model to obey a page filter.
//
// A fresh instance per page rather than one held across the model calls:
// the pool has one instance per worker goroutine, and holding one through
// a 20-minute gateway call would starve the sibling worker. Re-parsing the
// source is measured in tens of milliseconds.
func (p *PDFParser) slicePage(ctx context.Context, data []byte, page pdfPage) ([]byte, error) {
	return runPDFium(ctx, p, fmt.Sprintf("slicing page %d", page.number), func(instance pdfium.Pdfium) ([]byte, error) {
		return slicePageLocked(instance, data, page)
	})
}

func slicePageLocked(inst pdfium.Pdfium, data []byte, page pdfPage) ([]byte, error) {
	src, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		return nil, &files.PermanentError{Underlying: fmt.Errorf("docs: open pdf: %w", err)}
	}
	defer func() {
		_, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: src.Document})
	}()

	dst, err := inst.FPDF_CreateNewDocument(&requests.FPDF_CreateNewDocument{})
	if err != nil {
		return nil, fmt.Errorf("docs: create sub-document: %w", err)
	}
	defer func() {
		_, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: dst.Document})
	}()

	if _, err := inst.FPDF_ImportPagesByIndex(&requests.FPDF_ImportPagesByIndex{
		Source:      src.Document,
		Destination: dst.Document,
		PageIndices: []int{page.number - 1},
		Index:       0,
	}); err != nil {
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: import page %d: %w", page.number, err)}
	}

	// Neither FilePath nor FileWriter, so the bytes come back in the
	// response — no host file, no guest file.
	saved, err := inst.FPDF_SaveAsCopy(&requests.FPDF_SaveAsCopy{Document: dst.Document})
	if err != nil {
		return nil, fmt.Errorf("docs: save sub-document: %w", err)
	}
	if saved.FileBytes == nil || len(*saved.FileBytes) == 0 {
		return nil, errors.New("docs: sub-document saved as zero bytes")
	}
	return *saved.FileBytes, nil
}

// compile-time proof the arm satisfies the seam.
var _ Extractor = (*PDFParser)(nil)
