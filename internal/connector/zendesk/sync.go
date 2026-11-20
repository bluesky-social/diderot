package zendesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/source"
)

type Worker struct {
	Pool              *pgxpool.Pool
	Client            *Client
	Sink              *events.DBSink
	Logger            *slog.Logger
	SyncInterval      time.Duration
	TicketConcurrency int
	RecentLookback    time.Duration
	// IndexedGroups are the only Zendesk group IDs whose tickets are indexed.
	// A ticket in any other group, or in none, is tombstoned instead of stored,
	// and PurgeUnindexedGroups removes any indexed before it moved out. Empty
	// indexes nothing.
	IndexedGroups []int64
	// State is the source's state, where a requested backfill waits for Run
	// to start it.
	State source.State
	// Observer, when set, is told how each ticket, article and
	// reconciliation pass went, and of every other step that failed.
	Observer interface {
		Succeeded()
		Failed()
	}
}

// observe tells the Observer how a pass went, or with failedOnly only
// whether it failed. A pass cut short by shutdown says nothing.
func (worker *Worker) observe(err error, failedOnly bool) {
	switch {
	case worker.Observer == nil || errors.Is(err, context.Canceled):
	case err != nil:
		worker.Observer.Failed()
	case !failedOnly:
		worker.Observer.Succeeded()
	}
}

func (worker *Worker) indexes(groupID int64) bool {
	return slices.Contains(worker.IndexedGroups, groupID)
}

func (worker *Worker) Run(ctx context.Context) error {
	interval := worker.SyncInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	logger := worker.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// failed logs a step that failed. One cut short as the worker stops is
	// not a failure.
	failed := func(step string, err error) {
		if ctx.Err() != nil {
			logger.Debug(step+": stopping", "err", err)
			return
		}
		logger.Error(step, "err", err)
	}
	var nextTickets, nextRecent, nextArticles, nextReconcile, nextPurge time.Time
	var ticketsCaughtUp, articlesCaughtUp bool
	recentCaughtUp := worker.RecentLookback <= 0
	for ctx.Err() == nil {
		if started, err := worker.startBackfill(ctx); err != nil {
			worker.observe(err, true)
			failed("zendesk backfill did not start", err)
		} else if started {
			logger.Info("zendesk backfill started: tickets and articles are read from the start")
			nextTickets, nextRecent, nextArticles = time.Time{}, time.Time{}, time.Time{}
			ticketsCaughtUp, articlesCaughtUp, recentCaughtUp = false, false, worker.RecentLookback <= 0
		}
		if !time.Now().Before(nextPurge) {
			if err := worker.PurgeUnindexedGroups(ctx); err != nil {
				worker.observe(err, true)
				failed("zendesk unindexed-group purge failed", err)
			}
			nextPurge = time.Now().Add(interval)
		}
		if worker.RecentLookback > 0 && !time.Now().Before(nextRecent) {
			caughtUp, err := worker.SyncRecentTickets(ctx)
			worker.observe(err, false)
			if err != nil {
				failed("zendesk recent ticket sync failed", err)
			} else if caughtUp && !recentCaughtUp {
				logger.Info("zendesk recent ticket sync caught up")
			}
			recentCaughtUp = err == nil && caughtUp
			nextRecent = time.Now().Add(syncDelay(caughtUp, err, interval, 0))
		} else if !time.Now().Before(nextTickets) {
			caughtUp, err := worker.SyncTickets(ctx)
			worker.observe(err, false)
			if err != nil {
				failed("zendesk ticket sync failed", err)
			}
			ticketsCaughtUp = err == nil && caughtUp
			nextTickets = time.Now().Add(syncDelay(caughtUp, err, interval, 0))
		}
		if !time.Now().Before(nextArticles) {
			caughtUp, err := worker.SyncArticles(ctx)
			worker.observe(err, false)
			if err != nil {
				failed("zendesk article sync failed", err)
			}
			articlesCaughtUp = err == nil && caughtUp
			nextArticles = time.Now().Add(syncDelay(caughtUp, err, interval, 7*time.Second))
		}
		reconcileReady := ticketsCaughtUp && recentCaughtUp && articlesCaughtUp
		if reconcileReady && !time.Now().Before(nextReconcile) {
			caughtUp, err := worker.Reconcile(ctx)
			worker.observe(err, false)
			if err != nil {
				failed("zendesk reconciliation failed", err)
			}
			nextReconcile = time.Now().Add(syncDelay(caughtUp, err, interval, 7*time.Second))
		}
		if ctx.Err() == nil {
			if err := worker.RetryDeferred(ctx); err != nil {
				worker.observe(err, true)
				failed("zendesk deferred sync failed", err)
			}
		}
		nextWake := nextTickets
		nextPolls := []time.Time{nextArticles}
		if worker.RecentLookback > 0 {
			nextPolls = append(nextPolls, nextRecent)
		}
		if reconcileReady {
			nextPolls = append(nextPolls, nextReconcile)
		}
		nextPolls = append(nextPolls, nextPurge)
		for _, next := range nextPolls {
			if next.Before(nextWake) {
				nextWake = next
			}
		}
		if err := sleep(ctx, time.Until(nextWake)); err != nil {
			return nil
		}
	}
	return nil
}

func syncDelay(caughtUp bool, err error, interval, backlogDelay time.Duration) time.Duration {
	if err != nil {
		var failure *APIError
		if errors.As(err, &failure) {
			return max(interval, failure.RetryAfter)
		}
		return interval
	}
	if caughtUp {
		return interval
	}
	return backlogDelay
}

func (worker *Worker) SyncTickets(ctx context.Context) (bool, error) {
	return worker.syncTickets(ctx, "tickets")
}

func (worker *Worker) SyncRecentTickets(ctx context.Context) (bool, error) {
	if worker.RecentLookback <= 0 {
		return true, nil
	}
	_, err := worker.Pool.Exec(ctx, `INSERT INTO zendesk_sync (tenant, stream, watermark)
		VALUES ($1, 'tickets-recent', $2) ON CONFLICT (tenant, stream) DO NOTHING`, worker.Client.origin, time.Now().Add(-worker.RecentLookback).Unix())
	if err != nil {
		return false, fmt.Errorf("zendesk: initialize recent ticket checkpoint: %w", err)
	}
	return worker.syncTickets(ctx, "tickets-recent")
}

func (worker *Worker) syncTickets(ctx context.Context, stream string) (bool, error) {
	var cursor string
	var watermark int64
	err := worker.Pool.QueryRow(ctx, `SELECT cursor, watermark FROM zendesk_sync WHERE tenant = $1 AND stream = $2`, worker.Client.origin, stream).Scan(&cursor, &watermark)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("zendesk: load cursor: %w", err)
	}
	page, err := worker.Client.ticketsSince(ctx, cursor, watermark)
	if err != nil {
		return false, err
	}
	users := make(map[int64]User)
	for _, user := range page.Users {
		users[user.ID] = user
	}
	tickets := make(map[int64]Ticket, len(page.Tickets))
	for _, ticket := range page.Tickets {
		if ticket.ID <= 0 {
			return false, errors.New("zendesk: ticket has no valid ID")
		}
		if previous, exists := tickets[ticket.ID]; !exists || !previous.UpdatedAt.After(ticket.UpdatedAt) {
			tickets[ticket.ID] = ticket
		}
	}
	// An export page is a snapshot. Import each ticket's current state instead,
	// or a ticket that just moved out of an indexed group (or a lagging
	// historical page) would be indexed again, or older metadata restored.
	live := make([]int64, 0, len(tickets))
	for id, ticket := range tickets {
		if ticket.Status != "deleted" {
			live = append(live, id)
		}
	}
	current, err := worker.Client.currentTickets(ctx, live)
	if err != nil {
		return false, err
	}
	concurrency := worker.TicketConcurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(concurrency)
	for _, ticket := range tickets {
		if groupCtx.Err() != nil {
			break
		}
		group.Go(func() error {
			if ticket.Status == "deleted" {
				return worker.deleteTicket(groupCtx, ticket.ID)
			}
			now, readable := current[ticket.ID]
			if !readable {
				// Deleted or no longer readable: drop the indexed copy and let the
				// retry path settle which (refreshTicket deletes on 404, keeps
				// retrying on 403).
				if err := worker.tombstoneTicket(groupCtx, ticket.ID); err != nil {
					return err
				}
				return worker.deferRecord(groupCtx, "ticket", ticket.ID)
			}
			if now.Status == "deleted" {
				return worker.deleteTicket(groupCtx, ticket.ID)
			}
			if err := worker.importTicket(groupCtx, now, maps.Clone(users)); err != nil {
				if !deferTicketError(err) {
					return err
				}
				return worker.deferRecord(groupCtx, "ticket", ticket.ID)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return false, err
	}
	_, err = worker.Pool.Exec(ctx, `
		INSERT INTO zendesk_sync (tenant, stream, cursor) VALUES ($1, $2, $3)
		ON CONFLICT (tenant, stream) DO UPDATE SET cursor = EXCLUDED.cursor, updated_at = now()
	`, worker.Client.origin, stream, page.AfterCursor)
	if err != nil {
		return false, fmt.Errorf("zendesk: save cursor: %w", err)
	}
	return *page.EndOfStream, nil
}

func (worker *Worker) importTicket(ctx context.Context, ticket Ticket, users map[int64]User) error {
	if !worker.indexes(ticket.GroupID) {
		return worker.deleteTicket(ctx, ticket.ID)
	}
	comments, authors, err := worker.Client.Comments(ctx, ticket.ID)
	if err != nil {
		return err
	}
	for _, user := range authors {
		users[user.ID] = user
	}
	if err := worker.storeTicket(ctx, ticket, comments, users); err != nil {
		return err
	}
	return worker.clearRecordRetry(ctx, "ticket", ticket.ID)
}

func deferTicketError(err error) bool {
	var failure *APIError
	return errors.Is(err, errProtocol) || (errors.As(err, &failure) && (failure.Status == 403 || failure.Status == 404 || failure.Status == 410))
}

func ticketKey(ticketID int64) string {
	return fmt.Sprintf("ticket:%d", ticketID)
}

func (worker *Worker) storeTicket(ctx context.Context, ticket Ticket, comments []Comment, users map[int64]User) error {
	if ticket.CreatedAt.IsZero() {
		return fmt.Errorf("%w: ticket has no creation timestamp", errProtocol)
	}
	for _, comment := range comments {
		if comment.ID <= 0 || comment.CreatedAt.IsZero() {
			return fmt.Errorf("%w: comment has no valid ID or timestamp", errProtocol)
		}
	}
	key := ticketKey(ticket.ID)
	keep := make([]string, 0, len(comments))
	link := fmt.Sprintf("%s/agent/tickets/%d", worker.Client.origin, ticket.ID)
	body := fmt.Sprintf("Zendesk ticket #%d: %s\nStatus: %s\nPriority: %s\nType: %s\nTags: %s", ticket.ID, ticket.Subject, ticket.Status, ticket.Priority, ticket.Type, strings.Join(ticket.Tags, ", "))
	if len(ticket.CustomFields) > 0 && string(ticket.CustomFields) != "null" {
		body += "\nCustom fields: " + string(ticket.CustomFields)
	}
	if len(comments) == 0 && ticket.Description != "" {
		body += "\n\n" + ticket.Description
	}
	actor := users[ticket.RequesterID]
	raw, err := json.Marshal(ticket)
	if err != nil {
		return errors.New("zendesk: encode ticket metadata")
	}
	if _, err := worker.Sink.Upsert(ctx, events.Event{
		Source: "zendesk", ExternalID: key, ThreadID: key, Scope: events.ScopeZendesk,
		Title: ticket.Subject, Body: body, URL: link, OccurredAt: ticket.CreatedAt,
		Actor: actor.Name, ActorID: strconv.FormatInt(ticket.RequesterID, 10), ActorCanonical: actor.Email, Raw: raw,
	}); err != nil {
		return err
	}
	for _, comment := range comments {
		keep = append(keep, fmt.Sprintf("%s:comment:%d", key, comment.ID))
		actor := users[comment.AuthorID]
		body := comment.PlainBody
		if body == "" {
			body = comment.Body
		}
		kind := "Public reply"
		if !comment.Public {
			kind = "Internal note"
		}
		body = kind + "\n\n" + body
		for _, attachment := range comment.Attachments {
			if !attachment.Deleted {
				body += fmt.Sprintf("\nAttachment: %s (%s, %d bytes)", attachment.Filename, attachment.Mimetype, attachment.Size)
			}
		}
		raw, err := json.Marshal(comment)
		if err != nil {
			return errors.New("zendesk: encode comment metadata")
		}
		if err := worker.upsertWithAttachments(ctx, events.Event{
			Source: "zendesk", ExternalID: fmt.Sprintf("%s:comment:%d", key, comment.ID), ThreadID: key,
			Scope: events.ScopeZendesk, Title: ticket.Subject, Body: body, URL: link,
			OccurredAt: comment.CreatedAt, Actor: actor.Name, ActorID: strconv.FormatInt(comment.AuthorID, 10),
			ActorCanonical: actor.Email, Raw: raw,
		}, comment.Attachments); err != nil {
			return err
		}
	}
	return worker.deleteAbsent(ctx, key+":comment:", key, keep)
}

// PurgeUnindexedGroups tombstones every indexed ticket that is now outside
// the indexed groups. importTicket catches a ticket on its next update; this
// covers tickets indexed before they moved, or before their group was dropped
// from the list. It searches every other group (new groups included) and
// ungrouped tickets. Search can lag a move, so each still-indexed hit is judged
// by its current state; hits already tombstoned cost no further API calls.
// Search is permission-filtered, so the credential must be able to read every
// group (an admin-owned OAuth client can). If it can't, the daily
// Reconcile refresh is the backstop (refreshTicket fails closed on 403).
func (worker *Worker) PurgeUnindexedGroups(ctx context.Context) error {
	groups, err := worker.Client.groupIDs(ctx)
	if err != nil {
		return err
	}
	filters := []string{"group:none"}
	for _, groupID := range groups {
		if !worker.indexes(groupID) {
			filters = append(filters, fmt.Sprintf("group:%d", groupID))
		}
	}
	for _, filter := range filters {
		ids, err := worker.Client.searchTicketIDs(ctx, filter)
		if err != nil {
			return err
		}
		keys := make([]string, len(ids))
		for i, ticketID := range ids {
			keys[i] = ticketKey(ticketID)
		}
		rows, err := worker.Pool.Query(ctx, `SELECT DISTINCT split_part(thread_id, ':', 2)::bigint FROM events
			WHERE source = 'zendesk' AND thread_id = ANY($1) AND NOT deleted`, keys)
		if err != nil {
			return err
		}
		indexed, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, ticketID := range indexed {
			if err := worker.refreshTicket(ctx, ticketID); err != nil {
				if !deferTicketError(err) {
					return err
				}
				if err := worker.deferRecord(ctx, "ticket", ticketID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (worker *Worker) deleteTicket(ctx context.Context, ticketID int64) error {
	if err := worker.tombstoneTicket(ctx, ticketID); err != nil {
		return err
	}
	return worker.clearRecordRetry(ctx, "ticket", ticketID)
}

// tombstoneTicket deletes a ticket's indexed events but leaves any retry record.
func (worker *Worker) tombstoneTicket(ctx context.Context, ticketID int64) error {
	rows, err := worker.Pool.Query(ctx, `SELECT external_id FROM events WHERE source = 'zendesk' AND thread_id = $1 AND NOT deleted`, ticketKey(ticketID))
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, externalID := range ids {
		if err := worker.deleteEvent(ctx, externalID); err != nil {
			return err
		}
	}
	return nil
}
