package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/bluesky-social/diderot/internal/events"
)

// Ingester is the write side of the walker pipeline. Production uses an
// in-process events.DBSink adapter; tests can provide a lightweight fake.
type Ingester interface {
	IngestNotion(ctx context.Context, ev IngestEvent) (IngestOutcome, error)
}

type IngestOutcome string

const (
	IngestCreated   IngestOutcome = "created"
	IngestUpdated   IngestOutcome = "updated"
	IngestUnchanged IngestOutcome = "unchanged"
)

// IngestEvent is the connector-neutral shape handed to the in-process sink.
// Source is always "notion", so it is not repeated on every value.
//
// Images: collected by the walker via ExtractImages alongside the
// body render. Empty for the (common) case of pages without
// images. Server-side handler (or in-process adapter) hands them
// to events.ReconcileImages after Upsert returns the event_id.
type IngestEvent struct {
	ExternalID     string          `json:"external_id"`
	OccurredAt     time.Time       `json:"occurred_at"`
	Actor          string          `json:"actor"`
	ActorCanonical string          `json:"actor_canonical"`
	ActorID        string          `json:"actor_id"`
	ThreadID       string          `json:"thread_id,omitempty"`
	Title          string          `json:"title,omitempty"`
	URL            string          `json:"url"`
	Body           string          `json:"body"`
	Images         []ImageRef      `json:"images,omitempty"`
	Raw            json.RawMessage `json:"raw,omitempty"`
	ContextPrefix  string          `json:"context_prefix,omitempty"`
}

// Walker bundles a Notion API client, a directory cache, and an
// Ingester. Methods on it implement the two cooperating loops
// (search + reconcile) and the one-shot backfill that shares the
// same render path.
//
// All Walker calls are safe to use concurrently with each other
// (the Client's token bucket is the rate budget; the dir cache is
// goroutine-safe). One Walker per process is the expected shape.
type Walker struct {
	Client  *Client
	Catalog *CatalogResolver
	Dir     *DirCache
	Ingest  Ingester
	Logger  *slog.Logger

	// Concurrency is the size of the worker pool used by Search
	// and the reconcile tree-walker. Default 3. Notion's per-call
	// latency is ~500ms-1s, so a single-threaded loop achieves <
	// 1 req/s where the rate budget is 3. Three concurrent
	// in-flight calls (sharing one token bucket) saturates the
	// budget at typical latency.
	Concurrency int

	// UseNativeMarkdown selects the current API's page Markdown endpoint as the
	// primary body representation. Enabled by NewWalker; tests may disable it
	// when exercising the structured fallback renderer in isolation.
	UseNativeMarkdown bool
}

// NewWalker constructs a Walker. logger may be nil; defaults to
// slog.Default. Concurrency defaults to 3.
func NewWalker(client *Client, dir *DirCache, ingest Ingester, logger *slog.Logger) *Walker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Walker{
		Client:            client,
		Catalog:           &CatalogResolver{Client: client},
		Dir:               dir,
		Ingest:            ingest,
		Logger:            logger,
		Concurrency:       3,
		UseNativeMarkdown: true,
	}
}

// ingestJob is the unit of work passed from the discovery side
// (search-pagination loop or tree walker) to the worker pool. The
// blocks field is optional: callers that already have the page's
// children in hand (the tree walker) pass them through to skip a
// redundant fetch in ingestOne.
type ingestJob struct {
	page          *Page
	blocks        []Block
	fetchChildren childBlocksFn
	inaccessible  error
}

// blockChildrenCache lets the structural walker, fallback renderer, and image
// extractor share one nested-block fetch per page. Those consumers run
// concurrently during a managed-root walk, so a plain map is not enough: an
// in-flight call is represented by ready and followers wait for its result.
type blockChildrenCache struct {
	fetch childBlocksFn
	mu    sync.Mutex
	calls map[string]*blockChildrenCall
}

type blockChildrenCall struct {
	ready  chan struct{}
	blocks []Block
	err    error
}

func newBlockChildrenCache(fetch childBlocksFn) *blockChildrenCache {
	return &blockChildrenCache{
		fetch: fetch,
		calls: make(map[string]*blockChildrenCall),
	}
}

func (c *blockChildrenCache) Fetch(ctx context.Context, id string) ([]Block, error) {
	c.mu.Lock()
	if call, ok := c.calls[id]; ok {
		c.mu.Unlock()
		select {
		case <-call.ready:
			return call.blocks, call.err
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	call := &blockChildrenCall{ready: make(chan struct{})}
	c.calls[id] = call
	c.mu.Unlock()

	call.blocks, call.err = c.fetch(ctx, id)
	close(call.ready)
	return call.blocks, call.err
}

type PageSnapshot struct {
	PageID                  string
	LastEditedAt            time.Time
	Parent                  Parent
	URL                     string
	HasContent              bool
	TransientlyInaccessible bool
	Inaccessible            bool
	LastError               string
}

type WalkReport struct {
	Processed int
	Created   int
	Updated   int
	Unchanged int
	Pages     []PageSnapshot
}

// WalkRoot recursively walks a single root page and all its
// descendants (child_page + child_database + database rows),
// ingesting each one. Used by the root-based reconcile loop.
//
// Concurrency model: the structural traversal is sequential at
// the parent level (a page must be fetched before its children
// can be discovered), but ingest of each discovered page fans
// out across the worker pool. We use a per-walk seen set keyed on
// page UUID to break cycles — Notion permits a page to "embed"
// another page, which when followed naively can loop.
//
// Uses errgroup.WithContext so a fatal worker error cancels the
// derived context that both the workers and the producer
// (walkPageID/walkDatabaseRows) select on: the producer's blocked
// `jobs <-` send unblocks via ctx.Done() instead of hanging forever
// waiting for a worker that already exited. Retry story is
// deliberately simple — abort the whole walk on any fatal error and
// let the next 24h reconcile tick re-walk from scratch; the walk is
// naturally idempotent (events.Upsert / events.ReconcileImages both
// no-op on unchanged content), so no per-page retry/resume state is
// needed.
//
// Returns the count of pages processed + an error if any single
// fetch / ingest fatally failed. 404/403 on a sub-tree node →
// log + skip; the loop continues.
func (w *Walker) WalkRoot(ctx context.Context, rootID string) (int, error) {
	return w.WalkRootRef(ctx, RootRef{Type: RootTypePage, ObjectID: rootID})
}

// WalkRootRef walks a typed managed root. Auto roots are retained for
// migration and bare-UUID ergonomics; they are probed in resource order and
// fail unless one current API object can be fully enumerated.
func (w *Walker) WalkRootRef(ctx context.Context, root RootRef) (int, error) {
	report, err := w.WalkRootReport(ctx, root)
	return report.Processed, err
}

// WalkRootReport returns the complete set of reachable active pages when the
// walk succeeds. Callers must ignore Pages on error because the frontier was
// only partially enumerated.
func (w *Walker) WalkRootReport(ctx context.Context, root RootRef) (WalkReport, error) {
	seen := make(map[string]struct{})
	var mu sync.Mutex
	visit := func(id string) bool {
		if canonical, ok := canonicalize(id); ok {
			id = canonical
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := seen[id]; ok {
			return false
		}
		seen[id] = struct{}{}
		return true
	}

	concurrency := w.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	jobs := make(chan ingestJob, concurrency*2)
	var counter struct {
		mu        sync.Mutex
		total     int
		created   int
		updated   int
		unchanged int
		pages     map[string]PageSnapshot
	}
	counter.pages = make(map[string]PageSnapshot)

	g, gctx := errgroup.WithContext(ctx)
	for i := 0; i < concurrency; i++ {
		g.Go(func() error {
			for j := range jobs {
				pageID := j.page.ID
				if canonical, ok := canonicalize(pageID); ok {
					pageID = canonical
				}
				counter.mu.Lock()
				counter.pages[pageID] = PageSnapshot{
					PageID: pageID, LastEditedAt: j.page.LastEditedTime,
					Parent: j.page.Parent, URL: j.page.URL,
				}
				counter.mu.Unlock()
				if j.inaccessible != nil {
					counter.mu.Lock()
					snapshot := counter.pages[pageID]
					snapshot.Inaccessible = true
					snapshot.LastError = j.inaccessible.Error()
					counter.pages[pageID] = snapshot
					counter.mu.Unlock()
					continue
				}
				outcome, err := w.ingestOne(gctx, j.page, j.blocks, j.fetchChildren)
				if err != nil {
					if errors.Is(err, errSkipped) {
						continue
					}
					if isSkipError(err) {
						w.Logger.Warn("notion: page skipped",
							"id", j.page.ID, "err", err)
						// A page that becomes inaccessible between the
						// structural walk and rendering is not proven empty.
						// Keep it in the snapshot so reconciliation refreshes
						// membership, but preserve its last known-good event.
						counter.mu.Lock()
						snapshot := counter.pages[pageID]
						snapshot.TransientlyInaccessible = true
						snapshot.LastError = err.Error()
						counter.pages[pageID] = snapshot
						counter.mu.Unlock()
						continue
					}
					return err
				}
				counter.mu.Lock()
				snapshot := counter.pages[pageID]
				snapshot.HasContent = true
				counter.pages[pageID] = snapshot
				counter.total++
				switch outcome {
				case IngestCreated:
					counter.created++
				case IngestUpdated:
					counter.updated++
				case IngestUnchanged:
					counter.unchanged++
				}
				counter.mu.Unlock()
			}
			return nil
		})
	}

	// The structural walk itself also runs inside the group (rather
	// than on this goroutine) so a producer-side error (e.g.
	// GetPage/ListAllBlockChildren failing) cancels gctx too — not
	// just worker errors. Without this, a producer error would
	// leave gctx live, and the workers would keep draining every
	// already-buffered job before g.Wait() returned, delaying the
	// abort. `defer close(jobs)` guarantees
	// the workers see the channel close (and stop ranging over it)
	// on every exit path, including a mid-walk error.
	g.Go(func() error {
		defer close(jobs)
		return w.walkRootRef(gctx, root, visit, jobs)
	})

	walkErr := g.Wait()

	counter.mu.Lock()
	total := counter.total
	created := counter.created
	updated := counter.updated
	unchanged := counter.unchanged
	pages := make([]PageSnapshot, 0, len(counter.pages))
	for _, page := range counter.pages {
		pages = append(pages, page)
	}
	counter.mu.Unlock()
	return WalkReport{Processed: total, Created: created, Updated: updated, Unchanged: unchanged, Pages: pages}, walkErr
}

func (w *Walker) walkRootRef(
	ctx context.Context,
	root RootRef,
	visit func(string) bool,
	jobs chan<- ingestJob,
) error {
	switch root.Type {
	case RootTypePage:
		return w.walkPageID(ctx, root.ObjectID, true, visit, jobs)
	case RootTypeDatabase:
		return w.walkDatabaseRows(ctx, root.ObjectID, true, visit, jobs)
	case RootTypeDataSource:
		ds, err := w.Client.GetDataSource(ctx, root.ObjectID)
		if err != nil {
			return fmt.Errorf("walk: get root data source %s: %w", root.ObjectID, err)
		}
		return w.walkDataSourceRows(ctx, ds, visit, jobs)
	case RootTypeView:
		_, ds, err := w.Catalog.ResolveView(ctx, root.ObjectID)
		if err != nil {
			return fmt.Errorf("walk: resolve root view %s: %w", root.ObjectID, err)
		}
		return w.walkDataSourceRows(ctx, ds, visit, jobs)
	case RootTypeAuto, "":
		return w.walkAutoRoot(ctx, root.ObjectID, visit, jobs)
	default:
		return fmt.Errorf("walk: unsupported root type %q", root.Type)
	}
}

func (w *Walker) walkAutoRoot(
	ctx context.Context,
	id string,
	visit func(string) bool,
	jobs chan<- ingestJob,
) error {
	if err := w.walkPageID(ctx, id, true, visit, jobs); err == nil {
		return nil
	} else if !IsNotFound(err) && !IsForbidden(err) {
		return err
	}
	if err := w.walkDatabaseRows(ctx, id, true, visit, jobs); err == nil {
		return nil
	} else if !IsNotFound(err) && !IsForbidden(err) {
		return err
	}
	if ds, err := w.Client.GetDataSource(ctx, id); err == nil {
		return w.walkDataSourceRows(ctx, ds, visit, jobs)
	} else if !IsNotFound(err) && !IsForbidden(err) {
		return fmt.Errorf("walk: probe data source %s: %w", id, err)
	}
	if _, ds, err := w.Catalog.ResolveView(ctx, id); err == nil {
		return w.walkDataSourceRows(ctx, ds, visit, jobs)
	} else if !IsNotFound(err) && !IsForbidden(err) {
		return err
	}
	return fmt.Errorf("walk: auto root %s is missing or inaccessible", id)
}

// walkPageID is the recursive worker. It fetches the page, fetches
// its blocks once, hands them to the ingest pool, and recurses
// into child_page / child_database / table-database-row children.
//
// Block-fetch happens ONCE here even though both the body
// renderer and the structural walk need them. The renderer
// receives them through ingestOne via the jobs channel; the
// structural walk runs synchronously below.
//
// ctx is the errgroup-derived context from WalkRoot: it cancels the
// moment any worker returns a fatal error, which is what lets the
// `jobs <-` sends below abort promptly instead of blocking forever
// on a dead worker pool.
func (w *Walker) walkPageID(
	ctx context.Context,
	id string,
	isRoot bool,
	visit func(string) bool,
	jobs chan<- ingestJob,
) error {
	if id == "" || !visit(id) {
		return nil
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	default:
	}

	page, err := w.Client.GetPage(ctx, id)
	if err != nil {
		if IsNotFound(err) || IsForbidden(err) {
			if isRoot {
				return fmt.Errorf("walk: root %s is missing or inaccessible: %w", id, err)
			}
			w.Logger.Warn("notion: walk skipped page", "id", id, "err", err)
			return enqueueInaccessible(ctx, &Page{ID: id}, err, jobs)
		}
		return fmt.Errorf("walk: get page %s: %w", id, err)
	}
	blocks, err := w.Client.ListAllBlockChildren(ctx, page.ID)
	if err != nil {
		if IsNotFound(err) || IsForbidden(err) {
			if isRoot {
				return fmt.Errorf("walk: root %s blocks are missing or inaccessible: %w", id, err)
			}
			w.Logger.Warn("notion: walk skipped page (blocks)", "id", id, "err", err)
			return enqueueInaccessible(ctx, page, err, jobs)
		}
		return fmt.Errorf("walk: list children %s: %w", id, err)
	}

	return w.walkPageObject(ctx, page, blocks, visit, jobs)
}

func enqueueInaccessible(ctx context.Context, page *Page, accessErr error, jobs chan<- ingestJob) error {
	select {
	case jobs <- ingestJob{page: page, inaccessible: accessErr}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// walkPageObject enqueues an already-fetched page and traverses every block
// container looking for child pages and database containers. This covers
// descendants nested under toggles, columns, synced blocks, and database rows.
func (w *Walker) walkPageObject(
	ctx context.Context,
	page *Page,
	blocks []Block,
	visit func(string) bool,
	jobs chan<- ingestJob,
) error {
	if page.InTrash || page.Archived {
		return nil
	}
	children := newBlockChildrenCache(w.Client.ListAllBlockChildren)
	select {
	case jobs <- ingestJob{page: page, blocks: blocks, fetchChildren: children.Fetch}:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	return w.walkStructuralBlocks(ctx, blocks, children.Fetch, visit, jobs)
}

func (w *Walker) walkStructuralBlocks(
	ctx context.Context,
	blocks []Block,
	fetchChildren childBlocksFn,
	visit func(string) bool,
	jobs chan<- ingestJob,
) error {
	for i := range blocks {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		default:
		}
		b := &blocks[i]
		if b.Archived || b.InTrash {
			continue
		}
		switch b.Type {
		case "child_page":
			if err := w.walkPageID(ctx, b.ID, false, visit, jobs); err != nil {
				return err
			}
		case "child_database":
			if err := w.walkDatabaseRows(ctx, b.ID, false, visit, jobs); err != nil {
				return err
			}
		default:
			if b.HasChildren {
				children, err := fetchChildren(ctx, b.ID)
				if err != nil {
					if IsNotFound(err) || IsForbidden(err) {
						w.Logger.Warn("notion: walk skipped nested block children",
							"id", b.ID, "err", err)
						continue
					}
					return fmt.Errorf("walk: list nested children %s: %w", b.ID, err)
				}
				if err := w.walkStructuralBlocks(ctx, children, fetchChildren, visit, jobs); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// walkDatabaseRows resolves a database container's direct and linked views to
// current-API data sources, then enumerates every source row.
func (w *Walker) walkDatabaseRows(
	ctx context.Context,
	dbID string,
	isRoot bool,
	visit func(string) bool,
	jobs chan<- ingestJob,
) error {
	dataSources, err := w.Catalog.ResolveDatabase(ctx, dbID)
	if err != nil {
		if !isRoot && (IsNotFound(err) || IsForbidden(err)) {
			w.Logger.Warn("notion: walk skipped inaccessible child database", "id", dbID, "err", err)
			return nil
		}
		return fmt.Errorf("walk: resolve database %s: %w", dbID, err)
	}
	if isRoot && len(dataSources) == 0 {
		// A current Notion database has at least one backing data source,
		// even when it has zero rows. Treat zero accessible sources as an
		// incomplete root walk so reconciliation cannot sweep prior rows.
		return fmt.Errorf("walk: root database %s resolved to no accessible data sources", dbID)
	}
	for _, ds := range dataSources {
		if err := w.walkDataSourceRows(ctx, ds, visit, jobs); err != nil {
			return err
		}
	}
	return nil
}

func (w *Walker) walkDataSourceRows(
	ctx context.Context,
	ds *DataSource,
	visit func(string) bool,
	jobs chan<- ingestJob,
) error {
	if ds == nil || ds.ID == "" {
		return errors.New("walk: empty data source")
	}
	if ds.Parent.DatabaseID != "" {
		w.Dir.PrewarmDatabase(ds.Parent.DatabaseID, plainText(ds.Title))
	}
	cursor := ""
	for {
		page, err := w.Client.QueryDataSource(ctx, ds.ID, cursor, 100)
		if err != nil {
			return fmt.Errorf("walk: query data source %s: %w", ds.ID, err)
		}
		for _, raw := range page.Results {
			var p Page
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("walk: decode data source %s row: %w", ds.ID, err)
			}
			if !visit(p.ID) {
				continue
			}
			// We have the page object already (no need to
			// GET /pages/{id}). But we still need the blocks for
			// rendering — fetch them now.
			blocks, err := w.Client.ListAllBlockChildren(ctx, p.ID)
			if err != nil {
				if IsNotFound(err) || IsForbidden(err) {
					w.Logger.Warn("notion: skipped row blocks",
						"id", p.ID, "err", err)
					pCopy := p
					if err := enqueueInaccessible(ctx, &pCopy, err, jobs); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("walk: row blocks %s: %w", p.ID, err)
			}
			pCopy := p
			if err := w.walkPageObject(ctx, &pCopy, blocks, visit, jobs); err != nil {
				return err
			}
		}
		if !page.HasMore {
			return nil
		}
		if page.NextCursor == "" {
			return fmt.Errorf("walk: data source %s has_more without next_cursor", ds.ID)
		}
		cursor = page.NextCursor
	}
}

// errSkipped is returned by ingestOne for pages we deliberately
// drop on the floor (archived, empty after rendering). Distinct
// from a "real" error and from success — the worker doesn't count
// these in the ingested-pages tally.
var errSkipped = errors.New("notion: page skipped")

// ingestOne renders a page (using preFetched blocks if non-nil,
// else fetching on demand) and hands the resulting Event to the
// Ingester. Used by both the search loop and the tree walker; the
// tree walker passes pre-fetched blocks to avoid a duplicate
// fetch.
//
// Returns errSkipped for archived / empty pages so the worker's
// processed count doesn't tick for them.
func (w *Walker) ingestOne(ctx context.Context, page *Page, blocks []Block, fetchChildren childBlocksFn) (IngestOutcome, error) {
	if page == nil {
		return "", errors.New("notion: ingestOne nil page")
	}
	if page.Archived || page.InTrash {
		return "", errSkipped
	}
	if blocks == nil {
		var err error
		blocks, err = w.Client.ListAllBlockChildren(ctx, page.ID)
		if err != nil {
			return "", fmt.Errorf("ingestOne: list children: %w", err)
		}
	}

	// Rendered body: property preface (database row metadata) plus native
	// enhanced Markdown. The structured renderer remains the fallback for a
	// truncated response or unknown blocks.
	preface := PropertyPreface(ctx, page, w.Dir)
	if fetchChildren == nil {
		fetchChildren = func(ctx context.Context, parentID string) ([]Block, error) {
			return w.Client.ListAllBlockChildren(ctx, parentID)
		}
	}
	var markdown *PageMarkdown
	var body string
	var partialReason string
	if w.UseNativeMarkdown {
		var err error
		markdown, err = w.Client.GetPageMarkdown(ctx, page.ID)
		if err != nil {
			return "", fmt.Errorf("ingestOne: page markdown: %w", err)
		}
		body = stripExpiringNotionURLs(markdown.Markdown)
		// Raw participates in the corpus event dirty check. Persist the
		// normalized Markdown there too; otherwise Notion's rotating S3
		// signatures make an unchanged page look edited on every reconcile,
		// deleting and re-embedding all of its chunks.
		markdown.Markdown = body
	}
	if !w.UseNativeMarkdown || markdown.Truncated || len(markdown.UnknownBlockIDs) > 0 {
		renderer := NewRenderer(fetchChildren, RenderOptions{})
		rendered, err := renderer.Render(ctx, blocks)
		if err != nil {
			return "", fmt.Errorf("ingestOne: render fallback: %w", err)
		}
		body = stripExpiringNotionURLs(rendered.Body)
		if rendered.Truncated {
			partialReason = rendered.TruncationReason + " truncation"
		} else if markdown != nil && len(markdown.UnknownBlockIDs) > 0 {
			partialReason = "unsupported Notion blocks"
		}
	}

	// Image extraction: walk the block tree (recursing into
	// children of toggle/column/etc.) for image blocks. Runs
	// alongside the body render rather than reusing it because
	// the renderer's truncation cap (1000 blocks / 256 KiB) is
	// body-content-aware; we want every image regardless of
	// where in the page it sits. Per design doc § Recursion
	// bounds the structural walk is intentionally unbounded.
	images, err := ExtractImages(ctx, blocks, fetchChildren)
	if err != nil {
		return "", fmt.Errorf("ingestOne: extract images: %w", err)
	}

	title := PageTitle(page)
	if partialReason != "" {
		// Tag the title so consumers know the body is partial.
		title = fmt.Sprintf("%s (partial — %s)",
			title, partialReason)
	}
	if preface != "" && body != "" {
		body = preface + "\n\n" + body
	} else if preface != "" {
		body = preface
	}
	if strings.TrimSpace(events.SanitizeText(body)) == "" {
		// A page with no rendered content (no blocks, no
		// property preface), or whose only content is a NUL byte
		// (or other content the sink would sanitize to nothing),
		// has nothing useful to embed. Skip rather than call
		// Sink.Upsert with a body doomed to fail validate()'s
		// "Body is required" check forever.
		return "", errSkipped
	}

	// Resolve actor through the user cache. Prefer email when
	// known; fall back to a non-opaque display sentinel; canonical key uses
	// notion:<uuid> for humans, notion-bot:<uuid> for bots
	// (mirrors slack:<uuid>).
	actor, actorID, actorCanonical := w.resolveActor(ctx, page.LastEditedBy)
	raw, err := json.Marshal(struct {
		Page     *Page         `json:"page"`
		Markdown *PageMarkdown `json:"page_markdown,omitempty"`
	}{Page: page, Markdown: markdown})
	if err != nil {
		return "", fmt.Errorf("ingestOne: marshal source metadata: %w", err)
	}

	threadID := ""
	switch page.Parent.Type {
	case "page_id":
		threadID = page.Parent.PageID
	case "database_id":
		threadID = page.Parent.DatabaseID
	case "data_source_id":
		threadID = page.Parent.DataSourceID
	}
	if canonical, ok := canonicalize(threadID); ok {
		threadID = canonical
	}
	externalID := page.ID
	if canonical, ok := canonicalize(externalID); ok {
		externalID = canonical
	}

	ev := IngestEvent{
		ExternalID:     externalID,
		OccurredAt:     page.LastEditedTime,
		Actor:          actor,
		ActorCanonical: actorCanonical,
		ActorID:        actorID,
		ThreadID:       threadID,
		Title:          title,
		URL:            page.URL,
		Body:           body,
		Images:         images,
		Raw:            raw,
		ContextPrefix:  notionContextPrefix(title, preface),
	}
	return w.Ingest.IngestNotion(ctx, ev)
}

func notionContextPrefix(title, preface string) string {
	parts := []string{"Notion page: " + title}
	if preface != "" {
		for _, line := range strings.Split(preface, "\n") {
			if strings.HasPrefix(line, "Database: ") {
				parts = append(parts, line)
				break
			}
		}
	}
	return strings.Join(parts, "\n")
}

// resolveActor turns a PartialUser into the (display, id,
// canonical) triple we store in events. Bots without email get
// the notion-bot:<uuid> canonical form; humans get their email
// when known, else notion:<uuid>. Opaque ids are never used as
// display names.
func (w *Walker) resolveActor(ctx context.Context, p *PartialUser) (string, string, string) {
	if p == nil || p.ID == "" {
		return "", "", ""
	}
	id := p.ID
	user, err := w.Dir.User(ctx, id)
	if err != nil || user == nil {
		// No user details available — retain the source UUID as the
		// source identity, but do not present that opaque id as a name.
		return "unknown", id, "notion:" + id
	}
	displayName := user.Name
	if displayName == "" {
		// A directory record without a display name is not evidence
		// that the opaque Notion id is a human-readable name. Keep the
		// source id in actor_id/canonical and leave presentation explicit.
		displayName = "unknown"
	}
	if user.IsBot() {
		if displayName == "unknown" {
			return displayName, id, "notion-bot:" + id
		}
		return displayName + " [bot]", id, "notion-bot:" + id
	}
	if email := user.Email(); email != "" {
		return displayName, id, email
	}
	return displayName, id, "notion:" + id
}

// isSkipError reports whether err is a 404/403 (already logged at
// the call site, no need to escalate). Future-proofed: callers
// shouldn't need to know the specific error types.
func isSkipError(err error) bool {
	return IsNotFound(err) || IsForbidden(err)
}
