package adminui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminapi"
)

// fakeImages serves one PNG and one HTML "image", and records each image
// asked for.
type fakeImages struct{ asked []string }

func (f *fakeImages) AskInputImage(_ context.Context, id string) ([]byte, string, error) {
	f.asked = append(f.asked, id)
	switch id {
	case "FIMG1":
		return []byte("PNGDATA"), "image/png", nil
	case "FHTML":
		return []byte("<script>alert(1)</script>"), "text/html", nil
	case "FPENDING":
		return nil, "", adminapi.ErrImageNotMirrored
	}
	return nil, "", adminapi.ErrImageNotFound
}

// TestAskImageIsServedToTheAdministratorsBrowser: the image an Ask
// conversation page embeds comes from a route the browser's own sign-in
// reaches, for any administrator, and from nowhere an API bearer or another
// signed-in user can use.
func TestAskImageIsServedToTheAdministratorsBrowser(t *testing.T) {
	images := &fakeImages{}
	server, err := New(Config{
		Authenticator: testBrowserAuth(), Pool: new(pgxpool.Pool),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		Images: images,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	get := func(id string, edit func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "https://diderot.test/admin/ask/images/"+id, nil)
		edit(r)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	admin := func(r *http.Request) {
		r.Header.Set(proxyEmailHeader, "operator@example.com")
		r.Header.Set(proxyGroupsHeader, "everyone|admins")
	}

	w := get("FIMG1", admin)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "PNGDATA", w.Body.String())
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, []string{"FIMG1"}, images.asked)

	// Stored bytes that are not an image are never rendered in this origin.
	w = get("FHTML", admin)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, "attachment", w.Header().Get("Content-Disposition"))

	assert.Equal(t, http.StatusNotFound, get("FOTHER", admin).Code, "in no conversation, or never stored")
	assert.Equal(t, http.StatusNotFound, get("FPENDING", admin).Code)

	images.asked = nil
	assert.Equal(t, http.StatusUnauthorized, get("FIMG1", func(*http.Request) {}).Code)
	assert.Equal(t, http.StatusUnauthorized, get("FIMG1", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer an-api-token")
	}).Code, "an API bearer is not a browser sign-in")
	assert.Equal(t, http.StatusForbidden, get("FIMG1", func(r *http.Request) {
		r.Header.Set(proxyEmailHeader, "member@example.com")
		r.Header.Set(proxyGroupsHeader, "everyone")
	}).Code)
	assert.Empty(t, images.asked, "nobody but an administrator reaches the image reader")

	// Without object storage wired there is nothing to serve.
	server.images = nil
	assert.Equal(t, http.StatusNotFound, get("FIMG1", admin).Code)
}
