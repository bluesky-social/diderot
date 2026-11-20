package slack

import (
	"github.com/prometheus/client_golang/prometheus"
)

// threadMetrics is the ThreadWorker's prometheus surface. Same
// shape as syncMetrics, scoped to the thread queue. Held by
// pointer so a nil reg disables the lot.
type threadMetrics struct {
	// walks_total{outcome=ok|error|preempted}
	walks *prometheus.CounterVec

	// pages_total: one per successful conversations.replies page.
	pages prometheus.Counter

	// messages_total{outcome}: per-reply disposition out of the
	// thread walk. Same labels as syncMetrics.messages.
	messages *prometheus.CounterVec

	claims    prometheus.Counter
	idleTicks prometheus.Counter

	walkDuration prometheus.Histogram
	pageLatency  prometheus.Histogram
}

func newThreadMetrics(reg prometheus.Registerer) *threadMetrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	return &threadMetrics{
		walks: regSyncCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_slack_thread_walks_total",
				Help: "ThreadWorker walk completions by outcome (ok|error|preempted).",
			}, []string{"outcome"})),
		pages: regSyncCounter(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "diderot_slack_thread_pages_total",
				Help: "Successful conversations.replies pages returned by Slack during ThreadWorker walks.",
			})),
		messages: regSyncCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_slack_thread_messages_total",
				Help: "Per-reply disposition out of the ThreadWorker walk (inserted|updated|nochange|skipped_subtype|parse_error).",
			}, []string{"outcome"})),
		claims: regSyncCounter(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "diderot_slack_thread_claims_total",
				Help: "ClaimNext invocations on the thread queue that returned a row to walk.",
			})),
		idleTicks: regSyncCounter(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "diderot_slack_thread_idle_ticks_total",
				Help: "ClaimNext invocations on the thread queue that returned no eligible row.",
			})),
		walkDuration: regSyncHistogram(reg, prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "diderot_slack_thread_walk_duration_seconds",
				Help:    "Wall-clock duration of a ThreadWorker walk (claim → markDone or markError).",
				Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
			})),
		pageLatency: regSyncHistogram(reg, prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "diderot_slack_thread_page_latency_seconds",
				Help:    "Per-conversations.replies-page wall-clock latency.",
				Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
			})),
	}
}
