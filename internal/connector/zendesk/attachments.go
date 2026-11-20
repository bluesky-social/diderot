package zendesk

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/safehttp"
)

func (client *Client) articleAttachments(ctx context.Context, article Article, public bool) ([]Attachment, error) {
	query := url.Values{"page[size]": {"100"}}
	seen := make(map[string]bool)
	var attachments []Attachment
	for {
		var page struct {
			Attachments []Attachment `json:"article_attachments"`
			Meta        pageMeta     `json:"meta"`
		}
		path := fmt.Sprintf("/api/v2/help_center/%s/articles/%d/attachments.json", url.PathEscape(article.Locale), article.ID)
		if err := client.get(ctx, path, query, !public, &page); err != nil {
			var failure *APIError
			if public && errors.As(err, &failure) && (failure.Status == 401 || failure.Status == 403 || failure.Status == 404) {
				return nil, nil
			}
			return nil, err
		}
		if page.Attachments == nil || page.Meta.HasMore == nil {
			return nil, fmt.Errorf("%w: missing attachment pagination", errProtocol)
		}
		attachments = append(attachments, page.Attachments...)
		if !*page.Meta.HasMore {
			return attachments, nil
		}
		if page.Meta.AfterCursor == "" || seen[page.Meta.AfterCursor] {
			return nil, fmt.Errorf("%w: attachment cursor did not advance", errProtocol)
		}
		seen[page.Meta.AfterCursor] = true
		query.Set("page[after]", page.Meta.AfterCursor)
	}
}

func (worker *Worker) upsertWithAttachments(ctx context.Context, event events.Event, attachments []Attachment) error {
	var images []events.ImageRef
	var documents []events.DocumentRef
	var fileIDs []string
	sideWrite := func(ctx context.Context, tx pgx.Tx, eventID int64) error {
		for _, attachment := range attachments {
			if attachment.Deleted || attachment.ID <= 0 || attachment.ContentURL == "" {
				continue
			}
			mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(attachment.Mimetype, ";", 2)[0]))
			identity := fmt.Sprintf("%s|%s|%s|%d|%s|%d", worker.Client.origin, event.ExternalID, event.Scope, attachment.ID, attachment.ContentURL, attachment.Size)
			fileID := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
			var imageRefs []events.ImageRef
			if files.CaptionableImage(mediaType) {
				imageRefs = []events.ImageRef{{SourceImageID: fileID, Mimetype: mediaType}}
			}
			format := events.DocFormatForMediaType(mediaType, attachment.Filename)
			if len(imageRefs) == 0 && format == "" {
				continue
			}
			images = append(images, imageRefs...)
			if format != "" {
				documents = append(documents, events.DocumentRef{SourceFileID: fileID, Filename: attachment.Filename, Mimetype: mediaType, DocFormat: format})
			}
			fileIDs = append(fileIDs, fileID)
			_, err := tx.Exec(ctx, `INSERT INTO zendesk_files (source_file_id, event_id, content_url, media_type)
				VALUES ($1, $2, $3, $4) ON CONFLICT (source_file_id)
				DO UPDATE SET content_url = EXCLUDED.content_url, media_type = EXCLUDED.media_type`, fileID, eventID, events.SanitizeText(attachment.ContentURL), mediaType)
			if err != nil {
				return err
			}
		}
		return nil
	}
	result, err := worker.Sink.UpsertWithSideWrite(ctx, event, sideWrite)
	if err != nil {
		return err
	}
	if !result.WasUpdate && !result.NoChange && len(attachments) == 0 {
		return nil
	}
	if err := events.ReconcileImages(ctx, worker.Pool, result.EventID, images); err != nil {
		return err
	}
	if err := events.ReconcileDocuments(ctx, worker.Pool, result.EventID, documents); err != nil {
		return err
	}
	if fileIDs == nil {
		fileIDs = []string{}
	}
	_, err = worker.Pool.Exec(ctx, `DELETE FROM zendesk_files WHERE event_id = $1 AND NOT (source_file_id = ANY($2::text[]))`, result.EventID, fileIDs)
	return err
}

func (worker *Worker) deleteEvent(ctx context.Context, externalID string) error {
	result, err := worker.Sink.Delete(ctx, "zendesk", externalID)
	if err != nil || !result.Existed {
		return err
	}
	if err := events.ReconcileImages(ctx, worker.Pool, result.EventID, nil); err != nil {
		return err
	}
	if err := events.ReconcileDocuments(ctx, worker.Pool, result.EventID, nil); err != nil {
		return err
	}
	_, err = worker.Pool.Exec(ctx, `DELETE FROM zendesk_files WHERE event_id = $1`, result.EventID)
	return err
}

type FileFetcher struct {
	pool       *pgxpool.Pool
	client     *Client
	httpClient *http.Client
}

func NewFileFetcher(pool *pgxpool.Pool, client *Client) *FileFetcher {
	httpClient := safehttp.NewClient(safehttp.Options{
		Timeout: time.Minute, PublicOnly: true,
		CheckRedirect: func(request *http.Request, previous []*http.Request) error {
			request.Header.Del("Authorization")
			request.Header.Del("Cookie")
			if len(previous) >= 5 || !safeFileURL(request.URL) {
				return errors.New("zendesk: unsafe attachment redirect")
			}
			return nil
		},
	})
	return &FileFetcher{pool: pool, client: client, httpClient: httpClient}
}

func safeFileURL(location *url.URL) bool {
	return location.Scheme == "https" && location.Hostname() != "" && location.User == nil && (location.Port() == "" || location.Port() == "443")
}

func (fetcher *FileFetcher) FetchImage(ctx context.Context, fileID, _ string) ([]byte, string, error) {
	return fetcher.Fetch(ctx, fileID)
}

func (fetcher *FileFetcher) Fetch(ctx context.Context, fileID string) ([]byte, string, error) {
	var location, mediaType, scope string
	err := fetcher.pool.QueryRow(ctx, `SELECT f.content_url, f.media_type, e.scope FROM zendesk_files f
		JOIN events e ON e.id = f.event_id WHERE f.source_file_id = $1 AND e.source = 'zendesk' AND NOT e.deleted`, fileID).Scan(&location, &mediaType, &scope)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", &files.PermanentError{Underlying: errors.New("zendesk: attachment no longer indexed")}
		}
		return nil, "", &files.TransientError{Underlying: errors.New("zendesk: attachment lookup failed")}
	}
	parsed, err := url.Parse(location)
	if err != nil || !safeFileURL(parsed) || (scope != events.ScopePublic && scope != events.ScopeZendesk) {
		return nil, "", &files.PermanentError{Underlying: errors.New("zendesk: attachment URL or scope not permitted")}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, "", &files.PermanentError{Underlying: errors.New("zendesk: invalid attachment request")}
	}
	authenticated := scope == events.ScopeZendesk && parsed.Scheme+"://"+parsed.Host == fetcher.client.origin
	response, err := fetcher.client.do(request, fetcher.httpClient, authenticated)
	if err != nil {
		var blocked *safehttp.BlockedAddressError
		if errors.As(err, &blocked) {
			return nil, "", &files.PermanentError{Underlying: errors.New("zendesk: attachment destination blocked")}
		}
		return nil, "", &files.TransientError{Underlying: errors.New("zendesk: attachment download failed")}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		failure := fmt.Errorf("zendesk: attachment HTTP %d", response.StatusCode)
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == 429 || response.StatusCode >= 500 {
			return nil, "", &files.TransientError{Underlying: failure}
		}
		return nil, "", &files.PermanentError{Underlying: failure}
	}
	const maximumBytes = 25 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	if err != nil {
		return nil, "", &files.TransientError{Underlying: errors.New("zendesk: incomplete attachment download")}
	}
	if len(body) > maximumBytes {
		return nil, "", &files.PermanentError{Underlying: errors.New("zendesk: attachment exceeds size limit")}
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		mediaType = contentType
	}
	return body, mediaType, nil
}
