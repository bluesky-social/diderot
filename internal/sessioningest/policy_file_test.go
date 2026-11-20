package sessioningest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/events"
)

func TestLoadPolicyFile_ParsesAndFillsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	content := `
sessionIngest:
  include:
    ownerKinds: [human]
    roles: [primary]
  exclude:
    apps: [diderot]
    principals: [svc:build-bot]
  ownerOnly:
    principals: [alex@example.com]
`[1:]
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _, err := LoadPolicyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !p.admit("human", "primary", "codex", "codex", "alex@example.com") {
		t.Error("human primary should be admitted")
	}
	if p.admit("service", "primary", "codex", "codex", "svc:foo") {
		t.Error("non-human should be excluded")
	}
	if p.admit("human", "primary", "codex", "codex", "svc:build-bot") {
		t.Error("an excluded principal should not be admitted")
	}
	if !p.admit("human", "primary", "claude-code", "claude-code", "a@x") {
		t.Error("new harness not in include.clients must be admitted by default (empty clients list)")
	}
	if got := p.VisibilityScope("alex@example.com"); got != events.ScopeOwner("alex@example.com") {
		t.Errorf("owner-only YAML scope = %q", got)
	}
}

// TestLoadPolicyFile_RefusesSummarizeBlock: when sessions are summarized is
// set in the admin UI, so a file that still says is refused, with where the
// setting went, instead of being read without it.
func TestLoadPolicyFile_RefusesSummarizeBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	content := `
sessionIngest:
  include:
    ownerKinds: [human]
  summarize:
    quietFor: 5m
`[1:]
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadPolicyFile(path)
	if err == nil || !strings.Contains(err.Error(), "summarize block is no longer read") ||
		!strings.Contains(err.Error(), "/admin/operations/jobs") {
		t.Fatalf("LoadPolicyFile = %v, want the summarize block refused", err)
	}
}

func TestLoadPolicyFile_InvalidYAMLFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte("include: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadPolicyFile(path); err == nil {
		t.Fatal("malformed YAML should fail parse")
	}
}

func TestLoadPolicyFile_MissingRootFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte("include:\n  ownerKinds: [human]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadPolicyFile(path); err == nil {
		t.Fatal("policy without sessionIngest root must fail")
	}
}

func TestPolicyHashDoesNotMutatePolicy(t *testing.T) {
	p := DefaultPolicy()
	p.Include.OwnerKinds = []string{"service", "human"}
	before := append([]string(nil), p.Include.OwnerKinds...)
	_ = p.Hash()
	for i := range before {
		if p.Include.OwnerKinds[i] != before[i] {
			t.Fatalf("Hash mutated policy: got %v want %v", p.Include.OwnerKinds, before)
		}
	}
}

func TestOwnerOnlyPolicyScopesExactAgentOwners(t *testing.T) {
	p := DefaultPolicy()
	p.OwnerOnly.Principals = []string{"alex@example.com"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := p.VisibilityScope(" Alex@Example.com "); got != events.ScopeOwner("alex@example.com") {
		t.Fatalf("selected owner scope = %q", got)
	}
	if got := p.VisibilityScope("someone@example.com"); got != events.ScopeInternal {
		t.Fatalf("unselected owner scope = %q", got)
	}
}

func TestOwnerOnlyPolicyRejectsNonCanonicalAndDuplicatePrincipals(t *testing.T) {
	for _, principals := range [][]string{
		{""},
		{" Alex@example.com"},
		{"Alex@example.com"},
		{"alex@example.com", "alex@example.com"},
	} {
		p := DefaultPolicy()
		p.OwnerOnly.Principals = principals
		if err := p.Validate(); err == nil {
			t.Fatalf("Validate(%q) succeeded", principals)
		}
	}
}

func TestOwnerOnlyPolicyAffectsHashOrderIndependently(t *testing.T) {
	a := DefaultPolicy()
	a.OwnerOnly.Principals = []string{"a@x", "b@x"}
	b := DefaultPolicy()
	b.OwnerOnly.Principals = []string{"b@x", "a@x"}
	if a.Hash() != b.Hash() {
		t.Fatal("owner-only principal reorder changed policy hash")
	}
	b.OwnerOnly.Principals = []string{"b@x"}
	if a.Hash() == b.Hash() {
		t.Fatal("owner-only policy change did not change policy hash")
	}
}
