// Package retrieve is the shared core for /v1/retrieve (raw hits)
// and /v1/ask (hits -> LLM synthesis). Same pipeline: embed the
// question, search qdrant, dedup by event_id, dereference chunk
// content from Postgres.
//
// No rerank — a qwen3-reranker middle stage was designed but never
// built. We ship
// plain cosine + dedup so we get a fast feedback loop on whether
// retrieval quality is passable at all.
package retrieve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/embedder"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
)

// Request is the structured input to Retrieve. Nothing here is
// HTTP-specific; the HTTP endpoint wraps this, the MCP tool wraps this,
// a future Agent SDK wrapper would also wrap this.
type Request struct {
	Question string
	Filters  Filters
	// TopK is the number of fully-dereferenced results to return: from the
	// one source Filters.Sources names (20 if unset), or from each source
	// when it names several or none (EachSourceDefault if unset, and
	// EachSourceMax at most).
	TopK int
}

// A search of several sources, or of all of them, ranks each source on its
// own and returns the best few of each. Ranked together, the large sources
// take every place: of 40 events from two small sources that answers had
// cited, one search across everything returned 2 in its top 20, and the same
// question put to the event's own source returned 31 in its top 5.
const (
	EachSourceDefault = 5
	EachSourceMax     = 10
	// staleHeadroom is how many candidates beyond top_k are read for each
	// source, to replace those whose event is gone by the time it is read.
	// Points outlive their events only briefly, so a few is plenty.
	staleHeadroom = 10
)

// Filters mirrors qdrantstore.SearchFilters but lives in its own
// type so upstream callers don't have to import qdrantstore.
// Fields map 1:1; zero values mean "no constraint."
type Filters struct {
	Sources        []string
	Actors         []string
	OccurredAfter  time.Time
	OccurredBefore time.Time
	// AllowedScopes is the visibility boundary for this retrieval.
	// Empty/nil =
	// no access. Non-empty =
	// only events whose scope is in the set are returned —
	// enforced both in the qdrant pre-ANN filter and in the SQL
	// dereference (belt-and-suspenders; a stale qdrant payload
	// must not leak through the SQL leg). Set server-side by the
	// ask layer from the invocation context; never
	// model-controlled.
	AllowedScopes []string
	// ExcludeConversations leaves whole conversations out of the result.
	// The ask layer sets it to the conversation it is answering, which is
	// never evidence for its own answer; earlier conversations, earlier
	// askings of the same question among them, are not affected. Never
	// model-controlled.
	ExcludeConversations []Conversation
}

// Conversation names one conversation in a source by the thread_id its
// events share. The event that opened it carries that key as its
// external_id instead (a Slack thread's first message has no thread_id of
// its own until it is replied to), so both are matched.
type Conversation struct {
	Source   string
	ThreadID string
}

// conversationKeys splits conversations into the parallel source and key
// arrays the exclusion queries unnest.
func conversationKeys(conversations []Conversation) (sources, keys []string) {
	for _, c := range conversations {
		if c.Source == "" || c.ThreadID == "" {
			continue
		}
		sources = append(sources, c.Source)
		keys = append(keys, c.ThreadID)
	}
	return sources, keys
}

// conversationChunkIDs returns the chunks of every event in conversations.
// It is read on each search, not once per turn, because the conversation
// being answered grows while it is answered.
func (r *Retriever) conversationChunkIDs(ctx context.Context, conversations []Conversation) ([]uint64, error) {
	sources, keys := conversationKeys(conversations)
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT c.id
		  FROM unnest($1::text[], $2::text[]) AS x(source, key)
		  JOIN events e ON e.source = x.source AND e.external_id = x.key
		  JOIN event_chunks c ON c.event_id = e.id
		UNION
		SELECT c.id
		  FROM unnest($1::text[], $2::text[]) AS x(source, key)
		  JOIN events e ON e.source = x.source AND e.thread_id = x.key
		  JOIN event_chunks c ON c.event_id = e.id`, sources, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, uint64(id))
	}
	return ids, rows.Err()
}

// Hit is one retrieval result after dedup + dereference.
type Hit struct {
	EventID int64 `json:"event_id"`
	// ExternalID is the source-side stable id (e.g. for slack:
	// "C123:1700000000.000100"). Used as part of the citation
	// dedup key when the answering layer needs to collapse multiple
	// retrieve calls into a single citation list.
	ExternalID     string    `json:"external_id"`
	ChunkID        uint64    `json:"chunk_id"`
	Score          float32   `json:"score"`
	Source         string    `json:"source"`
	Actor          string    `json:"actor,omitempty"`
	ActorCanonical string    `json:"actor_canonical,omitempty"`
	ActorID        string    `json:"actor_id,omitempty"`
	ThreadID       string    `json:"thread_id,omitempty"`
	OccurredAt     time.Time `json:"occurred_at"`
	Title          string    `json:"title,omitempty"`
	URL            string    `json:"url,omitempty"`
	// Content is the matched chunk's text (not the full event body).
	// The embedder scored on this specific slice; showing it lets
	// the operator or LLM verify the match.
	Content string `json:"content"`
	// ChunkCount is how many chunks the parent event has in total.
	// Useful when the synthesizer wants to decide whether to
	// expand to the full body.
	ChunkCount int `json:"chunk_count,omitempty"`
	// Kind is the chunk's provenance: "text" (the message/page body as
	// written), "image_caption" (VLM description of an attached image),
	// or "document_text" (text extracted from an attached document).
	//
	// Load-bearing for attribution, not cosmetic. Without it, text
	// extracted from an attached PDF is indistinguishable from words the
	// poster typed, so the answering layer will attribute a document's
	// contents to whoever shared it — "Chester said retention is 18
	// months" when Chester attached someone else's contract.
	Kind string `json:"kind,omitempty"`
	// DocumentFilename / DocumentPageCount are populated only for
	// document_text chunks, so a citation can name the source document.
	DocumentFilename  string `json:"document_filename,omitempty"`
	DocumentPageCount int    `json:"document_page_count,omitempty"`
	// Empty for non-document chunks.
	DocumentCompleteness   string `json:"document_completeness,omitempty"`
	DocumentIncompleteWhy  string `json:"document_incomplete_reason,omitempty"`
	DocumentPagesExtracted int    `json:"document_pages_extracted,omitempty"`
	// ChannelName is the human-readable slack channel for slack
	// hits (e.g. "eng-platform"). Empty for non-slack sources and
	// for slack hits whose channel hasn't been directory-cached
	// yet. The model lifts this into rendered output —
	// without it, slack citations read as raw channel IDs from the
	// permalink ("C0EXAMPLE12"), which the model is prone to
	// hallucinating into friendly-but-wrong names like "#network".
	ChannelName string `json:"channel_name,omitempty"`
}

// ErrNotSetUp is a search asked before an administrator has chosen the
// embedding model. List, which reads Postgres alone, works without one.
var ErrNotSetUp = errors.New("search is not set up yet: an administrator chooses the embedding model on the Models page")

// vectors is the embedding model and the collection its vectors are in.
type vectors struct {
	embedder embedder.Embedder
	store    *qdrantstore.Store
}

// Retriever wires the dependencies once and exposes Retrieve.
// Safe for concurrent use.
type Retriever struct {
	pool *pgxpool.Pool
	// vectors is nil until SetVectors gives it the embedding model.
	vectors atomic.Pointer[vectors]
	logger  *slog.Logger

	// channelFinder resolves list channel entries of sources other than
	// Slack; nil (or an unset pointer) knows none. atomic.Pointer, like
	// vectors above, because SetChannelFinder can be called again after
	// construction while List reads this concurrently from live serving.
	channelFinder atomic.Pointer[ChannelFinder]

	// overfetch is the number of chunks pulled from qdrant before
	// dedup. 100 matches the design doc. A higher number means
	// better chance of covering diverse events when a handful of
	// big threads dominate; lower means smaller qdrant response +
	// SQL dereference. 100 is comfortably small either way.
	overfetch uint64
}

// NewRetriever wires a Retriever. A nil emb or store leaves search off
// until SetVectors; nil logger OK.
func NewRetriever(
	pool *pgxpool.Pool,
	emb embedder.Embedder,
	store *qdrantstore.Store,
	logger *slog.Logger,
) *Retriever {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Retriever{pool: pool, logger: logger, overfetch: 100}
	r.SetVectors(emb, store)
	return r
}

// SetVectors makes Retrieve embed questions with emb and search store, the
// collection emb's vectors are in, from the next call on. A nil emb or
// store turns search off.
func (r *Retriever) SetVectors(emb embedder.Embedder, store *qdrantstore.Store) {
	if emb == nil || store == nil {
		r.vectors.Store(nil)
		return
	}
	r.vectors.Store(&vectors{embedder: emb, store: store})
}

// Result bundles the hits + the stages' individual timings so
// dashboards and debugging tools can surface where latency lives.
type Result struct {
	Hits    []Hit            `json:"hits"`
	Latency LatencyBreakdown `json:"latency"`
	// EmbedModelID is what produced the query vector — useful for
	// an operator double-checking cross-embedder routing once we
	// have more than one embedder.
	EmbedModelID string `json:"embed_model_id,omitempty"`
	// NextCursor is set by List when more rows match beyond this
	// page: an opaque token to pass as ListRequest.Cursor (same
	// filters) for the next page. Empty = no more pages. Never set
	// by Retrieve — vector search has no stable enumeration order
	// to resume.
	NextCursor string `json:"next_cursor,omitempty"`
}

type LatencyBreakdown struct {
	EmbedMS       int64 `json:"embed_ms"`
	SearchMS      int64 `json:"search_ms"`
	DereferenceMS int64 `json:"dereference_ms"`
	TotalMS       int64 `json:"total_ms"`
}

// Retrieve runs the five-step query pipeline and returns the hits.
// Errors surface as-is — callers decide whether to 500 or fall back
// to a degraded response.
func (r *Retriever) Retrieve(ctx context.Context, req Request) (*Result, error) {
	if req.Question == "" {
		return nil, errors.New("retrieve: empty question")
	}
	req.Filters.Sources = trimFilterValues(req.Filters.Sources)
	req.Filters.Actors = trimFilterValues(req.Filters.Actors)
	eachSource := len(req.Filters.Sources) != 1
	topK := req.TopK
	switch {
	case eachSource && topK <= 0:
		topK = EachSourceDefault
	case eachSource:
		topK = min(topK, EachSourceMax)
	case topK <= 0:
		topK = 20
	}

	v := r.vectors.Load()
	if v == nil {
		return nil, ErrNotSetUp
	}

	total := time.Now()
	out := &Result{}

	// 1. Embed the question.
	t := time.Now()
	vecs, err := v.embedder.Embed(ctx, []string{req.Question})
	if err != nil {
		return nil, fmt.Errorf("embed question: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("embedder returned %d vectors, want 1", len(vecs))
	}
	queryVec := vecs[0]
	out.Latency.EmbedMS = time.Since(t).Milliseconds()
	out.EmbedModelID = v.embedder.ID()

	// 2. qdrant.Search — over-fetch so dedup has headroom.
	t = time.Now()
	excluded, err := r.conversationChunkIDs(ctx, req.Filters.ExcludeConversations)
	if err != nil {
		return nil, fmt.Errorf("resolve excluded conversations: %w", err)
	}
	filters := qdrantstore.SearchFilters{
		Sources:         req.Filters.Sources,
		Actors:          req.Filters.Actors,
		OccurredAfter:   req.Filters.OccurredAfter,
		OccurredBefore:  req.Filters.OccurredBefore,
		AllowedScopes:   req.Filters.AllowedScopes,
		ExcludePointIDs: excluded,
	}
	var rawHits []qdrantstore.Hit
	if eachSource {
		// The same headroom for each source as a search of one source has:
		// an event's chunks are folded into its best one, and a long page
		// can put dozens of chunks ahead of the next event.
		rawHits, err = v.store.SearchEachSource(ctx, queryVec, filters, r.overfetch)
		// Best first overall, so that the fold below keeps each event's
		// best chunk and each source's best events.
		sort.SliceStable(rawHits, func(i, j int) bool { return rawHits[i].Score > rawHits[j].Score })
	} else {
		rawHits, err = v.store.Search(ctx, queryVec, filters, r.overfetch)
	}
	if err != nil {
		return nil, fmt.Errorf("qdrant search: %w", err)
	}
	out.Latency.SearchMS = time.Since(t).Milliseconds()

	// 3. Dedup by event_id — keep the best-scoring chunk per event.
	// qdrant returns hits already sorted by score desc, so the
	// first time we see a given event_id is the one to keep.
	seen := make(map[int64]bool, len(rawHits))
	deduped := make([]qdrantstore.Hit, 0, len(rawHits))
	kept := map[string]int{}
	for _, h := range rawHits {
		if h.EventID == 0 {
			// Missing payload — qdrant row without event_id. Skip
			// rather than try to dereference garbage.
			r.logger.Warn("retrieve: qdrant hit with no event_id", "chunk_id", h.ChunkID)
			continue
		}
		if seen[h.EventID] {
			continue
		}
		seen[h.EventID] = true
		// More than is wanted from each source goes to Postgres: an event
		// deleted or moved out of scope since it was indexed is dropped
		// there, and must not cost its source a place. (A search of one
		// source has no Source on its hits and counts them together.)
		if kept[h.Source] >= topK+staleHeadroom {
			continue
		}
		kept[h.Source]++
		deduped = append(deduped, h)
	}
	if len(deduped) == 0 {
		out.Latency.TotalMS = time.Since(total).Milliseconds()
		return out, nil
	}

	// 4. Dereference — one SELECT with JOIN pulling chunk content
	// + event metadata for the whole deduped set.
	t = time.Now()
	hits, err := r.dereference(ctx, deduped, req.Filters.AllowedScopes)
	if err != nil {
		return nil, fmt.Errorf("dereference: %w", err)
	}
	out.Latency.DereferenceMS = time.Since(t).Milliseconds()
	// hits are best first; keep each source's best topK of those that are
	// still there to be read.
	returned := map[string]int{}
	readable := hits[:0]
	for _, h := range hits {
		if returned[h.Source] >= topK {
			continue
		}
		returned[h.Source]++
		readable = append(readable, h)
	}
	out.Hits = readable
	out.Latency.TotalMS = time.Since(total).Milliseconds()
	return out, nil
}

// dereference executes a single SELECT pulling chunk content +
// parent event metadata for the supplied qdrant hits. Filters out
// deleted events. Durable Qdrant deletion is asynchronous, so a tombstoned
// row can briefly still have a live derived point.
//
// allowedScopes re-applies the visibility filter against the
// authoritative Postgres scope; an empty set matches nothing. The qdrant payload
// filter already excluded out-of-scope points, but the payload is
// a denormalized copy that can lag while a queued delta drains; the
// events.scope column is the source of truth and this clause is
// what guarantees no stale-payload leak.
func (r *Retriever) dereference(ctx context.Context, qHits []qdrantstore.Hit, allowedScopes []string) ([]Hit, error) {
	// Preserve qdrant's score ordering: SELECT ... WHERE id = ANY($1)
	// returns in arbitrary order, so we build an id-index map and
	// re-sort at the end.
	chunkIDs := make([]int64, len(qHits))
	scoreByChunk := make(map[uint64]float32, len(qHits))
	for i, h := range qHits {
		chunkIDs[i] = int64(h.ChunkID)
		scoreByChunk[h.ChunkID] = h.Score
	}

	// LEFT JOIN slack_message_details + slack_channel_info: only
	// matches when the event is a slack row. coalesce(ci.name, '')
	// gives '' for both non-slack events and slack events whose
	// channel hasn't been directory-cached yet. The channel name
	// is read live at query time so a channel rename reflects
	// within ~5min (next reconcile) without any backfill against
	// existing events rows.
	derefSQL := `
		SELECT
			c.id,
			c.content,
			e.id, e.external_id, e.source,
			COALESCE(e.actor, ''),
			COALESCE(e.actor_canonical, ''),
			COALESCE(e.actor_id, ''),
			COALESCE(e.thread_id, ''),
			e.occurred_at,
			COALESCE(e.title, ''),
			COALESCE(e.url, ''),
			(SELECT count(*) FROM event_chunks WHERE event_id = e.id) AS chunk_count,
			COALESCE(ci.name, '') AS channel_name,
			c.kind,
			COALESCE(ed.filename, ''),
			COALESCE(ed.page_count, 0),
			CASE WHEN ed.id IS NULL THEN ''
			     WHEN ed.incomplete_reason IS NOT NULL THEN 'partial'
			     WHEN ed.rejected_reason IS NOT NULL THEN 'rejected'
			     ELSE 'complete' END,
			COALESCE(ed.incomplete_reason, ''),
			COALESCE(ed.pages_extracted, 0)
		FROM event_chunks c
		JOIN events e ON e.id = c.event_id
		LEFT JOIN slack_message_details d ON d.event_id = e.id AND e.source = 'slack'
		LEFT JOIN slack_channel_info ci ON ci.channel_id = d.channel_id
		LEFT JOIN event_documents ed ON ed.id = c.event_document_id
		WHERE c.id = ANY($1)
		  AND e.deleted = false AND e.scope = ANY($2)`
	args := []any{chunkIDs, events.ReadableScopes(allowedScopes)}
	rows, err := r.pool.Query(ctx, derefSQL, args...)
	if err != nil {
		return nil, fmt.Errorf("dereference query: %w", err)
	}
	defer rows.Close()

	out := make([]Hit, 0, len(qHits))
	for rows.Next() {
		var h Hit
		var chunkID int64
		if err := rows.Scan(
			&chunkID, &h.Content,
			&h.EventID, &h.ExternalID, &h.Source,
			&h.Actor, &h.ActorCanonical, &h.ActorID, &h.ThreadID, &h.OccurredAt,
			&h.Title, &h.URL,
			&h.ChunkCount,
			&h.ChannelName,
			&h.Kind,
			&h.DocumentFilename,
			&h.DocumentPageCount,
			&h.DocumentCompleteness,
			&h.DocumentIncompleteWhy,
			&h.DocumentPagesExtracted,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		h.ChunkID = uint64(chunkID)
		h.Score = scoreByChunk[h.ChunkID]
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Re-sort by score desc to undo the arbitrary ANY() order.
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}
