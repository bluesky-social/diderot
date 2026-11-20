package scheduledreport

import (
	"strings"
	"testing"
	"time"
)

func TestNextOccurrenceDailyWeekdaysAndWeekly(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 7, 30, 8, 30, 0, 0, la)
	daily, err := NextOccurrence(before, CadenceDaily, la.String(), nil, 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := daily.In(la); got.Day() != 30 || got.Hour() != 9 {
		t.Fatalf("daily = %v, want Jul 30 09:00 local", got)
	}

	after := time.Date(2026, 7, 30, 9, 0, 0, 0, la)
	daily, err = NextOccurrence(after, CadenceDaily, la.String(), nil, 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := daily.In(la); got.Day() != 31 || got.Hour() != 9 {
		t.Fatalf("strict daily = %v, want Jul 31 09:00 local", got)
	}

	weekdays, err := NextOccurrence(before, CadenceWeekdays, la.String(), nil, 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := weekdays.In(la); got.Weekday() != time.Thursday || got.Day() != 30 || got.Hour() != 9 {
		t.Fatalf("same-day weekdays = %v, want Thu Jul 30 09:00 local", got)
	}

	fridayAfter := time.Date(2026, 7, 31, 9, 0, 0, 0, la)
	weekdays, err = NextOccurrence(fridayAfter, CadenceWeekdays, la.String(), nil, 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := weekdays.In(la); got.Weekday() != time.Monday || got.Day() != 3 || got.Hour() != 9 {
		t.Fatalf("weekend skip = %v, want Mon Aug 3 09:00 local", got)
	}

	monday := 1
	weekly, err := NextOccurrence(before, CadenceWeekly, la.String(), &monday, 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := weekly.In(la)
	if got.Weekday() != time.Monday || got.Day() != 3 || got.Month() != time.August || got.Hour() != 9 {
		t.Fatalf("weekly = %v, want Mon Aug 3 09:00 local", got)
	}
}

func TestWindowForWeekdaysStartsAtLastStandupBoundary(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	report := Report{
		Cadence: CadenceWeekdays, Timezone: la.String(),
		ScheduleHour: 9, ScheduleMinute: 30,
	}

	tests := []struct {
		name      string
		end       time.Time
		wantDay   int
		wantMonth time.Month
	}{
		{
			name:    "monday scheduled run includes weekend",
			end:     time.Date(2026, 8, 3, 9, 30, 0, 0, la),
			wantDay: 31, wantMonth: time.July,
		},
		{
			name:    "tuesday scheduled run starts monday",
			end:     time.Date(2026, 8, 4, 9, 30, 0, 0, la),
			wantDay: 3, wantMonth: time.August,
		},
		{
			name:    "manual run before monday standup starts friday",
			end:     time.Date(2026, 8, 3, 8, 0, 0, 0, la),
			wantDay: 31, wantMonth: time.July,
		},
		{
			name:    "manual run after monday standup starts monday",
			end:     time.Date(2026, 8, 3, 11, 0, 0, 0, la),
			wantDay: 3, wantMonth: time.August,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, gotEnd, err := WindowFor(report, tt.end)
			if err != nil {
				t.Fatal(err)
			}
			got := start.In(la)
			if got.Month() != tt.wantMonth || got.Day() != tt.wantDay || got.Hour() != 9 || got.Minute() != 30 {
				t.Fatalf("start = %v, want %s %d 09:30 local", got, tt.wantMonth, tt.wantDay)
			}
			if !gotEnd.Equal(tt.end) {
				t.Fatalf("end = %v, want %v", gotEnd, tt.end)
			}
		})
	}
}

func TestWindowForUsesCalendarDaysAcrossDST(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	report := Report{Cadence: CadenceDaily, Timezone: la.String()}

	// Noon on the spring-forward day: the preceding local calendar day is
	// only 23 elapsed hours, but both endpoints remain noon wall time.
	end := time.Date(2026, 3, 8, 12, 0, 0, 0, la)
	start, _, err := WindowFor(report, end)
	if err != nil {
		t.Fatal(err)
	}
	if got := end.Sub(start); got != 23*time.Hour {
		t.Fatalf("spring window = %v, want 23h", got)
	}
	if start.In(la).Hour() != 12 {
		t.Fatalf("spring start wall hour = %d, want 12", start.In(la).Hour())
	}

	end = time.Date(2026, 11, 1, 12, 0, 0, 0, la)
	start, _, err = WindowFor(report, end)
	if err != nil {
		t.Fatal(err)
	}
	if got := end.Sub(start); got != 25*time.Hour {
		t.Fatalf("fall window = %v, want 25h", got)
	}
}

func TestCreateParamsValidationAndTemplates(t *testing.T) {
	p := CreateParams{
		Name: " Daily ", Prompt: " summarize ", Cadence: "DAILY",
		ScheduleHour: 9, Timezone: "America/Los_Angeles",
		DeliveryKind: DeliverySlackDM, SlackTarget: "U123",
		OwnerEmail: "PERSON@Example.com", CreatedBy: "ADMIN@Example.com",
	}
	if err := p.normalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	if p.Name != "Daily" || p.OwnerEmail != "person@example.com" || p.CatchupSeconds != 21600 {
		t.Fatalf("normalized params = %+v", p)
	}
	weekdays := p
	weekdays.Cadence = CadenceWeekdays
	if err := weekdays.normalizeAndValidate(); err != nil {
		t.Fatalf("weekdays validation: %v", err)
	}
	for _, name := range []string{"individual-daily", "team-weekly"} {
		body, err := Template(name)
		if err != nil {
			t.Fatalf("Template(%q): %v", name, err)
		}
		if strings.TrimSpace(body) == "" {
			t.Fatalf("Template(%q) is empty", name)
		}
	}
	individual, err := Template("individual-daily")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(individual, "today's intended focus") {
		t.Error("individual daily template asks for today's intended focus")
	}
	if !strings.Contains(individual, "leave out intentions and next steps") {
		t.Error("individual daily template does not exclude intended focus")
	}
	if got := strings.Join(TemplateNames(), ","); got != "individual-daily,team-weekly" {
		t.Fatalf("TemplateNames() = %q", got)
	}
	if _, err := Template("missing"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("missing template error = %v", err)
	}
}

func TestBuildPromptPinsWindow(t *testing.T) {
	r := Report{Name: "Legal weekly", OwnerEmail: "legal@example.com",
		DeliveryKind: DeliverySlackChannel, SlackTarget: "CLEGAL", Timezone: "UTC"}
	run := Run{WindowStart: time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC),
		WindowEnd: time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC), PromptSnapshot: "Leadership format."}
	prompt := buildPrompt(r, run)
	for _, want := range []string{"2026-07-20T09:00:00Z", "2026-07-27T09:00:00Z", "Leadership format.", "It covers the reporting window above and nothing outside it"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
