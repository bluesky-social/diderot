package sessiontitle

import (
	"errors"
	"testing"
)

// A title and a title error are written to Postgres text columns, which
// reject a NUL byte and invalid UTF-8.
func TestTitleAndErrorTextAreStorable(t *testing.T) {
	if got := Clean("Fix the\x00 parser\xff bug."); got != "Fix the parser� bug" {
		t.Errorf("Clean = %q", got)
	}
	if got := Fallback("why does\x00 this fail"); got != "why does this fail" {
		t.Errorf("Fallback = %q", got)
	}
	if got := boundedError(errors.New("gateway said: \x00\xff")); got != "gateway said: �" {
		t.Errorf("boundedError = %q", got)
	}
}
