package ask

import (
	"strings"
	"testing"
	"time"
)

// TestFormatContextEnvelope_Rich verifies the compact facts-only envelope.
func TestFormatContextEnvelope_Rich(t *testing.T) {
	pdt := time.FixedZone("PDT", -7*3600)
	now := time.Date(2026, 5, 18, 16, 0, 0, 0, time.UTC)
	asker := &AskerInfo{
		DisplayName: "Alex Rivera",
		Handle:      "Alex",
		Email:       "alex@example.com",
		SlackUserID: "U0EXAMPLE01",
		LocalTime:   now.In(pdt),
		TZ:          "America/Los_Angeles",
		TZLabel:     "Pacific Daylight Time",
	}
	got := formatContextEnvelope(asker, now, "", "what did I do last week?")

	want := "<context>\n" +
		`{"asker":{"display_name":"Alex Rivera","email":"alex@example.com","slack_user_id":"U0EXAMPLE01","slack_handle":"Alex"},"time":{"local":"2026-05-18T09:00:00-07:00","timezone":"America/Los_Angeles","timezone_label":"Pacific Daylight Time","utc":"2026-05-18T16:00:00Z"}}` + "\n" +
		"</context>\n\n" +
		"<question>\nwhat did I do last week?\n</question>"
	if got != want {
		t.Errorf("envelope mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestFormatContextEnvelope_Minimal: nil asker → UTC-only block. No
// asker line, no asker's-local time, but the user input still
// wrapped in <context>+<question>.
func TestFormatContextEnvelope_Minimal(t *testing.T) {
	now := time.Date(2026, 5, 18, 16, 0, 0, 0, time.UTC)
	got := formatContextEnvelope(nil, now, "", "what is this corpus about?")

	want := "<context>\n" + `{"time":{"utc":"2026-05-18T16:00:00Z"}}` + "\n</context>\n\n" +
		"<question>\nwhat is this corpus about?\n</question>"
	if got != want {
		t.Errorf("envelope mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestFormatContextEnvelope_HandleEqualsDisplayName keeps display and handle
// as distinct fields even when their values happen to match.
func TestFormatContextEnvelope_HandleEqualsDisplayName(t *testing.T) {
	now := time.Date(2026, 5, 18, 16, 0, 0, 0, time.UTC)
	asker := &AskerInfo{
		DisplayName: "Alex",
		Handle:      "Alex", // identical
		Email:       "alex@example.com",
		SlackUserID: "U1",
		LocalTime:   now,
	}
	got := formatContextEnvelope(asker, now, "", "q")
	if !strings.Contains(got, `"display_name":"Alex"`) ||
		!strings.Contains(got, `"slack_handle":"Alex"`) {
		t.Errorf("expected distinct display and handle fields, got:\n%s", got)
	}
	if !strings.Contains(got, `"slack_user_id":"U1"`) {
		t.Errorf("expected slack-id mention, got:\n%s", got)
	}
}

// TestFormatContextEnvelope_AskerWithoutLocalTime: the asker has
// identity (display name, email, slack id) but TZ wasn't yet
// populated in the directory cache. Should render the asker line +
// UTC line, no asker's-local line, and use the minimal-form
// framing prose.
func TestFormatContextEnvelope_AskerWithoutLocalTime(t *testing.T) {
	now := time.Date(2026, 5, 18, 16, 0, 0, 0, time.UTC)
	asker := &AskerInfo{
		DisplayName: "George",
		Email:       "george@example.com",
		SlackUserID: "U2",
		// LocalTime: zero value → TZ unknown
	}
	got := formatContextEnvelope(asker, now, "", "q")
	if !strings.Contains(got, `"asker":{"display_name":"George","email":"george@example.com","slack_user_id":"U2"}`) {
		t.Errorf("missing asker line in:\n%s", got)
	}
	if strings.Contains(got, `"local"`) {
		t.Errorf("should not render asker's-local line when LocalTime is zero:\n%s", got)
	}
	if !strings.Contains(got, `"utc":"2026-05-18T16:00:00Z"`) {
		t.Errorf("expected UTC fallback line:\n%s", got)
	}
}

// TestFormatContextEnvelope_AskerWithoutEmail: asker has display
// name + slack id but no email (rare; bot, or scope didn't return
// it). The email portion should be omitted; the rest still renders.
func TestFormatContextEnvelope_AskerWithoutEmail(t *testing.T) {
	now := time.Date(2026, 5, 18, 16, 0, 0, 0, time.UTC)
	asker := &AskerInfo{
		DisplayName: "Bot Account",
		SlackUserID: "B999",
		LocalTime:   now,
	}
	got := formatContextEnvelope(asker, now, "", "q")
	if !strings.Contains(got, `"display_name":"Bot Account"`) {
		t.Errorf("missing display name:\n%s", got)
	}
	if strings.Contains(got, `"email"`) {
		t.Errorf("unexpected empty email field in:\n%s", got)
	}
	if !strings.Contains(got, `"slack_user_id":"B999"`) {
		t.Errorf("missing slack id:\n%s", got)
	}
}

// A direct-message turn names the channel the asker has open, when the
// surface found the turn may read it.
func TestFormatContextEnvelope_Viewing(t *testing.T) {
	now := time.Date(2026, 5, 18, 16, 0, 0, 0, time.UTC)
	asker := &AskerInfo{SlackUserID: "U0EXAMPLE01", Viewing: &ViewedChannel{ID: "C0EXAMPLE02", Name: "deploys"}}
	got := formatContextEnvelope(asker, now, "", "what broke here?")
	want := "<context>\n" +
		`{"asker":{"slack_user_id":"U0EXAMPLE01"},"viewing":{"slack_channel_id":"C0EXAMPLE02","slack_channel":"#deploys"},"time":{"utc":"2026-05-18T16:00:00Z"}}` + "\n" +
		"</context>\n\n" +
		"<question>\nwhat broke here?\n</question>"
	if got != want {
		t.Errorf("envelope mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatContextEnvelope_AskerWithOnlyEmail(t *testing.T) {
	now := time.Date(2026, 5, 18, 16, 0, 0, 0, time.UTC)
	got := formatContextEnvelope(&AskerInfo{Email: "morgan@example.com"}, now, "", "what did I ask?")
	if !strings.Contains(got, `"email":"morgan@example.com"`) {
		t.Errorf("email-only API identity was dropped:\n%s", got)
	}
}

func TestFormatContextEnvelope_PreservesBareSlackUserID(t *testing.T) {
	now := time.Date(2026, 5, 18, 16, 0, 0, 0, time.UTC)
	got := formatContextEnvelope(&AskerInfo{SlackUserID: "U123"}, now, "", "q")
	if !strings.Contains(got, `"asker":{"slack_user_id":"U123"}`) {
		t.Errorf("bare Slack ID was not preserved as structured identity:\n%s", got)
	}
}

// TestFormatContextEnvelope_WithThreadContext: a non-empty threadContext
// injects a <thread_context> block between </context> and <question>,
// with the framing prose and the transcript lines.
func TestFormatContextEnvelope_WithThreadContext(t *testing.T) {
	now := time.Date(2026, 5, 18, 16, 0, 0, 0, time.UTC)
	asker := &AskerInfo{
		DisplayName: "Chester",
		Email:       "chester@example.com",
		SlackUserID: "U111",
		LocalTime:   now,
	}
	threadCtx := "[Mon 2026-05-18 15:00 UTC] Alice: what's the BGP plan?\n[Mon 2026-05-18 15:01 UTC] Bob: we're doing X\n"
	got := formatContextEnvelope(asker, now, threadCtx, "can you summarize the above?")

	// Block order: <context> then </context> then <thread_context> then <question>.
	contextEnd := strings.Index(got, "</context>")
	threadStart := strings.Index(got, "<thread_context>")
	threadEnd := strings.Index(got, "</thread_context>")
	questionStart := strings.Index(got, "<question>\n")
	if contextEnd < 0 || threadStart < 0 || threadEnd < 0 || questionStart < 0 {
		t.Fatalf("missing required tags in:\n%s", got)
	}
	if contextEnd >= threadStart || threadStart >= threadEnd || threadEnd >= questionStart {
		t.Errorf("block ordering wrong: </context>@%d <thread_context>@%d </thread_context>@%d <question>@%d",
			contextEnd, threadStart, threadEnd, questionStart)
	}
	// Transcript lines present inside the block.
	for _, want := range []string{
		"Alice: what's the BGP plan?",
		"Bob: we're doing X",
		"Slack thread background",
		"oldest first",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
	// Question still present.
	if !strings.Contains(got, "can you summarize the above?") {
		t.Errorf("missing user question in:\n%s", got)
	}
}

// TestFormatContextEnvelope_EmptyThreadContext: empty threadContext
// produces byte-identical output to the old (pre-thread-context)
// format — no <thread_context> block emitted.
func TestFormatContextEnvelope_EmptyThreadContext(t *testing.T) {
	now := time.Date(2026, 5, 18, 16, 0, 0, 0, time.UTC)
	asker := &AskerInfo{
		DisplayName: "Chester",
		Email:       "chester@example.com",
		SlackUserID: "U111",
		LocalTime:   now,
	}
	got := formatContextEnvelope(asker, now, "", "what is this corpus about?")
	if strings.Contains(got, "thread_context") {
		t.Errorf("empty threadContext should not emit <thread_context> block, got:\n%s", got)
	}
}
