package adminaction

import (
	"context"
	"strconv"
	"strings"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

// The prompt catalog (*promptcatalog.Store), one method at a time.
type (
	PromptActivator interface {
		Activate(ctx context.Context, id, expectedCurrentID int64, activatedBy string) (*promptcatalog.ActivationResult, error)
	}
	PromptDraftWriter interface {
		WriteDraft(ctx context.Context, changes []promptcatalog.DraftChange, actor string) error
	}
	PromptDraftDiscarder interface {
		DiscardDraft(ctx context.Context, fingerprint string) error
	}
	PromptDraftPublisher interface {
		PublishDraft(ctx context.Context, expectedCurrentID int64, fingerprint, actor, note string) (*promptcatalog.ActivationResult, error)
	}
)

// draftTarget is the audit target of an action on the whole draft.
const draftTarget = "draft"

// ActivatePromptRelease makes release id the active one in place of
// expectedCurrentID, failing with promptcatalog.ErrActiveReleaseChanged when
// that is no longer active.
func ActivatePromptRelease(ctx context.Context, a Audit, releases PromptActivator, id, expectedCurrentID int64) (*promptcatalog.ActivationResult, error) {
	var result *promptcatalog.ActivationResult
	err := a.Run(ctx, "prompts.activate", strconv.FormatInt(id, 10),
		map[string]any{"expected_release_id": expectedCurrentID}, func() error {
			var err error
			result, err = releases.Activate(ctx, id, expectedCurrentID, a.actor())
			return err
		})
	return result, err
}

// SaveDraftPrompt puts change, one prompt's new text or its removal, in the
// prompt draft.
func SaveDraftPrompt(ctx context.Context, a Audit, drafts PromptDraftWriter, change promptcatalog.DraftChange) error {
	return writeDraft(ctx, a, drafts, "prompts.draft.save", change.Key, []promptcatalog.DraftChange{change}, nil)
}

// DiscardDraftPrompt drops one prompt's change from the draft. change is the
// prompt's live text, or its removal when live has none.
func DiscardDraftPrompt(ctx context.Context, a Audit, drafts PromptDraftWriter, change promptcatalog.DraftChange) error {
	return writeDraft(ctx, a, drafts, "prompts.draft.discard", change.Key, []promptcatalog.DraftChange{change}, nil)
}

// RestorePromptToDraft puts one prompt's text from release restoreID in the
// draft.
func RestorePromptToDraft(ctx context.Context, a Audit, drafts PromptDraftWriter, restoreID int64, change promptcatalog.DraftChange) error {
	return writeDraft(ctx, a, drafts, "prompts.draft.restore", change.Key, []promptcatalog.DraftChange{change},
		map[string]any{"restore_release_id": restoreID})
}

// TakeShippedPrompts puts the text this build ships for each change's prompt
// in the draft, or the removal of a prompt it no longer uses.
func TakeShippedPrompts(ctx context.Context, a Audit, drafts PromptDraftWriter, changes []promptcatalog.DraftChange) error {
	return writeDraft(ctx, a, drafts, "prompts.draft.take_shipped", draftTarget, changes,
		map[string]any{"shipped_sha256": promptcatalog.MustEmbedded().SHA256})
}

func writeDraft(ctx context.Context, a Audit, drafts PromptDraftWriter, action, target string, changes []promptcatalog.DraftChange, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	written := make([]map[string]string, 0, len(changes))
	for _, change := range changes {
		body := strings.TrimSpace(change.Body)
		sha := promptcatalog.DraftRemoved
		if body != "" {
			sha = promptcatalog.BodySHA256(body)
		}
		written = append(written, map[string]string{"key": change.Key, "body_sha256": sha})
	}
	details["changes"] = written
	return a.Run(ctx, action, target, details, func() error {
		return drafts.WriteDraft(ctx, changes, a.actor())
	})
}

// DiscardPromptDraft empties the draft, if it is still the one with
// fingerprint.
func DiscardPromptDraft(ctx context.Context, a Audit, drafts PromptDraftDiscarder, fingerprint string) error {
	return a.Run(ctx, "prompts.draft.discard_all", draftTarget, map[string]any{"fingerprint": fingerprint}, func() error {
		return drafts.DiscardDraft(ctx, fingerprint)
	})
}

// PublishPromptDraft makes the draft with fingerprint live as one new
// release in place of expectedCurrentID.
func PublishPromptDraft(ctx context.Context, a Audit, drafts PromptDraftPublisher, expectedCurrentID int64, fingerprint, note string) (*promptcatalog.ActivationResult, error) {
	note = strings.TrimSpace(note)
	var result *promptcatalog.ActivationResult
	err := a.Run(ctx, "prompts.draft.publish", draftTarget,
		map[string]any{"expected_release_id": expectedCurrentID, "fingerprint": fingerprint, "note": note}, func() error {
			var err error
			result, err = drafts.PublishDraft(ctx, expectedCurrentID, fingerprint, a.actor(), note)
			return err
		})
	return result, err
}
