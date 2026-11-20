package files

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// chainedToken is a synthetic value shaped like an OpenShift user token
// whose last character is '-'. That rule needs a character outside
// [\w-] (or the end of the text) after the token, so in a run of these
// with nothing between them only the last one matches; masking it puts a
// '[' after the one before, which then matches on the next pass. Each
// pass of Redact therefore masks exactly one of them.
func chainedToken(i int) string {
	return fmt.Sprintf("sha256~Qx7Lm2Zp9Vt4Kd8Rw1Hs6Nb3Yc5Jf0Ga7Ue2Xo%04d-", i)
}

func chainedTokens(n int) (text string, tokens []string) {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		token := chainedToken(i)
		tokens = append(tokens, token)
		sb.WriteString(token)
	}
	return sb.String(), tokens
}

func TestRedactNeverReturnsTextAnotherPassWouldChange(t *testing.T) {
	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}
	if got := r.detector.DetectString(chainedToken(0)); len(got) != 1 {
		t.Fatalf("the synthetic token must be detected on its own, got %d findings", len(got))
	}

	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 4*redactMaxPasses).Draw(t, "tokens")
		text, tokens := chainedTokens(n)
		prefix := rapid.SampledFrom([]string{"", "see ", "token: ", "a\n"}).Draw(t, "prefix")

		redacted, findings := r.Redact(prefix + text)

		for _, token := range tokens {
			if strings.Contains(redacted, strings.TrimSuffix(token, "-")) {
				t.Fatalf("%d chained tokens: one is still in the output: %q", n, redacted)
			}
		}
		if _, more := r.redactOnePass(redacted); len(more) > 0 {
			t.Fatalf("%d chained tokens: another pass still finds %v in %q", n, more, redacted)
		}
		if len(findings) == 0 {
			t.Fatalf("%d chained tokens: no findings reported", n)
		}
	})
}

func TestRedactWithholdsTextThatDoesNotConverge(t *testing.T) {
	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}

	// One pass per token: within the cap this is masked token by token.
	text, _ := chainedTokens(redactMaxPasses)
	redacted, findings := r.Redact(text)
	if want := strings.Repeat("[REDACTED:openshift-user-token]", redactMaxPasses); redacted != want {
		t.Fatalf("within the cap: got %q, want %q", redacted, want)
	}
	if len(findings) != redactMaxPasses {
		t.Fatalf("within the cap: got %d findings, want %d", len(findings), redactMaxPasses)
	}

	// One more token than passes: the whole text is withheld.
	text, _ = chainedTokens(redactMaxPasses + 1)
	redacted, findings = r.Redact("keep this " + text)
	if want := "[REDACTED:" + redactNotConvergedRuleID + "]"; redacted != want {
		t.Fatalf("past the cap: got %q, want %q", redacted, want)
	}
	if len(findings) == 0 || findings[len(findings)-1] != redactNotConvergedRuleID {
		t.Fatalf("past the cap: findings %v must end with %q", findings, redactNotConvergedRuleID)
	}
}
