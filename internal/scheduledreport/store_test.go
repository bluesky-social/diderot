package scheduledreport

import (
	"context"
	"testing"
	"time"
)

func TestUpdatePreservesIdentityStateAndRunHistory(t *testing.T) {
	store, _ := reportTestStore(t)
	report := createTestReport(t, store, DeliverySlackChannel)
	run, err := store.EnqueueNow(context.Background(), report.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetEnabled(context.Background(), report.ID, false); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }
	weekday := 1
	updated, err := store.Update(context.Background(), report.ID, CreateParams{
		Name: "Legal Monday", Prompt: "New immutable snapshot for later runs.",
		Cadence: CadenceWeekly, Weekday: &weekday, ScheduleHour: 8, ScheduleMinute: 15,
		Timezone: "America/New_York", DeliveryKind: DeliverySlackDM,
		SlackTarget: "ULEGAL", OwnerEmail: "new-owner@example.com", CatchupSeconds: 7200,
		CreatedBy: "attempted-overwrite@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != report.ID || updated.CreatedBy != report.CreatedBy {
		t.Fatalf("identity changed: got id=%v creator=%q", updated.ID, updated.CreatedBy)
	}
	if updated.Enabled {
		t.Fatal("update re-enabled a disabled report")
	}
	if updated.Name != "Legal Monday" || updated.Cadence != CadenceWeekly || updated.Weekday == nil || *updated.Weekday != 1 {
		t.Fatalf("definition not updated: %+v", updated)
	}
	if !updated.NextRunAt.After(store.now()) {
		t.Fatalf("next run %v is not after %v", updated.NextRunAt, store.now())
	}
	runs, err := store.ListRuns(context.Background(), report.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].PromptSnapshot != report.Prompt {
		t.Fatalf("run history changed: %+v", runs)
	}
}

func TestLatestRunsReturnsEachReportsNewestRun(t *testing.T) {
	store, _ := reportTestStore(t)
	ctx := context.Background()
	ran := createTestReport(t, store, DeliverySlackChannel)
	never, err := store.Create(ctx, CreateParams{
		Name: "Never run", Prompt: "Make the update.", Cadence: CadenceDaily, Timezone: "UTC",
		DeliveryKind: DeliverySlackChannel, SlackTarget: "CLEGAL",
		OwnerEmail: "legal@example.com", CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueNow(ctx, ran.ID); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Date(2026, 7, 31, 18, 0, 0, 0, time.UTC) }
	newest, err := store.EnqueueNow(ctx, ran.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 1 || latest[ran.ID].ID != newest.ID {
		t.Fatalf("latest runs = %+v, want only %v for %v", latest, newest.ID, ran.ID)
	}
	if _, ok := latest[never.ID]; ok {
		t.Fatal("a report that never ran has a latest run")
	}
}
