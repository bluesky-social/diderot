package files

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/report"
)

// redactAuditGoldenPath is the committed, deterministic snapshot of the
// gitleaks default ruleset as consumed by secretRedactor. It is derived
// programmatically from detect.NewDetectorDefaultConfig().Config — never by
// scraping the vendor TOML — so it reflects exactly the rules, path
// constraints (which silently disable rules for DetectString), and
// allowlists (the exemptions) that apply when captions are redacted.
const redactAuditGoldenPath = "gitleaks-default-audit.json"

// regenerateRedactAudit is the exact command a maintainer runs to rewrite the
// golden snapshot after an intentional gitleaks/config bump. It is surfaced
// verbatim when the golden test detects drift.
const regenerateRedactAudit = "go test ./internal/attachments/files -run '^TestRedactAuditGolden$' -args -update"

// update is the standard golden-file switch; go test forwards it to the test
// binary via -args (see regenerateRedactAudit).
var update = flag.Bool("update", false, "rewrite the gitleaks default audit golden file in place")

// auditAllowlist is a deterministic, plain-data rendering of a gitleaks
// Allowlist. Ordering is normalized because gitleaks lowercases and dedupes
// stopwords/commits through maps (nondeterministic iteration); regexes and
// paths are sorted for byte-for-byte stability too.
type auditAllowlist struct {
	Description string   `json:"description,omitempty"`
	Condition   string   `json:"condition,omitempty"`
	RegexTarget string   `json:"regex_target,omitempty"`
	Commits     []string `json:"commits,omitempty"`
	Paths       []string `json:"paths,omitempty"`
	Regexes     []string `json:"regexes,omitempty"`
	StopWords   []string `json:"stop_words,omitempty"`
}

// auditRule is the audit-relevant subset of a gitleaks rule: the path
// constraint (which can make the rule a no-op for DetectString), the entropy
// floor, and any allowlists (the exemptions that suppress findings). The
// detection regex itself is intentionally omitted.
type auditRule struct {
	RuleID     string           `json:"rule_id"`
	Path       string           `json:"path,omitempty"`
	Entropy    float64          `json:"entropy,omitempty"`
	Allowlists []auditAllowlist `json:"allowlists,omitempty"`
}

// auditSnapshot is the full committed snapshot: title, total rule count,
// every global allowlist, and every rule carrying a path constraint, entropy
// floor, or allowlist.
type auditSnapshot struct {
	Title            string           `json:"title"`
	Description      string           `json:"description,omitempty"`
	GeneratedBy      string           `json:"generated_by"`
	RuleCount        int              `json:"rule_count"`
	GlobalAllowlists []auditAllowlist `json:"global_allowlists"`
	Rules            []auditRule      `json:"rules"`
}

// auditAllowlist renders an *config.Allowlist deterministically.
func toAuditAllowlist(a *config.Allowlist) auditAllowlist {
	aa := auditAllowlist{
		Description: a.Description,
		Condition:   a.MatchCondition.String(),
		RegexTarget: a.RegexTarget,
	}
	aa.Commits = append(aa.Commits, a.Commits...)
	for _, p := range a.Paths {
		if p != nil {
			aa.Paths = append(aa.Paths, p.String())
		}
	}
	for _, r := range a.Regexes {
		if r != nil {
			aa.Regexes = append(aa.Regexes, r.String())
		}
	}
	aa.StopWords = append(aa.StopWords, a.StopWords...)
	sort.Strings(aa.Commits)
	sort.Strings(aa.Paths)
	sort.Strings(aa.Regexes)
	sort.Strings(aa.StopWords)
	return aa
}

// deriveAuditSnapshot extracts the audit snapshot from a gitleaks default
// Config. Map-derived data (the Rules map) is iterated by sorted rule ID;
// every slice is sorted so the output is byte-for-byte stable across runs.
func deriveAuditSnapshot(cfg config.Config) auditSnapshot {
	snap := auditSnapshot{
		Title:       cfg.Title,
		Description: cfg.Description,
		GeneratedBy: "derived programmatically from detect.NewDetectorDefaultConfig().Config; regenerate with " + regenerateRedactAudit,
		RuleCount:   len(cfg.Rules),
	}
	for _, a := range cfg.Allowlists {
		if a != nil {
			snap.GlobalAllowlists = append(snap.GlobalAllowlists, toAuditAllowlist(a))
		}
	}

	ruleIDs := make([]string, 0, len(cfg.Rules))
	for id := range cfg.Rules {
		ruleIDs = append(ruleIDs, id)
	}
	sort.Strings(ruleIDs)
	for _, id := range ruleIDs {
		r := cfg.Rules[id]
		if r.Path == nil && r.Entropy == 0 && len(r.Allowlists) == 0 {
			// Not audit-relevant: no path, entropy floor, or exemptions.
			continue
		}
		ar := auditRule{RuleID: r.RuleID, Entropy: r.Entropy}
		if r.Path != nil {
			ar.Path = r.Path.String()
		}
		for _, a := range r.Allowlists {
			if a != nil {
				ar.Allowlists = append(ar.Allowlists, toAuditAllowlist(a))
			}
		}
		snap.Rules = append(snap.Rules, ar)
	}
	return snap
}

func marshalSnapshot(snap auditSnapshot) ([]byte, error) {
	return json.MarshalIndent(snap, "", "  ")
}

// TestRedactAuditGolden fails on any drift between the ruleset the redactor
// actually loads and the committed snapshot, printing the exact regeneration
// command. It deliberately takes no position on whether any exemption is safe
// or unsafe — it only records which exemptions exist.
func TestRedactAuditGolden(t *testing.T) {
	d, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		t.Fatalf("detect.NewDetectorDefaultConfig: %v", err)
	}
	snap := deriveAuditSnapshot(d.Config)
	got, err := marshalSnapshot(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	path := filepath.Join("testdata", redactAuditGoldenPath)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("rewrote %s (%d total rules, %d audit-relevant rules)", path, snap.RuleCount, len(snap.Rules))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v — regenerate with:\n    %s", path, err, regenerateRedactAudit)
	}
	if string(want) != string(append(got, '\n')) {
		t.Errorf("redaction config audit snapshot has drifted from %s; regenerate with:\n    %s", path, regenerateRedactAudit)
	}
}

// syntheticK8sSecretYAML is a synthetic Kubernetes Secret manifest: the value
// is base64 of "sensitive-password", a fabricated placeholder, not a real
// credential.
const syntheticK8sSecretYAML = `apiVersion: v1
kind: Secret
metadata:
  name: demo
type: Opaque
data:
  password: c2Vuc2l0aXZlLXBhc3N3b3Jk
`

// TestRedactAuditSyntheticProbes pins the factual behavior of the default
// detector/redactor with deliberately synthetic inputs (no real secrets):
// a GitHub PAT is detected; the AWS documentation placeholder is exempted by
// the default rule; and the Kubernetes Secret rule is inactive for
// DetectString because it requires a matching filename.
func TestRedactAuditSyntheticProbes(t *testing.T) {
	d, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		t.Fatalf("detect.NewDetectorDefaultConfig: %v", err)
	}
	r := &secretRedactor{detector: d}

	t.Run("github pat detected", func(t *testing.T) {
		caption := "A terminal screenshot.\n\nText:\ngithub_token=" + syntheticGithubPAT
		got := ruleIDs(d.DetectString(caption))
		if !contains(got, "github-pat") {
			t.Fatalf("DetectString did not flag synthetic GitHub PAT; findings=%v", got)
		}
	})

	t.Run("aws example allowed by default rule", func(t *testing.T) {
		const awsExample = "AKIAIOSFODNN7EXAMPLE"
		caption := "aws_access_key_id=" + awsExample
		if got := ruleIDs(d.DetectString(caption)); len(got) != 0 {
			t.Fatalf("aws-access-token flagged its documented EXAMPLE placeholder; findings=%v", got)
		}
		redacted, findings := r.Redact(caption)
		if redacted != caption {
			t.Errorf("AWS EXAMPLE should pass through unchanged; got %q", redacted)
		}
		if len(findings) != 0 {
			t.Errorf("AWS EXAMPLE produced findings %v; want none", findings)
		}
	})

	t.Run("kubernetes secret yaml rule not triggered by DetectString (path constrained)", func(t *testing.T) {
		k8sRule, ok := d.Config.Rules["kubernetes-secret-yaml"]
		if !ok || k8sRule.Path == nil {
			t.Fatal("default config is missing the path-constrained kubernetes-secret-yaml rule")
		}
		if k8sRule.Path.MatchString("") {
			t.Fatalf("kubernetes-secret-yaml path %q unexpectedly matches DetectString's empty filename", k8sRule.Path)
		}
		if !k8sRule.Path.MatchString("deploy/secret.yaml") {
			t.Fatalf("kubernetes-secret-yaml path %q does not match a representative YAML filename", k8sRule.Path)
		}

		// DetectString scans with an empty FilePath, so the
		// kubernetes-secret-yaml rule's `(?i)\.ya?ml$` path constraint never
		// matches and the rule is a silent no-op for captions. Other, non
		// path-constrained rules (e.g. generic-api-key) may still match parts
		// of the YAML; the point of this probe is that the k8s rule itself
		// is never activated for caption text.
		detected := ruleIDs(d.DetectString(syntheticK8sSecretYAML))
		if contains(detected, "kubernetes-secret-yaml") {
			t.Fatalf("path-constrained kubernetes-secret-yaml was flagged by DetectString; findings=%v", detected)
		}

		_, rfindings := r.Redact(syntheticK8sSecretYAML)
		if contains(rfindings, "kubernetes-secret-yaml") {
			t.Errorf("Redact produced a kubernetes-secret-yaml finding for path-constrained YAML; findings=%v", rfindings)
		}
	})
}

func ruleIDs(findings []report.Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.RuleID)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// BenchmarkSecretRedactor measures Redact on clean and finding-bearing
// captions at 1 KiB and 16 KiB. The default detector is built once before the
// timed loops; allocations are
// reported per iteration. There is no latency threshold asserted.
func BenchmarkSecretRedactor(b *testing.B) {
	// Detector construction deliberately lives outside the timed loop.
	r, err := newSecretRedactor()
	if err != nil {
		b.Fatalf("newSecretRedactor: %v", err)
	}

	cases := []struct {
		name    string
		caption string
	}{
		{name: "clean_1KiB", caption: makeCaption(1<<10, "")},
		{name: "clean_16KiB", caption: makeCaption(1<<14, "")},
		{name: "finding_1KiB", caption: makeCaption(1<<10, syntheticGithubPAT)},
		{name: "finding_16KiB", caption: makeCaption(1<<14, syntheticGithubPAT)},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.caption)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Redact(tc.caption)
			}
		})
	}
}

// makeCaption builds a caption of at least size bytes, optionally embedding a
// synthetic secret on a dedicated line.
func makeCaption(size int, secret string) string {
	var sb strings.Builder
	sb.WriteString("[Image]\nA screenshot of terminal output.\n\nText:\n")
	if secret != "" {
		sb.WriteString("password=" + secret + "\n")
	}
	const padding = "line of transcribed terminal text for padding.\n"
	for sb.Len() < size {
		remaining := size - sb.Len()
		if remaining < len(padding) {
			sb.WriteString(padding[:remaining])
			break
		}
		sb.WriteString(padding)
	}
	return sb.String()
}
