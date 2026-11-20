package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/safehttp"
)

// limiterMethodFilesInfo + limiterMethodFilesGet are the metric
// labels the Limiter tags Slack file traffic with. They don't
// match any slack-go SDK call exactly (we route the bytes fetch
// through net/http directly because the SDK doesn't expose a
// streaming url_private getter), but staying in the same label
// space keeps the dashboard panels honest.
const (
	limiterMethodFilesInfo = "files.info"
	limiterMethodFilesGet  = "files.get"
)

// FileFetcherClient is the slack-go subset NewFileFetcher needs.
// Just files.info, which gives us the canonical url_private +
// authoritative size/mimetype.
type FileFetcherClient interface {
	GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slack.File, []slack.Comment, *slack.Paging, error)
}

// NewFileFetcher wires the Slack fetcher of the attachment workers,
// which:
//  1. calls files.info for the canonical url_private + mimetype,
//  2. GETs url_private with the bot token via net/http,
//
// both routed through the diderot Slack rate Limiter so the file
// traffic shares the workspace token budget with conversations.* /
// users.* calls.
//
// botToken is required (xoxb-...). httpClient may be nil; a sane
// default with a 60-second timeout is used in that case (file
// downloads are dominated by Slack's CDN, not the API).
func NewFileFetcher(
	api FileFetcherClient,
	limiter *Limiter,
	botToken string,
	httpClient *http.Client,
	logger *slog.Logger,
) *fileFetcher {
	if httpClient == nil {
		httpClient = safehttp.NewClient(safehttp.Options{Timeout: 60 * time.Second})
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &fileFetcher{
		api:     api,
		limiter: limiter,
		token:   botToken,
		http:    httpClient,
		logger:  logger,
	}
}

type fileFetcher struct {
	api     FileFetcherClient
	limiter *Limiter
	token   string
	http    *http.Client
	logger  *slog.Logger
}

// FetchImage resolves url_private via files.info, then GETs the
// bytes with the bot token. Both requests go through
// WithSlackRetry so the Limiter sees them. Implements
// files.SourceFetcher.
func (s *fileFetcher) FetchImage(ctx context.Context, fileID string, _ string) ([]byte, string, error) {
	if s.token == "" {
		return nil, "", errors.New("files: bot token required for FetchImage")
	}

	var info *slack.File
	err := WithSlackRetry(ctx, s.limiter, limiterMethodFilesInfo, func() error {
		f, _, _, err := s.api.GetFileInfoContext(ctx, fileID, 1, 1)
		if err != nil {
			return err
		}
		info = f
		return nil
	})
	if err != nil {
		return nil, "", classifySlackError(fmt.Errorf("files.info %s: %w", fileID, err))
	}
	if info == nil || info.URLPrivate == "" {
		return nil, "", fmt.Errorf("files.info %s: no url_private", fileID)
	}

	// GET url_private with the bot token. The Slack CDN accepts
	// "Authorization: Bearer xoxb-..." even though it's served
	// off slack-files.com.
	var (
		data []byte
		ct   string
	)
	err = WithSlackRetry(ctx, s.limiter, limiterMethodFilesGet, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URLPrivate, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		resp, err := s.http.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			// Surface as a slack-style RateLimitedError so
			// WithSlackRetry honors Retry-After. slack-go's
			// RateLimitedError takes a duration; default to
			// 5s if the header is missing or unparseable.
			retry := 5 * time.Second
			if h := resp.Header.Get("Retry-After"); h != "" {
				if d, perr := time.ParseDuration(h + "s"); perr == nil {
					retry = d
				}
			}
			return &slack.RateLimitedError{RetryAfter: retry}
		}
		if resp.StatusCode != http.StatusOK {
			return files.ClassifyHTTPStatus(resp.StatusCode,
				fmt.Errorf("url_private %s: status %d", fileID, resp.StatusCode))
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		data = body
		ct = resp.Header.Get("Content-Type")
		return nil
	})
	if err != nil {
		return nil, "", classifySlackError(fmt.Errorf("url_private %s: %w", fileID, err))
	}
	if ct == "" && info.Mimetype != "" {
		ct = info.Mimetype
	}
	s.logger.Debug("files: fetched bytes",
		"file_id", fileID, "bytes", len(data), "content_type", ct)
	return data, ct, nil
}

// Fetch satisfies files.Fetcher for the document worker. FetchImage is
// the files.SourceFetcher spelling the caption worker calls; both names
// route to the same body, which is content-type agnostic.
func (s *fileFetcher) Fetch(ctx context.Context, sourceFileID string) ([]byte, string, error) {
	return s.FetchImage(ctx, sourceFileID, "")
}

// classifySlackError wraps Slack-side errors as TransientError when
// the cause looks transient: rate limits (slack-go's RateLimitedError),
// 5xx HTTP responses, and network-level timeouts/refusals. Other
// errors pass through unchanged.
func classifySlackError(err error) error {
	if err == nil {
		return nil
	}
	var rate *slack.RateLimitedError
	if errors.As(err, &rate) {
		return &files.TransientError{Underlying: err}
	}
	if files.IsNetworkError(err) {
		return &files.TransientError{Underlying: err}
	}
	return err
}
