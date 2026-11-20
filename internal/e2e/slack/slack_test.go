//go:build e2e

package slack

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/ask"
	slackgo "github.com/slack-go/slack"
)

// Sentinels wrap boolean SQL results so parsing psql's aligned output is
// unambiguous — the string can only appear on the data line, never in a
// header, separator rule, or timestamp value.
const (
	captionSentinel = "CAPTION_WRITTEN"
	orderSentinel   = "CAPTION_BEFORE_ANSWER"
	markerSentinel  = "MARKER_COUNT"
)

// TestSlackCaptionBarrierE2E posts a fixture screenshot + @diderot mention
// as the operator's user token into the test channel, then asserts the
// caption-barrier invariant:
//
//	caption was written BEFORE the assistant turn,
//	and the bot's reply references the known image content.
//
// Gate: DIDEROT_E2E_SLACK=1 + DIDEROT_SLACK_XOXP_TOKEN non-empty.
// Run:  just test-slack-e2e
func TestSlackCaptionBarrierE2E(t *testing.T) {
	// ── gates ────────────────────────────────────────────────────────────
	if os.Getenv("DIDEROT_E2E_SLACK") != "1" {
		t.Skip("DIDEROT_E2E_SLACK not set to 1 — skipping real-Slack e2e test")
	}
	xoxpToken := os.Getenv("DIDEROT_SLACK_XOXP_TOKEN")
	if xoxpToken == "" {
		t.Skip("DIDEROT_SLACK_XOXP_TOKEN not set — skipping real-Slack e2e test")
	}
	requireSlackTarget(t)
	requireDeploymentSQL(t)

	// ── setup ─────────────────────────────────────────────────────────────
	ctx := context.Background()
	client := slackgo.New(xoxpToken)

	// ── phase 1: post the test message ───────────────────────────────────
	t.Log("phase 1: posting fixture image + @diderot mention to the test channel")
	postCtx, postCancel := context.WithTimeout(ctx, 30*time.Second)
	defer postCancel()

	messageTS, err := postTestMessage(postCtx, client)
	if err != nil {
		t.Fatalf("failed to post test message: %v", err)
	}

	permalink := slackPermalink(testChannelID, messageTS)
	t.Logf("posted message ts=%s permalink=%s", messageTS, permalink)

	// ── phase 2: wait for the bot's reply ────────────────────────────────
	t.Log("phase 2: polling for bot reply (up to 120s)...")
	pollCtx, pollCancel := context.WithTimeout(ctx, pollTimeout+5*time.Second)
	defer pollCancel()

	replyText, pollErr := pollForBotReply(pollCtx, client, messageTS)
	if pollErr != nil {
		t.Fatalf("no bot reply: %v — check %s", pollErr, permalink)
	}
	t.Logf("bot replied: %q", truncate(replyText, 200))

	// ── phase 3: assertions ───────────────────────────────────────────────
	t.Log("phase 3: running SQL assertions against the deployment's DB")

	extID := externalID(testChannelID, messageTS)

	// 3a. Caption was written: event_images.captioned_at IS NOT NULL.
	assertCaptionWritten(t, extID)

	// 3b. Happens-before: captioned_at <= assistant ask_messages.created_at.
	// NOTE: this is a WEAK signal on its own. A turn that never reads
	// the caption can still be written after it, so this also passes
	// when the caption reaches nothing. Kept as a cheap sanity check;
	// 3c is the real discriminator.
	assertHappensBefore(t, testChannelID, messageTS)

	// 3c. THE discriminating assertion: the caption text actually reached
	// the model. The ask path appends "[attached image: <caption>]" to
	// the user turn, which is persisted verbatim into the v1
	// AgenticMessage JSONB payload of the user turn
	// (ask_messages.message). A turn built only from events.body never
	// carries this marker, whether the caption poll was fast or slow.
	// So the check is timing-independent: it passes even when the
	// barrier records no_wait, and fails whenever the caption is not
	// injected.
	assertCaptionReachedModel(t, testChannelID, messageTS)

	// 3c. Answer reflects the image (hard pass condition).
	t.Logf("checking that reply references known keyword %q", knownImageKeyword)
	if !strings.Contains(strings.ToUpper(replyText), strings.ToUpper(knownImageKeyword)) {
		t.Errorf("bot reply does not reference known image keyword %q; reply: %q",
			knownImageKeyword, truncate(replyText, 400))
	} else {
		t.Logf("PASS: reply references %q", knownImageKeyword)
	}

	// 3d. Soft signal: barrier metrics / log lines (diagnostic only).
	logBarrierSignal(t, messageTS)

	t.Logf("thread permalink for forensics: %s", permalink)
}

// assertCaptionWritten checks that the event_images row for extID has
// captioned_at IS NOT NULL.
func assertCaptionWritten(t *testing.T, extID string) {
	t.Helper()
	// Emit a self-labeling sentinel rather than a bare boolean: psql's
	// aligned output includes column headers and separator rules, so
	// scanning for "t"/"f" is ambiguous (headers and timestamps contain
	// those letters). A sentinel string can only appear on the data line
	// and can't collide with any header, rule, or timestamp value.
	query := fmt.Sprintf(
		"SELECT '%s=' || (captioned_at IS NOT NULL)::text "+
			"FROM event_images ei "+
			"JOIN events e ON e.id = ei.event_id "+
			"WHERE e.external_id = '%s' "+
			"LIMIT 1;",
		captionSentinel, extID,
	)
	out, err := sqlDeployment(context.Background(), query)
	if err != nil {
		t.Fatalf("SQL error checking event_images for %s: %v", extID, err)
	}
	t.Logf("event_images captioned query result:\n%s", out)
	switch {
	case strings.Contains(out, captionSentinel+"=true"):
		t.Logf("PASS: event_images.captioned_at is NOT NULL for external_id=%s", extID)
	case strings.Contains(out, captionSentinel+"=false"):
		t.Errorf("FAIL: event_images.captioned_at is NULL for external_id=%s; caption barrier invariant broken", extID)
	default:
		t.Errorf("FAIL: no event_images row found for external_id=%s (ingest/reconcile missing?); output:\n%s", extID, out)
	}
}

// assertHappensBefore checks the happens-before ordering:
//
//	event_images.captioned_at <= ask_messages.created_at (assistant role)
func assertHappensBefore(t *testing.T, channelID, threadTS string) {
	t.Helper()
	// Look up the session via ask_slack_sessions, then the assistant turn.
	// Select the earliest assistant turn and compare against the caption
	// time. Sentinel-wrap the boolean (see assertCaptionWritten for why).
	// The bare captioned_at / created_at values are logged for forensics
	// but the pass/fail decision keys only on the sentinel.
	query := fmt.Sprintf(
		"SELECT '%s=' || (ei.captioned_at <= am.created_at)::text, "+
			"  ei.captioned_at, am.created_at "+
			"FROM ask_slack_sessions ass "+
			"JOIN ask_messages am ON am.session_id = ass.session_id "+
			"JOIN events ev ON ev.external_id = '%s:%s' "+
			"JOIN event_images ei ON ei.event_id = ev.id "+
			"WHERE ass.channel_id = '%s' "+
			"  AND ass.thread_ts = '%s' "+
			"  AND am.role = 'assistant' "+
			"ORDER BY am.created_at "+
			"LIMIT 1;",
		orderSentinel,
		channelID, threadTS,
		channelID, threadTS,
	)
	out, err := sqlDeployment(context.Background(), query)
	if err != nil {
		t.Fatalf("SQL error checking happens-before for channel=%s thread=%s: %v", channelID, threadTS, err)
	}
	t.Logf("happens-before query result:\n%s", out)
	switch {
	case strings.Contains(out, orderSentinel+"=true"):
		t.Logf("PASS: captioned_at <= ask_messages.created_at")
	case strings.Contains(out, orderSentinel+"=false"):
		t.Errorf("FAIL: happens-before NOT satisfied: caption was written AFTER the assistant turn ran")
	default:
		t.Errorf("FAIL: no row for happens-before query — ask session, event, or image row missing; output:\n%s", out)
	}
}

// assertCaptionReachedModel is the discriminating assertion: it proves the
// image caption was actually injected into what the model saw, not merely
// that a caption row exists in the DB.
//
// The ask path appends "[attached image: <caption>]" to the user turn's
// text, and the ask runner persists that augmented text verbatim in the
// v1 AgenticMessage payload of the user turn's ask_messages row
// (message_schema_version = 1). So a user-role row whose decoded message
// text contains the marker proves the caption reached the model. A turn
// built only from events.body has no such marker, whether the caption
// poll was fast or slow, so this also catches the fast-poll case where
// the timing-based checks pass vacuously.
func assertCaptionReachedModel(t *testing.T, channelID, threadTS string) {
	t.Helper()
	// Hex-encode the canonical JSON bytes so every row stays on one line in
	// psql's aligned output. The sentinel lets the parser ignore headers,
	// separator rules, and row-count footers without mistaking them for data.
	query := fmt.Sprintf(
		"SELECT '%s=' || am.message_schema_version || '|' || "+
			"  encode(convert_to(am.message::text, 'UTF8'), 'hex') "+
			"FROM ask_slack_sessions ass "+
			"JOIN ask_messages am ON am.session_id = ass.session_id "+
			"WHERE ass.channel_id = '%s' "+
			"  AND ass.thread_ts = '%s' "+
			"  AND am.role = 'user' "+
			"ORDER BY am.turn;",
		markerSentinel, channelID, threadTS,
	)
	out, err := sqlDeployment(context.Background(), query)
	if err != nil {
		t.Fatalf("SQL error checking caption-reached-model for channel=%s thread=%s: %v", channelID, threadTS, err)
	}
	count, marked, decodeErrs, schemaErrs, err := countMarkerRows(out)
	if err != nil {
		t.Fatalf("could not parse caption-reached-model SQL: %v; output:\n%s", err, out)
	}
	t.Logf("caption-reached-model rows: %d total, %d carrying the mention-image marker, %d decode failures, %d bad schema versions",
		count, marked, decodeErrs, schemaErrs)
	if schemaErrs > 0 {
		t.Errorf("FAIL: %d user turn(s) had a message_schema_version other than %d; canonical decode skipped",
			schemaErrs, ask.AgenticMessageSchemaVersion)
	}
	if decodeErrs > 0 {
		t.Errorf("FAIL: %d user turn(s) failed canonical AgenticMessage decode — persisted rows must be valid v1",
			decodeErrs)
	}
	if count == 0 {
		t.Fatalf("FAIL: caption-reached-model query returned no %s rows — SQL error, schema drift, "+
			"or no ask session for channel=%s thread=%s (not the caption-bug signature); output:\n%s",
			markerSentinel, channelID, threadTS, out)
	}
	if marked == 0 {
		t.Errorf("FAIL: %d v1 user AgenticMessage(s) were present but none carried the '[attached image:' marker — "+
			"the caption never reached the model", count)
	} else {
		t.Logf("PASS: caption text reached the model (v1 user AgenticMessage carries the mention-image marker)")
	}
}

// countMarkerRows extracts sentinel-framed rows from psql's aligned output,
// validates their schema version, and decodes the canonical AgenticMessage.
// A malformed sentinel row fails; unrelated psql decoration is ignored.
func countMarkerRows(out string) (count, marked, decodeErrs, schemaErrs int, err error) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		payload, ok := strings.CutPrefix(line, markerSentinel+"=")
		if !ok {
			continue
		}
		version, encoded, ok := splitDataLine(payload)
		if !ok {
			return 0, 0, 0, 0, fmt.Errorf("unexpected data line %q", line)
		}
		count++
		if version != int(ask.AgenticMessageSchemaVersion) {
			schemaErrs++
			continue
		}
		raw, decErr := hex.DecodeString(encoded)
		if decErr != nil {
			decodeErrs++
			continue
		}
		msg, decErr := ask.DecodeAgenticMessage(raw)
		if decErr != nil {
			decodeErrs++
			continue
		}
		if strings.Contains(ask.AgenticMessageText(msg), "[attached image:") {
			marked++
		}
	}
	return count, marked, decodeErrs, schemaErrs, nil
}

func splitDataLine(line string) (version int, encoded string, ok bool) {
	separator := strings.IndexByte(line, '|')
	if separator <= 0 || separator == len(line)-1 {
		return 0, "", false
	}
	versionRaw, payload := line[:separator], line[separator+1:]
	v, err := strconv.Atoi(strings.TrimSpace(versionRaw))
	if err != nil {
		return 0, "", false
	}
	return v, payload, true
}

func TestSlackCountMarkerRows(t *testing.T) {
	markedPayload := encodeHexForTest(t, "what is this error?\n\n[attached image: an error dialog]")
	plainPayload := encodeHexForTest(t, "how does the corpus work?")
	garbage := hex.EncodeToString([]byte("not-json"))

	cases := []struct {
		name          string
		out           string
		wantCount     int
		wantMarked    int
		wantDecodeErr int
		wantSchemaErr int
		wantScanErr   bool
	}{
		{
			name:      "marked row passed the model input",
			out:       " ?column?\n----------\n " + markerSentinel + "=1|" + markedPayload + "\n(1 row)\n",
			wantCount: 1, wantMarked: 1,
		},
		{
			name:      "plain user row is counted but not marked",
			out:       markerSentinel + "=1|" + plainPayload + "\n(1 row)\n",
			wantCount: 1, wantMarked: 0,
		},
		{
			name:      "bad schema version is counted separately",
			out:       markerSentinel + "=7|" + plainPayload + "\n(1 row)\n",
			wantCount: 1, wantSchemaErr: 1,
		},
		{
			name:      "corrupt payload is counted separately",
			out:       markerSentinel + "=1|" + garbage + "\n(1 row)\n",
			wantCount: 1, wantDecodeErr: 1,
		},
		{
			name: "mixed rows and banner",
			out: markerSentinel + "=1|" + markedPayload + "\n" +
				markerSentinel + "=1|" + garbage + "\n" +
				markerSentinel + "=1|" + plainPayload + "\n" +
				"(3 rows)\n",
			wantCount: 3, wantMarked: 1, wantDecodeErr: 1,
		},
		{name: "headers and empty result are ignored", out: " ?column?\n----------\n(0 rows)\n"},
		{
			name:        "malformed sentinel row fails loudly",
			out:         markerSentinel + "=1-no-separator\n",
			wantScanErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, marked, decodeErrs, schemaErrs, err := countMarkerRows(tc.out)
			if tc.wantScanErr {
				if err == nil {
					t.Fatalf("expected a scan error, got none (count=%d marked=%d)", count, marked)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected scan error: %v", err)
			}
			if count != tc.wantCount || marked != tc.wantMarked ||
				decodeErrs != tc.wantDecodeErr || schemaErrs != tc.wantSchemaErr {
				t.Errorf("countMarkerRows() = (count=%d, marked=%d, decodeErrs=%d, schemaErrs=%d), want (%d, %d, %d, %d)",
					count, marked, decodeErrs, schemaErrs,
					tc.wantCount, tc.wantMarked, tc.wantDecodeErr, tc.wantSchemaErr)
			}
		})
	}
}

func encodeHexForTest(t *testing.T, text string) string {
	t.Helper()
	raw, err := ask.EncodeAgenticMessage(ask.NewAgenticUserText(text))
	if err != nil {
		t.Fatalf("encode AgenticMessage fixture: %v", err)
	}
	return hex.EncodeToString(raw)
}

// TestEditMentionAcceptance pins that a mention introduced by an edit is
// answered: post a message WITHOUT a mention, then edit it to add the
// @diderot mention. Slack delivers the edit as a message_changed
// subevent, and maybeRouteAsk routes a fresh edit that introduces the
// mention for the first time.
func TestEditMentionAcceptance(t *testing.T) {
	if os.Getenv("DIDEROT_E2E_SLACK") != "1" {
		t.Skip("DIDEROT_E2E_SLACK not set to 1 — skipping real-Slack e2e test")
	}
	xoxpToken := os.Getenv("DIDEROT_SLACK_XOXP_TOKEN")
	if xoxpToken == "" {
		t.Skip("DIDEROT_SLACK_XOXP_TOKEN not set — skipping real-Slack e2e test")
	}
	requireSlackTarget(t)
	requireDeploymentSQL(t)

	ctx := context.Background()
	client := slackgo.New(xoxpToken)

	// Phase 1: post WITHOUT a mention.
	t.Log("phase 1: posting mention-less message to the test channel")
	postCtx, postCancel := context.WithTimeout(ctx, 30*time.Second)
	defer postCancel()
	messageTS, err := postPlainMessage(postCtx, client,
		"the deploy job looks stuck, one sec")
	if err != nil {
		t.Fatalf("failed to post plain message: %v", err)
	}
	permalink := slackPermalink(testChannelID, messageTS)
	t.Logf("posted ts=%s permalink=%s", messageTS, permalink)

	// Give the original (mention-less) message a moment to be ingested
	// via the live path before editing, so the edit is a genuine
	// message_changed on an already-seen message.
	time.Sleep(5 * time.Second)

	// Phase 2: edit the message to introduce the mention.
	t.Log("phase 2: editing the message to add the @diderot mention")
	if err := updateMessage(postCtx, client, messageTS,
		fmt.Sprintf("<@%s> the deploy job says \"waiting for an available runner\", what does that mean?", botUserID)); err != nil {
		t.Fatalf("failed to edit message: %v", err)
	}
	t.Log("edit delivered; polling for bot reply")

	// Phase 3: poll for a bot reply. The edit's introduced mention
	// routes an ask turn and the bot answers.
	pollCtx, pollCancel := context.WithTimeout(ctx, pollTimeout+5*time.Second)
	defer pollCancel()
	replyText, pollErr := pollForBotReply(pollCtx, client, messageTS)
	if pollErr != nil {
		// Corroborate via SQL that this is the routing gap (no claim
		// row), not some other failure, then fail with the full story.
		out, sqlErr := sqlDeployment(context.Background(), fmt.Sprintf(
			"SELECT 'CLAIMED=' || count(*)::text FROM ask_turn_claims "+
				"WHERE channel_id = '%s' AND message_ts = '%s';",
			testChannelID, messageTS))
		claimed := "unknown (sql error)"
		if sqlErr == nil && strings.Contains(out, "CLAIMED=") {
			if strings.Contains(out, "CLAIMED=0") {
				claimed = "no ask_turn_claims row — the edit was never routed"
			} else {
				claimed = "a claim EXISTS — turn was routed but reply didn't arrive; different failure"
			}
		}
		t.Fatalf("bot ignored the mention introduced by edit.\n"+
			"  poll: %v\n  claim state: %s\n  thread: %s",
			pollErr, claimed, permalink)
	}

	t.Logf("bot replied to the edited-in mention: %q", truncate(replyText, 200))
	t.Logf("edit-introduced mention was routed. thread: %s", permalink)
}

// logBarrierSignal is a soft diagnostic: it names the log line and metric
// that show the barrier waited. It never fails the test.
func logBarrierSignal(t *testing.T, messageTS string) {
	t.Helper()
	t.Log("soft signal: caption barrier log lines to check (diagnostic only)")

	// How logs are queried differs per deployment, so name what to look
	// for and let the operator check.
	t.Logf("to verify barrier waited: search the deployment's logs from the last 5 minutes for 'caption'")
	t.Logf("  look for: 'ask: appended mention-image captions' and 'diderot_ask_caption_barrier_total{outcome=\"settled\"}'")
	t.Logf("  message_ts=%s channel=%s", messageTS, testChannelID)
}

// truncate shortens s to n runes for display.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
