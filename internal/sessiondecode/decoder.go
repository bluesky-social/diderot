// Package sessiondecode maps agent-session documents in existing formats onto
// diderot's private record model. Diderot defines no wire format of its own:
// each launch format has a decoder, and nothing outside diderot depends on the
// record model.
//
// Decoder contract:
//
//  1. The same bytes always produce the same records and record ids.
//  2. A record's id is the source's own identifier for that entry when there
//     is one. An entry split into several records gets a suffix naming the
//     part's kind and its position within the entry. A pairing key shared by a
//     tool call and its output (a Codex call_id) is never an id on its own.
//  3. Every id carries its Fidelity.
//  4. Tool calls and their results stay paired by the native call id; a result
//     with no earlier matching call fails the document.
//  5. Only the live branch is emitted. An explicit compaction marker becomes a
//     system record; history absent from the snapshot is never reconstructed.
//  6. Content passes through unchanged apart from normalization into Record.
//
// A document that breaks a structural assumption fails as a whole.
// Diderot performs no redaction: producers redact before submitting.
package sessiondecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Fidelity says how far a record id can be trusted across revisions.
type Fidelity string

const (
	// FidelityNative ids come from the source, which keeps an entry's
	// identity across rewrites. It says nothing about content.
	FidelityNative Fidelity = "native"
	// FidelityPositional ids are stable only while the source only appends.
	FidelityPositional Fidelity = "positional"
)

// ErrStructural marks a document that breaks an assumption its decoder relies
// on (an unparseable line, an orphaned tool result, a missing id). Callers
// treat it as a permanent rejection of those bytes.
var ErrStructural = errors.New("session document is structurally invalid")

func structural(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrStructural, fmt.Sprintf(format, args...))
}

// contractGuard enforces id uniqueness and tool pairing (rules 2 and 4) over
// records in emission order.
type contractGuard struct {
	ids   map[string]bool
	calls map[string]bool
}

func newContractGuard() *contractGuard {
	return &contractGuard{ids: map[string]bool{}, calls: map[string]bool{}}
}

func (g *contractGuard) check(rec Record) error {
	if rec.ID == "" {
		return structural("record %d has no id", rec.Index)
	}
	if g.ids[rec.ID] {
		return structural("duplicate record id %q", rec.ID)
	}
	if rec.Role == "tool" && !g.calls[rec.ToolCallID] {
		return structural("record %q answers unknown tool call %q", rec.ID, rec.ToolCallID)
	}
	g.ids[rec.ID] = true
	for _, call := range rec.ToolCalls {
		if call.ID != "" {
			g.calls[call.ID] = true
		}
	}
	return nil
}

// Record is one normalized transcript entry. The JSON encoding is the
// model-facing line the summarizer and expand read; its field set and order
// are load-bearing, because summary cache identity hashes those bytes.
type Record struct {
	Index           int             `json:"index"`
	ID              string          `json:"id"`
	Role            string          `json:"role"`
	Content         json.RawMessage `json:"content,omitempty"`
	Timestamp       string          `json:"timestamp"`
	TimestampSource string          `json:"timestamp_source,omitempty"`
	Model           string          `json:"model,omitempty"`
	ProviderModel   string          `json:"provider_model,omitempty"`
	InvocationID    string          `json:"invocation_id,omitempty"`
	Phase           string          `json:"phase,omitempty"`
	ToolCalls       []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID      string          `json:"tool_call_id,omitempty"`
	IsError         bool            `json:"is_error,omitempty"`
	Captured        *bool           `json:"captured,omitempty"`
	SessionRef      string          `json:"session_ref,omitempty"`

	// Fidelity and Time are metadata, never part of the model-facing line.
	Fidelity Fidelity  `json:"-"`
	Time     time.Time `json:"-"`
}

// ToolCall is one tool invocation carried by an assistant record. ID is the
// source's native call id; a tool record's ToolCallID names it.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

// ContentBlock is one element of rich record content.
type ContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	AssetID   string `json:"asset_id,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Alt       string `json:"alt,omitempty"`
	Available *bool  `json:"available,omitempty"`
}

// Document is the session-level result of decoding one snapshot.
type Document struct {
	Format         string
	DecoderVersion string
	// SourceVersion is the producer version the document declared, or "" when
	// the format carries none. It is recorded on every stored revision.
	SourceVersion string
	// Harness names the agent that produced the session ("claude-code",
	// "codex", ...); session-ingest policy filters on it.
	Harness string
	// SessionID is the session ID a native session file declares, set only
	// by decoders whose harness names the file after part of it (Gemini
	// CLI); "" otherwise.
	SessionID      string
	StartedAt      time.Time
	UpdatedAt      time.Time
	RecordCount    int
	UserTurnCount  int
	ContentBytes   int64
	WarningRecords int
	// Bsky is the envelope of a bsky.session.v1 document; nil otherwise.
	Bsky *BskyEnvelope
}

// Decoder maps one format's bytes onto Records.
type Decoder interface {
	// Format is the name producers send in the Diderot-Session-Format header.
	Format() string
	// Version identifies the mapping. Changing what Decode emits for the same
	// bytes requires a new Version, which yields a new materialization.
	Version() string
	// Decode validates data as a whole and passes each record, in order, to
	// emit. On error the caller discards everything emitted.
	Decode(data []byte, emit func(Record) error) (Document, error)
}

// DecodeAll runs d and collects its records.
func DecodeAll(d Decoder, data []byte) (Document, []Record, error) {
	var records []Record
	doc, err := d.Decode(data, func(r Record) error {
		records = append(records, r)
		return nil
	})
	if err != nil {
		return Document{}, nil, err
	}
	return doc, records, nil
}

// Registry resolves a format name to its decoder.
type Registry struct {
	decoders map[string]Decoder
}

// NewRegistry builds a registry; a duplicate format panics (programmer error).
func NewRegistry(decoders ...Decoder) *Registry {
	r := &Registry{decoders: make(map[string]Decoder, len(decoders))}
	for _, d := range decoders {
		if _, dup := r.decoders[d.Format()]; dup {
			panic("sessiondecode: duplicate decoder for " + d.Format())
		}
		r.decoders[d.Format()] = d
	}
	return r
}

// Launch is the registry of every launch decoder.
func Launch() *Registry {
	return NewRegistry(Bsky{}, ClaudeCode{}, Codex{}, CopilotCLI{}, GeminiCLI{}, Pi{}, Polytoken{}, ATIF{}, STS{})
}

// Lookup returns the decoder for format.
func (r *Registry) Lookup(format string) (Decoder, bool) {
	d, ok := r.decoders[format]
	return d, ok
}

// Formats lists the registered format names in sorted order.
func (r *Registry) Formats() []string {
	out := make([]string, 0, len(r.decoders))
	for f := range r.decoders {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
