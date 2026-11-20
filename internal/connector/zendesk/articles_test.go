package zendesk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/events"
)

type articleFixture struct {
	public             bool
	deleted            bool
	translationsFail   bool
	publicFilesDenied  bool
	authenticatedFiles int
}

func fixtureArticle() Article {
	return Article{
		ID: 7, AuthorID: 11, Title: "Public guidance", Body: "<p>Public answer</p>", Locale: "en-us",
		CreatedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC),
	}
}

func (fixture *articleFixture) serve(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	t.Helper()
	authenticated := request.Header.Get("Authorization") != ""
	article := fixtureArticle()
	switch {
	case strings.Contains(request.URL.Path, "incremental/articles"):
		require.True(t, authenticated)
		respond(t, writer, map[string]any{"articles": []Article{article}, "end_time": int64(1800000000), "next_page": nil})
	case strings.HasSuffix(request.URL.Path, "/translations.json"):
		require.True(t, authenticated)
		if fixture.deleted {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if fixture.translationsFail {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		respond(t, writer, map[string]any{"translations": []Translation{{SourceID: 7, Locale: "en-us", Title: "Unpublished guidance", Body: "<p>PRIVATE draft</p>", Draft: true, UpdatedAt: article.UpdatedAt}}, "meta": map[string]any{"has_more": false}})
	case strings.HasSuffix(request.URL.Path, "/attachments.json"):
		if authenticated {
			fixture.authenticatedFiles++
		} else if fixture.publicFilesDenied {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		respond(t, writer, map[string]any{"article_attachments": []Attachment{
			{ID: 80, Filename: "guide.pdf", ContentURL: "https://synthetic.zendesk.com/attachments/80", Mimetype: "application/pdf", Size: 25},
			{ID: 81, Filename: "recording.wav", ContentURL: "https://synthetic.zendesk.com/attachments/81", Mimetype: "audio/wav", Size: 100},
		}, "meta": map[string]any{"has_more": false}})
	case strings.HasSuffix(request.URL.Path, "/articles/7.json"):
		if !authenticated && !fixture.public {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if authenticated {
			article.Title, article.Body, article.Draft = "Unpublished guidance", "<p>PRIVATE draft</p>", true
		}
		respond(t, writer, map[string]any{"article": article})
	default:
		t.Errorf("unexpected route: %s", request.URL.Path)
		writer.WriteHeader(http.StatusNotFound)
	}
}

func TestPublicArticleNeverUsesAuthenticatedPayloadOrAttachmentCache(t *testing.T) {
	ctx := context.Background()
	fixture := &articleFixture{public: true}
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) { fixture.serve(t, writer, request) })
	worker := testWorker(t, client)
	caughtUp, err := worker.SyncArticles(ctx)
	require.NoError(t, err)
	require.True(t, caughtUp)
	var body, raw string
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT body, raw::text FROM events WHERE source = 'zendesk' AND scope = 'public' AND NOT deleted`).Scan(&body, &raw))
	require.Contains(t, body, "Public answer")
	require.Contains(t, body, "Visibility: Public")
	require.NotContains(t, body+raw, "PRIVATE")
	require.NotContains(t, body+raw, "Unpublished")
	require.Contains(t, raw, "recording.wav")
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(DISTINCT source_file_id) FROM zendesk_files`).Scan(&count))
	require.Equal(t, 2, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM event_documents`).Scan(&count))
	require.Equal(t, 2, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE scope = 'corpus:zendesk' AND body LIKE '%PRIVATE%'`).Scan(&count))
	require.Equal(t, 1, count)
	var firstChunk int64
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT min(id) FROM event_chunks`).Scan(&firstChunk))
	require.NoError(t, worker.RefreshArticle(ctx, 7))
	var nextChunk int64
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT min(id) FROM event_chunks`).Scan(&nextChunk))
	require.Equal(t, firstChunk, nextChunk)
	fixture.public = false
	require.NoError(t, worker.RefreshArticle(ctx, 7))
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE scope = 'public' AND NOT deleted`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_files`).Scan(&count))
	require.Equal(t, 1, count)
	fixture.deleted = true
	complete, err := worker.Reconcile(ctx)
	require.NoError(t, err)
	require.True(t, complete)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE NOT deleted`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_files`).Scan(&count))
	require.Zero(t, count)
}

func TestAnonymousAttachmentDenialDoesNotFallBackToCredentials(t *testing.T) {
	ctx := context.Background()
	fixture := &articleFixture{public: true, publicFilesDenied: true}
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) { fixture.serve(t, writer, request) })
	worker := testWorker(t, client)
	require.NoError(t, worker.RefreshArticle(ctx, 7))
	require.Equal(t, 1, fixture.authenticatedFiles)
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_files f JOIN events e ON e.id = f.event_id WHERE e.scope = 'public'`).Scan(&count))
	require.Zero(t, count)
}

func TestArticleFailuresAreDurablyRetriedWithoutDeletingExistingData(t *testing.T) {
	ctx := context.Background()
	fixture := &articleFixture{public: true}
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) { fixture.serve(t, writer, request) })
	worker := testWorker(t, client)
	require.NoError(t, worker.RefreshArticle(ctx, 7))
	fixture.translationsFail = true
	_, err := worker.SyncArticles(ctx)
	require.NoError(t, err)
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_retry WHERE kind = 'article' AND source_id = 7`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE NOT deleted`).Scan(&count))
	require.Equal(t, 2, count)
	fixture.translationsFail = false
	_, err = worker.Pool.Exec(ctx, `UPDATE zendesk_retry SET not_before = now()`)
	require.NoError(t, err)
	require.NoError(t, worker.RetryDeferred(ctx))
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM zendesk_retry`).Scan(&count))
	require.Zero(t, count)
}

func TestArticlePaginationCannotRedirectCredentials(t *testing.T) {
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		respond(t, writer, map[string]any{"articles": []Article{}, "end_time": int64(1700000000), "next_page": "https://other.example/api/v2/help_center/incremental/articles.json?start_time=1700000000"})
	})
	worker := testWorker(t, client)
	_, err := worker.SyncArticles(context.Background())
	require.ErrorContains(t, err, "unexpected article pagination destination")
}

func TestArticleExportResumesZendeskPaginationAlias(t *testing.T) {
	var starts []string
	var client *Client
	client = testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		start := request.URL.Query().Get("start_time")
		starts = append(starts, start)
		if start == "0" {
			respond(t, writer, map[string]any{"articles": []Article{}, "end_time": 12, "next_page": client.origin + "/hc/api/v2/incremental/articles.json?start_time=12&per_page=100"})
			return
		}
		respond(t, writer, map[string]any{"articles": []Article{}, "end_time": 20, "next_page": nil})
	})
	worker := testWorker(t, client)
	caughtUp, err := worker.SyncArticles(context.Background())
	require.NoError(t, err)
	require.False(t, caughtUp)
	resumed := &Worker{Pool: worker.Pool, Sink: worker.Sink, Client: client}
	caughtUp, err = resumed.SyncArticles(context.Background())
	require.NoError(t, err)
	require.True(t, caughtUp)
	caughtUp, err = resumed.SyncArticles(context.Background())
	require.NoError(t, err)
	require.True(t, caughtUp)
	require.Equal(t, []string{"0", "12", "20"}, starts)
}

func TestArticleTranslationAndAttachmentPagingAndLocaleRemoval(t *testing.T) {
	removeFrench := false
	client := testClient(t, func(writer http.ResponseWriter, request *http.Request) {
		article := fixtureArticle()
		switch {
		case strings.HasSuffix(request.URL.Path, "translations.json"):
			translation := Translation{SourceID: 7, Locale: "en-us", Title: article.Title, Body: article.Body, UpdatedAt: article.UpdatedAt}
			more := !removeFrench && request.URL.Query().Get("page[after]") == ""
			if request.URL.Query().Get("page[after]") == "fr-next" {
				translation.Locale = "fr"
			}
			respond(t, writer, map[string]any{"translations": []Translation{translation}, "meta": map[string]any{"has_more": more, "after_cursor": "fr-next"}})
		case strings.HasSuffix(request.URL.Path, "attachments.json"):
			attachment := Attachment{ID: 1, Filename: "one.pdf", Mimetype: "application/pdf", ContentURL: "https://synthetic.zendesk.com/file/1", Size: 10}
			more := request.URL.Query().Get("page[after]") == ""
			if !more {
				attachment.ID, attachment.ContentURL = 2, "https://synthetic.zendesk.com/file/2"
			}
			respond(t, writer, map[string]any{"article_attachments": []Attachment{attachment}, "meta": map[string]any{"has_more": more, "after_cursor": "file-next"}})
		default:
			if strings.Contains(request.URL.Path, "/fr/") {
				article.Locale = "fr"
			}
			respond(t, writer, map[string]any{"article": article})
		}
	})
	worker := testWorker(t, client)
	ctx := context.Background()
	require.NoError(t, worker.RefreshArticle(ctx, 7))
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE NOT deleted`).Scan(&count))
	require.Equal(t, 2, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM event_documents`).Scan(&count))
	require.Equal(t, 4, count)
	removeFrench = true
	require.NoError(t, worker.RefreshArticle(ctx, 7))
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE NOT deleted`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM event_documents`).Scan(&count))
	require.Equal(t, 2, count)
}

func TestHTMLTextDropsScriptAndPreservesWords(t *testing.T) {
	text := htmlText(`<p>Hello &amp; goodbye</p><script>secret()</script><p>Next paragraph</p>`)
	require.Contains(t, text, "Hello & goodbye")
	require.Contains(t, text, "Next paragraph")
	require.NotContains(t, text, "secret")
}

func TestTicketCommentRemovalRemovesItsDerivedAttachments(t *testing.T) {
	worker := testWorker(t, testClient(t, func(writer http.ResponseWriter, request *http.Request) { t.Fatal("unexpected API read") }))
	comments := fixtureComments()
	comments[1].Attachments = []Attachment{{ID: 40, Filename: "image.png", Mimetype: "image/png", ContentURL: "https://synthetic.zendesk.com/attachments/40", Size: 12}}
	require.NoError(t, worker.storeTicket(context.Background(), fixtureTicket(), comments, nil))
	var count int
	require.NoError(t, worker.Pool.QueryRow(context.Background(), `SELECT count(*) FROM event_images`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, worker.storeTicket(context.Background(), fixtureTicket(), comments[:1], nil))
	require.NoError(t, worker.Pool.QueryRow(context.Background(), `SELECT count(*) FROM event_images`).Scan(&count))
	require.Zero(t, count)
	var scope string
	require.NoError(t, worker.Pool.QueryRow(context.Background(), `SELECT scope FROM events WHERE external_id = 'ticket:12:comment:21'`).Scan(&scope))
	require.Equal(t, events.ScopeZendesk, scope)
	var record Comment
	raw, err := json.Marshal(comments[0])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &record))
	require.Equal(t, fmt.Sprint(comments[0].ID), fmt.Sprint(record.ID))
}
