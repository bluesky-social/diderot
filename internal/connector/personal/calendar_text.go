package personal

import (
	"errors"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"github.com/bluesky-social/diderot/internal/events"
)

func calendarEvent(comp *ical.Component, obj caldav.CalendarObject, prefix, scope string) (events.Event, error) {
	props := comp.Props
	uid, err := props.Text(ical.PropUID)
	if err != nil || strings.TrimSpace(uid) == "" {
		return events.Event{}, errors.New("CalDAV returned an entry without a valid UID")
	}
	recurrence := ""
	if prop := props.Get(ical.PropRecurrenceID); prop != nil {
		recurrence = prop.Value
	}
	start, _ := props.DateTime(ical.PropDateTimeStart, time.UTC)
	occurred := start
	if occurred.IsZero() {
		for _, name := range []string{ical.PropCreated, ical.PropDateTimeStamp, ical.PropLastModified} {
			occurred, _ = props.DateTime(name, time.UTC)
			if !occurred.IsZero() {
				break
			}
		}
	}
	if occurred.IsZero() {
		occurred = obj.ModTime
	}
	if occurred.IsZero() {
		occurred = time.Unix(0, 0)
	}

	kind := "Calendar event"
	if comp.Name == ical.CompJournal {
		kind = "Journal note"
	}
	title, _ := props.Text(ical.PropSummary)
	title = cleanText(title)
	if title == "" {
		title = "(Untitled " + strings.ToLower(kind) + ")"
	}
	lines := []string{kind + ": " + title}
	for _, field := range []struct{ key, label string }{
		{ical.PropDateTimeStart, "Starts"}, {ical.PropDateTimeEnd, "Ends"},
		{ical.PropDuration, "Duration"}, {ical.PropLocation, "Location"},
		{ical.PropStatus, "Status"}, {ical.PropCategories, "Categories"},
		{ical.PropOrganizer, "Organizer"}, {ical.PropAttendee, "Attendee"},
		{ical.PropRecurrenceRule, "Recurrence"}, {ical.PropRecurrenceDates, "Additional dates"},
		{ical.PropExceptionDates, "Excluded dates"}, {ical.PropRecurrenceID, "Original occurrence"},
	} {
		for _, prop := range props[field.key] {
			if value := calendarValue(prop); value != "" {
				lines = append(lines, field.label+": "+value)
			}
		}
	}
	for _, prop := range props[ical.PropDescription] {
		if value := calendarValue(prop); value != "" {
			lines = append(lines, "", value)
		}
	}
	return events.Event{
		Source: caldavName, ExternalID: prefix + strings.TrimSuffix(namespace(uid, recurrence), ":"),
		Scope: scope, OccurredAt: occurred.UTC(), Title: title, Body: strings.Join(lines, "\n"),
	}, nil
}

func calendarValue(prop ical.Prop) string {
	switch prop.ValueType() {
	case ical.ValueDate, ical.ValueDateTime:
		if date, err := prop.DateTime(time.UTC); err == nil {
			if prop.ValueType() == ical.ValueDate || len(prop.Value) == len("20060102") {
				return date.Format(time.DateOnly)
			}
			if !strings.HasSuffix(prop.Value, "Z") && prop.Params.Get(ical.PropTimezoneID) == "" {
				return date.Format("2006-01-02 15:04:05") + " (local time)"
			}
			return date.Format(time.RFC3339)
		}
	case ical.ValueText:
		if text, err := prop.Text(); err == nil {
			return cleanText(text)
		}
	case ical.ValueCalendarAddress:
		addr := strings.TrimPrefix(prop.Value, "mailto:")
		if name := prop.Params.Get("CN"); name != "" {
			return cleanText(name + " <" + addr + ">")
		}
		return cleanText(addr)
	}
	if tz := prop.Params.Get(ical.PropTimezoneID); tz != "" {
		return cleanText(prop.Value + " (" + tz + ")")
	}
	return cleanText(prop.Value)
}
