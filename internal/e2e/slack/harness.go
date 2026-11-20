//go:build e2e

// Package slack contains an on-demand end-to-end test harness that drives the
// real diderot Slack bot over socket mode — posting as an operator's xoxp user
// token and asserting the deployment's database state after the bot replies.
//
// Gate: set DIDEROT_E2E_SLACK=1 AND DIDEROT_SLACK_XOXP_TOKEN=<token>.
// Target: DIDEROT_E2E_SLACK_CHANNEL_ID, DIDEROT_E2E_SLACK_BOT_USER_ID and
// DIDEROT_E2E_SLACK_WORKSPACE_HOST (see requireSlackTarget), plus
// DIDEROT_E2E_SQL_COMMAND for the database assertions (see sqlCommand).
// Build: go build -tags e2e ./internal/e2e/...
// Run:   just test-slack-e2e
package slack

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
)

// The Slack target comes from the environment so the harness carries no
// workspace-specific IDs.
var (
	// testChannelID is a private channel the bot is a member of, with
	// ingest_enabled=true.
	testChannelID = os.Getenv("DIDEROT_E2E_SLACK_CHANNEL_ID")

	// botUserID is the diderot bot's Slack user id.
	botUserID = os.Getenv("DIDEROT_E2E_SLACK_BOT_USER_ID")

	// workspaceHost is the Slack hostname used in logged permalinks
	// (e.g. example.slack.com).
	workspaceHost = os.Getenv("DIDEROT_E2E_SLACK_WORKSPACE_HOST")

	// sqlCommand is a shell command that runs the SQL on its stdin against
	// the database of the deployment under test and prints psql's default
	// output, for example `psql "$DB_URL" -v ON_ERROR_STOP=1 -P pager=off -f -`.
	sqlCommand = os.Getenv("DIDEROT_E2E_SQL_COMMAND")
)

// requireSlackTarget skips t unless the test channel, bot user, and
// workspace host are all configured.
func requireSlackTarget(t *testing.T) {
	t.Helper()
	if testChannelID == "" || botUserID == "" || workspaceHost == "" {
		t.Skip("DIDEROT_E2E_SLACK_CHANNEL_ID, DIDEROT_E2E_SLACK_BOT_USER_ID and DIDEROT_E2E_SLACK_WORKSPACE_HOST are required")
	}
}

// requireDeploymentSQL skips t unless it can query the deployment's database.
func requireDeploymentSQL(t *testing.T) {
	t.Helper()
	if sqlCommand == "" {
		t.Skip("DIDEROT_E2E_SQL_COMMAND is required to assert the deployment's database state")
	}
}

const (
	// Barrier notices are bot-authored thread messages that are NOT the
	// answer; the poller must skip them or it declares victory on a
	// notice and runs assertions mid-turn (found the hard way:
	// the GREEN run matched the wait notice, asserted before
	// ask_messages existed, and reported the bug's signature against
	// fixed code).
	captionWaitNoticeText    = "hang on — waiting for an attached image to finish processing before I answer."
	captionTimeoutNoticeText = "the image didn't finish processing in time; answering without it."

	// pollInterval is how often we poll conversations.replies.
	pollInterval = 3 * time.Second

	// pollTimeout is the maximum time to wait for the bot reply.
	// 20s barrier + typical model turn + margin.
	pollTimeout = 120 * time.Second

	// sqlTimeout is the maximum time for one DIDEROT_E2E_SQL_COMMAND run.
	sqlTimeout = 30 * time.Second
)

// fixtureImagePath is the canned test PNG relative to the repo root.
var fixtureImagePath = "internal/e2e/slack/testdata/sso_error.png"

// knownImageKeyword is a case-insensitive substring we expect the bot's reply
// to reference when it has received the image caption.  The fixture image was
// designed to look like an AWS SSO error dialog.
const knownImageKeyword = "SSO"

// slackClient wraps the slack-go calls we need.  Defined as an interface so
// tests could substitute a fake if needed, but the e2e test uses the real
// *slackgo.Client directly (which satisfies this interface).
type slackClient interface {
	UploadFileContext(ctx context.Context, params slackgo.UploadFileParameters) (*slackgo.FileSummary, error)
	GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slackgo.File, []slackgo.Comment, *slackgo.Paging, error)
	GetConversationRepliesContext(ctx context.Context, params *slackgo.GetConversationRepliesParameters) ([]slackgo.Message, bool, string, error)
	PostMessageContext(ctx context.Context, channelID string, options ...slackgo.MsgOption) (string, string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slackgo.MsgOption) (string, string, string, error)
}

// postPlainMessage posts a text-only message to the test channel and
// returns its ts. Used by the edit-race scenario, which needs a message
// without a mention first.
func postPlainMessage(ctx context.Context, client slackClient, text string) (string, error) {
	_, ts, err := client.PostMessageContext(ctx, testChannelID,
		slackgo.MsgOptionText(text, false),
	)
	if err != nil {
		return "", fmt.Errorf("chat.postMessage: %w", err)
	}
	return ts, nil
}

// updateMessage edits an existing message (chat.update), producing a
// message_changed event on the bot's socket. This is how the edit-race
// scenario introduces an @-mention after the fact.
func updateMessage(ctx context.Context, client slackClient, ts, newText string) error {
	_, _, _, err := client.UpdateMessageContext(ctx, testChannelID, ts,
		slackgo.MsgOptionText(newText, false),
	)
	if err != nil {
		return fmt.Errorf("chat.update: %w", err)
	}
	return nil
}

// postTestMessage uploads the fixture image as a file-share message in the
// test channel with an @-mention of the diderot bot.  Returns the posted
// message ts (which is also the thread ts for the new thread).
func postTestMessage(ctx context.Context, client slackClient) (messageTS string, err error) {
	// Resolve fixture path: first try relative to cwd (repo root when called
	// via "just test-slack-e2e"), then absolute fallback.
	imgPath := fixtureImagePath
	if _, statErr := os.Stat(imgPath); statErr != nil {
		imgPath = fmt.Sprintf("%s/src/diderot/%s", os.Getenv("HOME"), fixtureImagePath)
	}

	f, err := os.Open(imgPath)
	if err != nil {
		return "", fmt.Errorf("open fixture image %q: %w", imgPath, err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat fixture image: %w", err)
	}

	// UploadFile uses Slack's modern external-upload flow. InitialComment is the
	// text of the accompanying message so the bot receives a message_event
	// carrying files[], which is what parse.go → ReconcileImages turns into
	// an event_images row.
	summary, err := client.UploadFileContext(ctx, slackgo.UploadFileParameters{
		Reader:         f,
		Filename:       "sso_error.png",
		FileSize:       int(fi.Size()),
		Title:          "AWS SSO error screenshot",
		Channel:        testChannelID,
		InitialComment: fmt.Sprintf("<@%s> what is this error?", botUserID),
	})
	if err != nil {
		return "", fmt.Errorf("UploadFile: %w", err)
	}
	if summary == nil {
		return "", fmt.Errorf("UploadFile returned nil FileSummary")
	}

	// FileSummary only carries ID and Title.  Call GetFileInfo to get the
	// full File struct including Shares, which has the channel share ts.
	// Slack's external-upload share metadata is eventually consistent —
	// the Shares map may be empty immediately after completeUploadExternal
	// returns.  Retry until populated or the context deadline fires.
	for {
		file, _, _, err := client.GetFileInfoContext(ctx, summary.ID, 0, 0)
		if err != nil && strings.Contains(err.Error(), "missing_scope") {
			// Scope problems don't heal with retries — fail loud so the
			// operator sees the real cause instead of a timeout. files.info
			// needs the files:read user scope.
			return "", fmt.Errorf("files.info missing_scope (token needs files:read): %w", err)
		}
		if err == nil {
			if ts, tsErr := extractShareTS(file); tsErr == nil {
				return ts, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for share ts for file %s: %w", summary.ID, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// extractShareTS extracts the message ts from the file's channel share info.
// Checks both Private and Public share maps for testChannelID.
func extractShareTS(file *slackgo.File) (string, error) {
	if file == nil {
		return "", fmt.Errorf("file is nil")
	}
	if file.Shares.Private != nil {
		if shares, ok := file.Shares.Private[testChannelID]; ok && len(shares) > 0 {
			return shares[0].Ts, nil
		}
	}
	if file.Shares.Public != nil {
		if shares, ok := file.Shares.Public[testChannelID]; ok && len(shares) > 0 {
			return shares[0].Ts, nil
		}
	}
	return "", fmt.Errorf("no share ts found in file %s for channel %s", file.ID, testChannelID)
}

// pollForBotReply polls conversations.replies until the bot posts an answer
// message, or the timeout fires. A missing_scope error
// fails immediately (conversations.replies needs groups:history for
// private channels) rather than polling a hard failure into a timeout.
func pollForBotReply(ctx context.Context, client slackClient, threadTS string) (replyText string, err error) {
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		msgs, _, _, err := client.GetConversationRepliesContext(ctx, &slackgo.GetConversationRepliesParameters{
			ChannelID: testChannelID,
			Timestamp: threadTS,
			Inclusive: true,
		})
		if err != nil && strings.Contains(err.Error(), "missing_scope") {
			return "", fmt.Errorf("conversations.replies missing_scope (token needs channels:history + groups:history): %w", err)
		}
		if err == nil {
			for _, m := range msgs {
				if m.User != botUserID {
					continue
				}
				if hasAgentProgressBlocks(m.Blocks) {
					continue
				}
				text := m.Text
				if text == "" {
					// Blocks-only message: fall back to attachments text.
					for _, a := range m.Attachments {
						if a.Text != "" {
							text = a.Text
							break
						}
					}
				}
				// Skip bot-authored progress blocks and barrier notices. The
				// answer is always a separate ordinary message.
				if text == "" ||
					strings.Contains(text, captionWaitNoticeText) ||
					strings.Contains(text, captionTimeoutNoticeText) {
					continue
				}
				return text, nil
			}
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", fmt.Errorf("bot did not reply within %s", pollTimeout)
}

func hasAgentProgressBlocks(blocks slackgo.Blocks) bool {
	for _, block := range blocks.BlockSet {
		if block.BlockType() == slackgo.MBTTaskCard || block.BlockType() == slackgo.MBTPlan {
			return true
		}
	}
	return false
}

// sqlDeployment runs query through DIDEROT_E2E_SQL_COMMAND from the repo root and
// returns stdout.
func sqlDeployment(ctx context.Context, query string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sqlTimeout)
	defer cancel()

	repoRoot, err := repoRootDir()
	if err != nil {
		return "", fmt.Errorf("locating repo root: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", sqlCommand)
	cmd.Dir = repoRoot
	cmd.Stdin = strings.NewReader(query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("DIDEROT_E2E_SQL_COMMAND: %w\nstderr: %s", err, stderr.String())
	}
	return stdout.String(), nil
}

// repoRootDir returns the diderot repo root by scanning candidates for go.mod
// with the expected module path.
func repoRootDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for _, candidate := range []string{
		cwd,
		cwd + "/../..",
		cwd + "/../../..",
		os.Getenv("HOME") + "/src/diderot",
	} {
		data, err := os.ReadFile(candidate + "/go.mod")
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "github.com/bluesky-social/diderot") {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not locate diderot repo root from %s", cwd)
}

// slackPermalink formats a Slack message permalink.
// ts "1234567890.123456" becomes "p1234567890123456".
func slackPermalink(channelID, ts string) string {
	notsep := strings.ReplaceAll(ts, ".", "")
	return fmt.Sprintf("https://%s/archives/%s/p%s", workspaceHost, channelID, notsep)
}

// externalID returns the diderot external_id for a Slack message.
func externalID(channelID, ts string) string {
	return channelID + ":" + ts
}
