package main

import "testing"

func TestSessionDevCommandAndPersona(t *testing.T) {
	command := sessionDevCmd()
	if command.Use != "session-dev" {
		t.Fatalf("Use = %q", command.Use)
	}
	for _, name := range []string{"bundle", "db-url", "listen", "session-viewer-url", "as", "runtime-dir", "qdrant-addr", "qdrant-collection"} {
		if command.Flag(name) == nil {
			t.Errorf("missing --%s", name)
		}
	}
	if got, err := selectSessionDevPersona([]string{"a@example.invalid"}, ""); err != nil || got != "a@example.invalid" {
		t.Fatalf("default persona = %q, %v", got, err)
	}
	if _, err := selectSessionDevPersona([]string{"a@example.invalid"}, "b@example.invalid"); err == nil {
		t.Fatal("undeclared persona accepted")
	}
}
