package slack

import (
	"context"
	"errors"
	"fmt"
	"github.com/bluesky-social/diderot/internal/askchat"
	"strings"

	"github.com/google/uuid"
	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/ask"
)

// DeliverScheduledReport posts a generated recurring report to a channel or
// user ID. Slack accepts both shapes as chat.postMessage's channel argument;
// the scheduled-report worker has already applied the corresponding audience
// retrieval boundary before generation.
func (c *Consumer) DeliverScheduledReport(ctx context.Context, kind, target string, runID uuid.UUID, body string) (string, error) {
	if c.api == nil {
		return "", errors.New("slack api not configured")
	}
	if kind != "slack_channel" && kind != "slack_dm" {
		return "", fmt.Errorf("unsupported scheduled-report delivery %q", kind)
	}
	if target == "" {
		return "", errors.New("scheduled-report Slack target is empty")
	}
	if body == "" {
		body = "_(empty scheduled report)_"
	}
	// Neutralize untrusted mention/broadcast forms in the generated
	// report before it reaches Slack.
	body = sanitizeSlackMarkdown(body)
	chunks := askchat.Split(body, slackPostChunkBytes)
	var firstTS string
	for i, chunk := range chunks {
		var messageTS string
		err := WithSlackRetry(ctx, c.limiter, methodPostMessage, func() error {
			opts := generatedMarkdownMessageOptions(chunk)
			opts = append(opts,
				slack.MsgOptionMetadata(slack.SlackMetadata{
					EventType: "diderot_scheduled_report",
					EventPayload: map[string]any{
						"run_id":      runID.String(),
						"chunk_index": i,
						"chunk_count": len(chunks),
					},
				}),
			)
			_, ts, apiErr := c.api.PostMessageContext(ctx, target, opts...)
			messageTS = ts
			return apiErr
		})
		if err != nil {
			return firstTS, fmt.Errorf("post scheduled report chunk %d/%d: %w", i+1, len(chunks), err)
		}
		if firstTS == "" {
			firstTS = messageTS
		}
	}
	return firstTS, nil
}

func (c *Consumer) DeliverScheduledArtifacts(ctx context.Context, kind, target, deliveryTS string, sessionID uuid.UUID, assistantTurn int, artifacts []ask.ArtifactDescriptor) error {
	if kind != "slack_channel" && kind != "slack_dm" {
		return fmt.Errorf("unsupported scheduled-report delivery %q", kind)
	}
	if strings.TrimSpace(deliveryTS) == "" {
		return errors.New("scheduled-report artifact delivery requires the root message timestamp")
	}
	return c.postAskArtifacts(ctx, target, deliveryTS, sessionID, assistantTurn, artifacts, "scheduled")
}
