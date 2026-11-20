package slack

import (
	"regexp"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// This file model-checks sanitizeSlackMarkdown (sanitize.go) against its
// documented postcondition: however the input is built, the output never
// contains a raw "<!" or "<@" substring, in any letter case, and the
// function never consumes content it has no business touching.

// sanitizeFragments draws from the grammar of Slack tokens, stray
// brackets, and ordinary text: every dangerous form in several casings
// and with/without a label, the forms that must survive verbatim
// (channel refs, links), the lone punctuation characters that make up
// those forms (so the generator also produces stray/doubled/malformed
// brackets, not just well-formed tokens), and plain prose.
var sanitizeFragments = []string{
	// Dangerous forms, mixed case, with and without a label.
	"<!channel>", "<!CHANNEL>", "<!Channel>",
	"<!here>", "<!HERE>", "<!Here|label>",
	"<!everyone>", "<!EVERYONE>", "<!everyone|hey all>",
	"<!subteam^S012345>", "<!SUBTEAM^S012345|SRE agents>",
	"<@U123>", "<@U123|Jane Doe>", "<@W999|Bot Name>",
	"<!date^1530892800|time>", "<!DATE^1530892800>",
	// Forms that must round-trip verbatim.
	"<#C123>", "<#C123|general>",
	"<https://example.com>", "<https://example.com|Example>",
	"<mailto:hi@example.com|email>",
	// Bare punctuation: lets the walk build doubled brackets, stray
	// brackets, and malformed forms out of the well-formed ones above.
	"<", ">", "<<", ">>", "|", "@", "!", "^",
	// Ordinary text.
	"plain text ", "is 5 ", " 3? reply", "\n", "hello world",
}

func genSanitizeInput(t *rapid.T) string {
	n := rapid.IntRange(1, 10).Draw(t, "n_fragments")
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(rapid.SampledFrom(sanitizeFragments).Draw(t, "frag"))
	}
	return b.String()
}

// liveTokenOpener matches the two characters that start every
// notification-triggering Slack form this function exists to remove,
// in any case (the characters '<', '!', '@' carry no case themselves;
// what varies is the keyword after them, which this check does not
// need to inspect: the postcondition is simply that neither opener
// ever survives, however the keyword after it is cased).
var liveTokenOpener = regexp.MustCompile(`<!|<@`)

// TestSanitizeSlackMarkdownNeverLeavesALiveToken: the output never
// contains "<!" or "<@", however the dangerous forms, stray brackets,
// and surrounding text are combined or cased.
func TestSanitizeSlackMarkdownNeverLeavesALiveToken(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := genSanitizeInput(t)
		out := sanitizeSlackMarkdown(in)
		if loc := liveTokenOpener.FindStringIndex(out); loc != nil {
			t.Fatalf("sanitizeSlackMarkdown(%q) = %q still contains a live token opener at byte %d: %q",
				in, out, loc[0], out[loc[0]:min(loc[0]+20, len(out))])
		}
	})
}

// TestSanitizeSlackMarkdownIsIdempotent: sanitizing twice equals
// sanitizing once. A replacement must never itself look like a form
// that a second pass would transform differently.
func TestSanitizeSlackMarkdownIsIdempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := genSanitizeInput(t)
		once := sanitizeSlackMarkdown(in)
		twice := sanitizeSlackMarkdown(once)
		if once != twice {
			t.Fatalf("sanitizeSlackMarkdown(%q) = %q, but sanitizing it again gives %q (not idempotent)", in, once, twice)
		}
	})
}

// TestSanitizeSlackMarkdownLeavesPlainTextUnchanged: text with no
// angle brackets at all passes through byte-for-byte.
func TestSanitizeSlackMarkdownLeavesPlainTextUnchanged(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.StringMatching(`[a-zA-Z0-9 .,:;'"_\-\n]*`).Draw(t, "plain")
		if got := sanitizeSlackMarkdown(s); got != s {
			t.Fatalf("sanitizeSlackMarkdown(%q) = %q, want unchanged (no angle brackets in input)", s, got)
		}
	})
}

// safeAngleFragments builds text containing '<' characters that can
// never start a notifying form: every dangerous form's regex requires
// '!' or '@' immediately after its '<' (or after absorbing a run of
// more '<'s — see sanitize.go's doc comment), and none of these
// fragments ever place one of those two characters right after a '<',
// nor does any fragment end with '<' followed by a fragment that
// starts with '!' or '@' (no fragment here starts with either). So any
// string built from them is "outside tokens" by construction.
var safeAngleFragments = []string{
	"<", "<=", "< ", "<-", "<3", "<n", "<String>", "<namespace>", "<pod>",
	"i", " ", "=", "-", "n", "x", "y", "5", "3", "is", "and", "List",
	"plain text ", "hello world", "\n", "`code`",
}

func genSafeAngleInput(t *rapid.T) string {
	n := rapid.IntRange(1, 12).Draw(t, "n_fragments")
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(rapid.SampledFrom(safeAngleFragments).Draw(t, "frag"))
	}
	return b.String()
}

// TestSanitizeSlackMarkdownLeavesTextOutsideTokensUnchanged is a
// property: a '<' that is not immediately followed by '!' or '@' —
// directly, or after absorbing a run of more '<'s — can never start a
// notifying form, so it must never be escaped or otherwise altered.
// Escaping every '<' that does not start a *complete* known form would
// over-sanitize code, comparisons, and generics ("i < n", "a <= b",
// "List<String>", "<namespace>") that legitimately contain a bare '<'.
func TestSanitizeSlackMarkdownLeavesTextOutsideTokensUnchanged(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := genSafeAngleInput(t)
		if got := sanitizeSlackMarkdown(in); got != in {
			t.Fatalf("sanitizeSlackMarkdown(%q) = %q, want unchanged (no '<' here is followed by '!' or '@', so none of it can start a notifying form)", in, got)
		}
	})
}

// TestSanitizeSlackMarkdownPreservesAllowedForms: the forms the doc
// comment says are preserved verbatim (channel refs, Slack link
// syntax) must round-trip unchanged even when adjacent to dangerous
// forms, stray brackets, or mixed-case keywords.
func TestSanitizeSlackMarkdownPreservesAllowedForms(t *testing.T) {
	allowed := []string{
		"<#C123>", "<#C123|general>",
		"<https://example.com>", "<https://example.com|Example>",
		"<mailto:hi@example.com|email>",
	}
	rapid.Check(t, func(t *rapid.T) {
		form := rapid.SampledFrom(allowed).Draw(t, "allowed_form")
		prefix := genSanitizeInput(t)
		suffix := genSanitizeInput(t)
		s := prefix + form + suffix
		out := sanitizeSlackMarkdown(s)
		if !strings.Contains(out, form) {
			t.Fatalf("sanitizeSlackMarkdown(%q) = %q, want it to still contain the preserved form %q verbatim", s, out, form)
		}
	})
}
