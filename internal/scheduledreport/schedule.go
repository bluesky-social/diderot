package scheduledreport

import (
	"fmt"
	"time"
)

// NextOccurrence returns the first scheduled wall-clock occurrence strictly
// after after. Weekday uses ISO numbering (Monday=1 ... Sunday=7).
func NextOccurrence(after time.Time, cadence, timezone string, weekday *int, hour, minute int) (time.Time, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load timezone: %w", err)
	}
	local := after.In(loc)
	date := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	switch cadence {
	case CadenceDaily:
		if !date.After(after) {
			date = localDateAdd(date, 1, hour, minute, loc)
		}
		return date.UTC(), nil
	case CadenceWeekdays:
		if !date.After(after) {
			date = localDateAdd(date, 1, hour, minute, loc)
		}
		for isWeekend(date) {
			date = localDateAdd(date, 1, hour, minute, loc)
		}
		return date.UTC(), nil
	case CadenceWeekly:
		if weekday == nil || *weekday < 1 || *weekday > 7 {
			return time.Time{}, fmt.Errorf("weekly schedule needs weekday 1..7")
		}
		currentISO := int(local.Weekday())
		if currentISO == 0 {
			currentISO = 7
		}
		delta := (*weekday - currentISO + 7) % 7
		date = localDateAdd(date, delta, hour, minute, loc)
		if !date.After(after) {
			date = localDateAdd(date, 7, hour, minute, loc)
		}
		return date.UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported cadence %q", cadence)
	}
}

// WindowFor returns the cadence-specific reporting window ending at end.
// Constructing boundaries from calendar fields preserves local wall time
// across DST transitions.
func WindowFor(r Report, end time.Time) (time.Time, time.Time, error) {
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	localEnd := end.In(loc)
	if r.Cadence == CadenceWeekdays {
		start := time.Date(localEnd.Year(), localEnd.Month(), localEnd.Day(),
			r.ScheduleHour, r.ScheduleMinute, 0, 0, loc)
		// At or before today's standup boundary, the previous boundary is
		// the last weekday. After it, today's boundary is the last standup.
		if !start.Before(localEnd) {
			start = localDateAdd(start, -1, r.ScheduleHour, r.ScheduleMinute, loc)
		}
		for isWeekend(start) {
			start = localDateAdd(start, -1, r.ScheduleHour, r.ScheduleMinute, loc)
		}
		return start.UTC(), end.UTC(), nil
	}
	days := 1
	if r.Cadence == CadenceWeekly {
		days = 7
	}
	start := time.Date(localEnd.Year(), localEnd.Month(), localEnd.Day()-days,
		localEnd.Hour(), localEnd.Minute(), localEnd.Second(), localEnd.Nanosecond(), loc)
	return start.UTC(), end.UTC(), nil
}

func isWeekend(t time.Time) bool {
	return t.Weekday() == time.Saturday || t.Weekday() == time.Sunday
}

func localDateAdd(t time.Time, days, hour, minute int, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day()+days, hour, minute, 0, 0, loc)
}
