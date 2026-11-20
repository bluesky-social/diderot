package slack

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

type backfillMetrics struct {
	events      *prometheus.CounterVec // diderot_slack_backfill_events_total{outcome}
	rateLimited *prometheus.CounterVec // diderot_slack_backfill_rate_limited_total{endpoint}
}

func newBackfillMetrics(reg prometheus.Registerer) *backfillMetrics {
	return &backfillMetrics{
		events: regBackfillCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_slack_backfill_events_total",
			Help: "Messages ingested by slack backfill, labeled by outcome (backfill_insert/backfill_update/backfill_noop).",
		}, []string{"outcome"})),
		rateLimited: regBackfillCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_slack_backfill_rate_limited_total",
			Help: "Count of Slack API rate-limit retries during backfill, labeled by endpoint (history/replies).",
		}, []string{"endpoint"})),
	}
}

func regBackfillCounter(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}
