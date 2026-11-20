package files

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCommitCaptionRejectsCommitAfterClaimIsSuperseded pins that
// commitCaption refuses a commit from a superseded claim.
//
// Trigger: worker A claims an event_images row, then its single captionOne
// attempt (fetch + caption + commit) keeps running past staleClaimAge (5
// minutes) — e.g. a hung VLM call, or a Slack/S3 stage with no timeout of
// its own. A second worker B sees the claim as abandoned, re-claims the
// same row, and completes a full, successful caption of its own. When A's
// stale attempt finally finishes, it calls commitCaption using the claim
// it took at the very start — a claim B's fresh commit has already
// superseded.
//
// Contract: each image is captioned once across concurrent workers;
// worker.go's package doc says "concurrent captions on the same event
// serialize cleanly". Without a check that the committing worker still
// holds the claim it started with, A's late commit lands unconditionally:
// it replaces B's chunk with A's (older, abandoned) content and deletes
// B's chunk outright, exactly the data loss that contract rules out.
//
// This test reproduces the race deterministically with plain inputs and no
// goroutines: claim the row once ("A"), force the claim stale by
// backdating claimed_at (standing in for staleClaimAge elapsing while A's
// attempt is still in flight), let B claim and caption the row for real,
// and only then let A's already-held claim run to completion. A's commit
// must be rejected as a no-op instead of overwriting B's result.
func TestCommitCaptionRejectsCommitAfterClaimIsSuperseded(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")

	// A claims the row. Outside this test, this is where A's fetch+caption+
	// commit pipeline begins; here we hold the claim and run it later,
	// after B has already finished, to stand in for "A's single attempt
	// is still in flight".
	workerA := newCaptionOneWorker(t, pool,
		&fakeCaptioner{result: &CaptionResult{Caption: "stale worker A caption"}},
		&fakeFetcher{data: encodePNG(t, 64, 64), ct: "image/png"})
	rowA, err := workerA.claimUncaptioned(ctx)
	require.NoError(t, err)
	require.NotNil(t, rowA)
	require.Equal(t, imageID, rowA.imageID)

	// staleClaimAge elapses while A's attempt is still running. Stand in
	// for the passage of time by backdating claimed_at directly (same
	// technique as TestClaimUncaptionedRecoversStaleClaim).
	_, err = pool.Exec(ctx, `UPDATE event_images SET claimed_at = $2 WHERE id = $1`,
		imageID, time.Now().Add(-(staleClaimAge + time.Minute)))
	require.NoError(t, err)

	// B sees the row as abandoned, re-claims it, and completes a full,
	// successful caption — exactly the intended stale-claim recovery.
	workerB := newCaptionOneWorker(t, pool,
		&fakeCaptioner{result: &CaptionResult{Caption: "fresh worker B caption"}},
		&fakeFetcher{data: encodePNG(t, 64, 64), ct: "image/png"})
	rowB, err := workerB.claimUncaptioned(ctx)
	require.NoError(t, err)
	require.NotNil(t, rowB)
	require.Equal(t, imageID, rowB.imageID, "B must be able to reclaim the stale row")
	require.NoError(t, workerB.captionOne(ctx, rowB))

	_, captioned, chunkIDAfterB := getImageRow(t, pool, imageID)
	require.True(t, captioned, "B's caption should have committed")
	require.NotNil(t, chunkIDAfterB)
	var contentAfterB string
	require.NoError(t, pool.QueryRow(ctx, `SELECT content FROM event_chunks WHERE id = $1`, *chunkIDAfterB).Scan(&contentAfterB))
	require.Contains(t, contentAfterB, "fresh worker B caption")

	// A's long-hung captionOne call finally returns and tries to commit,
	// using the claim it has held since the first claim above — a claim
	// that B's commit has already superseded. This must come back as
	// errClaimSuperseded (a no-op the caller recognizes and does not
	// bump failure_count over — see runLoop), not nil (silently
	// succeeding) and not some other error.
	err = workerA.captionOne(ctx, rowA)
	require.ErrorIs(t, err, errClaimSuperseded,
		"a superseded claim's commit must be rejected as superseded, not silently accepted")

	// Expected (and, before the fix, violated): B's caption stays
	// authoritative, and B's chunk is untouched.
	_, captionedAfter, chunkIDAfter := getImageRow(t, pool, imageID)
	require.True(t, captionedAfter)
	require.Equal(t, *chunkIDAfterB, *chunkIDAfter,
		"the committed chunk must still be B's; A's superseded commit must not repoint event_images.chunk_id")

	var contentAfter string
	require.NoError(t, pool.QueryRow(ctx, `SELECT content FROM event_chunks WHERE id = $1`, *chunkIDAfter).Scan(&contentAfter))
	require.Contains(t, contentAfter, "fresh worker B caption",
		"B's caption must remain authoritative over A's superseded commit")
	require.NotContains(t, contentAfter, "stale worker A caption")

	// B's chunk must not have been deleted out from under a caption that,
	// per the contract, was never legitimately replaced.
	var stillExists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM event_chunks WHERE id = $1)`, *chunkIDAfterB).Scan(&stillExists))
	require.True(t, stillExists, "B's chunk must not be deleted by A's superseded commit")
}

// TestCaptionOneUnsupportedFormatSkipRejectsSupersededClaim is the
// regression test for the same missing-ownership-check mechanism as
// TestCommitCaptionRejectsCommitAfterClaimIsSuperseded, in the
// unsupported-format skip path (worker.go's captionOne, the
// `caption_model = 'skipped:unsupported-format'` stamp): it also
// finalizes captioned_at unconditionally, with no check that the
// worker still holds the claim it started with.
//
// Trigger: worker A claims a row and its single captionOne attempt is
// still in flight when staleClaimAge elapses. Worker B re-claims the row
// and commits a real caption. A's attempt then resolves to an
// unsupported content type (a distinct but equally possible outcome of
// the same stale-claim race) and tries to stamp
// 'skipped:unsupported-format' over B's already-landed caption.
//
// Expected: A's stamp is rejected (errClaimSuperseded) and B's caption
// and chunk are left exactly as they were.
func TestCaptionOneUnsupportedFormatSkipRejectsSupersededClaim(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImage(t, pool, eventID, "F1")

	workerA := newCaptionOneWorker(t, pool,
		&fakeCaptioner{}, // never reached: the format check short-circuits first
		&fakeFetcher{data: []byte("<svg></svg>"), ct: "image/svg+xml"})
	rowA, err := workerA.claimUncaptioned(ctx)
	require.NoError(t, err)
	require.NotNil(t, rowA)

	_, err = pool.Exec(ctx, `UPDATE event_images SET claimed_at = $2 WHERE id = $1`,
		imageID, time.Now().Add(-(staleClaimAge + time.Minute)))
	require.NoError(t, err)

	workerB := newCaptionOneWorker(t, pool,
		&fakeCaptioner{result: &CaptionResult{Caption: "fresh worker B caption"}},
		&fakeFetcher{data: encodePNG(t, 64, 64), ct: "image/png"})
	rowB, err := workerB.claimUncaptioned(ctx)
	require.NoError(t, err)
	require.NotNil(t, rowB)
	require.NoError(t, workerB.captionOne(ctx, rowB))

	_, captionedByB, chunkIDByB := getImageRow(t, pool, imageID)
	require.True(t, captionedByB)
	require.NotNil(t, chunkIDByB)

	// A's attempt finally resolves (to an unsupported format) and tries
	// to stamp the skip outcome over B's already-committed caption.
	err = workerA.captionOne(ctx, rowA)
	require.ErrorIs(t, err, errClaimSuperseded,
		"a superseded claim's skip-stamp must be rejected as superseded, not silently accepted")

	model, captionedAfter, chunkIDAfter := getImageRow(t, pool, imageID)
	require.True(t, captionedAfter)
	require.Equal(t, chunkIDByB, chunkIDAfter, "B's chunk_id must be untouched")
	require.NotNil(t, model)
	require.NotEqual(t, "skipped:unsupported-format", *model,
		"A's superseded skip-stamp must not overwrite B's real caption_model")
}
