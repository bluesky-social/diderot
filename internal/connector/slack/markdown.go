package slack

import "github.com/slack-go/slack"

// generatedMarkdownMessageOptions is the sole rendering path for generated
// Slack prose. Blocks carry the authoritative standard-Markdown body and let
// Slack translate it to rich text. The top-level text is only an accessibility
// and notification fallback; explicitly disabling mrkdwn prevents Slack from
// independently interpreting the same bytes with its legacy dialect.
func generatedMarkdownMessageOptions(body string, trailingBlocks ...slack.Block) []slack.MsgOption {
	blocks := []slack.Block{slack.NewMarkdownBlock("", body)}
	blocks = append(blocks, trailingBlocks...)
	return []slack.MsgOption{
		slack.MsgOptionText(body, false),
		slack.MsgOptionDisableMarkdown(),
		slack.MsgOptionBlocks(blocks...),
	}
}
