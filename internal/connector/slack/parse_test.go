package slack

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeResolver rewrites U-> and #-> refs via simple substitution.
// Enough to verify ParseMessage plumbs through the resolver.
type fakeResolver struct{}

func (fakeResolver) Resolve(_ context.Context, text string) string {
	// Not realistic Slack-speak, but good enough: prepend "[R] " so
	// tests can see the resolver ran.
	return "[R] " + text
}

func TestParsePlainMessage(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "hello world",
		"ts": "1700000000.000100"
	}`)
	res, err := ParseMessage(context.Background(), raw, "example.slack.com", nil)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if res.Delete {
		t.Fatal("plain message should not be a delete")
	}
	if res.Event == nil {
		t.Fatal("plain message should return an Event")
	}
	e := res.Event
	if e.Source != "slack" {
		t.Errorf("Source = %q, want slack", e.Source)
	}
	if e.ExternalID != "C123:1700000000.000100" {
		t.Errorf("ExternalID = %q", e.ExternalID)
	}
	if e.Body != "hello world" {
		t.Errorf("Body = %q", e.Body)
	}
	if e.URL != "https://example.slack.com/archives/C123/p1700000000000100" {
		t.Errorf("URL = %q", e.URL)
	}
	if !e.OccurredAt.Equal(time.Unix(1700000000, 100*1000).UTC()) {
		t.Errorf("OccurredAt = %v", e.OccurredAt)
	}
	if e.ActorID != "U456" {
		t.Errorf("ActorID = %q", e.ActorID)
	}
}

func TestParseThreadReply(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "thread reply",
		"ts": "1700000005.000200",
		"thread_ts": "1700000000.000100"
	}`)
	res, err := ParseMessage(context.Background(), raw, "example.slack.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("expected Event")
	}
	if res.Event.ThreadID != "C123:1700000000.000100" {
		t.Errorf("ThreadID = %q, want C123:1700000000.000100", res.Event.ThreadID)
	}
	wantURL := "https://example.slack.com/archives/C123/p1700000005000200?thread_ts=1700000000.000100&cid=C123"
	if res.Event.URL != wantURL {
		t.Errorf("URL = %q, want %q", res.Event.URL, wantURL)
	}
}

func TestParseEdit(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "message_changed",
		"channel": "C123",
		"message": {
			"type": "message",
			"user": "U456",
			"text": "edited text",
			"ts": "1700000000.000100",
			"thread_ts": "1699999999.000050",
			"edited": {"user": "U456", "ts": "1700000010.000000"}
		},
		"ts": "1700000010.000000"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("edit should produce an Event (Upsert updates in place)")
	}
	if res.Event.ExternalID != "C123:1700000000.000100" {
		// Critical: external_id is the ORIGINAL message's ts, not the edit's.
		// Otherwise the edit would insert a new row instead of updating.
		t.Errorf("edit ExternalID = %q, want original ts", res.Event.ExternalID)
	}
	if res.Event.Body != "edited text" {
		t.Errorf("Body = %q, want edited text", res.Event.Body)
	}
	wantURL := "https://host/archives/C123/p1700000000000100?thread_ts=1699999999.000050&cid=C123"
	if res.Event.URL != wantURL {
		t.Errorf("URL = %q, want %q", res.Event.URL, wantURL)
	}
}

func TestParseDelete(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "message_deleted",
		"channel": "C123",
		"deleted_ts": "1700000000.000100",
		"previous_message": {
			"type": "message",
			"user": "U456",
			"text": "gone",
			"ts": "1700000000.000100"
		},
		"ts": "1700000020.000000"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Delete {
		t.Fatal("message_deleted should return Delete=true")
	}
	if res.Event != nil {
		t.Fatal("message_deleted should NOT return an Event")
	}
	if res.Source != "slack" || res.ExternalID != "C123:1700000000.000100" {
		t.Errorf("delete tuple: %s / %s", res.Source, res.ExternalID)
	}
}

func TestParseMessageChangedTombstone(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "message_changed",
		"channel": "C123",
		"message": {
			"type": "message",
			"subtype": "tombstone",
			"user": "USLACKBOT",
			"text": "This message was deleted.",
			"ts": "1700000000.000100",
			"thread_ts": "1699999999.000050"
		},
		"previous_message": {
			"type": "message",
			"user": "U456",
			"text": "original text",
			"ts": "1700000000.000100",
			"thread_ts": "1699999999.000050"
		},
		"ts": "1700000020.000000"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Delete || res.Event != nil {
		t.Fatalf("tombstone should be a delete, got %+v", res)
	}
	if res.Source != "slack" || res.ExternalID != "C123:1700000000.000100" {
		t.Errorf("delete tuple: %s / %s", res.Source, res.ExternalID)
	}
}

// TestParseUnwrappedTombstone pins the real conversations.history /
// conversations.replies shape for a thread root that was deleted while it
// still had a live reply: Slack can't fully remove the root (something
// must anchor the surviving reply), so it returns a plain top-level
// message AT THAT ts with subtype="tombstone", user="USLACKBOT", and
// reply_count/latest_reply still reflecting the live children. This is
// NOT wrapped in message_changed — contrast with
// TestParseMessageChangedTombstone, which is the LIVE Events API's delta
// shape for the same underlying deletion.
//
// Trigger: a history re-walk (the SyncWorker tail sweep, the backfill
// engine, or ThreadWorker's replies walk) observes a thread root whose
// live delete it never saw (bot disconnected past Slack's redelivery
// window, or any other missed-live-event reason).
// Contract (AGENTS.md "Slack delivery"): "The SyncWorker tail sweep is
// the correctness backstop for messages: ... the edits and deletes that
// redelivery doesn't replay." A deleted message must end up
// res.Delete=true regardless of which of Slack's two tombstone shapes
// carried it.
func TestParseUnwrappedTombstone(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "tombstone",
		"channel": "C123",
		"user": "USLACKBOT",
		"text": "This message was deleted.",
		"ts": "1700000000.000100",
		"thread_ts": "1700000000.000100",
		"reply_count": 1,
		"latest_reply": "1700000010.000200"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatalf("ParseMessage returned an error for an unwrapped tombstone, want a clean delete: %v", err)
	}
	if !res.Delete {
		t.Fatalf("unwrapped subtype=tombstone must parse as a delete; got Delete=false, Event=%+v", res.Event)
	}
	if res.Event != nil {
		t.Fatalf("a delete result must not also carry an Event; got %+v", res.Event)
	}
	if res.Source != "slack" || res.ExternalID != "C123:1700000000.000100" {
		t.Errorf("delete tuple: got source=%q external_id=%q, want slack / C123:1700000000.000100",
			res.Source, res.ExternalID)
	}
}

func TestParseThreadBroadcast(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "thread_broadcast",
		"channel": "C123",
		"user": "U456",
		"text": "broadcast",
		"ts": "1700000050.000100",
		"thread_ts": "1700000000.000100"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("thread_broadcast should produce an Event")
	}
	if res.Event.ThreadID != "C123:1700000000.000100" {
		t.Errorf("thread_broadcast ThreadID = %q", res.Event.ThreadID)
	}
	wantURL := "https://host/archives/C123/p1700000050000100?thread_ts=1700000000.000100&cid=C123"
	if res.Event.URL != wantURL {
		t.Errorf("URL = %q, want %q", res.Event.URL, wantURL)
	}
}

func TestParseUnhandledSubtype(t *testing.T) {
	// channel_join / channel_leave / topic changes / etc: skip, no
	// error. (bot_message is NOT here anymore — see
	// TestParseBotMessageIngested.)
	raw := []byte(`{
		"type": "message",
		"subtype": "channel_join",
		"channel": "C123",
		"user": "U456",
		"text": "<@U456> has joined",
		"ts": "1700000000.000100"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event != nil || res.Delete {
		t.Fatalf("unhandled subtype should be a no-op, got %+v", res)
	}
}

func TestParseResolverPluggedIn(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "plain",
		"ts": "1700000000.000100"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil || res.Event.Body != "[R] plain" {
		t.Fatalf("resolver not applied: %v", res.Event)
	}
}

func TestParseEmptyTextSubstituted(t *testing.T) {
	// A pure-image slack message has no text; the sink requires non-empty Body.
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "",
		"ts": "1700000000.000100"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil || res.Event.Body == "" {
		t.Fatalf("empty-text message should still produce an Event with non-empty Body; got %+v", res.Event)
	}
}

// TestParseNulOnlyTextSubstituted pins that a message whose text is
// nothing but a NUL byte gets the "(empty slack message)" placeholder.
// A NUL is not whitespace to strings.TrimSpace, so checking the raw
// text would treat it as content and leave "\x00" as the Event's
// Body; Sink.Upsert sanitizes that to "" and then fails validate()'s
// "Body is required" check on every attempt, so the message would
// never be indexed. ParseMessage checks
// strings.TrimSpace(events.SanitizeText(text)) instead, so a NUL-only
// message counts as having no text, the same as an empty one.
func TestParseNulOnlyTextSubstituted(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "\u0000",
		"ts": "1700000000.000100"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("NUL-only text message must still produce an Event")
	}
	if res.Event.Body != "(empty slack message)" {
		t.Fatalf("Body = %q, want the empty-message placeholder (NUL-only text has nothing to index)", res.Event.Body)
	}
}

func TestParseFileShareSynthesizesBody(t *testing.T) {
	// Pure file share with no text: body should carry the filenames
	// + permalinks so retrieval can score on the filename and the
	// synthesizer has a URL to cite.
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "",
		"ts": "1700000000.000100",
		"files": [
			{
				"id": "F1",
				"name": "k8s-cluster-errors.png",
				"title": "screenshot of dashboard",
				"permalink": "https://example.slack.com/files/U456/F1/k8s-cluster-errors.png"
			},
			{
				"id": "F2",
				"name": "trace.txt",
				"permalink": "https://example.slack.com/files/U456/F2/trace.txt"
			}
		]
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("expected Event")
	}
	body := res.Event.Body
	for _, want := range []string{
		"[file share]",
		"k8s-cluster-errors.png",
		"screenshot of dashboard",
		"trace.txt",
		"https://example.slack.com/files/U456/F1/k8s-cluster-errors.png",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got %q", want, body)
		}
	}

	details, err := DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !details.HasFiles {
		t.Error("DetailsFrom should set HasFiles=true for file-share messages")
	}
}

func TestParseFileShareSubtypeIngested(t *testing.T) {
	// Messages posted via the files upload API
	// (files.completeUploadExternal / UploadFileV2) arrive with
	// subtype="file_share", unlike the subtype-less shape above. If
	// they fell into the unhandled-subtype skip default, the live path
	// (including ask routing) would never see them: the tail sweep
	// would ingest the text minutes later, but an @-mention in a
	// file-upload message would never be answered.
	raw := []byte(`{
		"type": "message",
		"subtype": "file_share",
		"channel": "C123",
		"user": "U456",
		"text": "<@UBOT> what is this error?",
		"ts": "1700000000.000200",
		"files": [
			{
				"id": "F3",
				"name": "sso_error.png",
				"mimetype": "image/png",
				"permalink": "https://example.slack.com/files/U456/F3/sso_error.png"
			}
		]
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("file_share subtype must produce an Event (was skipped as unhandled subtype)")
	}
	if want := "<@UBOT> what is this error?"; !strings.Contains(res.Event.Body, want) {
		t.Errorf("body missing message text %q; got %q", want, res.Event.Body)
	}
	if res.Event.ExternalID != "C123:1700000000.000200" {
		t.Errorf("external_id = %q, want C123:1700000000.000200", res.Event.ExternalID)
	}
	details, err := DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !details.HasFiles {
		t.Error("DetailsFrom should set HasFiles=true for subtype=file_share")
	}
	if details.UserID != "U456" {
		t.Errorf("UserID = %q, want U456 (ask routing depends on it)", details.UserID)
	}
}

// TestParseFileShareNestedFilesIngested pins that files nested under
// .message are found for subtype="file_share". Slack emits file_share
// in both shapes: files at the top level, and files under .message. A
// collectFiles that unwrapped .message only for
// subtype="message_changed" would parse the nested shape with zero
// files: has_files stays false, no event_images rows are created, and
// nothing reaches the caption or document pipelines.
// TestParseFileShareSubtypeIngested above covers only the top-level
// shape.
//
// The JSON below is a synthetic payload carrying the fields
// collectFiles and DetailsFrom read.
func TestParseFileShareNestedFilesIngested(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "file_share",
		"channel": "C0EXAMPLE10",
		"user": "U0EXAMPLE03",
		"text": "",
		"ts": "1700000300.000100",
		"message": {
			"type": "message",
			"subtype": "file_share",
			"user": "U0EXAMPLE03",
			"ts": "1700000300.000100",
			"text": "",
			"files": [
				{
					"id": "F0EXAMPLE01",
					"name": "document.pdf",
					"title": "document.pdf",
					"mimetype": "application/pdf",
					"filetype": "pdf",
					"mode": "hosted",
					"size": 250000,
					"permalink": "https://example.slack.com/files/U0EXAMPLE03/F0EXAMPLE01/document.pdf"
				}
			]
		}
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("nested file_share must produce an Event")
	}
	// Body must be synthesized from the files list: the message has no
	// text, so without collectFiles finding the nested files the body
	// would be the "(empty slack message)" placeholder.
	if !strings.Contains(res.Event.Body, "document.pdf") {
		t.Errorf("body must name the attached file; got %q", res.Event.Body)
	}
	if strings.Contains(res.Event.Body, "(empty slack message)") {
		t.Errorf("body fell back to the empty placeholder: %q", res.Event.Body)
	}

	details, err := DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !details.HasFiles {
		t.Error("HasFiles must be true for nested file_share (drives event_images reconcile)")
	}
	if len(details.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(details.Attachments))
	}
	att := details.Attachments[0]
	if att.FileID != "F0EXAMPLE01" {
		t.Errorf("FileID = %q, want F0EXAMPLE01", att.FileID)
	}
	if att.Mimetype != "application/pdf" {
		t.Errorf("Mimetype = %q, want application/pdf", att.Mimetype)
	}
}

// TestParseNestedFilesAnySubtype covers the general shape of the fix:
// the nested fallback must not be gated on a subtype allowlist,
// because Slack adds subtypes without notice. thread_broadcast also
// arrives with nested files.
func TestParseNestedFilesAnySubtype(t *testing.T) {
	for _, subtype := range []string{
		"file_share",
		"thread_broadcast",
		"message_changed",
		"some_future_subtype_slack_has_not_invented_yet",
	} {
		t.Run(subtype, func(t *testing.T) {
			raw := []byte(`{
				"type": "message",
				"subtype": "` + subtype + `",
				"channel": "C123",
				"user": "U456",
				"ts": "1700000000.000300",
				"message": {
					"user": "U456",
					"ts": "1700000000.000300",
					"text": "here it is",
					"files": [
						{"id": "F9", "name": "notes.pdf", "mimetype": "application/pdf", "mode": "hosted"}
					]
				}
			}`)
			details, err := DetailsFrom(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !details.HasFiles {
				t.Errorf("subtype=%s: nested files not found", subtype)
			}
			if len(details.Attachments) != 1 || details.Attachments[0].FileID != "F9" {
				t.Errorf("subtype=%s: attachments = %+v", subtype, details.Attachments)
			}
		})
	}
}

// TestParseDeletePrefersPreviousMessageFiles pins the one subtype
// where nesting precedence is load-bearing. On message_deleted Slack
// can send both .previous_message (the pre-delete state, holding the
// files that were actually shared) and .message (the tombstone).
// collectFiles must prefer .previous_message there; every other
// subtype prefers .message.
func TestParseDeletePrefersPreviousMessageFiles(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "message_deleted",
		"channel": "C123",
		"deleted_ts": "1700000000.000400",
		"ts": "1700000099.000000",
		"previous_message": {
			"user": "U1",
			"ts": "1700000000.000400",
			"files": [{"id": "FPREV", "name": "real.pdf", "mimetype": "application/pdf"}]
		},
		"message": {
			"user": "U1",
			"ts": "1700000000.000400",
			"files": [{"id": "FTOMB", "name": "tombstone.pdf", "mimetype": "application/pdf"}]
		}
	}`)
	details, err := DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Attachments) != 1 {
		t.Fatalf("want 1 attachment, got %d", len(details.Attachments))
	}
	if got := details.Attachments[0].FileID; got != "FPREV" {
		t.Errorf("FileID = %q, want FPREV (previous_message wins on delete)", got)
	}
}

func TestParseBotMessageIngested(t *testing.T) {
	// Classic webhook/integration output (Alertmanager, CI, GitHub,
	// Grafana) arrives with subtype="bot_message", empty top-level
	// text, and all content in attachments[].text/.fallback. If these
	// fell into the unhandled-subtype skip default, a bot-heavy alerts
	// channel would look frozen at its last human/app-bot message while
	// most of its traffic was dropped. The attachment-folding body
	// composition has to be reachable for this subtype.
	raw := []byte(`{
		"type": "message",
		"subtype": "bot_message",
		"channel": "C0EXAMPLE03",
		"bot_id": "B0EXAMPLE02",
		"username": "Alertmanager",
		"text": "",
		"ts": "1700000200.000100",
		"attachments": [
			{
				"color": "danger",
				"title": "[FIRING:2] Disk almost full",
				"text": "Disk usage is above 90% on node-1.example.internal",
				"fallback": "[FIRING:2] Disk almost full",
				"footer": "Alertmanager"
			}
		]
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("bot_message must produce an Event (was skipped as unhandled subtype)")
	}
	if res.Event.ExternalID != "C0EXAMPLE03:1700000200.000100" {
		t.Errorf("external_id = %q", res.Event.ExternalID)
	}
	// Content lives entirely in the attachment; the fallback/title/text
	// must be folded into the body so the alert is findable.
	body := res.Event.Body
	for _, want := range []string{"[FIRING:2] Disk almost full", "Disk usage is above 90%"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got %q", want, body)
		}
	}
	if strings.Contains(body, "(empty slack message)") {
		t.Errorf("placeholder should not appear when an attachment provides content; got %q", body)
	}
	// bot_message carries no human `user`; the author id lives in
	// `bot_id`. Preserve it as the event's source identity without
	// changing slack_message_details.user_id (tested separately below).
	if res.Event.ActorID != "B0EXAMPLE02" {
		t.Errorf("ActorID = %q, want B0EXAMPLE02", res.Event.ActorID)
	}
	if res.Event.ActorCanonical != "slack-bot:B0EXAMPLE02" {
		t.Errorf("ActorCanonical = %q, want slack-bot:B0EXAMPLE02", res.Event.ActorCanonical)
	}
	if res.Event.Actor != "Alertmanager" {
		t.Errorf("Actor = %q, want Alertmanager (from username)", res.Event.Actor)
	}
}

func TestParseChangedBotMessageIdentity(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "message_changed",
		"channel": "C123",
		"message": {
			"type": "message",
			"subtype": "bot_message",
			"bot_id": "BCHANGE",
			"bot_profile": {"name": "Deploy notifier"},
			"text": "deployment finished",
			"ts": "1700000200.000100"
		}
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("changed bot message must produce an event")
	}
	if res.Event.ActorID != "BCHANGE" || res.Event.ActorCanonical != "slack-bot:BCHANGE" {
		t.Errorf("identity = (%q, %q)", res.Event.ActorID, res.Event.ActorCanonical)
	}
	if res.Event.Actor != "Deploy notifier" {
		t.Errorf("Actor = %q, want Deploy notifier", res.Event.Actor)
	}
}

func TestParseTopLevelBotIDWithoutBotSubtype(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"bot_profile": {"id": "BAPP", "name": "Build app"},
		"text": "build finished",
		"ts": "1700000200.000100"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event.ActorID != "BAPP" || res.Event.ActorCanonical != "slack-bot:BAPP" {
		t.Errorf("identity = (%q, %q)", res.Event.ActorID, res.Event.ActorCanonical)
	}
	if res.Event.Actor != "Build app" {
		t.Errorf("Actor = %q, want Build app", res.Event.Actor)
	}
	details, err := DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if details.UserID != "" {
		t.Errorf("DetailsFrom UserID = %q, want empty for app-authored message", details.UserID)
	}
}

func TestBotProfileNeverBecomesHumanAsker(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "UAPPBOT",
		"bot_profile": {"name": "Build app"},
		"text": "build finished",
		"ts": "1700000200.000100"
	}`)
	details, err := DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if details.UserID != "" {
		t.Errorf("DetailsFrom UserID = %q, want empty for bot_profile message", details.UserID)
	}
}

func TestBotDisplayWithoutStableIdentityRejected(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"bot_profile": {"name": "Unknown app"},
		"text": "build finished",
		"ts": "1700000200.000100"
	}`)
	if _, err := ParseMessage(context.Background(), raw, "host", nil); err == nil {
		t.Fatal("expected bot message without bot_id or user to be rejected")
	}
}

func TestParseSlackbotIdentity(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "bot_message",
		"channel": "C123",
		"user": "USLACKBOT",
		"username": "slackbot",
		"text": "A reminder",
		"ts": "1700000200.000100"
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event.ActorID != "USLACKBOT" || res.Event.ActorCanonical != "slack-bot:USLACKBOT" {
		t.Errorf("identity = (%q, %q)", res.Event.ActorID, res.Event.ActorCanonical)
	}
	if res.Event.Actor != "slackbot" {
		t.Errorf("Actor = %q, want slackbot", res.Event.Actor)
	}
	details, err := DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if details.UserID != "" {
		t.Errorf("DetailsFrom UserID = %q, want empty for Slackbot", details.UserID)
	}
}

func TestDetailsFromChangedBotMessageLeavesHumanUserEmpty(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "message_changed",
		"channel": "C123",
		"message": {
			"type": "message",
			"subtype": "bot_message",
			"user": "USLACKBOT",
			"text": "edited reminder",
			"ts": "1700000200.000100",
			"thread_ts": "1700000100.000001",
			"edited": {"user": "USLACKBOT", "ts": "1700000201.000001"}
		}
	}`)
	details, err := DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if details.UserID != "" {
		t.Errorf("UserID = %q, want empty for changed bot message", details.UserID)
	}
	if !details.IsEdit || details.ThreadTS != "1700000100.000001" || details.EditedTS != "1700000201.000001" {
		t.Errorf("edit metadata lost: %+v", details)
	}
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event.ActorID != "USLACKBOT" || res.Event.ActorCanonical != "slack-bot:USLACKBOT" {
		t.Errorf("event identity = (%q, %q)", res.Event.ActorID, res.Event.ActorCanonical)
	}
}

func TestParseBotMessageThreadReplyAnchors(t *testing.T) {
	// A bot_message that is itself a thread root: the human replies that
	// follow carry thread_ts pointing at this ts. If the root is
	// dropped, those replies orphan. Verify the root's
	// ThreadID resolves to channel:ts so a reply's channel:thread_ts
	// matches it.
	raw := []byte(`{
		"type": "message",
		"subtype": "bot_message",
		"channel": "C0EXAMPLE03",
		"bot_id": "B0EXAMPLE02",
		"username": "Alertmanager",
		"text": "",
		"ts": "1700000250.000100",
		"thread_ts": "1700000250.000100",
		"attachments": [
			{"title": "[FIRING:1] Queue depth is growing", "fallback": "[FIRING:1] Queue depth"}
		]
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("bot_message thread root must produce an Event")
	}
	if res.Event.ThreadID != "C0EXAMPLE03:1700000250.000100" {
		t.Errorf("ThreadID = %q, want C0EXAMPLE03:1700000250.000100 (anchors human replies)", res.Event.ThreadID)
	}
}

func TestParseBotMessageDetailsUserFromBotID(t *testing.T) {
	// DetailsFrom must not blow up on a bot_message and should leave
	// UserID empty (ask routing keys on a real user id; a bot alert is
	// never an ask). HasFiles false, IsEdit false.
	raw := []byte(`{
		"type": "message",
		"subtype": "bot_message",
		"channel": "C0EXAMPLE03",
		"bot_id": "B0EXAMPLE02",
		"username": "Alertmanager",
		"text": "",
		"ts": "1700000200.000100",
		"attachments": [{"fallback": "[FIRING:2] Disk almost full"}]
	}`)
	d, err := DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.UserID != "" {
		t.Errorf("UserID = %q, want empty for bot_message", d.UserID)
	}
	if d.IsEdit || d.HasFiles {
		t.Errorf("got IsEdit=%v HasFiles=%v, want both false", d.IsEdit, d.HasFiles)
	}
	if d.ChannelID != "C0EXAMPLE03" {
		t.Errorf("ChannelID = %q", d.ChannelID)
	}
}

func TestParseUnfurlFoldedIntoBody(t *testing.T) {
	// A message with a link that Slack unfurled: the preview card
	// arrives as an `attachments` entry. Its author + preview text +
	// underlying URL should be folded into the body so the linked post
	// is findable by its content, while the user's own text still leads.
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "check this out",
		"ts": "1700000000.000100",
		"attachments": [
			{
				"service_name": "bluesky",
				"author_name": "Alice Example",
				"author_subname": "@alice.example.com",
				"text": "the release notes are up",
				"fallback": "@alice.example.com: the release notes are up",
				"from_url": "https://bsky.app/profile/alice.example.com/post/3kexamplepost",
				"image_url": "https://cdn.bsky.app/img/feed_thumbnail/plain/whatever"
			}
		]
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("expected Event")
	}
	body := res.Event.Body
	if !strings.HasPrefix(body, "check this out") {
		t.Errorf("user text should lead the body; got %q", body)
	}
	for _, want := range []string{
		"[link] bluesky",
		"Alice Example (@alice.example.com)",
		"the release notes are up",
		"https://bsky.app/profile/alice.example.com/post/3kexamplepost",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got %q", want, body)
		}
	}
	// Media/layout noise should not leak into the body.
	if strings.Contains(body, "cdn.bsky.app/img") {
		t.Errorf("image_url should not appear in body; got %q", body)
	}
}

func TestParseUnfurlOnMessageChanged(t *testing.T) {
	// Slack delivers a post-crawl unfurl as a message_changed whose
	// attachments hang off the nested `message`, not the top-level
	// event. collectAttachments must reach into m.Message.
	raw := []byte(`{
		"type": "message",
		"subtype": "message_changed",
		"channel": "C123",
		"message": {
			"type": "message",
			"user": "U456",
			"text": "look at this post",
			"ts": "1700000000.000100",
			"attachments": [
				{
					"service_name": "bluesky",
					"text": "the linked post body",
					"from_url": "https://bsky.app/profile/x/post/y"
				}
			]
		}
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("expected Event")
	}
	for _, want := range []string{"look at this post", "[link] bluesky", "the linked post body"} {
		if !strings.Contains(res.Event.Body, want) {
			t.Errorf("body missing %q; got %q", want, res.Event.Body)
		}
	}
}

// TestParseUnfurlNestedUnderAnyIngestedSubtype is the
// collectAttachments twin of TestParseNestedFilesAnySubtype. The old
// implementation gated the nested lookup on
// subtype == "message_changed", which is exactly the shape that lost 49
// images and a PDF on the files side.
//
// Scope note, learned by writing this wrong first: an *unknown* subtype
// is deliberately skipped before body composition ever runs
// (parse.go's default case), so this cannot assert "any subtype at
// all" — that would be testing against a policy we want. What it
// asserts is that every subtype we DO ingest finds nested attachments,
// which is the property the subtype-gated version broke. file_share is
// the pointed case: Slack already nests *files* under .message for it.
//
// Negative control: reintroducing the `m.Subtype == "message_changed"`
// guard turns every subtype below except message_changed red on the
// "[link] bluesky" assertion.
func TestParseUnfurlNestedUnderAnyIngestedSubtype(t *testing.T) {
	for _, subtype := range []string{
		"message_changed",
		"thread_broadcast",
		"file_share",
		"", // no subtype at all, attachments nested anyway
	} {
		t.Run("subtype="+subtype, func(t *testing.T) {
			// The envelope carries its own ts: only message_changed
			// resolves ts from the nested message, so a fixture without
			// one would fail on identity rather than on attachments,
			// which is not what this test is about.
			raw := []byte(`{
				"type": "message",
				"subtype": "` + subtype + `",
				"channel": "C123",
				"user": "U456",
				"text": "look at this post",
				"ts": "1700000000.000100",
				"message": {
					"type": "message",
					"user": "U456",
					"text": "look at this post",
					"ts": "1700000000.000100",
					"attachments": [
						{
							"service_name": "bluesky",
							"text": "the linked post body",
							"from_url": "https://bsky.app/profile/x/post/y"
						}
					]
				}
			}`)
			res, err := ParseMessage(context.Background(), raw, "host", nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Event == nil {
				t.Fatalf("subtype=%s: expected Event", subtype)
			}
			if !strings.Contains(res.Event.Body, "[link] bluesky") {
				t.Errorf("subtype=%s: nested attachments not found; body=%q",
					subtype, res.Event.Body)
			}
			if !strings.Contains(res.Event.Body, "the linked post body") {
				t.Errorf("subtype=%s: unfurl text lost; body=%q",
					subtype, res.Event.Body)
			}
		})
	}
}

// TestParseUnfurlTopLevelWinsOverNested pins the precedence, which is
// the one thing a "find it anywhere" search can get wrong: when both
// shapes carry attachments, the top-level set is the current state.
func TestParseUnfurlTopLevelWinsOverNested(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "message_changed",
		"channel": "C123",
		"user": "U456",
		"text": "top level text",
		"ts": "1700000000.000100",
		"attachments": [
			{"service_name": "current", "text": "the current unfurl", "from_url": "https://a"}
		],
		"message": {
			"type": "message",
			"user": "U456",
			"text": "nested text",
			"ts": "1700000000.000100",
			"attachments": [
				{"service_name": "stale", "text": "the stale unfurl", "from_url": "https://b"}
			]
		}
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("expected Event")
	}
	if !strings.Contains(res.Event.Body, "the current unfurl") {
		t.Errorf("top-level attachments must win; body=%q", res.Event.Body)
	}
	if strings.Contains(res.Event.Body, "the stale unfurl") {
		t.Errorf("nested attachments must not be used when top-level exist; body=%q",
			res.Event.Body)
	}
}

func TestParseBareLinkUnfurlProvidesBody(t *testing.T) {
	// A message that is *just* a URL: after Slack unfurls it, the
	// preview text is the only real retrieval signal. The bare URL text
	// leads, the unfurl block follows.
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "https://bsky.app/profile/x/post/y",
		"ts": "1700000000.000100",
		"attachments": [
			{"service_name": "bluesky", "text": "the actual post content", "from_url": "https://bsky.app/profile/x/post/y"}
		]
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil || !strings.Contains(res.Event.Body, "the actual post content") {
		t.Errorf("bare-link unfurl content missing from body; got %q", res.Event.Body)
	}
}

func TestParseEmptyTextWithUnfurlNoPlaceholder(t *testing.T) {
	// Empty text + an unfurl (e.g. a message_changed that carries only
	// the crawl result): the unfurl block is the body, and the
	// "(empty slack message)" placeholder must NOT be stamped over it.
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "",
		"ts": "1700000000.000100",
		"attachments": [
			{"service_name": "bluesky", "text": "post content here", "from_url": "https://bsky.app/profile/x/post/y"}
		]
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil {
		t.Fatal("expected Event")
	}
	body := res.Event.Body
	if strings.Contains(body, "(empty slack message)") {
		t.Errorf("placeholder should not appear when an unfurl provides content; got %q", body)
	}
	if !strings.HasPrefix(body, "[link] bluesky") || !strings.Contains(body, "post content here") {
		t.Errorf("unfurl should be the body; got %q", body)
	}
}

func TestParseMediaOnlyAttachmentSkipped(t *testing.T) {
	// A pure-image unfurl with no title/text/fallback carries no
	// retrieval signal; the body should be unchanged (no dangling
	// "[link]" header).
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "a photo",
		"ts": "1700000000.000100",
		"attachments": [
			{"service_name": "imgur", "image_url": "https://i.imgur.com/x.png", "from_url": "https://imgur.com/x"}
		]
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil || res.Event.Body != "a photo" {
		t.Errorf("media-only attachment should leave body unchanged; got %q", res.Event.Body)
	}
}

func TestParseTextMessageWithFileAttachment(t *testing.T) {
	// A message that has both text and files: keep the text as body
	// (files are supplementary), but DetailsFrom still flags HasFiles
	// so a file-specific query can find the row.
	raw := []byte(`{
		"type": "message",
		"channel": "C123",
		"user": "U456",
		"text": "here's the repro",
		"ts": "1700000000.000100",
		"files": [{"id": "F1", "name": "repro.txt"}]
	}`)
	res, err := ParseMessage(context.Background(), raw, "host", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil || res.Event.Body != "here's the repro" {
		t.Errorf("text+file: body should be the text, got %q", res.Event.Body)
	}
	details, _ := DetailsFrom(raw)
	if !details.HasFiles {
		t.Error("HasFiles should still be true on text+file messages")
	}
}

func TestParseMissingChannelErrors(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"user": "U456",
		"text": "hi",
		"ts": "1700000000.000100"
	}`)
	_, err := ParseMessage(context.Background(), raw, "host", nil)
	if err == nil {
		t.Fatal("missing channel should error")
	}
}

func TestParseMessageChangedWithoutMessageErrors(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"subtype": "message_changed",
		"channel": "C123"
	}`)
	_, err := ParseMessage(context.Background(), raw, "host", nil)
	if err == nil {
		t.Fatal("message_changed without message body should error")
	}
}

func TestParseMalformedJSONErrors(t *testing.T) {
	_, err := ParseMessage(context.Background(), []byte(`not json`), "host", nil)
	if err == nil {
		t.Fatal("invalid JSON should error")
	}
}

func TestTsToTime(t *testing.T) {
	cases := []struct {
		ts   string
		want time.Time
	}{
		{"1700000000.000100", time.Unix(1700000000, 100*1000).UTC()},
		{"1700000000.5", time.Unix(1700000000, 500000*1000).UTC()},
		{"1700000000", time.Unix(1700000000, 0).UTC()},
	}
	for _, tc := range cases {
		got, err := tsToTime(tc.ts)
		if err != nil {
			t.Errorf("%q: %v", tc.ts, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%q: got %v, want %v", tc.ts, got, tc.want)
		}
	}
}

func TestPermalink(t *testing.T) {
	tests := []struct {
		name     string
		ts       string
		threadTS string
		want     string
	}{
		{
			name: "standalone",
			ts:   "1700000000.000100",
			want: "https://example.slack.com/archives/C123/p1700000000000100",
		},
		{
			name:     "reply",
			ts:       "1700000050.000200",
			threadTS: "1700000000.000100",
			want:     "https://example.slack.com/archives/C123/p1700000050000200?thread_ts=1700000000.000100&cid=C123",
		},
		{
			name:     "thread root",
			ts:       "1700000000.000100",
			threadTS: "1700000000.000100",
			want:     "https://example.slack.com/archives/C123/p1700000000000100?thread_ts=1700000000.000100&cid=C123",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := permalink("example.slack.com", "C123", tc.ts, tc.threadTS)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if permalink("", "C123", "1.1", "") != "" {
		t.Error("empty host should return empty permalink")
	}
}

func TestDetailsFrom(t *testing.T) {
	// message_changed: is_edit = true, user + thread_ts from message.
	raw := []byte(`{
		"type": "message",
		"subtype": "message_changed",
		"channel": "C123",
		"message": {"user": "U1", "ts": "1.1", "thread_ts": "0.1", "text": "t"}
	}`)
	d, err := DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !d.IsEdit || d.UserID != "U1" || d.ThreadTS != "0.1" || d.ChannelID != "C123" {
		t.Errorf("got %+v", d)
	}

	// message_deleted: is_edit=false, user from previous_message.
	raw = []byte(`{
		"type": "message",
		"subtype": "message_deleted",
		"channel": "C123",
		"deleted_ts": "1.1",
		"previous_message": {"user": "U1", "ts": "1.1"}
	}`)
	d, err = DetailsFrom(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.IsEdit || d.UserID != "U1" {
		t.Errorf("got %+v", d)
	}
}
