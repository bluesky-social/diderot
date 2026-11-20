package slack

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/bluesky-social/diderot/internal/events"
)

// seedImageRow inserts an event_images row for the message identified
// by (channelID, ts), with the given captioned/failure state. Returns
// the event_images.id so a test can flip captioned_at mid-wait. The
// target event must already exist (seed it via ingestTestMsg first).
func seedImageRow(t *testing.T, pool *pgxpool.Pool, channelID, ts string, captioned bool, failureCount int) int64 {
	t.Helper()
	ctx := context.Background()
	var capExpr any
	if captioned {
		capExpr = time.Now().UTC()
	}
	var imageID int64
	// occurred_at comes from the same events row this image belongs
	// to, keeping the seed consistent with the production
	// ReconcileImages derivation.
	err := pool.QueryRow(ctx, `
		INSERT INTO event_images
			(event_id, source_image_id, media_type, captioned_at, failure_count, occurred_at)
		SELECT id, $2, 'image/png', $3, $4, occurred_at
		  FROM events WHERE external_id = $1
		RETURNING id
	`, channelID+":"+ts, "F_"+ts, capExpr, failureCount).Scan(&imageID)
	if err != nil {
		t.Fatalf("seedImageRow ts=%s: %v", ts, err)
	}
	return imageID
}

// fullWindow is the (since, before) pair used by most barrier tests:
// zero lower bound, far-future upper bound, so any seeded in-scope
// message qualifies.
func fullWindow() (time.Time, time.Time) {
	return time.Time{}, time.Unix(9999999999, 0)
}

// TestWaitForCaptions_ImmediateWhenSettled: an already-captioned
// in-scope image means the first query sees zero pending — return
// {0, false} promptly, no wait.
func TestWaitForCaptions_ImmediateWhenSettled(t *testing.T) {
	pool := getThreadContextPool(t)
	ctx := context.Background()

	const channel = "C_BARR_SETTLED"
	const rootTS = "1700020000.000100"
	ingestTestMsg(t, pool, channel, "U_A", rootTS, "", "with screenshot", "Alice", false)
	seedImageRow(t, pool, channel, rootTS, true /*captioned*/, 0)

	since, before := fullWindow()
	start := time.Now()
	res := waitForAttachments(ctx, pool, channel, rootTS, since, before)
	elapsed := time.Since(start)

	if res.initialPending.total() != 0 {
		t.Errorf("initialPending: got %+v, want 0", res.initialPending)
	}
	if res.timedOut {
		t.Error("timedOut: got true, want false (image was settled)")
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected immediate return, took %s", elapsed)
	}
}

// TestWaitForCaptions_WaitsThenProceeds: an uncaptioned in-scope image
// that gets captioned mid-wait — initialPending reflects the first
// query (1), and the barrier returns timedOut:false once the caption
// lands.
func TestWaitForCaptions_WaitsThenProceeds(t *testing.T) {
	pool := getThreadContextPool(t)
	ctx := context.Background()

	const channel = "C_BARR_LANDS"
	const rootTS = "1700021000.000100"
	ingestTestMsg(t, pool, channel, "U_A", rootTS, "", "with screenshot", "Alice", false)
	imageID := seedImageRow(t, pool, channel, rootTS, false /*uncaptioned*/, 0)

	// Land the caption ~700ms in — after the first poll, before the
	// (default 20s) deadline.
	go func() {
		time.Sleep(700 * time.Millisecond)
		_, _ = pool.Exec(context.Background(),
			`UPDATE event_images SET captioned_at = now() WHERE id = $1`, imageID)
	}()

	since, before := fullWindow()
	start := time.Now()
	res := waitForAttachments(ctx, pool, channel, rootTS, since, before)
	elapsed := time.Since(start)

	if res.initialPending.images != 1 || res.initialPending.documents != 0 {
		t.Errorf("initialPending: got %+v, want 1 image", res.initialPending)
	}
	if res.timedOut {
		t.Error("timedOut: got true, want false (caption landed before deadline)")
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("expected at least one poll (>=500ms), took %s", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("expected return shortly after caption landed, took %s", elapsed)
	}
}

// TestWaitForAttachments_WaitsForDerivedEmbedding proves that caption
// completion is not the end of the pre-Ask barrier. The caption worker first
// commits an unembedded image_caption chunk; only after the embedder stamps
// embedded_at may the turn proceed. This keeps preprocessing time outside the
// Ask/model timeout budget.
func TestWaitForAttachments_WaitsForDerivedEmbedding(t *testing.T) {
	pool := getThreadContextPool(t)
	ctx := context.Background()

	const channel = "C_BARR_EMBED"
	const rootTS = "1700021500.000100"
	ingestTestMsg(t, pool, channel, "U_A", rootTS, "", "with screenshot", "Alice", false)
	imageID := seedImageRow(t, pool, channel, rootTS, false /*captioned*/, 0)

	done := make(chan error, 1)
	go func() {
		time.Sleep(700 * time.Millisecond)
		tx, err := pool.Begin(context.Background())
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		var chunkID int64
		if err = tx.QueryRow(context.Background(), `
			INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
			SELECT event_id, 'image_caption', 0, 'caption text', now()
			  FROM event_images WHERE id = $1
			RETURNING id
		`, imageID).Scan(&chunkID); err == nil {
			_, err = tx.Exec(context.Background(), `
				UPDATE event_images SET captioned_at = now(), chunk_id = $2 WHERE id = $1
			`, imageID, chunkID)
		}
		if err == nil {
			err = tx.Commit(context.Background())
		}
		if err != nil {
			done <- err
			return
		}

		// Leave the derived chunk visibly pending for another poll. A barrier
		// that only watches captioned_at would return during this interval.
		time.Sleep(700 * time.Millisecond)
		_, err = pool.Exec(context.Background(), `
			UPDATE event_chunks SET embedded_at = now(), embedder_id = 'test'
			 WHERE id = $1
		`, chunkID)
		done <- err
	}()

	since, before := fullWindow()
	start := time.Now()
	res := waitForAttachments(ctx, pool, channel, rootTS, since, before)
	elapsed := time.Since(start)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if res.timedOut {
		t.Fatal("timedOut: got true, want false (derived embedding landed)")
	}
	if elapsed < time.Second {
		t.Fatalf("barrier returned after %s; it did not wait for embedded_at", elapsed)
	}
}

func TestWaitForAttachments_ReportsDerivedEmbeddingAtDeadline(t *testing.T) {
	pool := getThreadContextPool(t)

	const channel = "C_BARR_EMBED_TIMEOUT"
	const rootTS = "1700021750.000100"
	ingestTestMsg(t, pool, channel, "U_A", rootTS, "", "with screenshot", "Alice", false)
	seedImageRow(t, pool, channel, rootTS, true /*captioned*/, 0)
	seedCaption(t, pool, channel, rootTS, 0, "caption awaiting its vector")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	since, before := fullWindow()
	res := waitForAttachments(ctx, pool, channel, rootTS, since, before)

	if !res.timedOut {
		t.Fatal("timedOut: got false, want true")
	}
	if res.remaining.imageEmbeddings != 1 || res.remaining.documentEmbeddings != 0 {
		t.Fatalf("remaining = %+v, want one image embedding", res.remaining)
	}
	if res.remaining.embeddings() != 1 {
		t.Fatalf("remaining embeddings = %d, want 1", res.remaining.embeddings())
	}
}

// TestWaitForCaptions_DegradesAtDeadline: an image that never settles
// makes the barrier run to its deadline and return timedOut:true. A
// short caller-ctx deadline stands in for the 20s ceiling (the barrier
// takes the earlier of caller ctx and captionBarrierDeadline), which
// also proves the ceiling is enforced rather than open-ended.
func TestWaitForCaptions_DegradesAtDeadline(t *testing.T) {
	pool := getThreadContextPool(t)

	const channel = "C_BARR_TIMEOUT"
	const rootTS = "1700022000.000100"
	ingestTestMsg(t, pool, channel, "U_A", rootTS, "", "with screenshot", "Alice", false)
	seedImageRow(t, pool, channel, rootTS, false /*uncaptioned*/, 0)

	// 1s caller deadline caps the wait well under the 20s constant.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	since, before := fullWindow()
	start := time.Now()
	res := waitForAttachments(ctx, pool, channel, rootTS, since, before)
	elapsed := time.Since(start)

	if res.initialPending.images != 1 || res.initialPending.documents != 0 {
		t.Errorf("initialPending: got %+v, want 1 image", res.initialPending)
	}
	if !res.timedOut {
		t.Error("timedOut: got false, want true (caption never landed)")
	}
	// Ceiling: must return near the 1s caller deadline, not the 20s
	// constant.
	if elapsed > 3*time.Second {
		t.Errorf("deadline ceiling not enforced: took %s", elapsed)
	}
}

// TestWaitForCaptions_MaxFailuresSettled: an uncaptioned image whose
// failure_count has reached events.MaxFailures is treated as settled
// (poison dropout), so the first query sees zero pending.
func TestWaitForCaptions_MaxFailuresSettled(t *testing.T) {
	pool := getThreadContextPool(t)
	ctx := context.Background()

	const channel = "C_BARR_POISON"
	const rootTS = "1700023000.000100"
	ingestTestMsg(t, pool, channel, "U_A", rootTS, "", "poison image", "Alice", false)
	seedImageRow(t, pool, channel, rootTS, false /*uncaptioned*/, events.MaxFailures)

	since, before := fullWindow()
	res := waitForAttachments(ctx, pool, channel, rootTS, since, before)

	if res.initialPending.total() != 0 {
		t.Errorf("initialPending: got %+v, want 0 (poison image is settled)", res.initialPending)
	}
	if res.timedOut {
		t.Error("timedOut: got true, want false")
	}
}

// TestWaitForCaptions_DBErrorRequiresFinalVerification: a first-query DB
// error cannot prove that derived embeddings are settled. timedOut routes the
// caller through its parent-context final query; if that also fails, Ask does
// not start.
func TestWaitForCaptions_DBErrorRequiresFinalVerification(t *testing.T) {
	// Independent pool we can close to force a query error.
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()

	since, before := fullWindow()
	res := waitForAttachments(context.Background(), pool, "C_DEAD", "1700024000.000100", since, before)

	if res.initialPending.total() != 0 {
		t.Errorf("initialPending on DB error: got %+v, want 0", res.initialPending)
	}
	if !res.timedOut {
		t.Error("timedOut on DB error: got false, want true")
	}
}

// TestWaitForCaptions_OutOfScopeIgnored: an uncaptioned image on a
// message OUTSIDE the (since, before] window must not count — the
// barrier only waits on in-scope images.
func TestWaitForCaptions_OutOfScopeIgnored(t *testing.T) {
	pool := getThreadContextPool(t)
	ctx := context.Background()

	const channel = "C_BARR_SCOPE"
	const rootTS = "1700025000.000100"
	// In-scope, settled.
	ingestTestMsg(t, pool, channel, "U_A", rootTS, "", "in scope", "Alice", false)
	seedImageRow(t, pool, channel, rootTS, true, 0)
	// Later message (after `before`) with an uncaptioned image.
	const laterTS = "1700025500.000100"
	ingestTestMsg(t, pool, channel, "U_B", laterTS, rootTS, "out of scope", "Bob", false)
	seedImageRow(t, pool, channel, laterTS, false, 0)

	// before excludes the later message.
	since := time.Time{}
	before := time.Unix(1700025100, 0)
	res := waitForAttachments(ctx, pool, channel, rootTS, since, before)

	if res.initialPending.total() != 0 {
		t.Errorf("initialPending: got %+v, want 0 (later image is out of scope)", res.initialPending)
	}
	if res.timedOut {
		t.Error("timedOut: got true, want false")
	}
}

// TestBarrierMetrics_NoWait: when there are no in-scope uncaptioned
// images, maybeRouteAsk increments the no_wait counter and does NOT
// observe wait_seconds.
func TestBarrierMetrics_NoWait(t *testing.T) {
	pool := getConsumerPool(t)
	reg := prometheus.NewRegistry()
	m := newConsumerMetrics(reg)

	c := newTestConsumer(t, pool)
	c.metrics = m
	c.selfUserID.Store("U_DIDEROT")

	// Wire a fake router that short-circuits after the session lookup
	// so runAskTurn never tries to call a nil Slack API.
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router

	// Ingest the mention message (no images attached).
	const channel = "C_BARR_METRIC"
	const ts = "1700030000.000100"
	raw := `{
		"type": "message",
		"channel": "` + channel + `",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> hey",
		"ts": "` + ts + `"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	// Wait for the routing goroutine to reach the session lookup.
	waitForLen(t, func() int { router.mu.Lock(); defer router.mu.Unlock(); return len(router.createCalls) }, 1,
		"no_wait: LookupOrCreateSlackSession not reached")

	// no_wait counter must be exactly 1.
	got := testutil.ToFloat64(m.askCaptionBarrier.WithLabelValues("no_wait"))
	if got != 1 {
		t.Errorf("no_wait counter = %v, want 1", got)
	}
	// settled + timed_out must both be 0.
	if n := testutil.ToFloat64(m.askCaptionBarrier.WithLabelValues("settled")); n != 0 {
		t.Errorf("settled counter = %v, want 0", n)
	}
	if n := testutil.ToFloat64(m.askCaptionBarrier.WithLabelValues("timed_out")); n != 0 {
		t.Errorf("timed_out counter = %v, want 0", n)
	}
	// wait_seconds must have zero observations (no real wait happened).
	if n := testutil.CollectAndCount(m.askCaptionBarrierWait, "diderot_ask_caption_barrier_wait_seconds"); n != 0 {
		t.Errorf("wait_seconds has %d observations, want 0", n)
	}
}

// TestBarrierMetrics_Settled: an in-scope uncaptioned image (posted
// as an earlier thread message) that settles before the deadline
// bumps the settled counter and observes wait_seconds once.
//
// The image is on a thread message earlier than the @-mention so
// the ingest of the @-mention does not trigger ReconcileImages on
// that message's event_images row (each message's images are
// reconciled independently).
func TestBarrierMetrics_Settled(t *testing.T) {
	pool := getConsumerPool(t)
	reg := prometheus.NewRegistry()
	m := newConsumerMetrics(reg)

	c := newTestConsumer(t, pool)
	c.metrics = m
	c.selfUserID.Store("U_DIDEROT")

	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router

	const channel = "C_BARR_SETTLED_M"
	// earlier_ts: the message with the image (the thread root).
	// mention_ts: the @-mention (later, in the same thread).
	const earlierTS = "1700031000.000100"
	const mentionTS = "1700031001.000200"

	// 1. Seed the thread root with an uncaptioned image.
	ingestTestMsg(t, pool, channel, "U_A", earlierTS, "", "here's a screenshot", "Alice", false)
	imageID := seedImageRow(t, pool, channel, earlierTS, false /*uncaptioned*/, 0)

	// 2. Land the caption ~700ms after the barrier starts.
	go func() {
		time.Sleep(700 * time.Millisecond)
		_, _ = pool.Exec(context.Background(),
			`UPDATE event_images SET captioned_at = now() WHERE id = $1`, imageID)
	}()

	// 3. Ingest the @-mention as a reply in the same thread. No files,
	//    so ReconcileImages runs with zero refs for this message only
	//    — it does not touch the root message's event_images row.
	raw := `{
		"type": "message",
		"channel": "` + channel + `",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> check this screenshot",
		"ts": "` + mentionTS + `",
		"thread_ts": "` + earlierTS + `"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	// Wait for the session-create call that follows a completed barrier.
	waitForLen(t, func() int { router.mu.Lock(); defer router.mu.Unlock(); return len(router.createCalls) }, 1,
		"settled: LookupOrCreateSlackSession not reached")

	if n := testutil.ToFloat64(m.askCaptionBarrier.WithLabelValues("settled")); n != 1 {
		t.Errorf("settled counter = %v, want 1", n)
	}
	if n := testutil.ToFloat64(m.askCaptionBarrier.WithLabelValues("no_wait")); n != 0 {
		t.Errorf("no_wait counter = %v, want 0 (image was pending)", n)
	}
	// wait_seconds must have exactly one observation.
	if n := testutil.CollectAndCount(m.askCaptionBarrierWait, "diderot_ask_caption_barrier_wait_seconds"); n == 0 {
		t.Error("wait_seconds has no observations, want >=1")
	}
}

// TestFetchMentionImages returns direct images in Slack attachment order and
// keeps an image with no event_images row visible as unavailable.
func TestFetchMentionImages(t *testing.T) {
	pool := getThreadContextPool(t)
	ctx := context.Background()

	const channel = "C_MENTION_CAP"
	const mentionTS = "1700026000.000100"
	ingestTestMsg(t, pool, channel, "U_A", mentionTS, "", "@diderot what's this", "Alice", false)
	var eventID int64
	if err := pool.QueryRow(ctx, `
		UPDATE events
		   SET raw = jsonb_set(raw, '{files}',
		       '[{"id":"F_CAP","name":"captioned.png","mimetype":"image/png","permalink":"https://slack/files/F_CAP"},{"id":"F_MISSING","name":"missing.png","mimetype":"image/png"}]'::jsonb)
		 WHERE external_id = $1
		 RETURNING id`, channel+":"+mentionTS).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	var chunkID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
		VALUES ($1, 'image_caption', 0, 'a login error dialog', now())
		RETURNING id`, eventID).Scan(&chunkID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO event_images
		       (event_id, source_image_id, media_type, captioned_at, chunk_id, occurred_at)
		SELECT $1, 'F_CAP', 'image/png', now(), $2, occurred_at
		  FROM events WHERE id = $1`, eventID, chunkID); err != nil {
		t.Fatal(err)
	}

	got, err := fetchMentionImages(ctx, pool, channel+":"+mentionTS)
	if err != nil {
		t.Fatalf("fetchMentionImages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("images = %d, want 2: %+v", len(got), got)
	}
	if got[0].SourceImageID != "F_CAP" || got[0].Caption != "a login error dialog" || got[0].Filename != "captioned.png" {
		t.Errorf("captioned image = %+v", got[0])
	}
	if got[1].SourceImageID != "F_MISSING" || got[1].FailureReason != "image_not_indexed" {
		t.Errorf("missing image = %+v", got[1])
	}
	fallback, err := fallbackMentionImages(ctx, pool, channel+":"+mentionTS, []FileAttachment{{
		FileID: "F_FALLBACK", Name: "fallback.png", Mimetype: "image/png",
	}}, "image_metadata_unavailable")
	if err != nil {
		t.Fatalf("fallbackMentionImages: %v", err)
	}
	if len(fallback) != 1 || fallback[0].EventID != eventID {
		t.Fatalf("fallback manifest lost event identity: %+v", fallback)
	}

	_, delivered := appendMentionImages("what is this?", got)
	var sessionID string
	if err := pool.QueryRow(ctx, `INSERT INTO ask_sessions DEFAULT VALUES RETURNING id`).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if err := persistAskInputImages(ctx, pool, sessionID, mentionTS, mentionTS, delivered); err != nil {
		t.Fatalf("persistAskInputImages: %v", err)
	}
	var status, caption, reason string
	if err := pool.QueryRow(ctx, `
		SELECT delivery_status, COALESCE(caption_snapshot, ''), COALESCE(failure_reason, '')
		  FROM ask_input_images
		 WHERE session_id = $1 AND source_image_id = 'F_CAP'`, sessionID).Scan(&status, &caption, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "captioned" || caption != "a login error dialog" || reason != "" {
		t.Errorf("persisted captioned manifest = (%q, %q, %q)", status, caption, reason)
	}
	if err := pool.QueryRow(ctx, `
		SELECT delivery_status, COALESCE(failure_reason, '')
		  FROM ask_input_images
		 WHERE session_id = $1 AND source_image_id = 'F_MISSING'`, sessionID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "unavailable" || reason != "image_not_indexed" {
		t.Errorf("persisted unavailable manifest = (%q, %q)", status, reason)
	}
}
