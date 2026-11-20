package promptcatalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// The draft is the one set of changes to the live prompts that administrators
// are preparing: the live release with each entry applied. It changes nothing
// that runs until PublishDraft makes it live, which clears it.

// DraftRemoved is a draft entry's DraftChange.DraftSHA256 when it removes
// its prompt.
const DraftRemoved = "removed"

var (
	// ErrDraftChanged is a draft write or publish whose writer saw a draft or
	// live text that has since changed.
	ErrDraftChanged = errors.New("prompt catalog: draft changed")
	// ErrDraftOutOfDate is a publish refused because live text changed under
	// an entry after it was written.
	ErrDraftOutOfDate = errors.New("prompt catalog: draft is out of date")
)

// DraftEntry is one change the draft makes to the live prompts: new text for
// Key, or its removal.
type DraftEntry struct {
	Key    string
	Body   string
	Remove bool
	// BaseSHA256 is the hash of Key's live text when the entry was written,
	// empty when live had no such prompt.
	BaseSHA256 string
	UpdatedBy  string
	UpdatedAt  time.Time
}

// SHA256 identifies the entry's text for a DraftChange: the body's hash, or
// DraftRemoved.
func (e DraftEntry) SHA256() string {
	if e.Remove {
		return DraftRemoved
	}
	return hash(e.Body)
}

// Draft is the live release and the draft's entries, in key order.
type Draft struct {
	Live    Release
	Entries []DraftEntry
}

// Entry returns key's entry.
func (d Draft) Entry(key string) (DraftEntry, bool) {
	i := slices.IndexFunc(d.Entries, func(e DraftEntry) bool { return e.Key == key })
	if i < 0 {
		return DraftEntry{}, false
	}
	return d.Entries[i], true
}

// OutOfDate reports whether live text changed under entry after it was
// written.
func (d Draft) OutOfDate(entry DraftEntry) bool {
	return entry.BaseSHA256 != liveSHA256(d.Live.Bundle, entry.Key)
}

// Bundle is the live release with every entry applied. Its SHA256 is the
// draft's fingerprint: a publish or discard names the one it was shown.
func (d Draft) Bundle() (Bundle, error) { return applyDraft(d.Live.Bundle, d.Entries) }

// DraftChange is one write to the draft: Key's new text, or its removal when
// Body is empty. It applies only while the draft and live text of Key are
// still the ones the writer saw: DraftSHA256 is the draft entry's SHA256 (empty
// for none), and LiveSHA256 the live text's hash (empty when live has none).
type DraftChange struct {
	Key                     string
	Body                    string
	DraftSHA256, LiveSHA256 string
}

// Draft reads the live release and the draft from one snapshot, so a
// publish between the two reads cannot pair one release with another's
// draft.
func (s *Store) Draft(ctx context.Context) (*Draft, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: begin draft read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var liveID int64
	if err := tx.QueryRow(ctx, `SELECT release_id FROM prompt_active_release WHERE singleton`).Scan(&liveID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("prompt catalog: no active release")
		}
		return nil, fmt.Errorf("prompt catalog: draft live release: %w", err)
	}
	draft, err := s.draftWith(ctx, tx, liveID)
	if err != nil {
		return nil, err
	}
	return &draft, nil
}

// WriteDraft applies changes to the draft, all or none. Text equal to the
// live text drops the key's entry, as does removing a prompt live does not
// have. It fails with ErrDraftChanged when any change's draft or live text is
// no longer the one its writer saw, and refuses text that could not go live:
// a required prompt removed, a prompt this build neither ships nor reads, or
// an unknown variable.
func (s *Store) WriteDraft(ctx context.Context, changes []DraftChange, actor string) error {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return errors.New("prompt catalog: draft writer is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("prompt catalog: begin draft write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	liveID, err := holdActive(ctx, tx)
	if err != nil {
		return err
	}
	if err := lockDraft(ctx, tx); err != nil {
		return err
	}
	live, err := s.getWith(ctx, tx, liveID)
	if err != nil {
		return err
	}
	entries, err := draftEntries(ctx, tx)
	if err != nil {
		return err
	}
	draft := Draft{Live: *live, Entries: entries}
	for _, change := range changes {
		body := strings.TrimSpace(change.Body)
		seen := ""
		if entry, ok := draft.Entry(change.Key); ok {
			seen = entry.SHA256()
		}
		base := liveSHA256(live.Bundle, change.Key)
		if seen != change.DraftSHA256 || base != change.LiveSHA256 {
			return fmt.Errorf("%w: %s", ErrDraftChanged, change.Key)
		}
		liveBody, inLive := textOf(live.Bundle, change.Key)
		switch {
		case body == "" && slices.Contains(requiredKeys(), change.Key):
			return fmt.Errorf("prompt catalog: %s is required and cannot be removed", change.Key)
		case body != "" && !inLive && !knownPrompt(change.Key):
			return fmt.Errorf("prompt catalog: %s is not a prompt this version of Diderot uses", change.Key)
		case body != "":
			if err := ValidateText(change.Key, body); err != nil {
				return err
			}
		}
		if body == liveBody {
			if _, err := tx.Exec(ctx, `DELETE FROM prompt_draft_entries WHERE prompt_key = $1`, change.Key); err != nil {
				return fmt.Errorf("prompt catalog: drop draft %s: %w", change.Key, err)
			}
			continue
		}
		var stored, baseSHA *string
		if body != "" {
			stored = &body
		}
		if base != "" {
			baseSHA = &base
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO prompt_draft_entries (prompt_key, body, base_sha256, updated_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (prompt_key) DO UPDATE
			   SET body = EXCLUDED.body, base_sha256 = EXCLUDED.base_sha256,
			       updated_by = EXCLUDED.updated_by, updated_at = now()`,
			change.Key, stored, baseSHA, actor); err != nil {
			return fmt.Errorf("prompt catalog: write draft %s: %w", change.Key, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("prompt catalog: commit draft write: %w", err)
	}
	return nil
}

// DiscardDraft empties the draft, only while its fingerprint is still
// fingerprint; it fails with ErrDraftChanged otherwise.
func (s *Store) DiscardDraft(ctx context.Context, fingerprint string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("prompt catalog: begin draft discard: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	liveID, err := holdActive(ctx, tx)
	if err != nil {
		return err
	}
	if err := lockDraft(ctx, tx); err != nil {
		return err
	}
	draft, err := s.draftWith(ctx, tx, liveID)
	if err != nil {
		return err
	}
	if err := draft.fingerprintIs(fingerprint); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM prompt_draft_entries`); err != nil {
		return fmt.Errorf("prompt catalog: discard draft: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("prompt catalog: commit draft discard: %w", err)
	}
	return nil
}

// PublishDraft makes the draft live as one new release in place of
// expectedCurrentID, and empties the draft, in one transaction. It fails with
// ErrActiveReleaseChanged when expectedCurrentID is no longer live,
// ErrDraftChanged when the draft's fingerprint is no longer fingerprint, and
// ErrDraftOutOfDate when live text changed under an entry. A draft whose
// text is already live is emptied and changes nothing.
func (s *Store) PublishDraft(ctx context.Context, expectedCurrentID int64, fingerprint, actor, note string) (*ActivationResult, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, errors.New("prompt catalog: activated_by is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: begin publish: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	currentID, err := lockActive(ctx, tx)
	if err != nil {
		return nil, err
	}
	if currentID != expectedCurrentID {
		return nil, fmt.Errorf("%w from expected id %d", ErrActiveReleaseChanged, expectedCurrentID)
	}
	if err := lockDraft(ctx, tx); err != nil {
		return nil, err
	}
	draft, err := s.draftWith(ctx, tx, currentID)
	if err != nil {
		return nil, err
	}
	if err := draft.fingerprintIs(fingerprint); err != nil {
		return nil, err
	}
	for _, entry := range draft.Entries {
		if draft.OutOfDate(entry) {
			return nil, fmt.Errorf("%w: %s", ErrDraftOutOfDate, entry.Key)
		}
	}
	bundle, err := draft.Bundle()
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM prompt_draft_entries`); err != nil {
		return nil, fmt.Errorf("prompt catalog: clear published draft: %w", err)
	}
	result := &ActivationResult{Release: draft.Live}
	if bundle.EffectiveSHA256() != draft.Live.Bundle.EffectiveSHA256() {
		id, err := s.createWith(ctx, tx, bundle, "ui:"+bundle.SHA256, actor, note)
		if err != nil {
			return nil, err
		}
		if err := movePointer(ctx, tx, id, currentID, actor); err != nil {
			return nil, err
		}
		result.Changed = true
		result.Release.ID = id
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("prompt catalog: commit publish: %w", err)
	}
	if result.Changed {
		release, err := s.Get(ctx, result.Release.ID)
		if err != nil {
			return nil, err
		}
		result.Release = *release
	}
	return result, nil
}

func (s *Store) draftWith(ctx context.Context, q queryer, liveID int64) (Draft, error) {
	live, err := s.getWith(ctx, q, liveID)
	if err != nil {
		return Draft{}, err
	}
	entries, err := draftEntries(ctx, q)
	if err != nil {
		return Draft{}, err
	}
	return Draft{Live: *live, Entries: entries}, nil
}

func (d Draft) fingerprintIs(fingerprint string) error {
	bundle, err := d.Bundle()
	if err != nil {
		return err
	}
	if bundle.SHA256 != fingerprint {
		return ErrDraftChanged
	}
	return nil
}

// holdActive keeps the live release from changing for the rest of tx and
// returns it. Every transaction that takes both locks takes this one first.
func holdActive(ctx context.Context, tx pgx.Tx) (int64, error) {
	var id int64
	if err := tx.QueryRow(ctx, `SELECT release_id FROM prompt_active_release WHERE singleton FOR SHARE`).Scan(&id); err != nil {
		return 0, fmt.Errorf("prompt catalog: draft live release: %w", err)
	}
	return id, nil
}

// lockDraft serializes the draft's writers for the rest of tx. Readers are
// not blocked.
func lockDraft(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `LOCK TABLE prompt_draft_entries IN EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("prompt catalog: lock draft: %w", err)
	}
	return nil
}

func draftEntries(ctx context.Context, q queryer) ([]DraftEntry, error) {
	rows, err := q.Query(ctx, `
		SELECT prompt_key, body, COALESCE(base_sha256, ''), updated_by, updated_at
		  FROM prompt_draft_entries ORDER BY prompt_key`)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: read draft: %w", err)
	}
	defer rows.Close()
	var entries []DraftEntry
	for rows.Next() {
		var entry DraftEntry
		var body *string
		if err := rows.Scan(&entry.Key, &body, &entry.BaseSHA256, &entry.UpdatedBy, &entry.UpdatedAt); err != nil {
			return nil, fmt.Errorf("prompt catalog: scan draft: %w", err)
		}
		if body == nil {
			entry.Remove = true
		} else {
			entry.Body = *body
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prompt catalog: read draft: %w", err)
	}
	return entries, nil
}

// applyDraft returns live with each entry applied. A prompt live does not
// have takes its path and description from the shipped example, or from the
// notes.
func applyDraft(live Bundle, entries []DraftEntry) (Bundle, error) {
	prompts := map[string]Prompt{}
	for _, prompt := range live.Prompts {
		prompts[prompt.Key] = prompt
	}
	for _, entry := range entries {
		if entry.Remove {
			delete(prompts, entry.Key)
			continue
		}
		prompt, ok := prompts[entry.Key]
		if !ok {
			if prompt, ok = promptDefinition(entry.Key); !ok {
				// An edit of a prompt live had then, before an older
				// version without it went live. The entry is out of date,
				// so it cannot go live, but the draft must still show and
				// discard.
				prompt = Prompt{Key: entry.Key, Path: entry.Key + ".md"}
			}
		}
		prompt.Body = entry.Body
		prompt.SHA256 = hash(entry.Body)
		prompts[entry.Key] = prompt
	}
	out := Bundle{SchemaVersion: live.SchemaVersion, Prompts: make([]Prompt, 0, len(prompts))}
	for _, prompt := range prompts {
		out.Prompts = append(out.Prompts, prompt)
	}
	sort.Slice(out.Prompts, func(i, j int) bool { return out.Prompts[i].Key < out.Prompts[j].Key })
	out.SHA256 = bundleHash(out)
	return out, nil
}

// shipped is the example this build ships.
var shipped = sync.OnceValue(MustEmbedded)

// promptDefinition is key's path and description when this build ships it
// or it is a note.
func promptDefinition(key string) (Prompt, bool) {
	for _, prompt := range slices.Concat(shipped().Prompts, notes) {
		if prompt.Key == key {
			return Prompt{Key: key, Path: prompt.Path, Description: prompt.Description}, true
		}
	}
	return Prompt{}, false
}

func knownPrompt(key string) bool {
	_, ok := promptDefinition(key)
	return ok
}

func textOf(bundle Bundle, key string) (string, bool) {
	text, err := bundle.Text(key)
	return text, err == nil
}

// liveSHA256 is key's text hash in bundle, empty when it has none.
func liveSHA256(bundle Bundle, key string) string {
	i := sort.Search(len(bundle.Prompts), func(i int) bool { return bundle.Prompts[i].Key >= key })
	if i == len(bundle.Prompts) || bundle.Prompts[i].Key != key {
		return ""
	}
	return bundle.Prompts[i].SHA256
}
