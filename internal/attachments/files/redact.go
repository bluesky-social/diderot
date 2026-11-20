package files

import (
	"fmt"
	"math/bits"
	"sort"
	"strings"
	"sync"

	"github.com/zricethezav/gitleaks/v8/detect"
)

// secretRedactor scans flattened caption text for secrets using
// gitleaks' embedded default ruleset (~150 curated regexes) and
// replaces any matches with a "[REDACTED:<rule-id>]" marker before
// the caption is written to event_chunks and embedded. A screenshot
// of a .env file or a terminal token is exactly the kind of thing
// the VLM would otherwise transcribe verbatim.
//
// Construction compiles all ~150 regexes and is NOT cheap. newSecretRedactor
// initializes one process-wide instance and every attachment worker reuses it.
type secretRedactor struct {
	detector *detect.Detector
}

var processSecretRedactor = sync.OnceValues(func() (*secretRedactor, error) {
	// NewDetectorDefaultConfig uses Viper's package-global singleton while it
	// parses the embedded rules. Calling it concurrently can race its internal
	// maps and crash the process, so initialization must remain behind this
	// process-wide once boundary.
	d, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		return nil, fmt.Errorf("gitleaks default detector: %w", err)
	}
	// The scanner skips any line carrying its inline allow marker. That
	// is a repository author's say over their own fixtures, not something
	// the text of a screenshot or shared document gets to assert.
	d.IgnoreGitleaksAllow = true
	return &secretRedactor{detector: d}, nil
})

// newSecretRedactor returns the process-wide gitleaks redactor. Construction
// errors are cached because the embedded config cannot change during a
// process. A worker that cannot build it must not run: text it captioned or
// extracted would be indexed unredacted.
func newSecretRedactor() (*secretRedactor, error) {
	return processSecretRedactor()
}

// redactMaxMaskedSpans bounds how many merged (overlapping-or-touching)
// spans one maskSecretOccurrences call will build full records for. Below
// this cap, counting how many spans the input needs is cheap (one int32
// counter over the existing coverage sweep), but each span surviving that
// count gets a record in the output: an entry in the marker-name slice, a
// Fprintf call, and findings entries. A single distinct secret value
// repeated throughout a document, with no separator that would merge its
// repeats into one span, produces one span per repeat — so the number of
// merged spans is itself an attacker-controlled count, independent of
// redactMaxDistinctSecrets (which bounds distinct secret VALUES, not how
// many times a single value recurs as separate, non-touching spans).
// Above this many merged spans, Redact withholds the whole input rather
// than build a record per span, for the same fail-closed reason
// redactMaxDistinctSecrets withholds rather than scan unboundedly.
const redactMaxMaskedSpans = 100_000

// redactTooManyMaskedSpansRuleID is the synthetic "rule" reported in
// findings (and named in the output marker) when maskSecretOccurrences
// withholds the whole input because it would need more than
// redactMaxMaskedSpans span records. Never a real gitleaks RuleID, so a
// caller or dashboard can tell this apart from both an ordinary redaction
// and a too-many-distinct-secrets withholding.
const redactTooManyMaskedSpansRuleID = "too-many-masked-spans"

// redactMaxDistinctSecrets bounds how many distinct secret VALUES one
// redactOnePass will attempt to individually locate and mask.
// maskSecretOccurrences locates every occurrence of a distinct secret with
// one strings.Index scan over the whole of s, so its cost is proportional
// to len(s) times the number of distinct secrets — fine for the handful to
// low thousands of distinct secrets an ordinary document carries, but
// unbounded for a document crafted to carry far more (nothing requires a
// gitleaks finding's Secret to be long, so an attacker can make this count
// arbitrarily large relative to len(s)). Above this many distinct secrets,
// Redact does not spend that unbounded work trying to figure out which
// bytes are safe: it withholds the whole input instead of masking it. A
// document that defeats redaction must fail closed, never pass a
// partially-checked text through.
const redactMaxDistinctSecrets = 8192

// redactWithheldRuleID is the synthetic "rule" reported in findings (and
// named in the output marker) when redactOnePass withholds the whole input
// because it exceeds redactMaxDistinctSecrets, rather than masking
// individual secrets. It is never a real gitleaks RuleID, so a caller or
// dashboard keying off specific rule names (e.g. "github-pat") can still
// tell a withheld document apart from an ordinary redaction.
const redactWithheldRuleID = "too-many-distinct-secrets"

// redactMaxPasses bounds how many times Redact re-scans its own output.
// Inserting a "[REDACTED:...]" marker changes the characters immediately
// around it, which can occasionally hand a boundary-sensitive rule (one
// using \b, or requiring a specific terminator character) something it
// needs to match a secret it couldn't on the previous pass — e.g. two
// credentials glued together with no separator, where the first one's
// replacement marker supplies the non-word character the second one's \b
// anchor was missing. Re-scanning converges in one or two passes in
// practice; this cap is a safety valve against pathological input, not a
// mechanism the common case relies on.
const redactMaxPasses = 5

// redactNotConvergedRuleID is the synthetic "rule" reported in findings (and
// named in the output marker) when Redact withholds the whole input because
// a pass after the last one redactMaxPasses allows would still mask
// something. Never a real gitleaks RuleID.
const redactNotConvergedRuleID = "redaction-did-not-converge"

// Redact scans s for secrets and replaces each match with a
// "[REDACTED:<rule-id>]" marker (or "[REDACTED:<rule-id>+<rule-id>]" when
// two rules' matches overlap), returning the redacted string and the
// RuleIDs of every finding actually redacted (for metrics/logging —
// never the secret values themselves).
//
// Each pass masks findings by their byte position in the ORIGINAL input
// to that pass, not by sequentially replacing each finding's Secret in a
// progressively mutated copy. gitleaks evaluates every rule against the
// same text, so two findings' matched spans can overlap — e.g. one rule's
// greedy, boundary-less match running into an adjacent, different
// credential (a Slack bot token immediately followed by a GitHub PAT).
// Replacing sequentially made the result depend on which finding was
// applied first: the first replacement would consume characters the
// second finding's Secret depended on, so the second's string search
// silently matched nothing, leaving a real credential (or a fragment of
// one) in plaintext while still being reported as redacted. Masking by
// position and merging overlapping (or touching) spans before rendering
// is independent of finding order and closes that leak.
//
// The mask is a coverage count over byte positions (maskSecretOccurrences),
// not one allocated span per occurrence: a secret that recurs, or several
// distinct secrets whose occurrences blanket a shared region (e.g. many
// overlapping substrings of one repeated line), can occur far more times
// than there are bytes to hold a record of each one, so memory tracks
// len(s), never the occurrence count. Above redactMaxDistinctSecrets
// distinct secret values, Redact gives up locating individual occurrences
// (that cost is proportional to len(s) times the distinct-secret count, so
// it is itself unbounded for an adversarial document) and withholds the
// whole input instead. Separately, above redactMaxMaskedSpans MERGED spans
// — a count a single repeated secret with no merging separator drives up
// on its own, independent of the distinct-secret count — Redact withholds
// the whole input rather than build a per-span record for each one; each
// span that does get built is a fixed-size record (an interned rule-ID
// bitset, not a map), so neither cap alone, nor the two combined, can make
// per-span cost grow with how large either count is crafted to be.
//
// Redact then re-scans its own output (see redactMaxPasses) until a pass
// finds nothing left to mask, so calling Redact twice is idempotent:
// Redact(Redact(s)) equals Redact(s), including when the caller's own
// pipeline already redacts in two stages (per-page checkpoints, then the
// assembled document). Text that redactMaxPasses passes leave with
// something still to mask is withheld whole (redactNotConvergedRuleID).
//
// Nil-safe: a nil receiver, or one with a nil detector (a test driving a
// worker without Run, which is what builds the redactor and fails when
// it cannot), returns s unchanged with no findings.
func (r *secretRedactor) Redact(s string) (redacted string, findings []string) {
	if r == nil || r.detector == nil {
		return s, nil
	}

	// Each pass only runs against text already stripped of every span the
	// previous pass covered, so a later pass's findings are genuinely new
	// occurrences, never a re-discovery of something already counted —
	// no cross-pass dedup is needed.
	out := s
	for pass := 0; ; pass++ {
		next, passFindings := r.redactOnePass(out)
		if len(passFindings) == 0 {
			return out, findings
		}
		if pass == redactMaxPasses {
			return "[REDACTED:" + redactNotConvergedRuleID + "]", append(findings, redactNotConvergedRuleID)
		}
		findings = append(findings, passFindings...)
		out = next
	}
}

// redactOnePass runs one position-based mask-and-merge pass of Redact
// over s: it detects findings in s, groups them by distinct Secret value,
// and (below redactMaxDistinctSecrets) hands off to
// maskSecretOccurrences to mask every occurrence. It returns s unchanged
// with no findings when nothing matches.
func (r *secretRedactor) redactOnePass(s string) (out string, findings []string) {
	// Group findings by Secret before scanning for occurrences. gitleaks
	// returns one finding per match location, so a value that occurs many
	// times (e.g. the same token repeated down a spreadsheet column)
	// produces that many findings with an identical Secret. Scanning s
	// once per FINDING instead of once per unique Secret made this
	// quadratic: a token repeated k times ran k strings.Index scans, each
	// one walking all of s and (re-)discovering all k occurrences. A
	// multi-thousand-row xlsx or docx table repeating one detected secret
	// could grow this past the pod's memory limit and OOM-kill the leader.
	// Deduping to one scan per unique Secret, merging the rule IDs that
	// share it, makes the scanning linear in the number of distinct
	// secrets rather than the number of findings.
	secretRuleIDs := make(map[string]map[string]struct{})
	for _, f := range r.detector.DetectString(s) {
		if f.Secret == "" {
			// A zero-length secret would make the scan below a no-op at
			// best and loop forever at worst; skip it rather than
			// reason about that edge case.
			continue
		}
		ids, ok := secretRuleIDs[f.Secret]
		if !ok {
			ids = make(map[string]struct{}, 1)
			secretRuleIDs[f.Secret] = ids
		}
		ids[f.RuleID] = struct{}{}
	}
	if len(secretRuleIDs) == 0 {
		return s, nil
	}

	if len(secretRuleIDs) > redactMaxDistinctSecrets {
		// See redactMaxDistinctSecrets: locating every occurrence of this
		// many distinct secrets is itself unbounded work (proportional to
		// len(s) times the distinct-secret count), and a partially-checked
		// text must never reach the caller. Withhold the whole input
		// instead of masking it.
		return "[REDACTED:" + redactWithheldRuleID + "]", []string{redactWithheldRuleID}
	}

	return maskSecretOccurrences(s, secretRuleIDs)
}

// maskSecretOccurrences locates every occurrence of every key of
// secretRuleIDs in s and replaces each one with a "[REDACTED:...]" marker,
// merging occurrences that overlap or touch (whether of the same secret or
// different ones) into a single marker naming every rule involved. It
// returns s unchanged with no findings if no occurrence is found (gitleaks
// found the secret, built secretRuleIDs from it, but strings.Index finds no
// instance of it — only possible if a previous pass's masking already
// consumed it).
//
// It is independent of secretRuleIDs itself answering to gitleaks — it just
// masks whatever strings it is given — specifically so a test can drive it
// directly with a crafted secretRuleIDs map (many distinct, heavily
// overlapping secrets) without needing gitleaks to actually detect
// thousands of realistic-looking credentials.
//
// Memory is proportional to len(s) plus the number of MERGED spans, never
// to the number of occurrences or to (distinct secrets x merged spans):
//
//   - occurrences are marked in a coverage array over byte positions (a
//     difference array: +1 at each occurrence's start, -1 at its end),
//     swept once to find merged (overlapping-or-touching) regions, rather
//     than allocated as one span-plus-rule-ID-map object per occurrence. A
//     secret that recurs, or several distinct secrets whose occurrences
//     blanket a shared region (e.g. many overlapping substrings of one
//     repeated line), can occur far more times than there is input to hold
//     a record of each one; a per-occurrence allocation would make memory
//     grow with the occurrence count instead of with len(s).
//   - the number of MERGED spans is itself counted, from that same coverage
//     sweep, before anything is allocated per span. A single distinct
//     secret repeated throughout s with no separator that merges its
//     repeats produces one span per repeat, so this count is itself
//     attacker-controlled and independent of len(s) and of the number of
//     distinct secrets; above redactMaxMaskedSpans, the whole input is
//     withheld rather than given one record per span (see
//     redactMaxMaskedSpans).
//   - each surviving span's rule-ID set is a fixed-size bitset slot (every
//     span uses the same, precomputed number of words), not a map: which
//     RuleIDs can appear is bounded by gitleaks' own fixed ruleset, never
//     by how many distinct secret VALUES or merged spans the input is
//     crafted to contain, so per-span cost cannot be driven up by either
//     of those two attacker-controlled counts, individually or combined.
func maskSecretOccurrences(s string, secretRuleIDs map[string]map[string]struct{}) (out string, findings []string) {
	// forEachOccurrence calls fn once for every occurrence, in s, of every
	// key of secretRuleIDs — not only the location(s) gitleaks itself
	// reported, matching the old sequential-ReplaceAll's
	// replace-every-occurrence behavior. Finding all occurrences of one
	// secret this way costs one O(len(s)) scan (strings.Index advances by
	// len(secret) past each match, so matches of the SAME secret can never
	// overlap and re-walk the same bytes); called once per distinct
	// secret, total cost is O(len(s) * len(secretRuleIDs)).
	forEachOccurrence := func(fn func(secret string, start, end int)) {
		for secret := range secretRuleIDs {
			for start := 0; ; {
				i := strings.Index(s[start:], secret)
				if i < 0 {
					break
				}
				a := start + i
				b := a + len(secret)
				fn(secret, a, b)
				start = b
			}
		}
	}

	// Intern every RuleID that can possibly appear (the union across all of
	// secretRuleIDs's values) into a small index, assigned in sorted order
	// so a span's bit positions already come out in the marker's sorted
	// name order. This set's size is bounded by gitleaks' own fixed
	// ruleset (a small constant — currently ~150 rules — baked into the
	// embedded config), never by len(secretRuleIDs) (the distinct-secret
	// count, already capped separately at redactMaxDistinctSecrets) or by
	// the merged-span count: many distinct secret VALUES, or many spans,
	// can still only ever carry a handful of distinct RuleIDs between them.
	ruleIDSeen := make(map[string]struct{})
	for _, ids := range secretRuleIDs {
		for id := range ids {
			ruleIDSeen[id] = struct{}{}
		}
	}
	ruleIDNames := make([]string, 0, len(ruleIDSeen))
	for id := range ruleIDSeen {
		ruleIDNames = append(ruleIDNames, id)
	}
	sort.Strings(ruleIDNames)
	ruleIDIndex := make(map[string]int, len(ruleIDNames))
	for i, id := range ruleIDNames {
		ruleIDIndex[id] = i
	}
	numWords := (len(ruleIDNames) + 63) / 64
	if numWords == 0 {
		numWords = 1
	}

	// Precompute each distinct secret's rule-ID bitset once — not once per
	// occurrence — so attaching an occurrence to its span (below) is a
	// fixed number of word-ORs, never a map insert.
	secretBits := make(map[string][]uint64, len(secretRuleIDs))
	for secret, ids := range secretRuleIDs {
		bitset := make([]uint64, numWords)
		for id := range ids {
			i := ruleIDIndex[id]
			bitset[i/64] |= 1 << uint(i%64)
		}
		secretBits[secret] = bitset
	}

	// coverage[p] accumulates +1/-1 at every occurrence's start/end byte
	// offset. A left-to-right sweep of its running sum turns that into
	// merged (overlapping-or-touching) spans without ever allocating per
	// occurrence.
	coverage := make([]int32, len(s)+1)
	found := false
	forEachOccurrence(func(_ string, a, b int) {
		found = true
		coverage[a]++
		coverage[b]--
	})
	if !found {
		return s, nil
	}

	// Count merged spans before allocating anything per span. This sweep
	// costs nothing beyond the coverage array itself (one int32 counter),
	// so the cap below is checked before any per-span cost — map, slice,
	// bitset, or otherwise — is paid.
	numSpans := 0
	var depth int32
	for pos := 0; pos <= len(s); pos++ {
		prev := depth
		depth += coverage[pos]
		if prev == 0 && depth > 0 {
			numSpans++
		}
	}
	if numSpans > redactMaxMaskedSpans {
		// See redactMaxMaskedSpans: building a record for this many merged
		// spans is itself unbounded work family (driven by how many times
		// one secret recurs without merging, independent of the
		// distinct-secret cap above), and a partially-checked text must
		// never reach the caller. Withhold the whole input instead of
		// masking it.
		return "[REDACTED:" + redactTooManyMaskedSpansRuleID + "]", []string{redactTooManyMaskedSpansRuleID}
	}

	// starts/ends hold each merged span's byte range as two plain int
	// slices (no map, no per-span struct) — the sweep is deterministic and
	// left-to-right, so both are already sorted by construction.
	starts := make([]int, 0, numSpans)
	ends := make([]int, numSpans)
	depth = 0
	for pos := 0; pos <= len(s); pos++ {
		prev := depth
		depth += coverage[pos]
		switch {
		case prev == 0 && depth > 0:
			starts = append(starts, pos)
		case prev > 0 && depth == 0:
			ends[len(starts)-1] = pos
		}
	}
	coverage = nil // let the GC reclaim it before the output builder grows

	// spanBits is ONE allocation holding every span's rule-ID bitset back
	// to back (span i occupies spanBits[i*numWords : (i+1)*numWords]),
	// rather than one slice (or map) per span: per-span cost is the same
	// fixed numWords regardless of how many distinct secrets or merged
	// spans the input contains.
	spanBits := make([]uint64, numSpans*numWords)

	// Attach each occurrence's rule-ID bitset to the merged span it falls
	// in. starts/ends are sorted by construction, so the span covering
	// offset a is found by binary search: the first span whose end is past
	// a. OR-ing an already-set bitset into a span is idempotent, so this
	// stays fixed-size per occurrence, never growing with occurrence
	// count.
	forEachOccurrence(func(secret string, a, _ int) {
		i := sort.Search(len(starts), func(i int) bool { return ends[i] > a })
		base := i * numWords
		for w, bit := range secretBits[secret] {
			spanBits[base+w] |= bit
		}
	})

	// redactMaxFindings bounds the total number of rule-ID entries Redact
	// returns, independent of redactMaxMaskedSpans: a span can carry more
	// than one RuleID (two different credentials overlapping), so the
	// total findings count is (merged spans) x (rule IDs per span), which
	// numSpans alone does not bound. Masking itself (the loop below) is
	// unaffected — every span is still replaced — only the returned
	// findings slice (and so the log line and metric built from it) stops
	// growing past this cap.
	const redactMaxFindings = redactMaxMaskedSpans

	var b strings.Builder
	prev := 0
	for i, start := range starts {
		end := ends[i]
		b.WriteString(s[prev:start])

		var ids []string
		base := i * numWords
		for w := 0; w < numWords; w++ {
			word := spanBits[base+w]
			for word != 0 {
				bit := bits.TrailingZeros64(word)
				ids = append(ids, ruleIDNames[w*64+bit])
				word &= word - 1
			}
		}
		// ids is already in sorted order: bit position was assigned from
		// ruleIDNames in sorted order, and the scan above walks bits from
		// low to high.
		fmt.Fprintf(&b, "[REDACTED:%s]", strings.Join(ids, "+"))
		if len(findings) < redactMaxFindings {
			room := redactMaxFindings - len(findings)
			if room < len(ids) {
				ids = ids[:room]
			}
			findings = append(findings, ids...)
		}
		prev = end
	}
	b.WriteString(s[prev:])

	return b.String(), findings
}
