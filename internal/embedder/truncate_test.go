package embedder

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Byte-level truncation of a
// string with multi-byte runes can split a codepoint and produce
// invalid UTF-8, which Postgres rejects on UPDATE. truncateRunes
// slices at rune boundaries so the output is always valid UTF-8.
func TestTruncateRunesPreservesUTF8(t *testing.T) {
	// Rocket emojis are 4 bytes each in UTF-8. A 600-rune string is
	// 2400 bytes. A naive byte slice to 512 would land on byte 512,
	// mid-codepoint of rune 129 (512 / 4 = 128, with 0-byte remainder
	// sometimes; for 4-byte runes any 512-byte slice boundary lands
	// on a boundary but other multi-byte runes won't. Use a mixed
	// input to be sure).
	mixed := strings.Repeat("a", 500) + strings.Repeat("\U0001F680", 200) // 500 ascii + 200*4 = 1300 bytes
	out := truncateRunes(mixed, 512)
	if utf8.RuneCountInString(out) > 512 {
		t.Errorf("rune count > 512: %d", utf8.RuneCountInString(out))
	}
	if !utf8.ValidString(out) {
		t.Errorf("output is not valid UTF-8")
	}
}

func TestTruncateRunesShort(t *testing.T) {
	if got := truncateRunes("hello", 512); got != "hello" {
		t.Errorf("short input: got %q, want unchanged", got)
	}
}

func TestTruncateRunesExact(t *testing.T) {
	s := strings.Repeat("a", 512)
	if got := truncateRunes(s, 512); got != s {
		t.Errorf("exact length: got len %d, want %d", len(got), len(s))
	}
}

func TestTruncateRunesJustUnder(t *testing.T) {
	// 512 bytes, but all ASCII = 512 runes; the fast-path returns
	// as-is.
	s := strings.Repeat("x", 512)
	if got := truncateRunes(s, 512); got != s {
		t.Error("identical ASCII-512 should be returned unchanged")
	}
}

func TestTruncateRunesMultibyteBoundary(t *testing.T) {
	// Build a string where the Nth rune is a 4-byte rune that would
	// be split by byte truncation. Emoji-only, 100 runes, 400 bytes.
	// Truncate to 50 runes -> 200 bytes, entirely valid.
	s := strings.Repeat("\U0001F680", 100)
	out := truncateRunes(s, 50)
	if utf8.RuneCountInString(out) != 50 {
		t.Errorf("rune count: got %d, want 50", utf8.RuneCountInString(out))
	}
	if !utf8.ValidString(out) {
		t.Error("output not valid UTF-8")
	}
}
