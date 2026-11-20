package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

// This file holds the single per-message ingest core shared by the
// live socket path (Consumer.processMessageEventAsync) and the
// worker/backfill path (Backfill.ingestMessageCore, called by
// SyncWorker + ThreadWorker). Before this existed the two paths
// hand-wrote the same sequence — parse, self-filter, actor resolve,
// prefix build, scope stamp, sink upsert, slack_message_details
// write, image reconcile — and they drifted: scope stamping (the
// most recent cross-cutting change) had to be applied in both
// places, and the error postures diverged silently. Now both call
// ingestRawMessage and translate one neutral result into their own
// metric vocabulary.
//
// The DESIGN of the error-posture split (see the long comment on
// ingestResult below) is the load-bearing part of this refactor:
// the live path treats side effects as best-effort and leans on the
// SyncWorker tail sweep as the durable backstop; the backfill/worker
// path treats them as hard errors so its own retry re-walks the
// message. The core preserves both by returning errors and letting
// the caller decide to swallow or propagate (plan choice (a): the
// core always returns, callers choose).

// ingestDeps is the set of collaborators the shared core needs. Both
// Consumer and Backfill hold all of these (under slightly different
// names), so each assembles an ingestDeps and calls ingestRawMessage.
// Keeping it a struct rather than a long positional parameter list
// means adding a future dependency is a one-field edit at the two
// call sites instead of a signature churn.
type ingestDeps struct {
	pool          *pgxpool.Pool
	sink          *events.DBSink
	dir           *Directory
	prefix        *PrefixBuilder
	logger        *slog.Logger
	workspaceHost string
	// selfUserID returns diderot's own slack user_id, or "" when not
	// yet resolved / filter disabled. A thunk (not a static string)
	// because the Consumer resolves the id asynchronously via
	// auth.test after construction; reading it lazily lets ingest see
	// the live value. nil disables the self-message filter.
	selfUserID func() string
	// ingest emits the per-path freshness counter + lag histogram at
	// sink-write time. Optional; nil disables observability without
	// affecting correctness. path is the label this caller stamps.
	ingest *ingestMetrics
	path   string
	// docGate reports whether a channel is opted in to document ingest.
	// nil disables document reconcile entirely (no event_documents rows
	// are ever created), which is the correct posture for any path that
	// hasn't been given a gate: default-deny.
	docGate DocIngestGate
}

// DocIngestGate is the per-channel document-ingest opt-in check the
// ingest path consults before creating any event_documents row.
// *ChannelConfigStore implements it.
//
// Gating at enqueue means an opted-out channel never even records that
// a document exists, so there is nothing to leak and nothing to purge
// if the flag is never flipped. The extraction worker re-checks
// independently, because a channel can be opted out after a row is
// already queued.
type DocIngestGate interface {
	DocIngestEnabled(ctx context.Context, channelID string) (bool, error)
}

// ingestDisposition is the neutral outcome label the core returns.
// Callers map it onto their own metric vocabulary (the live path's
// "upsert_insert"/"upsert_update"/... on diderot_slack_events_total,
// the backfill path's disposition* consts on
// diderot_slack_backfill_events_total, and the sync/thread workers'
// own per-source counters). Homogenizing the disposition here while
// keeping the metric NAMES caller-side is deliberate: the dashboards
// key on the existing names, so the core must
// NOT emit those counters itself.
type ingestDisposition string

const (
	ingestInserted       ingestDisposition = "inserted"
	ingestUpdated        ingestDisposition = "updated"
	ingestNoChange       ingestDisposition = "nochange"
	ingestSkippedSubtype ingestDisposition = "skipped_subtype"
	ingestSkippedSelf    ingestDisposition = "skipped_self"
)

// ingestResult is the structured outcome of one ingestRawMessage
// call. It carries the disposition (for the success metric) plus
// EventID, and — crucially — separates the two best-effort side
// effects from the hard upsert so each caller can apply its own
// error posture.
//
// THE ERROR-POSTURE SPLIT, in full:
//
//   - The event upsert is the hard core. If it fails, the function
//     returns a non-nil error and Disposition/EventID are unset.
//     BOTH callers treat this as a failure (live: log + metric +
//     ack-and-lean-on-sweep; backfill: return the error so the
//     sweep re-walks the message).
//
//   - slack_message_details is now folded INTO the upsert
//     transaction via a SideWrite (see detailsSideWrite). It is no
//     longer a separate post-commit Exec, so a details failure rolls
//     the WHOLE event back and surfaces as the hard upsert error
//     above. This is the real fix in this refactor: previously the
//     live path committed the event, then wrote details in a second
//     Exec, and swallowed a failure there — a transient DB blip
//     between the two stranded an event with no detail row, invisible
//     to the (channel_id, occurred_at) history pager. Atomicity
//     removes that window. The behavioral consequence for the live
//     path: a details failure used to be a swallowed "details_err"
//     with the event still committed; it is now an "upsert_err" with
//     nothing committed, and the sweep repairs it on the next pass.
//     The externally observable contract (ack to slack, rely on the
//     sweep) is unchanged — only which counter fires moves.
//
//   - ReconcileImages stays OUTSIDE the transaction. Folding it in
//     was considered and rejected: it's a multi-statement reconcile
//     against event_images with its own delete ordering, and the
//     image body is non-load-bearing for retrieval (the message text
//     already landed). Keeping it post-commit preserves its current
//     transactionality and keeps the hot event tx short. So image
//     reconcile remains a genuinely best-effort step whose posture
//     DIFFERS per caller — that difference is what ImageErr exists to
//     express: the core runs the reconcile, captures any error in
//     ImageErr, and returns nil for the function error. The live
//     caller logs ImageErr + bumps "attachments_err" and acks
//     anyway; the backfill caller promotes a non-nil ImageErr to a
//     returned error so its retry re-walks.
type ingestResult struct {
	Disposition ingestDisposition
	EventID     int64
	// ImageErr is the (best-effort) image-reconcile error, captured
	// rather than returned so the caller picks the posture. nil on the
	// skipped dispositions (no upsert happened, nothing to reconcile).
	ImageErr error
	// DocErr is the (best-effort) document-reconcile error, captured
	// rather than returned for the same reason as ImageErr — the caller
	// picks the posture. nil on the skipped dispositions.
	DocErr error
}

// detailsSideWrite builds the events.SideWrite that upserts the
// slack_message_details row inside the event transaction. d is the
// parsed details and occurredAt is the parent event's occurred_at,
// denormalized so the (channel_id, occurred_at) composite index can
// serve the channel-history pager + slack-fetch without sorting
// across the JOIN to events. ON CONFLICT (event_id) DO UPDATE so an
// edit (or a no-change re-ingest repairing a previously-missing row)
// refreshes the fields. Slack's message_changed events preserve the
// original ts, so occurred_at never moves on an edit.
func detailsSideWrite(d MessageDetails, occurredAt any) events.SideWrite {
	return func(ctx context.Context, tx pgx.Tx, eventID int64) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO slack_message_details
				(event_id, channel_id, user_id, thread_ts, is_edit, has_files, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (event_id) DO UPDATE SET
				channel_id  = EXCLUDED.channel_id,
				user_id     = EXCLUDED.user_id,
				thread_ts   = EXCLUDED.thread_ts,
				is_edit     = EXCLUDED.is_edit,
				has_files   = EXCLUDED.has_files,
				occurred_at = EXCLUDED.occurred_at
		`, eventID, d.ChannelID, nullIfEmpty(d.UserID),
			nullIfEmpty(d.ThreadTS), d.IsEdit, d.HasFiles, occurredAt)
		return err
	}
}

// ingestRawMessage is the single per-message ingest core. It takes
// the raw Slack message JSON (the exact shape ParseMessage expects —
// the live path passes the remarshaled MessageEvent, backfill passes
// a synthesized msgEnvelope) and runs the full sequence:
// parse -> self-filter -> actor resolve -> prefix build -> scope
// stamp -> sink upsert (with the slack_message_details side write in
// the same tx) -> image reconcile.
//
// The live socket path handles message_deleted before calling this, but
// history/sync can expose a deletion as an unwrapped top-level
// subtype="tombstone" message (the snapshot shape conversations.history /
// conversations.replies return for a thread root deleted while it still
// has live replies — distinct from the live Events API's message_changed-
// wrapped delta shape). ParseMessage maps both to res.Delete; this must
// call sink.Delete for it here so a missed live event can still be
// repaired by the durable sweep. Beyond that one shape, a message that
// Slack simply stops returning at all (an ordinary reply deleted, or a
// childless root deleted) is NOT something this per-message core can see:
// it only ever sees messages Slack DID return, and nothing reconciles
// that absence, so such a delete missed live is not applied.
//
// Returns a hard error for parse / upsert (the latter now including a
// folded-in details failure). The image-reconcile error is carried in
// the result's ImageErr for the caller to interpret per its posture.
func ingestRawMessage(ctx context.Context, deps ingestDeps, raw []byte) (ingestResult, error) {
	res, err := ParseMessage(ctx, raw, deps.workspaceHost, deps.dir)
	if err != nil {
		return ingestResult{}, fmt.Errorf("parse: %w", err)
	}
	if res.Delete {
		deleted, err := deps.sink.Delete(ctx, res.Source, res.ExternalID)
		if err != nil {
			return ingestResult{}, fmt.Errorf("delete: %w", err)
		}
		if !deleted.Existed {
			return ingestResult{Disposition: ingestNoChange}, nil
		}
		return ingestResult{Disposition: ingestUpdated, EventID: deleted.EventID}, nil
	}
	// Skipped subtype (channel_join, channel_leave, etc). Nothing to
	// upsert, and unlike the tombstone branch above there is no source
	// identity to delete.
	if res.Event == nil {
		return ingestResult{Disposition: ingestSkippedSubtype}, nil
	}

	// Skip messages diderot itself posted. The bot's slack output
	// (assistant status activity and answer chunks)
	// is duplicative with ask_messages and pollutes retrieve hits —
	// chunks similar to the asker's question cluster at the top of
	// vector search because they literally discuss whatever the asker
	// just asked about. Filter by actor_id (the bot's user_id) so
	// human follow-up messages in diderot threads still get ingested.
	//
	// selfUserID is "" until the consumer resolves it (auth.test on
	// Run) or when the filter is disabled (standalone backfill, most
	// tests); the guard is belt-and-suspenders for those cases.
	if deps.selfUserID != nil {
		if self := deps.selfUserID(); self != "" && res.Event.ActorID == self {
			return ingestResult{Disposition: ingestSkippedSelf}, nil
		}
	}

	// Resolve the author's display name + canonical key via Directory.
	// Best-effort: on lookup failure we leave Actor empty and accept
	// "actor_id only" in retrieval output (still correct, just less
	// readable). Stored value is the name as-of ingest; renames don't
	// back-propagate. ActorCanonical (email, or
	// 'slack:<user_id>' when email is missing) is what the retrieve
	// actor filter matches against for cross-source actor lookups.
	if deps.dir != nil && res.Event.ActorID != "" {
		switch {
		case res.Event.ActorCanonical == "":
			if ui, err := deps.dir.LookupUserInfo(ctx, res.Event.ActorID); err == nil {
				res.Event.Actor = pickName(ui.DisplayName, ui.Name)
				res.Event.ActorCanonical = SlackActorCanonical(ui)
			}
		case res.Event.Actor == "" && res.Event.ActorCanonical == SlackBotActorCanonical(res.Event.ActorID):
			if bi, err := deps.dir.LookupBot(ctx, res.Event.ActorID); err == nil {
				res.Event.Actor = bi.Name
			}
		}
	}

	details, _ := DetailsFrom(raw)

	// Contextual-retrieval prefix (channel name + optional thread
	// excerpt). Best-effort: degrades to "" on error, so we don't gate
	// the upsert on it.
	if deps.prefix != nil {
		// The message ts is the ExternalID with the "channelID:" prefix
		// stripped (ExternalID is "<channel>:<ts>"). This is what the
		// live path computed inline; backfill passed msg.Timestamp,
		// which is the same value — deriving it from ExternalID here
		// means the core doesn't need the original slack.Message.
		ts := strings.TrimPrefix(res.Event.ExternalID, details.ChannelID+":")
		res.Event.ContextPrefix = deps.prefix.Build(ctx, details.ChannelID, details.ThreadTS, ts, res.Event.Actor)
	}

	// Visibility scope from channel privacy (deny-by-default on lookup
	// failure — see stampScope).
	stampScope(ctx, deps.dir, deps.logger, details.ChannelID, res.Event)

	// Upsert the event AND its slack_message_details row in one
	// transaction. Folding details into the tx is the correctness fix
	// in this refactor: a transient failure between the event commit
	// and a separate details Exec used to strand the event with no
	// detail row (invisible to the channel-history pager). Now it's
	// all-or-nothing.
	upRes, err := deps.sink.UpsertWithSideWrite(ctx, *res.Event,
		detailsSideWrite(details, res.Event.OccurredAt))
	if err != nil {
		return ingestResult{}, fmt.Errorf("upsert: %w", err)
	}

	if deps.ingest != nil && !upRes.NoChange {
		// Increment the per-path ingest counter. Skip NoChange to keep
		// the counter signal clean — re-ingesting an already-present
		// message isn't fresh ingest. Source-level lag is tracked at
		// the DBSink layer (diderot_events_ingest_lag_seconds).
		deps.ingest.observe(deps.path)
	}

	out := ingestResult{EventID: upRes.EventID}
	switch {
	case upRes.NoChange:
		out.Disposition = ingestNoChange
	case upRes.WasUpdate:
		out.Disposition = ingestUpdated
	default:
		out.Disposition = ingestInserted
	}

	// File attachments: reconcile event_images for this event against
	// the current attachment list — INSERT new rows for new
	// attachments, DELETE chunks+rows for attachments removed in an
	// edit, maintain events.image_count. The caption worker picks new
	// rows up via the uncaptioned partial index. Same shape across
	// both paths so a backfill replay of an edited message lands the
	// same end state as a live edit.
	//
	// Kept OUTSIDE the upsert tx (see ingestResult): captured in
	// ImageErr, not returned, so the caller applies its posture (live:
	// best-effort log+swallow; backfill: promote to a returned error).
	if err := events.ReconcileImages(ctx, deps.pool, upRes.EventID,
		ImageRefsFromAttachments(details.Attachments)); err != nil {
		out.ImageErr = err
	}

	// Document attachments: same reconcile shape, gated per channel.
	//
	// When the gate is CLOSED we still reconcile, with an empty ref list.
	// That's deliberate rather than a skip: reconcile-with-nothing is
	// what DELETES rows for documents that are no longer eligible, so
	// disabling a channel and re-ingesting a message actually removes
	// its extracted text rather than orphaning it. A skip would leave
	// previously-created rows and their chunks searchable.
	//
	// When the gate is UNREADABLE we skip entirely, because the
	// delete-on-empty semantics above make "reconcile with nothing" a
	// destructive act — a transient DB blip must not wipe extracted text.
	if refs, ok := deps.documentRefs(ctx, details); ok {
		if err := events.ReconcileDocuments(ctx, deps.pool, upRes.EventID, refs); err != nil {
			out.DocErr = err
		}
	}
	return out, nil
}

// documentRefs returns the extractable document attachments for this
// message and whether the caller should reconcile at all.
//
// The bool is load-bearing, not stylistic. Reconciling with an empty
// list deletes every existing row for the event, so the three outcomes
// must stay distinct:
//
//	(refs, true)  channel opted in — reconcile to this exact set.
//	(nil,  true)  channel opted out, or nothing extractable attached —
//	              reconcile to empty, which correctly removes any rows
//	              and chunks that a prior opt-in period created.
//	(nil,  false) gate unreadable, or no gate wired — do nothing.
//	              Failing closed on CREATION is right; failing closed on
//	              DELETION would destroy data on a transient error.
func (deps *ingestDeps) documentRefs(ctx context.Context, details MessageDetails) ([]events.DocumentRef, bool) {
	if deps.docGate == nil || details.ChannelID == "" {
		return nil, false
	}
	enabled, err := deps.docGate.DocIngestEnabled(ctx, details.ChannelID)
	if err != nil {
		if deps.logger != nil {
			deps.logger.Warn("slack: doc-ingest gate unreadable; skipping document reconcile",
				"channel_id", details.ChannelID, "err", err)
		}
		return nil, false
	}
	if !enabled {
		return nil, true
	}
	return DocumentRefsFromAttachments(details.Attachments), true
}
