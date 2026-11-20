package files

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// syntheticGithubPATForIndex derives a distinct, high-entropy, GitHub-PAT-
// shaped synthetic secret from an index: "ghp_" plus the first 36 hex
// characters of sha256("redact-test-secret-<i>"). None of these are real
// credentials. github-pat's rule has no secretGroup and no separate entropy
// floor of its own, but gitleaks' generic-api-key-style low-entropy
// allowlisting elsewhere means a literal, mostly-zero placeholder (e.g.
// "ghp_000...0001") is NOT reliably detected — a hash gives each one the
// entropy a real token has, so DetectString flags it like it would a real
// leaked PAT.
func syntheticGithubPATForIndex(i int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("redact-test-secret-%d", i)))
	return "ghp_" + hex.EncodeToString(h[:])[:36]
}

// TestRedactManyRepeatsOfSameSecretDoesNotBlowUpMemory covers the same
// detected secret repeated many times in one call to Redact, which is
// exactly what a shared .xlsx with one token column repeated down thousands
// of rows produces. Quadratic growth here exceeds any reasonable memory
// limit.
//
// gitleaks returns one finding per match location, so a secret that occurs
// k times produces k findings with an identical Secret. Building byte-range
// spans by looping over FINDINGS and, for each one, re-scanning the whole
// string for every occurrence of that finding's Secret is k full scans, each
// rediscovering all k occurrences and allocating a fresh map per span: O(k^2)
// spans and allocations instead of O(k). At 2,000 repeats of one GitHub PAT
// that is more than a gigabyte.
//
// This test measures total bytes allocated (runtime.MemStats.TotalAlloc,
// not live heap, so GC timing doesn't make it flaky) across a single
// Redact call over 2,000 repeats of one secret, and fails if it exceeds a
// budget far above what a linear, one-scan-per-unique-secret
// implementation needs but far below what a quadratic one allocates at
// this size.
func TestRedactManyRepeatsOfSameSecretDoesNotBlowUpMemory(t *testing.T) {
	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}

	const rows = 2000
	var sb strings.Builder
	for range rows {
		sb.WriteString(syntheticGithubPAT)
		sb.WriteByte('\n')
	}
	text := sb.String()

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	redacted, findings := r.Redact(text)

	runtime.ReadMemStats(&after)

	if strings.Contains(redacted, syntheticGithubPAT) {
		t.Error("synthetic GitHub PAT still present after redacting many repeats of it")
	}
	if len(findings) != rows {
		t.Errorf("got %d findings, want %d (one per repeated occurrence)", len(findings), rows)
	}

	// 150 MiB: generous enough that a linear implementation's ordinary
	// allocations (the spans, their small per-rule-ID maps, the output
	// builder, gitleaks' own match bookkeeping) have ample room, but far
	// below the gigabyte-plus a quadratic implementation allocates at this
	// size.
	const budget = 150 << 20
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > budget {
		t.Errorf("Redact allocated %d bytes redacting %d repeats of one secret (budget %d bytes); "+
			"this is the quadratic-blowup regression: redactOnePass must scan the text once per "+
			"UNIQUE secret, not once per gitleaks finding, or a secret repeated many times "+
			"(e.g. one token down thousands of spreadsheet rows) allocates memory quadratic in the "+
			"repeat count and can OOM-kill the pod", allocated, rows, budget)
	}
}

// windowsOf200CharLine returns the n distinct w-byte-wide sliding windows of
// a fixed 200-character line (every offset 0..200-w), for use as crafted
// "secrets" below. Built from a repeating alphanumeric sequence so no window
// repeats another.
func windowsOf200CharLine(w int) []string {
	const line = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"0123456789"
	var windows []string
	for i := 0; i+w <= len(line); i++ {
		windows = append(windows, line[i:i+w])
	}
	return windows
}

// TestRedactManyOverlappingDistinctSecretsDoesNotBlowUpMemoryOrTime covers
// the case the test above does not: grouping by identical Secret bounds
// memory only when secrets don't overlap. The shape is many DISTINCT secrets
// that are all overlapping substrings of one line, that line repeated many
// times. Each repeat makes every one of those distinct secrets occur again,
// so an implementation that keeps one span (with its own rule-ID map) per
// OCCURRENCE has (distinct secrets) x (repeats) spans: a crafted document of
// a few kilobytes with thousands of distinct secrets reaches millions of
// spans and gigabytes of allocation.
//
// This test drives maskSecretOccurrences directly (see its doc comment)
// with a crafted secretRuleIDs map of many distinct, heavily overlapping
// secrets — every sliding window of one 200-character line — repeated many
// times, instead of depending on gitleaks to detect thousands of
// realistic-looking credentials. Within one repeat of the line, every
// window overlaps its neighbors (window i and i+1 share w-1 of their w
// bytes), so the whole repeat should merge into a single masked span; the
// number of merged spans (and findings) should equal the repeat count, not
// (distinct secrets) x (repeats).
func TestRedactManyOverlappingDistinctSecretsDoesNotBlowUpMemoryOrTime(t *testing.T) {
	const windowLen = 20
	windows := windowsOf200CharLine(windowLen)
	if len(windows) < 100 {
		t.Fatalf("fixture produced only %d windows; test needs many distinct, overlapping secrets", len(windows))
	}
	secretRuleIDs := make(map[string]map[string]struct{}, len(windows))
	for _, w := range windows {
		secretRuleIDs[w] = map[string]struct{}{"synthetic-overlap-rule": {}}
	}

	// Reconstruct the original 200-char line from its first window plus
	// the last byte of every subsequent window — i.e. the line
	// windowsOf200CharLine's windows were cut from.
	fullLineBytes := []byte(windows[0])
	for _, w := range windows[1:] {
		fullLineBytes = append(fullLineBytes, w[len(w)-1])
	}
	fullLine := string(fullLineBytes)

	const repeats = 5000
	var sb strings.Builder
	for range repeats {
		sb.WriteString(fullLine)
		sb.WriteByte('\n')
	}
	text := sb.String()

	// Sanity: every window really is a substring of what we built, and the
	// fixture is actually exercising overlap (adjacent windows share
	// windowLen-1 bytes), not just adjacency.
	if !strings.Contains(text, windows[0]) || !strings.Contains(text, windows[len(windows)-1]) {
		t.Fatal("fixture line does not contain its own windows")
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()

	redacted, findings := maskSecretOccurrences(text, secretRuleIDs)

	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	if strings.Contains(redacted, fullLine) {
		t.Error("the repeated line still appears verbatim after masking its own overlapping windows")
	}
	for _, w := range []string{windows[0], windows[len(windows)/2], windows[len(windows)-1]} {
		if strings.Contains(redacted, w) {
			t.Errorf("planted window %q still present after masking", w)
		}
	}
	if len(findings) != repeats {
		t.Errorf("got %d findings, want %d (one merged span per repeated line, all overlapping "+
			"windows within one repeat should merge into a single span)", len(findings), repeats)
	}

	// 40 MiB: generous for a linear (len(s)-proportional) implementation —
	// the coverage array (len(text) int32s, a few MB), ~repeats small
	// per-span maps, and the output builder — but far below what one
	// allocation per OCCURRENCE (distinct secrets x repeats, here
	// ~181 x 5,000 = ~905,000) would cost.
	const memBudget = 40 << 20
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > memBudget {
		t.Errorf("maskSecretOccurrences allocated %d bytes (budget %d bytes) masking %d distinct "+
			"overlapping windows across %d repeats; this is the quadratic-in-distinct-secrets "+
			"regression: maskSecretOccurrences must mark occurrences in a "+
			"coverage array and merge, not allocate one span per occurrence", allocated, memBudget,
			len(windows), repeats)
	}

	// 10s: generous on a shared machine. A correct implementation finishes
	// in a small fraction of this budget; a quadratic one does not.
	const timeBudget = 10 * time.Second
	if elapsed > timeBudget {
		t.Errorf("maskSecretOccurrences took %s (budget %s) masking %d distinct overlapping "+
			"windows across %d repeats", elapsed, timeBudget, len(windows), repeats)
	}
}

// TestRedactManyNonMergingSpansFailsClosedAndWithholdsWholeText covers
// what a coverage-sweep merge alone leaves open: it bounds memory per
// OCCURRENCE, but not per MERGED SPAN. A single distinct secret value,
// repeated many times with a separator between repeats so none of the
// repeats ever merge into one another, produces one merged span per
// repeat — a count redactMaxDistinctSecrets does nothing to limit, since
// it bounds distinct secret VALUES and there is only one here.
//
// A small crafted DOCX reaches this: its word/document.xml can inflate to
// a hundred megabytes of one short secret repeated with spaces between
// copies, which is millions of merged spans. A record per span (a map, an
// ids slice, a Fprintf call) is then gigabytes of live heap in one call to
// Redact, multiplied by the number of concurrent extraction workers.
//
// This test reproduces that shape — one distinct secret, repeated,
// separated so repeats never merge — at a smaller scale chosen to
// comfortably exceed redactMaxMaskedSpans while staying fast in CI: well
// beyond the cap, Redact must withhold the whole text, and must do so
// before paying any per-span allocation, not after building one record per
// span and then discarding it.
func TestRedactManyNonMergingSpansFailsClosedAndWithholdsWholeText(t *testing.T) {
	const secret = "ghp_" + "0123456789abcdef" + "0123456789abcdef0123"
	const repeats = redactMaxMaskedSpans * 2
	secretRuleIDs := map[string]map[string]struct{}{
		secret: {"github-pat": {}},
	}

	var sb strings.Builder
	for i := 0; i < repeats; i++ {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(secret)
	}
	text := sb.String()

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()

	redacted, findings := maskSecretOccurrences(text, secretRuleIDs)

	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	if strings.Contains(redacted, secret) {
		t.Error("planted secret still present after Redact should have withheld the text")
	}
	if want := "[REDACTED:" + redactTooManyMaskedSpansRuleID + "]"; redacted != want {
		t.Errorf("got redacted %q, want the whole-text withheld marker %q", redacted, want)
	}
	if len(findings) != 1 || findings[0] != redactTooManyMaskedSpansRuleID {
		t.Errorf("got findings %v, want exactly [%q] (fail-closed, whole text withheld)",
			findings, redactTooManyMaskedSpansRuleID)
	}

	// 80 MiB: generous for the coverage array (len(text) int32s — a few
	// tens of MB at this size — plus the small rule-ID interning
	// structures), but far below what one map-plus-slice-plus-Fprintf
	// record per merged span would cost at `repeats` spans.
	// redactMaxDistinctSecrets does nothing to bound this case (there is
	// only ONE distinct secret value here), so the span cap must be
	// checked before any per-span cost is paid.
	const budget = 80 << 20
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > budget {
		t.Errorf("maskSecretOccurrences allocated %d bytes (budget %d bytes) masking %d non-merging "+
			"repeats of one secret; this is the unbounded-merged-span-count regression: above "+
			"redactMaxMaskedSpans, Redact must withhold before building a per-span record, not "+
			"after building %d of them", allocated, budget, repeats, repeats)
	}

	// 10s: generous on a shared machine; the point is that withholding
	// short-circuits the per-span work entirely instead of merely being
	// memory-bounded but still slow.
	const timeBudget = 10 * time.Second
	if elapsed > timeBudget {
		t.Errorf("maskSecretOccurrences took %s (budget %s) masking %d non-merging repeats of one secret",
			elapsed, timeBudget, repeats)
	}
}

// TestRedactTooManyDistinctSecretsFailsClosedAndWithholdsWholeText covers
// the distinct-secret cap: even a linear-in-occurrences implementation
// still costs one O(len(s)) scan per DISTINCT secret, so a document crafted
// to carry an adversarially large number of distinct detected secrets can
// still drive that cost unboundedly high. Above redactMaxDistinctSecrets,
// Redact must not attempt it: it must fail closed and withhold the whole
// input, never pass a partially-checked text through.
//
// This drives the real pipeline (the process-wide gitleaks detector, not a
// crafted secretRuleIDs map) with more distinct GitHub-PAT-shaped secrets
// than redactMaxDistinctSecrets, interspersed with a benign marker that
// carries no secret of its own, and checks that NONE of it — neither the
// planted secrets nor the benign marker — survives, proving the whole text
// is withheld rather than selectively masked.
func TestRedactTooManyDistinctSecretsFailsClosedAndWithholdsWholeText(t *testing.T) {
	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}

	const distinctSecrets = redactMaxDistinctSecrets + 1
	const benignMarker = "BENIGN-CONTEXT-SHOULD-ALSO-BE-WITHHELD"

	var sb strings.Builder
	sb.WriteString(benignMarker)
	sb.WriteByte('\n')
	for i := range distinctSecrets {
		sb.WriteString(syntheticGithubPATForIndex(i))
		sb.WriteByte('\n')
	}
	text := sb.String()

	start := time.Now()
	redacted, findings := r.Redact(text)
	elapsed := time.Since(start)

	if strings.Contains(redacted, benignMarker) {
		t.Error("benign, non-secret text survived; the whole input should have been withheld, " +
			"not selectively masked")
	}
	for i := 0; i < distinctSecrets; i += distinctSecrets / 10 {
		secret := syntheticGithubPATForIndex(i)
		if strings.Contains(redacted, secret) {
			t.Errorf("planted secret %d still present after Redact withheld the text", i)
		}
	}
	if len(findings) != 1 || findings[0] != redactWithheldRuleID {
		t.Errorf("got findings %v, want exactly [%q] (fail-closed, whole text withheld)",
			findings, redactWithheldRuleID)
	}

	// 20s: generous on a shared machine. redactMaxDistinctSecrets exists
	// precisely so this path never attempts the O(len(s) x distinct
	// secrets) occurrence scan; if it regressed back to attempting that
	// scan here (distinctSecrets distinct secrets), this budget would
	// catch it.
	const timeBudget = 20 * time.Second
	if elapsed > timeBudget {
		t.Errorf("Redact took %s (budget %s) on %d distinct secrets; redactMaxDistinctSecrets "+
			"should have made it withhold the text immediately instead of scanning for every "+
			"occurrence of every one of them", elapsed, timeBudget, distinctSecrets)
	}
}
