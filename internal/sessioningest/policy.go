// Policy is the validated diderot-side session-ingest policy.
//
// It mirrors the `sessionIngest:` block of the policy file; includes are
// ANDed, and any exact-match exclusion wins.
//
//   - include.ownerKinds: accepted owner_kind values ("human", ...).
//     Empty = accept any owner_kind.
//   - include.roles: accepted role values ("primary", ...). Empty =
//     accept any role.
//   - include.clients: accepted exact harness client ids. Empty by
//     default so a new human harness is not silently omitted.
//   - exclude.apps: exact origin_app values to exclude (diderot's own
//     service traffic is excluded here — defense in depth).
//   - exclude.principals: exact owner/principal identifiers to
//     exclude (e.g. "svc:build-bot").
//   - exclude.clients: exact client ids to exclude.
//   - ownerOnly.principals: canonical human-owner emails whose agent
//     sessions remain indexed but are visible only on that owner's scope.
//
// The policy is loaded from a mounted YAML file
// (DIDEROT_SESSION_INGEST_POLICY_PATH); the file's effective content
// drives policy hash + reconciliation. Environment overrides
// (codex-app include, an excluded app) layer on top only when no YAML is
// mounted.
package sessioningest

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/diderot/internal/events"
	"gopkg.in/yaml.v3"
)

// Policy is the validated include/exclude policy.
type Policy struct {
	Include   PolicyInclude   `yaml:"include" json:"include"`
	Exclude   PolicyExclude   `yaml:"exclude" json:"exclude"`
	OwnerOnly PolicyOwnerOnly `yaml:"ownerOnly" json:"ownerOnly"`
}

// PolicyOwnerOnly is the exact set of canonical session owners whose
// admitted agent sessions are restricted to their single-reader owner scope.
// This is visibility policy, not exclusion: summaries and chunks remain in
// the corpus and become available when a verified owner surface grants the
// matching owner:<email> scope.
type PolicyOwnerOnly struct {
	Principals []string `yaml:"principals" json:"principals"`
}

// PolicyInclude is the ANDed include list. An empty slice for a field
// means "no filtering on that dimension".
type PolicyInclude struct {
	OwnerKinds []string `yaml:"ownerKinds" json:"ownerKinds"`
	Roles      []string `yaml:"roles" json:"roles"`
	Clients    []string `yaml:"clients" json:"clients"`
}

// PolicyExclude is the exact-match exclusion list.
type PolicyExclude struct {
	Apps       []string `yaml:"apps" json:"apps"`
	Principals []string `yaml:"principals" json:"principals"`
	Clients    []string `yaml:"clients" json:"clients"`
	SessionIDs []string `yaml:"sessionIDs" json:"sessionIDs,omitempty"`
}

// SummarizePolicy is when a session is summarized: the thresholds an
// administrator sets on the Jobs and maintenance page. It is not part of
// Policy, so changing it never re-checks the sessions already seen.
type SummarizePolicy struct {
	QuietFor             time.Duration
	MinAddedUserTurns    int
	MinAddedRecords      int
	MinAddedContentBytes int64
	MaxPendingFor        time.Duration
}

// DefaultSummarizePolicy summarizes a session after 10m quiet or 6h pending,
// and again once it has grown by 2 user turns, 12 records or 32 KiB.
func DefaultSummarizePolicy() SummarizePolicy {
	return SummarizePolicy{
		QuietFor:             10 * time.Minute,
		MinAddedUserTurns:    2,
		MinAddedRecords:      12,
		MinAddedContentBytes: 32768,
		MaxPendingFor:        6 * time.Hour,
	}
}

// DefaultPolicy is the policy used when no policy file is configured:
// include human primaries, and exclude Diderot's own app so it never ingests
// its own sessions. It names no principal: a deployment whose Diderot runs
// its models as a service principal of its own excludes that principal in a
// policy file.
func DefaultPolicy() Policy {
	return Policy{
		Include: PolicyInclude{
			OwnerKinds: []string{"human"},
			Roles:      []string{"primary"},
			Clients:    nil,
		},
		Exclude: PolicyExclude{
			Apps:       []string{"diderot"},
			Principals: nil,
			Clients:    nil,
		},
	}
}

// Validate checks the policy for internal consistency.
func (p Policy) Validate() error {
	for _, sessionID := range p.Exclude.SessionIDs {
		parsed, err := uuid.Parse(sessionID)
		if err != nil || parsed.String() != sessionID {
			return fmt.Errorf("sessioningest: exclude.sessionIDs value %q must be a canonical UUID", sessionID)
		}
	}
	for _, k := range p.Include.OwnerKinds {
		if k == "" {
			return fmt.Errorf("sessioningest: include.ownerKinds contains an empty value")
		}
	}
	for _, r := range p.Include.Roles {
		if r == "" {
			return fmt.Errorf("sessioningest: include.roles contains an empty value")
		}
	}
	seenOwnerOnly := make(map[string]struct{}, len(p.OwnerOnly.Principals))
	for _, principal := range p.OwnerOnly.Principals {
		if principal == "" || strings.TrimSpace(principal) != principal || strings.ToLower(principal) != principal {
			return fmt.Errorf("sessioningest: ownerOnly.principals value %q must be a non-empty canonical lowercase owner", principal)
		}
		if _, exists := seenOwnerOnly[principal]; exists {
			return fmt.Errorf("sessioningest: ownerOnly.principals contains duplicate %q", principal)
		}
		seenOwnerOnly[principal] = struct{}{}
	}
	return nil
}

// LoadPolicyFile reads + parses + validates a mounted policy YAML.
// On any parse/validation error it returns a descriptive error (the
// caller disables the worker rather than silently falling back to a
// policy the operator did not author). Returns the parsed policy and
// the file's raw bytes (used for hashing / change detection).
func LoadPolicyFile(path string) (Policy, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, nil, fmt.Errorf("sessioningest: read policy %q: %w", path, err)
	}
	if err := checkPolicyKeys(raw); err != nil {
		return Policy{}, nil, fmt.Errorf("sessioningest: parse policy %q: %w", path, err)
	}
	var holder struct {
		SessionIngest Policy `yaml:"sessionIngest"`
	}
	if err := yaml.Unmarshal(raw, &holder); err != nil {
		return Policy{}, nil, fmt.Errorf("sessioningest: parse policy %q: %w", path, err)
	}
	p := holder.SessionIngest
	if err := p.Validate(); err != nil {
		return Policy{}, nil, err
	}
	return p, raw, nil
}

// checkPolicyKeys requires the file's `sessionIngest` root and refuses a
// `summarize` block under it: when sessions are summarized is set on the
// Jobs and maintenance page, so a file that still carries the block is
// refused rather than read without it. An empty file has nothing to check.
func checkPolicyKeys(raw []byte) error {
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return err
	}
	root := node.Content
	if len(root) == 0 {
		return nil
	}
	var sessionIngest *yaml.Node
	for i := 0; i+1 < len(root[0].Content); i += 2 {
		if root[0].Content[i].Value == "sessionIngest" {
			sessionIngest = root[0].Content[i+1]
		}
	}
	if sessionIngest == nil {
		return fmt.Errorf("required sessionIngest root is missing")
	}
	for i := 0; i+1 < len(sessionIngest.Content); i += 2 {
		if sessionIngest.Content[i].Value == "summarize" {
			return fmt.Errorf("the summarize block is no longer read: remove it, and set when sessions are summarized at /admin/operations/jobs")
		}
	}
	return nil
}

// admit reports whether a session descriptor passes the policy.
func (p Policy) admit(ownerKinds, role, client, originApp, owner string) bool {
	if len(p.Include.OwnerKinds) > 0 && !strIn(p.Include.OwnerKinds, ownerKinds) {
		return false
	}
	if len(p.Include.Roles) > 0 && !strIn(p.Include.Roles, role) {
		return false
	}
	if len(p.Include.Clients) > 0 && !strIn(p.Include.Clients, client) {
		return false
	}
	if strIn(p.Exclude.Apps, originApp) {
		return false
	}
	if strIn(p.Exclude.Principals, owner) {
		return false
	}
	if strIn(p.Exclude.Clients, client) {
		return false
	}
	return true
}

// AdmitChange is a SessionChange-aware admission helper used
// by the store's UpsertChanges reconciliation. It returns whether the
// change should remain active (admitted + not soft-deleted).
func (p Policy) AdmitChange(sessionID, ownerKinds, role, client, originApp, owner string, deleted bool) bool {
	if deleted || strIn(p.Exclude.SessionIDs, sessionID) {
		return false
	}
	return p.admit(ownerKinds, role, client, originApp, owner)
}

// VisibilityScope returns the searchable audience for an admitted agent
// session. Session owners are authenticated gateway principals. Email
// comparison is case-insensitive and whitespace-normalized to match the
// owner-scope resolver; malformed policy values still fail at load time.
func (p Policy) VisibilityScope(owner string) string {
	normalized := CanonicalOwner(owner)
	if strIn(p.OwnerOnly.Principals, normalized) {
		return events.ScopeOwner(normalized)
	}
	return events.ScopeInternal
}

func strIn(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// Hash returns a stable fingerprint of the policy's effective
// content, normalized for order-independence. The worker stores the hash
// of the policy the candidates were last checked against; when it differs,
// the worker checks them again (Store.ReapplyPolicy). A hash that changes
// while the decisions do not, as when a release changes what is hashed,
// costs that one pass over the candidates and writes nothing.
func (p Policy) Hash() string {
	c := p
	c.Include.OwnerKinds = append([]string(nil), p.Include.OwnerKinds...)
	c.Include.Roles = append([]string(nil), p.Include.Roles...)
	c.Include.Clients = append([]string(nil), p.Include.Clients...)
	c.Exclude.Apps = append([]string(nil), p.Exclude.Apps...)
	c.Exclude.Principals = append([]string(nil), p.Exclude.Principals...)
	c.Exclude.Clients = append([]string(nil), p.Exclude.Clients...)
	c.Exclude.SessionIDs = append([]string(nil), p.Exclude.SessionIDs...)
	c.OwnerOnly.Principals = append([]string(nil), p.OwnerOnly.Principals...)
	for _, s := range []*[]string{
		&c.Include.OwnerKinds, &c.Include.Roles, &c.Include.Clients,
		&c.Exclude.Apps, &c.Exclude.Principals, &c.Exclude.Clients, &c.Exclude.SessionIDs,
		&c.OwnerOnly.Principals,
	} {
		sort.Strings(*s)
	}
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", sum[:])
}
