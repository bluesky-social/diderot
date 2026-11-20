package sessioningest

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Writer kinds bound to a session on its first write.
const (
	WriterFeed = "feed"
	WriterPush = "push"
)

// Binding errors from the shared candidate upsert.
var (
	// ErrSessionWriterConflict: the session was first written by the other
	// writer kind (feed vs push). A session that came from the feed can never
	// be updated by an upload, and the reverse.
	ErrSessionWriterConflict = errors.New("sessioningest: session is bound to another writer")
	// ErrSessionOwnerMismatch: a push names a session another owner holds.
	ErrSessionOwnerMismatch = errors.New("sessioningest: session is bound to another owner")
	// ErrSessionFormatConflict: a push names a different format than the one
	// the session was first accepted in.
	ErrSessionFormatConflict = errors.New("sessioningest: session is bound to another format")
	// ErrInvalidVisibilityScope: a change's decided visibility scope is
	// neither internal nor its own owner's scope. Like the binding errors
	// above, this is deterministic and permanent for the change that
	// produced it: replaying it decides the same invalid scope every time.
	ErrInvalidVisibilityScope = errors.New("sessioningest: invalid visibility scope for owner")
	// ErrMalformedFeedChange: the change's own data fails a check Go makes
	// before the transaction ever touches it -- see validateSessionChange.
	// Like the binding errors above, this depends only on the change's own
	// data, so replaying the identical row fails the identical way forever.
	ErrMalformedFeedChange = errors.New("sessioningest: change failed validation")
)

// skippablePerChangeError reports whether err is a deterministic, permanent
// refusal of exactly the one change that produced it -- never a transient or
// unexpected database failure. UpsertChangesScopedAt's loop skips a change
// that fails this way (logging and counting it) instead of discarding the
// rest of the page and stalling the cursor behind it; every other error
// still fails the whole page, so the drain retries it.
func skippablePerChangeError(err error) bool {
	return errors.Is(err, ErrSessionWriterConflict) ||
		errors.Is(err, ErrSessionFormatConflict) ||
		errors.Is(err, ErrInvalidVisibilityScope) ||
		errors.Is(err, ErrMalformedFeedChange)
}

// skipReason returns a short, stable label for a skipped change's error, for
// the skip log line and the diderot_sessioningest_feed_changes_skipped_total
// "reason" label. Keep this in sync with skippablePerChangeError's cases.
func skipReason(err error) string {
	switch {
	case errors.Is(err, ErrSessionWriterConflict):
		return "writer_conflict"
	case errors.Is(err, ErrSessionFormatConflict):
		return "format_conflict"
	case errors.Is(err, ErrInvalidVisibilityScope):
		return "invalid_scope"
	case errors.Is(err, ErrMalformedFeedChange):
		return "malformed_row"
	default:
		return "other"
	}
}

// sessionBinding is a session_ingest_bindings row after bindSessionTx.
type sessionBinding struct {
	Owner, Writer, Format string
	// Head is the accepted push head, "" for the feed or before a first push.
	Head string
	// Inserted reports that this call created the binding.
	Inserted bool
}

// bindSessionTx binds sessionID to its owner, writer kind, and format on the
// first write and locks the binding for the rest of tx. Later writes must
// name the same writer and format. Push must also name the same owner. The
// feed is the authority for the owner of a session it writes, so a feed
// owner change moves the binding, as the candidate row always followed the
// feed's owner.
//
// A push that would create a binding for an id the feed wrote before
// bindings existed is a writer conflict too.
func bindSessionTx(ctx context.Context, tx pgx.Tx, sessionID, owner, writer, format string) (sessionBinding, error) {
	var b sessionBinding
	var head *string
	err := tx.QueryRow(ctx, `
		INSERT INTO session_ingest_bindings (session_id, owner_canonical, writer, format)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (session_id) DO UPDATE SET updated_at = now()
		RETURNING owner_canonical, writer, format, head_revision, (xmax = 0)`,
		sessionID, owner, writer, format).Scan(&b.Owner, &b.Writer, &b.Format, &head, &b.Inserted)
	if err != nil {
		return sessionBinding{}, fmt.Errorf("sessioningest: bind session %s: %w", sessionID, err)
	}
	if head != nil {
		b.Head = *head
	}
	if b.Writer != writer {
		return sessionBinding{}, ErrSessionWriterConflict
	}
	if b.Format != format {
		return sessionBinding{}, ErrSessionFormatConflict
	}
	if b.Inserted && writer == WriterPush {
		var legacy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM session_ingest_candidates WHERE session_id = $1)`,
			sessionID).Scan(&legacy); err != nil {
			return sessionBinding{}, fmt.Errorf("sessioningest: probe legacy session %s: %w", sessionID, err)
		}
		if legacy {
			return sessionBinding{}, ErrSessionWriterConflict
		}
	}
	if b.Owner != owner {
		if writer == WriterPush {
			return sessionBinding{}, ErrSessionOwnerMismatch
		}
		if _, err := tx.Exec(ctx, `UPDATE session_ingest_bindings SET owner_canonical = $2 WHERE session_id = $1`,
			sessionID, owner); err != nil {
			return sessionBinding{}, fmt.Errorf("sessioningest: rebind session %s owner: %w", sessionID, err)
		}
		b.Owner = owner
	}
	return b, nil
}
