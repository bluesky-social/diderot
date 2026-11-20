package adminapi

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

// slackPermalinkRE matches the path component of a Slack permalink
// pointing at a thread-rooted message. Capture groups: 1=channel
// id (C... / G... / D...), 2=ts numeric block (the p123... id minus
// the leading "p"). The whole-URL form
// (https://workspace.slack.com/archives/CHANNEL/pTSWITHOUTDOT...)
// includes a query string sometimes (?thread_ts=, etc.) that we
// ignore — only the permalink id matters.
//
// The trailing `(?:[^0-9]|$)` is a lookahead-equivalent that pins
// the digit run at exactly 16. Without it, a malformed permalink
// like `p1700000000351689999` would silently match the first 16
// digits and discard the rest, producing a wrong (but well-formed)
// thread_ts. Better to refuse than to lookup-by-truncated-id.
var slackPermalinkRE = regexp.MustCompile(`/archives/([CDG][A-Z0-9]+)/p([0-9]{16})(?:[^0-9]|$)`)

// ParseSlackPermalink extracts (channel_id, thread_ts) from a Slack
// permalink. The permalink id "p1700000000351689" decodes to
// "1700000000.351689" — Slack drops the dot in URLs but the database
// stores the dotted form (matches what the events API returns).
//
// Accepts:
//   - https://workspace.slack.com/archives/C0EXAMPLE01/p1700000000351689
//   - https://workspace.slack.com/archives/C0EXAMPLE01/p1700000000351689?thread_ts=...
//   - /archives/C0EXAMPLE01/p1700000000351689
//
// Rejects everything else (returns ok=false).
func ParseSlackPermalink(s string) (channelID, threadTS string, ok bool) {
	// Trim a possible URL prefix; the regex matches on the path.
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Path != "" {
		s = u.Path
	} else {
		// Not a parseable URL — treat as a raw path.
		s = path.Clean(s)
	}
	m := slackPermalinkRE.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	channelID = m[1]
	tsRaw := m[2]
	// Slack permalinks stored 16 contiguous digits; the dotted form
	// is the first 10 + "." + last 6.
	if len(tsRaw) != 16 {
		return "", "", false
	}
	threadTS = tsRaw[:10] + "." + tsRaw[10:]
	return channelID, threadTS, true
}
