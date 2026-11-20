package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// sessionFeedSource is the session feed source. Its Run holds the session
// ingest worker reading the feed, and its title backfill. With push ingest
// (DIDEROT_SESSION_INGEST_SOURCE=push) that worker reads pushed sessions
// instead and runs without this connector, so Run starts nothing. Its
// events are every agent session, which expand from diderot's own store
// when pushed and through the feed otherwise.
type sessionFeedSource struct {
	sessionfeed.Connector
	term *settings.Live[leaderTerm]
	// feed returns the feed's client, nil while the connector is off.
	feed func() *sessionfeed.Client
	// push serves pushed sessions' records; nil unless push ingest is on.
	push *sessioningest.PushStore
	// summaries says whether sessions are summarized; nil in tests.
	summaries *sessioningest.SummaryControl
}

func (s *sessionFeedSource) Run(ctx context.Context, deps source.Deps, resolved settings.Resolved) error {
	term, ok := s.term.Get()
	if !ok {
		return errNoLeaderTerm
	}
	if term.ingest.cfg.SessionPush.Enabled {
		deps.Logger.Info("session ingest reads pushed sessions; the feed is used only to expand its own sessions")
		<-ctx.Done()
		return nil
	}
	sup := worker.NewSupervisor(deps.Logger, deps.Metrics)
	feed := sessioningest.NewClientFeed(s.NewClient(resolved))
	if err := term.ingest.register(sup, deps.Sink, feed, sessionfeed.SettingsFrom(resolved).ViewerBaseURL); err != nil {
		return err
	}
	sup.Run(ctx)
	return nil
}

// Expand returns the redacted session records the event's stored
// (session_id, record_start, record_end) range covers. A whole session whose
// records do not fit expand's budget comes back as an outline of its
// stretches instead, so the reader can find the stretch to expand: cut to the
// budget, a long session would be only its opening records.
func (s *sessionFeedSource) Expand(ctx context.Context, deps source.Deps, eventID int64, scopes []string) (source.Expanded, error) {
	rng, err := deps.Sink.AgentSessionRange(ctx, eventID)
	switch {
	case errors.Is(err, events.ErrEventNotFound):
		return source.Expanded{}, err
	case errors.Is(err, events.ErrNotAgentSession):
		// Race: the source switched between the caller's lookup and now
		// (e.g. a re-ingest dropped + reinserted).
		return source.Expanded{}, source.ErrNotExpandable
	case err != nil:
		return source.Expanded{}, fmt.Errorf("event lookup: %w", err)
	}
	expanded, err := s.records(ctx, rng, scopes)
	if err != nil || !rng.Whole || len(expanded.Content) <= ask.ExpandContentBudget {
		return expanded, err
	}
	stretches, err := deps.Sink.SessionStretches(ctx, rng.SessionID, scopes)
	if err != nil {
		return source.Expanded{}, fmt.Errorf("session outline: %w", err)
	}
	if len(stretches) == 0 {
		// Nothing to outline until the session's stretches are published.
		return expanded, nil
	}
	return source.Expanded{Content: sessionOutline(rng.End, stretches, ask.ExpandContentBudget)}, nil
}

// records returns the session records in rng: from diderot's own store for a
// pushed session, and from the feed otherwise.
func (s *sessionFeedSource) records(ctx context.Context, rng events.SessionRange, scopes []string) (source.Expanded, error) {
	if rng.SessionID == "" || rng.Revision == "" {
		// Provider-shaped transcript reads were removed at the hard cutover.
		// Historical legacy rows may remain searchable until a native revision
		// replaces them, but they must never reopen the retired content path.
		return source.Expanded{}, source.ErrNotExpandable
	}
	if s.push != nil {
		body, err := s.push.ExpandRange(ctx, sessioningest.ExpandRequest{
			SessionID: rng.SessionID, Revision: rng.Revision, DecoderVersion: rng.DecoderVersion,
			Start: rng.Start, End: rng.End, AllowedScopes: scopes,
		})
		var superseded *sessioningest.SupersededError
		switch {
		case errors.As(err, &superseded):
			return source.Expanded{Content: supersededNote(superseded)}, nil
		case errors.Is(err, sessioningest.ErrPushNotFound):
			// Not a pushed session the caller may read; a feed session
			// falls through to the feed.
		case err != nil:
			return source.Expanded{}, fmt.Errorf("pushed session record range: %w", err)
		default:
			return source.Expanded{Content: body}, nil
		}
	}
	var feed *sessionfeed.Client
	if s.feed != nil {
		feed = s.feed()
	}
	if feed == nil {
		// Session feed connector off.
		return source.Expanded{}, source.ErrNotExpandable
	}
	body, err := feed.GetSessionRecordRange(ctx, rng.SessionID, rng.Revision, rng.Start, rng.End)
	if err != nil {
		// 404 from the feed = session deleted between our DB write and now.
		if sessionfeed.IsNotFound(err) {
			return source.Expanded{}, events.ErrEventNotFound
		}
		return source.Expanded{}, fmt.Errorf("session feed record range: %w", err)
	}
	return source.Expanded{Content: body}, nil
}

// Status is when the feed was last read and how many of the sessions seen
// are waiting, indexed, excluded or deleted, and what keeps summaries from
// running.
func (s *sessionFeedSource) Status(ctx context.Context, deps source.Deps) (settings.Status, error) {
	var waiting string
	if s.summaries != nil {
		state, err := s.summaries.State(ctx)
		if err != nil {
			return settings.Status{}, err
		}
		if !state.EffectiveEnabled {
			waiting = "Sessions aren't summarized, so they aren't searchable. " + state.Reason
		}
	}
	var updated *time.Time
	// A new database's cursor row has no position and no policy yet: the
	// feed has not been read.
	err := deps.Pool.QueryRow(ctx, `SELECT updated_at FROM session_ingest_cursor
		WHERE id AND (change_seq <> 0 OR policy_hash <> '')`).Scan(&updated)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return settings.Status{}, fmt.Errorf("session ingest cursor: %w", err)
	}
	// A parked session is not waiting: nothing tries it again until it
	// changes or an administrator retries it, so it has a row of its own.
	var candidates, pending, excluded, deleted, tombstones, published, overdue, parked int64
	err = deps.Pool.QueryRow(ctx, `SELECT
		count(*),
		count(*) FILTER (WHERE `+sessioningest.CandidatePendingSQL+` AND NOT `+sessioningest.CandidateParkedSQL+`),
		count(*) FILTER (WHERE excluded_at IS NOT NULL), count(*) FILTER (WHERE deleted), count(*) FILTER (WHERE corpus_tombstone_pending IS NOT NULL),
		(SELECT count(*) FROM session_ingest_published WHERE superseded_at IS NULL AND tombstoned_at IS NULL),
		count(*) FILTER (WHERE first_pending_at IS NOT NULL AND first_pending_at < now() - interval '24 hours'
		                   AND revision <> published_revision AND NOT `+sessioningest.CandidateParkedSQL+`),
		count(*) FILTER (WHERE `+sessioningest.CandidatePendingSQL+` AND `+sessioningest.CandidateParkedSQL+`)
		FROM session_ingest_candidates`).Scan(&candidates, &pending, &excluded, &deleted, &tombstones, &published, &overdue, &parked)
	if err != nil {
		return settings.Status{}, fmt.Errorf("session ingest counts: %w", err)
	}
	return settings.Status{
		Waiting: waiting,
		Rows: []settings.StatusRow{
			{Label: "Last read", Value: updated},
			{Label: "Sessions seen", Value: candidates},
			{Label: "Waiting for a summary", Value: pending},
			{Label: "Indexed", Value: published},
			{Label: "Excluded", Value: excluded},
			{Label: "Deleted", Value: deleted},
			{Label: "Removals pending", Value: tombstones},
			{Label: "Waiting over a day", Value: overdue},
			{Label: "Could not be summarized", Value: parked},
		},
		Note: "Which sessions are indexed is set by the eligibility policy file (DIDEROT_SESSION_INGEST_POLICY_PATH).",
	}, nil
}

// supersededNote tells the reader of a citation into a pruned revision what
// happened and where the session is now, instead of failing silently.
func supersededNote(e *sessioningest.SupersededError) string {
	current := e.Current
	if current == "" {
		current = "(none)"
	}
	return fmt.Sprintf("[superseded] This citation points at revision %s of agent session %s. "+
		"That revision was superseded and its records were pruned by the retention window. "+
		"The current revision is %s; search the session again to cite it.", e.Revision, e.SessionID, current)
}

// liveSessionFeed is the session feed connector on every replica, from the
// settings cache: the client that expands feed sessions and reads Ask usage
// accounting, and the viewer that links point at. While the connector is off
// there is neither; expansion of feed sessions reports not expandable,
// accounting is hidden, and links are omitted.
type liveSessionFeed struct {
	store *settings.Store

	mu      sync.Mutex
	version int64
	client  *sessionfeed.Client
}

// current returns the client and settings of the enabled connector, or nil.
// The client is rebuilt only when the settings change.
func (l *liveSessionFeed) current() (*sessionfeed.Client, sessionfeed.Settings) {
	resolved, enabled := l.store.Connector(sessionfeed.ConnectorName)
	if !enabled {
		return nil, sessionfeed.Settings{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client == nil || l.version != resolved.Version {
		l.client, l.version = sessionfeed.Connector{}.NewClient(resolved), resolved.Version
	}
	return l.client, sessionfeed.SettingsFrom(resolved)
}

// Client is the feed client, nil while the connector is off.
func (l *liveSessionFeed) Client() *sessionfeed.Client {
	client, _ := l.current()
	return client
}

// ViewerBaseURL is the session viewer's origin, empty while the connector is
// off or names none.
func (l *liveSessionFeed) ViewerBaseURL() string {
	_, s := l.current()
	return s.ViewerBaseURL
}

// GetWorkUsage reads the feed's accounting for a Diderot work reference; with
// the connector off there is none to show.
func (l *liveSessionFeed) GetWorkUsage(ctx context.Context, namespace, referenceID string) (*sessionfeed.Usage, error) {
	client := l.Client()
	if client == nil {
		return nil, nil
	}
	return client.GetWorkUsage(ctx, namespace, referenceID)
}
