package authorization

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

const testPolicy = `authorization:
  grants:
    - principals:
        - group:support-team
        - slack-channel:C0EXAMPLE02
      permissions:
        - action: retrieve
          resource: corpus:zendesk
`

func TestResolve(t *testing.T) {
	authorizer, err := Parse([]byte(testPolicy))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		principals []Principal
		allowed    bool
	}{
		{name: "support group", principals: []Principal{GroupPrincipal("support-team")}, allowed: true},
		{name: "approved channel", principals: []Principal{ChannelPrincipal("C0EXAMPLE02")}, allowed: true},
		{name: "any principal", principals: []Principal{GroupPrincipal("everyone"), GroupPrincipal("support-team")}, allowed: true},
		{name: "other channel", principals: []Principal{ChannelPrincipal("COTHER")}},
		{name: "other group", principals: []Principal{GroupPrincipal("everyone")}},
		{name: "group prefix", principals: []Principal{GroupPrincipal("support-team-extra")}},
		{name: "group case", principals: []Principal{GroupPrincipal("Support-team")}},
		{name: "eval without grant", principals: []Principal{MetricEval}},
		{name: "missing identity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := authorizer.Resolve(test.principals)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Allows(Retrieve, ZendeskCorpus) != test.allowed {
				t.Fatalf("unexpected decision: %+v", decision.Permissions())
			}
			if slices.Contains(decision.CorpusScopes(), string(ZendeskCorpus)) != test.allowed {
				t.Fatalf("unexpected scopes: %v", decision.CorpusScopes())
			}
			if decision.Allows("read", ZendeskCorpus) || decision.Allows(Retrieve, "connector:zendesk") || decision.Allows(Retrieve, "corpus:other") {
				t.Fatal("decision widened the action or resource")
			}
		})
	}
}

func TestIndependentPermissionsAndDuplicateGrants(t *testing.T) {
	retrieve := Permission{Action: Retrieve, Resource: ZendeskCorpus}
	other := Permission{Action: Retrieve, Resource: "corpus:other"}
	group := GroupPrincipal("support-team")
	channel := ChannelPrincipal("C0EXAMPLE02")
	authorizer, err := New(Config{Grants: []Grant{
		{Principals: []Principal{group, group}, Permissions: []Permission{retrieve, retrieve}},
		{Principals: []Principal{group}, Permissions: []Permission{retrieve}},
		{Principals: []Principal{channel}, Permissions: []Permission{other}},
		{Principals: []Principal{MetricEval}, Permissions: []Permission{retrieve}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, principal := range []Principal{group, channel, MetricEval} {
		decision, err := authorizer.Resolve([]Principal{principal, principal})
		if err != nil {
			t.Fatal(err)
		}
		if len(decision.Permissions()) != 1 {
			t.Fatalf("%s: duplicate permissions: %+v", principal, decision.Permissions())
		}
		if decision.Allows(Retrieve, ZendeskCorpus) != (principal != channel) || decision.Allows(Retrieve, "corpus:other") != (principal == channel) {
			t.Fatalf("%s: permissions were not independent", principal)
		}
	}
}

func TestEmptyPolicyAndNilAuthorizerDeny(t *testing.T) {
	empty, err := Parse([]byte("authorization: {grants: []}"))
	if err != nil {
		t.Fatal(err)
	}
	defaultPolicy, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, authorizer := range []*Authorizer{nil, {}, empty, defaultPolicy} {
		decision, err := authorizer.Resolve([]Principal{GroupPrincipal("support-team")})
		if err != nil || len(decision.Permissions()) != 0 || len(decision.CorpusScopes()) != 0 {
			t.Fatalf("empty policy granted access: %+v, %v", decision, err)
		}
	}
}

func TestLoadPolicyFile(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "authorization.yaml")
	if err := os.WriteFile(filename, []byte(testPolicy), 0600); err != nil {
		t.Fatal(err)
	}
	authorizer, err := Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := authorizer.Resolve([]Principal{ChannelPrincipal("C0EXAMPLE02")})
	if err != nil || !decision.Allows(Retrieve, ZendeskCorpus) {
		t.Fatalf("unexpected loaded policy: %+v, %v", decision.Permissions(), err)
	}
	if _, err := Load(filename + ".missing"); err == nil {
		t.Fatal("missing configured policy file was ignored")
	}
}

func TestInvalidPolicy(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "empty file"},
		{name: "missing block", data: "{}"},
		{name: "unknown field", data: strings.ReplaceAll(testPolicy, "principals:", "principal:")},
		{name: "extra document", data: testPolicy + "\n---\nauthorization: {}\n"},
		{name: "malformed yaml", data: "authorization: ["},
		{name: "empty grant", data: "authorization: {grants: [{}]}"},
		{name: "wildcard principal", data: strings.ReplaceAll(testPolicy, "support-team", "*")},
		{name: "wildcard resource", data: strings.ReplaceAll(testPolicy, "corpus:zendesk", "corpus:*")},
		{name: "unstamped resource", data: strings.ReplaceAll(testPolicy, "corpus:zendesk", "unstamped")},
		{name: "legacy resource", data: strings.ReplaceAll(testPolicy, "corpus:zendesk", "internal")},
		{name: "write action", data: strings.ReplaceAll(testPolicy, "action: retrieve", "action: write")},
		{name: "wrong resource kind", data: strings.ReplaceAll(testPolicy, "corpus:zendesk", "connector:zendesk")},
		{name: "unknown principal kind", data: strings.ReplaceAll(testPolicy, "group:", "untrusted:")},
		// A provider-specific spelling has no alias: such a policy fails
		// to load instead of silently granting nothing.
		{name: "provider-specific group prefix", data: strings.ReplaceAll(testPolicy, "group:", "idp-group:")},
		{name: "channel name", data: strings.ReplaceAll(testPolicy, "C0EXAMPLE02", "support-channel")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse([]byte(test.data)); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
}

func TestDecisionDoesNotExposeMutableState(t *testing.T) {
	authorizer, err := Parse([]byte(testPolicy))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := authorizer.Resolve([]Principal{GroupPrincipal("support-team")})
	if err != nil {
		t.Fatal(err)
	}
	permissions := decision.Permissions()
	permissions[0] = Permission{Action: "read", Resource: "connector:other"}
	scopes := decision.CorpusScopes()
	scopes[0] = "unstamped"
	if decision.Allows("read", "connector:other") || !slices.Equal(decision.CorpusScopes(), []string{"corpus:zendesk"}) {
		t.Fatal("callers mutated the decision")
	}
}

type failingEnforcer struct {
	calls int
}

func (enforcer *failingEnforcer) Enforce(...interface{}) (bool, error) {
	enforcer.calls++
	if enforcer.calls == 1 {
		return true, nil
	}
	return false, errors.New("evaluation failed")
}

func TestEvaluationErrorDiscardsPartialDecision(t *testing.T) {
	authorizer := &Authorizer{
		enforcer: &failingEnforcer{},
		permissions: []Permission{
			{Action: Retrieve, Resource: ZendeskCorpus},
			{Action: Retrieve, Resource: "corpus:other"},
		},
	}
	decision, err := authorizer.Resolve([]Principal{GroupPrincipal("support-team")})
	if err == nil || len(decision.Permissions()) != 0 || len(decision.CorpusScopes()) != 0 {
		t.Fatalf("evaluation failure retained a partial grant: %+v, %v", decision, err)
	}
}

func TestConcurrentResolution(t *testing.T) {
	authorizer, err := Parse([]byte(testPolicy))
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := range 12 {
		workers.Go(func() {
			for range 40 {
				decision, err := authorizer.Resolve([]Principal{GroupPrincipal("support-team")})
				if err != nil || !decision.Allows(Retrieve, ZendeskCorpus) {
					t.Error(fmt.Errorf("worker %d: unexpected decision %+v: %v", worker, decision.Permissions(), err))
					return
				}
			}
		})
	}
	workers.Wait()
}
