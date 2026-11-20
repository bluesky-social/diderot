package promptcatalog

import (
	"context"
	"log/slog"
	"time"
)

// RunWatcher keeps long-lived non-session workers on the active DB release,
// polling every interval for a newly activated one. The caller has already
// applied the current release (through SetActive) before calling this, and
// passes its id as activeID so the first tick does not redo that work;
// RunWatcher never reads the database before its first tick. Activation
// remains durable even if this best-effort refresh is delayed.
func RunWatcher(ctx context.Context, store *Store, activeID int64, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	refresh := func() {
		release, err := store.Current(ctx)
		if err != nil {
			logger.Error("prompt release refresh failed", "err", err)
			return
		}
		if release.ID == activeID {
			return
		}
		if err := SetActive(release.Bundle); err != nil {
			logger.Error("active prompt release invalid", "release_id", release.ID, "err", err)
			return
		}
		activeID = release.ID
		logger.Info("active prompt release loaded", "release_id", release.ID,
			"source_revision", release.SourceRevision, "bundle_sha256", release.Bundle.SHA256)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}
