package markdown

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// Info is the Markdown folder's facts as a source. Its documents have no
// actor.
func (Connector) Info() source.Info {
	return source.Info{
		EventSource: ConnectorName, Title: "Markdown folder", ResultNoun: "Markdown",
		Summary:         "Markdown files in a folder the deployment mounts.",
		ToolDescription: "Markdown documents from a folder this deployment indexes.",
	}
}

// Run scans the folder at once, then again every rescan interval, until ctx
// ends.
func (Connector) Run(ctx context.Context, deps source.Deps, resolved settings.Resolved) error {
	dir := os.Getenv(DirVariable)
	if dir == "" {
		return errNoFolder
	}
	baseURL := resolved.String(FieldBaseURL)
	sup := worker.NewSupervisor(deps.Logger, deps.Metrics)
	sup.AddRunner(worker.Runner{
		Name: "markdown-scan", Interval: max(resolved.Duration(FieldRescanInterval), minRescanInterval), BootTick: true,
		Run: func(ctx context.Context) error { return rescan(ctx, deps, dir, baseURL, maxFiles) },
	})
	sup.Run(ctx)
	return nil
}

// lastScanKey is the source's state key for how its last scan went, which
// its page shows on every replica.
const lastScanKey = "last_scan"

// lastScan is how the last scan that was not cancelled went.
type lastScan struct {
	At time.Time `json:"at"`
	// Error is what the page says about the scan's failure, or "".
	Error string `json:"error,omitempty"`
	// Indexed and Skipped are the counts of the last scan that read the
	// whole folder.
	Indexed int64 `json:"indexed"`
	Skipped int64 `json:"skipped"`
}

// rescan scans the folder and records how it went, unless ctx ended first.
func rescan(ctx context.Context, deps source.Deps, dir, baseURL string, limit int) error {
	done, err := scan(ctx, deps, dir, baseURL, limit)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var last lastScan
	if _, getErr := deps.State.Get(ctx, lastScanKey, &last); getErr != nil {
		return errors.Join(err, getErr)
	}
	last.At, last.Error = time.Now().UTC(), ""
	if err != nil {
		last.Error = describe(err)
	} else {
		last.Indexed, last.Skipped = done.indexed, done.skipped
	}
	if setErr := deps.State.Set(ctx, lastScanKey, last); setErr != nil {
		return errors.Join(err, setErr)
	}
	if err != nil {
		return fmt.Errorf("markdown scan: %w", err)
	}
	if done.changed > 0 || done.removed > 0 {
		deps.Logger.Info("markdown scan changed the index", "changed", done.changed, "removed", done.removed,
			"indexed", done.indexed, "skipped", done.skipped)
	}
	return nil
}

// Expand returns the document's stored text. The scope gate ran before it.
func (Connector) Expand(ctx context.Context, deps source.Deps, eventID int64, _ []string) (source.Expanded, error) {
	stored, err := deps.Sink.StoredBody(ctx, eventID)
	if err != nil {
		return source.Expanded{}, err
	}
	if stored.Source != ConnectorName {
		// The event was re-ingested as another source since the caller
		// looked it up.
		return source.Expanded{}, source.ErrNotExpandable
	}
	return source.Expanded{Content: stored.Body}, nil
}
