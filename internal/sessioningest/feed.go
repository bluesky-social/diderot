package sessioningest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
)

// ErrRevisionTooLarge marks a session revision whose size exceeds the
// configured byte cap. It is a permanent index failure for that revision.
var ErrRevisionTooLarge = errors.New("session revision exceeds byte cap")

// ErrNoRecords marks a session revision with no records. There is nothing
// in it to summarize and a revision never changes, so it is a permanent
// index failure for that revision; a later revision of the session is
// indexed as usual.
var ErrNoRecords = errors.New("session revision has no records")

// SessionChange is one head advertised by a Feed's ordered change clock.
type SessionChange struct {
	ChangeSeq       int64
	SessionID       string
	Owner           string
	OwnerKind       string
	Client          string
	Role            string
	OriginApp       string
	Revision        string
	RewriteEpoch    int64
	RecordCount     int
	UserTurnCount   int
	ContentBytes    int64
	SourceStartedAt time.Time
	SourceUpdatedAt time.Time
	UpdatedAt       time.Time
	Complete        bool
	Deleted         bool
}

// maxIdentityFieldBytes bounds SessionID, Owner and Client: they key
// session_ingest_bindings (session_id alone) and session_ingest_candidates
// (owner, client, session_id together), so an oversized value blows the
// composite btree index's per-tuple limit (Postgres's default ~2704 bytes
// for an 8 KiB page) and the insert fails with a permanent 54000
// (program_limit_exceeded). The cap is far above any real email/client/
// session-UUID value, with ample headroom for the three fields together.
const maxIdentityFieldBytes = 512

// validateSessionChange reports whether ch's own data can ever be written:
// every field it feeds into session_ingest_bindings or
// session_ingest_candidates, checked against that column's type and
// constraints, independent of the database. A failure here depends only on
// ch's own data, so replaying the identical change fails identically
// forever -- exactly the property UpsertChangesScopedAt needs to skip it
// instead of failing the whole page. This runs in Go, before the
// transaction touches the row at all, so a malformed change never reaches
// Postgres and never needs special per-row transaction handling.
//
// Swept field by field against the columns each one is written to. Every
// field here is a Go value type (string or number, never a pointer), so its
// zero value ("" or 0) already satisfies its column's NOT NULL -- an absent
// field is never what makes Postgres permanently reject a row, and a feed
// session_id or client genuinely can be an empty string without Postgres
// objecting. The owner is the one required field: a session nobody owns
// would be indexed for everyone, with no owner to restrict it and no
// principal for the policy to match. A deletion needs no owner.
// SourceStartedAt/SourceUpdatedAt are the one pair where Go's zero value
// (time.Time{}) IS meaningful -- see below.
//
//   - SessionID, Owner, Client: session_id/owner/client, text NOT NULL,
//     part of the primary keys above -- valid UTF-8, no NUL byte, and
//     within maxIdentityFieldBytes.
//   - OwnerKind, Role, OriginApp, Revision: owner_kind/role/origin_app/
//     revision, text NOT NULL DEFAULT '' and not part of any key or index --
//     valid UTF-8 and no NUL byte (Postgres text rejects both regardless of
//     length); no length cap, since none is indexed.
//   - RecordCount, UserTurnCount: record_count/user_turn_count, integer
//     (int4) -- must fit in int32; ch's Go int can exceed it on a 64-bit
//     build, and pgx refuses to encode an out-of-range int4 arg.
//   - RewriteEpoch, ChangeSeq, ContentBytes: rewrite_epoch/change_seq/
//     content_bytes, bigint (int8) -- already int64 in Go, so always in
//     range; no check needed.
//   - SourceStartedAt, SourceUpdatedAt: source_started_at/
//     source_updated_at, timestamptz pair under the
//     session_ingest_candidates_source_time_pair CHECK -- both zero or both
//     set, and if set, updated not before started.
//   - UpdatedAt, Complete, Deleted: updated_at/complete/deleted carry no
//     column constraint beyond their Go types already guarantee -- no check.
func validateSessionChange(ch SessionChange) error {
	type textField struct {
		name    string
		value   string
		bounded bool
	}
	fields := []textField{
		{"session_id", ch.SessionID, true},
		{"owner", ch.Owner, true},
		{"client", ch.Client, true},
		{"owner_kind", ch.OwnerKind, false},
		{"role", ch.Role, false},
		{"origin_app", ch.OriginApp, false},
		{"revision", ch.Revision, false},
	}
	for _, f := range fields {
		if !utf8.ValidString(f.value) {
			return fmt.Errorf("%w: %s is not valid UTF-8", ErrMalformedFeedChange, f.name)
		}
		if strings.IndexByte(f.value, 0) >= 0 {
			return fmt.Errorf("%w: %s contains a NUL byte", ErrMalformedFeedChange, f.name)
		}
		if f.bounded && len(f.value) > maxIdentityFieldBytes {
			return fmt.Errorf("%w: %s is %d bytes, over the %d-byte cap", ErrMalformedFeedChange, f.name, len(f.value), maxIdentityFieldBytes)
		}
	}
	if !ch.Deleted && CanonicalOwner(ch.Owner) == "" {
		return fmt.Errorf("%w: owner is empty", ErrMalformedFeedChange)
	}
	if ch.RecordCount < math.MinInt32 || ch.RecordCount > math.MaxInt32 {
		return fmt.Errorf("%w: record_count %d is out of int4 range", ErrMalformedFeedChange, ch.RecordCount)
	}
	if ch.UserTurnCount < math.MinInt32 || ch.UserTurnCount > math.MaxInt32 {
		return fmt.Errorf("%w: user_turn_count %d is out of int4 range", ErrMalformedFeedChange, ch.UserTurnCount)
	}
	startZero, updatedZero := ch.SourceStartedAt.IsZero(), ch.SourceUpdatedAt.IsZero()
	if startZero != updatedZero {
		return fmt.Errorf("%w: source_started_at and source_updated_at must both be set or both empty", ErrMalformedFeedChange)
	}
	if !startZero && ch.SourceUpdatedAt.Before(ch.SourceStartedAt) {
		return fmt.Errorf("%w: source_updated_at is before source_started_at", ErrMalformedFeedChange)
	}
	return nil
}

// ChangesPage is one ordered page of a Feed. NextCursor is the explicit
// resume position; the worker never derives one locally.
type ChangesPage struct {
	Changes    []SessionChange
	NextCursor int64
}

// Feed is a change-feed + revision-fetch surface the worker polls for
// sessions. Revisions are Bsky Session v1 documents
// (sessiondecode.FormatBsky). ClientFeed is the production implementation;
// tests stub it. Pushed sessions arrive without a Feed.
type Feed interface {
	ListSessionChanges(ctx context.Context, after int64, limit int) (*ChangesPage, error)
	// GetSessionRevisionBytes fetches a revision bounded to a specific
	// post-decompression cap. Exceeding it returns ErrRevisionTooLarge.
	GetSessionRevisionBytes(ctx context.Context, sessionID, revision string, maxRevisionBytes int64) ([]byte, error)
}

// ClientFeed adapts the session feed connector's client, which reads a feed's
// commit-ordered native head feed.
type ClientFeed struct {
	Client *sessionfeed.Client
}

// NewClientFeed wraps a configured session feed client.
func NewClientFeed(client *sessionfeed.Client) *ClientFeed {
	return &ClientFeed{Client: client}
}

func (f *ClientFeed) ListSessionChanges(ctx context.Context, after int64, limit int) (*ChangesPage, error) {
	page, err := f.Client.ListSessionChanges(ctx, after, limit)
	if err != nil {
		return nil, err
	}
	out := &ChangesPage{NextCursor: page.NextCursor, Changes: make([]SessionChange, len(page.Changes))}
	for i, c := range page.Changes {
		out.Changes[i] = SessionChange{
			ChangeSeq: c.ChangeSeq, SessionID: c.SessionID,
			Owner: c.Owner, OwnerKind: c.OwnerKind, Client: c.Client, Role: c.Role, OriginApp: c.OriginApp,
			Revision: c.Revision, RewriteEpoch: c.RewriteEpoch,
			RecordCount: c.RecordCount, UserTurnCount: c.UserTurnCount, ContentBytes: c.ContentBytes,
			SourceStartedAt: c.SourceStartedAt, SourceUpdatedAt: c.SourceUpdatedAt, UpdatedAt: c.UpdatedAt,
			Complete: c.Complete, Deleted: c.Deleted,
		}
	}
	return out, nil
}

func (f *ClientFeed) GetSessionRevisionBytes(ctx context.Context, sessionID, revision string, maxRevisionBytes int64) ([]byte, error) {
	body, err := f.Client.GetSessionRevisionBytes(ctx, sessionID, revision, maxRevisionBytes)
	if errors.Is(err, sessionfeed.ErrRevisionTooLarge) {
		return nil, tooLargeError{err}
	}
	return body, err
}

// tooLargeError keeps the connector's message while matching
// ErrRevisionTooLarge.
type tooLargeError struct{ err error }

func (e tooLargeError) Error() string   { return e.err.Error() }
func (e tooLargeError) Unwrap() []error { return []error{ErrRevisionTooLarge, e.err} }
