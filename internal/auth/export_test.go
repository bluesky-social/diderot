package auth

import "time"

// SetBrowserLoginClock replaces b's clock for session-lifetime tests.
func SetBrowserLoginClock(b *BrowserLogin, now func() time.Time) {
	b.now = now
}
