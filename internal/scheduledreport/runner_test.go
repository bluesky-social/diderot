package scheduledreport

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/testdb"
)

type fakeAgent struct {
	pool             *pgxpool.Pool
	calls            int
	prompt           string
	scopes           []string
	corpusOnly       bool
	reply            string
	incompleteReason ask.IncompleteReason
	artifacts        []ask.ArtifactDescriptor
}

func (f *fakeAgent) CreateSession(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := f.pool.QueryRow(ctx, `INSERT INTO ask_sessions DEFAULT VALUES RETURNING id`).Scan(&id)
	return id, err
}

func (f *fakeAgent) ExecuteTurn(_ context.Context, req ask.TurnRequest) (ask.TurnResult, error) {
	f.calls++
	f.prompt = req.UserInput
	f.scopes = slices.Clone(req.AllowedScopes)
	f.corpusOnly = req.CorpusOnly
	return ask.TurnResult{Reply: f.reply, IncompleteReason: f.incompleteReason, Artifacts: f.artifacts}, nil
}

type fakeChannelAuthorizer struct {
	calls  int
	access authorization.ChannelAccess
}

func (authorizer *fakeChannelAuthorizer) ResolveChannel(context.Context, string) authorization.ChannelAccess {
	authorizer.calls++
	return authorizer.access
}

type baselineChannelAuthorizer struct{}

func (baselineChannelAuthorizer) ResolveChannel(_ context.Context, channel string) authorization.ChannelAccess {
	return authorization.ChannelAccess{AllowedScopes: []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel(channel)}}
}

func TestScheduledAuthorizationResolvesOnlyForGeneration(t *testing.T) {
	store, pool := reportTestStore(t)
	report := createTestReport(t, store, DeliverySlackChannel)
	if _, err := store.EnqueueNow(context.Background(), report.ID); err != nil {
		t.Fatal(err)
	}
	policy, err := authorization.New(authorization.Config{Grants: []authorization.Grant{{
		Principals: []authorization.Principal{authorization.ChannelPrincipal("CLEGAL")},
		Permissions: []authorization.Permission{
			{Action: authorization.Retrieve, Resource: authorization.ZendeskCorpus},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := policy.Resolve([]authorization.Principal{authorization.ChannelPrincipal("CLEGAL")})
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &fakeChannelAuthorizer{access: authorization.ChannelAccess{AllowedScopes: append([]string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CLEGAL")}, decision.CorpusScopes()...)}}
	agent := &fakeAgent{pool: pool, reply: "report"}
	delivery := &fakeDelivery{fail: errors.New("delivery unavailable")}
	runner := &Runner{Store: store, Agent: agent, Delivery: delivery, ChannelAuthorizer: authorizer}
	if err := runner.Tick(context.Background()); err == nil {
		t.Fatal("expected delivery failure")
	}
	if authorizer.calls != 1 || !slices.Contains(agent.scopes, events.ScopeZendesk) {
		t.Fatalf("authorization not captured: calls=%d scopes=%v", authorizer.calls, agent.scopes)
	}
	authorizer.access = authorization.ChannelAccess{}
	delivery.fail = nil
	if err := runner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if authorizer.calls != 1 || agent.calls != 1 {
		t.Fatalf("delivery retry resolved or regenerated: auth=%d agent=%d", authorizer.calls, agent.calls)
	}
}

func (f *fakeAgent) LoadResultArtifacts(context.Context, uuid.UUID, int) ([]ask.ArtifactDescriptor, error) {
	return f.artifacts, nil
}

func TestScheduledRestrictedChannelAccess(t *testing.T) {
	for _, granted := range []bool{true, false} {
		t.Run(map[bool]string{true: "granted", false: "denied"}[granted], func(t *testing.T) {
			store, pool := reportTestStore(t)
			report := createTestReport(t, store, DeliverySlackChannel)
			if _, err := store.EnqueueNow(context.Background(), report.ID); err != nil {
				t.Fatal(err)
			}
			access := authorization.ChannelAccess{CorpusOnly: true}
			if granted {
				access.AllowedScopes = []string{events.ScopePublic, events.ScopeZendesk}
			}
			agent := &fakeAgent{pool: pool, reply: "report"}
			delivery := &fakeDelivery{}
			runner := &Runner{Store: store, Agent: agent, Delivery: delivery, ChannelAuthorizer: &fakeChannelAuthorizer{access: access}}
			err := runner.Tick(context.Background())
			if granted {
				if err != nil || agent.calls != 1 || !agent.corpusOnly || !slices.Equal(agent.scopes, []string{events.ScopePublic, events.ScopeZendesk}) {
					t.Fatalf("restricted authorization lost: calls=%d scopes=%v corpusOnly=%v err=%v", agent.calls, agent.scopes, agent.corpusOnly, err)
				}
			} else if err == nil || agent.calls != 0 || delivery.body != "" {
				t.Fatalf("denied audience generated or delivered: calls=%d body=%q err=%v", agent.calls, delivery.body, err)
			}
		})
	}
}

func TestScheduledMissingChannelAuthorizerDenies(t *testing.T) {
	store, pool := reportTestStore(t)
	report := createTestReport(t, store, DeliverySlackChannel)
	if _, err := store.EnqueueNow(context.Background(), report.ID); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{pool: pool, reply: "report"}
	delivery := &fakeDelivery{}
	runner := &Runner{Store: store, Agent: agent, Delivery: delivery}
	if err := runner.Tick(context.Background()); err == nil || agent.calls != 0 || delivery.body != "" {
		t.Fatalf("missing authorizer generated or delivered: calls=%d body=%q err=%v", agent.calls, delivery.body, err)
	}
}

func TestRunnerAddsPreciseIncompleteNotice(t *testing.T) {
	store, pool := reportTestStore(t)
	report := createTestReport(t, store, DeliverySlackChannel)
	if _, err := store.EnqueueNow(context.Background(), report.ID); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{
		pool: pool, reply: "partial report", incompleteReason: ask.IncompleteContextExceeded,
	}
	delivery := &fakeDelivery{}
	runner := &Runner{Store: store, Agent: agent, Delivery: delivery, ChannelAuthorizer: baselineChannelAuthorizer{}}
	if err := runner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "partial report\n_(" + ask.IncompleteNotice(ask.IncompleteContextExceeded) + ")_"
	if delivery.body != want {
		t.Fatalf("delivered body = %q, want %q", delivery.body, want)
	}
}

type fakeDelivery struct {
	calls          int
	fail           error
	body           string
	kind           string
	target         string
	artifactFail   error
	artifactCalls  int
	artifactKind   string
	artifactTarget string
	artifactTS     string
}

func (f *fakeDelivery) DeliverScheduledArtifacts(_ context.Context, kind, target, deliveryTS string, _ uuid.UUID, _ int, _ []ask.ArtifactDescriptor) error {
	f.artifactCalls++
	f.artifactKind, f.artifactTarget, f.artifactTS = kind, target, deliveryTS
	return f.artifactFail
}

func (f *fakeDelivery) DeliverScheduledReport(_ context.Context, kind, target string, _ uuid.UUID, body string) (string, error) {
	f.calls++
	f.body, f.kind, f.target = body, kind, target
	if f.fail != nil {
		return "", f.fail
	}
	return "1700000000.000100", nil
}

type fakeScopes struct {
	calls  int
	value  []string
	userID string
}

func (f *fakeScopes) ScopesForEmail(context.Context, string) []string {
	f.calls++
	return slices.Clone(f.value)
}

func (f *fakeScopes) UserIDForEmail(context.Context, string) (string, bool) {
	return f.userID, f.userID != ""
}

func reportTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "scheduledreport"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE scheduled_reports, ask_sessions RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	store.now = func() time.Time { return time.Date(2026, 7, 30, 18, 0, 0, 0, time.UTC) }
	return store, pool
}

func createTestReport(t *testing.T, store *Store, delivery string) *Report {
	t.Helper()
	target := "CLEGAL"
	if delivery == DeliverySlackDM {
		target = "ULEGAL"
	}
	r, err := store.Create(context.Background(), CreateParams{
		Name: "Legal weekly", Prompt: "Make the leadership update.",
		Cadence: CadenceDaily, ScheduleHour: 9, Timezone: "America/Los_Angeles",
		DeliveryKind: delivery, SlackTarget: target,
		OwnerEmail: "legal@example.com", CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunnerChannelReportUsesAudienceScopeAndSucceeds(t *testing.T) {
	store, pool := reportTestStore(t)
	report := createTestReport(t, store, DeliverySlackChannel)
	if _, err := store.EnqueueNow(context.Background(), report.ID); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{pool: pool, reply: "*Legal update*\n• Shipped a thing."}
	delivery := &fakeDelivery{}
	resolver := &fakeScopes{value: []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CSECRET")}, userID: "ULEGAL"}
	runner := &Runner{Store: store, Agent: agent, Delivery: delivery, ScopeResolver: resolver, ChannelAuthorizer: baselineChannelAuthorizer{}}
	if err := runner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || delivery.calls != 1 {
		t.Fatalf("agent calls=%d delivery calls=%d", agent.calls, delivery.calls)
	}
	if resolver.calls != 0 {
		t.Fatalf("channel delivery consulted owner resolver %d times", resolver.calls)
	}
	wantScopes := []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CLEGAL")}
	if !slices.Equal(agent.scopes, wantScopes) {
		t.Fatalf("scopes=%v want=%v", agent.scopes, wantScopes)
	}
	if delivery.kind != DeliverySlackChannel || delivery.target != "CLEGAL" {
		t.Fatalf("delivery=%s:%s", delivery.kind, delivery.target)
	}
	runs, err := store.ListRuns(context.Background(), report.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != "succeeded" || runs[0].Attempts != 1 || runs[0].DeliveryTS == nil {
		t.Fatalf("runs=%+v", runs)
	}
}

// TestRunnerLeavesDueReportsQueuedUntilReady: while delivery isn't ready,
// a due report is neither generated nor taken, and runs once it is; a tick
// stops taking reports when delivery stops being ready.
func TestRunnerLeavesDueReportsQueuedUntilReady(t *testing.T) {
	store, pool := reportTestStore(t)
	first := createTestReport(t, store, DeliverySlackChannel)
	second, err := store.Create(context.Background(), CreateParams{
		Name: "Legal daily", Prompt: "Make the daily update.",
		Cadence: CadenceDaily, ScheduleHour: 9, Timezone: "America/Los_Angeles",
		DeliveryKind: DeliverySlackChannel, SlackTarget: "CLEGAL",
		OwnerEmail: "legal@example.com", CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range []*Report{first, second} {
		if _, err := store.EnqueueNow(context.Background(), report.ID); err != nil {
			t.Fatal(err)
		}
	}
	agent := &fakeAgent{pool: pool, reply: "report"}
	ready := false
	delivery := &fakeDelivery{}
	runner := &Runner{Store: store, Agent: agent, Delivery: delivery, ChannelAuthorizer: baselineChannelAuthorizer{},
		Ready: func() bool { return ready }}
	statuses := func() []string {
		t.Helper()
		var out []string
		for _, report := range []*Report{first, second} {
			runs, err := store.ListRuns(context.Background(), report.ID, 10)
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs=%+v err=%v", runs, err)
			}
			out = append(out, runs[0].Status)
		}
		return out
	}

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agent.calls != 0 || delivery.calls != 0 || !slices.Equal(statuses(), []string{"pending", "pending"}) {
		t.Fatalf("a report ran while delivery wasn't ready: agent=%d delivery=%d statuses=%v", agent.calls, delivery.calls, statuses())
	}

	ready = true
	runner.Ready = func() bool {
		// Delivery stops being ready once the first report is delivered.
		return ready && delivery.calls == 0
	}
	if err := runner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || delivery.calls != 1 || !slices.Contains(statuses(), "pending") || !slices.Contains(statuses(), "succeeded") {
		t.Fatalf("want one report delivered and one left queued: agent=%d delivery=%d statuses=%v", agent.calls, delivery.calls, statuses())
	}
}

func TestRunnerDeliveryRetryReusesGeneratedOutput(t *testing.T) {
	store, pool := reportTestStore(t)
	report := createTestReport(t, store, DeliverySlackDM)
	if _, err := store.EnqueueNow(context.Background(), report.ID); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{pool: pool, reply: "daily update"}
	delivery := &fakeDelivery{fail: errors.New("Slack unavailable")}
	resolver := &fakeScopes{value: []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CPRIVATE")}, userID: "ULEGAL"}
	runner := &Runner{Store: store, Agent: agent, Delivery: delivery, ScopeResolver: resolver, ChannelAuthorizer: baselineChannelAuthorizer{}}
	if err := runner.Tick(context.Background()); err == nil {
		t.Fatal("first Tick succeeded, want delivery error")
	}
	runs, err := store.ListRuns(context.Background(), report.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Status != "generated" || runs[0].Output == nil {
		t.Fatalf("after delivery failure run=%+v", runs[0])
	}
	if runs[0].DeliveryKind == nil || *runs[0].DeliveryKind != DeliverySlackDM || runs[0].SlackTarget == nil || *runs[0].SlackTarget != "ULEGAL" {
		t.Fatalf("missing generation audience: %+v", runs[0])
	}
	if _, err := pool.Exec(context.Background(), `UPDATE scheduled_reports SET delivery_kind='slack_channel', slack_target='COTHER', owner_email='other@example.com' WHERE id=$1`, report.ID); err != nil {
		t.Fatal(err)
	}
	resolver.userID = ""
	runner.ScopeResolver = nil
	delivery.fail = nil
	if err := runner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 {
		t.Fatalf("agent called %d times; generated output should be reused", agent.calls)
	}
	if delivery.calls != 2 {
		t.Fatalf("delivery calls=%d want=2", delivery.calls)
	}
	if delivery.kind != DeliverySlackDM || delivery.target != "ULEGAL" || resolver.calls != 1 {
		t.Fatalf("retry changed audience: %s:%s resolver calls=%d", delivery.kind, delivery.target, resolver.calls)
	}
	if !slices.Equal(agent.scopes, resolver.value) {
		t.Fatalf("DM scopes=%v want=%v", agent.scopes, resolver.value)
	}
	runs, err = store.ListRuns(context.Background(), report.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Status != "succeeded" || runs[0].Attempts != 2 {
		t.Fatalf("final run=%+v", runs[0])
	}
}

func TestRunnerRejectsLegacyOutputWithoutSavedAudience(t *testing.T) {
	store, pool := reportTestStore(t)
	report := createTestReport(t, store, DeliverySlackChannel)
	run, err := store.EnqueueNow(context.Background(), report.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE scheduled_report_runs SET output='old private output', status='generated' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{pool: pool}
	delivery := &fakeDelivery{}
	runner := &Runner{Store: store, Agent: agent, Delivery: delivery, ChannelAuthorizer: baselineChannelAuthorizer{}}
	if err := runner.Tick(context.Background()); err == nil {
		t.Fatal("legacy output with unknown audience was accepted")
	}
	if agent.calls != 0 || delivery.calls != 0 {
		t.Fatalf("agent=%d delivery=%d", agent.calls, delivery.calls)
	}
}

func TestRunnerArtifactRetryKeepsGenerationAudience(t *testing.T) {
	store, pool := reportTestStore(t)
	report := createTestReport(t, store, DeliverySlackChannel)
	if _, err := store.EnqueueNow(context.Background(), report.ID); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{pool: pool, reply: "report", artifacts: []ask.ArtifactDescriptor{{}}}
	delivery := &fakeDelivery{artifactFail: errors.New("upload unavailable")}
	runner := &Runner{Store: store, Agent: agent, Delivery: delivery, ChannelAuthorizer: baselineChannelAuthorizer{}}
	if err := runner.Tick(context.Background()); err == nil {
		t.Fatal("expected artifact failure")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE scheduled_reports SET delivery_kind='slack_dm', slack_target='UOTHER' WHERE id=$1`, report.ID); err != nil {
		t.Fatal(err)
	}
	delivery.artifactFail = nil
	if err := runner.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || delivery.calls != 1 || delivery.artifactCalls != 2 {
		t.Fatalf("generation=%d delivery=%d uploads=%d", agent.calls, delivery.calls, delivery.artifactCalls)
	}
	if delivery.artifactKind != DeliverySlackChannel || delivery.artifactTarget != "CLEGAL" || delivery.artifactTS != "1700000000.000100" {
		t.Fatalf("artifact redirected: %s:%s ts=%s", delivery.artifactKind, delivery.artifactTarget, delivery.artifactTS)
	}
}

func TestDMScopesRejectMismatchedRecipient(t *testing.T) {
	resolver := &fakeScopes{
		userID: "UOTHER",
		value:  []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CSECRET")},
	}
	runner := &Runner{ScopeResolver: resolver}
	_, err := runner.scopes(context.Background(), Report{
		DeliveryKind: DeliverySlackDM,
		SlackTarget:  "ULEGAL",
		OwnerEmail:   "legal@example.com",
	})
	if err == nil {
		t.Fatal("mismatched DM recipient received owner scopes")
	}
}

func TestMaterializeDueSkipsOutsideCatchupWindow(t *testing.T) {
	store, _ := reportTestStore(t)
	report := createTestReport(t, store, DeliverySlackChannel)
	due := store.now().Add(-7 * time.Hour)
	if _, err := store.pool.Exec(context.Background(), `UPDATE scheduled_reports SET next_run_at=$2 WHERE id=$1`, report.ID, due); err != nil {
		t.Fatal(err)
	}
	if err := store.MaterializeDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListRuns(context.Background(), report.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	// The artificial cursor is 04:00 local. It is skipped, then the real
	// 09:00 occurrence from the same day is within the catch-up window and is
	// queued. Materialization advances through every missed wall-clock slot.
	if len(runs) != 2 || runs[0].Status != "pending" || runs[1].Status != "skipped" {
		t.Fatalf("runs=%+v, want newest pending and older skipped", runs)
	}
	updated, err := store.Get(context.Background(), report.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.NextRunAt.After(store.now()) {
		t.Fatalf("next_run_at=%v is not future", updated.NextRunAt)
	}
}

func TestMaterializeWeekdaysMondayWindowStartsFriday(t *testing.T) {
	store, _ := reportTestStore(t)
	store.now = func() time.Time {
		return time.Date(2026, 8, 3, 17, 0, 0, 0, time.UTC) // Monday 10:00 PDT.
	}
	report, err := store.Create(context.Background(), CreateParams{
		Name: "Platform standup", Prompt: "Summarize since the last standup.",
		Cadence: CadenceWeekdays, ScheduleHour: 9, ScheduleMinute: 30,
		Timezone: "America/Los_Angeles", DeliveryKind: DeliverySlackDM,
		SlackTarget: "ULEGAL", OwnerEmail: "legal@example.com",
		CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	monday := time.Date(2026, 8, 3, 16, 30, 0, 0, time.UTC)
	if _, err := store.pool.Exec(context.Background(),
		`UPDATE scheduled_reports SET next_run_at=$2 WHERE id=$1`, report.ID, monday); err != nil {
		t.Fatal(err)
	}
	if err := store.MaterializeDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListRuns(context.Background(), report.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs=%+v, want one Monday occurrence", runs)
	}
	wantStart := time.Date(2026, 7, 31, 16, 30, 0, 0, time.UTC)
	if !runs[0].WindowStart.Equal(wantStart) || !runs[0].WindowEnd.Equal(monday) {
		t.Fatalf("window=%v..%v, want %v..%v", runs[0].WindowStart, runs[0].WindowEnd, wantStart, monday)
	}
	updated, err := store.Get(context.Background(), report.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantNext := time.Date(2026, 8, 4, 16, 30, 0, 0, time.UTC)
	if !updated.NextRunAt.Equal(wantNext) {
		t.Fatalf("next_run_at=%v, want %v", updated.NextRunAt, wantNext)
	}
}
