package sessioningest

import "testing"

func TestDefaultPolicy_AdmitsHumanPrimary(t *testing.T) {
	p := DefaultPolicy()
	if !p.admit("human", "primary", "codex", "codex", "alex@example.com") {
		t.Error("interactive human primary codex session should be admitted")
	}
}

func TestDefaultPolicy_ExcludesDiderotsOwnApp(t *testing.T) {
	p := DefaultPolicy()
	if p.admit("human", "primary", "codex", "diderot", "alex@example.com") {
		t.Error("excluded diderot app should not be admitted")
	}
	if len(p.Exclude.Principals) != 0 {
		t.Errorf("the default policy excludes principals %v; it should name none", p.Exclude.Principals)
	}
}

func TestDefaultPolicy_ExcludesNonHumanOwnerKind(t *testing.T) {
	p := DefaultPolicy()
	if p.admit("service", "primary", "codex", "codex", "svc:foo") {
		t.Error("non-human owner_kind should be excluded")
	}
	if p.admit("human", "rider", "codex", "codex", "alex@example.com") {
		t.Error("non-primary role should be excluded")
	}
}

func TestPolicyHash_OrderIndependent(t *testing.T) {
	a := DefaultPolicy()
	a.Include.OwnerKinds = []string{"human", "service"}
	a.Include.Roles = []string{"primary", "rider"}
	b := DefaultPolicy()
	b.Include.OwnerKinds = []string{"service", "human"} // same set, reversed
	b.Include.Roles = []string{"rider", "primary"}
	if a.Hash() != b.Hash() {
		t.Error("policy hash should be order-independent across a set reorder")
	}
}

func TestPolicyValidate_Blank200(t *testing.T) {
	p := DefaultPolicy()
	p.Include.OwnerKinds = []string{""}
	if err := p.Validate(); err == nil {
		t.Error("expected validation error on empty ownerKinds entry")
	}
}
