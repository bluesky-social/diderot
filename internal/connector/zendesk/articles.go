package zendesk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/net/html"

	"github.com/bluesky-social/diderot/internal/events"
)

type Article struct {
	ID          int64        `json:"id"`
	AuthorID    int64        `json:"author_id"`
	Title       string       `json:"title"`
	Body        string       `json:"body"`
	Locale      string       `json:"locale"`
	Draft       bool         `json:"draft"`
	Outdated    bool         `json:"outdated"`
	Labels      []string     `json:"label_names"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

type Translation struct {
	SourceID  int64     `json:"source_id"`
	Locale    string    `json:"locale"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Draft     bool      `json:"draft"`
	Outdated  bool      `json:"outdated"`
	UpdatedAt time.Time `json:"updated_at"`
}

type pageMeta struct {
	HasMore     *bool  `json:"has_more"`
	AfterCursor string `json:"after_cursor"`
}

func (client *Client) article(ctx context.Context, articleID int64, locale string, authenticated bool) (Article, error) {
	var response struct {
		Article Article `json:"article"`
	}
	path := fmt.Sprintf("/api/v2/help_center/%s/articles/%d.json", url.PathEscape(locale), articleID)
	err := client.get(ctx, path, nil, authenticated, &response)
	if err == nil && (response.Article.ID != articleID || response.Article.Locale != locale || response.Article.CreatedAt.IsZero()) {
		err = fmt.Errorf("%w: article identity or timestamp mismatch", errProtocol)
	}
	return response.Article, err
}

func (client *Client) translations(ctx context.Context, articleID int64) ([]Translation, error) {
	query := url.Values{"page[size]": {"100"}}
	var translations []Translation
	seen := make(map[string]bool)
	for {
		var page struct {
			Translations []Translation `json:"translations"`
			Meta         pageMeta      `json:"meta"`
		}
		if err := client.get(ctx, fmt.Sprintf("/api/v2/help_center/articles/%d/translations.json", articleID), query, true, &page); err != nil {
			return nil, err
		}
		if page.Translations == nil || page.Meta.HasMore == nil {
			return nil, fmt.Errorf("%w: missing translation pagination", errProtocol)
		}
		translations = append(translations, page.Translations...)
		if !*page.Meta.HasMore {
			return translations, nil
		}
		if page.Meta.AfterCursor == "" || seen[page.Meta.AfterCursor] {
			return nil, fmt.Errorf("%w: translation cursor did not advance", errProtocol)
		}
		seen[page.Meta.AfterCursor] = true
		query.Set("page[after]", page.Meta.AfterCursor)
	}
}

func (worker *Worker) SyncArticles(ctx context.Context) (bool, error) {
	var watermark int64
	var cursor string
	err := worker.Pool.QueryRow(ctx, `SELECT watermark, cursor FROM zendesk_sync WHERE tenant = $1 AND stream = 'articles'`, worker.Client.origin).Scan(&watermark, &cursor)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	query := url.Values{"start_time": {strconv.FormatInt(watermark, 10)}, "per_page": {"100"}}
	if cursor != "" {
		query, err = url.ParseQuery(cursor)
		if err != nil {
			return false, errors.New("zendesk: invalid article checkpoint")
		}
	}
	var page struct {
		Articles []Article `json:"articles"`
		EndTime  *int64    `json:"end_time"`
		NextPage string    `json:"next_page"`
	}
	const path = "/api/v2/help_center/incremental/articles.json"
	if err := worker.Client.get(ctx, path, query, true, &page); err != nil {
		return false, err
	}
	if page.Articles == nil || page.EndTime == nil || *page.EndTime < watermark {
		return false, fmt.Errorf("%w: missing article export pagination", errProtocol)
	}
	nextCursor := ""
	if page.NextPage != "" {
		next, err := url.Parse(page.NextPage)
		if err != nil || next.Scheme+"://"+next.Host != worker.Client.origin || (next.Path != path && next.Path != "/hc/api/v2/incremental/articles.json") || next.User != nil || next.Fragment != "" {
			return false, fmt.Errorf("%w: unexpected article pagination destination", errProtocol)
		}
		nextCursor = next.Query().Encode()
		if nextCursor == "" || nextCursor == query.Encode() {
			return false, fmt.Errorf("%w: article cursor did not advance", errProtocol)
		}
	}
	seen := make(map[int64]bool)
	for _, article := range page.Articles {
		if article.ID <= 0 {
			return false, fmt.Errorf("%w: article has no ID", errProtocol)
		}
		if seen[article.ID] {
			continue
		}
		seen[article.ID] = true
		if err := worker.RefreshArticle(ctx, article.ID); err != nil {
			if !deferTicketError(err) {
				return false, err
			}
			if err := worker.deferRecord(ctx, "article", article.ID); err != nil {
				return false, err
			}
		}
	}
	_, err = worker.Pool.Exec(ctx, `INSERT INTO zendesk_sync (tenant, stream, cursor, watermark)
		VALUES ($1, 'articles', $2, $3) ON CONFLICT (tenant, stream)
		DO UPDATE SET cursor = EXCLUDED.cursor, watermark = EXCLUDED.watermark, updated_at = now()`, worker.Client.origin, nextCursor, *page.EndTime)
	return page.NextPage == "", err
}

func (worker *Worker) RefreshArticle(ctx context.Context, articleID int64) error {
	translations, err := worker.Client.translations(ctx, articleID)
	if err != nil {
		var failure *APIError
		if errors.As(err, &failure) && (failure.Status == 404 || failure.Status == 410) {
			return worker.deleteArticle(ctx, articleID)
		}
		return err
	}
	keep := make([]string, 0, len(translations)*2)
	for _, translation := range translations {
		if translation.SourceID != articleID || translation.Locale == "" {
			return fmt.Errorf("%w: translation identity mismatch", errProtocol)
		}
		private, err := worker.Client.article(ctx, articleID, translation.Locale, true)
		if err != nil {
			return err
		}
		private.Title, private.Body = translation.Title, translation.Body
		private.Draft, private.Outdated = translation.Draft, translation.Outdated
		private.UpdatedAt = translation.UpdatedAt
		public, publicErr := worker.Client.article(ctx, articleID, translation.Locale, false)
		var failure *APIError
		if publicErr != nil {
			if !errors.As(publicErr, &failure) {
				return publicErr
			}
			switch failure.Status {
			case 401, 403, 404, 410, 302:
			default:
				return publicErr
			}
		}
		key := fmt.Sprintf("article:%d:%s", articleID, translation.Locale)
		if publicErr == nil && !public.Draft {
			keep = append(keep, key+":public")
			if err := worker.storeArticle(ctx, key+":public", public, events.ScopePublic); err != nil {
				return err
			}
			if private.Draft || private.Body != public.Body || private.Title != public.Title {
				keep = append(keep, key+":restricted")
				if err := worker.storeArticle(ctx, key+":restricted", private, events.ScopeZendesk); err != nil {
					return err
				}
			}
		} else {
			keep = append(keep, key+":restricted")
			if err := worker.storeArticle(ctx, key+":restricted", private, events.ScopeZendesk); err != nil {
				return err
			}
		}
	}
	if err := worker.deleteAbsent(ctx, fmt.Sprintf("article:%d:", articleID), "", keep); err != nil {
		return err
	}
	return worker.clearRecordRetry(ctx, "article", articleID)
}

func (worker *Worker) storeArticle(ctx context.Context, key string, article Article, scope string) error {
	attachments, err := worker.Client.articleAttachments(ctx, article, scope == events.ScopePublic)
	if err != nil {
		return err
	}
	article.Attachments = attachments
	raw, err := json.Marshal(article)
	if err != nil {
		return errProtocol
	}
	state := "Published"
	if article.Draft {
		state = "Draft"
	}
	visibility := "Restricted"
	if scope == events.ScopePublic {
		visibility = "Public"
	}
	body := fmt.Sprintf("%s\nLocale: %s\nVisibility: %s\n%s; outdated: %t\nUpdated: %s\n\n%s", article.Title, article.Locale, visibility, state, article.Outdated, article.UpdatedAt.Format(time.RFC3339), htmlText(article.Body))
	for _, attachment := range attachments {
		if !attachment.Deleted {
			body += fmt.Sprintf("\nAttachment: %s (%s, %d bytes)", attachment.Filename, attachment.Mimetype, attachment.Size)
		}
	}
	return worker.upsertWithAttachments(ctx, events.Event{
		Source: "zendesk", ExternalID: key, ThreadID: fmt.Sprintf("article:%d:%s", article.ID, article.Locale),
		Scope: scope, Title: article.Title, Body: body, Raw: raw, OccurredAt: article.CreatedAt,
		ActorID: strconv.FormatInt(article.AuthorID, 10),
		URL:     fmt.Sprintf("%s/hc/%s/articles/%d", worker.Client.origin, url.PathEscape(article.Locale), article.ID),
	}, attachments)
}

func (worker *Worker) deleteAbsent(ctx context.Context, prefix, threadID string, keep []string) error {
	if keep == nil {
		keep = []string{}
	}
	query := `SELECT external_id FROM events WHERE source = 'zendesk'
		AND starts_with(external_id, $1) AND NOT (external_id = ANY($2::text[])) AND NOT deleted`
	args := []any{prefix, keep}
	if threadID != "" {
		query += " AND thread_id = $3"
		args = append(args, threadID)
	}
	rows, err := worker.Pool.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, externalID := range ids {
		if err := worker.deleteEvent(ctx, externalID); err != nil {
			return err
		}
	}
	return nil
}

func (worker *Worker) deleteArticle(ctx context.Context, articleID int64) error {
	if err := worker.deleteAbsent(ctx, fmt.Sprintf("article:%d:", articleID), "", nil); err != nil {
		return err
	}
	return worker.clearRecordRetry(ctx, "article", articleID)
}

func htmlText(body string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(body))
	var text strings.Builder
	skip := false
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return strings.TrimSpace(text.String())
		case html.StartTagToken, html.EndTagToken:
			token := tokenizer.Token()
			if token.Data == "script" || token.Data == "style" {
				skip = token.Type == html.StartTagToken
			}
			if token.Data == "p" || token.Data == "br" || token.Data == "div" || token.Data == "li" || strings.HasPrefix(token.Data, "h") {
				text.WriteByte('\n')
			}
		case html.TextToken:
			if !skip {
				text.WriteString(string(tokenizer.Text()))
			}
		}
	}
}
