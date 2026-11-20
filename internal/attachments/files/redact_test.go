package files

import (
	"strings"
	"sync"
	"testing"
)

// syntheticGithubPAT is shaped like a GitHub personal access token
// (ghp_ + 36 hex chars) but is not a real credential — synthesized
// for this test, never copied from a live value.
const syntheticGithubPAT = "ghp_" + "0123456789abcdef0123456789abcdef0123"

func TestSecretRedactorConcurrentConstructionAndUse(t *testing.T) {
	const callers = 64

	start := make(chan struct{})
	redactors := make(chan *secretRedactor, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := newSecretRedactor()
			if err == nil {
				redacted, findings := r.Redact("github_token=" + syntheticGithubPAT)
				if strings.Contains(redacted, syntheticGithubPAT) || len(findings) == 0 {
					t.Errorf("concurrent redaction did not remove synthetic GitHub PAT")
				}
			}
			redactors <- r
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(redactors)
	close(errs)

	var singleton *secretRedactor
	for err := range errs {
		if err != nil {
			t.Fatalf("newSecretRedactor: %v", err)
		}
	}
	for r := range redactors {
		if singleton == nil {
			singleton = r
			continue
		}
		if r != singleton {
			t.Fatal("newSecretRedactor returned more than one process instance")
		}
	}
}

func TestRedactRedactsRealShapedSecrets(t *testing.T) {
	// Negative control: gitleaks' canonical AWS example fake
	// (AKIAIOSFODNN7EXAMPLE — the AWS docs' own placeholder key,
	// safe to use verbatim) plus a synthetic GitHub PAT shape.
	//
	// NOTE: gitleaks' default ruleset (vendor/.../config/gitleaks.toml,
	// aws-access-token rule) carries an explicit allowlist
	// (`regexes = ['.+EXAMPLE$']`) that exempts any string ending in
	// "EXAMPLE" — precisely because AKIAIOSFODNN7EXAMPLE is the
	// well-known AWS documentation placeholder. So this string is
	// intentionally NEVER flagged by the default detector; it's kept
	// here (and in the "fixture is meaningful" check below) to
	// document that behavior, but the "must be redacted" assertion
	// only applies to the GitHub PAT, which the default ruleset does
	// catch (rule "github-pat").
	const awsExample = "AKIAIOSFODNN7EXAMPLE"
	caption := "[Image]\nA screenshot of a terminal.\n\n" +
		"Text:\naws_access_key_id=" + awsExample + "\n" +
		"github_token=" + syntheticGithubPAT + "\n\n" +
		"Tags: terminal, secrets"

	// Prove the fixture is meaningful before Redact touches it.
	if !strings.Contains(caption, awsExample) {
		t.Fatal("fixture missing AWS example secret")
	}
	if !strings.Contains(caption, syntheticGithubPAT) {
		t.Fatal("fixture missing synthetic GitHub PAT")
	}

	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}

	redacted, findings := r.Redact(caption)

	if strings.Contains(redacted, syntheticGithubPAT) {
		t.Error("synthetic GitHub PAT still present after redaction")
	}
	if !strings.Contains(redacted, "[REDACTED:") {
		t.Error("expected a [REDACTED:...] marker in output")
	}
	if len(findings) == 0 {
		t.Error("expected non-empty findings")
	}
	found := false
	for _, id := range findings {
		if id == "github-pat" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a github-pat finding, got %v", findings)
	}
}

func TestRedactCleanCaptionUnchanged(t *testing.T) {
	caption := "[Image]\nA photo of a mountain lake at sunset.\n\n" +
		"Text:\nNo text visible in this image.\n\n" +
		"Tags: nature, lake, sunset"

	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}

	redacted, findings := r.Redact(caption)

	if redacted != caption {
		t.Errorf("clean caption should pass through unchanged:\ngot:  %q\nwant: %q", redacted, caption)
	}
	if len(findings) != 0 {
		t.Errorf("expected no findings for clean caption, got %v", findings)
	}
}

// TestRedactAdjacentOverlappingFindingsAreFullyRedacted covers the case
// where two DIFFERENT gitleaks rules match overlapping spans of the same
// input: a Slack bot token immediately followed by a GitHub PAT, with no
// separator between them.
//
// Trigger: slack-bot-token's regex
// (`xoxb-[0-9]{10,13}-[0-9]{10,13}[a-zA-Z0-9-]*`) has no secretGroup, so its
// whole match is its Secret, and its trailing character class is greedy
// with no boundary before an underscore. When the token is immediately
// followed by "ghp_...", the match consumes the "ghp" letters of the
// adjacent PAT too, stopping only at the underscore. So slack-bot-token's
// Secret is "<slack token>ghp" and github-pat's Secret is "ghp_<36 hex
// chars>" — two findings whose matched text overlaps in the original
// string on the shared "ghp".
//
// Contract (redact.go's doc comment on Redact): every detected secret
// (every RuleID present in the returned findings) must not appear
// verbatim in the returned string. Replacing each finding's Secret one
// at a time with strings.ReplaceAll against the *already-mutated* output
// breaks that: whichever finding is applied second no longer finds its
// own Secret as a literal substring (the first replacement has already
// consumed the shared "ghp"), so its ReplaceAll silently does nothing
// while both RuleIDs are reported as redacted. gitleaks evaluates rules via Go map iteration, so which
// finding is "first" is randomized per call; the loop below runs enough
// iterations to observe both orders.
func TestRedactAdjacentOverlappingFindingsAreFullyRedacted(t *testing.T) {
	const slackSecret = "xoxb-" + "123456789012-123456789012-abcdefghijklmnopqrstuvwx"
	const githubSecret = "ghp_" + "0123456789abcdef" + "0123456789abcdef0123"
	// githubSecretBody is the 36-character token body after the "ghp_"
	// prefix: the part slack-bot-token's greedy match leaves behind when
	// it is applied first (it stops one character short of the body, at
	// "ghp"). That body alone is still sensitive, credential-shaped
	// material, so checking for it catches the "leftover fragment" case
	// too, not just the "whole secret left behind" case.
	githubSecretBody := strings.TrimPrefix(githubSecret, "ghp_")
	const input = slackSecret + githubSecret

	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}

	for i := 0; i < 50; i++ {
		redacted, findings := r.Redact(input)

		if !contains(findings, "slack-bot-token") || !contains(findings, "github-pat") {
			t.Fatalf("run %d: expected both rules to fire on this fixture; findings=%v", i, findings)
		}
		if strings.Contains(redacted, slackSecret) {
			t.Fatalf("run %d: full Slack bot token still present after Redact, even though "+
				"\"slack-bot-token\" is in findings=%v\nredacted: %q", i, findings, redacted)
		}
		if strings.Contains(redacted, githubSecretBody) {
			t.Fatalf("run %d: GitHub PAT body still present after Redact, even though "+
				"\"github-pat\" is in findings=%v\nredacted: %q", i, findings, redacted)
		}
	}
}

// TestRedactRepeatedSecretDoesNotInheritAnUnrelatedOccurrencesRuleIDs covers
// an aliasing hazard in redactOnePass's fix for the quadratic-memory
// regression: grouping findings by Secret before scanning for occurrences
// means every occurrence of the SAME Secret starts from one shared rule-ID
// set. If that set were shared by reference across occurrences instead of
// copied, merging one occurrence with an unrelated, overlapping,
// DIFFERENT secret (mutating the shared set to add that other secret's
// rule ID) would silently also tag every OTHER, non-overlapping occurrence
// of the first secret with that unrelated rule ID — because they all
// pointed at the same mutated map.
//
// Fixture: a GitHub PAT occurs twice. The first occurrence is immediately
// followed by a Slack bot token whose matched span overlaps it (sharing
// the "ghp" the Slack rule's greedy match consumes — the scenario
// TestRedactAdjacentOverlappingFindingsAreFullyRedacted also exercises),
// but the token comes first here so what the Slack rule's greedy match
// actually consumes is irrelevant; what matters is that the two findings'
// spans touch. The second occurrence stands alone, with no other secret
// anywhere near it, so it must be reported (and marked) as github-pat only.
func TestRedactRepeatedSecretDoesNotInheritAnUnrelatedOccurrencesRuleIDs(t *testing.T) {
	const githubSecret = "ghp_" + "0123456789abcdef" + "0123456789abcdef0123"
	const slackSecret = "xoxb-" + "123456789012-123456789012-abcdefghijklmnopqrstuvwx"
	const input = githubSecret + slackSecret + "\n\nunrelated filler text\n\n" + githubSecret

	r, err := newSecretRedactor()
	if err != nil {
		t.Fatalf("newSecretRedactor: %v", err)
	}

	redacted, findings := r.Redact(input)

	if strings.Contains(redacted, githubSecret) || strings.Contains(redacted, slackSecret) {
		t.Fatalf("a planted secret survived redaction; findings=%v\nredacted: %q", findings, redacted)
	}

	slackCount := 0
	for _, id := range findings {
		if id == "slack-bot-token" {
			slackCount++
		}
	}
	if slackCount != 1 {
		t.Errorf("expected exactly one slack-bot-token finding (it occurs once), got %d in findings=%v",
			slackCount, findings)
	}

	// The second, isolated occurrence must be marked github-pat alone: it
	// must not inherit slack-bot-token from the first occurrence's merge.
	if !strings.HasSuffix(redacted, "\n\n[REDACTED:github-pat]") {
		t.Errorf("isolated second occurrence was not marked as github-pat alone; redacted: %q", redacted)
	}
}

func TestRedactNilSafety(t *testing.T) {
	const s = "some caption text, doesn't matter"

	t.Run("nil detector field", func(t *testing.T) {
		r := &secretRedactor{detector: nil}
		redacted, findings := r.Redact(s)
		if redacted != s {
			t.Errorf("got %q, want unchanged %q", redacted, s)
		}
		if len(findings) != 0 {
			t.Errorf("expected no findings, got %v", findings)
		}
	})

	t.Run("nil pointer receiver", func(t *testing.T) {
		var r *secretRedactor
		redacted, findings := r.Redact(s)
		if redacted != s {
			t.Errorf("got %q, want unchanged %q", redacted, s)
		}
		if len(findings) != 0 {
			t.Errorf("expected no findings, got %v", findings)
		}
	})
}
