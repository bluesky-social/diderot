package files

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// redactPropertySecretKinds are literal, synthetic strings that match real
// default-ruleset gitleaks rules end-to-end (not shaped approximations —
// each one is independently confirmed detected below). They were chosen to
// cover several different rule shapes (SecretGroup 0 with no capture group,
// a capturing group, a URL-shaped rule, a high-entropy suffix rule) and to
// include the exact pair (slack-bot-token, github-pat) whose overlapping
// matches were the original leak. None of these are real credentials.
var redactPropertySecretKinds = []struct {
	ruleID string
	secret string
}{
	{"slack-bot-token", "xoxb-" + "123456789012-123456789012-abcdefghijklmnopqrstuvwx"},
	{"github-pat", "ghp_" + "0123456789abcdef" + "0123456789abcdef0123"},
	{"slack-webhook-url", "hooks.slack.com/services/" + "T" + "00000000/" + "B" + "00000000/" + "XXXXXXXXXXXXXXXXXXXXXXXX"},
	{"jwt", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." + "eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"},
	{"pypi-upload-token", "pypi-" + "AgEIcHlwaS5vcmc7KpLsNq2XvB9fTmZ4jR0cYdAeGhU1QoW3EiF6tSbVnOyPx58rC"},
}

// redactPropertyJoiners are the separators placed between planted secrets.
// "" is included deliberately: it is what makes two findings' matched spans
// collide, and some joiners (-, _, /, +) let a rule's boundary-less tail
// run into the next token.
var redactPropertyJoiners = []string{"", "-", "_", "/", "+", " ", "\n", "=", ":", ",", "."}

// TestRedactPropertyNoPlantedSecretSurvives is a property test, not a
// targeted regression: it generates many texts that plant 2-5 of the
// synthetic secrets above, in random order, at random positions, repeated,
// and joined so that their matched spans are adjacent or overlapping (the
// "" joiner in particular produces colliding spans for
// every pair of kinds, not just slack+github). For each generated text it
// checks two invariants that must hold for EVERY input, not just the
// handful the deterministic regression test above pins down:
//
//  1. No value gitleaks itself reports as a finding on the original text
//     survives verbatim in Redact's output. The oracle is gitleaks'
//     DetectString on the un-redacted text, called independently of
//     Redact's own internal call — the match VALUES a regex produces
//     depend only on the regex and the text, never on the order other
//     rules are evaluated in, so this is a trustworthy ground truth even
//     though gitleaks' own rule-evaluation order is randomized.
//  2. Redacting already-redacted output is a no-op: Redact(Redact(s)) ==
//     Redact(s). A marker must not itself look like a new secret to the
//     detector, and merged spans must not produce fresh adjacency that
//     trips a rule on the second pass.
func TestRedactPropertyNoPlantedSecretSurvives(t *testing.T) {
	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}

	// Fixed seed: deterministic and reproducible, not flaky, while still
	// exploring far more shapes than the handwritten regression test.
	rng := rand.New(rand.NewSource(20261119))

	const iterations = 300
	for i := 0; i < iterations; i++ {
		n := 2 + rng.Intn(4) // 2..5 planted secrets per generated text
		var sb strings.Builder
		for j := 0; j < n; j++ {
			if j > 0 {
				sb.WriteString(redactPropertyJoiners[rng.Intn(len(redactPropertyJoiners))])
			}
			sb.WriteString(redactPropertySecretKinds[rng.Intn(len(redactPropertySecretKinds))].secret)
		}
		text := sb.String()

		redacted, findings := r.Redact(text)

		// Ground truth: whatever gitleaks itself finds in the ORIGINAL
		// text must not appear verbatim in the redacted output, for
		// every finding gitleaks reports — regardless of how many other
		// findings overlap it.
		for _, f := range r.detector.DetectString(text) {
			if f.Secret == "" {
				continue
			}
			if strings.Contains(redacted, f.Secret) {
				t.Fatalf("iteration %d: detected secret for rule %q still present after Redact\n"+
					"input:    %q\nredacted: %q\nfindings: %v",
					i, f.RuleID, text, redacted, findings)
			}
		}

		// Idempotence: a second pass over already-redacted text changes
		// nothing.
		redactedAgain, findingsAgain := r.Redact(redacted)
		if redactedAgain != redacted {
			t.Fatalf("iteration %d: redacting twice != redacting once\n"+
				"input:           %q\nredacted once:   %q\nredacted twice:  %q",
				i, text, redacted, redactedAgain)
		}
		if len(findingsAgain) != 0 {
			t.Fatalf("iteration %d: second Redact pass reported new findings %v on already-redacted text %q",
				i, findingsAgain, redacted)
		}
	}
}

// TestRedactPropertyEveryKindIsActuallyDetected guards the property test's
// own fixture: if a future gitleaks/config bump changes one of these rules'
// regex so a planted literal no longer matches, the property test above
// would silently stop exercising that rule shape instead of failing loudly.
func TestRedactPropertyEveryKindIsActuallyDetected(t *testing.T) {
	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}
	for _, k := range redactPropertySecretKinds {
		findings := r.detector.DetectString(k.secret)
		got := ruleIDs(findings)
		if !contains(got, k.ruleID) {
			t.Errorf("fixture secret for %q is no longer detected as that rule; findings=%v (%s)",
				k.ruleID, got, fmt.Sprintf("secret=%q", k.secret))
		}
	}
}
