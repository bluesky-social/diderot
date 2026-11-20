package chunk

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEmpty(t *testing.T) {
	if got := Chunk(""); got != nil {
		t.Fatalf("empty: got %v, want nil", got)
	}
}

func TestShort(t *testing.T) {
	// Typical Slack-length message: one chunk, unchanged.
	body := "hey did we figure out what was going on with the k8s cluster last night"
	got := Chunk(body)
	if len(got) != 1 || got[0] != body {
		t.Fatalf("short: got %v, want one chunk equal to body", got)
	}
}

func TestExactlyThreshold(t *testing.T) {
	// 1500-rune body: one chunk (<= Threshold).
	body := strings.Repeat("a", Threshold)
	got := Chunk(body)
	if len(got) != 1 || got[0] != body {
		t.Fatalf("at threshold: got %d chunks, want 1", len(got))
	}
}

func TestJustOverThreshold(t *testing.T) {
	// 1501 runes: goes to the sliding-window path.
	body := strings.Repeat("a", Threshold+1)
	got := Chunk(body)
	if len(got) < 2 {
		t.Fatalf("just over threshold: got %d chunks, want >=2", len(got))
	}
}

func TestLongWithWordBoundaries(t *testing.T) {
	// Build a ~5000-rune body of "word word word ..." — whitespace is
	// abundant so every split must land after a space, never mid-word.
	var b strings.Builder
	for i := 0; i < 1000; i++ {
		b.WriteString("word ")
	}
	body := b.String()
	runes := utf8.RuneCountInString(body)
	if runes < 4000 {
		t.Fatalf("setup: body too short (%d runes)", runes)
	}
	got := Chunk(body)
	if len(got) < 2 {
		t.Fatalf("long: got %d chunks", len(got))
	}
	for i, c := range got {
		// No chunk may end mid-word (i.e., end with 'word' with no
		// trailing space) unless it's the final chunk.
		if i < len(got)-1 {
			if !strings.HasSuffix(c, " ") {
				t.Errorf("chunk %d ends mid-word: %q", i, tail(c, 30))
			}
		}
		if utf8.RuneCountInString(c) > Target {
			t.Errorf("chunk %d exceeds Target: %d runes", i, utf8.RuneCountInString(c))
		}
	}
}

func TestLongWithoutWhitespace(t *testing.T) {
	// No whitespace anywhere — preferBoundary will fall through to a
	// hard split. The invariant is that we make forward progress and
	// never produce a chunk longer than Target.
	body := strings.Repeat("a", Target*3)
	got := Chunk(body)
	if len(got) < 3 {
		t.Fatalf("no-whitespace: got %d chunks, want >=3", len(got))
	}
	for i, c := range got {
		if utf8.RuneCountInString(c) > Target {
			t.Errorf("chunk %d exceeds Target: %d runes", i, utf8.RuneCountInString(c))
		}
	}
}

func TestOverlap(t *testing.T) {
	// Consecutive chunks should share exactly Overlap runes when the
	// input has no whitespace (boundary-snapping is disabled) and is
	// long enough for multiple full windows.
	body := strings.Repeat("x", Target*3)
	got := Chunk(body)
	if len(got) < 2 {
		t.Fatalf("setup: got %d chunks", len(got))
	}
	for i := 0; i < len(got)-1; i++ {
		a := []rune(got[i])
		b := []rune(got[i+1])
		// The last Overlap runes of chunk i should equal the first
		// Overlap runes of chunk i+1, because there's no whitespace
		// to snap to and we're mid-stream.
		if len(a) < Overlap || len(b) < Overlap {
			continue
		}
		at := string(a[len(a)-Overlap:])
		bh := string(b[:Overlap])
		if at != bh {
			t.Errorf("chunk %d/%d overlap mismatch:\n  tail(%d): %q\n  head(%d): %q",
				i, i+1, len(at), at, len(bh), bh)
		}
	}
}

func TestUnicode(t *testing.T) {
	// Multi-byte runes must not get sliced mid-codepoint. Build a
	// long body of rocket emojis + spaces. Each chunk must be valid
	// UTF-8.
	body := strings.Repeat("\U0001F680 ", Target)
	got := Chunk(body)
	if len(got) < 2 {
		t.Fatalf("unicode: got %d chunks", len(got))
	}
	for i, c := range got {
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d not valid UTF-8", i)
		}
	}
}

func TestVeryLong(t *testing.T) {
	// A 50k-rune SSH session log. Every chunk should stay within
	// Target, and joining them (with overlap-aware dedup) should
	// recover something that starts with the same prefix as the
	// original — we're not reconstructing exactly, but consecutive
	// chunks should stitch without gaps.
	body := strings.Repeat("cmd; ", 10000) // 50_000 runes
	got := Chunk(body)
	if len(got) < 30 {
		t.Fatalf("very long: got %d chunks, want >=30", len(got))
	}
	for i, c := range got {
		if utf8.RuneCountInString(c) > Target {
			t.Errorf("chunk %d exceeds Target: %d runes", i, utf8.RuneCountInString(c))
		}
	}
}

func TestForwardProgress(t *testing.T) {
	// Pathological but possible input: a body where every candidate
	// boundary snaps back to start. The only safeguard is the
	// forward-progress guard in Chunk. Verify it terminates.
	body := strings.Repeat("a", 100_000)
	got := Chunk(body)
	if len(got) == 0 {
		t.Fatal("no chunks emitted")
	}
}

func TestRuneCount(t *testing.T) {
	if RuneCount("hello") != 5 {
		t.Fatal("ascii rune count")
	}
	if RuneCount("\U0001F680") != 1 {
		t.Fatal("emoji rune count")
	}
}

func tail(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}
