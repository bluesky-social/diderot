// Feed-revision validation + model-input encoding.
//
// A Feed serves sanitized Bsky Session v1 documents
// (internal/sessiondecode.FormatBsky). Before a fetched revision may be
// published, diderot enforces:
//
//   - the post-decompression byte total must be <= a configured cap;
//   - line 1 must be a well-formed `bsky.session.v1` envelope whose
//     session_id and revision match what the change feed advertised
//     (identity + revision pinning);
//   - every non-envelope line must be a well-formed record; unfamiliar
//     roles and content become explicit placeholders, while identity,
//     revision, privacy, and envelope consistency remain mandatory.
//
// Diderot never reads raw audit records — this is the ONLY content
// surface: Bsky Session v1 is the only application content contract for
// the viewer and the corpus, and raw records remain storage-only.
package sessioningest

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/bluesky-social/diderot/internal/sessiondecode"
	"github.com/bluesky-social/diderot/internal/sessionsummary"
)

// sessionDocument retains the bounded, allowlisted model input of one
// decoded revision plus the session metadata publication needs.
type sessionDocument struct {
	// Owner is the corpus event actor.
	Owner          string
	StartedAt      time.Time
	UpdatedAt      time.Time
	Transcript     []byte
	RecordCount    int
	RecordTimes    []time.Time
	WarningRecords int
	// Bsky is the envelope of a feed document; nil for pushed sessions.
	Bsky *sessiondecode.BskyEnvelope
	// DecoderVersion names the materialization a pushed session's records
	// came from; "" for feed documents, which are decoded on fetch.
	DecoderVersion string
}

// transcriptBuilder accumulates decoded records into the summarizer's
// newline-joined model input without retaining the records themselves.
type transcriptBuilder struct {
	doc *sessionDocument
}

func (b *transcriptBuilder) add(rec sessiondecode.Record) error {
	safeLine, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("session record %d safe encoding: %w", rec.Index, err)
	}
	b.addLine(safeLine, rec.Time)
	return nil
}

// addLine appends one already-encoded record line.
func (b *transcriptBuilder) addLine(safeLine []byte, at time.Time) {
	if len(safeLine) > sessionsummary.MaxLineBytes {
		safeLine, _ = sessionsummary.BoundLineSizes(safeLine)
	}
	if b.doc.RecordCount > 0 {
		b.doc.Transcript = append(b.doc.Transcript, '\n')
	}
	b.doc.Transcript = append(b.doc.Transcript, safeLine...)
	b.doc.RecordTimes = append(b.doc.RecordTimes, at)
	b.doc.RecordCount++
}

// parseSessionDocument decodes and validates a feed's Bsky Session v1 body
// against the canonical session UUID, the expected revision, and the
// post-decompression byte cap. It hard-fails on any contract violation
// rather than publishing a partial/mismatched session. The canonical
// session UUID alone is the identity key; owner and harness are echoed
// provenance on the envelope, never identity inputs.
func parseSessionDocument(data []byte, expected Identity, expectedRevision string, maxBytes int64) (*sessionDocument, error) {
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: bsky session revision %d bytes exceeds configured cap %d", ErrRevisionTooLarge, len(data), maxBytes)
	}
	out := &sessionDocument{}
	builder := transcriptBuilder{doc: out}
	decoded, err := sessiondecode.Bsky{}.DecodePinned(data, expected.SessionID, expectedRevision, builder.add)
	if err != nil {
		return nil, err
	}
	out.Owner = decoded.Bsky.Owner
	out.StartedAt = decoded.StartedAt
	out.UpdatedAt = decoded.UpdatedAt
	out.WarningRecords = decoded.WarningRecords
	out.Bsky = decoded.Bsky
	return out, nil
}
