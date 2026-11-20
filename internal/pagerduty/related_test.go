package pagerduty

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestRelatedWorkURLsReadOnlyTheHeadedSection(t *testing.T) {
	for _, test := range []struct {
		name   string
		report string
		want   []string
	}{
		{
			name: "markdown heading stops at next heading",
			report: "Cause: [earlier session](https://d.example/sessions/old)\n\n## Possibly related agent work\n" +
				"- Alice restarted it ([session](https://d.example/sessions/a?start=0&end=40))\n## Next checks\n- [x](https://d.example/sessions/b)",
			want: []string{"https://d.example/sessions/a?start=0&end=40"},
		},
		{
			name: "list with continuations stops at bold sub-heading",
			report: "**Possibly related agent work:**\n\n1. Alice\n   [s](https://d.example/sessions/a)\n* [s](https://d.example/sessions/b)\n\n" +
				"**Ruled out**\n- [s](https://d.example/sessions/c)",
			want: []string{"https://d.example/sessions/a", "https://d.example/sessions/b"},
		},
		{
			name:   "paragraph ends the list",
			report: "### Possibly related agent work\n- [s](https://d.example/sessions/a)\nBob's [session](https://d.example/sessions/b) is unrelated.",
			want:   []string{"https://d.example/sessions/a"},
		},
		{
			name:   "bold prose containing the phrase is not a heading",
			report: "**Why Bob is not Possibly related agent work:**\n- [s](https://d.example/sessions/b)",
		},
		{
			name:   "plain heading with colon",
			report: "possibly related agent work:\n- [s](https://d.example/sessions/a)",
			want:   []string{"https://d.example/sessions/a"},
		},
		// Regression: isRelatedWorkHeading trimmed emphasis markup and the
		// trailing colon in two separate strings.Trim passes. When the colon
		// comes right after the closing emphasis -- the form a model actually
		// writes, e.g. "**Possibly related agent work**:" -- strings.Trim(text,
		// "*_") could not reach past the colon to remove the closing "**", so
		// the heading went unrecognized and relatedWorkURLs silently returned
		// no links, skipping the trusted owner ping for an otherwise-valid
		// report. The doc comment on isRelatedWorkHeading already promises
		// these forms ("optionally wrapped in ATX or bold/italic markup and
		// followed by a colon").
		{
			name:   "triple-asterisk heading with colon outside the emphasis",
			report: "***Possibly related agent work***:\n- [s](https://d.example/sessions/a)",
			want:   []string{"https://d.example/sessions/a"},
		},
		{
			name:   "single-asterisk heading with colon outside the emphasis",
			report: "*Possibly related agent work*:\n- [s](https://d.example/sessions/a)",
			want:   []string{"https://d.example/sessions/a"},
		},
		{
			name:   "bold heading with colon outside the emphasis",
			report: "**Possibly related agent work**:\n- [s](https://d.example/sessions/a)",
			want:   []string{"https://d.example/sessions/a"},
		},
		{
			name:   "double-underscore heading with colon outside the emphasis",
			report: "__Possibly related agent work__:\n- [s](https://d.example/sessions/a)",
			want:   []string{"https://d.example/sessions/a"},
		},
		{
			name:   "single-underscore heading with colon outside the emphasis",
			report: "_Possibly related agent work_:\n- [s](https://d.example/sessions/a)",
			want:   []string{"https://d.example/sessions/a"},
		},
		{
			name:   "ATX heading with bold text and colon outside the emphasis",
			report: "## **Possibly related agent work**:\n- [s](https://d.example/sessions/a)",
			want:   []string{"https://d.example/sessions/a"},
		},
		{
			name:   "bold heading with a space before the trailing colon",
			report: "**Possibly related agent work** :\n- [s](https://d.example/sessions/a)",
			want:   []string{"https://d.example/sessions/a"},
		},
		{
			name:   "prose mention is not a heading",
			report: "Nothing belongs under Possibly related agent work. [s](https://d.example/sessions/a)",
		},
		{name: "no section", report: "[s](https://d.example/sessions/a)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, relatedWorkURLs(test.report))
		})
	}
}

func TestRelatedWorkMessageEscapesSessionTitles(t *testing.T) {
	message := relatedWorkMessage([]relatedOwner{
		{SlackUserID: "UALICE", Sessions: []relatedSession{
			{URL: "https://d.example/sessions/a", Title: "<!here> & <@UEVIL>"},
			{URL: "https://d.example/sessions/b"},
		}},
		{SlackUserID: "UBOB", Sessions: []relatedSession{{URL: "https://d.example/sessions/c", Title: "Drain dc2"}}},
	})
	require.Equal(t, "<@UALICE> is this related to the work you're doing? "+
		"<https://d.example/sessions/a|&lt;!here&gt; &amp; &lt;@UEVIL&gt;>, <https://d.example/sessions/b|agent session>\n"+
		"<@UBOB> is this related to the work you're doing? <https://d.example/sessions/c|Drain dc2>", message)
}

type relatedFixtureSession struct {
	id, owner, scope, title string
	started, updated        time.Time
}

func seedRelatedWork(t *testing.T, pool *pgxpool.Pool, sessions []relatedFixtureSession, slackUsers map[string]string) {
	t.Helper()
	cleanup := func() {
		_, err := pool.Exec(context.Background(), `DELETE FROM events WHERE source='agent_session' AND external_id LIKE 'pd-related:%'`)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), `DELETE FROM slack_users WHERE user_id LIKE 'UPD%'`)
		require.NoError(t, err)
	}
	cleanup()
	t.Cleanup(cleanup)
	for _, session := range sessions {
		_, err := pool.Exec(t.Context(), `INSERT INTO events
			(source, external_id, occurred_at, actor, actor_canonical, title, body, url, session_id, scope, raw)
			VALUES ('agent_session', $1, $2, $3, $3, $4, 'summary', $5, $6, $7, jsonb_build_object('source_started_at', $8::text))`,
			"pd-related:"+session.id, session.updated, session.owner, session.title,
			"https://d.example/sessions/"+session.id, session.id, session.scope, session.started.Format(time.RFC3339Nano))
		require.NoError(t, err)
	}
	for userID, email := range slackUsers {
		restricted := email == "grace@example.com"
		_, err := pool.Exec(t.Context(), `INSERT INTO slack_users (user_id, email, is_restricted) VALUES ($1, $2, $3)`,
			userID, email, restricted)
		require.NoError(t, err)
	}
}

func TestRelatedWorkOwnersBindsLinksToVisibleInProgressSessions(t *testing.T) {
	store, pool := testStore(t)
	alertAt := testEvent().OccurredAt
	before, after := alertAt.Add(-40*time.Minute), alertAt.Add(-5*time.Minute)
	seedRelatedWork(t, pool, []relatedFixtureSession{
		{id: "s-alice", owner: "Alice@Example.com", scope: "internal", title: "Restart api hosts", started: before, updated: after},
		{id: "s-alice-2", owner: "alice@example.com", scope: "internal", title: "Bump api config", started: before, updated: alertAt.Add(time.Hour)},
		{id: "s-responder", owner: "bob@example.com", scope: "internal", started: alertAt.Add(time.Minute), updated: alertAt.Add(time.Hour)},
		{id: "s-stale", owner: "carol@example.com", scope: "internal", started: alertAt.Add(-72 * time.Hour), updated: alertAt.Add(-48 * time.Hour)},
		{id: "s-private", owner: "dave@example.com", scope: "owner:dave@example.com", started: before, updated: after},
		{id: "s-no-slack", owner: "erin@example.com", scope: "internal", started: before, updated: after},
		{id: "s-two-slack", owner: "frank@example.com", scope: "internal", started: before, updated: after},
		{id: "s-guest", owner: "grace@example.com", scope: "internal", started: before, updated: after},
		{id: "s-earlier", owner: "heidi@example.com", scope: "internal", started: before, updated: after},
	}, map[string]string{
		"UPDALICE": "alice@example.com", "UPDBOB": "bob@example.com", "UPDCAROL": "carol@example.com",
		"UPDDAVE": "dave@example.com", "UPDFRANK1": "frank@example.com", "UPDFRANK2": "Frank@example.com",
		"UPDGRACE": "grace@example.com", "UPDHEIDI": "heidi@example.com",
	})
	link := func(id string) string { return "[s](https://d.example/sessions/" + id + ")" }
	report := "Cause: " + link("s-earlier") + "\n\n## Possibly related agent work\n" +
		"- " + "[s](https://d.example/sessions/s-alice?start=0&end=40)\n" +
		"- " + link("s-alice-2") + "\n- " + link("s-responder") + "\n- " + link("s-stale") + "\n- " + link("s-private") +
		"\n- " + link("s-no-slack") + "\n- " + link("s-two-slack") + "\n- " + link("s-guest") + "\n- " + link("s-unknown") +
		"\n- [transcript](https://d.example/admin/ask/sessions/9b3f)\n"

	owners, err := store.relatedWorkOwners(t.Context(), report, alertAt, []string{"public", "internal"})
	require.NoError(t, err)
	require.Equal(t, []relatedOwner{{SlackUserID: "UPDALICE", Sessions: []relatedSession{
		{URL: "https://d.example/sessions/s-alice", Title: "Restart api hosts"},
		{URL: "https://d.example/sessions/s-alice-2", Title: "Bump api config"},
	}}}, owners)

	owners, err = store.relatedWorkOwners(t.Context(), report, alertAt, []string{"public"})
	require.NoError(t, err)
	require.Empty(t, owners)
}

func TestSlackDeliveryAsksRelatedSessionOwnersOnce(t *testing.T) {
	store, pool := testStore(t)
	alertAt := testEvent().OccurredAt
	seedRelatedWork(t, pool, []relatedFixtureSession{{id: "s-alice", owner: "alice@example.com", scope: "internal",
		title: "Restart api hosts", started: alertAt.Add(-time.Hour), updated: alertAt.Add(-time.Minute)}},
		map[string]string{"UPDALICE": "alice@example.com"})
	job := newSlackJob(t, store)
	// The trusted reply's first POST fails after Slack recorded it, so the
	// retry must reconcile it rather than ask Alice twice.
	fixture := &slackFixture{rootVisible: true, failAtPost: 2}
	delivery := newSlackFixture(t, store, fixture)
	delivery.Scopes = []string{"public", "internal"}
	report := "API hosts restarted at 19:29.\n\n## Possibly related agent work\n" +
		"- alice@example.com restarted api hosts ([session](https://d.example/sessions/s-alice)) <@UPDALICE>"

	_, err := delivery.Deliver(t.Context(), job, report)
	require.ErrorContains(t, err, "post related agent work")
	messageTS, err := delivery.Deliver(t.Context(), job, report)
	require.NoError(t, err)
	require.Equal(t, "1792006300.000001", messageTS)
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	require.Equal(t, 2, fixture.posts)
	require.NotContains(t, fixture.posted, "<@UPDALICE>")
	require.Len(t, fixture.trusted, 1)
	require.Equal(t, "<@UPDALICE> is this related to the work you're doing? <https://d.example/sessions/s-alice|Restart api hosts>"+
		"\n\nRelated-work reference: diderot-pd-"+sha256EventID(job.Event.ID), fixture.trusted[0])
}

func TestSlackDeliverySkipsRelatedWorkWithoutSection(t *testing.T) {
	store, pool := testStore(t)
	alertAt := testEvent().OccurredAt
	seedRelatedWork(t, pool, []relatedFixtureSession{{id: "s-alice", owner: "alice@example.com", scope: "internal",
		started: alertAt.Add(-time.Hour), updated: alertAt}}, map[string]string{"UPDALICE": "alice@example.com"})
	job := newSlackJob(t, store)
	fixture := &slackFixture{rootVisible: true}
	delivery := newSlackFixture(t, store, fixture)
	delivery.Scopes = []string{"public", "internal"}
	_, err := delivery.Deliver(t.Context(), job, "Ruled out [Alice's session](https://d.example/sessions/s-alice).")
	require.NoError(t, err)
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	require.Equal(t, 1, fixture.posts)
	require.Empty(t, fixture.trusted)
}
