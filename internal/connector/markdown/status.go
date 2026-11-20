package markdown

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// Status is when the last scan finished, and the files the last complete
// scan indexed and skipped. While no folder is mounted, or the last scan
// failed, it says what keeps the source from working.
func (Connector) Status(ctx context.Context, deps source.Deps) (settings.Status, error) {
	var status settings.Status
	var last lastScan
	found, err := deps.State.Get(ctx, lastScanKey, &last)
	if err != nil {
		return settings.Status{}, fmt.Errorf("markdown status: %w", err)
	}
	var at *time.Time
	var outcome string
	if found {
		at = &last.At
		if last.Error != "" {
			outcome, status.Waiting = "failed", "The last scan failed: "+last.Error
		}
	}
	if os.Getenv(DirVariable) == "" {
		status.Waiting = string(errNoFolder)
	}
	var skipped string
	if last.Skipped > 0 {
		skipped = "too large, not UTF-8 text, or empty"
	}
	status.Rows = []settings.StatusRow{
		{Label: "Last scan", Value: at, Detail: outcome},
		{Label: "Files indexed", Value: last.Indexed},
		{Label: "Files skipped", Value: last.Skipped, Detail: skipped},
	}
	return status, nil
}
