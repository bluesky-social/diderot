package sessioningest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

// SupersededError reports that a citation names a superseded revision whose
// records the retention window pruned. Current is the session's head.
type SupersededError struct {
	SessionID string
	Revision  string
	Current   string
}

func (e *SupersededError) Error() string {
	return fmt.Sprintf("sessioningest: revision %s of session %s was superseded and pruned; current revision is %s",
		e.Revision, e.SessionID, e.Current)
}

// ExpandRequest names a record range of a pushed session as an event cites
// it.
type ExpandRequest struct {
	SessionID string
	Revision  string
	// DecoderVersion is the materialization the citation was built from; ""
	// resolves to the one the session's summary event names, else current.
	DecoderVersion string
	Start, End     int
	// AllowedScopes is the caller's visibility boundary. A session outside
	// it reads as not found.
	AllowedScopes []string
}

// ExpandRange returns the model-facing record lines in [Start, End) of a
// pushed session's cited revision, from diderot's own store. It fails with
// ErrPushNotFound when the session is not pushed, is outside the caller's
// scopes, or is excluded, and with *SupersededError when the cited revision
// was pruned.
func (p *PushStore) ExpandRange(ctx context.Context, req ExpandRequest) (string, error) {
	if req.Start < 0 || req.End <= req.Start {
		return "", fmt.Errorf("sessioningest: invalid record range [%d,%d)", req.Start, req.End)
	}
	var head *string
	var writer, scope string
	var excluded *time.Time
	var deleted bool
	err := p.pool.QueryRow(ctx, `
		SELECT b.writer, b.head_revision, c.visibility_scope, c.excluded_at, c.deleted
		  FROM session_ingest_bindings b
		  JOIN session_ingest_candidates c ON c.session_id = b.session_id AND c.owner = b.owner_canonical
		 WHERE b.session_id = $1`, req.SessionID).Scan(&writer, &head, &scope, &excluded, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPushNotFound
	}
	if err != nil {
		return "", fmt.Errorf("sessioningest: expand lookup: %w", err)
	}
	if writer != WriterPush || excluded != nil || deleted || !slices.Contains(req.AllowedScopes, scope) {
		return "", ErrPushNotFound
	}
	current := ""
	if head != nil {
		current = *head
	}

	version := req.DecoderVersion
	if version == "" {
		_ = p.pool.QueryRow(ctx, `SELECT COALESCE(raw->>'decoder_version', '') FROM events
			WHERE source = 'agent_session' AND external_id = $1 AND deleted = false AND raw->>'revision' = $2`,
			externalID(Identity{SessionID: req.SessionID}), req.Revision).Scan(&version)
	}
	rev, decoder, err := p.lookupRevision(ctx, p.pool, req.SessionID, req.Revision)
	switch {
	case errors.Is(err, ErrPushRevisionPruned):
		return "", &SupersededError{SessionID: req.SessionID, Revision: req.Revision, Current: current}
	case err != nil:
		return "", err
	}
	blobID := rev.RecordsBlob
	if version != "" && version != rev.DecoderVersion {
		// The citation was built from an earlier decoder version; its
		// materialization is immutable and still stored.
		var older int64
		err := p.pool.QueryRow(ctx, `SELECT records_blob_id FROM session_push_materializations
			WHERE session_id=$1 AND revision=$2 AND decoder_version=$3`, req.SessionID, req.Revision, version).Scan(&older)
		if err == nil {
			blobID = &older
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("sessioningest: load cited materialization: %w", err)
		}
	}
	if blobID == nil {
		if err := p.rematerialize(ctx, req.SessionID, req.Revision); err != nil {
			return "", err
		}
		if rev, _, err = p.lookupRevision(ctx, p.pool, req.SessionID, req.Revision); err != nil {
			return "", err
		}
		blobID = rev.RecordsBlob
		if blobID == nil {
			return "", fmt.Errorf("sessioningest: no %s materialization of %s", decoder.Version(), req.SessionID)
		}
	}
	blob, err := readBlob(ctx, p.pool, *blobID)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	index := 0
	err = forEachStoredRecord(blob, func(rec json.RawMessage, _ time.Time, _ sessiondecode.Fidelity) error {
		if index >= req.Start && index < req.End {
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.Write(rec)
		}
		index++
		return nil
	})
	if err != nil {
		return "", err
	}
	if req.End > index {
		return "", fmt.Errorf("sessioningest: record range [%d,%d) exceeds %d records", req.Start, req.End, index)
	}
	return out.String(), nil
}
