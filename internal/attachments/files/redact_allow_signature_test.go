package files

import (
	"strings"
	"testing"
)

// The detector's scanner honours an inline marker that tells it to skip a
// line. That is for a repository's own reviewed test fixtures; text from a
// screenshot or a shared document gets no such say.
func TestRedactIgnoresTheInlineAllowMarker(t *testing.T) {
	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}
	for _, kind := range redactPropertySecretKinds {
		text := "export TOKEN=" + kind.secret + " # " + "gitleaks" + ":allow\nnext line"
		redacted, findings := r.Redact(text)
		if strings.Contains(redacted, kind.secret) {
			t.Errorf("%s: secret survived beside the marker: %q", kind.ruleID, redacted)
		}
		if len(findings) == 0 {
			t.Errorf("%s: no findings reported", kind.ruleID)
		}
	}
}
