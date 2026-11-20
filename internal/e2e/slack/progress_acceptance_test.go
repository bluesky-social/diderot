//go:build e2e

package slack

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
)

const progressAcceptanceDuration = 6*time.Minute + 10*time.Second

// progressAcceptanceBot is the deployment's bot: it posts the test thread,
// sets the thread's agent session, and reads the thread back. The test needs
// no user token.
func progressAcceptanceBot(t *testing.T) *slackgo.Client {
	t.Helper()
	if os.Getenv("DIDEROT_E2E_SLACK_PROGRESS") != "1" {
		t.Skip("DIDEROT_E2E_SLACK_PROGRESS not set to 1")
	}
	botToken := os.Getenv("DIDEROT_E2E_SLACK_BOT_TOKEN")
	if botToken == "" {
		t.Skip("DIDEROT_E2E_SLACK_BOT_TOKEN is required")
	}
	requireSlackTarget(t)
	return slackgo.New(botToken)
}

func progressAcceptanceDurationFromEnv(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv("DIDEROT_SLACK_PROGRESS_DURATION")
	if raw == "" {
		return progressAcceptanceDuration
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		t.Fatalf("invalid DIDEROT_SLACK_PROGRESS_DURATION %q", raw)
	}
	return duration
}

func setProgressAcceptanceStatus(ctx context.Context, bot *slackgo.Client, threadTS, status, title string) error {
	_, err := bot.SetAgentSessionStatusContext(ctx, slackgo.AgentSessionSetStatusParameters{
		ChannelID: testChannelID,
		ThreadTS:  threadTS,
		Status:    status,
		Title:     title,
	})
	return err
}

// TestSlackProgressSurfaceAcceptance exercises the sole progress primitive
// production uses: the thread's agent session, set processing once for a
// turn longer than the old assistant-thread status lasted, then active
// after the answer.
func TestSlackProgressSurfaceAcceptance(t *testing.T) {
	bot := progressAcceptanceBot(t)
	duration := progressAcceptanceDurationFromEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), duration+2*time.Minute)
	defer cancel()

	_, threadTS, err := bot.PostMessageContext(ctx, testChannelID,
		slackgo.MsgOptionText(fmt.Sprintf("diderot Slack status acceptance (%s)", time.Now().Format(time.RFC3339)), false),
	)
	if err != nil {
		t.Fatalf("post test thread: %v", err)
	}
	t.Logf("VISUAL CHECK THREAD: %s", slackPermalink(testChannelID, threadTS))
	t.Log("confirm the thread shows Slack's Working… treatment, titled \"Status acceptance\", and nothing else")
	defer func() {
		clearCtx, clearCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer clearCancel()
		_ = setProgressAcceptanceStatus(clearCtx, bot, threadTS, slackgo.AgentSessionStatusActive, "")
	}()
	if err := setProgressAcceptanceStatus(ctx, bot, threadTS, slackgo.AgentSessionStatusProcessing, "Status acceptance"); err != nil {
		t.Fatalf("set processing: %v", err)
	}

	started := time.Now()
	timer := time.NewTimer(duration)
	select {
	case <-ctx.Done():
		timer.Stop()
		t.Fatal(ctx.Err())
	case <-timer.C:
	}

	answerText := fmt.Sprintf("Slack status acceptance answered after %s", time.Since(started).Round(time.Second))
	_, answerTS, err := bot.PostMessageContext(ctx, testChannelID,
		slackgo.MsgOptionTS(threadTS),
		slackgo.MsgOptionText(answerText, false),
	)
	if err != nil {
		t.Fatalf("post authoritative answer: %v", err)
	}
	if err := setProgressAcceptanceStatus(ctx, bot, threadTS, slackgo.AgentSessionStatusActive, ""); err != nil {
		t.Fatalf("set active: %v", err)
	}

	assertProgressAcceptanceMessageState(t, ctx, bot, threadTS, answerTS, answerText)
	t.Log("VISUAL CHECK: confirm Working… is gone and no progress/task-card message exists")
}

func assertProgressAcceptanceMessageState(t *testing.T, ctx context.Context, bot *slackgo.Client, threadTS, resultTS, resultText string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	lastState := "no thread snapshot"
	for {
		messages, _, _, err := bot.GetConversationRepliesContext(ctx, &slackgo.GetConversationRepliesParameters{
			ChannelID: testChannelID,
			Timestamp: threadTS,
			Inclusive: true,
		})
		lastState = fmt.Sprintf("fetch error: %v", err)
		if err == nil {
			foundResult := false
			foundPlan := false
			for _, message := range messages {
				if message.Timestamp == resultTS && message.Text == resultText {
					foundResult = true
				}
				for _, block := range message.Blocks.BlockSet {
					if _, ok := block.(*slackgo.PlanBlock); ok {
						foundPlan = true
					}
				}
			}
			if foundResult && !foundPlan {
				return
			}
			lastState = fmt.Sprintf("result=%t plan=%t", foundResult, foundPlan)
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread did not converge within 15s: %s", lastState)
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatal(ctx.Err())
		case <-timer.C:
		}
	}
}

func TestProgressAcceptanceDurationFromEnv(t *testing.T) {
	t.Setenv("DIDEROT_SLACK_PROGRESS_DURATION", "250ms")
	if got := progressAcceptanceDurationFromEnv(t); got != 250*time.Millisecond {
		t.Fatalf("duration = %s", got)
	}
}
