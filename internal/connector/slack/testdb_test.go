package slack

import (
	"testing"

	"github.com/bluesky-social/diderot/internal/testdb"
)

// slackTestDBURL returns the connection URL for the slack package's
// isolated integration database — a per-package clone of the migrated
// base (see internal/testdb). Every pool-getter in this package routes
// through here so they all share ONE database; that's intentional,
// since the getters TRUNCATE overlapping table sets and the
// intra-package tests already run serially. Cross-package isolation
// (the win that lets the suite drop `-p 1`) comes from this package's
// database being distinct from every other package's.
//
// Skips the test when DIDEROT_TEST_DB_URL is unset, preserving the
// original behavior of each getter's inline os.Getenv check.
func slackTestDBURL(t *testing.T) string {
	t.Helper()
	return testdb.URL(t, "slack")
}
