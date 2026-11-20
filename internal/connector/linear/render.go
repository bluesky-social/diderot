package linear

import (
	"fmt"
	"strings"
	"time"
)

// Issue is the Linear issue shape returned by the GraphQL API and used
// as input to the render pipeline. All fields are optional in the API
// response; we use pointers for non-primitive types and zero values for
// the rest — callers must handle absent fields gracefully.
type Issue struct {
	ID          string    `json:"id"`
	Identifier  string    `json:"identifier"` // e.g. "ENG-655"
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	URL         string    `json:"url"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Priority    int       `json:"priority"` // 0=none, 1=urgent, 2=high, 3=medium, 4=low

	Creator  *LinearUser  `json:"creator,omitempty"`
	Assignee *LinearUser  `json:"assignee,omitempty"`
	State    *IssueState  `json:"state,omitempty"`
	Team     *Team        `json:"team,omitempty"`
	Project  *Project     `json:"project,omitempty"`
	Labels   *LabelConn   `json:"labels,omitempty"`
	Comments *CommentConn `json:"comments,omitempty"`
}

// LinearUser is a Linear workspace user.
type LinearUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
}

// IssueState is the workflow state of an issue.
type IssueState struct {
	Name string `json:"name"`
	Type string `json:"type"` // "triage", "backlog", "unstarted", "started", "completed", "cancelled"
}

// Team is the Linear team an issue belongs to.
type Team struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Key     string `json:"key"`
	Private bool   `json:"private"`
}

// Project is the Linear project an issue belongs to (optional).
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Label is a Linear issue label.
type Label struct {
	Name string `json:"name"`
}

// LabelConn is a GraphQL connection of labels.
type LabelConn struct {
	Nodes []Label `json:"nodes"`
}

// Comment is a single comment on a Linear issue.
type Comment struct {
	ID        string      `json:"id"`
	Body      string      `json:"body"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
	User      *LinearUser `json:"user,omitempty"`
}

// CommentConn is a GraphQL connection of comments.
type CommentConn struct {
	Nodes    []Comment `json:"nodes"`
	PageInfo PageInfo  `json:"pageInfo"`
}

// PageInfo is the pagination cursor info from GraphQL connections.
type PageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// priorityLabel maps Linear's numeric priority to a human-readable label.
func priorityLabel(p int) string {
	switch p {
	case 1:
		return "Urgent"
	case 2:
		return "High"
	case 3:
		return "Medium"
	case 4:
		return "Low"
	default:
		return "No priority"
	}
}

// renderTitle builds the issue title in the form "IDENTIFIER: title"
// (e.g. "ENG-655: Upgrade to latest pulumi"). This is how humans cite
// issues in Slack so the identifier appears in embedded text.
func renderTitle(issue *Issue) string {
	if issue.Identifier == "" {
		return issue.Title
	}
	return issue.Identifier + ": " + issue.Title
}

// renderBody builds the full markdown body for a Linear issue event.
// Structure:
//
//	Header block: state, assignee, labels, project, priority
//	(blank line)
//	Description (if non-empty)
//	(blank line)
//	Comments as "**author** (date):" + body
//
// Comments are sorted in the order returned by Linear (chronological).
// This matches the "body tracks the source's current state" design —
// re-ingesting on any change regenerates the full body; the sink's
// unchanged-content short-circuit makes no-ops free.
func renderBody(issue *Issue) string {
	var b strings.Builder

	// Header block.
	state := "Unknown"
	if issue.State != nil && issue.State.Name != "" {
		state = issue.State.Name
	}
	fmt.Fprintf(&b, "**State:** %s\n", state)

	assignee := "Unassigned"
	if issue.Assignee != nil && issue.Assignee.Name != "" {
		assignee = issue.Assignee.Name
	}
	fmt.Fprintf(&b, "**Assignee:** %s\n", assignee)

	if issue.Labels != nil && len(issue.Labels.Nodes) > 0 {
		names := make([]string, len(issue.Labels.Nodes))
		for i, l := range issue.Labels.Nodes {
			names[i] = l.Name
		}
		fmt.Fprintf(&b, "**Labels:** %s\n", strings.Join(names, ", "))
	}

	if issue.Project != nil && issue.Project.Name != "" {
		fmt.Fprintf(&b, "**Project:** %s\n", issue.Project.Name)
	}

	fmt.Fprintf(&b, "**Priority:** %s\n", priorityLabel(issue.Priority))

	// Description (if present).
	if desc := strings.TrimSpace(issue.Description); desc != "" {
		b.WriteString("\n")
		b.WriteString(desc)
		b.WriteString("\n")
	}

	// Comments.
	if issue.Comments != nil && len(issue.Comments.Nodes) > 0 {
		b.WriteString("\n")
		for _, c := range issue.Comments.Nodes {
			author := "Unknown"
			if c.User != nil && c.User.Name != "" {
				author = c.User.Name
			}
			date := c.CreatedAt.UTC().Format("2006-01-02")
			fmt.Fprintf(&b, "\n**%s** (%s):\n%s\n", author, date, strings.TrimSpace(c.Body))
		}
	}

	return b.String()
}

// actorName returns the display name for the issue creator, falling back
// to ID-based placeholder when the creator is absent.
func actorName(issue *Issue) string {
	if issue.Creator != nil && issue.Creator.Name != "" {
		return issue.Creator.Name
	}
	return "unknown"
}

// actorCanonical returns the cross-source canonical identity key for the
// issue creator. Uses email when present; falls back to "linear:<uuid>".
func actorCanonical(issue *Issue) string {
	if issue.Creator != nil {
		if issue.Creator.Email != "" {
			return issue.Creator.Email
		}
		if issue.Creator.ID != "" {
			return "linear:" + issue.Creator.ID
		}
	}
	return ""
}

// actorID returns the opaque upstream id of the issue creator.
func actorID(issue *Issue) string {
	if issue.Creator != nil {
		return issue.Creator.ID
	}
	return ""
}

// teamID returns the team UUID for use as thread_id.
func teamID(issue *Issue) string {
	if issue.Team != nil {
		return issue.Team.ID
	}
	return ""
}
