package adminui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/reembed"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// stallAfter is how long work may wait, with nothing of its kind finishing,
// before the pages say it has stalled.
const stallAfter = 15 * time.Minute

// health is how Diderot's background work and its sources are doing, read
// fresh for the Overview and Operations > Status.
type health struct {
	Work    []workRow
	Sources []sourceHealth
	// Attention is what needs an administrator now, each with where it is
	// handled.
	Attention []attentionItem
}

// workRow is one kind of background work.
type workRow struct {
	ID    string
	Name  string
	Tone  string
	State string
	// Detail says what it is doing in a sentence; Note adds one more, with
	// an optional link.
	Detail   string
	Note     string
	Href     string
	LinkText string
	// When is a time worth knowing beside the detail, labelled WhenLabel.
	When      *time.Time
	WhenLabel string
	// Attention is what the row says on the Overview while it needs an
	// administrator, or "".
	Attention string
}

// sourceHealth is one registered source.
type sourceHealth struct {
	Name        string
	Title       string
	EventSource string
	Href        string
	Enabled     bool
	Tone        string
	State       string
	// Detail says what it is doing, or what is wrong.
	Detail string
}

type attentionItem struct {
	Text     string
	Href     string
	LinkText string
}

// Paths of the Operations tabs.
const (
	operationsPath = "/admin/operations"
	jobsPath       = "/admin/operations/jobs"
	failuresPath   = "/admin/operations/failures"
)

// queueState is one queue of background work.
type queueState struct {
	// Any reports that the work has ever had an item.
	Any bool
	// Waiting are the items left to do, Done those finished within
	// stallAfter, GaveUp those that failed every attempt.
	Waiting, Done, GaveUp int64
	// OldestUntried is when the longest-waiting item that has not been tried
	// yet entered the queue. An item that has failed an attempt is being
	// worked on, and is listed with the failures while it is retried.
	OldestUntried *time.Time
}

// stalled reports that nothing is working the queue: an item has waited
// longer than stallAfter without one attempt, while none finished. The wait
// is the item's own, in the queue, not the age of what it belongs to: a new
// image on a page indexed months ago has waited since it was added.
func (q queueState) stalled(now time.Time) bool {
	return q.Waiting > 0 && q.Done == 0 && q.OldestUntried != nil && now.Sub(*q.OldestUntried) > stallAfter
}

func (s *Server) loadHealth(ctx context.Context) (health, error) {
	var h health
	now := time.Now()
	var index, captions, documents queueState
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(DISTINCT c.event_id) FROM event_chunks c JOIN events e ON e.id = c.event_id
		    WHERE c.embedded_at IS NULL AND c.failure_count < $1 AND NOT e.deleted),
		  (SELECT min(c.queued_at) FROM event_chunks c JOIN events e ON e.id = c.event_id
		    WHERE c.embedded_at IS NULL AND c.failure_count = 0 AND NOT e.deleted),
		  (SELECT count(DISTINCT event_id) FROM event_chunks
		    WHERE embedded_at >= now() - make_interval(secs => $2)),
		  (SELECT count(DISTINCT c.event_id) FROM event_chunks c JOIN events e ON e.id = c.event_id
		    WHERE c.embedded_at IS NULL AND c.failure_count >= $1 AND NOT e.deleted),
		  EXISTS (SELECT 1 FROM event_images),
		  (SELECT count(*) FROM event_images i JOIN events e ON e.id = i.event_id
		    WHERE i.captioned_at IS NULL AND i.failure_count < $1 AND NOT e.deleted),
		  (SELECT min(i.queued_at) FROM event_images i JOIN events e ON e.id = i.event_id
		    WHERE i.captioned_at IS NULL AND i.failure_count = 0 AND NOT e.deleted),
		  (SELECT count(*) FROM event_images WHERE captioned_at >= now() - make_interval(secs => $2)),
		  (SELECT count(*) FROM event_images i JOIN events e ON e.id = i.event_id
		    WHERE i.captioned_at IS NULL AND i.failure_count >= $1 AND NOT e.deleted),
		  EXISTS (SELECT 1 FROM event_documents),
		  (SELECT count(*) FROM event_documents d JOIN events e ON e.id = d.event_id
		    WHERE d.extracted_at IS NULL AND d.rejected_reason IS NULL AND d.failure_count < $1 AND NOT e.deleted),
		  (SELECT min(d.queued_at) FROM event_documents d JOIN events e ON e.id = d.event_id
		    WHERE d.extracted_at IS NULL AND d.rejected_reason IS NULL AND d.failure_count = 0 AND NOT e.deleted),
		  (SELECT count(*) FROM event_documents WHERE extracted_at >= now() - make_interval(secs => $2)),
		  (SELECT count(*) FROM event_documents d JOIN events e ON e.id = d.event_id
		    WHERE d.extracted_at IS NULL AND d.rejected_reason IS NULL AND d.failure_count >= $1 AND NOT e.deleted)
	`, events.MaxFailures, stallAfter.Seconds()).Scan(
		&index.Waiting, &index.OldestUntried, &index.Done, &index.GaveUp,
		&captions.Any, &captions.Waiting, &captions.OldestUntried, &captions.Done, &captions.GaveUp,
		&documents.Any, &documents.Waiting, &documents.OldestUntried, &documents.Done, &documents.GaveUp,
	)
	if err != nil {
		return h, fmt.Errorf("background work: %w", err)
	}
	index.Any = true
	h.Work = append(h.Work, queueRow(now, "index", "Search index", index, queueWords{
		one: "item", many: "items", waiting: "waiting to be indexed", done: "indexed",
		upToDate: "Every item is searchable.", gaveUp: "could not be indexed",
		drained: "Nothing is waiting to be indexed.",
	}))
	if captions.Any {
		h.Work = append(h.Work, queueRow(now, "captions", "Image captions", captions, queueWords{
			one: "image", many: "images", waiting: "waiting for a caption", done: "captioned",
			upToDate: "Every image has a caption.", gaveUp: "could not be captioned",
			drained: "Nothing is waiting for a caption.",
		}))
	}
	if documents.Any {
		h.Work = append(h.Work, queueRow(now, "documents", "Document reading", documents, queueWords{
			one: "document", many: "documents", waiting: "waiting to be read", done: "read",
			upToDate: "Every document has been read.", gaveUp: "could not be read",
			drained: "Nothing is waiting to be read.",
		}))
	}
	if h.Sources, err = s.loadSourceHealth(ctx); err != nil {
		return h, err
	}
	summaryState, err := s.loadSummaryState(ctx)
	if err != nil {
		return h, err
	}
	summaries := summaryRow(summaryState)
	reports, err := s.reportWork(ctx)
	if err != nil {
		return h, err
	}
	jobs, err := s.jobWork(ctx, now)
	if err != nil {
		return h, err
	}
	for _, row := range []*workRow{summaries, reports, jobs} {
		if row != nil {
			h.Work = append(h.Work, *row)
		}
	}
	for _, row := range h.Work {
		if row.Attention != "" {
			h.Attention = append(h.Attention, attentionItem{Text: row.Attention, Href: row.Href, LinkText: row.LinkText})
		}
	}
	// Pushed agent sessions arrive with every source off, so an install
	// that has them isn't one that indexes nothing.
	if s.connectors != nil && len(h.Sources) > 0 && summaryState.Seen == 0 &&
		!slices.ContainsFunc(h.Sources, func(src sourceHealth) bool { return src.Enabled }) {
		h.Attention = append(h.Attention, attentionItem{
			Text: "No source is on, so Diderot has nothing to index.", Href: "/admin/connectors", LinkText: "Connectors",
		})
	}
	for _, src := range h.Sources {
		// The session feed waits only on summaries, which their own row
		// already raises while sessions wait.
		if src.EventSource == "agent_session" && summaries != nil && summaries.Attention != "" {
			continue
		}
		if src.Tone == toneAttention {
			h.Attention = append(h.Attention, attentionItem{Text: src.Title + ": " + lowerFirst(src.Detail), Href: src.Href, LinkText: src.Title})
		}
	}
	connections, err := s.connectionAttention(ctx)
	if err != nil {
		return h, err
	}
	h.Attention = append(h.Attention, connections...)
	providers, err := s.providerAttention(ctx)
	if err != nil {
		return h, err
	}
	h.Attention = append(h.Attention, providers...)
	if s.setup != nil {
		models, err := s.setup.Models(ctx)
		if err != nil {
			return h, err
		}
		for _, item := range models.Attention() {
			h.Attention = append(h.Attention, attentionItem{Text: item, Href: modelsPath, LinkText: "Models"})
		}
	}

	if index.GaveUp+captions.GaveUp+documents.GaveUp+summaryState.GaveUp > 0 {
		failed, _, err := s.failureStore().load(ctx, true)
		if err != nil {
			return h, err
		}
		var retryable int
		for _, item := range failed {
			if item.Cause.Retryable {
				retryable++
			}
		}
		if retryable > 0 {
			h.Attention = append(h.Attention, attentionItem{
				Text: fmt.Sprintf("%s stopped after %d attempts for a reason that retrying may fix.",
					countOf(int64(retryable), "item", "items"), events.MaxFailures),
				Href: failuresPath, LinkText: "Failures",
			})
		}
	}
	return h, nil
}

// queueWords are how a queue's row talks about its items. upToDate says
// that every item is done, drained that none waits while some gave up.
type queueWords struct {
	one, many, waiting, done, upToDate, drained, gaveUp string
}

func queueRow(now time.Time, id, name string, q queueState, words queueWords) workRow {
	row := workRow{ID: id, Name: name, Tone: toneGood, State: "up to date", Detail: words.upToDate}
	switch {
	case q.stalled(now):
		row.Tone, row.State = toneAttention, "needs attention"
		row.Detail = fmt.Sprintf("%s %s, and none %s in the last %d minutes.",
			countOf(q.Waiting, words.one, words.many), words.waiting, words.done, int(stallAfter.Minutes()))
		row.Attention = name + " has stalled: " + row.Detail
		row.Href, row.LinkText = operationsPath, "Status"
	case q.Waiting > 0:
		row.State = "working"
		row.Detail = fmt.Sprintf("%s %s; %s %s in the last %d minutes.",
			countOf(q.Waiting, words.one, words.many), words.waiting, formatCount(q.Done), words.done, int(stallAfter.Minutes()))
	}
	if q.GaveUp > 0 {
		if q.Waiting == 0 {
			row.Detail = words.drained
		}
		row.Note = fmt.Sprintf("%s %s.", countOf(q.GaveUp, words.one, words.many), words.gaveUp)
		if row.Attention == "" {
			row.Href, row.LinkText = failuresPath, "See failures"
		}
	}
	return row
}

// summaryState is whether session summaries run, and the agent sessions
// they have seen and that wait for one.
type summaryState struct {
	sessioningest.SummaryControlState
	Seen, Waiting int64
	// GaveUp are the sessions whose summary is parked: every attempt
	// failed, or the first failed for a cause no retry gets past.
	GaveUp int64
	// FeedOn reports that the session feed source is on, and PushOn that
	// the deployment takes pushed sessions.
	FeedOn, PushOn bool
}

// Relevant reports that agent sessions reach this install: a session has
// arrived, the session feed is on, or sessions can be pushed.
func (st summaryState) Relevant() bool {
	return st.Seen > 0 || st.FeedOn || st.PushOn
}

func (s *Server) loadSummaryState(ctx context.Context) (summaryState, error) {
	var st summaryState
	var err error
	if st.SummaryControlState, err = s.sessionSummaries(ctx); err != nil {
		return st, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE `+sessioningest.CandidatePendingSQL+` AND NOT `+sessioningest.CandidateParkedSQL+`),
		       count(*) FILTER (WHERE `+sessioningest.CandidatePendingSQL+` AND `+sessioningest.CandidateParkedSQL+`)
		  FROM session_ingest_candidates`).Scan(&st.Seen, &st.Waiting, &st.GaveUp); err != nil {
		return st, fmt.Errorf("session summaries: %w", err)
	}
	st.PushOn = s.setup != nil && s.setup.Facts().SessionPush != nil
	st.FeedOn, err = s.sourceOn(ctx, "agent_session")
	return st, err
}

// sourceOn reports whether the source writing eventSource is on.
func (s *Server) sourceOn(ctx context.Context, eventSource string) (bool, error) {
	src, ok := s.registries.Sources.Lookup(eventSource)
	if !ok || s.connectors == nil {
		return false, nil
	}
	views, err := s.connectors.List(ctx)
	if err != nil {
		return false, fmt.Errorf("connectors: %w", err)
	}
	for _, view := range views {
		if view.Name == src.Name() {
			return view.Enabled, nil
		}
	}
	return false, nil
}

// summaryRow is the session summaries row, nil while no agent sessions
// reach this install.
func summaryRow(st summaryState) *workRow {
	if !st.Relevant() {
		return nil
	}
	row := &workRow{ID: "summaries", Name: "Session summaries", Href: jobsPath + "#summaries", LinkText: "Pause or resume"}
	sessions := countOf(st.Waiting, "agent session", "agent sessions")
	switch {
	case st.EffectiveEnabled:
		row.Tone, row.State = toneGood, "on"
		row.Detail = "Every finished agent session is summarized and searchable."
		switch {
		case st.Waiting > 0:
			row.Detail = sessions + " waiting for a summary."
		case st.GaveUp > 0:
			// Not every session is searchable while some gave up.
			row.Detail = "No agent session is waiting for a summary."
		}
	case st.Blocked == sessioningest.SummaryPaused:
		row.Tone, row.State = toneNeutral, "paused"
		row.Detail = "An administrator paused summaries."
		row.LinkText = "Resume"
	default:
		row.Tone, row.State = toneNeutral, "off"
		row.Detail = st.Reason
		row.LinkText = "Details"
	}
	if st.GaveUp > 0 {
		row.Note = fmt.Sprintf("%s could not be summarized, and %s listed under Failures.",
			countOf(st.GaveUp, "agent session", "agent sessions"), pluralWord(st.GaveUp, "is", "are"))
	}
	if !st.EffectiveEnabled && st.Waiting > 0 {
		row.Tone = toneAttention
		row.Note = strings.TrimSpace(sessions + " waiting, not searchable until summaries run. " + row.Note)
		row.Attention = fmt.Sprintf("%s waiting for a summary %s not searchable: summaries are %s.",
			sessions, pluralWord(st.Waiting, "is", "are"), row.State)
	}
	return row
}

// reportWork is the scheduled reports row, nil while no report is on.
func (s *Server) reportWork(ctx context.Context) (*workRow, error) {
	var on, failing int64
	var delivered *time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE enabled),
		       count(*) FILTER (WHERE enabled AND last_error IS NOT NULL),
		       max(last_success_at) FILTER (WHERE enabled)
		  FROM scheduled_reports`).Scan(&on, &failing, &delivered); err != nil {
		return nil, fmt.Errorf("scheduled reports: %w", err)
	}
	if on == 0 {
		return nil, nil
	}
	row := &workRow{
		ID: "reports", Name: "Scheduled reports", Tone: toneGood, State: "on",
		Detail: countOf(on, "report", "reports") + " on.", Href: "/admin/reports", LinkText: "Reports",
		When: delivered, WhenLabel: "Last delivered",
	}
	if failing > 0 {
		row.Tone, row.State = toneAttention, "needs attention"
		row.Detail = fmt.Sprintf("%s of %d failed the last time %s.", countOf(failing, "report", "reports"), on, pluralWord(failing, "it ran", "they ran"))
		row.Attention = row.Detail
	}
	return row, nil
}

// jobWork is the row of the long-running jobs waiting or running, nil while
// there are none.
func (s *Server) jobWork(ctx context.Context, now time.Time) (*workRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind, status, heartbeat_at, stale_after_seconds, progress
		  FROM admin_jobs
		 WHERE status IN ('queued', 'running')
		 ORDER BY requested_at, id`)
	if err != nil {
		return nil, fmt.Errorf("active jobs: %w", err)
	}
	defer rows.Close()
	var parts []string
	stalled := false
	for rows.Next() {
		var kind, status string
		var heartbeat time.Time
		var staleSeconds int64
		var progress []byte
		if err := rows.Scan(&kind, &status, &heartbeat, &staleSeconds, &progress); err != nil {
			return nil, fmt.Errorf("active jobs scan: %w", err)
		}
		word, _ := jobState(status, now.Sub(heartbeat) > time.Duration(staleSeconds)*time.Second)
		part := jobName(kind) + ": " + word
		if word == "stalled" {
			stalled = true
			part = jobName(kind) + " stopped responding"
		} else if kind == reembed.JobKind && status == "running" {
			if percent, ok := reembedPercent(progress); ok {
				part += fmt.Sprintf(", %d%% done", percent)
			}
		}
		parts = append(parts, part)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("active jobs rows: %w", err)
	}
	if len(parts) == 0 {
		return nil, nil
	}
	row := &workRow{
		ID: "jobs", Name: "Maintenance jobs", Tone: toneGood, State: "working",
		Detail: strings.Join(parts, "; ") + ".", Href: jobsPath, LinkText: "Jobs and maintenance",
	}
	if stalled {
		row.Tone, row.State = toneAttention, "needs attention"
		row.Attention = row.Detail
	}
	return row, nil
}

// loadSourceHealth reads each registered source: whether it is on and, if
// it is, what keeps it from working.
func (s *Server) loadSourceHealth(ctx context.Context) ([]sourceHealth, error) {
	views := map[string]settings.View{}
	if s.connectors != nil {
		listed, err := s.connectors.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("connectors: %w", err)
		}
		for _, view := range listed {
			views[view.Name] = view
		}
	}
	var sources []sourceHealth
	for _, src := range s.registries.Sources.Sources() {
		item, err := s.sourceHealthOf(ctx, src, views[src.Name()])
		if err != nil {
			return nil, err
		}
		sources = append(sources, item)
	}
	return sources, nil
}

// failedTestReason is what a connector needs while its saved settings
// failed their last test.
const failedTestReason = "Its saved settings failed their last test. Fix what the test reports and save again."

// sourceHealthOf is one source's state, which every page that shows it
// reads here: off, on, or needs attention with what is wrong. view is the
// source's connector, the zero View when it has none.
func (s *Server) sourceHealthOf(ctx context.Context, src source.Source, view settings.View) (sourceHealth, error) {
	name, info := src.Name(), src.Info()
	item := sourceHealth{
		Name: name, Title: info.Title, EventSource: info.EventSource,
		Href: "/admin/connectors/" + name, Enabled: view.Enabled,
		Tone: toneNeutral, State: "off",
	}
	if !item.Enabled {
		return item, nil
	}
	deps := s.sourceDeps
	if deps.Pool == nil {
		deps.Pool = s.pool
	}
	item.Tone, item.State = toneGood, "on"
	var problems, notes []string
	status, reported, err := s.registries.Status(ctx, deps, name)
	if err != nil {
		return item, fmt.Errorf("%s status: %w", name, err)
	}
	if reported && status.Waiting != "" {
		problems = append(problems, status.Waiting)
	}
	switch info.EventSource {
	case slackEventSource:
		problem, note, err := s.slackHealth(ctx)
		if err != nil {
			return item, err
		}
		problems, notes = appendNonEmpty(problems, problem), appendNonEmpty(notes, note)
	case "notion":
		problem, err := s.notionHealth(ctx)
		if err != nil {
			return item, err
		}
		problems = appendNonEmpty(problems, problem)
	}
	if len(problems) == 0 && view.LastTest != nil && !view.LastTest.Passed {
		problems = append(problems, failedTestReason)
	}
	if len(problems) > 0 {
		item.Tone, item.State = toneAttention, "needs attention"
		item.Detail = strings.Join(problems, " ")
	} else {
		item.Detail = strings.Join(notes, " ")
	}
	return item, nil
}

// state is the source's state as the connector pages show it.
func (h sourceHealth) state() connectorState {
	state := connectorState{Tone: h.Tone, Word: h.State}
	if h.Tone == toneAttention {
		state.Reason = h.Detail
	} else {
		state.Note = h.Detail
	}
	return state
}

// slackHealth says which channels report an error, and how many are being
// indexed.
func (s *Server) slackHealth(ctx context.Context) (problem, note string, err error) {
	var channels, failing, reading, documents int64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE ingest_enabled),
		       count(*) FILTER (WHERE ingest_enabled AND last_error IS NOT NULL),
		       count(*) FILTER (WHERE ingest_enabled AND last_synced_at IS NULL),
		       (SELECT count(*) FROM slack_channel_config
		         WHERE doc_ingest_enabled AND doc_backfill_cursor IS NOT NULL)
		  FROM slack_channel_sync`).Scan(&channels, &failing, &reading, &documents); err != nil {
		return "", "", fmt.Errorf("slack channels: %w", err)
	}
	switch {
	case failing > 0 && failing == channels:
		problem = fmt.Sprintf("None of the %s the bot is in can be read.", countOf(channels, "channel", "channels"))
	case failing > 0:
		problem = fmt.Sprintf("%s of the %s the bot is in can't be read.", formatCount(failing), countOf(channels, "channel", "channels"))
	}
	note = countOf(channels, "channel", "channels") + " indexed."
	if reading > 0 {
		note += fmt.Sprintf(" Reading the history of %d.", reading)
	}
	if documents > 0 {
		note += fmt.Sprintf(" Reading earlier documents in %d.", documents)
	}
	return problem, note, nil
}

// notionHealth says what went wrong in Notion's last sync, when it did.
func (s *Server) notionHealth(ctx context.Context) (string, error) {
	var status, runErr string
	var failed int64
	err := s.pool.QueryRow(ctx, `
		SELECT status, failed_count, error FROM notion_sync_runs
		 ORDER BY started_at DESC, run_id DESC LIMIT 1`).Scan(&status, &failed, &runErr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("notion sync runs: %w", err)
	}
	switch status {
	case "failed":
		if reason := strings.TrimSpace(runErr); reason != "" {
			cause := classifyFailure(reason)
			return "The last sync failed: " + lowerFirst(cmp.Or(cause.Detail, cause.Message)) + ".", nil
		}
		return "The last sync failed.", nil
	case "partial":
		return fmt.Sprintf("The last sync could not read %s.", countOf(failed, "page", "pages")), nil
	}
	return "", nil
}

// countOf is a count and its noun: "1 item", "1.2K items".
func countOf(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return formatCount(n) + " " + many
}

func pluralWord(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func lowerFirst(text string) string {
	if text == "" {
		return ""
	}
	runes := []rune(text)
	// An acronym or a name such as "Slack" keeps its capital.
	if len(runes) > 1 && strings.ToUpper(string(runes[1])) == string(runes[1]) && strings.ToLower(string(runes[1])) != string(runes[1]) {
		return text
	}
	return strings.ToLower(string(runes[0])) + string(runes[1:])
}

func appendNonEmpty(list []string, value string) []string {
	if value == "" {
		return list
	}
	return append(list, value)
}
