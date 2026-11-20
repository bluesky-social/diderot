package slack

import (
	"strings"
	"testing"
)

func TestFormatAskNotificationUsesSharedLayout(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    askNotification
		want []string
	}{
		{
			name: "cli",
			n: askNotification{Source: "HTTP", AskerEmail: "alice@example.com",
				SessionID: "session-1", SessionURL: "https://diderot.example/admin/ask/sessions/session-1", Question: "how do we deploy the billing service?"},
			want: []string{"*diderot ask*", "*Asked by:* alice@example.com",
				"*Source:* HTTP", "> how do we deploy the billing service?", "*Session:* <https://diderot.example/admin/ask/sessions/session-1|View Diderot session>"},
		},
		{
			name: "slack",
			n: askNotification{Source: "Slack", Asker: "Alice", AskerEmail: "alice@example.com",
				Channel: "#eng", SessionID: "session-2", SessionURL: "https://diderot.example/admin/ask/sessions/session-2",
				Question: "is the billing service deployed?", Permalink: "https://slack.example/thread"},
			want: []string{"*diderot ask*", "*Asked by:* Alice (alice@example.com)",
				"*Source:* Slack in #eng", "> is the billing service deployed?", "*Session:* <https://diderot.example/admin/ask/sessions/session-2|View Diderot session>", "*Thread:* https://slack.example/thread"},
		},
		{
			name: "mcp retrieve",
			n: askNotification{Source: "MCP", Operation: "retrieve", AskerEmail: "harper@example.com",
				Question: "what changed?"},
			want: []string{"*diderot retrieve*", "*Asked by:* harper@example.com",
				"*Source:* MCP", "*Question:*", "> what changed?"},
		},
		{
			name: "mcp list",
			n: askNotification{Source: "MCP", Operation: "list", AskerEmail: "harper@example.com",
				Question: `{"sources":["slack"]}`},
			want: []string{"*diderot list*", "*Filters:*", `> {"sources":["slack"]}`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatAskNotification(tc.n)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("formatAskNotification() = %q, missing %q", got, want)
				}
			}
		})
	}
}

func TestAskSessionViewerURL(t *testing.T) {
	c := &Consumer{askSessionViewerBaseURL: "https://diderot.example"}
	if got, want := c.askSessionViewerURL("8ad9c3af-4ead-4a7f-a091-12668af43d56"),
		"https://diderot.example/admin/ask/sessions/8ad9c3af-4ead-4a7f-a091-12668af43d56"; got != want {
		t.Fatalf("askSessionViewerURL() = %q, want %q", got, want)
	}
	if got := (&Consumer{}).askSessionViewerURL("session-1"); got != "" {
		t.Fatalf("unconfigured askSessionViewerURL() = %q, want empty", got)
	}
}

func TestAskNotificationSuppressesExcludedAskers(t *testing.T) {
	c := (&Consumer{}).WithNotifyExcludeEmails([]string{" Watcher@Example.com ", ""})
	for _, email := range []string{
		"watcher@example.com",
		" WATCHER@example.COM ",
	} {
		if !c.suppressAskNotification(email) {
			t.Errorf("suppressAskNotification(%q) = false", email)
		}
	}
	for _, email := range []string{"alice@example.com", ""} {
		if c.suppressAskNotification(email) {
			t.Errorf("suppressAskNotification(%q) = true, want only excluded askers suppressed", email)
		}
	}
	if (&Consumer{}).suppressAskNotification("watcher@example.com") {
		t.Error("an unconfigured consumer should suppress no one")
	}
}

func TestFormatAskNotificationSanitizesUntrustedFields(t *testing.T) {
	got := formatAskNotification(askNotification{
		Source:     "CLI <!here>",
		Asker:      "Alice <@U_BAD>",
		AskerEmail: "alice+<!everyone>@example.com",
		Channel:    "#<!subteam^S_BAD|oncall>",
		Question:   "please ask <@U_BAD|Mallory> and <!channel>",
		// Details may contain references deliberately constructed by the
		// application, so field-level sanitization must leave them intact.
		Details: "*External members:* <@U_TRUSTED>",
	})

	for _, want := range []string{
		"*Asked by:* Alice @U_BAD (alice+@everyone@example.com)",
		"*Source:* CLI @here in #@oncall",
		"> please ask @Mallory and @channel",
		"*External members:* <@U_TRUSTED>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatAskNotification() = %q, missing %q", got, want)
		}
	}
}

func TestFormatAskNotificationSanitizesEmailWithoutDuplicatingIt(t *testing.T) {
	got := formatAskNotification(askNotification{
		Source:     "HTTP",
		AskerEmail: "alice+<!everyone>@example.com",
		Question:   "hello",
	})
	if strings.Count(got, "alice+@everyone@example.com") != 1 {
		t.Errorf("formatAskNotification() duplicated sanitized fallback email: %q", got)
	}
}
