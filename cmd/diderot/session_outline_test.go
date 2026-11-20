package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// outlineStretches is n consecutive stretches of ten records each, with
// event ids from firstID and summaries of the given length.
func outlineStretches(n int, firstID int64, summaryLen int) []events.SessionStretch {
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	stretches := make([]events.SessionStretch, n)
	for i := range stretches {
		label := fmt.Sprintf("stretch %d: ", i)
		stretches[i] = events.SessionStretch{
			EventID:    firstID + int64(i),
			Start:      i * 10,
			End:        (i + 1) * 10,
			OccurredAt: at.Add(time.Duration(i) * time.Minute),
			Summary:    label + strings.Repeat("é", max(summaryLen-len(label), 0)/2),
		}
	}
	return stretches
}

// requireEveryStretch checks that outline names every stretch's event_id, in
// session order.
func requireEveryStretch(t *testing.T, outline string, stretches []events.SessionStretch) {
	t.Helper()
	_, entries, found := strings.Cut(outline, "\n\n")
	require.True(t, found)
	var ids []string
	for line := range strings.Lines(entries) {
		if list, ok := strings.CutPrefix(line, "event_id "); ok {
			ids = append(ids, strings.TrimSpace(strings.Split(list, " | ")[0]))
		} else if list, ok := strings.CutPrefix(line, "event_ids "); ok {
			for id := range strings.SplitSeq(strings.Split(list, " | ")[0], ", ") {
				ids = append(ids, id)
			}
		}
	}
	want := make([]string, len(stretches))
	for i, stretch := range stretches {
		want[i] = fmt.Sprint(stretch.EventID)
	}
	require.Equal(t, want, ids)
}

func TestSessionOutlineKeepsWholeSummariesThatFit(t *testing.T) {
	stretches := outlineStretches(14, 2_000_000, 900)
	outline := sessionOutline(140, stretches, ask.ExpandContentBudget)

	require.LessOrEqual(t, len(outline), ask.ExpandContentBudget)
	require.True(t, strings.HasPrefix(outline, "This session is too long to return whole (140 records), so this is an outline of its 14 stretches in order"))
	require.Contains(t, outline, "call `expand` with its event_id")
	requireEveryStretch(t, outline, stretches)
	require.Contains(t, outline, "event_id 2000005 | records 50-60 | 2026-03-01T09:05:00Z\n"+stretches[5].Summary+"\n\n")
	require.NotContains(t, outline, "é…")
}

func TestSessionOutlineSharesRoomFairly(t *testing.T) {
	stretches := outlineStretches(100, 2_000_000, 2000)
	stretches[40].Summary = "a short one"
	outline := sessionOutline(1000, stretches, ask.ExpandContentBudget)

	require.LessOrEqual(t, len(outline), ask.ExpandContentBudget)
	require.Greater(t, len(outline), ask.ExpandContentBudget-200, "the room is used")
	requireEveryStretch(t, outline, stretches)
	require.Contains(t, outline, "\na short one\n\n")
	// Every long summary is cut to the same length, so the last stretch is
	// as readable as the first.
	_, entries, _ := strings.Cut(outline, "\n\n")
	var lengths []int
	for entry := range strings.SplitSeq(entries, "\n\n") {
		_, summary, _ := strings.Cut(entry, "\n")
		if summary != "a short one" {
			require.True(t, strings.HasSuffix(summary, "é…"), summary)
			lengths = append(lengths, len(summary))
		}
	}
	require.Len(t, lengths, 99)
	for _, length := range lengths {
		require.InDelta(t, lengths[0], length, 2)
		require.Greater(t, length, 500)
	}
}

func TestSessionOutlineCoversVeryManyStretches(t *testing.T) {
	// More stretches than the largest session in the reference corpus, with
	// eight-digit event ids.
	stretches := outlineStretches(5000, 10_000_000, 900)
	outline := sessionOutline(50_000, stretches, ask.ExpandContentBudget)

	require.LessOrEqual(t, len(outline), ask.ExpandContentBudget)
	requireEveryStretch(t, outline, stretches)
	require.Contains(t, outline, "consecutive stretches: it lists their event_ids in session order")
	require.Contains(t, outline, "| records 0-")
	require.Contains(t, outline, "-50000 | ")
	_, entries, _ := strings.Cut(outline, "\n\n")
	for entry := range strings.SplitSeq(entries, "\n\n") {
		_, summary, _ := strings.Cut(entry, "\n")
		require.GreaterOrEqual(t, len(summary), outlineSummaryFloor-len("é…"), entry)
	}
}

// TestExpandWholeSession reads a session-level event through the expander: a
// session that fits comes back whole, and a longer one as an outline of the
// stretches the caller may read.
func TestExpandWholeSession(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "cmd_session_outline"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	const revision = "sha256:outline-test"
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	upsert := func(sessionID, suffix, kind, scope, body string, start, end int) int64 {
		raw, err := json.Marshal(map[string]string{"revision": revision, "event_kind": kind})
		require.NoError(t, err)
		result, err := sink.Upsert(ctx, events.Event{
			Scope: scope, Source: "agent_session", ExternalID: sessionID + suffix,
			OccurredAt: at.Add(time.Duration(end) * time.Minute), Body: body,
			SessionID: sessionID, RecordStart: start, RecordEnd: end, Raw: raw,
		})
		require.NoError(t, err)
		return result.EventID
	}

	const shortID = "019f0083-e3fe-7355-b31c-78efefcc1701"
	const longID = "019f0083-e3fe-7355-b31c-78efefcc1702"
	records := map[string]string{
		shortID: "SHORT_SESSION_RECORDS\n",
		longID:  strings.Repeat("x", ask.ExpandContentBudget+1),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		for sessionID, body := range records {
			if request.URL.Path == "/api/v1/sessions/"+sessionID+"/records/redacted" {
				if request.URL.Query().Get("end") == "30" {
					_, _ = writer.Write([]byte(body))
				} else {
					_, _ = writer.Write([]byte("STRETCH_RECORDS\n"))
				}
				return
			}
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(server.Close)
	feed := sessionfeed.NewClient(server.URL, "test-only", server.Client())
	feedSource := &sessionFeedSource{feed: func() *sessionfeed.Client { return feed }}
	expander := newSourceExpander(source.NewRegistry(feedSource), source.Deps{Pool: pool, Sink: sink})
	internal := ask.WithAllowedScopes(ctx, []string{events.ScopeInternal})

	var whole, second int64
	for _, sessionID := range []string{shortID, longID} {
		id := upsert(sessionID, ":session", "native_session", events.ScopeInternal, "whole-session summary", 0, 30)
		first := upsert(sessionID, ":records:0:10", "native_session_chunk", events.ScopeInternal, "set up the branch", 0, 10)
		next := upsert(sessionID, ":records:10:20", "native_session_chunk", events.ScopeInternal, "found the failing query", 10, 20)
		upsert(sessionID, ":records:20:30", "native_session_chunk", "private:elsewhere", "out of the caller's reach", 20, 30)
		replaced := upsert(sessionID, ":records:0:20", "native_session_chunk", events.ScopeInternal, "an earlier split", 0, 20)
		_, err := sink.Delete(ctx, "agent_session", sessionID+":records:0:20")
		require.NoError(t, err)
		require.NotZero(t, first+replaced)
		whole, second = id, next
		if sessionID == shortID {
			expanded, err := expander.Expand(internal, id)
			require.NoError(t, err)
			require.Equal(t, "SHORT_SESSION_RECORDS\n", expanded.Content)
		}
	}

	expanded, err := expander.Expand(internal, whole)
	require.NoError(t, err)
	require.False(t, expanded.Truncated)
	require.LessOrEqual(t, len(expanded.Content), ask.ExpandContentBudget)
	require.True(t, strings.HasPrefix(expanded.Content, "This session is too long to return whole (30 records), so this is an outline of its 2 stretches in order"), expanded.Content)
	require.Contains(t, expanded.Content, "| records 0-10 | 2026-03-01T09:10:00Z\nset up the branch\n\n")
	require.True(t, strings.HasSuffix(expanded.Content,
		fmt.Sprintf("event_id %d | records 10-20 | 2026-03-01T09:20:00Z\nfound the failing query", second)), expanded.Content)
	require.NotContains(t, expanded.Content, "out of the caller's reach")
	require.NotContains(t, expanded.Content, "an earlier split")

	// The event_id an outline entry carries expands to that stretch's records.
	stretch, err := expander.Expand(internal, second)
	require.NoError(t, err)
	require.Equal(t, "STRETCH_RECORDS\n", stretch.Content)
}
