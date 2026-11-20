package slack

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

type askNotification struct {
	Source        string
	Operation     string
	Asker         string
	AskerEmail    string
	Channel       string
	SessionID     string
	SessionURL    string
	Question      string
	Permalink     string
	BlockedReason string
	Details       string
}

func (c *Consumer) suppressAskNotification(email string) bool {
	_, excluded := c.notifyExcludeEmails[strings.ToLower(strings.TrimSpace(email))]
	return excluded
}

func formatAskNotification(n askNotification) string {
	operation := strings.TrimSpace(n.Operation)
	if operation == "" {
		operation = "ask"
	}
	header := fmt.Sprintf(":mag: *diderot %s*", sanitizeSlackMarkdown(operation))
	if n.BlockedReason != "" {
		header = fmt.Sprintf(":no_entry: *diderot ask blocked* — %s", sanitizeSlackMarkdown(n.BlockedReason))
	}
	// Names, addresses, channel names, and the question all originate
	// outside the application. Sanitize them before composing the message so
	// trusted Slack references in the surrounding template and Details are
	// left intact.
	askerRaw := strings.TrimSpace(n.Asker)
	if askerRaw == "" {
		askerRaw = strings.TrimSpace(n.AskerEmail)
	}
	if askerRaw == "" {
		askerRaw = "(unknown caller)"
	}
	asker := sanitizeSlackMarkdown(askerRaw)
	if n.AskerEmail != "" && !strings.EqualFold(askerRaw, n.AskerEmail) {
		asker += " (" + sanitizeSlackMarkdown(n.AskerEmail) + ")"
	}
	source := sanitizeSlackMarkdown(n.Source)
	if n.Channel != "" {
		source += " in " + sanitizeSlackMarkdown(n.Channel)
	}
	question := sanitizeSlackMarkdown(strings.TrimSpace(n.Question))
	if runes := []rune(question); len(runes) > 400 {
		question = string(runes[:397]) + "..."
	}

	inputLabel := "Question"
	if operation == "list" {
		inputLabel = "Filters"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n*Asked by:* %s\n*Source:* %s\n*%s:*\n> %s", header, asker, source, inputLabel, question)
	if n.SessionURL != "" {
		fmt.Fprintf(&b, "\n*Session:* <%s|View Diderot session>", n.SessionURL)
	} else if n.SessionID != "" {
		fmt.Fprintf(&b, "\n*Session:* `%s`", sanitizeSlackMarkdown(n.SessionID))
	}
	if n.Permalink != "" {
		fmt.Fprintf(&b, "\n*Thread:* %s", n.Permalink)
	}
	if n.Details != "" {
		b.WriteByte('\n')
		b.WriteString(n.Details)
	}
	return b.String()
}

// sendAskNotification is the single formatting and delivery path for ask
// notifications, regardless of whether the invocation arrived via Slack or
// the HTTP API.
func (c *Consumer) sendAskNotification(ctx context.Context, n askNotification, askerSlackUserID string) error {
	if c.api == nil || len(c.notifyEmails) == 0 || c.suppressAskNotification(n.AskerEmail) {
		return nil
	}
	// An MCP retrieve or list is a search; everything else is a question.
	if search := n.Source == "MCP" && n.Operation != "ask"; (search && !c.notifySearches) || (!search && !c.notifyQuestions) {
		return nil
	}
	if n.SessionURL == "" && n.SessionID != "" {
		n.SessionURL = c.askSessionViewerURL(n.SessionID)
	}
	msg := formatAskNotification(n)
	var firstErr error
	for _, uid := range c.operatorIDs(ctx) {
		if uid == askerSlackUserID {
			continue
		}
		if err := c.dmUser(ctx, uid, msg); err != nil {
			c.logger.Warn("ask notification: DM failed", "user_id", uid, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (c *Consumer) askSessionViewerURL(sessionID string) string {
	if c.askSessionViewerBaseURL == "" || strings.TrimSpace(sessionID) == "" {
		return ""
	}
	return c.askSessionViewerBaseURL + "/admin/ask/sessions/" + url.PathEscape(strings.TrimSpace(sessionID))
}

// NotifyAskInvocation implements adminapi.AskInvocationNotifier for HTTP
// asks. Slack asks enter the same formatter/sender through notifyMention.
func (c *Consumer) NotifyAskInvocation(ctx context.Context, ownerEmail, sessionID, question string) error {
	return c.sendAskNotification(ctx, askNotification{
		Source: "HTTP", Operation: "ask", AskerEmail: ownerEmail, SessionID: sessionID, Question: question,
	}, "")
}

// NotifyMCPInvocation reports meaningful corpus tool executions. Transport
// chatter such as ping and tools/list never reaches this method.
func (c *Consumer) NotifyMCPInvocation(ctx context.Context, ownerEmail, toolName, sessionID, input string) error {
	return c.sendAskNotification(ctx, askNotification{
		Source: "MCP", Operation: toolName, AskerEmail: ownerEmail, SessionID: sessionID, Question: input,
	}, "")
}
