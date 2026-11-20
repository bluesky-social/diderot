package slack

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

type consumerMetrics struct {
	// events counts each Slack message event by outcome:
	//   upsert_insert / upsert_update / delete_ok
	//   parse_err / upsert_err / delete_err / details_err / remarshal_err
	//   skipped (unhandled subtype)
	//   blacklisted (channel on slack_ingest_blacklist; live event dropped)
	events *prometheus.CounterVec

	// retries counts Slack-side re-deliveries (envelope.RetryAttempt > 0).
	// Two distinct cases share this label space, distinguished by reason:
	//
	//   - in-connection 3s ack-timeout retries (we failed to ack within the
	//     socket-mode deadline; Slack redelivers on the same connection).
	//     reason="timeout" or similar.
	//
	//   - "Delayed Events" recovery queue (the bot was unreachable for a
	//     stretch; Slack queues events and replays them hourly for up to
	//     24h after we reconnect). reason values likely include
	//     "queued_timeout" or similar — captured verbatim for forensics.
	//
	// The retry_reason label disambiguates without us having to guess.
	retries *prometheus.CounterVec

	// redeliveries counts events the consumer dropped at the
	// dedupe boundary because their (channel_id, message_ts) was
	// already processed inside the seenStore TTL window. Always
	// the result of either:
	//   - Slack ack-timeout retries (we acked late; Slack
	//     redelivered) — should be vanishingly rare once the
	//     consumer always acks fast enough.
	//   - A real bug in the consumer's ack path that re-enters
	//     handleEventsAPI for the same event.
	// Either way, a non-zero rate is a problem to investigate;
	// we log loudly and metric noisily.
	redeliveries *prometheus.CounterVec

	// askGate counts every @-mention gate decision in maybeRouteAsk
	// by outcome: allowed / blocked_not_enabled /
	// blocked_external_members / blocked_external_asker /
	// blocked_gate_error. Blocked mentions used to be log-only
	// ("silent drop"); this is the operator-visible counter that
	// closes that gap.
	askGate *prometheus.CounterVec

	// askOptin counts self-serve "Enable diderot in this channel"
	// button outcomes: enabled / refused_external_members /
	// refused_external_clicker / error.
	askOptin *prometheus.CounterVec

	// askFeedback counts ask answer reactions processed from
	// reaction_added events, by verdict (up, down, other).
	askFeedback *prometheus.CounterVec

	// askClaims counts ask-turn claim attempts by result:
	//   claimed   — INSERT succeeded; turn will spawn.
	//   duplicate — ON CONFLICT DO NOTHING suppressed a redelivery.
	//   error     — DB error; turn spawned anyway (fail-open).
	askClaims *prometheus.CounterVec

	// askCaptionBarrierWait records how long an ask turn spent in the
	// attachment barrier when a real wait happened (initialPending > 0).
	// Not observed on the no_wait path — zeros in the histogram would
	// mask the distribution of genuine waits. The barrier outcome is
	// tracked separately via askCaptionBarrier.
	askCaptionBarrierWait *prometheus.HistogramVec

	// askCaptionBarrier counts attachment-barrier outcomes for every ask
	// turn that reaches the barrier check:
	//   no_wait   — count query returned 0; no wait entered.
	//   settled   — waited; extraction/captioning and derived embeds landed.
	//   timed_out           — caption/extraction remained pending; Ask did not start.
	//   embedding_timed_out — a derived embed remained pending; Ask did not start.
	//   state_error         — final state could not be verified; Ask did not start.
	askCaptionBarrier *prometheus.CounterVec

	// askStatus counts native assistant thread status lifecycle outcomes.
	askStatus *prometheus.CounterVec

	// summarize counts thread-summarizer message-shortcut outcomes:
	//   posted          — summary delivered to the thread (public or ephemeral).
	//   post_error      — summary generated but delivery failed (limiter,
	//                    Slack API, or timeout); invoker notified.
	//   rejected_busy   — admission semaphore full; invoker told to retry.
	//   not_configured  — no summarizer wired on this deployment.
	//   not_thread      — shortcut run on a standalone message; guidance sent.
	//   empty           — thread fetched but empty.
	//   fetch_error     — conversations.replies / users.info failed.
	//   llm_error       — gateway chat-completions call failed.
	//   llm_empty       — gateway returned 200 with empty/whitespace
	//                    content; invoker told there was no result.
	summarize *prometheus.CounterVec
}

func newConsumerMetrics(reg prometheus.Registerer) *consumerMetrics {
	return &consumerMetrics{
		events: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_slack_events_total",
			Help: "Slack events by outcome.",
		}, []string{"outcome"})),
		retries: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_slack_socket_retries_total",
			Help: "Inbound socket-mode events with retry_attempt > 0, grouped by retry_reason. Surfaces both in-connection ack-timeout retries and Delayed Events recovery-queue replays.",
		}, []string{"retry_reason"})),
		redeliveries: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_slack_redeliveries_dropped_total",
			Help: "Events dropped at the dedupe boundary because (channel, ts) was already processed inside the seenStore TTL window. Non-zero rate means the consumer is acking late OR there's a bug in our ack path. event_subtype labels by retry_reason when set, else 'no_retry' for unexpected duplicates.",
		}, []string{"retry_reason"})),
		askGate: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_slack_ask_gate_total",
			Help: "Ask-gate decisions for @-mentions by outcome (allowed, blocked_not_enabled, blocked_external_members, blocked_external_asker, blocked_gate_error).",
		}, []string{"outcome"})),
		askOptin: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_slack_ask_optin_total",
			Help: "Self-serve ask opt-in button outcomes (enabled, refused_external_members, refused_external_clicker, error).",
		}, []string{"outcome"})),
		askFeedback: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_feedback_total",
			Help: "Ask answer reactions by verdict (up, down, other).",
		}, []string{"verdict"})),
		askClaims: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_slack_ask_claims_total",
			Help: "Ask-turn claim attempts by result (claimed, duplicate, error). A 'duplicate' means a redelivery reached the ask gate; an 'error' means the claim DB write failed and the turn spawned anyway (fail-open).",
		}, []string{"result"})),
		askCaptionBarrierWait: regHistogram(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_ask_caption_barrier_wait_seconds",
			Help:    "Duration of real attachment-barrier waits (observed only when initialPending > 0). Covers image captioning, document extraction, and their derived embeddings before the Ask timeout starts; the longest deadline is 45s.",
			Buckets: prometheus.ExponentialBucketsRange(0.1, 45, 8),
		}, []string{})),
		askCaptionBarrier: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_caption_barrier_total",
			Help: "Ask-turn attachment-barrier outcomes: no_wait, settled, timed_out (caption/extraction pending; Ask not started), embedding_timed_out (Ask not started), or state_error (Ask not started because final state was unverifiable).",
		}, []string{"outcome"})),
		askStatus: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_slack_ask_status_total",
			Help: "Native Slack Ask thread-status lifecycle outcomes.",
		}, []string{"outcome"})),
		summarize: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_slack_summarize_total",
			Help: "Thread-summarizer message-shortcut outcomes: posted, post_error, rejected_busy, not_configured, not_thread, empty, fetch_error, llm_error, llm_empty.",
		}, []string{"outcome"})),
	}
}

func regCounter(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}

func regHistogram(reg prometheus.Registerer, v *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.HistogramVec)
		}
		panic(err)
	}
	return v
}
