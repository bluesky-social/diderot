package promptcatalog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func newPromptStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "promptcatalog"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `
		TRUNCATE legacy_ask_session_prompts, legacy_ask_prompts, ask_sessions,
		         prompt_release_activations, prompt_active_release,
		         prompt_release_entries, prompt_releases, prompt_draft_entries
		RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return NewStore(pool), pool
}

func TestLegacyAskPromptReadsOneArchivedPrompt(t *testing.T) {
	store, pool := newPromptStore(t)
	ctx := context.Background()
	createdAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	_, err := pool.Exec(ctx, `
		INSERT INTO legacy_ask_prompts (id, body, created_by, note, created_at, recovery_source)
		VALUES (4, 'first body', 'one@example.com', 'first', $1, 'test restore'),
		       (9, 'second body', NULL, NULL, $1, 'test restore')`, createdAt)
	require.NoError(t, err)
	var sessionID string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ask_sessions DEFAULT VALUES RETURNING id`).Scan(&sessionID))
	_, err = pool.Exec(ctx, `
		INSERT INTO legacy_ask_session_prompts (session_id, legacy_prompt_id, recovery_source)
		VALUES ($1, 4, 'test restore')`, sessionID)
	require.NoError(t, err)

	prompt, err := store.LegacyAskPrompt(ctx, 4)
	require.NoError(t, err)
	require.Equal(t, LegacyAskPrompt{ID: 4, Body: "first body", CreatedBy: "one@example.com", Note: "first", CreatedAt: prompt.CreatedAt}, *prompt)
	require.True(t, createdAt.Equal(prompt.CreatedAt))
	anonymous, err := store.LegacyAskPrompt(ctx, 9)
	require.NoError(t, err)
	require.Empty(t, anonymous.CreatedBy)
	require.Empty(t, anonymous.Note)
	_, err = store.LegacyAskPrompt(ctx, 5)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = pool.Exec(ctx, `UPDATE legacy_ask_prompts SET note = 'changed' WHERE id = 4`)
	require.ErrorContains(t, err, "immutable")
	_, err = pool.Exec(ctx, `DELETE FROM legacy_ask_session_prompts WHERE session_id = $1`, sessionID)
	require.ErrorContains(t, err, "immutable")
}

// TestLegacyAskPromptHistoryNamesItsRecoverySource checks that recovery_source
// has no default: a row added to the archive must say where it came from.
func TestLegacyAskPromptHistoryNamesItsRecoverySource(t *testing.T) {
	_, pool := newPromptStore(t)
	ctx := context.Background()
	var defaults int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = current_schema()
		   AND table_name IN ('legacy_ask_prompts', 'legacy_ask_session_prompts')
		   AND column_name = 'recovery_source'
		   AND column_default IS NOT NULL`).Scan(&defaults))
	require.Zero(t, defaults)
	_, err := pool.Exec(ctx, `
		INSERT INTO legacy_ask_prompts (id, body, created_at)
		VALUES (1, 'body', now())`)
	require.ErrorContains(t, err, `null value in column "recovery_source"`)
}

func changedBundle(t *testing.T, suffix string) Bundle {
	t.Helper()
	bundle := MustEmbedded()
	bundle.Prompts[0].Body += suffix
	bundle.Prompts[0].SHA256 = hash(bundle.Prompts[0].Body)
	bundle.SHA256 = bundleHash(bundle)
	require.NoError(t, ValidateWire(bundle))
	return bundle
}

func TestStoreCreateIsRaceSafeAndReproducible(t *testing.T) {
	store, _ := newPromptStore(t)
	bundle := MustEmbedded()
	ctx := context.Background()

	const workers = 8
	ids := make(chan int64, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			release, err := store.Create(ctx, bundle, "revision:same", fmt.Sprintf("test:%d", i), "")
			if err != nil {
				errs <- err
				return
			}
			ids <- release.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var first int64
	for id := range ids {
		if first == 0 {
			first = id
		}
		require.Equal(t, first, id)
	}
	require.NotZero(t, first)

	_, err := store.Create(ctx, changedBundle(t, "\nchanged"), "revision:same", "test", "")
	require.ErrorContains(t, err, "different bundle")
}

func TestEnsureSeedOnlySeedsAFreshDatabase(t *testing.T) {
	store, pool := newPromptStore(t)
	ctx := context.Background()
	first, err := store.EnsureSeed(ctx, MustEmbedded(), "embedded:one")
	require.NoError(t, err)
	require.True(t, first.Example)
	require.True(t, first.Active)

	// A later binary with a different example stores nothing.
	again, err := store.EnsureSeed(ctx, changedBundle(t, "\nnewer example"), "embedded:two")
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
	var releases int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM prompt_releases`).Scan(&releases))
	require.Equal(t, 1, releases)
}

func TestStoreActivationCompareAndSwap(t *testing.T) {
	store, pool := newPromptStore(t)
	ctx := context.Background()
	first, err := store.EnsureSeed(ctx, MustEmbedded(), "revision:one")
	require.NoError(t, err)
	second, err := store.Create(ctx, changedBundle(t, "\nsecond"), "revision:two", "test", "")
	require.NoError(t, err)

	activation, err := store.Activate(ctx, second.ID, first.ID, "test")
	require.NoError(t, err)
	require.True(t, activation.Changed)
	require.Equal(t, second.ID, activation.Release.ID)
	_, err = store.Activate(ctx, first.ID, first.ID, "stale")
	require.ErrorContains(t, err, "active release changed")
	_, err = store.Activate(ctx, 999999, second.ID, "test")
	require.ErrorContains(t, err, "not found")
	var activations int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM prompt_release_activations`).Scan(&activations))
	require.Equal(t, 2, activations)
	_, err = pool.Exec(ctx, `UPDATE prompt_releases SET note = 'mutated' WHERE id = $1`, first.ID)
	require.ErrorContains(t, err, "immutable")
}

func TestStoreActivationLeavesIdenticalBundleOnExistingIdentity(t *testing.T) {
	store, pool := newPromptStore(t)
	ctx := context.Background()
	first, err := store.EnsureSeed(ctx, MustEmbedded(), "revision:one")
	require.NoError(t, err)
	metadataOnly := MustEmbedded()
	metadataOnly.Prompts[0].Description += " (clarified)"
	metadataOnly.SHA256 = bundleHash(metadataOnly)
	require.NotEqual(t, first.Bundle.SHA256, metadataOnly.SHA256)
	require.Equal(t, first.Bundle.EffectiveSHA256(), metadataOnly.EffectiveSHA256())
	duplicate, err := store.Create(ctx, metadataOnly, "revision:metadata-only", "test", "same effective prompts")
	require.NoError(t, err)
	require.NotEqual(t, first.ID, duplicate.ID, "retain the source revision as release provenance")

	activation, err := store.Activate(ctx, duplicate.ID, first.ID, "test")
	require.NoError(t, err)
	require.False(t, activation.Changed)
	require.Equal(t, first.ID, activation.Release.ID)
	require.True(t, activation.Release.Active)

	current, err := store.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, first.ID, current.ID)
	var activations int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM prompt_release_activations`).Scan(&activations))
	require.Equal(t, 1, activations, "an unchanged bundle is not a new active interval")

	_, err = store.Activate(ctx, duplicate.ID, duplicate.ID, "stale")
	require.ErrorContains(t, err, "active release changed")
}

func TestWatcherRefreshesProcessBundle(t *testing.T) {
	store, _ := newPromptStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Cleanup(func() { require.NoError(t, SetActive(MustEmbedded())) })
	first, err := store.EnsureSeed(ctx, MustEmbedded(), "revision:watch-one")
	require.NoError(t, err)
	secondBundle := changedBundle(t, "\nwatcher-visible")
	second, err := store.Create(ctx, secondBundle, "revision:watch-two", "test", "")
	require.NoError(t, err)

	// The caller (preparePrompts) applies the first release itself, before
	// starting the watcher, and passes its id so the watcher's first tick
	// does not redo that read.
	require.NoError(t, SetActive(first.Bundle))
	go RunWatcher(ctx, store, first.ID, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err = store.Activate(ctx, second.ID, first.ID, "test")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return Active().SHA256 == secondBundle.Render(Values()).SHA256 }, time.Second, 5*time.Millisecond)
}

func TestPublishingOnePromptKeepsItsHistory(t *testing.T) {
	store, _ := newPromptStore(t)
	ctx := context.Background()
	seed, err := store.EnsureSeed(ctx, MustEmbedded(), "embedded:test")
	require.NoError(t, err)
	require.True(t, seed.Example)
	original, err := seed.Bundle.Text(AskAnswerDiscipline)
	require.NoError(t, err)

	edited, err := publishOne(ctx, store, seed.ID, AskAnswerDiscipline, "Be brief.\r\n", "ada@example.com", "shorter")
	require.NoError(t, err)
	require.True(t, edited.Changed)
	saved := edited.Release
	require.True(t, saved.Active)
	require.False(t, saved.Example)
	require.Equal(t, "ui:"+saved.Bundle.SHA256, saved.SourceRevision)
	require.Equal(t, "ada@example.com", saved.CreatedBy)
	require.Equal(t, "shorter", saved.Note)
	for i, prompt := range saved.Bundle.Prompts {
		if prompt.Key == AskAnswerDiscipline {
			require.Equal(t, "Be brief.", prompt.Body)
			continue
		}
		require.Equal(t, seed.Bundle.Prompts[i], prompt, "only the edited entry changes")
	}

	_, err = publishOne(ctx, store, seed.ID, AskAnswerDiscipline, "Stale edit.", "bob@example.com", "")
	require.ErrorIs(t, err, ErrDraftChanged, "an editor opened on the seed must not overwrite")
	current, err := store.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, saved.ID, current.ID)

	unchanged, err := publishOne(ctx, store, saved.ID, AskAnswerDiscipline, "Be brief.", "ada@example.com", "")
	require.NoError(t, err)
	require.False(t, unchanged.Changed)
	require.Equal(t, saved.ID, unchanged.Release.ID)

	reverted, err := publishOne(ctx, store, saved.ID, AskAnswerDiscipline, original, "bob@example.com", "revert to release 1")
	require.NoError(t, err)
	require.True(t, reverted.Changed)
	require.Equal(t, seed.Bundle.SHA256, reverted.Release.Bundle.SHA256)
	require.NotEqual(t, seed.ID, reverted.Release.ID, "a revert is a new release")

	again, err := publishOne(ctx, store, reverted.Release.ID, AskAnswerDiscipline, "Be brief.", "ada@example.com", "")
	require.NoError(t, err)
	require.Equal(t, saved.ID, again.Release.ID, "identical content reuses its immutable release")

	_, err = publishOne(ctx, store, saved.ID, "ask.missing", "x", "ada@example.com", "")
	require.ErrorContains(t, err, "ask.missing is not a prompt this version of Diderot uses")
	_, err = publishOne(ctx, store, saved.ID, AskAnswerDiscipline, "Hi {{NAME}}", "ada@example.com", "")
	require.ErrorContains(t, err, "unknown variable {{NAME}}")
	_, err = publishOne(ctx, store, saved.ID, AskAnswerDiscipline, "  ", "ada@example.com", "")
	require.ErrorContains(t, err, "is required and cannot be removed")

	history, err := store.PromptHistory(ctx, AskAnswerDiscipline, 0, 20)
	require.NoError(t, err)
	require.Len(t, history, 4)
	require.Equal(t, []int64{saved.ID, reverted.Release.ID, saved.ID, seed.ID},
		[]int64{history[0].ReleaseID, history[1].ReleaseID, history[2].ReleaseID, history[3].ReleaseID})
	require.Equal(t, BodySHA256("Be brief."), history[0].SHA256)
	require.Equal(t, "ada@example.com", history[0].ActivatedBy)
	require.Equal(t, "revert to release 1", history[1].Note)
	require.Equal(t, BodySHA256(original), history[3].SHA256)
	require.True(t, history[3].Example)
	older, err := store.PromptHistory(ctx, AskAnswerDiscipline, history[1].ActivationID, 20)
	require.NoError(t, err)
	require.Equal(t, history[2:], older, "before pages past the activations already shown")
	untouched, err := store.PromptHistory(ctx, AskBase, 0, 20)
	require.NoError(t, err)
	require.Len(t, untouched, 1, "edits to another prompt are not this prompt's history")
	require.Equal(t, seed.ID, untouched[0].ReleaseID)

	// saved went live twice; its change is the one it made first.
	change, err := store.PromptChange(ctx, AskAnswerDiscipline, saved.ID)
	require.NoError(t, err)
	require.Equal(t, history[2], change.PromptVersion)
	require.Equal(t, "Be brief.", change.Body)
	require.Equal(t, original, change.PreviousBody)
	require.False(t, change.First)
	first, err := store.PromptChange(ctx, AskAnswerDiscipline, seed.ID)
	require.NoError(t, err)
	require.True(t, first.First)
	require.Empty(t, first.PreviousBody)
	require.Equal(t, original, first.Body)
	_, err = store.PromptChange(ctx, AskBase, saved.ID)
	require.ErrorIs(t, err, ErrNotFound, "saved did not change ask.base")

	latest, err := store.LatestVersions(ctx)
	require.NoError(t, err)
	require.Len(t, latest, len(seed.Bundle.Prompts))
	require.Equal(t, history[0], latest[AskAnswerDiscipline])
	require.Equal(t, untouched[0], latest[AskBase])
	require.True(t, latest[AskBase].Example)

	activations, err := store.Activations(ctx, 0, 20)
	require.NoError(t, err)
	require.Len(t, activations, 4)
	require.Equal(t, saved.ID, activations[0].ReleaseID)
	require.Equal(t, reverted.Release.ID, activations[0].FromReleaseID)
	require.True(t, activations[0].Again, "saved was active before the revert")
	require.Equal(t, []string{AskAnswerDiscipline}, activations[0].Changed)
	require.Equal(t, "ada@example.com", activations[0].ActivatedBy)
	require.False(t, activations[2].Again)
	seedActivation := activations[3]
	require.Zero(t, seedActivation.FromReleaseID)
	require.Equal(t, SeedActor, seedActivation.CreatedBy)
	require.Len(t, seedActivation.Changed, len(seed.Bundle.Prompts), "the first activation changes every prompt")
	page, err := store.Activations(ctx, activations[1].ID, 20)
	require.NoError(t, err)
	require.Equal(t, activations[2:], page)
	savedActivations, err := store.ReleaseActivations(ctx, saved.ID)
	require.NoError(t, err)
	require.Len(t, savedActivations, 2)
	require.Equal(t, []int64{activations[2].ID, activations[0].ID},
		[]int64{savedActivations[0].ID, savedActivations[1].ID}, "oldest first")

	by, at, err := store.ActiveSince(ctx)
	require.NoError(t, err)
	require.Equal(t, "ada@example.com", by)
	require.True(t, activations[0].ActivatedAt.Equal(at))
}

func TestStoreCreateRejectsUnknownPlaceholder(t *testing.T) {
	store, _ := newPromptStore(t)
	bundle := MustEmbedded()
	bundle.Prompts[0].Body += "\nWrite for {{COMPANY_NAME}}."
	bundle.Prompts[0].SHA256 = hash(bundle.Prompts[0].Body)
	bundle.SHA256 = bundleHash(bundle)
	require.NoError(t, ValidateWire(bundle))
	_, err := store.Create(context.Background(), bundle, "revision:unknown-variable", "test", "")
	require.ErrorContains(t, err, "unknown variable {{COMPANY_NAME}}")
}

func TestActivationsNameAddedAndRemovedPrompts(t *testing.T) {
	store, _ := newPromptStore(t)
	ctx := context.Background()
	seed, err := store.EnsureSeed(ctx, MustEmbedded(), "embedded:test")
	require.NoError(t, err)
	extra := MustEmbedded()
	extra.Prompts = append(extra.Prompts, Prompt{Key: "zz.extra", Path: "zz/extra.md", Body: "Extra.", SHA256: hash("Extra.")})
	extra.SHA256 = bundleHash(extra)
	require.NoError(t, ValidateWire(extra))
	added, err := store.Create(ctx, extra, "revision:extra", "test", "")
	require.NoError(t, err)
	_, err = store.Activate(ctx, added.ID, seed.ID, "test")
	require.NoError(t, err)
	removed, err := store.Activate(ctx, seed.ID, added.ID, "test")
	require.NoError(t, err)
	require.True(t, removed.Changed, "dropping a prompt is a change")

	activations, err := store.Activations(ctx, 0, 20)
	require.NoError(t, err)
	require.Len(t, activations, 3)
	require.Equal(t, []string{"zz.extra"}, activations[0].Changed, "the removed prompt")
	require.Equal(t, []string{"zz.extra"}, activations[1].Changed, "the added prompt")
}
