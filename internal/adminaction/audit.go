// Package adminaction holds the state-changing administrator actions, each as
// one function that performs the action and records it in admin_audit_log.
// The browser UI (internal/adminui) and the /v1 API (internal/adminapi) parse
// their own input and render their own output, and call these for the action
// itself, so an action writes the same audit row whichever surface it came
// from. Connector, organization, models, and setup changes are recorded by
// internal/settings in the transaction that writes them.
package adminaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// ErrRecord means the audit record could not be started, so the action did
// not run.
var ErrRecord = errors.New("admin audit")

// maxErrorText bounds the error stored with a failed action.
const maxErrorText = 2000

// Actor is who performs an action.
type Actor struct {
	// Email is the caller's email, or, for an API caller whose token carries
	// no accepted email, its subject.
	Email string
	// RequestID is the request's X-Request-ID, if any.
	RequestID string
}

// Audit records the actions of one actor in admin_audit_log.
type Audit struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Actor  Actor
}

// actor is the actor as recorded: the email lowercased.
func (a Audit) actor() string {
	return strings.ToLower(strings.TrimSpace(a.Actor.Email))
}

// Run records action on target as started, runs act, and records whether it
// succeeded. It returns act's error, or an ErrRecord error, without running
// act, when the record can't be started. A failure to record the outcome is
// logged: the action has already happened.
func (a Audit) Run(ctx context.Context, action, target string, details any, act func() error) error {
	if a.Pool == nil {
		return fmt.Errorf("%w: no database", ErrRecord)
	}
	actor := a.actor()
	if actor == "" {
		return fmt.Errorf("%w: operator identity required", ErrRecord)
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("%w: encode details: %v", ErrRecord, err)
	}
	var id int64
	err = a.Pool.QueryRow(ctx, `
		INSERT INTO admin_audit_log
		       (actor_email, action, target, request_id, details)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		RETURNING id`, actor, action, target, strings.TrimSpace(a.Actor.RequestID), encoded).Scan(&id)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRecord, err)
	}
	actErr := act()
	status := "succeeded"
	var message any
	if actErr != nil {
		status = "failed"
		text := events.SanitizeText(actErr.Error())
		if len(text) > maxErrorText {
			end := maxErrorText
			for !utf8.RuneStart(text[end]) {
				end--
			}
			text = text[:end]
		}
		message = text
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if _, err := a.Pool.Exec(finishCtx, `
		UPDATE admin_audit_log
		   SET status = $2, error = $3, finished_at = now()
		 WHERE id = $1`, id, status, message); err != nil {
		logger := a.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("admin audit completion failed", "audit_id", id, "action", action, "err", err)
	}
	return actErr
}
