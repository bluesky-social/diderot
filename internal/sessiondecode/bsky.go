package sessiondecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// FormatBsky is a session feed's sanitized Bsky Session v1 NDJSON
// (application/vnd.bsky.session+ndjson;version=1). Line 1 is the session
// envelope; every later line is one ordered record.
const FormatBsky = "bsky.session.v1"

// bskyDecoderVersion must change whenever the records emitted for the same
// bytes change.
const bskyDecoderVersion = "1"

// knownRoles is the set of record roles diderot understands. Unfamiliar
// roles receive a placeholder instead of sending their content to a model.
var knownRoles = map[string]bool{
	"user": true, "assistant": true, "tool": true, "system": true, "reasoning": true,
}

// BskyEnvelope is line 1 of a Bsky Session v1 document.
type BskyEnvelope struct {
	Type          string         `json:"type"`
	Schema        string         `json:"schema"`
	SessionID     string         `json:"session_id"`
	Source        string         `json:"source"`
	Owner         string         `json:"owner"`
	OwnerKind     string         `json:"owner_kind"`
	Role          string         `json:"session_role"`
	Lineage       *BskyLineage   `json:"lineage,omitempty"`
	OriginApp     string         `json:"origin_app"`
	Revision      string         `json:"revision"`
	RewriteEpoch  int64          `json:"rewrite_epoch"`
	RecordCount   int            `json:"record_count"`
	UserTurnCount int            `json:"user_turn_count"`
	ContentBytes  int64          `json:"content_bytes"`
	StartedAt     string         `json:"started_at"`
	UpdatedAt     string         `json:"updated_at"`
	Complete      bool           `json:"complete"`
	Accounting    map[string]any `json:"accounting"`
}

// sessionEnvelope and sessionRecord exist only so encoding/json type errors
// keep naming the types the feed's validator reported before decoders
// (encoding/json names the outermost struct, e.g. sessionRecord.tool_calls.0.id).
type (
	sessionEnvelope BskyEnvelope
	sessionRecord   Record
)

// BskyLineage links a secondary (subagent) session to its ancestor.
type BskyLineage struct {
	SessionID string `json:"session_id"`
	Kind      string `json:"kind"`
}

// Bsky decodes bsky.session.v1. The feed assigns each record a stable id, so
// ids are native.
type Bsky struct{}

func (Bsky) Format() string  { return FormatBsky }
func (Bsky) Version() string { return bskyDecoderVersion }

// Decode applies the full decoder contract, including id uniqueness and
// tool pairing.
func (b Bsky) Decode(data []byte, emit func(Record) error) (Document, error) {
	guard := newContractGuard()
	var emitErr error
	doc, err := b.DecodePinned(data, "", "", func(rec Record) error {
		if err := guard.check(rec); err != nil {
			return err
		}
		emitErr = emit(rec)
		return emitErr
	})
	if err != nil && emitErr == nil && !errors.Is(err, ErrStructural) {
		err = fmt.Errorf("%w: %w", ErrStructural, err)
	}
	return doc, err
}

// DecodePinned decodes and also requires the envelope's session_id and
// revision to equal the expected values when those are non-empty: the feed
// pins a fetched document to the head it advertised. It hard-fails on any
// contract violation rather than emitting a partial or mismatched session.
//
// It is the session feed's validator and deliberately keeps that path's
// historical acceptance: it does not reject duplicate ids or unpaired tool
// results, which the feed owns. Decode adds both checks.
func (Bsky) DecodePinned(data []byte, expectedSessionID, expectedRevision string, emit func(Record) error) (Document, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)

	var env sessionEnvelope
	first := true
	doc := Document{Format: FormatBsky, DecoderVersion: bskyDecoderVersion}
	idx := 0
	userTurns := 0
	var contentBytes int64
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if first {
			if err := json.Unmarshal(line, &env); err != nil {
				return Document{}, fmt.Errorf("bsky session envelope: %w", err)
			}
			first = false
			continue
		}
		var wire sessionRecord
		contentBytes += int64(len(line) + 1)
		if err := json.Unmarshal(line, &wire); err != nil {
			return Document{}, fmt.Errorf("bsky session record %d: %w", idx, err)
		}
		rec := Record(wire)
		if rec.Index != idx {
			return Document{}, fmt.Errorf("bsky session record %d: index %d is not contiguous", idx, rec.Index)
		}
		warning, err := NormalizeBskyRecord(&rec)
		if err != nil {
			return Document{}, fmt.Errorf("bsky session record %d: %w", idx, err)
		}
		if warning {
			doc.WarningRecords++
		}
		if rec.Role == "user" {
			userTurns++
		}
		if strings.TrimSpace(rec.ID) == "" {
			return Document{}, fmt.Errorf("bsky session record %d missing stable id", rec.Index)
		}
		if strings.TrimSpace(rec.Timestamp) == "" {
			return Document{}, fmt.Errorf("bsky session record %d missing timestamp", rec.Index)
		}
		recordTime, err := time.Parse(time.RFC3339, rec.Timestamp)
		if err != nil {
			return Document{}, fmt.Errorf("bsky session record %d timestamp %q is not RFC3339", rec.Index, rec.Timestamp)
		}
		rec.Time = recordTime
		rec.Fidelity = FidelityNative
		if err := emit(rec); err != nil {
			return Document{}, err
		}
		idx++
	}
	if err := sc.Err(); err != nil {
		return Document{}, fmt.Errorf("bsky session scan: %w", err)
	}
	if first {
		return Document{}, fmt.Errorf("bsky session: empty document")
	}
	doc.RecordCount = idx
	doc.UserTurnCount = userTurns
	doc.ContentBytes = contentBytes

	if env.Type != "session" {
		return Document{}, fmt.Errorf("bsky session type=%q, want \"session\"", env.Type)
	}
	if env.Schema != FormatBsky {
		return Document{}, fmt.Errorf("bsky session schema=%q, want %q", env.Schema, FormatBsky)
	}
	if env.SessionID == "" || env.Owner == "" || env.Source == "" || env.OwnerKind == "" || env.Role == "" || env.Revision == "" {
		return Document{}, fmt.Errorf("bsky session incomplete canonical identity (session_id=%q owner=%q source=%q)",
			env.SessionID, env.Owner, env.Source)
	}
	if expectedSessionID != "" && env.SessionID != expectedSessionID {
		return Document{}, fmt.Errorf("bsky session session_id=%q does not match requested %q", env.SessionID, expectedSessionID)
	}
	if expectedRevision != "" && env.Revision != expectedRevision {
		return Document{}, fmt.Errorf("bsky session revision=%q does not match requested %q", env.Revision, expectedRevision)
	}
	if env.RecordCount != doc.RecordCount {
		return Document{}, fmt.Errorf("bsky session envelope record_count=%d but parsed %d records", env.RecordCount, doc.RecordCount)
	}
	if env.UserTurnCount != userTurns {
		return Document{}, fmt.Errorf("bsky session envelope user_turn_count=%d but parsed %d user records", env.UserTurnCount, userTurns)
	}
	if env.ContentBytes != contentBytes {
		return Document{}, fmt.Errorf("bsky session envelope content_bytes=%d but parsed record lines contain %d bytes", env.ContentBytes, contentBytes)
	}
	if env.Role != "primary" && env.Role != "secondary" {
		return Document{}, fmt.Errorf("bsky session role=%q is unknown", env.Role)
	}
	startedAt, err := time.Parse(time.RFC3339, env.StartedAt)
	if err != nil {
		return Document{}, fmt.Errorf("bsky session started_at %q is not RFC3339", env.StartedAt)
	}
	updatedAt, err := time.Parse(time.RFC3339, env.UpdatedAt)
	if err != nil {
		return Document{}, fmt.Errorf("bsky session updated_at %q is not RFC3339", env.UpdatedAt)
	}
	if updatedAt.Before(startedAt) {
		return Document{}, fmt.Errorf("bsky session updated_at precedes started_at")
	}
	if !env.Complete {
		// Refuse to index incomplete sessions rather than silently
		// training on a partial log.
		return Document{}, fmt.Errorf("bsky session marked incomplete (capture truncation or normalization recovery); refusing to publish")
	}

	if lineage := env.Lineage; lineage != nil {
		ancestorID, ancestorErr := uuid.Parse(lineage.SessionID)
		sessionID, sessionErr := uuid.Parse(env.SessionID)
		if env.Role != "secondary" || ancestorErr != nil || sessionErr != nil || ancestorID == sessionID ||
			(lineage.Kind != "parent" && lineage.Kind != "root") {
			return Document{}, fmt.Errorf("bsky session invalid lineage")
		}
	}
	doc.StartedAt = startedAt
	doc.UpdatedAt = updatedAt
	envelope := BskyEnvelope(env)
	doc.Bsky = &envelope
	doc.Harness = envelope.Source
	return doc, nil
}

// NormalizeBskyRecord rewrites rec into its allowlisted model-facing form.
// It reports whether unsupported content was replaced by a notice.
func NormalizeBskyRecord(rec *Record) (bool, error) {
	if !knownRoles[rec.Role] {
		*rec = Record{Index: rec.Index, ID: rec.ID, Timestamp: rec.Timestamp,
			Role: "system", Phase: "normalization", Content: json.RawMessage(`"[Unsupported record role omitted from the searchable view.]"`)}
		return true, nil
	}
	if rec.SessionRef != "" && (uuid.Validate(rec.SessionRef) != nil || rec.Role != "system") {
		return false, fmt.Errorf("invalid subagent session reference")
	}
	if rec.Content == nil {
		return false, nil
	}
	var s string
	if err := json.Unmarshal(rec.Content, &s); err == nil {
		rec.Content, _ = json.Marshal(s)
		return false, nil
	}
	// Rich content is either a single block object or an ordered array of
	// blocks. Validate the block type in both shapes.
	var blocks []json.RawMessage
	if err := json.Unmarshal(rec.Content, &blocks); err != nil {
		var single json.RawMessage
		if err2 := json.Unmarshal(rec.Content, &single); err2 != nil {
			return false, fmt.Errorf("content block type not scalar or array: %w", err)
		}
		blocks = []json.RawMessage{single}
	}
	warning := false
	canonical := make([]ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		var fields map[string]json.RawMessage
		var blk ContentBlock
		if json.Unmarshal(b, &fields) != nil || json.Unmarshal(b, &blk) != nil {
			canonical = append(canonical, ContentBlock{Type: "text", Text: "[Unsupported content omitted from the searchable view.]"})
			warning = true
			continue
		}
		switch blk.Type {
		case "text":
		case "image":
			for _, forbidden := range []string{"data", "bytes", "source", "base64"} {
				if _, ok := fields[forbidden]; ok {
					return false, fmt.Errorf("image block contains forbidden inline field %q", forbidden)
				}
			}
		default:
			blk = ContentBlock{Type: "text", Text: "[Unsupported content omitted from the searchable view.]"}
			warning = true
		}
		canonical = append(canonical, blk)
	}
	var err error
	trimmed := bytes.TrimSpace(rec.Content)
	if len(canonical) == 1 && len(blocks) == 1 && len(trimmed) > 0 && trimmed[0] == '{' {
		rec.Content, err = json.Marshal(canonical[0])
	} else {
		rec.Content, err = json.Marshal(canonical)
	}
	if err != nil {
		return false, fmt.Errorf("encode safe content: %w", err)
	}
	return warning, nil
}
