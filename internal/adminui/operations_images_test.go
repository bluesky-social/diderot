package adminui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/testdb"
)

// seedCaptionedImage stores one image of a new event with a caption chunk
// written by model.
func seedCaptionedImage(t *testing.T, pool *pgxpool.Pool, externalID, model string) int64 {
	t.Helper()
	ctx := context.Background()
	var eventID, chunkID, imageID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO events (source, external_id, occurred_at, actor, body, raw)
		VALUES ('slack', $1, now(), 'tester', 'body', '{}'::jsonb) RETURNING id`, externalID).Scan(&eventID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
		VALUES ($1, 'image_caption', 0, 'a caption', now()) RETURNING id`, eventID).Scan(&chunkID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at, storage_key, captioned_at, caption_model, chunk_id)
		VALUES ($1, 'F1', 'image/png', now(), 'slack/ab/abc.png', now(), $2, $3) RETURNING id`, eventID, model, chunkID).Scan(&imageID))
	return imageID
}

// TestOperationsDescribeImagesAgainBrowserWorkflow: the Jobs page offers to
// have the current model describe the images an earlier model described,
// queues exactly those on a confirmed request, and then says they are waiting.
func TestOperationsDescribeImagesAgainBrowserWorkflow(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui_operations_images"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE admin_audit_log, events RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	earlier := seedCaptionedImage(t, pool, "C0EXAMPLE01:1.0", "earlier-model")
	also := seedCaptionedImage(t, pool, "C0EXAMPLE01:2.0", "earlier-model")
	current := seedCaptionedImage(t, pool, "C0EXAMPLE01:3.0", "current-model")

	model := "current-model"
	server, err := New(Config{
		Authenticator: testBrowserAuth(), Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminGroups: []string{"admins"}, PublicURL: "https://diderot.test",
		CaptionModel: func() string { return model },
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, server.Mount(mux))
	const jobsURL, imagesURL = "https://diderot.test/admin/operations/jobs", "https://diderot.test/admin/operations/images"

	page := browserRequest(t, mux, http.MethodGet, jobsURL, nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "<span>Describe 2 images again</span>")
	assert.Contains(t, page.Body.String(), "Has current-model, the model that describes images now")
	assert.NotContains(t, page.Body.String(), "waiting to be described")
	var csrfToken string
	for _, cookie := range page.Result().Cookies() {
		if cookie.Name == csrfCookieName {
			csrfToken = cookie.Value
		}
	}
	require.NotEmpty(t, csrfToken)
	waiting := func(imageID int64) bool {
		var captioned bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT captioned_at IS NOT NULL FROM event_images WHERE id = $1`, imageID).Scan(&captioned))
		return !captioned
	}

	unconfirmed := browserRequest(t, mux, http.MethodPost, imagesURL, url.Values{"csrf_token": {csrfToken}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, unconfirmed.Code)
	assert.Contains(t, unconfirmed.Header().Get("Location"), "error=Nothing+was+started")
	assert.False(t, waiting(earlier), "nothing is queued without the confirmation")

	started := browserRequest(t, mux, http.MethodPost, imagesURL, url.Values{"csrf_token": {csrfToken}, "confirm": {"images"}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, started.Code)
	assert.Contains(t, started.Header().Get("Location"), "notice=2+images+will+be+described+again+by+current-model")
	assert.True(t, waiting(earlier))
	assert.True(t, waiting(also))
	assert.False(t, waiting(current), "the current model's image is left alone")
	var chunks int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks WHERE kind = 'image_caption'`).Scan(&chunks))
	assert.Equal(t, 3, chunks, "every image keeps its caption until the new one is ready")
	var action, target, status, details string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT action, target, status, details::text FROM admin_audit_log ORDER BY id DESC LIMIT 1`).Scan(&action, &target, &status, &details))
	assert.Equal(t, "operations.images.describe_again", action)
	assert.Equal(t, "image-captions", target)
	assert.Equal(t, "succeeded", status)
	assert.JSONEq(t, `{"model":"current-model"}`, details)

	page = browserRequest(t, mux, http.MethodGet, jobsURL, nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "<p>2 images are waiting to be described.</p>")
	assert.NotContains(t, page.Body.String(), ">Describe images again</button>", "nothing is left to request")

	again := browserRequest(t, mux, http.MethodPost, imagesURL, url.Values{"csrf_token": {csrfToken}, "confirm": {"images"}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, again.Code)
	assert.Contains(t, again.Header().Get("Location"), "notice=Nothing+to+do")

	// With image captions off there is no control, and a request does nothing.
	model = ""
	page = browserRequest(t, mux, http.MethodGet, jobsURL, nil, "")
	require.Equal(t, http.StatusOK, page.Code)
	assert.NotContains(t, page.Body.String(), "Describe images again")
	off := browserRequest(t, mux, http.MethodPost, imagesURL, url.Values{"csrf_token": {csrfToken}, "confirm": {"images"}}, csrfToken)
	require.Equal(t, http.StatusSeeOther, off.Code)
	assert.Contains(t, off.Header().Get("Location"), "error=Nothing+was+started")
	assert.False(t, waiting(current))
}
