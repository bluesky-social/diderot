package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bluesky-social/diderot/internal/events"
)

// outlineSummaryFloor is the least summary text, in bytes, an outline entry
// is given before entries start covering more than one stretch each.
const outlineSummaryFloor = 200

// outlineEntry is one entry of a session outline: a run of consecutive
// stretches, which is a single stretch unless the session has too many to
// give each its own entry.
type outlineEntry struct {
	head      string
	stretches []events.SessionStretch
}

// sessionOutline is what expanding a whole session returns when its records
// do not fit in budget bytes: every stretch in session order with the
// event_id that expands to its records, and as much of each summary as fits.
// The summary room is shared so that short summaries stay whole and long ones
// are cut to the same length. A session with so many stretches that one entry
// each would leave under outlineSummaryFloor bytes of summary gets entries of
// several consecutive stretches, so the outline still reaches the end of the
// session. Only a session whose event_ids alone exceed budget comes out
// longer than budget.
func sessionOutline(records int, stretches []events.SessionStretch, budget int) string {
	var (
		intro   string
		entries []outlineEntry
		room    int
	)
	for per := 1; ; per++ {
		intro, entries = outlineEntries(records, stretches, per)
		room = budget - len(intro)
		floor := 0
		for _, entry := range entries {
			room -= len(entry.head) + len("\n\n")
			floor += min(entry.summaryLen(), outlineSummaryFloor)
		}
		if floor <= room || per >= len(stretches) {
			break
		}
	}

	// Shortest first, each takes what it needs up to an equal share of what
	// is left, so what a short summary leaves goes to the longer ones.
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return entries[a].summaryLen() - entries[b].summaryLen()
	})
	limits := make([]int, len(entries))
	room = max(room, 0)
	for rank, i := range order {
		limits[i] = min(entries[i].summaryLen(), room/(len(order)-rank))
		room -= limits[i]
	}

	var out strings.Builder
	out.WriteString(intro)
	for i, entry := range entries {
		out.WriteString(entry.head)
		out.WriteString(entry.summary(limits[i]))
		out.WriteString("\n\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// outlineEntries splits stretches into entries of per consecutive stretches
// and writes the sentence that tells the reader what the entries are.
func outlineEntries(records int, stretches []events.SessionStretch, per int) (string, []outlineEntry) {
	const shortened = " A summary ending in … was shortened to fit. To read a stretch verbatim, call `expand` with its event_id.\n\n"
	intro := fmt.Sprintf("This session is too long to return whole (%d records), so this is an outline of its %d stretches in order, each with a summary of what happened in it.", records, len(stretches)) + shortened
	if per > 1 {
		intro = fmt.Sprintf("This session is too long to return whole (%d records in %d stretches), so this is an outline in order. Each entry covers up to %d consecutive stretches: it lists their event_ids in session order, then the start of their summaries.", records, len(stretches), per) + shortened
	}
	entries := make([]outlineEntry, 0, (len(stretches)+per-1)/per)
	for run := range slices.Chunk(stretches, per) {
		ids := make([]string, len(run))
		for i, stretch := range run {
			ids[i] = strconv.FormatInt(stretch.EventID, 10)
		}
		label := "event_id"
		if len(run) > 1 {
			label = "event_ids"
		}
		last := run[len(run)-1]
		entries = append(entries, outlineEntry{
			head: fmt.Sprintf("%s %s | records %d-%d | %s\n", label, strings.Join(ids, ", "),
				run[0].Start, last.End, last.OccurredAt.UTC().Format(time.RFC3339)),
			stretches: run,
		})
	}
	return intro, entries
}

// summaryLen is the length of the entry's whole summary text.
func (e outlineEntry) summaryLen() int {
	n := len(e.stretches) - 1
	for _, stretch := range e.stretches {
		n += len(stretch.Summary)
	}
	return n
}

// summary is the entry's stretch summaries in order, in at most limit bytes.
func (e outlineEntry) summary(limit int) string {
	const ellipsis = "…"
	whole := e.summaryLen() <= limit
	var text strings.Builder
	for i, stretch := range e.stretches {
		if i > 0 {
			text.WriteByte(' ')
		}
		text.WriteString(stretch.Summary)
		if !whole && text.Len() >= limit {
			break
		}
	}
	if whole {
		return text.String()
	}
	cut := limit - len(ellipsis)
	if cut <= 0 {
		return ""
	}
	joined := text.String()
	for cut > 0 && !utf8.RuneStart(joined[cut]) {
		cut--
	}
	return strings.TrimRight(joined[:cut], " \n") + ellipsis
}
