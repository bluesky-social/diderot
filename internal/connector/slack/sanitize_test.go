package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	slackgo "github.com/slack-go/slack"
)

func TestSanitizeSlackMarkdown(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Broadcasts are defanged but keep their visible text.
		{"channel broadcast", "<!channel>", "@channel"},
		{"here broadcast", "hi <!here> folks", "hi @here folks"},
		{"everyone broadcast", "<!everyone> heads up", "@everyone heads up"},
		{"channel labeled", "<!channel|Attention in here>", "@Attention in here"},
		{"here labeled", "<!here|CW readers only>", "@CW readers only"},

		// User-group mentions lose their group ping.
		{"usergroup raw", "ping <!subteam^S012345>", "ping @S012345"},
		{"usergroup labeled", "ping <!subteam^S012345|SRE agents>", "ping @SRE agents"},

		// Direct user/workspace/bot mentions lose their ping.
		{"user mention", "<@U123>", "@U123"},
		{"user mention labeled", "<@U123|Jane Doe>", "@Jane Doe"},
		{"user mention in sentence", "ask <@U456> about it", "ask @U456 about it"},

		// Ordinary Markdown is preserved exactly.
		{"bold", "**bold** *italic* `code`", "**bold** *italic* `code`"},
		{"bullet list", "- one\n- two", "- one\n- two"},
		{"quote", "> quoted line", "> quoted line"},
		{"inline markdown link", "[text](https://example.com)", "[text](https://example.com)"},

		// Standard Slack links are untouched.
		{"bare url", "see <https://example.com>", "see <https://example.com>"},
		{"labeled url", "<https://example.com|Example>", "<https://example.com|Example>"},
		{"mailto link", "<mailto:hi@example.com|email>", "<mailto:hi@example.com|email>"},

		// Trusted channel references are preserved.
		{"channel ref", "<#C123>", "<#C123>"},
		{"channel ref labeled", "<#C123|general>", "<#C123|general>"},

		// The date control sequence is harmless but is still a "<!"
		// form; it is neutralized like the others rather than carved
		// out as an exception (see sanitize.go's doc comment: the
		// postcondition is that no raw "<!" survives, unconditionally).
		// Its fallback label carries no "@" prefix (unlike a mention),
		// since a date is not a mention.
		{"date command", "<!date^1530892800|time>", "time"},

		// No angle brackets: fast path, unchanged.
		{"no angle brackets", "plain *text* only", "plain *text* only"},

		// Mixed realistic sentence.
		{
			"mixed",
			"<!here> check <@U123|Sam> and <!subteam^S99|oncall> in <#C7|bugs> — see <https://example.com/i|link> *bold*",
			"@here check @Sam and @oncall in <#C7|bugs> — see <https://example.com/i|link> *bold*",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeSlackMarkdown(tc.in); got != tc.want {
				t.Errorf("sanitizeSlackMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeSlackMarkdownDoesNotRecombineStrayBracketIntoLiveMention
// covers the bug found by the slack-sanitize property test: a doubled
// leading '<' right before a dangerous "<@...>" form was left in the
// output untouched, sitting directly in front of whatever "<...>" form
// came next. Trigger: "<<@U123><#C123>" — a stray '<' immediately
// before a user mention, immediately followed by a channel reference.
// Contract (sanitize.go's doc comment): "channel references are
// preserved verbatim" as their own, separately-parseable "<#...>"
// token. The old single-regex-pass-per-form implementation instead
// produced "<@U123<#C123>": the leftover '<' fused with the neutralized
// mention's "@U123" text, and the LATER user-mention regex pass (same
// call) then matched that whole span, plus the channel reference's own
// trailing '>', as one combined "<@...>" token — absorbing "<#C123>"
// so it could no longer render as its own channel link at all. The
// current implementation's dangerous-form regexes absorb any run of
// leading '<' into the match itself, so the whole "<<@U123>" is
// consumed and replaced as one unit, leaving nothing behind to fuse
// with "<#C123>".
func TestSanitizeSlackMarkdownDoesNotRecombineStrayBracketIntoLiveMention(t *testing.T) {
	in := "<<@U123><#C123>"
	got := sanitizeSlackMarkdown(in)
	want := "@U123<#C123>"
	if got != want {
		t.Fatalf("sanitizeSlackMarkdown(%q) = %q, want %q (the channel reference \"<#C123>\" must stay its own, separately-parseable token, and no \"<@\" may survive)", in, got, want)
	}
}

// TestSanitizeSlackMarkdownDoesNotGarbleUnrelatedTextAfterStrayBracket
// covers the sibling bug: the same stray-bracket defect let the
// user-mention regex's unbounded id/label capture (`[^|>]+`) reach
// forward across unrelated prose to the next literal '>' ANYWHERE
// later in the message, deleting it. Trigger: a stray '<' before
// "<!channel>", followed later in the same message by an ordinary '>'
// used in a comparison that has nothing to do with Slack control
// syntax. Contract (sanitize.go's doc comment): "Ordinary Markdown ...
// preserved verbatim". The old implementation returned "@channel
// please check: is 5 3? reply here": the comparison's '>' (and the
// space before it) vanished. The current implementation's
// dangerous-form regexes absorb the leading run of '<' into the match
// (consuming exactly "<<!channel>" and nothing more), so the
// comparison later in the message is never touched.
func TestSanitizeSlackMarkdownDoesNotGarbleUnrelatedTextAfterStrayBracket(t *testing.T) {
	in := "<<!channel> please check: is 5 > 3? reply here"
	got := sanitizeSlackMarkdown(in)
	want := "@channel please check: is 5 > 3? reply here"
	if got != want {
		t.Fatalf("sanitizeSlackMarkdown(%q) = %q, want %q (the unrelated comparison \"is 5 > 3?\" must survive intact)", in, got, want)
	}
}

// TestSanitizeSlackMarkdownPreservesCodeAndComparisons pins that
// ordinary engineering text is left alone. Go generics, channel
// receives, shell placeholders, and numeric comparisons all contain a
// bare '<' that never starts a Slack control sequence; escaping every
// '<' that does not start a known form would turn each into "&lt;".
// Contract (sanitize.go's doc comment): "Ordinary Markdown, code,
// standard Slack links, and channel references are preserved
// verbatim." None of these inputs contain '!' or '@' right after any
// '<', so none of them should be touched at all.
func TestSanitizeSlackMarkdownPreservesCodeAndComparisons(t *testing.T) {
	cases := []string{
		"Run `kubectl -n <namespace> logs <pod>`",
		"```go\nfor i := 0; i < n; i++ {\n\tv := <-ch\n}\n```",
		"a <= b and List<String>",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if got := sanitizeSlackMarkdown(in); got != in {
				t.Fatalf("sanitizeSlackMarkdown(%q) = %q, want unchanged (no '<!' or '<@' anywhere in the input, so nothing should be escaped)", in, got)
			}
		})
	}
}

// TestSanitizeSlackMarkdownDateDirectiveDoesNotReachAcrossProse pins
// that reDate's timestamp capture is bounded. With an unbounded capture
// (`[^|<>]*`, no whitespace exclusion), a bare "<!date^" not followed
// by an actual timestamp reaches forward, across paragraph breaks, to
// the next unrelated '>' and replaces everything in between with
// "@date" or a fallback label.
func TestSanitizeSlackMarkdownDateDirectiveDoesNotReachAcrossProse(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"<!date^ then a long paragraph of prose\n\nand a quote\n> quoted",
			"&lt;!date^ then a long paragraph of prose\n\nand a quote\n> quoted",
		},
		{
			"Slack's date token starts with <!date^ and the docs say\nis 5 > 3? yes",
			"Slack's date token starts with &lt;!date^ and the docs say\nis 5 > 3? yes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeSlackMarkdown(tc.in); got != tc.want {
				t.Fatalf("sanitizeSlackMarkdown(%q) = %q, want %q (a malformed \"<!date^\" must not reach across unrelated prose to a later '>')", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeSlackMarkdownHandlesMixedCaseBroadcasts covers the third
// bug: reBroadcast and reSubteam matched their keywords
// ("channel"/"here"/"everyone"/"subteam") as case-sensitive lowercase
// literals only, so "<!CHANNEL>", "<!Channel>", "<!HERE>",
// "<!EVERYONE>", and "<!SUBTEAM^...>" passed through completely
// unsanitized. Contract (sanitize.go's doc comment / stated purpose):
// "neutralizes the Slack <...> forms that can trigger a broadcast or a
// user/group notification" — case is not part of that contract's
// exception list, and Slack's client renders these keywords the same
// regardless of case. The old implementation returned each of these
// inputs byte-for-byte unchanged.
func TestSanitizeSlackMarkdownHandlesMixedCaseBroadcasts(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<!CHANNEL>", "@CHANNEL"},
		{"<!Channel>", "@Channel"},
		{"<!HERE>", "@HERE"},
		{"<!EVERYONE>", "@EVERYONE"},
		{"<!SUBTEAM^S123>", "@S123"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeSlackMarkdown(tc.in); got != tc.want {
				t.Fatalf("sanitizeSlackMarkdown(%q) = %q, want %q (mixed-case broadcast/subteam forms must be neutralized just like their lowercase form)", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeSlackMarkdownIsLinearOnLongRuns pins linear time on long
// runs of '<'. Each dangerous-form regex absorbs a leading run of '<'
// (`<+`); a loop that retried the match one position at a time across
// a run not in fact followed by a dangerous form would cost O(n)
// attempts of O(n) each, which takes seconds at 10,000 bytes. Each
// case here is sized well past where a superquadratic implementation
// would time out, and bounded at a fraction of a second so the test
// itself stays fast.
func TestSanitizeSlackMarkdownIsLinearOnLongRuns(t *testing.T) {
	const n = 1 << 20 // 1 MiB
	const budget = 500 * time.Millisecond
	cases := []struct {
		name string
		in   string
	}{
		{"run of <", strings.Repeat("<", n)},
		{"run of <!", strings.Repeat("<!", n/2)},
		{"run of <@", strings.Repeat("<@", n/2)},
		{"run of < then a non-form tail", strings.Repeat("<", n) + "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			out := sanitizeSlackMarkdown(tc.in)
			elapsed := time.Since(start)
			if elapsed > budget {
				t.Fatalf("sanitizeSlackMarkdown took %s on a %d-byte input, want under %s (superquadratic regression)", elapsed, len(tc.in), budget)
			}
			if strings.Contains(out, "<!") || strings.Contains(out, "<@") {
				t.Fatalf("sanitizeSlackMarkdown left a live token opener in a %d-byte input", len(tc.in))
			}
		})
	}
}

// TestFormatAskNotificationIsLinearOnAnUncappedQuestion pins the same
// property at its real call site: an HTTP /ask or MCP retrieve/ask
// question has no length cap before it reaches formatAskNotification,
// which sanitizes it ahead of the 400-rune truncation
// (ask_notification.go).
func TestFormatAskNotificationIsLinearOnAnUncappedQuestion(t *testing.T) {
	start := time.Now()
	out := formatAskNotification(askNotification{
		Source:   "slack",
		Asker:    "U1",
		Question: "why " + strings.Repeat("<", 1<<18),
	})
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("formatAskNotification took %s on an uncapped question, want under 500ms (superquadratic regression)", elapsed)
	}
	if strings.Contains(out, "<!") || strings.Contains(out, "<@") {
		t.Fatalf("formatAskNotification left a live token opener: %q", out[:min(len(out), 80)])
	}
}

// newCapturingSlackServer spins up a fake Slack API that records every
// chat.postMessage `text` it is asked to send and returns a canned TS.
func newCapturingSlackServer(t *testing.T, got chan<- url.Values) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got <- r.PostForm
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/conversations.open") {
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D1"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1700000000.000100"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPostAskAnswerSanitizesMentions verifies the answer path posts the
// sanitized model reply in both the non-mrkdwn text fallback and the
// authoritative Markdown block.
func TestPostAskAnswerSanitizesMentions(t *testing.T) {
	forms := make(chan url.Values, 4)
	srv := newCapturingSlackServer(t, forms)

	reply := "Summary:\n**Bold** and [link](https://example.com). Could someone <!here> ping <@U123|Sam> and <!subteam^S99|oncall>? Keep it in <#C7|bugs>."
	c := &Consumer{api: slackgo.New("xoxb-test", slackgo.OptionAPIURL(srv.URL+"/"))}

	// sessionID empty so the best-effort answer-message insert is skipped.
	if err := c.postAskAnswer(context.Background(), "C1", "1700000000.000100", "", 0, reply, ""); err != nil {
		t.Fatalf("postAskAnswer: %v", err)
	}

	form := <-forms
	wantText := "Summary:\n**Bold** and [link](https://example.com). Could someone @here ping @Sam and @oncall? Keep it in <#C7|bugs>."
	if got := form.Get("text"); got != wantText {
		t.Errorf("answer text fallback = %q, want %q", got, wantText)
	}
	if got := form.Get("mrkdwn"); got != "false" {
		t.Errorf("answer text fallback mrkdwn = %q, want false", got)
	}

	// The final chunk also ships a Markdown block duplicating the text.
	blocksJSON := form.Get("blocks")
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(blocksJSON), &blocks); err != nil {
		t.Fatalf("unmarshal blocks: %v", err)
	}
	var blockText string
	for _, b := range blocks {
		if b["type"] == "markdown" {
			blockText, _ = b["text"].(string)
		}
	}
	if blockText != wantText {
		t.Errorf("answer Markdown block = %q, want %q", blockText, wantText)
	}

	// The Markdown block must not contain any surviving mention/control form.
	for _, d := range []string{"<!channel>", "<!here>", "<!everyone>", "<!subteam^", "<@"} {
		if strings.Contains(blockText, d) {
			t.Errorf("Markdown block still contains dangerous form %q: %q", d, blockText)
		}
	}
}

// TestDeliverScheduledReportSanitizesMentions verifies model-generated
// scheduled-report bodies are sanitized before posting.
func TestDeliverScheduledReportSanitizesMentions(t *testing.T) {
	forms := make(chan url.Values, 4)
	srv := newCapturingSlackServer(t, forms)

	c := &Consumer{api: slackgo.New("xoxb-test", slackgo.OptionAPIURL(srv.URL+"/"))}
	runID := uuid.New()
	body := "Daily digest: <!channel> — <@U1|Kim> flagged <#C2|ops>; see [report](https://example.com)."
	if _, err := c.DeliverScheduledReport(context.Background(), "slack_channel", "C1", runID, body); err != nil {
		t.Fatalf("DeliverScheduledReport: %v", err)
	}
	form := <-forms
	want := "Daily digest: @channel — @Kim flagged <#C2|ops>; see [report](https://example.com)."
	if got := form.Get("text"); got != want {
		t.Errorf("scheduled report text = %q, want %q", got, want)
	}
	if got := form.Get("mrkdwn"); got != "false" {
		t.Errorf("scheduled report fallback mrkdwn = %q, want false", got)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(form.Get("blocks")), &blocks); err != nil {
		t.Fatalf("decode scheduled report blocks: %v", err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "markdown" || blocks[0]["text"] != want {
		t.Fatalf("scheduled report blocks = %#v", blocks)
	}
}

// TestDMUserPreservesTrustedReferences guards the delivery boundary: callers
// compose application-owned mentions and channel references in DMs, while
// untrusted fields are sanitized before they reach dmUser.
func TestDMUserPreservesTrustedReferences(t *testing.T) {
	forms := make(chan url.Values, 4)
	srv := newCapturingSlackServer(t, forms)
	c := &Consumer{api: slackgo.New("xoxb-test", slackgo.OptionAPIURL(srv.URL+"/"))}

	msg := "ping <@U_OPERATOR> about <#C_PRIVATE>"
	if err := c.dmUser(context.Background(), "U_RECIPIENT", msg); err != nil {
		t.Fatalf("dmUser: %v", err)
	}

	<-forms // conversations.open
	if got := (<-forms).Get("text"); got != msg {
		t.Errorf("DM text = %q, want trusted references preserved as %q", got, msg)
	}
}
