package zendesk

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type countingObserver struct{ succeeded, failed atomic.Int64 }

func (o *countingObserver) Succeeded() { o.succeeded.Add(1) }
func (o *countingObserver) Failed()    { o.failed.Add(1) }

// TestRunTellsItsObserver: each ticket and article pass tells the
// connector's metrics whether it succeeded.
func TestRunTellsItsObserver(t *testing.T) {
	for _, failing := range []bool{false, true} {
		client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
			if failing && strings.Contains(request.URL.Path, "incremental/tickets") {
				writer.WriteHeader(http.StatusForbidden)
				return
			}
			if strings.Contains(request.URL.Path, "incremental/tickets") {
				respond(t, writer, map[string]any{"tickets": []Ticket{}, "after_cursor": "next", "end_of_stream": true})
				return
			}
			respond(t, writer, map[string]any{"articles": []Article{}, "end_time": 0})
		})
		worker := testWorker(t, client)
		observer := &countingObserver{}
		worker.Observer = observer
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan error, 1)
		go func() { finished <- worker.Run(ctx) }()
		require.Eventually(t, func() bool { return observer.succeeded.Load() > 0 }, 3*time.Second, time.Millisecond, "the article pass succeeds")
		if failing {
			require.Eventually(t, func() bool { return observer.failed.Load() > 0 }, 3*time.Second, time.Millisecond, "the ticket pass fails")
		}
		cancel()
		require.NoError(t, <-finished)
		if !failing {
			require.Zero(t, observer.failed.Load())
		}
	}
}
