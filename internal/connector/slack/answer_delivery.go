package slack

import (
	"context"
	"errors"
	"strings"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/google/uuid"
	"github.com/slack-go/slack"
)

func (c *Consumer) WithAPI(api *slack.Client) *Consumer {
	c.api = api
	return c
}

func (c *Consumer) DeliverAskAnswer(ctx context.Context, channelID, threadTS string, sessionID uuid.UUID, result ask.TurnResult, marker string) (string, error) {
	messageTS, err := c.postAskAnswerChunks(ctx, channelID, threadTS, sessionID.String(), result.AssistantTurn, result.Reply, result.IncompleteReason, marker)
	if err != nil {
		return messageTS, err
	}
	return messageTS, c.postAskArtifacts(ctx, channelID, threadTS, sessionID, result.AssistantTurn, result.Artifacts, "slack")
}

// DeliverTrustedReply posts application-composed mrkdwn into a thread at most
// once per marker, reconciling an ambiguous earlier post by the marker. The
// text is deliberately not sanitized so its <@U...> mentions notify: callers
// must build it from verified identifiers and escape any untrusted fragment.
func (c *Consumer) DeliverTrustedReply(ctx context.Context, channelID, threadTS, text, marker string) (string, error) {
	if c.api == nil {
		return "", errors.New("slack api not configured")
	}
	if marker == "" {
		return "", errors.New("trusted reply requires a delivery marker")
	}
	delivered, err := c.deliveredAnswerChunks(ctx, channelID, threadTS, marker)
	if err != nil {
		return "", err
	}
	if messageTS := delivered[marker]; messageTS != "" {
		return messageTS, nil
	}
	var messageTS string
	err = WithSlackRetry(ctx, c.limiter, methodPostMessage, func() error {
		var apiErr error
		_, messageTS, apiErr = c.api.PostMessageContext(ctx, channelID,
			slack.MsgOptionText(text+"\n\n"+marker, false), slack.MsgOptionTS(threadTS),
			slack.MsgOptionDisableLinkUnfurl(), slack.MsgOptionDisableMediaUnfurl())
		return apiErr
	})
	if err != nil {
		return "", err
	}
	if messageTS == "" {
		return "", errors.New("slack post response missing message timestamp")
	}
	return messageTS, nil
}

func (c *Consumer) deliveredAnswerChunks(ctx context.Context, channelID, threadTS, marker string) (map[string]string, error) {
	identity, err := c.api.AuthTestContext(ctx)
	if err != nil {
		return nil, err
	}
	if identity.UserID == "" {
		return nil, errors.New("slack identity response missing user ID")
	}
	delivered := map[string]string{}
	parameters := &slack.GetConversationRepliesParameters{ChannelID: channelID, Timestamp: threadTS, Limit: 100}
	for page := 0; page < 100; page++ {
		var messages []slack.Message
		var more bool
		var cursor string
		err := WithSlackRetry(ctx, c.limiter, "conversations.replies", func() error {
			var apiErr error
			messages, more, cursor, apiErr = c.api.GetConversationRepliesContext(ctx, parameters)
			return apiErr
		})
		if err != nil {
			return nil, err
		}
		if messages == nil {
			return nil, errors.New("slack reply response missing messages array")
		}
		if page == 0 {
			rootPresent := false
			for _, message := range messages {
				rootPresent = rootPresent || message.Timestamp == threadTS
			}
			if !rootPresent {
				return nil, errors.New("slack reply response missing alert root")
			}
		}
		for _, message := range messages {
			if message.User != identity.UserID || message.Timestamp == "" {
				continue
			}
			// Both writers (DeliverTrustedReply and Deliver, via
			// deliver.go's `chunk += "\n\n" + chunkMarker`) append the
			// marker as the message's own TRAILING suffix. LastIndex
			// anchors on that trailing occurrence; strings.Cut matched
			// the first occurrence anywhere in the text, which a body
			// that itself echoes marker-shaped text could match before
			// the real trailing marker, hiding the delivered chunk from
			// every later idempotent-retry lookup.
			needle := "\n\n" + marker
			if idx := strings.LastIndex(message.Text, needle); idx >= 0 {
				suffix := message.Text[idx+len(needle):]
				delivered[marker+suffix] = message.Timestamp
			}
		}
		if !more && cursor == "" {
			return delivered, nil
		}
		if cursor == "" || cursor == parameters.Cursor {
			return nil, errors.New("slack reply pagination did not advance")
		}
		parameters.Cursor = cursor
	}
	return nil, errors.New("slack answer reconciliation exceeded 100 reply pages")
}
