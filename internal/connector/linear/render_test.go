package linear

import (
	"strings"
	"testing"
	"time"
)

func TestRenderTitle(t *testing.T) {
	tests := []struct {
		name  string
		issue Issue
		want  string
	}{
		{
			name:  "with identifier",
			issue: Issue{Identifier: "ENG-655", Title: "Upgrade to latest pulumi"},
			want:  "ENG-655: Upgrade to latest pulumi",
		},
		{
			name:  "without identifier",
			issue: Issue{Title: "Some task"},
			want:  "Some task",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderTitle(&tc.issue)
			if got != tc.want {
				t.Errorf("renderTitle=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderBody_HeaderBlock(t *testing.T) {
	issue := &Issue{
		State:    &IssueState{Name: "In Progress"},
		Assignee: &LinearUser{Name: "Alice"},
		Labels:   &LabelConn{Nodes: []Label{{Name: "bug"}, {Name: "high-impact"}}},
		Project:  &Project{Name: "Platform"},
		Priority: 2,
	}
	body := renderBody(issue)

	assertContains(t, body, "**State:** In Progress")
	assertContains(t, body, "**Assignee:** Alice")
	assertContains(t, body, "**Labels:** bug, high-impact")
	assertContains(t, body, "**Project:** Platform")
	assertContains(t, body, "**Priority:** High")
}

func TestRenderBody_Description(t *testing.T) {
	issue := &Issue{
		Description: "This is a detailed description.\n\nWith multiple paragraphs.",
	}
	body := renderBody(issue)
	assertContains(t, body, "This is a detailed description.")
	assertContains(t, body, "With multiple paragraphs.")
}

func TestRenderBody_Comments(t *testing.T) {
	ts := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	issue := &Issue{
		Comments: &CommentConn{
			Nodes: []Comment{
				{
					User:      &LinearUser{Name: "Bob"},
					CreatedAt: ts,
					Body:      "First comment",
				},
				{
					User:      &LinearUser{Name: "Carol"},
					CreatedAt: ts.Add(time.Hour),
					Body:      "Second comment",
				},
			},
		},
	}
	body := renderBody(issue)
	assertContains(t, body, "**Bob** (2026-07-11):")
	assertContains(t, body, "First comment")
	assertContains(t, body, "**Carol** (2026-07-11):")
	assertContains(t, body, "Second comment")
}

func TestRenderBody_Defaults(t *testing.T) {
	// Issue with no optional fields should render without panicking
	// and include defaults.
	body := renderBody(&Issue{})
	assertContains(t, body, "**State:** Unknown")
	assertContains(t, body, "**Assignee:** Unassigned")
	assertContains(t, body, "**Priority:** No priority")
}

func TestPriorityLabel(t *testing.T) {
	cases := []struct {
		p    int
		want string
	}{
		{0, "No priority"},
		{1, "Urgent"},
		{2, "High"},
		{3, "Medium"},
		{4, "Low"},
		{99, "No priority"},
	}
	for _, c := range cases {
		got := priorityLabel(c.p)
		if got != c.want {
			t.Errorf("priorityLabel(%d)=%q, want %q", c.p, got, c.want)
		}
	}
}

func TestActorCanonical_WithEmail(t *testing.T) {
	issue := &Issue{Creator: &LinearUser{ID: "abc123", Email: "alice@example.com"}}
	got := actorCanonical(issue)
	if got != "alice@example.com" {
		t.Errorf("actorCanonical=%q, want email", got)
	}
}

func TestActorCanonical_NoEmail(t *testing.T) {
	issue := &Issue{Creator: &LinearUser{ID: "abc123"}}
	got := actorCanonical(issue)
	if got != "linear:abc123" {
		t.Errorf("actorCanonical=%q, want linear:abc123", got)
	}
}

func TestActorCanonical_NoCreator(t *testing.T) {
	issue := &Issue{}
	got := actorCanonical(issue)
	if got != "" {
		t.Errorf("actorCanonical=%q, want empty identity", got)
	}
}

func TestTeamID(t *testing.T) {
	issue := &Issue{Team: &Team{ID: "team-uuid"}}
	if got := teamID(issue); got != "team-uuid" {
		t.Errorf("teamID=%q, want team-uuid", got)
	}
}

func TestTeamID_NoTeam(t *testing.T) {
	if got := teamID(&Issue{}); got != "" {
		t.Errorf("teamID=%q, want empty", got)
	}
}

func assertContains(t *testing.T, body, sub string) {
	t.Helper()
	if !strings.Contains(body, sub) {
		t.Errorf("body does not contain %q\nbody:\n%s", sub, body)
	}
}
