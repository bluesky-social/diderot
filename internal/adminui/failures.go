package adminui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/adminaction"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/sessioningest"
)

// The kinds of background work whose items can fail.
const (
	failureKindIndex    = "index"
	failureKindCaption  = "caption"
	failureKindDocument = "document"
	failureKindSummary  = "summary"
)

var failureKindNames = map[string]string{
	failureKindIndex:    "Search index",
	failureKindCaption:  "Image captions",
	failureKindDocument: "Document reading",
	failureKindSummary:  "Session summaries",
}

const (
	// failureScanLimit bounds the failures one view reads, the most recently
	// failed first. A stored failure is at most 512 runes.
	failureScanLimit = 5000
	failurePageSize  = 25
)

var failuresPage = mustPage("operations_failures.html")

type failuresData struct {
	pageData
	CSRFToken string
	Notice    string
	Error     string
	// Groups are the failures by kind of work and cause, what can be retried
	// first.
	Groups []failureGroup
	// Truncated says there are more failures than one view reads.
	Truncated bool
	// Kind and Cause narrow Items to one group; Group is that group.
	Kind  string
	Cause string
	Group *failureGroup
	Items []failureItem
	Pages pagination
}

type failureGroup struct {
	Kind     string
	KindName string
	Cause    failureCause
	Items    int
	GaveUp   int
	Retrying int
	// LastFailedAt is when one of the group's items last failed, nil when
	// none of them has a recorded time.
	LastFailedAt *time.Time
	// NewestAt is the latest of the items' own dates at their sources.
	NewestAt  time.Time
	FilterURL string
}

// failureItem is one item whose work failed.
type failureItem struct {
	Kind     string
	KindName string
	ID       int64
	// Record identifies the stored row, for someone looking it up.
	Record string
	// Label names the item: a document's file name, or a message's title or
	// opening words.
	Label string
	// Where is where it is: a Slack channel, or its source.
	Where string
	// URL opens the item at its source, when it has one.
	URL      string
	Source   string
	Attempts int
	GaveUp   bool
	// FailedAt is when the work last failed. It is nil for a failure from
	// before failures were timed: nothing recorded when that was.
	FailedAt *time.Time
	// ItemAt is the item's own date at its source.
	ItemAt time.Time
	Cause  failureCause
	Raw    string
}

// failureStore reads and retries failed background work.
type failureStore struct {
	pool *pgxpool.Pool
	// sourceTitle names an event source.
	sourceTitle func(eventSource string) string
	// sessionURL is where an agent session can be opened, "" for nowhere.
	sessionURL func(sessionID string) string
}

func (s *Server) failureStore() failureStore {
	return failureStore{pool: s.pool, sourceTitle: s.sourceTitle, sessionURL: func(sessionID string) string {
		return ownerSessionViewerURL(s.sessionViewerURL(), sessionID)
	}}
}

// sourceTitle is how pages name an event source: its source's title, or the
// stored value for one no registered source writes.
func (s *Server) sourceTitle(eventSource string) string {
	if src, ok := s.registries.Sources.Lookup(eventSource); ok {
		return src.Info().Title
	}
	return eventSource
}

// load reads the failing items of kinds (every kind when none), the most
// recently failed first, the ones that gave up only when gaveUp. Items whose
// failure has no recorded time come last, by their own dates. truncated says
// there were more than failureScanLimit.
func (f failureStore) load(ctx context.Context, gaveUp bool, kinds ...string) (items []failureItem, truncated bool, err error) {
	if len(kinds) == 0 {
		kinds = []string{failureKindIndex, failureKindCaption, failureKindDocument, failureKindSummary}
	}
	// Each kind of work on an event reads the same columns: the work's own
	// row, the event it belongs to, and the Slack channel's name for a Slack
	// event. The last four are a session summary's alone.
	const columns = `e.source, COALESCE(e.url, ''), COALESCE(e.title, ''), left(e.body, 400),
	       COALESCE(ci.name, ''), w.failure_count, w.occurred_at, w.last_failure, w.last_failed_at,
	       ''::text, ''::text, ''::text, false`
	const joins = `JOIN events e ON e.id = w.event_id
	  LEFT JOIN slack_channel_info ci ON e.source = 'slack' AND ci.channel_id = split_part(e.external_id, ':', 1)`
	gaveUpClause := ""
	if gaveUp {
		gaveUpClause = " AND w.failure_count >= $2"
	}
	var parts []string
	for _, kind := range kinds {
		switch kind {
		case failureKindIndex:
			parts = append(parts, `SELECT 'index'::text, w.id, w.kind, ''::text, `+columns+`
			  FROM event_chunks w `+joins+`
			 WHERE w.embedded_at IS NULL AND w.last_failure IS NOT NULL AND NOT e.deleted`+gaveUpClause)
		case failureKindCaption:
			parts = append(parts, `SELECT 'caption'::text, w.id, ''::text, ''::text, `+columns+`
			  FROM event_images w `+joins+`
			 WHERE w.captioned_at IS NULL AND w.last_failure IS NOT NULL AND NOT e.deleted`+gaveUpClause)
		case failureKindDocument:
			parts = append(parts, `SELECT 'document'::text, w.id, ''::text, w.filename, `+columns+`
			  FROM event_documents w `+joins+`
			 WHERE w.extracted_at IS NULL AND w.rejected_reason IS NULL AND w.last_failure IS NOT NULL
			   AND NOT e.deleted`+gaveUpClause)
		case failureKindSummary:
			// A session whose current revision last failed to be summarized:
			// still being retried, or parked. Its date is the session's own.
			parked := ""
			if gaveUp {
				parked = " AND " + sessioningest.CandidateParkedSQL
			}
			parts = append(parts, `SELECT 'summary'::text, 0::bigint, ''::text, ''::text, 'agent_session'::text, ''::text,
			       ''::text, ''::text, ''::text, index_failure_count,
			       COALESCE(source_updated_at, updated_at, updated_in_db), index_last_error, index_last_failed_at,
			       session_id, client, owner, `+sessioningest.CandidateParkedSQL+`
			  FROM session_ingest_candidates
			 WHERE `+sessioningest.CandidatePendingSQL+` AND `+sessioningest.CandidateFailingSQL+parked)
		default:
			return nil, false, fmt.Errorf("unknown kind of work %q", kind)
		}
	}
	query := strings.Join(parts, "\n UNION ALL\n") + `
		 ORDER BY 13 DESC NULLS LAST, 11 DESC, 2 DESC, 14 DESC
		 LIMIT $1`
	args := []any{failureScanLimit + 1}
	if strings.Contains(query, "$2") {
		args = append(args, events.MaxFailures)
	}
	rows, err := f.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("failures: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item failureItem
		var part, filename, eventSource, title, excerpt, channel, sessionID, client, owner string
		var parked bool
		if err := rows.Scan(&item.Kind, &item.ID, &part, &filename, &eventSource, &item.URL, &title, &excerpt,
			&channel, &item.Attempts, &item.ItemAt, &item.Raw, &item.FailedAt,
			&sessionID, &client, &owner, &parked); err != nil {
			return nil, false, fmt.Errorf("failures scan: %w", err)
		}
		if len(items) == failureScanLimit {
			truncated = true
			break
		}
		item.KindName = failureKindNames[item.Kind]
		item.Cause = classifyFailure(item.Raw)
		item.Source = f.sourceTitle(eventSource)
		if item.Kind == failureKindSummary {
			// A session is parked when its attempts are used up, or at once
			// for a cause no retry can get past.
			item.GaveUp = parked
			item.Record = "session " + sessionID
			item.Label = displaySessionClient(client) + " session"
			item.Where = owner
			if f.sessionURL != nil {
				item.URL = f.sessionURL(sessionID)
			}
		} else {
			item.GaveUp = item.Attempts >= events.MaxFailures
			item.Record = item.Kind + " " + strconv.FormatInt(item.ID, 10)
			item.Label = failureLabel(item.Kind, part, filename, title, excerpt)
			item.Where = item.Source
			if channel != "" {
				item.Where = "#" + channel
			}
		}
		if !strings.HasPrefix(item.URL, "https://") && !strings.HasPrefix(item.URL, "http://") {
			item.URL = ""
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("failures rows: %w", err)
	}
	return items, truncated, nil
}

// failureLabel names a failing item: a document by its file name, anything
// else by its event's title or opening words.
func failureLabel(kind, part, filename, title, excerpt string) string {
	if kind == failureKindDocument && strings.TrimSpace(filename) != "" {
		return filename
	}
	label := strings.TrimSpace(title)
	if label == "" {
		label = compactText(excerpt, 90)
	}
	if label == "" {
		label = "Untitled item"
	}
	switch part {
	case "image_caption":
		label += " (image caption)"
	case "document_text":
		label += " (document text)"
	}
	return label
}

// RetryFailures puts the items of kind that gave up for cause back in their
// queue: their attempts start again from none, and their wait from now.
func (f failureStore) RetryFailures(ctx context.Context, kind, cause string) (int64, error) {
	if _, ok := failureKindNames[kind]; !ok {
		return 0, fmt.Errorf("unknown kind of work %q", kind)
	}
	if kind == failureKindSummary {
		return f.retrySummaries(ctx, cause)
	}
	ids, err := f.stoppedFor(ctx, kind, cause)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	var statement string
	switch kind {
	case failureKindIndex:
		statement = `UPDATE event_chunks SET failure_count = 0, last_failure = NULL, last_failed_at = NULL, queued_at = now()
		 WHERE id = ANY($1) AND embedded_at IS NULL AND failure_count >= $2`
	case failureKindCaption:
		statement = `UPDATE event_images SET failure_count = 0, last_failure = NULL, last_failed_at = NULL, claimed_at = NULL,
		        queued_at = now()
		 WHERE id = ANY($1) AND captioned_at IS NULL AND failure_count >= $2`
	case failureKindDocument:
		statement = `UPDATE event_documents SET failure_count = 0, last_failure = NULL, last_failed_at = NULL, claimed_at = NULL,
		        queued_at = now()
		 WHERE id = ANY($1) AND extracted_at IS NULL AND rejected_reason IS NULL AND failure_count >= $2`
	}
	tag, err := f.pool.Exec(ctx, statement, ids, events.MaxFailures)
	if err != nil {
		return 0, fmt.Errorf("retry failures: %w", err)
	}
	return tag.RowsAffected(), nil
}

// retrySummaries puts every session parked for cause back to be summarized:
// its attempts start again from none and the next pass of the session worker
// takes it. Like stoppedFor it reads every parked session, not the newest
// failureScanLimit.
func (f failureStore) retrySummaries(ctx context.Context, cause string) (int64, error) {
	rows, err := f.pool.Query(ctx, `SELECT session_id, index_last_error FROM session_ingest_candidates
		 WHERE `+sessioningest.CandidatePendingSQL+` AND `+sessioningest.CandidateFailingSQL+`
		   AND `+sessioningest.CandidateParkedSQL)
	if err != nil {
		return 0, fmt.Errorf("parked sessions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return 0, fmt.Errorf("parked sessions scan: %w", err)
		}
		found := classifyFailure(raw)
		if found.Key != cause {
			continue
		}
		if !found.Retryable {
			return 0, errors.New(found.Message + ": retrying would fail the same way")
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("parked sessions rows: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	// Guarded on the session still being parked at the revision that
	// failed: one that changed since is already back in the queue.
	tag, err := f.pool.Exec(ctx, `UPDATE session_ingest_candidates
		    SET index_quarantined_at = NULL, index_retry_at = NULL, index_failure_count = 0,
		        index_last_error = NULL, index_last_failed_at = NULL, updated_in_db = now()
		  WHERE session_id = ANY($1) AND `+sessioningest.CandidatePendingSQL+` AND `+sessioningest.CandidateParkedSQL, ids)
	if err != nil {
		return 0, fmt.Errorf("retry parked sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// stoppedFor is every item of kind that gave up for cause. Unlike load it
// reads them all, not the newest failureScanLimit, so newer failures of
// other causes can't hide them.
func (f failureStore) stoppedFor(ctx context.Context, kind, cause string) ([]int64, error) {
	var statement string
	switch kind {
	case failureKindIndex:
		statement = `SELECT w.id, w.last_failure FROM event_chunks w JOIN events e ON e.id = w.event_id
		 WHERE w.embedded_at IS NULL AND w.failure_count >= $1 AND w.last_failure IS NOT NULL AND NOT e.deleted`
	case failureKindCaption:
		statement = `SELECT w.id, w.last_failure FROM event_images w JOIN events e ON e.id = w.event_id
		 WHERE w.captioned_at IS NULL AND w.failure_count >= $1 AND w.last_failure IS NOT NULL AND NOT e.deleted`
	case failureKindDocument:
		statement = `SELECT w.id, w.last_failure FROM event_documents w JOIN events e ON e.id = w.event_id
		 WHERE w.extracted_at IS NULL AND w.rejected_reason IS NULL AND w.failure_count >= $1
		   AND w.last_failure IS NOT NULL AND NOT e.deleted`
	}
	rows, err := f.pool.Query(ctx, statement, events.MaxFailures)
	if err != nil {
		return nil, fmt.Errorf("stopped failures: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("stopped failures scan: %w", err)
		}
		found := classifyFailure(raw)
		if found.Key != cause {
			continue
		}
		if !found.Retryable {
			return nil, errors.New(found.Message + ": retrying would fail the same way")
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stopped failures rows: %w", err)
	}
	return ids, nil
}

// groupFailures groups items by kind of work and cause: those an
// administrator can retry first, then the largest.
func groupFailures(items []failureItem) []failureGroup {
	index := map[[2]string]int{}
	var groups []failureGroup
	for _, item := range items {
		key := [2]string{item.Kind, item.Cause.Key}
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, failureGroup{
				Kind: item.Kind, KindName: item.KindName, Cause: item.Cause,
				FilterURL: "?" + url.Values{"kind": {item.Kind}, "cause": {item.Cause.Key}}.Encode() + "#items",
			})
		}
		group := &groups[i]
		group.Items++
		if item.GaveUp {
			group.GaveUp++
		} else {
			group.Retrying++
		}
		if item.FailedAt != nil && (group.LastFailedAt == nil || item.FailedAt.After(*group.LastFailedAt)) {
			group.LastFailedAt = item.FailedAt
		}
		if item.ItemAt.After(group.NewestAt) {
			group.NewestAt = item.ItemAt
		}
	}
	slices.SortStableFunc(groups, func(a, b failureGroup) int {
		if a.CanRetry() != b.CanRetry() {
			if a.CanRetry() {
				return -1
			}
			return 1
		}
		return b.Items - a.Items
	})
	return groups
}

// CanRetry reports that the group has items that gave up for a cause that
// retrying can get past.
func (g failureGroup) CanRetry() bool { return g.Cause.Retryable && g.GaveUp > 0 }

func (s *Server) handleFailures(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	data := failuresData{
		Notice: cleanNotice(query.Get("notice")), Error: cleanNotice(query.Get("error")),
		Kind: strings.TrimSpace(query.Get("kind")), Cause: strings.TrimSpace(query.Get("cause")),
	}
	items, truncated, err := s.failureStore().load(r.Context(), false)
	if err != nil {
		s.logger.Error("admin failures query failed", "err", err)
		http.Error(w, "failures query failed", http.StatusInternalServerError)
		return
	}
	data.Groups, data.Truncated = groupFailures(items), truncated
	if data.Kind != "" || data.Cause != "" {
		for i := range data.Groups {
			if data.Groups[i].Kind == data.Kind && data.Groups[i].Cause.Key == data.Cause {
				data.Group = &data.Groups[i]
			}
		}
		if data.Group == nil {
			data.Error = "Nothing fails for that cause any more, so every failure is shown."
			data.Kind, data.Cause = "", ""
		}
	}
	if data.Group != nil {
		items = slices.DeleteFunc(items, func(item failureItem) bool {
			return item.Kind != data.Kind || item.Cause.Key != data.Cause
		})
	}
	page, _ := strconv.Atoi(query.Get("page"))
	data.Items, data.Pages = pageOfFailures(items, page, query)

	token, err := issueCSRFToken(w, r)
	if err != nil {
		s.logger.Error("admin failures CSRF token failed", "err", err)
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	data.CSRFToken = token
	data.pageData = s.page(r, "Failures", navOperations)
	data.CurrentTab = tabOperationsFailures
	data.Stylesheet = "operations.css"
	s.render(w, failuresPage, data)
}

// pageOfFailures is the items on page (from 1) and the pagination row.
func pageOfFailures(items []failureItem, page int, query url.Values) ([]failureItem, pagination) {
	pages := max(1, (len(items)+failurePageSize-1)/failurePageSize)
	page = min(max(page, 1), pages)
	start := (page - 1) * failurePageSize
	end := min(start+failurePageSize, len(items))
	link := func(to int) string {
		next := cloneURLValues(query)
		next.Del("notice")
		next.Del("error")
		if to == 1 {
			next.Del("page")
		} else {
			next.Set("page", strconv.Itoa(to))
		}
		return "?" + next.Encode() + "#items"
	}
	nav := pagination{Summary: fmt.Sprintf("%s, most recently failed first", plural(len(items), "item"))}
	if pages > 1 {
		nav.Summary = fmt.Sprintf("%d–%d of %d items, most recently failed first", start+1, end, len(items))
		if page > 1 {
			nav.BackURL = link(page - 1)
		}
		if page < pages {
			nav.ForwardURL = link(page + 1)
		}
	}
	return items[start:end], nav
}

func (s *Server) handleFailuresRetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	kind, cause := r.Form.Get("kind"), r.Form.Get("cause")
	retried, err := adminaction.RetryFailures(r.Context(), s.audit(r), s.failureStore(), kind, cause)
	if s.auditFailed(w, err) {
		return
	}
	back := "/admin/operations/failures?"
	if err != nil {
		s.logger.Warn("admin failure retry failed", "kind", kind, "cause", cause, "err", err)
		http.Redirect(w, r, back+url.Values{"error": {"Nothing was retried: " + err.Error()}}.Encode(), http.StatusSeeOther)
		return
	}
	notice := fmt.Sprintf("Retrying %s: they are back in the queue and are tried again within a few minutes.", plural(int(retried), "item"))
	if retried == 0 {
		notice = "Nothing was waiting to be retried for that cause."
	}
	http.Redirect(w, r, back+url.Values{"notice": {notice}}.Encode(), http.StatusSeeOther)
}
