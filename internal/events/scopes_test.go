package events

import (
	"slices"
	"testing"
)

func TestScopesAlwaysExcludeMissingAndUnstamped(t *testing.T) {
	for _, scopes := range [][]string{nil, {}, {"", ScopeUnstamped}, {ScopePublic, ScopeZendesk, "", ScopeUnstamped}} {
		if ScopeAllowed("", scopes) || ScopeAllowed(ScopeUnstamped, scopes) {
			t.Fatal("a sentinel scope was granted")
		}
		readable := ReadableScopes(scopes)
		if slices.Contains(readable, "") || slices.Contains(readable, ScopeUnstamped) {
			t.Fatalf("invalid readable scopes %v", readable)
		}
	}
	if ScopeAllowed(ScopeZendesk, nil) || !ScopeAllowed(ScopeZendesk, []string{ScopeZendesk}) {
		t.Fatal("protected scope must be explicitly granted")
	}
}
