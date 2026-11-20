// Package files implements the caption worker: a leader-only async
// worker that fetches image bytes from each ingested image source,
// mirrors them to S3 (content-addressed by sha256), generates a
// VLM caption, and emits a sibling event_chunks row of
// kind='image_caption' that the embedder picks up on its next
// sweep.
//
// One row → one transaction. The pipeline is collapsed from the
// earlier two-phase shape (mirror first, caption later) because
// event_images keeps all of (storage_key, sha256, caption_model,
// captioned_at, chunk_id) on one row — atomicity is straightforward.
//
// The chunk_idx allocation under the (event_id, kind, chunk_idx)
// unique constraint is the only non-trivial concurrency concern.
// The worker holds a row-level lock on the parent events row for
// the brief tx that allocates an image_caption chunk_idx, so
// concurrent captions on the same event serialize cleanly. Body
// chunks are unaffected because their kind sequence is independent.
package files

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
)

// imageMimePrefix is used when normalizing unresolved image types.
const imageMimePrefix = "image/"

// supportedGeminiImageTypes is the exact set of image content
// types Gemini's generateContent endpoint accepts, per Google's
// documentation. Anything image/* outside
// this set (image/svg+xml and image/gif, for example) 400s with
// "Provided image is not valid" — not worth a VLM round trip.
var supportedGeminiImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/heic": true,
	"image/heif": true,
}

// CaptionableImage reports whether the worker captions an image of
// mediaType. Sources record only these images, so the worker never
// claims one it would skip.
func CaptionableImage(mediaType string) bool {
	return supportedGeminiImageTypes[normalizeImageType(mediaType)]
}

// normalizeImageType canonicalizes a Content-Type for
// supportedGeminiImageTypes. Content-Type is case-insensitive and may
// carry a "; charset=..." parameter (both legal per RFC 9110), and the
// map is an exact-match set. image/jpg is a common alias of image/jpeg.
func normalizeImageType(ct string) string {
	normalized := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	if normalized == "image/jpg" {
		return "image/jpeg"
	}
	return normalized
}

// Config governs polling cadence + concurrency. Same shape as the
// embedder's Config so operator mental models stay consistent.
type Config struct {
	// Concurrency is the number of goroutines doing the
	// fetch+caption pipeline in parallel. Default 4: generous for
	// the ongoing rate of image-bearing messages, quick enough to
	// drain a historical backfill, and well inside a hosted vision
	// model's usual quota.
	Concurrency int
	// PollInterval is the sleep when the queue is empty. Default
	// 10s — caption latency is not user-facing (the events row
	// is searchable on text immediately; the caption lands a few
	// seconds later when the worker catches up).
	PollInterval time.Duration
	// MinBackoff / MaxBackoff govern the loop's exponential
	// backoff after errors. Default 5s / 60s.
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

func (c *Config) fillDefaults() {
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 10 * time.Second
	}
	if c.MinBackoff <= 0 {
		c.MinBackoff = 5 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 60 * time.Second
	}
}

// SourceFetcher resolves source-specific bytes for an image. The
// worker uses one fetcher per source. Slack's implementation pulls
// url_private with the bot token; agent_session's (when shipped
// via the session feed) will hit a feed API endpoint.
//
// Implementations should wrap transient failures (rate limit, 5xx,
// network blip) in *TransientError so the worker's loop sleeps +
// retries instead of bumping failure_count. See transient.go.
type SourceFetcher interface {
	// FetchImage returns the (raw bytes, content-type) for a
	// source-specific image identifier. The same identifier flows
	// through event_images.source_image_id; mediaType carries the
	// same row's media_type so fetchers whose resolution is
	// media-type-sensitive (agent_session's content-addressed S3
	// lookup) can go straight to the right key instead of probing.
	FetchImage(ctx context.Context, sourceImageID string, mediaType string) (data []byte, contentType string, err error)
}

// AvailableFetcher is a SourceFetcher that is sometimes unable to fetch,
// such as one a connector lends only while it runs. Rows of its source are
// not claimed while Available reports false, so they don't spend their
// failure budget.
type AvailableFetcher interface {
	Available() bool
}

// S3Mirror writes image bytes to the diderot images bucket and
// reads them back for the captioning phase + the file proxy on
// every replica. Implementation uses the AWS SDK; injected as an
// interface so tests can fake it.
type S3Mirror interface {
	// Put writes data to the configured bucket at key with the
	// given Content-Type. Idempotent on retry: the same (key,
	// bytes) pair produces the same final S3 object.
	Put(ctx context.Context, key string, contentType string, data []byte) error
	// Get reads bytes back. Returns (bytes, content-type, err).
	Get(ctx context.Context, key string) (data []byte, contentType string, err error)
	// Bucket reports the configured bucket name; used in log
	// lines and metric labels.
	Bucket() string
}

// Worker drives one captioning pipeline. Construct, then call Run
// from a leader term (cmd/diderot's runLeaderTerm). Run blocks until
// ctx is cancelled.
type Worker struct {
	Pool      *pgxpool.Pool
	S3        S3Mirror
	Captioner Captioner
	// Fetchers maps source name (events.source value) to its
	// implementation. Today only "slack" is wired; "agent_session"
	// is gated on the session feed exposing image bytes.
	Fetchers map[string]SourceFetcher
	Logger   *slog.Logger
	Registry prometheus.Registerer
	Config   Config

	metrics  *workerMetrics
	redactor *secretRedactor
}

// Run launches Concurrency goroutines all draining the same queue.
// FOR UPDATE SKIP LOCKED ensures they don't double-claim. Returns
// ctx.Err() when ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.Config.fillDefaults()
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	if w.Registry == nil {
		w.Registry = prometheus.NewRegistry()
	}
	w.metrics = newWorkerMetrics(w.Registry)

	// A caption worker that can't redact secrets must not caption: what
	// it wrote would be indexed and embedded unredacted.
	redactor, err := newSecretRedactor()
	if err != nil {
		return fmt.Errorf("files: secret redactor: %w", err)
	}
	w.redactor = redactor

	if w.Pool == nil {
		return errors.New("files: Pool is required")
	}
	if w.S3 == nil {
		return errors.New("files: S3 is required")
	}
	if w.Captioner == nil {
		return errors.New("files: Captioner is required")
	}
	if len(w.Fetchers) == 0 {
		return errors.New("files: at least one SourceFetcher required")
	}

	var wg sync.WaitGroup
	for i := 0; i < w.Config.Concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			w.runLoop(ctx, id)
		}(i)
	}
	wg.Wait()
	return ctx.Err()
}

// runLoop drains the uncaptioned queue. Each iteration: claim one
// row → fetch bytes → mirror to S3 → caption → write chunk + update
// row in one tx. Failures bump failure_count on terminal errors;
// transient errors retry the SAME claim until success or terminal,
// so the row's claimed_at stays non-stale across the retry window
// and other workers don't try to re-claim.
func (w *Worker) runLoop(ctx context.Context, id int) {
	backoff := w.Config.MinBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		w.updateGauges(ctx)
		row, err := w.claimUncaptioned(ctx)
		if err != nil {
			w.Logger.Warn("files: claim failed", "worker", id, "err", err)
			if sleepCtx(ctx, backoff) != nil {
				return
			}
			backoff = nextBackoff(backoff, w.Config.MaxBackoff)
			continue
		}
		if row == nil {
			if sleepCtx(ctx, w.Config.PollInterval) != nil {
				return
			}
			continue
		}
		backoff = w.Config.MinBackoff
		// Inner retry on transient. On each transient, refresh the
		// claim's claimed_at so a long retry sequence doesn't trip
		// the staleClaimAge cutoff. Bail to the outer loop only on
		// success, terminal failure, or maxTransientRetries hit.
		//
		// The maxTransientRetries cap prevents a single row from
		// monopolizing one worker if its transient errors don't
		// clear (e.g., a Vertex finishReason=UNSPECIFIED that
		// always recurs on this specific image). After the cap we
		// release the row to the outer loop where staleClaimAge
		// recovery + a fresh claim will pick it up later, behind
		// other queue work.
		const maxTransientRetries = 5
		transients := 0
		for {
			err := w.captionOne(ctx, row)
			if err == nil {
				w.metrics.captionSuccess.Inc()
				break
			}
			if errors.Is(err, errClaimSuperseded) {
				// Another worker's stale-claim recovery already
				// re-claimed (and may have already committed) this
				// row. Nothing on the row is wrong, so this isn't a
				// failure — don't bump failure_count — it's just no
				// longer this worker's to finish.
				w.Logger.Info("files: claim superseded by another worker; discarding this attempt",
					"worker", id, "image_id", row.imageID)
				break
			}
			if IsTransient(err) {
				transients++
				if transients > maxTransientRetries {
					w.Logger.Warn("files: transient retries exhausted; bumping + releasing",
						"worker", id, "image_id", row.imageID,
						"transients", transients, "err", err)
					w.metrics.captionTransient.Inc()
					// Bump failure_count + clear claimed_at. We
					// override the "skip transient" rule here because
					// a row that's persistently transient should
					// eventually hit the poison cap, where an operator
					// can requeue it later by resetting failure_count
					// (and claimed_at, if stuck). Without this,
					// persistently-transient rows would loop forever.
					if uerr := w.recordExhaustion(ctx, row.imageID, err); uerr != nil {
						w.Logger.Error("files: exhaustion bump failed",
							"image_id", row.imageID, "err", uerr)
					}
					break
				}
				w.Logger.Warn("files: caption transient; retrying same claim",
					"worker", id, "image_id", row.imageID,
					"transients", transients, "err", err)
				w.metrics.captionTransient.Inc()
				if sleepCtx(ctx, backoff) != nil {
					return
				}
				backoff = nextBackoff(backoff, w.Config.MaxBackoff)
				if uerr := w.refreshClaim(ctx, row); uerr != nil {
					if errors.Is(uerr, errClaimSuperseded) {
						w.Logger.Info("files: claim superseded during refresh; discarding this attempt",
							"worker", id, "image_id", row.imageID)
					} else {
						// If the refresh itself fails (DB hiccup), bail
						// to outer loop; the claim will go stale and
						// another worker will pick the row up after the
						// staleClaimAge window.
						w.Logger.Warn("files: claim refresh failed; releasing to outer loop",
							"image_id", row.imageID, "err", uerr)
					}
					break
				}
				continue
			}
			w.Logger.Warn("files: caption failed",
				"worker", id, "image_id", row.imageID, "err", err)
			if uerr := w.bumpFailure(ctx, row.imageID, err); uerr != nil {
				w.Logger.Error("files: bump failure failed",
					"image_id", row.imageID, "err", uerr)
			}
			w.metrics.captionFailures.Inc()
			break
		}
	}
}

// refreshClaim resets claimed_at to now() for a row this worker
// is mid-retry on. Without it, a long transient-retry sequence
// would let claimed_at age past staleClaimAge while we're still
// actively working the row, allowing a second worker to claim it
// and produce duplicate VLM calls.
//
// The UPDATE only matches while the row's stored claimed_at still equals
// row.claimedAt — the same fencing token commitCaption checks. Without
// this guard, a worker whose claim has ALREADY been stolen (its row went
// stale and a second worker re-claimed it while this one was blocked
// elsewhere, e.g. a slow error path between transient attempts) would
// blindly stamp claimed_at = now() over the new owner's claim, making its
// own later, superseded commit look current again and defeating
// commitCaption's check entirely. On success, row.claimedAt is advanced
// to the new value so the next refresh or commit fences against it.
func (w *Worker) refreshClaim(ctx context.Context, row *claimRow) error {
	var newClaimedAt time.Time
	err := w.Pool.QueryRow(ctx, `
		UPDATE event_images SET claimed_at = now()
		 WHERE id = $1 AND claimed_at = $2
		RETURNING claimed_at
	`, row.imageID, row.claimedAt).Scan(&newClaimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return errClaimSuperseded
	}
	if err != nil {
		return err
	}
	row.claimedAt = newClaimedAt
	return nil
}

// claimRow is the per-iteration unit of work.
type claimRow struct {
	imageID       int64  // event_images.id (the auto-generated PK)
	eventID       int64  // event_images.event_id (the parent event)
	source        string // events.source ('slack', 'agent_session', ...)
	sourceImageID string // event_images.source_image_id
	mediaType     string
	storageKey    string // empty until the worker mirrors bytes
	// claimedAt is event_images.claimed_at as stamped by the claim this
	// worker is holding (claimUncaptioned), updated whenever refreshClaim
	// renews it. Every write that finalizes this row (commitCaption, the
	// unsupported-format skip stamp) is conditioned on claimed_at still
	// matching this value — a fencing token proving the worker still
	// holds the claim it took. See errClaimSuperseded.
	claimedAt time.Time
}

// errClaimSuperseded means the claim this worker started with (or last
// refreshed) is no longer the row's current claimed_at: another worker's
// stale-claim recovery already re-claimed this row — and, since then, may
// have committed its own caption. A commit is only valid while the
// committing worker still holds the claim it took, so a worker that sees
// this must treat the row as someone else's responsibility now: not a
// failure (nothing on this row is wrong), and not something to bump
// failure_count over.
var errClaimSuperseded = errors.New("files: claim superseded by another worker")

// staleClaimAge is how long a row's claimed_at can be before
// another worker considers the claim abandoned and re-claims.
// A worker that dies mid-LLM leaves claimed_at set but captioned_at
// NULL; without recovery the row would be permanently stuck. Five
// minutes is well past the captioner's typical end-to-end (~10s
// p50, ~30s p99) so a healthy worker never trips this.
const staleClaimAge = 5 * time.Minute

// claimUncaptioned atomically claims one event_images row by
// stamping claimed_at = now(). The atomic UPDATE-with-CTE pattern
// ensures concurrent workers don't double-claim: the
// FOR UPDATE SKIP LOCKED in the inner SELECT serializes the choice
// of row across in-flight UPDATE statements, and the outer UPDATE
// sets claimed_at so subsequent claims (in separate transactions)
// see the row as in-flight and skip it.
//
// Pool.QueryRow runs in an implicit single-statement transaction;
// that's fine here because the lock + UPDATE happen in the SAME
// statement. The race "lock released between SELECT and UPDATE"
// that breaks naive `SELECT FOR UPDATE SKIP LOCKED` against a
// connection pool can't apply.
//
// Ordering is newest-first (occurred_at DESC, id as tiebreaker),
// not insertion order. A live @-mention screenshot occurs "now" and
// should jump ahead of a backfill's uncaptioned backlog — the
// interactive caller is blocked on the ask caption barrier's 20s
// deadline (captionbarrier.go), the backfill isn't waiting on
// anyone. occurred_at is denormalized onto event_images precisely
// so this can stay a single-table index scan with no join.
//
// A row needs its source's fetcher only while its bytes are not in the
// mirror. One that is mirrored (an image queued to be described again) is
// claimed whether or not its source is running: captionOne reads the
// mirror first.
//
// Stale-claim recovery: claimed_at < now() - staleClaimAge is
// re-claimable. A worker that died mid-LLM has its row picked up
// on the next sweep without any explicit cleanup pass.
//
// Returns (nil, nil) when the queue is empty.
func (w *Worker) claimUncaptioned(ctx context.Context) (*claimRow, error) {
	sources := make([]string, 0, len(w.Fetchers))
	for source, fetcher := range w.Fetchers {
		if available, ok := fetcher.(AvailableFetcher); ok && !available.Available() {
			continue
		}
		sources = append(sources, source)
	}
	var r claimRow
	var storage *string
	err := w.Pool.QueryRow(ctx, `
		UPDATE event_images
		   SET claimed_at = now()
		 WHERE id = (
		   SELECT ei.id
		     FROM event_images ei
		    WHERE ei.captioned_at IS NULL
		      AND ei.failure_count < $1
		      AND EXISTS (SELECT 1 FROM events e WHERE e.id = ei.event_id AND NOT e.deleted
			                     AND (ei.storage_key IS NOT NULL OR e.source = ANY($3::text[])))
		      AND (ei.claimed_at IS NULL OR ei.claimed_at < now() - make_interval(secs => $2))
		      AND `+SlackChannelNotBlocked("ei.event_id")+`
		    ORDER BY ei.occurred_at DESC, ei.id
		    LIMIT 1
		    FOR UPDATE SKIP LOCKED
		 )
		RETURNING id, event_id,
		          (SELECT source FROM events WHERE id = event_images.event_id),
		          source_image_id, media_type, storage_key, claimed_at
	`, events.MaxFailures, staleClaimAge.Seconds(), sources).Scan(
		&r.imageID, &r.eventID, &r.source,
		&r.sourceImageID, &r.mediaType, &storage, &r.claimedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if storage != nil {
		r.storageKey = *storage
	}
	return &r, nil
}

// captionOne runs the full pipeline for one image: fetch bytes,
// mirror to S3 (if not already), caption, write the chunk + update
// the row.
func (w *Worker) captionOne(ctx context.Context, row *claimRow) error {
	timer := w.metrics.startCaptionTimer()

	// Step 1: get bytes. Prefer our own S3 mirror when a prior pass
	// already stored them (storage_key set): the mirror is
	// content-addressed and immutable, the read is ~100x faster than
	// a source fetch (Slack's files.info + CDN GET averages ~7s and
	// eats the shared workspace API budget), and — decisively — the
	// source copy may no longer exist at all. Slack's 12-month file
	// retention deletes the upstream file while our mirror keeps the
	// bytes; a recaption pass that insisted on re-fetching would
	// poison every row older than that which was already captioned once. Fall back
	// to the per-source fetcher when there's no key or the S3 read
	// fails (bucket rot shouldn't strand a row the source can still
	// serve).
	var (
		data       []byte
		ct         string
		fromMirror bool
	)
	if row.storageKey != "" {
		s3Start := time.Now()
		b, mct, gerr := w.S3.Get(ctx, row.storageKey)
		if gerr == nil {
			data, ct, fromMirror = b, mct, true
			w.metrics.observeStage("fetch", "ok", s3Start)
		} else {
			w.metrics.observeStage("fetch", "err", s3Start)
			w.Logger.Warn("files: mirror read failed; falling back to source fetch",
				"image_id", row.imageID, "storage_key", row.storageKey, "err", gerr)
		}
	}
	if !fromMirror {
		fetcher, ok := w.Fetchers[row.source]
		if !ok {
			timer.done("no_fetcher")
			return fmt.Errorf("no fetcher for source %q", row.source)
		}
		fetchStart := time.Now()
		var err error
		data, ct, err = fetcher.FetchImage(ctx, row.sourceImageID, row.mediaType)
		if err != nil {
			w.metrics.observeStage("fetch", "err", fetchStart)
			timer.done("fetch_err")
			return fmt.Errorf("fetch %s/%s: %w", row.source, row.sourceImageID, err)
		}
		w.metrics.observeStage("fetch", "ok", fetchStart)
	}
	if ct == "" {
		ct = row.mediaType
	}

	// Unsupported-format short-circuit: Gemini's image inputs are
	// an exact allowlist (see supportedGeminiImageTypes). ct is the
	// authoritative type now that the fetch has run — prefer it
	// over row.mediaType, which for some sources (notion's
	// extensionless fallback) is only a best-effort guess.
	//
	// Canonicalize before the lookup: an uncanonicalized "image/PNG"
	// or "image/png; charset=binary" would otherwise false-positive
	// as unsupported and lose a caption we could have produced.
	//
	// An unresolved ct ("image/unknown", "", bare "image/" with no
	// subtype, or empty/octet-stream) is treated as POTENTIALLY
	// supported rather than skipped: only a definitively-resolved,
	// definitively-unsupported type earns the skip. This is
	// deliberately NOT scoped to the image/* prefix — a fetch that
	// authoritatively resolves to something like text/html (e.g. an
	// external URL that 200s with an error page) is just as
	// unsupported as image/svg+xml, and skipping it here avoids the
	// S3 write for bytes the captioner will reject anyway.
	normalizedCT := normalizeImageType(ct)
	resolvedCT := normalizedCT != "" &&
		normalizedCT != "application/octet-stream" &&
		normalizedCT != imageMimePrefix &&
		normalizedCT != "image/unknown"
	if resolvedCT && !supportedGeminiImageTypes[normalizedCT] {
		w.Logger.Info("files: skipping unsupported image format",
			"image_id", row.imageID, "content_type", ct)
		// Same claim-ownership guard as commitCaption's step 3: this
		// also finalizes captioned_at, so it must not stamp a row
		// another worker has already re-claimed (or already captioned)
		// since this worker's claim.
		tag, err := w.Pool.Exec(ctx, `
			UPDATE event_images
			   SET captioned_at  = now(),
			       caption_model = 'skipped:unsupported-format'
			 WHERE id = $1 AND claimed_at = $2
		`, row.imageID, row.claimedAt)
		if err != nil {
			timer.done("skipped_unsupported_format_err")
			return err
		}
		if tag.RowsAffected() == 0 {
			timer.done("skipped_unsupported_format_superseded")
			return errClaimSuperseded
		}
		w.metrics.captionSkipped.WithLabelValues("unsupported_format").Inc()
		timer.done("skipped_unsupported_format")
		return nil
	}
	// Propagate the canonicalized form downstream: S3.Put and
	// Captioner.Caption should see the same normalized type the
	// allowlist just validated, not a charset-suffixed or
	// differently-cased original that Gemini might not recognize.
	// Only overwrite when the fetch actually resolved a type —
	// preserve the "" / octet-stream / image/unknown / bare
	// "image/" placeholders as-is so the fallback to row.mediaType
	// (line 423) and the Captioner's own mediaType handling are
	// unaffected for the unresolved case.
	if resolvedCT {
		ct = normalizedCT
	}

	// Step 2: mirror to S3 at a content-addressed key. Hash first
	// so the key is deterministic; idempotent on retry (PutObject
	// overwrites). Skipped when the bytes came FROM the mirror —
	// re-putting what we just read is a no-op write, and the
	// existing key stays authoritative.
	hash := sha256Hex(data)
	key := row.storageKey
	if !fromMirror {
		key = storageKeyFor(row.source, hash, extensionFor(ct))
		s3Start := time.Now()
		if err := w.S3.Put(ctx, key, ct, data); err != nil {
			w.metrics.observeStage("s3_put", "err", s3Start)
			timer.done("s3_put_err")
			return fmt.Errorf("s3 put %s: %w", key, err)
		}
		w.metrics.observeStage("s3_put", "ok", s3Start)
	}

	// Step 3: caption. The vision model, with the caption prompt,
	// returns a structured CaptionResult.
	vlmStart := time.Now()
	result, err := w.Captioner.Caption(ctx, ct, data)
	if err != nil {
		w.metrics.observeStage("vlm", "err", vlmStart)
		timer.done("caption_err")
		return fmt.Errorf("caption %s: %w", row.sourceImageID, err)
	}
	if result == nil || strings.TrimSpace(result.Caption) == "" {
		// Empty CAPTION section is poison — the cheap-tier model
		// declined to engage. An operator can requeue the row by
		// resetting event_images.failure_count (and claimed_at, if
		// stuck) so the worker re-runs it.
		w.metrics.observeStage("vlm", "err", vlmStart)
		timer.done("empty_caption")
		return errors.New("captioner returned empty caption")
	}
	w.metrics.observeStage("vlm", "ok", vlmStart)

	// Step 4: flatten + truncate. The flat string is what the
	// embedder reads; truncated_at on the row tells the operator
	// whether the cheap tier hit the 1500-rune cap.
	flat := FlattenCaption("", result)
	// Redact before truncation so the [REDACTED:...] marker can't
	// itself get truncated away, and so the content that lands in
	// event_chunks (and gets embedded) is already clean. Scans the
	// whole flat string, not just the Text: section — a secret could
	// in theory land in the caption sentence too.
	var redactions []string
	flat, redactions = w.redactor.Redact(flat)
	if len(redactions) > 0 {
		w.Logger.Warn("files: redacted secrets from caption",
			"image_id", row.imageID,
			"event_id", row.eventID,
			"rule_ids", redactions,
			"count", len(redactions))
		w.metrics.captionRedactions.Add(float64(len(redactions)))
	}
	flat, truncated := ApplyTruncation(flat)

	// Step 5: tx — chunk_idx allocation under the new
	// (event_id, kind, chunk_idx) constraint requires a row-
	// exclusive lock on the parent event so concurrent captions
	// on the same event serialize cleanly. Body chunks are
	// untouched (different kind, different sequence).
	commitStart := time.Now()
	if err := w.commitCaption(ctx, row, hash, len(data), key, flat, result, truncated); err != nil {
		w.metrics.observeStage("commit", "err", commitStart)
		timer.done("commit_err")
		return err
	}
	w.metrics.observeStage("commit", "ok", commitStart)
	timer.done("ok")
	w.Logger.Info("files: captioned",
		"image_id", row.imageID,
		"event_id", row.eventID,
		"source", row.source,
		"model", w.Captioner.ModelID(),
		"caption_runes", len([]rune(flat)),
		"truncated", truncated,
		"unread", result.Unread)
	w.metrics.chunksEmitted.Inc()
	return nil
}

// commitCaption is the load-bearing transaction. Runs:
//
//  1. SELECT id, ingested_at FROM events WHERE id=$1 FOR UPDATE —
//     row-exclusive lock on the parent event so concurrent captions
//     allocate chunk_idx sequentially. Also captures ingested_at for
//     the diderot_image_ingested_to_captioned_seconds metric.
//  2. INSERT INTO event_chunks (event_id, kind, chunk_idx,
//     content) — chunk_idx is computed as MAX existing + 1
//     scoped to this event AND kind='image_caption'. The
//     (event_id, kind, chunk_idx) unique constraint provides
//     defense-in-depth.
//  3. UPDATE event_images — pin the chunk_id FK, set storage_key,
//     sha256, bytes, caption_model, captioned_at, unread_flagged,
//     truncated_at. Conditioned on claimed_at = row.claimedAt: the
//     commit only applies while this worker still holds the claim it
//     took (see errClaimSuperseded).
//  4. DELETE the caption chunk the row pointed at before, if any: the
//     image was queued to be described again, and its old caption stayed
//     searchable until this replacement.
func (w *Worker) commitCaption(
	ctx context.Context,
	row *claimRow,
	hash string,
	size int,
	storageKey string,
	flatContent string,
	result *CaptionResult,
	truncated bool,
) error {
	tx, err := w.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Step 1: lock the parent event row. Fails with ErrNoRows if
	// the event got deleted between claim and now (slack-side
	// retention or hard-delete elsewhere). In that case roll back
	// silently — the event_images row will cascade away on the
	// next sweep. A soft delete (events.deleted) that landed after
	// the claim also aborts: the delete path already dropped the
	// event's chunks, and the embedder skips deleted events, so a
	// caption committed now would sit unembedded forever. The lock
	// serializes with the delete transaction, so the flag read here
	// is final.
	// We also read ingested_at here so we can observe the
	// ingested→captioned latency metric after a successful commit,
	// without an extra round-trip. ingested_at is set by the DB
	// DEFAULT (now()) on INSERT and is never updated; reading it
	// inside this lock is equivalent to reading it from the
	// immutable stored value.
	var (
		eventID    int64
		ingestedAt time.Time
		deleted    bool
	)
	if err := tx.QueryRow(ctx, `
		SELECT id, ingested_at, deleted FROM events WHERE id = $1 FOR UPDATE
	`, row.eventID).Scan(&eventID, &ingestedAt, &deleted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("parent event vanished mid-caption")
		}
		return fmt.Errorf("lock event: %w", err)
	}
	if deleted {
		return errors.New("parent event deleted mid-caption")
	}

	// Step 2: allocate chunk_idx + INSERT. The COALESCE handles
	// the first-image case where MAX returns NULL.
	var chunkID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO event_chunks
			(event_id, kind, chunk_idx, content, occurred_at)
		SELECT $1, 'image_caption',
		       COALESCE(MAX(chunk_idx), -1) + 1,
		       $2,
		       (SELECT occurred_at FROM events WHERE id = $1)
		  FROM event_chunks
		 WHERE event_id = $1 AND kind = 'image_caption'
		RETURNING id
	`, row.eventID, flatContent).Scan(&chunkID)
	if err != nil {
		return fmt.Errorf("insert chunk: %w", err)
	}

	// Step 3: stamp the event_images row and learn which chunk it
	// pointed at before — but only while this worker still holds the
	// claim it took (claimed_at = row.claimedAt). Without that guard, a
	// worker whose claim went stale while this same captionOne attempt
	// was still running (a hung fetch/VLM call outliving staleClaimAge)
	// could commit here even after a second worker already re-claimed
	// the row and committed its own, newer caption — silently
	// overwriting it and, in step 4 below, deleting its chunk. The
	// claimed_at check makes that commit a no-op instead: it can only
	// match if no one has re-claimed this row since.
	var previousChunk *int64
	err = tx.QueryRow(ctx, `
		UPDATE event_images i
		   SET storage_key    = $2,
		       sha256         = decode($3, 'hex'),
		       bytes          = $4,
		       caption_model  = $5,
		       captioned_at   = now(),
		       unread_flagged = $6,
		       truncated_at   = CASE WHEN $7 THEN now() ELSE NULL END,
		       chunk_id       = $8
		  FROM (SELECT id, chunk_id FROM event_images
		         WHERE id = $1 AND claimed_at = $9 FOR UPDATE) previous
		 WHERE i.id = previous.id
		RETURNING previous.chunk_id
	`, row.imageID, storageKey, hash, int64(size),
		w.Captioner.ModelID(), result.Unread, truncated, chunkID, row.claimedAt).Scan(&previousChunk)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either the row vanished outright, or the claimed_at guard
		// didn't match because another worker re-claimed (and possibly
		// already committed) this row since. Tell the two apart: the
		// row's own existence is unaffected by anything in this
		// still-open, still-uncommitted tx, so a plain read is final.
		var exists bool
		if cerr := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM event_images WHERE id = $1)`, row.imageID,
		).Scan(&exists); cerr != nil {
			return fmt.Errorf("check event_images existence: %w", cerr)
		}
		if !exists {
			return errors.New("event_images row vanished mid-caption")
		}
		return errClaimSuperseded
	}
	if err != nil {
		return fmt.Errorf("update event_images: %w", err)
	}

	// Step 4: an image described again (RequeueOtherModels) had a
	// caption until this moment. Drop it only now, in the transaction
	// that installs its replacement, so the image never goes without a
	// caption; the delete trigger queues the old vector's removal.
	if previousChunk != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM event_chunks WHERE id = $1`, *previousChunk); err != nil {
			return fmt.Errorf("delete replaced caption: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Observe ingested→captioned latency only on successful commit.
	// ingestedAt is the DB-stamped ingested_at from the events row;
	// now() minus it is the wall-clock from first ingest to caption
	// landing, which is the duration the ask caption barrier's 20s
	// deadline bounds. Guard against clock skew (future-dated
	// ingested_at) with a 0-floor.
	lag := time.Since(ingestedAt).Seconds()
	if lag < 0 {
		lag = 0
	}
	w.metrics.ingestedToCaptioned.WithLabelValues(row.source).Observe(lag)

	return nil
}

// bumpFailure records a per-row failure. Truncates the error
// message to keep the column compact. Skips transient errors
// (defense-in-depth — the loop already filters, but this guards
// against future callers).
//
// Known-terminal errors (file deleted from Slack, etc.) jump
// straight to the poison cap so the worker doesn't waste 4 more
// claim cycles on a guaranteed re-failure. Non-terminal errors
// (vertex MAX_TOKENS, opaque slack 5xx that classifySlackError
// missed, etc.) get the gradual +1 bump path so we don't
// poison-pill on a one-off.
func (w *Worker) bumpFailure(ctx context.Context, imageID int64, cause error) error {
	if IsTransient(cause) {
		return nil
	}
	msg := truncateRunes(cause.Error(), 512)
	target := `failure_count + 1`
	if isPermanentTerminal(cause) {
		target = fmt.Sprintf("%d", events.MaxFailures)
	}
	_, err := w.Pool.Exec(ctx, fmt.Sprintf(`
		UPDATE event_images
		   SET failure_count  = %s,
		       last_failure   = $2,
		       last_failed_at = now()
		 WHERE id = $1
	`, target), imageID, msg)
	return err
}

// isPermanentTerminal classifies known-terminal errors that there's
// no value in retrying. Typed *PermanentError is checked first
// (e.g. notion's relative/schemeless image URLs, which have
// nothing to fetch against); the legacy string-match on slack
// files.info file_not_found (file deleted from the workspace;
// never coming back) is retained as a fallback for callers that
// haven't been converted to the typed form.
func isPermanentTerminal(err error) bool {
	if err == nil {
		return false
	}
	var pe *PermanentError
	if errors.As(err, &pe) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "file_not_found") || strings.Contains(msg, "file_deleted")
}

// recordExhaustion is the bumpFailure variant for the
// transient-retries-exhausted case. Bumps failure_count + last_failure
// despite the transient classification (the whole point is that
// retrying isn't getting us anywhere on this row right now), but
// LEAVES claimed_at untouched. The staleClaimAge window (5min) then
// becomes the natural backoff before any worker re-claims this row.
// claimUncaptioned now orders by occurred_at DESC rather than id, so
// a recently-occurred poison row already sorts near the head of the
// queue on its own merits — the staleClaimAge backoff is what stops
// it from being re-claimed the instant this transaction commits and
// burning through the poison cap (5) in under a minute; ordering
// alone wouldn't prevent that.
func (w *Worker) recordExhaustion(ctx context.Context, imageID int64, cause error) error {
	msg := truncateRunes(cause.Error(), 512)
	_, err := w.Pool.Exec(ctx, `
		UPDATE event_images
		   SET failure_count  = failure_count + 1,
		       last_failure   = $2,
		       last_failed_at = now()
		 WHERE id = $1
	`, imageID, msg)
	return err
}

// updateGauges piggy-backs queue-depth metrics onto the loop.
// Throttled so the COUNT(*)s don't fire every iteration.
func (w *Worker) updateGauges(ctx context.Context) {
	if !w.metrics.shouldUpdateGauges() {
		return
	}
	var pending, stranded int64
	_ = w.Pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM event_images
		    WHERE captioned_at IS NULL AND failure_count < $1),
		  (SELECT count(*) FROM event_images
		    WHERE captioned_at IS NULL AND failure_count >= $1)
	`, events.MaxFailures).Scan(&pending, &stranded)
	w.metrics.captionPending.Set(float64(pending))
	w.metrics.captionStranded.Set(float64(stranded))
}

// storageKeyFor returns the canonical S3 key for a given
// (source, sha256) pair. Two-character prefix sharding keeps
// ListBucket usable; the worker doesn't enumerate but operators
// might (e.g., orphan-reaper jobs in v2).
func storageKeyFor(source, sha256Hex, ext string) string {
	if len(sha256Hex) < 2 {
		// Defensive — sha256 is always 64 chars, but a malformed
		// hash shouldn't write to a key that breaks ListBucket.
		return fmt.Sprintf("%s/%s%s", source, sha256Hex, ext)
	}
	return fmt.Sprintf("%s/%s/%s%s", source, sha256Hex[:2], sha256Hex, ext)
}

// extensionFor maps a Content-Type to a file extension for the
// S3 key. Cosmetic only — operators reading the bucket appreciate
// the hint. Falls back to no extension on unrecognized types.
func extensionFor(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// truncateRunes is rune-aware so multi-byte codepoints aren't
// split mid-character (Postgres rejects invalid UTF-8 in text
// columns).
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
