package adminui

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNotFoundPage: an address nothing is at answers 404 with a page in the
// shell that says so and leads somewhere, for a path no route has and for a
// record that does not exist; neither is bare text.
func TestNotFoundPage(t *testing.T) {
	s := &Server{
		authenticator: testBrowserAuth(), version: "test", logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		adminGroups: map[string]struct{}{"admins": {}},
	}
	mux := http.NewServeMux()
	require.NoError(t, s.Mount(mux))
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		return browserRequest(t, mux, http.MethodGet, "https://diderot.test"+path, nil, "")
	}

	for path, want := range map[string]missing{
		"/admin/no-such-page":             missingPage,
		"/admin/operations/no-such-tab":   missingPage,
		"/admin/ask/sessions/not-an-id":   missingConversation,
		"/admin/prompts/versions/nothing": missingVersion,
		"/admin/prompts/legacy/0":         missingLegacyPrompt,
	} {
		response := get(path)
		assert.Equal(t, http.StatusNotFound, response.Code, path)
		body := response.Body.String()
		assert.Contains(t, body, "<h1>"+want.heading+"</h1>", path)
		assert.Contains(t, body, `<a class="button" href="`+want.backHref+`">`+want.backLabel+`</a>`, path)
		assert.Contains(t, body, `<nav class="app-nav" aria-label="Main">`, "the page is in the shell: "+path)
		assert.Contains(t, response.Header().Get("Content-Type"), "text/html", path)
	}
}
