// Package chunk slices event body text into pieces the embedder sends
// to the embedding model. The rule from docs/design.md § event_chunks:
//
//   - <= 1500 chars: one chunk containing the whole body.
//   - >  1500 chars: target size = 1000 chars with 200-char overlap
//     between adjacent chunks. Overlap gives the embedder a bit of
//     context across chunk boundaries so that a concept split across
//     two chunks still has a shot at scoring high on whichever chunk
//     contains its query-relevant language.
//
// Why hybrid: the median chat message is well under 1500 chars
// (around 100). Chunking those would just inflate the embedder queue
// and add zero information. Long documents, session summaries and
// pasted code dumps are where chunking earns its keep.
//
// "chars" above = runes, not bytes. An emoji-heavy message would
// otherwise get split mid-codepoint, which the embedder would either
// reject (some models) or silently mangle (others). We target rune
// counts throughout and split only on whitespace when possible.
package chunk

import (
	"unicode"
	"unicode/utf8"
)

// Threshold is the rune-count boundary at which Chunk switches from
// "one chunk for the whole body" to "slide a window with overlap".
// Exposed as a const because a few callers (tests, chunker debug
// tools) find it useful to reference.
const Threshold = 1500

// Target is the nominal rune length of each slice produced when the
// body exceeds Threshold. Each chunk is at most Target runes; most
// will land right around this value, a tail chunk may be shorter.
const Target = 1000

// Overlap is the number of trailing runes of a chunk that are also
// included at the start of the next chunk. Smaller than Target by
// construction so forward progress is guaranteed.
const Overlap = 200

// Chunk splits body into one or more slices per the hybrid rule.
// Returns an empty slice for an empty body (the events pipeline
// should never see that, but it's cheap to handle).
func Chunk(body string) []string {
	if body == "" {
		return nil
	}
	runes := []rune(body)
	if len(runes) <= Threshold {
		return []string{body}
	}

	// Emit sliding windows from a rune buffer. The first window starts
	// at 0; each subsequent start is (end - Overlap) of the prior
	// window, so consecutive chunks share `Overlap` runes. A window
	// ends at either start+Target or the nearest preceding whitespace
	// rune within a tolerance, whichever is closer to start+Target.
	// Breaking on whitespace means the embedder rarely sees a chunk
	// that begins or ends mid-word.
	var out []string
	n := len(runes)
	start := 0
	for start < n {
		end := start + Target
		if end >= n {
			out = append(out, string(runes[start:]))
			break
		}
		end = preferBoundary(runes, start, end)
		out = append(out, string(runes[start:end]))
		next := end - Overlap
		// Guarantee forward progress even in pathological inputs
		// (e.g. if preferBoundary snapped end too close to start).
		if next <= start {
			next = start + 1
		}
		start = next
	}
	return out
}

// preferBoundary looks for a whitespace rune within a small window
// before `hardEnd` and returns the split point just after that rune.
// If no whitespace is present in the window, returns hardEnd — the
// split happens mid-word. `lookback` of 64 runes is generous enough
// to catch sentence-end punctuation + space for normal English/code
// text while small enough that the worst-case chunk is within a few
// percent of the Target size.
func preferBoundary(runes []rune, start, hardEnd int) int {
	const lookback = 64
	lo := hardEnd - lookback
	if lo < start+1 {
		lo = start + 1
	}
	// Prefer the last whitespace rune in [lo, hardEnd). Split at the
	// index after that rune so the chunk ends cleanly without a
	// trailing space running into the next chunk's leading content.
	for i := hardEnd - 1; i >= lo; i-- {
		if unicode.IsSpace(runes[i]) {
			return i + 1
		}
	}
	return hardEnd
}

// RuneCount is a thin utf8.RuneCountInString wrapper. Exposed for the
// call sites that want to mirror Chunk's threshold test without
// shelling into this package's internals.
func RuneCount(s string) int {
	return utf8.RuneCountInString(s)
}
