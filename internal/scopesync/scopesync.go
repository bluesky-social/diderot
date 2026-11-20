// Package scopesync durably reconciles event visibility scopes and Qdrant
// point deletion with Postgres. Normal work is delta-only and bounded to one
// thousand points per batch, with paced serial scope catch-up per worker tick.
package scopesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/qdrantstore"
)

const (
	batchSize            = 1000
	scopeCatchUpBatches  = 20
	scopeCatchUpInterval = 250 * time.Millisecond
)

// Store is the Qdrant surface used by the delta workers and cutover audit.
type Store interface {
	SetScope(ctx context.Context, ids []uint64, scope string) error
	SetOccurredAt(ctx context.Context, ids []uint64, occurredAt time.Time) error
	DeleteIDs(ctx context.Context, ids []uint64) error
	ScrollScopes(ctx context.Context, pageSize uint32, visit func(ids []uint64, scopes []string) error) error
}

type Metrics struct {
	reconciledEvents prometheus.Counter
	queuedChunks     prometheus.Counter
	scopeApplied     prometheus.Counter
	scopeRetried     prometheus.Counter
	scopeCalls       prometheus.Counter
	scopeBatchSize   prometheus.Histogram
	scopePending     prometheus.Gauge
	scopeOldest      prometheus.Gauge
	scopeMissing     prometheus.Counter
	deleteApplied    prometheus.Counter
	deleteRetried    prometheus.Counter
	deletePending    prometheus.Gauge
	deleteOldest     prometheus.Gauge
	unstamped        prometheus.Gauge
	auditMismatches  prometheus.Counter
	auditMissing     prometheus.Counter
	auditOrphans     prometheus.Counter
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		reconciledEvents: prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_scope_reconciled_events_total", Help: "Events whose authoritative scope changed during reconciliation."}),
		queuedChunks:     prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_scope_chunks_queued_total", Help: "Chunks durably queued for a Qdrant scope payload repair."}),
		scopeApplied:     prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_scope_chunks_applied_total", Help: "Queued scope payload repairs acknowledged in Postgres."}),
		scopeRetried:     prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_scope_chunks_retried_total", Help: "Queued scope payload repairs left pending after a failed Qdrant call."}),
		scopeCalls:       prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_scope_worker_calls_total", Help: "Qdrant SetPayload calls made by the delta scope worker."}),
		scopeBatchSize:   prometheus.NewHistogram(prometheus.HistogramOpts{Name: "diderot_scope_worker_batch_size", Help: "Point count in each Qdrant scope payload call.", Buckets: prometheus.ExponentialBuckets(1, 2, 11)}),
		scopePending:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "diderot_scope_sync_pending_chunks", Help: "Embedded chunks waiting for a Qdrant scope payload repair."}),
		scopeOldest:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "diderot_scope_sync_oldest_eligible_seconds", Help: "Age in seconds of the oldest scope repair currently eligible to run."}),
		scopeMissing:     prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_scope_sync_not_found_total", Help: "Repeated scope failures while embedded rows still exist in Postgres."}),
		deleteApplied:    prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_qdrant_delete_points_applied_total", Help: "Durably queued Qdrant point deletes completed."}),
		deleteRetried:    prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_qdrant_delete_points_retried_total", Help: "Durably queued Qdrant point deletes retained after a failed call."}),
		deletePending:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "diderot_qdrant_delete_queue_depth", Help: "Qdrant point deletes waiting in Postgres."}),
		deleteOldest:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "diderot_qdrant_delete_queue_oldest_seconds", Help: "Age in seconds of the oldest queued Qdrant point delete."}),
		unstamped:        prometheus.NewGauge(prometheus.GaugeOpts{Name: "diderot_scope_unstamped_events", Help: "Non-deleted events carrying the deny-by-default unstamped scope."}),
		auditMismatches:  prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_qdrant_audit_scope_mismatches_total", Help: "Scope mismatches found by cutover audits."}),
		auditMissing:     prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_qdrant_audit_missing_points_total", Help: "Embedded Postgres chunks missing from Qdrant in cutover audits."}),
		auditOrphans:     prometheus.NewCounter(prometheus.CounterOpts{Name: "diderot_qdrant_audit_orphan_points_total", Help: "Qdrant points without Postgres chunks found by cutover audits."}),
	}
	if reg != nil {
		for _, counter := range []*prometheus.Counter{
			&m.reconciledEvents, &m.queuedChunks, &m.scopeApplied, &m.scopeRetried,
			&m.scopeCalls, &m.scopeMissing, &m.deleteApplied, &m.deleteRetried,
			&m.auditMismatches, &m.auditMissing, &m.auditOrphans,
		} {
			registerOrReuse(reg, counter)
		}
		for _, gauge := range []*prometheus.Gauge{
			&m.scopePending, &m.scopeOldest, &m.deletePending, &m.deleteOldest, &m.unstamped,
		} {
			registerOrReuse(reg, gauge)
		}
		registerOrReuse(reg, &m.scopeBatchSize)
	}
	return m
}

// registerOrReuse registers *c, or points it at the collector already
// registered under the same name: a leader that loses and regains
// leadership rebuilds its workers, and with them these metrics, in the
// same process.
func registerOrReuse[T prometheus.Collector](reg prometheus.Registerer, c *T) {
	if err := reg.Register(*c); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			panic(err)
		}
		*c = already.ExistingCollector.(T)
	}
}

type Service struct {
	Pool            *pgxpool.Pool
	Store           Store
	Logger          *slog.Logger
	Metrics         *Metrics
	RefreshChannels func(context.Context) (int, error)
}

type ReconcileResult struct {
	Events  int64
	Chunks  int64
	Pending int64
}

// Reconcile refreshes Slack privacy metadata, changes authoritative event
// scopes, and marks only the affected chunks in the same SQL statement.
func (s *Service) Reconcile(ctx context.Context) (ReconcileResult, error) {
	s.defaults()
	if s.RefreshChannels != nil {
		if _, err := s.RefreshChannels(ctx); err != nil {
			s.Logger.Warn("scopesync: channel privacy refresh failed; using cached channel info", "err", err)
		}
	}
	var out ReconcileResult
	err := s.Pool.QueryRow(ctx, `
		WITH changed AS (
			UPDATE events e
			   SET scope = 'slack-channel:' || d.channel_id
			  FROM slack_message_details d
			  JOIN slack_channel_info ci ON ci.channel_id = d.channel_id
			 WHERE e.id = d.event_id
			   AND e.source = 'slack'
			   AND d.channel_id ~ '^[CG][A-Z0-9]+$'
			   AND e.scope NOT LIKE 'owner:%'
			   AND COALESCE(ci.name, '') <> ''
			   AND e.scope IS DISTINCT FROM 'slack-channel:' || d.channel_id
			 RETURNING e.id
		), queued AS (
			UPDATE event_chunks c
			   SET scope_sync_needed = true,
			       scope_sync_attempts = 0,
			       scope_sync_next_attempt_at = now(),
			       scope_sync_last_error = NULL
			  FROM changed
			 WHERE c.event_id = changed.id
			 RETURNING c.id
		)
		SELECT (SELECT count(*) FROM changed),
		       (SELECT count(*) FROM queued)
	`).Scan(&out.Events, &out.Chunks)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("scopesync: reconcile: %w", err)
	}
	if s.Metrics != nil {
		s.Metrics.reconciledEvents.Add(float64(out.Events))
		s.Metrics.queuedChunks.Add(float64(out.Chunks))
	}
	out.Pending = s.pendingScope(ctx)
	return out, nil
}

type scopeWork struct {
	id         int64
	scope      string
	occurredAt time.Time
}

// DrainScope attempts at most one thousand pending chunks. Any Qdrant call
// failure retains the entire selected pass and stops until the next tick.
func (s *Service) DrainScope(ctx context.Context) (int, error) {
	applied, _, err := s.drainScope(ctx)
	return applied, err
}

func (s *Service) DrainScopeCatchUp(ctx context.Context) (int, error) {
	var total int
	for batch := range scopeCatchUpBatches {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		applied, selected, err := s.drainScope(ctx)
		total += applied
		if err != nil || selected < batchSize || batch == scopeCatchUpBatches-1 {
			return total, err
		}
		timer := time.NewTimer(scopeCatchUpInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return total, ctx.Err()
		case <-timer.C:
		}
	}
	return total, nil
}

func (s *Service) drainScope(ctx context.Context) (int, int, error) {
	s.defaults()
	rows, err := s.Pool.Query(ctx, `
		SELECT c.id, e.scope, c.occurred_at
		  FROM event_chunks c
		  JOIN events e ON e.id = c.event_id
		 WHERE c.scope_sync_needed
		   AND c.embedded_at IS NOT NULL
		   AND c.scope_sync_next_attempt_at <= now()
		   AND NOT e.deleted
		 ORDER BY c.scope_sync_next_attempt_at, c.id
		 LIMIT $1
	`, batchSize)
	if err != nil {
		return 0, 0, fmt.Errorf("scopesync: select scope work: %w", err)
	}
	var work []scopeWork
	for rows.Next() {
		var item scopeWork
		if err := rows.Scan(&item.id, &item.scope, &item.occurredAt); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scopesync: scan scope work: %w", err)
		}
		work = append(work, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, fmt.Errorf("scopesync: read scope work: %w", err)
	}
	if len(work) == 0 {
		return 0, 0, nil
	}

	byScope := make(map[string][]uint64)
	ids := make([]int64, 0, len(work))
	for _, item := range work {
		byScope[item.scope] = append(byScope[item.scope], uint64(item.id))
		ids = append(ids, item.id)
	}
	scopes := make([]string, 0, len(byScope))
	for scope := range byScope {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	for _, scope := range scopes {
		pointIDs := byScope[scope]
		if s.Metrics != nil {
			s.Metrics.scopeCalls.Inc()
			s.Metrics.scopeBatchSize.Observe(float64(len(pointIDs)))
		}
		if err := s.Store.SetScope(ctx, pointIDs, scope); err != nil {
			persistErr := s.failScope(ctx, ids, err)
			if s.Metrics != nil {
				s.Metrics.scopeRetried.Add(float64(len(work)))
			}
			s.Logger.Error("scopesync: scope batch failed; whole pass remains pending", "points", len(work), "scope", scope, "err", err)
			return 0, len(work), errors.Join(fmt.Errorf("scopesync: set scope: %w", err), persistErr)
		}
	}
	byTime := make(map[time.Time][]uint64)
	for _, item := range work {
		occurredAt := item.occurredAt.UTC()
		byTime[occurredAt] = append(byTime[occurredAt], uint64(item.id))
	}
	times := make([]time.Time, 0, len(byTime))
	for occurredAt := range byTime {
		times = append(times, occurredAt)
	}
	sort.Slice(times, func(first, second int) bool { return times[first].Before(times[second]) })
	for _, occurredAt := range times {
		pointIDs := byTime[occurredAt]
		if s.Metrics != nil {
			s.Metrics.scopeCalls.Inc()
			s.Metrics.scopeBatchSize.Observe(float64(len(pointIDs)))
		}
		if err := s.Store.SetOccurredAt(ctx, pointIDs, occurredAt); err != nil {
			persistErr := s.failScope(ctx, ids, err)
			if s.Metrics != nil {
				s.Metrics.scopeRetried.Add(float64(len(work)))
			}
			return 0, len(work), errors.Join(fmt.Errorf("scopesync: set recency: %w", err), persistErr)
		}
	}

	writtenScopes := make([]string, len(work))
	writtenTimes := make([]time.Time, len(work))
	for i := range work {
		writtenScopes[i] = work[i].scope
		writtenTimes[i] = work[i].occurredAt
	}
	var applied int
	err = s.Pool.QueryRow(ctx, `
		WITH sent AS (
			SELECT * FROM unnest($1::bigint[], $2::text[], $3::timestamptz[]) AS x(id, scope, occurred_at)
		), cleared AS (
			UPDATE event_chunks c
			   SET scope_sync_needed = false,
			       scope_sync_attempts = 0,
			       scope_sync_last_error = NULL
			  FROM sent, events e
			 WHERE c.id = sent.id
			   AND e.id = c.event_id
			   AND e.scope = sent.scope
			   AND c.occurred_at = sent.occurred_at
			 RETURNING c.id
		)
		SELECT count(*) FROM cleared
	`, ids, writtenScopes, writtenTimes).Scan(&applied)
	if err != nil {
		return 0, len(work), fmt.Errorf("scopesync: acknowledge scope batch: %w", err)
	}
	if s.Metrics != nil {
		s.Metrics.scopeApplied.Add(float64(applied))
	}
	return applied, len(work), nil
}

func (s *Service) failScope(ctx context.Context, ids []int64, cause error) error {
	msg := truncate(cause.Error(), 2000)
	var persistent int64
	err := s.Pool.QueryRow(ctx, `
		WITH failed AS (
			UPDATE event_chunks
			   SET scope_sync_attempts = scope_sync_attempts + 1,
			       scope_sync_next_attempt_at = now() + LEAST(
			           interval '15 minutes',
			           interval '1 minute' * (1 << LEAST(scope_sync_attempts, 4))),
			       scope_sync_last_error = $2
			 WHERE id = ANY($1::bigint[])
			   AND scope_sync_needed
			 RETURNING scope_sync_attempts, embedded_at
		)
		SELECT count(*) FILTER (WHERE scope_sync_attempts > 1 AND embedded_at IS NOT NULL)
		  FROM failed
	`, ids, msg).Scan(&persistent)
	if err != nil {
		return fmt.Errorf("scopesync: persist scope failure: %w", err)
	}
	if persistent > 0 && qdrantstore.IsNotFound(cause) && s.Metrics != nil {
		s.Metrics.scopeMissing.Add(float64(persistent))
	}
	if persistent > 0 && qdrantstore.IsNotFound(cause) {
		s.Logger.Error("scopesync: persistent Qdrant/Postgres inconsistency", "surviving_embedded_rows", persistent, "err", cause)
	}
	return nil
}

// DrainDeletes attempts at most one thousand durable point deletions.
func (s *Service) DrainDeletes(ctx context.Context) (int, error) {
	s.defaults()
	rows, err := s.Pool.Query(ctx, `
		SELECT point_id
		  FROM qdrant_delete_queue
		 WHERE next_attempt_at <= now()
		 ORDER BY next_attempt_at, enqueued_at
		 LIMIT $1
	`, batchSize)
	if err != nil {
		return 0, fmt.Errorf("scopesync: select deletes: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scopesync: scan delete: %w", err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("scopesync: read deletes: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	pointIDs := make([]uint64, len(ids))
	for i, id := range ids {
		pointIDs[i] = uint64(id)
	}
	if err := s.Store.DeleteIDs(ctx, pointIDs); err != nil {
		msg := truncate(err.Error(), 2000)
		_, persistErr := s.Pool.Exec(ctx, `
			UPDATE qdrant_delete_queue
			   SET attempts = attempts + 1,
			       next_attempt_at = now() + LEAST(
			           interval '15 minutes',
			           interval '1 minute' * (1 << LEAST(attempts, 4))),
			       last_error = $2
			 WHERE point_id = ANY($1::bigint[])
		`, ids, msg)
		if s.Metrics != nil {
			s.Metrics.deleteRetried.Add(float64(len(ids)))
		}
		return 0, errors.Join(fmt.Errorf("scopesync: delete points: %w", err), persistErr)
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM qdrant_delete_queue WHERE point_id = ANY($1::bigint[])`, ids)
	if err != nil {
		return 0, fmt.Errorf("scopesync: acknowledge deletes: %w", err)
	}
	applied := int(tag.RowsAffected())
	if s.Metrics != nil {
		s.Metrics.deleteApplied.Add(float64(applied))
	}
	return applied, nil
}

type AuditResult struct {
	ScopeMismatches int64
	OrphanPoints    int64
	MissingPoints   int64
	ScopePending    int64
	DeletePending   int64
}

// Audit performs the one-time cutover comparison. It reads Qdrant but mutates
// only Postgres repair state.
func (s *Service) Audit(ctx context.Context) (AuditResult, error) {
	s.defaults()
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return AuditResult{}, fmt.Errorf("scopesync: audit acquire connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS qdrant_audit_seen`); err != nil {
		return AuditResult{}, err
	}
	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE qdrant_audit_seen (point_id bigint PRIMARY KEY)`); err != nil {
		return AuditResult{}, fmt.Errorf("scopesync: create audit table: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `DROP TABLE IF EXISTS qdrant_audit_seen`) }()
	var auditStartedAt time.Time
	if err := conn.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&auditStartedAt); err != nil {
		return AuditResult{}, fmt.Errorf("scopesync: capture audit start: %w", err)
	}

	var out AuditResult
	err = s.Store.ScrollScopes(ctx, batchSize, func(pointIDs []uint64, scopes []string) error {
		ids := make([]int64, len(pointIDs))
		for i, pointID := range pointIDs {
			if pointID > math.MaxInt64 {
				return fmt.Errorf("qdrant point id %d exceeds Postgres bigint", pointID)
			}
			ids[i] = int64(pointID)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO qdrant_audit_seen SELECT unnest($1::bigint[]) ON CONFLICT DO NOTHING`, ids); err != nil {
			return fmt.Errorf("record audit page: %w", err)
		}
		var mismatches, orphans int64
		if err := conn.QueryRow(ctx, `
			WITH seen AS (
				SELECT * FROM unnest($1::bigint[], $2::text[]) AS x(id, scope)
			), mismatched AS (
				SELECT c.id
				  FROM seen
				  JOIN event_chunks c ON c.id = seen.id
				  JOIN events e ON e.id = c.event_id
				 WHERE e.scope IS DISTINCT FROM seen.scope
			), marked AS (
				UPDATE event_chunks c
				   SET scope_sync_needed = true,
				       scope_sync_attempts = 0,
				       scope_sync_next_attempt_at = now(),
				       scope_sync_last_error = NULL
				  FROM mismatched
				 WHERE c.id = mismatched.id
				 RETURNING c.id
			), orphaned AS (
				SELECT seen.id FROM seen
				 WHERE NOT EXISTS (SELECT 1 FROM event_chunks c WHERE c.id = seen.id)
			), queued AS (
				INSERT INTO qdrant_delete_queue (point_id)
				SELECT id FROM orphaned
				ON CONFLICT (point_id) DO NOTHING
				RETURNING point_id
			)
			SELECT (SELECT count(*) FROM marked),
			       (SELECT count(*) FROM orphaned)
		`, ids, scopes).Scan(&mismatches, &orphans); err != nil {
			return fmt.Errorf("compare audit page: %w", err)
		}
		out.ScopeMismatches += mismatches
		out.OrphanPoints += orphans
		return nil
	})
	if err != nil {
		return AuditResult{}, fmt.Errorf("scopesync: audit scroll: %w", err)
	}
	tag, err := conn.Exec(ctx, `
		UPDATE event_chunks c
		   SET embedded_at = NULL,
		       embedder_id = NULL,
		       queued_at = now(),
		       scope_sync_needed = false,
		       scope_sync_attempts = 0,
		       scope_sync_last_error = NULL
		 WHERE c.embedded_at IS NOT NULL
		   AND c.embedded_at < $1
		   AND NOT EXISTS (SELECT 1 FROM qdrant_audit_seen s WHERE s.point_id = c.id)
	`, auditStartedAt)
	if err != nil {
		return AuditResult{}, fmt.Errorf("scopesync: queue missing points for embedding: %w", err)
	}
	out.MissingPoints = tag.RowsAffected()
	if err := conn.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM event_chunks WHERE scope_sync_needed AND embedded_at IS NOT NULL),
		       (SELECT count(*) FROM qdrant_delete_queue)
	`).Scan(&out.ScopePending, &out.DeletePending); err != nil {
		return AuditResult{}, fmt.Errorf("scopesync: count audit backlogs: %w", err)
	}
	if s.Metrics != nil {
		s.Metrics.auditMismatches.Add(float64(out.ScopeMismatches))
		s.Metrics.auditOrphans.Add(float64(out.OrphanPoints))
		s.Metrics.auditMissing.Add(float64(out.MissingPoints))
	}
	return out, nil
}

func (s *Service) defaults() {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
}

func (s *Service) RefreshGauges(ctx context.Context) error {
	if s.Metrics == nil {
		return nil
	}
	var scopePending, deletePending int64
	var scopeOldest, deleteOldest float64
	err := s.Pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM event_chunks WHERE scope_sync_needed AND embedded_at IS NOT NULL),
		  COALESCE((SELECT extract(epoch FROM now() - min(scope_sync_next_attempt_at))
		              FROM event_chunks
		             WHERE scope_sync_needed AND embedded_at IS NOT NULL
		               AND scope_sync_next_attempt_at <= now()), 0),
		  (SELECT count(*) FROM qdrant_delete_queue),
		  COALESCE((SELECT extract(epoch FROM now() - min(enqueued_at))
		              FROM qdrant_delete_queue), 0)
	`).Scan(&scopePending, &scopeOldest, &deletePending, &deleteOldest)
	if err != nil {
		return err
	}
	s.Metrics.scopePending.Set(float64(scopePending))
	s.Metrics.scopeOldest.Set(math.Max(0, scopeOldest))
	s.Metrics.deletePending.Set(float64(deletePending))
	s.Metrics.deleteOldest.Set(math.Max(0, deleteOldest))
	var unstamped int64
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE scope = 'unstamped' AND NOT deleted`).Scan(&unstamped); err != nil {
		return err
	}
	s.Metrics.unstamped.Set(float64(unstamped))
	return nil
}

func (s *Service) pendingScope(ctx context.Context) int64 {
	var n int64
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks WHERE scope_sync_needed AND embedded_at IS NOT NULL`).Scan(&n)
	return n
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
