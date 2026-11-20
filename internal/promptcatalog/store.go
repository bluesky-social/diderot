package promptcatalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Release struct {
	ID             int64     `json:"id"`
	SourceRevision string    `json:"source_revision"`
	Bundle         Bundle    `json:"bundle"`
	CreatedBy      string    `json:"created_by"`
	Note           string    `json:"note,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	Active         bool      `json:"active"`
	// Example marks a release EnsureSeed stored from the built-in example.
	Example bool `json:"example"`
}

// SeedActor creates the releases EnsureSeed installs from the example.
const SeedActor = "diderot:seed"

// ErrActiveReleaseChanged is Activate's compare-and-swap failure: another
// writer activated a release after the caller read the one it expected.
var ErrActiveReleaseChanged = errors.New("prompt catalog: active release changed")

// ErrNotFound is a release, prompt version, or legacy prompt that does not
// exist.
var ErrNotFound = errors.New("not found")

// PromptVersion is one body a prompt had while it was active: the activation
// that changed it to that body, the release it activated, who activated it,
// and when.
type PromptVersion struct {
	ActivationID int64     `json:"activation_id"`
	ReleaseID    int64     `json:"release_id"`
	SHA256       string    `json:"sha256"`
	ActivatedBy  string    `json:"activated_by"`
	ActivatedAt  time.Time `json:"activated_at"`
	Note         string    `json:"note,omitempty"`
	Example      bool      `json:"example"`
}

// PromptChange is one version of a prompt with its text and the text it
// replaced. First marks the version that had nothing to replace.
type PromptChange struct {
	PromptVersion
	Body         string `json:"body"`
	PreviousBody string `json:"previous_body,omitempty"`
	First        bool   `json:"first"`
}

// Activation is one change of the active release: the release activated, the
// one it replaced (zero for the first), who activated it and when, and the
// keys whose text it changed, added, or removed. Again marks a release that
// was active before.
type Activation struct {
	ID            int64     `json:"id"`
	ReleaseID     int64     `json:"release_id"`
	FromReleaseID int64     `json:"from_release_id,omitempty"`
	ActivatedBy   string    `json:"activated_by"`
	ActivatedAt   time.Time `json:"activated_at"`
	CreatedBy     string    `json:"created_by"`
	Note          string    `json:"note,omitempty"`
	Again         bool      `json:"again"`
	Changed       []string  `json:"changed"`
}

// ActivationResult distinguishes a real active-identity transition from an
// effective no-op. For a no-op, Release is the release that remains active;
// the uploaded target remains available in immutable release history.
type ActivationResult struct {
	Release Release `json:"release"`
	Changed bool    `json:"changed"`
}

// LegacyAskPrompt is an Ask prompt from before catalog releases, which older
// Ask sessions record as theirs. It is never activatable.
type LegacyAskPrompt struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	CreatedBy string    `json:"created_by,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// EnsureSeed stores the binary's example catalog as release 1 and activates
// it, only on a database with no active release. Once any release is active
// it returns that release and stores nothing, so upgrades never add example
// releases to an install's history; an administrator takes a newer example's
// text through the draft.
func (s *Store) EnsureSeed(ctx context.Context, bundle Bundle, sourceRevision string) (*Release, error) {
	var activeID int64
	err := s.pool.QueryRow(ctx, `SELECT release_id FROM prompt_active_release WHERE singleton`).Scan(&activeID)
	if err == nil {
		return s.Get(ctx, activeID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("prompt catalog: check active release: %w", err)
	}
	release, err := s.Create(ctx, bundle, sourceRevision, SeedActor, "initial embedded prompt release")
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: begin seed activation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		INSERT INTO prompt_active_release (singleton, release_id, activated_by)
		VALUES (true, $1, 'diderot:seed') ON CONFLICT (singleton) DO NOTHING`, release.ID)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: seed active release: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO prompt_release_activations (from_release_id, to_release_id, activated_by)
			VALUES (NULL, $1, 'diderot:seed')`, release.ID); err != nil {
			return nil, fmt.Errorf("prompt catalog: audit seed activation: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("prompt catalog: commit seed activation: %w", err)
	}
	return s.Get(ctx, release.ID)
}

// Create stores a complete immutable bundle. sourceRevision is idempotent: a
// retry with identical bytes returns the existing release, while a different
// bundle under the same revision is rejected as a non-reproducible build.
func (s *Store) Create(ctx context.Context, bundle Bundle, sourceRevision, createdBy, note string) (*Release, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: begin release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, err := s.createWith(ctx, tx, bundle, sourceRevision, createdBy, note)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("prompt catalog: commit release: %w", err)
	}
	return s.Get(ctx, id)
}

// createWith is Create inside tx, returning the release's id.
func (s *Store) createWith(ctx context.Context, tx pgx.Tx, bundle Bundle, sourceRevision, createdBy, note string) (int64, error) {
	if err := ValidateWire(bundle); err != nil {
		return 0, err
	}
	if err := ValidatePlaceholders(bundle); err != nil {
		return 0, err
	}
	sourceRevision = strings.TrimSpace(sourceRevision)
	createdBy = strings.TrimSpace(createdBy)
	note = strings.TrimSpace(note)
	if sourceRevision == "" || len(sourceRevision) > 200 {
		return 0, errors.New("prompt catalog: source revision is required and must be at most 200 characters")
	}
	if createdBy == "" || len(createdBy) > 300 {
		return 0, errors.New("prompt catalog: created_by is required and must be at most 300 characters")
	}
	if len(note) > 1000 {
		return 0, errors.New("prompt catalog: note must be at most 1000 characters")
	}
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO prompt_releases
		    (source_revision, bundle_sha256, schema_version, created_by, note)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (source_revision) DO NOTHING
		RETURNING id`, sourceRevision, bundle.SHA256, bundle.SchemaVersion, createdBy, note).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		var existingHash string
		if err := tx.QueryRow(ctx, `
			SELECT id, bundle_sha256 FROM prompt_releases WHERE source_revision = $1`, sourceRevision).Scan(&id, &existingHash); err != nil {
			return 0, fmt.Errorf("prompt catalog: find concurrent release: %w", err)
		}
		if existingHash != bundle.SHA256 {
			return 0, fmt.Errorf("prompt catalog: source revision %q already exists with a different bundle", sourceRevision)
		}
		return id, nil
	}
	if err != nil {
		return 0, fmt.Errorf("prompt catalog: insert release: %w", err)
	}
	for _, prompt := range bundle.Prompts {
		_, err = tx.Exec(ctx, `
			INSERT INTO prompt_release_entries
			    (release_id, prompt_key, source_path, description, body, body_sha256)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			id, prompt.Key, prompt.Path, prompt.Description, prompt.Body, prompt.SHA256)
		if err != nil {
			return 0, fmt.Errorf("prompt catalog: insert %s: %w", prompt.Key, err)
		}
	}
	return id, nil
}

func (s *Store) Current(ctx context.Context) (*Release, error) {
	var id int64
	if err := s.pool.QueryRow(ctx, `SELECT release_id FROM prompt_active_release WHERE singleton`).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("prompt catalog: no active release")
		}
		return nil, fmt.Errorf("prompt catalog: current release: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *Store) Get(ctx context.Context, id int64) (*Release, error) {
	return s.getWith(ctx, s.pool, id)
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (s *Store) getWith(ctx context.Context, q queryer, id int64) (*Release, error) {
	var release Release
	var schemaVersion int
	err := q.QueryRow(ctx, `
		SELECT r.id, r.source_revision, r.bundle_sha256, r.schema_version,
		       r.created_by, r.note, r.created_at,
		       COALESCE(a.release_id = r.id, false)
		  FROM prompt_releases r
		  LEFT JOIN prompt_active_release a ON a.singleton
		 WHERE r.id = $1`, id).Scan(
		&release.ID, &release.SourceRevision, &release.Bundle.SHA256, &schemaVersion,
		&release.CreatedBy, &release.Note, &release.CreatedAt, &release.Active,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("prompt catalog: release %d %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("prompt catalog: get release: %w", err)
	}
	release.Bundle.SchemaVersion = schemaVersion
	release.Example = release.CreatedBy == SeedActor
	rows, err := q.Query(ctx, `
		SELECT prompt_key, source_path, description, body, body_sha256
		  FROM prompt_release_entries WHERE release_id = $1 ORDER BY prompt_key`, id)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: get entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var prompt Prompt
		if err := rows.Scan(&prompt.Key, &prompt.Path, &prompt.Description, &prompt.Body, &prompt.SHA256); err != nil {
			return nil, fmt.Errorf("prompt catalog: scan entry: %w", err)
		}
		release.Bundle.Prompts = append(release.Bundle.Prompts, prompt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prompt catalog: scan entries: %w", err)
	}
	if err := ValidateWire(release.Bundle); err != nil {
		return nil, fmt.Errorf("prompt catalog: stored release %d is invalid: %w", id, err)
	}
	return &release, nil
}

// Activate moves the singleton pointer only if expectedCurrentID still owns
// it. This prevents a stale editor or push from replacing a newer release.
// Releases whose effective bundle is already active are retained as immutable
// deployment provenance, but do not move the pointer or create an activation
// event. Besides keeping activation history meaningful, this prevents caches
// keyed by release identity from being invalidated by metadata-only commits.
func (s *Store) Activate(ctx context.Context, id, expectedCurrentID int64, activatedBy string) (*ActivationResult, error) {
	activatedBy = strings.TrimSpace(activatedBy)
	if activatedBy == "" {
		return nil, errors.New("prompt catalog: activated_by is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: begin activation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	target, err := s.getWith(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	currentID, err := lockActive(ctx, tx)
	if err != nil {
		return nil, err
	}
	if currentID != expectedCurrentID {
		return nil, fmt.Errorf("%w from expected id %d", ErrActiveReleaseChanged, expectedCurrentID)
	}
	current, err := s.getWith(ctx, tx, currentID)
	if err != nil {
		return nil, err
	}
	if target.Bundle.EffectiveSHA256() == current.Bundle.EffectiveSHA256() {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("prompt catalog: commit unchanged activation: %w", err)
		}
		return &ActivationResult{Release: *current}, nil
	}
	if err := movePointer(ctx, tx, id, expectedCurrentID, activatedBy); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("prompt catalog: commit activation: %w", err)
	}
	release, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &ActivationResult{Release: *release, Changed: true}, nil
}

// lockActive locks the active release pointer for the rest of tx and
// returns the release it names.
func lockActive(ctx context.Context, tx pgx.Tx) (int64, error) {
	var currentID int64
	if err := tx.QueryRow(ctx, `
		SELECT release_id FROM prompt_active_release
		 WHERE singleton FOR UPDATE`).Scan(&currentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("prompt catalog: no active release")
		}
		return 0, fmt.Errorf("prompt catalog: lock active release: %w", err)
	}
	return currentID, nil
}

// movePointer makes release id active in place of expectedCurrentID, whose
// pointer tx has locked, and records the activation.
func movePointer(ctx context.Context, tx pgx.Tx, id, expectedCurrentID int64, activatedBy string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE prompt_active_release
		   SET release_id = $1, activated_by = $3, activated_at = now()
		 WHERE singleton AND release_id = $2`, id, expectedCurrentID, activatedBy)
	if err != nil {
		return fmt.Errorf("prompt catalog: activate release: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w from expected id %d", ErrActiveReleaseChanged, expectedCurrentID)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO prompt_release_activations (from_release_id, to_release_id, activated_by)
		VALUES ($1, $2, $3)`, expectedCurrentID, id, activatedBy); err != nil {
		return fmt.Errorf("prompt catalog: audit activation: %w", err)
	}
	return nil
}

// promptVersionColumns and promptVersionChanges select, per activation a and
// entry e of the release it activated, the activations that changed e's
// text: p is the entry it replaced, absent for the first.
const (
	promptVersionColumns = `a.id, a.to_release_id, e.body_sha256, a.activated_by, a.activated_at, r.note, r.created_by`
	promptVersionChanges = `
		  FROM prompt_release_activations a
		  JOIN prompt_releases r ON r.id = a.to_release_id
		  JOIN prompt_release_entries e ON e.release_id = a.to_release_id
		  LEFT JOIN prompt_release_entries p ON p.release_id = a.from_release_id AND p.prompt_key = e.prompt_key
		 WHERE p.body_sha256 IS DISTINCT FROM e.body_sha256`
)

func scanPromptVersion(row pgx.Row, version *PromptVersion, extra ...any) error {
	var createdBy string
	err := row.Scan(append([]any{&version.ActivationID, &version.ReleaseID, &version.SHA256,
		&version.ActivatedBy, &version.ActivatedAt, &version.Note, &createdBy}, extra...)...)
	version.Example = createdBy == SeedActor
	return err
}

// PromptHistory lists the bodies key has had as active, newest first: one
// entry per activation that changed it, starting with the current body.
// before, when not zero, lists only activations older than that one.
func (s *Store) PromptHistory(ctx context.Context, key string, before int64, limit int) ([]PromptVersion, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT `+promptVersionColumns+promptVersionChanges+`
		   AND e.prompt_key = $1 AND ($2::bigint = 0 OR a.id < $2::bigint)
		 ORDER BY a.id DESC LIMIT $3`, key, before, limit)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: %s history: %w", key, err)
	}
	defer rows.Close()
	var out []PromptVersion
	for rows.Next() {
		var version PromptVersion
		if err := scanPromptVersion(rows, &version); err != nil {
			return nil, fmt.Errorf("prompt catalog: scan %s history: %w", key, err)
		}
		out = append(out, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prompt catalog: %s history: %w", key, err)
	}
	return out, nil
}

// PromptChange returns the version of key that release made active, with
// the text it replaced. A release made active more than once is the change
// it made the first time, as a version's page compares it too. It fails with
// ErrNotFound when release never changed key's text.
func (s *Store) PromptChange(ctx context.Context, key string, release int64) (*PromptChange, error) {
	var change PromptChange
	var previous *string
	err := scanPromptVersion(s.pool.QueryRow(ctx, `SELECT `+promptVersionColumns+`, e.body, p.body`+promptVersionChanges+`
		   AND e.prompt_key = $1 AND a.to_release_id = $2
		 ORDER BY a.id LIMIT 1`, key, release), &change.PromptVersion, &change.Body, &previous)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("prompt catalog: %s in release %d %w", key, release, ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("prompt catalog: %s in release %d: %w", key, release, err)
	}
	change.First = previous == nil
	if previous != nil {
		change.PreviousBody = *previous
	}
	return &change, nil
}

// LatestVersions returns, for every key any activation changed, the newest
// activation that changed it.
func (s *Store) LatestVersions(ctx context.Context) (map[string]PromptVersion, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (e.prompt_key) `+promptVersionColumns+`, e.prompt_key`+promptVersionChanges+`
		 ORDER BY e.prompt_key, a.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: latest versions: %w", err)
	}
	defer rows.Close()
	out := map[string]PromptVersion{}
	for rows.Next() {
		var key string
		var version PromptVersion
		if err := scanPromptVersion(rows, &version, &key); err != nil {
			return nil, fmt.Errorf("prompt catalog: scan latest versions: %w", err)
		}
		out[key] = version
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prompt catalog: latest versions: %w", err)
	}
	return out, nil
}

// ActiveSince reports who activated the active release, and when.
func (s *Store) ActiveSince(ctx context.Context) (string, time.Time, error) {
	var by string
	var at time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT activated_by, activated_at FROM prompt_active_release WHERE singleton`).Scan(&by, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, errors.New("prompt catalog: no active release")
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("prompt catalog: active since: %w", err)
	}
	return by, at, nil
}

const activationSelect = `
		SELECT a.id, a.to_release_id, COALESCE(a.from_release_id, 0), a.activated_by, a.activated_at,
		       r.created_by, r.note,
		       EXISTS (SELECT 1 FROM prompt_release_activations b
		                WHERE b.to_release_id = a.to_release_id AND b.id < a.id),
		       ARRAY(SELECT changed.key FROM (
		                SELECT e.prompt_key AS key FROM prompt_release_entries e
		                  LEFT JOIN prompt_release_entries p
		                    ON p.release_id = a.from_release_id AND p.prompt_key = e.prompt_key
		                 WHERE e.release_id = a.to_release_id AND p.body_sha256 IS DISTINCT FROM e.body_sha256
		                UNION ALL
		                SELECT p.prompt_key FROM prompt_release_entries p
		                 WHERE p.release_id = a.from_release_id
		                   AND NOT EXISTS (SELECT 1 FROM prompt_release_entries e
		                                    WHERE e.release_id = a.to_release_id AND e.prompt_key = p.prompt_key)
		             ) changed ORDER BY changed.key)
		  FROM prompt_release_activations a
		  JOIN prompt_releases r ON r.id = a.to_release_id`

// Activations lists changes of the active release, newest first. before,
// when not zero, lists only activations older than that one.
func (s *Store) Activations(ctx context.Context, before int64, limit int) ([]Activation, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return s.activations(ctx, activationSelect+`
		 WHERE ($1::bigint = 0 OR a.id < $1::bigint)
		 ORDER BY a.id DESC LIMIT $2`, before, limit)
}

// ReleaseActivations lists every time release id was activated, oldest
// first.
func (s *Store) ReleaseActivations(ctx context.Context, id int64) ([]Activation, error) {
	return s.activations(ctx, activationSelect+`
		 WHERE a.to_release_id = $1
		 ORDER BY a.id`, id)
}

func (s *Store) activations(ctx context.Context, query string, args ...any) ([]Activation, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: activations: %w", err)
	}
	defer rows.Close()
	var out []Activation
	for rows.Next() {
		var activation Activation
		if err := rows.Scan(&activation.ID, &activation.ReleaseID, &activation.FromReleaseID,
			&activation.ActivatedBy, &activation.ActivatedAt, &activation.CreatedBy, &activation.Note,
			&activation.Again, &activation.Changed); err != nil {
			return nil, fmt.Errorf("prompt catalog: scan activations: %w", err)
		}
		out = append(out, activation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prompt catalog: activations: %w", err)
	}
	return out, nil
}

func (s *Store) List(ctx context.Context, limit int) ([]Release, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.id FROM prompt_releases r ORDER BY r.created_at DESC, r.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("prompt catalog: list releases: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := make([]Release, 0, len(ids))
	for _, id := range ids {
		release, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *release)
	}
	return out, nil
}

// LegacyAskPrompt returns one Ask prompt from before catalog releases.
func (s *Store) LegacyAskPrompt(ctx context.Context, id int64) (*LegacyAskPrompt, error) {
	prompt := LegacyAskPrompt{ID: id}
	err := s.pool.QueryRow(ctx, `
		SELECT body, COALESCE(created_by, ''), COALESCE(note, ''), created_at
		  FROM legacy_ask_prompts WHERE id = $1`, id).Scan(&prompt.Body, &prompt.CreatedBy, &prompt.Note, &prompt.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("prompt catalog: legacy Ask prompt %d %w", id, ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("prompt catalog: legacy Ask prompt %d: %w", id, err)
	}
	return &prompt, nil
}
