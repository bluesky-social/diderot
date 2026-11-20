package adminaction

import (
	"context"
	"errors"
)

// SummaryControl switches session summarization (*sessioningest.SummaryControl).
type SummaryControl interface {
	SetEnabled(context.Context, bool) error
}

// SetSessionSummarization turns session summarization on or off.
func SetSessionSummarization(ctx context.Context, a Audit, control SummaryControl, enabled bool) error {
	return a.Run(ctx, "operations.session_summarization.set", "session-summarization",
		map[string]any{"enabled": enabled}, func() error {
			if control == nil {
				return errors.New("session summarization control unavailable")
			}
			return control.SetEnabled(ctx, enabled)
		})
}

// FailureRetrier puts background work that gave up back in its queue.
type FailureRetrier interface {
	// RetryFailures retries the items of one kind of work ("index",
	// "caption", or "document") that gave up for one cause, and returns how
	// many it put back.
	RetryFailures(ctx context.Context, kind, cause string) (int64, error)
}

// RetryFailures retries the items of kind that gave up for cause.
func RetryFailures(ctx context.Context, a Audit, retrier FailureRetrier, kind, cause string) (int64, error) {
	var retried int64
	err := a.Run(ctx, "operations.failures.retry", kind, map[string]any{"cause": cause}, func() error {
		if retrier == nil {
			return errors.New("failure retry is not configured")
		}
		var err error
		retried, err = retrier.RetryFailures(ctx, kind, cause)
		return err
	})
	return retried, err
}

// ImageRequeuer puts captioned images back in the caption queue.
type ImageRequeuer interface {
	// RequeueOtherModels queues every image whose caption a model other
	// than model wrote, and returns how many it queued.
	RequeueOtherModels(ctx context.Context, model string) (int64, error)
}

// DescribeImagesAgain queues every image whose caption a model other than
// model wrote, for model to describe, and returns how many it queued.
func DescribeImagesAgain(ctx context.Context, a Audit, requeuer ImageRequeuer, model string) (int64, error) {
	var queued int64
	err := a.Run(ctx, "operations.images.describe_again", "image-captions", map[string]any{"model": model}, func() error {
		if requeuer == nil {
			return errors.New("image captions are not configured")
		}
		var err error
		queued, err = requeuer.RequeueOtherModels(ctx, model)
		return err
	})
	return queued, err
}

// SummaryRepairRequester records a repair of session summaries for the
// leader to run.
type SummaryRepairRequester interface {
	RequestSummaryRepair(ctx context.Context, requestedBy string) (int64, error)
}

// RepairSessionSummaries requests the repair of session summaries written in
// the last 30 days whose text holds the summary model's tool-call markup, and
// returns the job's ID.
func RepairSessionSummaries(ctx context.Context, a Audit, requester SummaryRepairRequester) (int64, error) {
	var jobID int64
	err := a.Run(ctx, "operations.session_summaries.repair", "session-summaries", nil, func() error {
		if requester == nil {
			return errors.New("session summary repair is not configured")
		}
		var err error
		jobID, err = requester.RequestSummaryRepair(ctx, a.actor())
		return err
	})
	return jobID, err
}

// ReembedLauncher records a re-embed job for the leader to run
// (*adminapi.Server).
type ReembedLauncher interface {
	StartReembed(ctx context.Context, requestedBy string, sources []string) (int64, error)
}

// StartReembed requests a re-embed of sources, or of every source when
// sources is empty, and returns the job's ID.
func StartReembed(ctx context.Context, a Audit, launcher ReembedLauncher, sources []string) (int64, error) {
	var requested any = "all"
	if len(sources) > 0 {
		requested = sources
	}
	var jobID int64
	err := a.Run(ctx, "operations.reembed.start", "reembed", map[string]any{"sources": requested}, func() error {
		if launcher == nil {
			return errors.New("re-embed launcher is not configured")
		}
		var err error
		jobID, err = launcher.StartReembed(ctx, a.actor(), sources)
		return err
	})
	return jobID, err
}
