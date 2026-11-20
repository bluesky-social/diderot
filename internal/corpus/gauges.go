// Package corpus runs a periodic gauge updater for "how big is the
// corpus right now" metrics — total events (by source), total chunks
// (by source), and total ask sessions. These are fundamentally
// DB-state, not flow-counters, so a poll loop is the right shape:
// the alternative (incrementing/decrementing on every write) is
// fragile against any out-of-band insert (backfill scripts,
// migrations, manual fixups) and the cardinality is small enough
// that a 60s SELECT count is cheap.
//
// Pattern mirrors internal/embedder/queue_metrics.go — same poll
// cadence, same nil-safe wiring.
package corpus

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// PollInterval is how often we re-run the count queries. 60s is a
// balance between "operator dashboards feel live" and "Postgres
// doesn't see a thundering herd of count(*) every second." The
// gauges read by Grafana are scrape-time snapshots regardless of
// our poll cadence.
const PollInterval = 60 * time.Second

// Metrics is the gauge bundle. All gauges are vector-shaped so we
// can add labels (today: source) without a wire-shape change. nil
// reg returns a zero-value Metrics whose Update is a no-op.
type Metrics struct {
	events      *prometheus.GaugeVec // labels: source
	chunks      *prometheus.GaugeVec // labels: source
	askSessions prometheus.Gauge     // total
	askMessages prometheus.Gauge     // total
}

// NewMetrics constructs and registers the gauge bundle. Idempotent
// against AlreadyRegisteredError so tests and hot-reload paths
// don't panic.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return &Metrics{}
	}
	return &Metrics{
		events: regGV(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "diderot_corpus_events",
				Help: "Total events in the corpus, partitioned by source.",
			}, []string{"source"})),
		chunks: regGV(reg, prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "diderot_corpus_chunks",
				Help: "Total event_chunks in the corpus, partitioned by source.",
			}, []string{"source"})),
		askSessions: regG(reg, prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "diderot_corpus_ask_sessions",
				Help: "Total ask_sessions rows.",
			})),
		askMessages: regG(reg, prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "diderot_corpus_ask_messages",
				Help: "Total ask_messages rows across all sessions.",
			})),
	}
}

// Run polls every PollInterval and updates the gauges. Returns when
// ctx is cancelled. Errors during a poll are logged and the next
// tick still fires — a transient DB hiccup shouldn't take the gauge
// updater down.
//
// Pool is the shared diderot pgxpool. We use it directly rather than
// going through DBSink/Manager so we don't have to plumb method
// surfaces; the queries are read-only and trivially safe to share.
//
// If m was constructed via NewMetrics(nil) (test path with no
// Registerer), the gauge fields are all nil; Run early-returns so
// update() doesn't NPE on Set/WithLabelValues. Production always
// passes a real Registerer so this never trips.
func Run(ctx context.Context, pool *pgxpool.Pool, m *Metrics, logger *slog.Logger) {
	if m == nil || pool == nil || m.events == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	// Initial scan immediately on start so the gauges aren't all
	// zeros until the first tick fires (a 60s blank window on
	// boot looks like data loss to an unfamiliar dashboard
	// viewer).
	m.update(ctx, pool, logger)
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.update(ctx, pool, logger)
		}
	}
}

// update runs all the count queries in a single goroutine. They're
// all sub-second on a corpus of tens of thousands of events; no need
// to parallelize or pipeline.
func (m *Metrics) update(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	// Per-source event counts. group by source so a new source
	// landing automatically gets its own gauge series.
	rows, err := pool.Query(ctx,
		`SELECT source, count(*) FROM events WHERE deleted = false GROUP BY source`)
	if err != nil {
		logger.Warn("corpus: events count failed", "err", err)
	} else {
		seen := map[string]bool{}
		for rows.Next() {
			var source string
			var n int64
			if err := rows.Scan(&source, &n); err != nil {
				logger.Warn("corpus: events scan failed", "err", err)
				continue
			}
			m.events.WithLabelValues(source).Set(float64(n))
			seen[source] = true
		}
		_ = rows.Err()
		rows.Close()
		// We don't reset gauges for sources that vanished — sources
		// don't get deleted in practice (they're encoded in
		// migrations + indexes), and keeping the last-seen value
		// hides a transient query failure rather than amplifying it.
		_ = seen
	}

	// Per-source chunk counts. Joined so we can attribute chunks
	// to their source. event_chunks is at most as large as events
	// times the average chunks-per-event; today that's ~1:1 for
	// slack and ~1:1 for agent_session, so this is a small
	// table relative to most.
	chunkRows, err := pool.Query(ctx,
		`SELECT e.source, count(*)
		   FROM event_chunks c JOIN events e ON e.id = c.event_id
		  GROUP BY e.source`)
	if err != nil {
		logger.Warn("corpus: chunks count failed", "err", err)
	} else {
		for chunkRows.Next() {
			var source string
			var n int64
			if err := chunkRows.Scan(&source, &n); err != nil {
				logger.Warn("corpus: chunks scan failed", "err", err)
				continue
			}
			m.chunks.WithLabelValues(source).Set(float64(n))
		}
		_ = chunkRows.Err()
		chunkRows.Close()
	}

	// Ask sessions + messages. Two separate scalar gauges; small
	// table, fast count.
	var sessionsN, messagesN int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ask_sessions`).Scan(&sessionsN); err != nil {
		logger.Warn("corpus: ask_sessions count failed", "err", err)
	} else {
		m.askSessions.Set(float64(sessionsN))
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ask_messages`).Scan(&messagesN); err != nil {
		logger.Warn("corpus: ask_messages count failed", "err", err)
	} else {
		m.askMessages.Set(float64(messagesN))
	}
}

// regGV / regG match the embedder package's idempotent-register
// pattern so re-construction in tests doesn't panic.
func regGV(reg prometheus.Registerer, v *prometheus.GaugeVec) *prometheus.GaugeVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.GaugeVec)
		}
		panic(err)
	}
	return v
}

func regG(reg prometheus.Registerer, v prometheus.Gauge) prometheus.Gauge {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(prometheus.Gauge)
		}
		panic(err)
	}
	return v
}
