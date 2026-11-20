package notion

import "testing"

func TestStripExpiringNotionURLs(t *testing.T) {
	in := "before ![diagram](https://prod-files-secure.s3.us-west-2.amazonaws.com/x.png?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeef) after [spec](https://example.com/spec)"
	want := "before ![diagram] after [spec](https://example.com/spec)"
	if got := stripExpiringNotionURLs(in); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
