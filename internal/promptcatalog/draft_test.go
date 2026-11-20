package promptcatalog

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// publishOne makes body key's live text through the draft, as an
// administrator does from a prompt's page opened on release expected.
func publishOne(ctx context.Context, store *Store, expected int64, key, body, actor, note string) (*ActivationResult, error) {
	opened, err := store.Get(ctx, expected)
	if err != nil {
		return nil, err
	}
	if err := store.WriteDraft(ctx, []DraftChange{{Key: key, Body: body, LiveSHA256: liveSHA256(opened.Bundle, key)}}, actor); err != nil {
		return nil, err
	}
	return publishDraft(ctx, store, expected, actor, note)
}

// publishDraft makes the draft live as the draft page shows it.
func publishDraft(ctx context.Context, store *Store, expected int64, actor, note string) (*ActivationResult, error) {
	draft, err := store.Draft(ctx)
	if err != nil {
		return nil, err
	}
	bundle, err := draft.Bundle()
	if err != nil {
		return nil, err
	}
	return store.PublishDraft(ctx, expected, bundle.SHA256, actor, note)
}

func seededStore(t *testing.T) (*Store, *Release) {
	t.Helper()
	store, _ := newPromptStore(t)
	seed, err := store.EnsureSeed(context.Background(), MustEmbedded(), "embedded:test")
	require.NoError(t, err)
	return store, seed
}

func TestDraftGoesLiveAsOneRelease(t *testing.T) {
	store, seed := seededStore(t)
	ctx := context.Background()
	require.NoError(t, store.WriteDraft(ctx, []DraftChange{
		{Key: AskAnswerDiscipline, Body: "Be brief.", LiveSHA256: liveSHA256(seed.Bundle, AskAnswerDiscipline)},
		{Key: AskSurfaceHTTP, Body: "Plain Markdown.", LiveSHA256: liveSHA256(seed.Bundle, AskSurfaceHTTP)},
		{Key: AskOrganization, Body: "Runbooks live in the ops repository."},
	}, "ada@example.com"))

	current, err := store.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, seed.ID, current.ID, "a draft changes nothing that runs")

	draft, err := store.Draft(ctx)
	require.NoError(t, err)
	require.Len(t, draft.Entries, 3)
	for _, entry := range draft.Entries {
		require.False(t, draft.OutOfDate(entry), entry.Key)
		require.Equal(t, "ada@example.com", entry.UpdatedBy)
	}

	result, err := publishDraft(ctx, store, seed.ID, "bob@example.com", "two rules and a note")
	require.NoError(t, err)
	require.True(t, result.Changed)
	release := result.Release
	require.True(t, release.Active)
	require.Equal(t, "ui:"+release.Bundle.SHA256, release.SourceRevision)
	require.Equal(t, "two rules and a note", release.Note)
	require.Equal(t, "Be brief.", release.Bundle.MustText(AskAnswerDiscipline))
	require.Equal(t, "Plain Markdown.", release.Bundle.MustText(AskSurfaceHTTP))
	require.Equal(t, "Runbooks live in the ops repository.", release.Bundle.NoteText(AskOrganization))
	require.Equal(t, seed.Bundle.MustText(AskBase), release.Bundle.MustText(AskBase))

	activations, err := store.Activations(ctx, 0, 20)
	require.NoError(t, err)
	require.Len(t, activations, 2, "one activation for the whole draft")
	require.Equal(t, []string{AskAnswerDiscipline, AskOrganization, AskSurfaceHTTP}, activations[0].Changed)
	require.Equal(t, "bob@example.com", activations[0].ActivatedBy)

	emptied, err := store.Draft(ctx)
	require.NoError(t, err)
	require.Empty(t, emptied.Entries, "going live clears the draft")

	// Saving a note empty removes it.
	require.NoError(t, store.WriteDraft(ctx, []DraftChange{{
		Key: AskOrganization, LiveSHA256: liveSHA256(release.Bundle, AskOrganization),
	}}, "ada@example.com"))
	removed, err := publishDraft(ctx, store, release.ID, "ada@example.com", "")
	require.NoError(t, err)
	require.True(t, removed.Changed)
	require.Empty(t, removed.Release.Bundle.NoteText(AskOrganization))
}

func TestDraftWritesApplyOnlyToWhatTheWriterSaw(t *testing.T) {
	store, seed := seededStore(t)
	ctx := context.Background()
	live := liveSHA256(seed.Bundle, AskAnswerDiscipline)
	require.NoError(t, store.WriteDraft(ctx, []DraftChange{{Key: AskAnswerDiscipline, Body: "First.", LiveSHA256: live}}, "ada@example.com"))

	// A second page opened before the first save saw no draft text.
	err := store.WriteDraft(ctx, []DraftChange{{Key: AskAnswerDiscipline, Body: "Second.", LiveSHA256: live}}, "bob@example.com")
	require.ErrorIs(t, err, ErrDraftChanged)
	require.NoError(t, store.WriteDraft(ctx, []DraftChange{{
		Key: AskAnswerDiscipline, Body: "Second.", DraftSHA256: BodySHA256("First."), LiveSHA256: live,
	}}, "bob@example.com"))

	// All or none: one stale change refuses the others.
	err = store.WriteDraft(ctx, []DraftChange{
		{Key: AskSurfaceHTTP, Body: "Plain.", LiveSHA256: liveSHA256(seed.Bundle, AskSurfaceHTTP)},
		{Key: AskAnswerDiscipline, Body: "Third.", LiveSHA256: live},
	}, "ada@example.com")
	require.ErrorIs(t, err, ErrDraftChanged)
	draft, err := store.Draft(ctx)
	require.NoError(t, err)
	require.Len(t, draft.Entries, 1)
	require.Equal(t, "Second.", draft.Entries[0].Body)

	// Text equal to live drops the entry.
	require.NoError(t, store.WriteDraft(ctx, []DraftChange{{
		Key: AskAnswerDiscipline, Body: seed.Bundle.MustText(AskAnswerDiscipline) + "\n",
		DraftSHA256: BodySHA256("Second."), LiveSHA256: live,
	}}, "ada@example.com"))
	draft, err = store.Draft(ctx)
	require.NoError(t, err)
	require.Empty(t, draft.Entries)

	for _, tc := range []struct {
		change DraftChange
		err    string
	}{
		{DraftChange{Key: "ask.missing", Body: "x"}, "ask.missing is not a prompt this version of Diderot uses"},
		{DraftChange{Key: AskAnswerDiscipline, Body: "Hi {{NAME}}", LiveSHA256: live}, "unknown variable {{NAME}}"},
		{DraftChange{Key: AskAnswerDiscipline, Body: "  ", LiveSHA256: live}, "is required and cannot be removed"},
	} {
		require.ErrorContains(t, store.WriteDraft(ctx, []DraftChange{tc.change}, "ada@example.com"), tc.err)
	}
}

func TestPublishRefusesADraftOrLiveSetItWasNotShown(t *testing.T) {
	store, seed := seededStore(t)
	ctx := context.Background()
	require.NoError(t, store.WriteDraft(ctx, []DraftChange{{
		Key: AskAnswerDiscipline, Body: "Be brief.", LiveSHA256: liveSHA256(seed.Bundle, AskAnswerDiscipline),
	}}, "ada@example.com"))
	draft, err := store.Draft(ctx)
	require.NoError(t, err)
	reviewed, err := draft.Bundle()
	require.NoError(t, err)

	// Someone adds a change after the review.
	require.NoError(t, store.WriteDraft(ctx, []DraftChange{{
		Key: AskSurfaceHTTP, Body: "Plain.", LiveSHA256: liveSHA256(seed.Bundle, AskSurfaceHTTP),
	}}, "bob@example.com"))
	_, err = store.PublishDraft(ctx, seed.ID, reviewed.SHA256, "ada@example.com", "")
	require.ErrorIs(t, err, ErrDraftChanged)
	require.ErrorIs(t, store.DiscardDraft(ctx, reviewed.SHA256), ErrDraftChanged, "discard needs the draft it was shown")

	// Live moves under the draft: an older version made live.
	other := MustEmbedded()
	changed, err := other.WithBody(AskAnswerDiscipline, "Moved.")
	require.NoError(t, err)
	moved, err := store.Create(ctx, changed, "revision:moved", "test", "")
	require.NoError(t, err)
	_, err = store.Activate(ctx, moved.ID, seed.ID, "test")
	require.NoError(t, err)
	_, err = publishDraft(ctx, store, seed.ID, "ada@example.com", "")
	require.ErrorIs(t, err, ErrActiveReleaseChanged)
	_, err = publishDraft(ctx, store, moved.ID, "ada@example.com", "")
	require.ErrorIs(t, err, ErrDraftOutOfDate)
	draft, err = store.Draft(ctx)
	require.NoError(t, err)
	entry, ok := draft.Entry(AskAnswerDiscipline)
	require.True(t, ok)
	require.True(t, draft.OutOfDate(entry))

	// Saving it again from a page that showed the new live text rebases it.
	require.NoError(t, store.WriteDraft(ctx, []DraftChange{{
		Key: AskAnswerDiscipline, Body: "Be brief.", DraftSHA256: entry.SHA256(),
		LiveSHA256: liveSHA256(moved.Bundle, AskAnswerDiscipline),
	}}, "ada@example.com"))
	result, err := publishDraft(ctx, store, moved.ID, "ada@example.com", "")
	require.NoError(t, err)
	require.True(t, result.Changed)

	// A discard shown the current draft empties it.
	require.NoError(t, store.WriteDraft(ctx, []DraftChange{{
		Key: AskSurfaceHTTP, Body: "Plainer.", LiveSHA256: liveSHA256(result.Release.Bundle, AskSurfaceHTTP),
	}}, "ada@example.com"))
	draft, err = store.Draft(ctx)
	require.NoError(t, err)
	shown, err := draft.Bundle()
	require.NoError(t, err)
	require.NoError(t, store.DiscardDraft(ctx, shown.SHA256))
	draft, err = store.Draft(ctx)
	require.NoError(t, err)
	require.Empty(t, draft.Entries)
}

func TestDraftRemovesAPromptThisVersionDoesNotUse(t *testing.T) {
	store, _ := newPromptStore(t)
	ctx := context.Background()
	old := MustEmbedded()
	old.Prompts = append(old.Prompts, Prompt{Key: "zz.retired", Path: "zz/retired.md", Body: "Retired.", SHA256: hash("Retired.")})
	old.SHA256 = bundleHash(old)
	seed, err := store.EnsureSeed(ctx, old, "embedded:old")
	require.NoError(t, err)

	require.NoError(t, store.WriteDraft(ctx, []DraftChange{{Key: "zz.retired", LiveSHA256: hash("Retired.")}}, "ada@example.com"))
	draft, err := store.Draft(ctx)
	require.NoError(t, err)
	require.Len(t, draft.Entries, 1)
	require.True(t, draft.Entries[0].Remove)
	require.Equal(t, DraftRemoved, draft.Entries[0].SHA256())

	result, err := publishDraft(ctx, store, seed.ID, "ada@example.com", "")
	require.NoError(t, err)
	require.True(t, result.Changed)
	require.Equal(t, MustEmbedded().EffectiveSHA256(), result.Release.Bundle.EffectiveSHA256())
}

// TestDraftOfARetiredPromptStillDiscards: an edit of a prompt this version
// does not ship, made while live had it, survives an older version without
// it going live as an out-of-date change that can be discarded.
func TestDraftOfARetiredPromptStillDiscards(t *testing.T) {
	store, _ := newPromptStore(t)
	ctx := context.Background()
	old := MustEmbedded()
	old.Prompts = append(old.Prompts, Prompt{Key: "zz.retired", Path: "zz/retired.md", Body: "Retired.", SHA256: hash("Retired.")})
	old.SHA256 = bundleHash(old)
	seed, err := store.EnsureSeed(ctx, old, "embedded:old")
	require.NoError(t, err)
	require.NoError(t, store.WriteDraft(ctx, []DraftChange{{Key: "zz.retired", Body: "Edited.", LiveSHA256: hash("Retired.")}}, "ada@example.com"))

	shipped, err := store.Create(ctx, MustEmbedded(), "revision:shipped", "test", "")
	require.NoError(t, err)
	_, err = store.Activate(ctx, shipped.ID, seed.ID, "test")
	require.NoError(t, err)

	draft, err := store.Draft(ctx)
	require.NoError(t, err)
	bundle, err := draft.Bundle()
	require.NoError(t, err, "the draft still shows")
	require.True(t, draft.OutOfDate(draft.Entries[0]))
	_, err = store.PublishDraft(ctx, shipped.ID, bundle.SHA256, "ada@example.com", "")
	require.ErrorIs(t, err, ErrDraftOutOfDate)
	require.NoError(t, store.DiscardDraft(ctx, bundle.SHA256))
}
