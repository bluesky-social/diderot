package askchat

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSplitForSlackMessage_Empty: empty input round-trips to a
// single empty chunk. Callers (postAskAnswer) substitute a
// non-empty placeholder before calling this; we just don't crash.
func TestSplitForSlackMessage_Empty(t *testing.T) {
	got := Split("", 100)
	if len(got) != 1 || got[0] != "" {
		t.Errorf("got %#v, want [\"\"]", got)
	}
}

// TestSplitForSlackMessage_ShortPassthrough: input under the limit
// returns as a single chunk byte-for-byte unchanged.
func TestSplitForSlackMessage_ShortPassthrough(t *testing.T) {
	in := "hello world\nsecond line\n"
	got := Split(in, 100)
	if len(got) != 1 || got[0] != in {
		t.Errorf("got %#v, want [%q]", got, in)
	}
}

// TestSplitForSlackMessage_ExactlyAtLimit: input of exactly limit
// bytes returns as one chunk (the boundary is inclusive — splitting
// would just produce a final empty chunk for nothing).
func TestSplitForSlackMessage_ExactlyAtLimit(t *testing.T) {
	in := strings.Repeat("a", 100)
	got := Split(in, 100)
	if len(got) != 1 || got[0] != in {
		t.Errorf("got %d chunks (want 1)", len(got))
	}
}

// TestSplitForSlackMessage_ChunksAtLineBreak: when the input is
// over the limit but contains '\n's, the chunker prefers the last
// '\n' at or before the limit as its cut.
func TestSplitForSlackMessage_ChunksAtLineBreak(t *testing.T) {
	in := strings.Repeat("a", 60) + "\n" +
		strings.Repeat("b", 60) + "\n" +
		strings.Repeat("c", 60) + "\n"
	got := Split(in, 100)
	// 60a + '\n' = 61 bytes ≤ 100. Adding 60b would put us at 121,
	// so the second cut is the '\n' at index 61. Chunk 0 = "aa..a"
	// (60 chars). Then rest starts with the b run (TrimPrefix
	// dropped the '\n'). Chunk 1 = "bb..b" (60). Etc.
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3:\n%v", len(got), got)
	}
	if got[0] != strings.Repeat("a", 60) {
		t.Errorf("chunk 0 = %q", got[0])
	}
	if got[1] != strings.Repeat("b", 60) {
		t.Errorf("chunk 1 = %q", got[1])
	}
	if got[2] != strings.Repeat("c", 60)+"\n" {
		t.Errorf("chunk 2 = %q", got[2])
	}
}

// TestSplitForSlackMessage_LongSingleLine_HardCut: when a single
// line exceeds the limit, the chunker falls back to a byte-boundary
// hard cut walked back to a rune start.
func TestSplitForSlackMessage_LongSingleLine_HardCut(t *testing.T) {
	in := strings.Repeat("a", 250)
	got := Split(in, 100)
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3:\n%v", len(got), got)
	}
	if len(got[0]) != 100 || len(got[1]) != 100 || len(got[2]) != 50 {
		t.Errorf("chunk lens = [%d %d %d], want [100 100 50]",
			len(got[0]), len(got[1]), len(got[2]))
	}
	if got[0]+got[1]+got[2] != in {
		t.Errorf("reassembled chunks ≠ input")
	}
}

// TestSplitForSlackMessage_MultiByteAtBoundary: a 4-byte UTF-8
// rune (😀 = 4 bytes) straddling the cut point must NOT be split
// mid-rune. The chunker walks back to the rune start, so the rune
// lands wholly in the next chunk.
func TestSplitForSlackMessage_MultiByteAtBoundary(t *testing.T) {
	// "😀" is 0xF0 0x9F 0x98 0x80, 4 bytes.
	// Construct input: 98 'a's + "😀" + 98 'b's = 98 + 4 + 98 = 200.
	// At limit=100, the first cut walks back from byte index 100.
	// Byte 100 is the third byte of the emoji (a continuation
	// byte), so cut walks back to byte 98 (the rune start). Chunk
	// 0 should be 98 'a's, chunk 1 should start with the emoji.
	in := strings.Repeat("a", 98) + "\U0001F600" + strings.Repeat("b", 98)
	got := Split(in, 100)
	if len(got) < 2 {
		t.Fatalf("got %d chunks, want ≥2", len(got))
	}
	if got[0] != strings.Repeat("a", 98) {
		t.Errorf("chunk 0 = %q (len %d), want 98 a's",
			got[0], len(got[0]))
	}
	if !strings.HasPrefix(got[1], "\U0001F600") {
		t.Errorf("chunk 1 should start with emoji; got prefix %q",
			got[1][:8])
	}
	for i, c := range got {
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8: %q", i, c)
		}
	}
}

// TestSplitForSlackMessage_PreservesParagraphBreaks: a paragraph
// break ("foo\n\nbar") that lands at a chunk boundary must NOT
// have both newlines eaten. TrimPrefix drops one, leaving the
// second '\n' as the start of the next chunk. The original
// double-newline survives across chunks.
func TestSplitForSlackMessage_PreservesParagraphBreaks(t *testing.T) {
	// Construct input where the chunk boundary lands on the first
	// '\n' of a paragraph break. 95 'a's + "\n\n" + 95 'b's is
	// 95+2+95 = 192. Limit=100. First chunk: scan rest[0:100], find
	// last '\n' at index 96. Chunk 0 = first 96 bytes = 95 'a's +
	// '\n'... wait, that's not the boundary I want.
	//
	// To force the boundary to land on the FIRST of two '\n's,
	// build: 99 'a's + '\n' + 'b' + '\n' + 90 'c's = 99+1+1+1+90 = 192.
	// At limit=100, scan rest[0:100], last '\n' at index 99 (the
	// one after the 99 a's). Chunk 0 = "aaa...a" (99 chars). Then
	// rest[99:] = "\nb\nccc...". TrimPrefix drops one '\n' →
	// "b\nccc...". Chunk 1 starts with "b\n" — paragraph break
	// preserved as expected.
	//
	// More direct test: literal "foo\n\nbar" with limit=4 forces
	// the boundary to land on the first '\n'. Limit=4: scan
	// "foo\n", last '\n' at index 3. Chunk 0 = "foo". rest[3:] =
	// "\n\nbar". TrimPrefix("\n") → "\nbar". Chunk 1 = "\nbar"
	// (≤4). Paragraph break SURVIVES across the chunk boundary.
	got := Split("foo\n\nbar", 4)
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2: %#v", len(got), got)
	}
	if got[0] != "foo" {
		t.Errorf("chunk 0 = %q, want \"foo\"", got[0])
	}
	if got[1] != "\nbar" {
		t.Errorf("chunk 1 = %q, want \"\\nbar\" (paragraph break preserved)", got[1])
	}
}

// TestSplitForSlackMessage_EmptyLineInsideCodeBlock: empty lines
// inside a code block must survive across chunk boundaries — same
// TrimPrefix guarantee as paragraph breaks.
func TestSplitForSlackMessage_EmptyLineInsideCodeBlock(t *testing.T) {
	in := "```\n" +
		"line1\n" +
		"\n" +
		"line2\n" +
		"```"
	// limit big enough that the whole thing fits — should round-trip.
	got := Split(in, 1000)
	if len(got) != 1 || got[0] != in {
		t.Errorf("expected single-chunk passthrough, got %#v", got)
	}
}

// TestSplitForSlackMessage_ZeroLimitReturnsInputUnchanged: defensive
// against caller passing limit≤0. Better to return the input
// unchanged than infinite-loop.
func TestSplitForSlackMessage_ZeroLimitReturnsInputUnchanged(t *testing.T) {
	got := Split("hello", 0)
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("got %#v, want [\"hello\"]", got)
	}
}

// TestSplitForSlackMessage_RealisticLLMAnswer: the 8164-char
// answer from yesterday's bug report (rough size + shape). A
// typical model output of ~8 KB should split into 3 chunks at
// the production 3500-byte limit. Each chunk well under the
// 4000-char `text` cap.
func TestSplitForSlackMessage_RealisticLLMAnswer(t *testing.T) {
	// Synthesize a plausible 8KB Markdown body with bullets +
	// paragraphs, structurally similar to what the model emits.
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("• Item " + strings.Repeat("x", 50) + "\n")
		b.WriteString("  more detail " + strings.Repeat("y", 100) + "\n")
		b.WriteString("\n")
	}
	in := b.String()
	got := Split(in, slackChunkBytes)
	if len(got) < 2 {
		t.Fatalf("expected ≥2 chunks, got %d (input %d bytes)",
			len(got), len(in))
	}
	for i, c := range got {
		if len(c) > slackChunkBytes {
			t.Errorf("chunk %d = %d bytes, exceeds limit %d",
				i, len(c), slackChunkBytes)
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d is not valid UTF-8", i)
		}
	}
}

// slackChunkBytes is Slack's chunk limit, the case these tests were written
// against.
const slackChunkBytes = 3500
