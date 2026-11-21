package sessioningest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

// Push ingest stores agent sessions submitted over HTTP. A push
// replaces a session's whole snapshot. Diderot keeps the received bytes, the
// records each decoder version produced from them, and the revision, and
// queues the session for summarization through the shared candidate upsert.

// PushFormats is the registry push accepts. bsky.session.v1 is the session
// feed's format; its envelope carries its own owner and revision, which push
// takes from the authenticated principal and the transport instead.
func PushFormats() *sessiondecode.Registry {
	return sessiondecode.NewRegistry(sessiondecode.ClaudeCode{}, sessiondecode.Codex{}, sessiondecode.Antigravity{}, sessiondecode.ATIF{}, sessiondecode.STS{})
}

// PushConfig bounds push ingest. Every limit is required when push is on.
type PushConfig struct {
	// RevisionKey keys revision tokens. Rotating it changes every token, so
	// writers holding an old one get 412 and refetch.
	RevisionKey []byte
	// MaxDocumentBytes caps one document after decompression.
	MaxDocumentBytes int64
	// OwnerQuotaBytes caps an owner's stored bytes (deduplicated chunks of
	// received documents and materialized records).
	OwnerQuotaBytes int64
	// OwnerQuotaRevisions caps an owner's stored (unpruned) revisions.
	OwnerQuotaRevisions int64
	// Policy admits pushed sessions and sets their visibility, as for the feed.
	Policy  Policy
	Formats *sessiondecode.Registry
}

// PushStore is the durable side of push ingest.
type PushStore struct {
	pool *pgxpool.Pool
	cfg  PushConfig
}

const minRevisionKeyBytes = 32

// NewPushStore validates cfg.
func NewPushStore(pool *pgxpool.Pool, cfg PushConfig) (*PushStore, error) {
	switch {
	case len(cfg.RevisionKey) < minRevisionKeyBytes:
		return nil, fmt.Errorf("sessioningest: push revision key must be at least %d bytes", minRevisionKeyBytes)
	case cfg.MaxDocumentBytes <= 0 || cfg.OwnerQuotaBytes <= 0 || cfg.OwnerQuotaRevisions <= 0:
		return nil, errors.New("sessioningest: push size and quota limits must be positive")
	case cfg.Formats == nil:
		return nil, errors.New("sessioningest: push needs a decoder registry")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	return &PushStore{pool: pool, cfg: cfg}, nil
}

// MaxDocumentBytes is the configured post-decompression cap.
func (p *PushStore) MaxDocumentBytes() int64 { return p.cfg.MaxDocumentBytes }

// Decoder resolves a pushable format.
func (p *PushStore) Decoder(format string) (sessiondecode.Decoder, bool) {
	return p.cfg.Formats.Lookup(format)
}

// Formats lists the pushable formats.
func (p *PushStore) Formats() []string { return p.cfg.Formats.Formats() }

// Push errors the HTTP layer maps onto status codes.
var (
	// ErrPushNotFound: no pushed session with that id.
	ErrPushNotFound = errors.New("sessioningest: no pushed session")
	// ErrPushForbidden: the session belongs to another owner.
	ErrPushForbidden = errors.New("sessioningest: session belongs to another owner")
)

// PushConflictError is a non-precondition conflict: the session is bound to
// another format or writer.
type PushConflictError struct{ Reason string }

func (e *PushConflictError) Error() string { return "sessioningest: push conflict: " + e.Reason }

// PushPreconditionError is a failed If-Match / If-None-Match.
type PushPreconditionError struct {
	// Current is the head revision, or "" when the session has none.
	Current string
	// BodyMatchesCurrent reports that the submitted bytes are the head: a
	// create or update retried after a lost response already landed.
	BodyMatchesCurrent bool
}

func (e *PushPreconditionError) Error() string { return "sessioningest: precondition failed" }

// PushQuotaError is an owner storage quota refusal.
type PushQuotaError struct {
	Quota string // "bytes" or "revisions"
	Limit int64
	Used  int64
}

func (e *PushQuotaError) Error() string {
	return fmt.Sprintf("sessioningest: owner %s quota exceeded (%d of %d)", e.Quota, e.Used, e.Limit)
}

// PushDecodeError is a document its decoder rejected.
type PushDecodeError struct{ Err error }

func (e *PushDecodeError) Error() string {
	return "sessioningest: undecodable document: " + e.Err.Error()
}
func (e *PushDecodeError) Unwrap() error { return e.Err }

// PushHead is a pushed session's binding as stored.
type PushHead struct {
	Owner    string
	Format   string
	Revision string // "" before the first accepted push
}

// Head returns the session's push binding for owner. It fails with
// ErrPushNotFound when nothing was pushed, ErrPushForbidden for another
// owner's session, and a PushConflictError when the feed owns the id.
func (p *PushStore) Head(ctx context.Context, owner, sessionID string) (PushHead, error) {
	var h PushHead
	var writer string
	var head *string
	err := p.pool.QueryRow(ctx, `SELECT owner_canonical, writer, format, head_revision
		FROM session_ingest_bindings WHERE session_id=$1`, sessionID).Scan(&h.Owner, &writer, &h.Format, &head)
	if errors.Is(err, pgx.ErrNoRows) {
		var legacy bool
		if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM session_ingest_candidates WHERE session_id=$1)`, sessionID).Scan(&legacy); err != nil {
			return PushHead{}, fmt.Errorf("sessioningest: push legacy lookup: %w", err)
		}
		if legacy {
			return PushHead{}, &PushConflictError{Reason: "session is written by the session feed"}
		}
		return PushHead{}, ErrPushNotFound
	}
	if err != nil {
		return PushHead{}, fmt.Errorf("sessioningest: push head: %w", err)
	}
	if writer != WriterPush {
		return PushHead{}, &PushConflictError{Reason: "session is written by the session feed"}
	}
	if h.Owner != CanonicalOwner(owner) {
		return PushHead{}, ErrPushForbidden
	}
	if head != nil {
		h.Revision = *head
	}
	return h, nil
}

// PushInput is one authenticated, size-capped PUT.
type PushInput struct {
	Owner     string
	SessionID string
	Format    string
	// Create is If-None-Match: *.
	Create bool
	// IfMatch holds the strong entity tags of If-Match; "*" matches any head.
	IfMatch []string
	Body    []byte
}

// PushOutcome is an accepted push.
type PushOutcome struct {
	Revision string
	Created  bool
	// AlreadyApplied: the body already was the head (a retried update).
	AlreadyApplied bool
}

// Revision computes the token Accept would assign to in.Body.
func (p *PushStore) Revision(owner, sessionID, format string, body []byte) string {
	return revisionToken(p.cfg.RevisionKey, CanonicalOwner(owner), sessionID, format, body)
}

// evaluatePrecondition applies RFC 9110 §13.1.1/§13.1.2 to head. ok means the
// push may proceed; applied means it is a no-op that already happened.
func evaluatePrecondition(in PushInput, head, token string) (applied bool, err error) {
	if in.Create {
		if head == "" {
			return false, nil
		}
		return false, &PushPreconditionError{Current: head, BodyMatchesCurrent: token == head}
	}
	if head == "" {
		return false, &PushPreconditionError{}
	}
	for _, tag := range in.IfMatch {
		if tag == "*" || tag == head {
			return token == head, nil
		}
	}
	if token == head {
		return true, nil
	}
	return false, &PushPreconditionError{Current: head}
}

// Accept stores one push: precondition, quota pre-check, decode, then one
// transaction that rechecks the precondition, stores the bytes, records,
// and revision, advances the head, and queues the session. Nothing is
// acknowledged before that transaction commits.
func (p *PushStore) Accept(ctx context.Context, in PushInput) (PushOutcome, error) {
	owner := CanonicalOwner(in.Owner)
	if owner == "" {
		return PushOutcome{}, ErrPushForbidden
	}
	decoder, ok := p.cfg.Formats.Lookup(in.Format)
	if !ok {
		return PushOutcome{}, &PushConflictError{Reason: "unsupported format " + in.Format}
	}
	if int64(len(in.Body)) > p.cfg.MaxDocumentBytes {
		return PushOutcome{}, fmt.Errorf("sessioningest: document exceeds %d bytes", p.cfg.MaxDocumentBytes)
	}
	token := revisionToken(p.cfg.RevisionKey, owner, in.SessionID, in.Format, in.Body)

	head, err := p.Head(ctx, owner, in.SessionID)
	switch {
	case errors.Is(err, ErrPushNotFound):
	case err != nil:
		return PushOutcome{}, err
	case head.Format != in.Format:
		return PushOutcome{}, &PushConflictError{Reason: fmt.Sprintf("session is bound to format %s", head.Format)}
	}
	applied, err := evaluatePrecondition(in, head.Revision, token)
	if err != nil || applied {
		return PushOutcome{Revision: token, AlreadyApplied: applied}, err
	}
	// Reinstating a retained revision (A -> B -> A) adds no storage, so an
	// owner at a limit may still do it.
	var retained bool
	if err := p.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM session_push_revisions
		WHERE session_id=$1 AND revision=$2 AND pruned_at IS NULL)`, in.SessionID, token).Scan(&retained); err != nil {
		return PushOutcome{}, fmt.Errorf("sessioningest: probe retained revision: %w", err)
	}
	if !retained {
		if err := p.checkQuota(ctx, owner); err != nil {
			return PushOutcome{}, err
		}
	}

	doc, records, err := sessiondecode.DecodeAll(decoder, in.Body)
	if err != nil {
		return PushOutcome{}, &PushDecodeError{Err: err}
	}
	recordBlob, err := encodeStoredRecords(records)
	if err != nil {
		return PushOutcome{}, err
	}
	return p.commit(ctx, in, owner, token, doc, recordBlob)
}

// storedRecord is one line of a materialization blob: the model-facing record
// plus the id fidelity kept beside it.
type storedRecord struct {
	Fidelity sessiondecode.Fidelity `json:"fidelity"`
	Record   json.RawMessage        `json:"record"`
}

func encodeStoredRecords(records []sessiondecode.Record) ([]byte, error) {
	var out bytes.Buffer
	for _, r := range records {
		line, err := json.Marshal(r)
		if err != nil {
			return nil, fmt.Errorf("sessioningest: encode record %q: %w", r.ID, err)
		}
		wrapped, err := json.Marshal(storedRecord{Fidelity: r.Fidelity, Record: line})
		if err != nil {
			return nil, err
		}
		out.Write(wrapped)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// checkQuota is the cheap admission check before decoding: an owner already
// at a limit is refused without reading further. The transaction enforces
// the exact limit once it knows how many new bytes deduplication left.
func (p *PushStore) checkQuota(ctx context.Context, owner string) error {
	var used, revisions int64
	err := p.pool.QueryRow(ctx, `SELECT stored_bytes, revisions FROM session_push_owner_usage WHERE owner_canonical=$1`, owner).Scan(&used, &revisions)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("sessioningest: load push usage: %w", err)
	}
	if used >= p.cfg.OwnerQuotaBytes {
		return &PushQuotaError{Quota: "bytes", Limit: p.cfg.OwnerQuotaBytes, Used: used}
	}
	if revisions >= p.cfg.OwnerQuotaRevisions {
		return &PushQuotaError{Quota: "revisions", Limit: p.cfg.OwnerQuotaRevisions, Used: revisions}
	}
	return nil
}

// quotaError reports whether what a push adds takes usage past a limit. A
// push that adds nothing on a dimension is never refused on it.
func (p *PushStore) quotaError(bytes, addBytes, revisions, addRevisions int64) error {
	if addBytes > 0 && bytes+addBytes > p.cfg.OwnerQuotaBytes {
		return &PushQuotaError{Quota: "bytes", Limit: p.cfg.OwnerQuotaBytes, Used: bytes}
	}
	if addRevisions > 0 && revisions+addRevisions > p.cfg.OwnerQuotaRevisions {
		return &PushQuotaError{Quota: "revisions", Limit: p.cfg.OwnerQuotaRevisions, Used: revisions}
	}
	return nil
}

func (p *PushStore) commit(ctx context.Context, in PushInput, owner, token string, doc sessiondecode.Document, recordBlob []byte) (PushOutcome, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return PushOutcome{}, fmt.Errorf("sessioningest: begin push: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Owner lock first: the shared candidate upsert takes it too, and every
	// writer acquires owner lock -> binding -> candidate in that order.
	lockedOwners := map[string]struct{}{owner: {}}
	if err := lockOwnerPrivacyTx(ctx, tx, owner); err != nil {
		return PushOutcome{}, err
	}
	binding, err := bindSessionTx(ctx, tx, in.SessionID, owner, WriterPush, in.Format)
	if err != nil {
		return PushOutcome{}, pushBindingError(err)
	}
	applied, err := evaluatePrecondition(in, binding.Head, token)
	if err != nil || applied {
		return PushOutcome{Revision: token, AlreadyApplied: applied}, err
	}

	var used, revisions int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO session_push_owner_usage (owner_canonical) VALUES ($1)
		ON CONFLICT (owner_canonical) DO UPDATE SET updated_at = now()
		RETURNING stored_bytes, revisions`, owner).Scan(&used, &revisions); err != nil {
		return PushOutcome{}, fmt.Errorf("sessioningest: lock push usage: %w", err)
	}

	var (
		addBytes     int64
		addRevisions int64
		pruned       *time.Time
		bodyBlob     *int64
	)
	err = tx.QueryRow(ctx, `SELECT pruned_at, body_blob_id FROM session_push_revisions
		WHERE session_id=$1 AND revision=$2 FOR UPDATE`, in.SessionID, token).Scan(&pruned, &bodyBlob)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return PushOutcome{}, fmt.Errorf("sessioningest: load push revision: %w", err)
	}
	if !exists || pruned != nil {
		blobID, added, err := storeBlobTx(ctx, tx, in.SessionID, in.Body)
		if err != nil {
			return PushOutcome{}, err
		}
		addBytes += added
		addRevisions++
		if _, err := tx.Exec(ctx, `
			INSERT INTO session_push_revisions
			    (session_id, revision, owner_canonical, format, source_version, byte_size, body_blob_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (session_id, revision) DO UPDATE SET
			    body_blob_id = EXCLUDED.body_blob_id, pruned_at = NULL,
			    superseded_at = NULL, accepted_at = now()`,
			in.SessionID, token, owner, in.Format, doc.SourceVersion, len(in.Body), blobID); err != nil {
			return PushOutcome{}, fmt.Errorf("sessioningest: store push revision: %w", err)
		}
	} else if _, err := tx.Exec(ctx, `UPDATE session_push_revisions
		SET superseded_at = NULL, accepted_at = now() WHERE session_id=$1 AND revision=$2`, in.SessionID, token); err != nil {
		return PushOutcome{}, fmt.Errorf("sessioningest: reinstate push revision: %w", err)
	}
	added, err := materializeTx(ctx, tx, in.SessionID, token, doc, recordBlob)
	if err != nil {
		return PushOutcome{}, err
	}
	addBytes += added
	if err := p.quotaError(used, addBytes, revisions, addRevisions); err != nil {
		return PushOutcome{}, err
	}

	if binding.Head != "" && binding.Head != token {
		if _, err := tx.Exec(ctx, `UPDATE session_push_revisions SET superseded_at = now()
			WHERE session_id=$1 AND revision=$2 AND superseded_at IS NULL`, in.SessionID, binding.Head); err != nil {
			return PushOutcome{}, fmt.Errorf("sessioningest: supersede push revision: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE session_ingest_bindings SET head_revision=$2, updated_at=now()
		WHERE session_id=$1`, in.SessionID, token); err != nil {
		return PushOutcome{}, fmt.Errorf("sessioningest: advance push head: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE session_push_owner_usage
		SET stored_bytes = stored_bytes + $2, revisions = revisions + $3, updated_at = now()
		WHERE owner_canonical=$1`, owner, addBytes, addRevisions); err != nil {
		return PushOutcome{}, fmt.Errorf("sessioningest: account push usage: %w", err)
	}

	client := doc.Harness
	if !binding.Inserted {
		// The candidate key includes the client; keep the one the session
		// was first queued under.
		if err := tx.QueryRow(ctx, `SELECT client FROM session_ingest_candidates
			WHERE session_id=$1 AND owner=$2`, in.SessionID, owner).Scan(&client); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return PushOutcome{}, fmt.Errorf("sessioningest: load push client: %w", err)
		}
	}
	if client == "" {
		client = in.Format
	}
	now := time.Now()
	change := SessionChange{
		ChangeSeq: now.UnixMicro(), SessionID: in.SessionID,
		Owner: owner, OwnerKind: "human", Client: client, Role: "primary",
		Revision: token, RecordCount: doc.RecordCount, UserTurnCount: doc.UserTurnCount,
		ContentBytes: doc.ContentBytes, SourceStartedAt: doc.StartedAt, SourceUpdatedAt: doc.UpdatedAt,
		UpdatedAt: now, Complete: true,
	}
	decision := ChangePolicy{
		Admitted:        p.cfg.Policy.AdmitChange(in.SessionID, change.OwnerKind, change.Role, client, "", owner, false),
		VisibilityScope: p.cfg.Policy.VisibilityScope(owner),
	}
	if _, err := upsertCandidateTx(ctx, tx, change, decision, WriterPush, in.Format, lockedOwners); err != nil {
		return PushOutcome{}, pushBindingError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PushOutcome{}, fmt.Errorf("sessioningest: commit push: %w", err)
	}
	return PushOutcome{Revision: token, Created: binding.Head == ""}, nil
}

func pushBindingError(err error) error {
	switch {
	case errors.Is(err, ErrSessionOwnerMismatch):
		return ErrPushForbidden
	case errors.Is(err, ErrSessionWriterConflict):
		return &PushConflictError{Reason: "session is written by the session feed"}
	case errors.Is(err, ErrSessionFormatConflict):
		return &PushConflictError{Reason: "session is bound to another format"}
	}
	return err
}

// storeBlobTx splits data into content-defined chunks, inserts the ones the
// session does not hold yet, and records the blob. It returns the bytes the
// new chunks added.
func storeBlobTx(ctx context.Context, tx pgx.Tx, sessionID string, data []byte) (int64, int64, error) {
	chunks := cdcChunks(data)
	digests := make([][]byte, len(chunks))
	batch := &pgx.Batch{}
	for i, c := range chunks {
		sum := sha256.Sum256(c)
		digests[i] = sum[:]
		batch.Queue(`INSERT INTO session_push_chunks (session_id, digest, data) VALUES ($1,$2,$3)
			ON CONFLICT (session_id, digest) DO NOTHING`, sessionID, digests[i], c)
	}
	var added int64
	if batch.Len() > 0 {
		results := tx.SendBatch(ctx, batch)
		for i := range chunks {
			tag, err := results.Exec()
			if err != nil {
				_ = results.Close()
				return 0, 0, fmt.Errorf("sessioningest: store push chunk: %w", err)
			}
			if tag.RowsAffected() == 1 {
				added += int64(len(chunks[i]))
			}
		}
		if err := results.Close(); err != nil {
			return 0, 0, fmt.Errorf("sessioningest: store push chunks: %w", err)
		}
	}
	whole := sha256.Sum256(data)
	var blobID int64
	if err := tx.QueryRow(ctx, `INSERT INTO session_push_blobs (session_id, byte_size, sha256)
		VALUES ($1,$2,$3) RETURNING id`, sessionID, len(data), whole[:]).Scan(&blobID); err != nil {
		return 0, 0, fmt.Errorf("sessioningest: store push blob: %w", err)
	}
	rows := make([][]any, len(chunks))
	for i := range chunks {
		rows[i] = []any{blobID, i, sessionID, digests[i]}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"session_push_blob_chunks"},
		[]string{"blob_id", "ordinal", "session_id", "digest"}, pgx.CopyFromRows(rows)); err != nil {
		return 0, 0, fmt.Errorf("sessioningest: store push blob chunks: %w", err)
	}
	return blobID, added, nil
}

// materializeTx stores the records doc's decoder produced for revision,
// unless that (revision, decoder version) already has a materialization:
// materializations are immutable.
func materializeTx(ctx context.Context, tx pgx.Tx, sessionID, revision string, doc sessiondecode.Document, recordBlob []byte) (int64, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM session_push_materializations
		WHERE session_id=$1 AND revision=$2 AND decoder_version=$3)`, sessionID, revision, doc.DecoderVersion).Scan(&exists); err != nil {
		return 0, fmt.Errorf("sessioningest: probe materialization: %w", err)
	}
	if exists {
		return 0, nil
	}
	blobID, added, err := storeBlobTx(ctx, tx, sessionID, recordBlob)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO session_push_materializations
		    (session_id, revision, decoder_version, format, source_version, harness,
		     record_count, user_turn_count, content_bytes, warning_records,
		     started_at, source_updated_at, records_blob_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		sessionID, revision, doc.DecoderVersion, doc.Format, doc.SourceVersion, doc.Harness,
		doc.RecordCount, doc.UserTurnCount, doc.ContentBytes, doc.WarningRecords,
		doc.StartedAt, doc.UpdatedAt, blobID); err != nil {
		return 0, fmt.Errorf("sessioningest: store materialization: %w", err)
	}
	return added, nil
}

// readBlob reassembles a stored blob and verifies its digest.
func readBlob(ctx context.Context, q pgxQuerier, blobID int64) ([]byte, error) {
	var size int64
	var want []byte
	if err := q.QueryRow(ctx, `SELECT byte_size, sha256 FROM session_push_blobs WHERE id=$1`, blobID).Scan(&size, &want); err != nil {
		return nil, fmt.Errorf("sessioningest: load push blob %d: %w", blobID, err)
	}
	rows, err := q.Query(ctx, `SELECT c.data FROM session_push_blob_chunks b
		JOIN session_push_chunks c ON c.session_id=b.session_id AND c.digest=b.digest
		WHERE b.blob_id=$1 ORDER BY b.ordinal`, blobID)
	if err != nil {
		return nil, fmt.Errorf("sessioningest: read push blob %d: %w", blobID, err)
	}
	defer rows.Close()
	out := make([]byte, 0, size)
	for rows.Next() {
		var chunk []byte
		if err := rows.Scan(&chunk); err != nil {
			return nil, fmt.Errorf("sessioningest: scan push chunk: %w", err)
		}
		out = append(out, chunk...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessioningest: read push blob %d: %w", blobID, err)
	}
	if got := sha256.Sum256(out); !bytes.Equal(got[:], want) || int64(len(out)) != size {
		return nil, fmt.Errorf("sessioningest: push blob %d failed its digest check", blobID)
	}
	return out, nil
}

type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// HeadSize returns the head and its received size without reading the
// bytes.
func (p *PushStore) HeadSize(ctx context.Context, owner, sessionID string) (PushHead, int64, error) {
	head, err := p.Head(ctx, owner, sessionID)
	if err != nil {
		return PushHead{}, 0, err
	}
	if head.Revision == "" {
		return PushHead{}, 0, ErrPushNotFound
	}
	var size int64
	if err := p.pool.QueryRow(ctx, `SELECT byte_size FROM session_push_revisions
		WHERE session_id=$1 AND revision=$2`, sessionID, head.Revision).Scan(&size); err != nil {
		return PushHead{}, 0, fmt.Errorf("sessioningest: head size: %w", err)
	}
	return head, size, nil
}

// RevisionBody returns one stored revision's received bytes.
func (p *PushStore) RevisionBody(ctx context.Context, sessionID, revision string) ([]byte, error) {
	var blobID *int64
	err := p.pool.QueryRow(ctx, `SELECT body_blob_id FROM session_push_revisions
		WHERE session_id=$1 AND revision=$2`, sessionID, revision).Scan(&blobID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrPushNotFound
	case err != nil:
		return nil, fmt.Errorf("sessioningest: revision body: %w", err)
	case blobID == nil:
		return nil, ErrPushRevisionPruned
	}
	return readBlob(ctx, p.pool, *blobID)
}

// HeadBody returns the head revision's received bytes.
func (p *PushStore) HeadBody(ctx context.Context, owner, sessionID string) (PushHead, []byte, error) {
	head, _, err := p.HeadSize(ctx, owner, sessionID)
	if err != nil {
		return PushHead{}, nil, err
	}
	body, err := p.RevisionBody(ctx, sessionID, head.Revision)
	return head, body, err
}

// contentDigest is the published-ledger digest of stored bytes.
func contentDigest(sum []byte) string {
	return "sha256:" + hex.EncodeToString(sum)
}

// pushedRevision is one stored revision's materialization under the current
// decoder version.
type pushedRevision struct {
	Owner          string
	Format         string
	DecoderVersion string
	BodyBlob       int64
	BodySHA256     []byte
	BodyBytes      int64
	RecordsBlob    *int64
	StartedAt      time.Time
	UpdatedAt      time.Time
	Warnings       int
}

func (p *PushStore) lookupRevision(ctx context.Context, q pgxQuerier, sessionID, revision string) (pushedRevision, sessiondecode.Decoder, error) {
	var r pushedRevision
	var startedAt, updatedAt *time.Time
	var warnings *int
	var bodyBlob *int64
	err := q.QueryRow(ctx, `
		SELECT r.owner_canonical, r.format, r.body_blob_id, b.sha256, r.byte_size
		  FROM session_push_revisions r
		  LEFT JOIN session_push_blobs b ON b.id = r.body_blob_id
		 WHERE r.session_id=$1 AND r.revision=$2`, sessionID, revision).Scan(&r.Owner, &r.Format, &bodyBlob, &r.BodySHA256, &r.BodyBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return pushedRevision{}, nil, ErrPushNotFound
	}
	if err != nil {
		return pushedRevision{}, nil, fmt.Errorf("sessioningest: load pushed revision: %w", err)
	}
	if bodyBlob == nil {
		return pushedRevision{}, nil, ErrPushRevisionPruned
	}
	r.BodyBlob = *bodyBlob
	decoder, ok := p.cfg.Formats.Lookup(r.Format)
	if !ok {
		return pushedRevision{}, nil, fmt.Errorf("%w: no decoder for stored format %s", sessiondecode.ErrStructural, r.Format)
	}
	r.DecoderVersion = decoder.Version()
	err = q.QueryRow(ctx, `
		SELECT records_blob_id, started_at, source_updated_at, warning_records
		  FROM session_push_materializations
		 WHERE session_id=$1 AND revision=$2 AND decoder_version=$3`,
		sessionID, revision, r.DecoderVersion).Scan(&r.RecordsBlob, &startedAt, &updatedAt, &warnings)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return pushedRevision{}, nil, fmt.Errorf("sessioningest: load materialization: %w", err)
	}
	if startedAt != nil {
		r.StartedAt, r.UpdatedAt, r.Warnings = *startedAt, *updatedAt, *warnings
	}
	return r, decoder, nil
}

// ErrPushRevisionPruned: the retention window removed a superseded
// revision's bytes and records.
var ErrPushRevisionPruned = errors.New("sessioningest: pushed revision was pruned")

// rematerialize decodes a stored revision with the current decoder version
// and stores the result as a new, immutable materialization. It runs when a
// decoder upgrade meets a revision accepted under an older version.
func (p *PushStore) rematerialize(ctx context.Context, sessionID, revision string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sessioningest: begin rematerialize: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize with pushes to this session.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM session_ingest_bindings WHERE session_id=$1 FOR UPDATE`, sessionID); err != nil {
		return fmt.Errorf("sessioningest: lock session for rematerialize: %w", err)
	}
	rev, decoder, err := p.lookupRevision(ctx, tx, sessionID, revision)
	if err != nil {
		return err
	}
	if rev.RecordsBlob != nil {
		return nil
	}
	body, err := readBlob(ctx, tx, rev.BodyBlob)
	if err != nil {
		return err
	}
	doc, records, err := sessiondecode.DecodeAll(decoder, body)
	if err != nil {
		return err
	}
	recordBlob, err := encodeStoredRecords(records)
	if err != nil {
		return err
	}
	added, err := materializeTx(ctx, tx, sessionID, revision, doc, recordBlob)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE session_push_owner_usage
		SET stored_bytes = stored_bytes + $2, updated_at = now() WHERE owner_canonical=$1`, rev.Owner, added); err != nil {
		return fmt.Errorf("sessioningest: account rematerialization: %w", err)
	}
	return tx.Commit(ctx)
}

// loadDocument is the worker's document source for a pushed session: the
// records of the candidate's revision under the current decoder version.
func (p *PushStore) loadDocument(ctx context.Context, c Candidate) (*sessionDocument, string, int64, error) {
	rev, _, err := p.lookupRevision(ctx, p.pool, c.SessionID, c.Revision)
	if err != nil {
		return nil, "", 0, err
	}
	if rev.RecordsBlob == nil {
		if err := p.rematerialize(ctx, c.SessionID, c.Revision); err != nil {
			return nil, "", 0, err
		}
		if rev, _, err = p.lookupRevision(ctx, p.pool, c.SessionID, c.Revision); err != nil {
			return nil, "", 0, err
		}
		if rev.RecordsBlob == nil {
			return nil, "", 0, fmt.Errorf("sessioningest: rematerialization of %s left no records", c.SessionID)
		}
	}
	blob, err := readBlob(ctx, p.pool, *rev.RecordsBlob)
	if err != nil {
		return nil, "", 0, err
	}
	doc := &sessionDocument{
		Owner: rev.Owner, StartedAt: rev.StartedAt, UpdatedAt: rev.UpdatedAt,
		WarningRecords: rev.Warnings, DecoderVersion: rev.DecoderVersion,
	}
	builder := transcriptBuilder{doc: doc}
	if err := forEachStoredRecord(blob, func(rec json.RawMessage, at time.Time, _ sessiondecode.Fidelity) error {
		builder.addLine(rec, at)
		return nil
	}); err != nil {
		return nil, "", 0, err
	}
	return doc, contentDigest(rev.BodySHA256), rev.BodyBytes, nil
}

// forEachStoredRecord walks a materialization blob in record order.
func forEachStoredRecord(blob []byte, fn func(rec json.RawMessage, at time.Time, fidelity sessiondecode.Fidelity) error) error {
	for len(blob) > 0 {
		line := blob
		if i := bytes.IndexByte(blob, '\n'); i >= 0 {
			line, blob = blob[:i], blob[i+1:]
		} else {
			blob = nil
		}
		if len(line) == 0 {
			continue
		}
		var stored storedRecord
		if err := json.Unmarshal(line, &stored); err != nil {
			return fmt.Errorf("sessioningest: stored record: %w", err)
		}
		var head struct {
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(stored.Record, &head); err != nil {
			return fmt.Errorf("sessioningest: stored record: %w", err)
		}
		at, err := time.Parse(time.RFC3339, head.Timestamp)
		if err != nil {
			return fmt.Errorf("sessioningest: stored record timestamp: %w", err)
		}
		if err := fn(stored.Record, at, stored.Fidelity); err != nil {
			return err
		}
	}
	return nil
}
