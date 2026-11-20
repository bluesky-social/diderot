package adminapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/retrieve"
)

// logRetrieveCall records one bare /v1/retrieve or /v1/list
// invocation to retrieve_log. These are real user queries with no
// agentic reformulation in front of them: the down payment on
// per-user audit logging.
//
// Best-effort: the user already has their results; a logging
// failure warns and drops the row rather than failing the request.
// Skipped when the status pool isn't wired (tests, partial config).
func (s *Server) logRetrieveCall(ctx context.Context, endpoint, question string, filters any, res *retrieve.Result, latency time.Duration) {
	pool := s.getStatusPool()
	if pool == nil {
		return
	}
	op := operatorFromContext(ctx)
	if op == nil {
		// Unreachable today (both routes sit behind s.authed), but a
		// future auth-path change shouldn't panic here.
		op = &auth.Principal{Subject: "unknown"}
	}

	type loggedHit struct {
		EventID int64   `json:"event_id"`
		Score   float32 `json:"score"`
	}
	hits := make([]loggedHit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, loggedHit{EventID: h.EventID, Score: h.Score})
	}
	results, err := json.Marshal(hits)
	if err != nil {
		s.logger.Warn("retrieve_log: marshal results", "err", err)
		return
	}
	filtersJSON, err := json.Marshal(filters)
	if err != nil {
		s.logger.Warn("retrieve_log: marshal filters", "err", err)
		return
	}

	var q *string
	if question != "" {
		q = &question
	}
	var email *string
	if op.Email != "" {
		email = &op.Email
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO retrieve_log
		   (endpoint, question, filters, results, latency_ms, caller_sub, caller_email)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		endpoint, q, filtersJSON, results,
		latency.Milliseconds(), op.Subject, email,
	); err != nil {
		s.logger.Warn("retrieve_log: insert", "endpoint", endpoint, "err", err)
	}
}
