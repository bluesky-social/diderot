package pagerduty

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/urlx"
)

// relatedWorkHeading names the report section whose session links Diderot
// turns into Slack mentions. The model chooses the sessions; code binds each
// link to its corpus owner, so alert text cannot steer a ping at an arbitrary
// person.
const relatedWorkHeading = "Possibly related agent work"

const relatedWorkSteering = "Also check whether a teammate's agent session may have caused this. Search " +
	"agent_session with retrieve for the alerting hosts, services, clusters and resources, from 24 hours " +
	"before the alert until now: a session is dated by its last activity, so one that began before the " +
	"alert and is still running is dated after it. Do not page through a list of every session in that " +
	"period. A session qualifies only if its summary shows that, shortly before the alert began, its owner " +
	"changed the alerting resource or something it directly depends on, or changed shared infrastructure " +
	"(a deploy, a configuration change, a restart, a migration, an apply) in a way that could produce " +
	"this failure, or changed the monitoring or alert rules that raised this alert. Someone investigating " +
	"or responding to the problem does not qualify. Most alerts have " +
	"no such session, and leaving the section out is the normal case. When a session qualifies, end the " +
	"report with a final section headed exactly \"" + relatedWorkHeading + "\": one bullet per session " +
	"giving the owner's name, what they changed, why it may relate, and a Markdown link to the session's " +
	"URL, even if the report links it earlier. Never list a session there only to rule it out. Each " +
	"listed owner is asked about it automatically afterwards, so do not address them or put questions to " +
	"them in the report."

const (
	// relatedWorkLookback bounds how stale a session's last activity may be.
	// Sessions that started after the alert are excluded separately: they
	// are more likely responders than causes.
	relatedWorkLookback = 24 * time.Hour
	maxRelatedOwners    = 3
	maxRelatedSessions  = 3
	maxRelatedURLs      = 20
)

var listItem = regexp.MustCompile(`^(?:[-*+•]|\d+[.)])\s`)

type relatedSession struct {
	URL   string
	Title string
}

type relatedOwner struct {
	SlackUserID string
	Sessions    []relatedSession
}

// relatedWorkURLs returns the links in the list directly under the report's
// related-work heading. The first line that is neither blank, a list item,
// nor an indented continuation ends the section, so a later sub-heading or
// paragraph (for example, sessions the model ruled out) is never harvested.
func relatedWorkURLs(report string) []string {
	lines := strings.Split(report, "\n")
	for index, line := range lines {
		if !isRelatedWorkHeading(line) {
			continue
		}
		var items []string
		for _, next := range lines[index+1:] {
			trimmed := strings.TrimSpace(next)
			if trimmed != "" && !listItem.MatchString(trimmed) && next[0] != ' ' && next[0] != '\t' {
				break
			}
			items = append(items, next)
		}
		return urlx.ExtractURLs(strings.Join(items, "\n"))
	}
	return nil
}

// isRelatedWorkHeading accepts the heading phrase alone, optionally wrapped in
// ATX or bold/italic markup and followed by a colon. Prose that merely
// contains the phrase does not qualify.
func isRelatedWorkHeading(line string) bool {
	text := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
	// A single trim across emphasis markers, the trailing colon, and the
	// space between them: trimming colon and emphasis in separate passes
	// (as this used to) stops short whenever the colon sits right outside
	// the closing emphasis, since strings.Trim halts at the first
	// character outside its cutset.
	text = strings.TrimSpace(strings.Trim(text, "*_: \t"))
	return strings.EqualFold(text, relatedWorkHeading)
}

// relatedWorkOwners resolves the report's related-work links to visible
// agent sessions that were in progress around the alert, grouped by owner.
// Owners without exactly one internal Slack profile are skipped; the report
// still names them.
func (store *Store) relatedWorkOwners(ctx context.Context, report string, alertAt time.Time, scopes []string) ([]relatedOwner, error) {
	links := relatedWorkURLs(report)
	if len(links) > maxRelatedURLs {
		links = links[:maxRelatedURLs]
	}
	var sessionIDs, sessionURLs []string
	for _, link := range links {
		base, _, _ := strings.Cut(link, "#")
		base, _, _ = strings.Cut(base, "?")
		parsed, err := url.Parse(base)
		if err != nil {
			continue
		}
		_, sessionID, found := strings.Cut(parsed.Path, "/sessions/")
		if !found || sessionID == "" || strings.Contains(sessionID, "/") {
			continue
		}
		sessionIDs = append(sessionIDs, sessionID)
		sessionURLs = append(sessionURLs, base)
	}
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	rows, err := store.pool.Query(ctx, `SELECT url, lower(actor_canonical), COALESCE(title, ''),
		COALESCE(raw->>'source_started_at', '')
		FROM events
		WHERE source = 'agent_session' AND NOT deleted AND session_id = ANY($1) AND url = ANY($2)
		AND scope = ANY($3) AND occurred_at >= $4 AND actor_canonical LIKE '%_@_%'`,
		sessionIDs, sessionURLs, events.ReadableScopes(scopes), alertAt.Add(-relatedWorkLookback))
	if err != nil {
		return nil, fmt.Errorf("query related agent sessions: %w", err)
	}
	type candidate struct {
		owner   string
		session relatedSession
	}
	byURL := map[string]candidate{}
	for rows.Next() {
		var sessionURL, owner, title, startedRaw string
		if err := rows.Scan(&sessionURL, &owner, &title, &startedRaw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan related agent session: %w", err)
		}
		started, err := time.Parse(time.RFC3339Nano, startedRaw)
		if err != nil || started.After(alertAt) {
			continue
		}
		byURL[sessionURL] = candidate{owner: owner, session: relatedSession{URL: sessionURL, Title: title}}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read related agent sessions: %w", err)
	}
	var owners []string
	sessions := map[string][]relatedSession{}
	for _, sessionURL := range sessionURLs {
		match, ok := byURL[sessionURL]
		if !ok {
			continue
		}
		delete(byURL, sessionURL)
		if _, seen := sessions[match.owner]; !seen {
			owners = append(owners, match.owner)
		}
		if len(sessions[match.owner]) < maxRelatedSessions {
			sessions[match.owner] = append(sessions[match.owner], match.session)
		}
	}
	if len(owners) == 0 {
		return nil, nil
	}
	profiles, err := store.pool.Query(ctx, `SELECT lower(email), user_id FROM slack_users
		WHERE lower(email) = ANY($1) AND NOT is_bot AND NOT is_stranger AND NOT is_restricted
		AND NOT is_ultra_restricted`, owners)
	if err != nil {
		return nil, fmt.Errorf("query related session owners' Slack profiles: %w", err)
	}
	slackUsers := map[string][]string{}
	for profiles.Next() {
		var email, userID string
		if err := profiles.Scan(&email, &userID); err != nil {
			profiles.Close()
			return nil, fmt.Errorf("scan Slack profile: %w", err)
		}
		slackUsers[email] = append(slackUsers[email], userID)
	}
	profiles.Close()
	if err := profiles.Err(); err != nil {
		return nil, fmt.Errorf("read Slack profiles: %w", err)
	}
	var related []relatedOwner
	for _, owner := range owners {
		if len(slackUsers[owner]) != 1 {
			continue
		}
		related = append(related, relatedOwner{SlackUserID: slackUsers[owner][0], Sessions: sessions[owner]})
		if len(related) == maxRelatedOwners {
			break
		}
	}
	return related, nil
}

// relatedWorkMessage composes the trusted mrkdwn follow-up. Session titles
// are model-written summaries, so they are escaped before reaching Slack.
func relatedWorkMessage(owners []relatedOwner) string {
	escape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	lines := make([]string, 0, len(owners))
	for _, owner := range owners {
		links := make([]string, 0, len(owner.Sessions))
		for _, session := range owner.Sessions {
			title := strings.TrimSpace(session.Title)
			if title == "" {
				title = "agent session"
			}
			links = append(links, "<"+session.URL+"|"+escape.Replace(title)+">")
		}
		lines = append(lines, "<@"+owner.SlackUserID+"> is this related to the work you're doing? "+strings.Join(links, ", "))
	}
	return strings.Join(lines, "\n")
}

// alertTime is the earliest known moment of the page, matching the thread
// lookup's lower bound.
func alertTime(event Event) time.Time {
	if !event.Data.CreatedAt.IsZero() && event.Data.CreatedAt.Before(event.OccurredAt) {
		return event.Data.CreatedAt
	}
	return event.OccurredAt
}
