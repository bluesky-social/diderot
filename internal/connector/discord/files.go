package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/safehttp"
)

// maxFileBytes bounds one attachment download.
const maxFileBytes = 25 << 20

// FileFetcher reads a message's attachments and embed images for the
// caption and document workers. Discord's attachment links are signed and
// expire, so it reads the message again for a fresh link each time, and it
// downloads only from Discord's own file hosts.
type FileFetcher struct {
	client *Client
	http   *http.Client
	// allowed says a download URL is one of Discord's file hosts.
	allowed func(*url.URL) bool
}

// NewFileFetcher returns the fetcher of client's attachments.
func NewFileFetcher(client *Client) *FileFetcher {
	return &FileFetcher{
		client: client,
		http: safehttp.NewClient(safehttp.Options{
			Timeout: time.Minute, PublicOnly: true, CheckRedirect: safehttp.MaxRedirects(3),
		}),
		allowed: discordFileHost,
	}
}

// discordFileHost reports whether u is an https URL on Discord's CDN or
// media proxy.
func discordFileHost(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "cdn.discordapp.com" || host == "media.discordapp.net" || strings.HasSuffix(host, ".discordapp.net")
}

// FetchImage is Fetch, for the caption worker.
func (f *FileFetcher) FetchImage(ctx context.Context, fileID, _ string) ([]byte, string, error) {
	return f.Fetch(ctx, fileID)
}

// Fetch downloads the file named "<channel>/<message>/<attachment>" or
// "<channel>/<message>/embed<n>".
func (f *FileFetcher) Fetch(ctx context.Context, fileID string) ([]byte, string, error) {
	parts := strings.Split(fileID, "/")
	if len(parts) != 3 {
		return nil, "", &files.PermanentError{Underlying: errors.New("discord: malformed file ID")}
	}
	m, err := f.client.message(ctx, parts[0], parts[1])
	if err != nil {
		if isStatus(err, 403, 404) {
			return nil, "", &files.PermanentError{Underlying: fmt.Errorf("discord: the message is gone or unreadable: %w", err)}
		}
		return nil, "", &files.TransientError{Underlying: fmt.Errorf("discord: read message: %w", err)}
	}
	location, mediaType := "", ""
	if index, ok := strings.CutPrefix(parts[2], "embed"); ok {
		n, err := strconv.Atoi(index)
		if err != nil || n < 0 || n >= len(m.Embeds) {
			return nil, "", &files.PermanentError{Underlying: errors.New("discord: the embed is gone")}
		}
		location = embedImage(m.Embeds[n])
	} else {
		for _, a := range m.Attachments {
			if a.ID == parts[2] {
				location, mediaType = a.URL, mediaTypeOf(a.ContentType)
			}
		}
	}
	if location == "" {
		return nil, "", &files.PermanentError{Underlying: errors.New("discord: the attachment is gone")}
	}
	parsed, err := url.Parse(location)
	if err != nil || !f.allowed(parsed) {
		return nil, "", &files.PermanentError{Underlying: errors.New("discord: attachment host not permitted")}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, "", &files.PermanentError{Underlying: errors.New("discord: invalid attachment request")}
	}
	request.Header.Set("User-Agent", userAgent)
	response, err := f.http.Do(request)
	if err != nil {
		var blocked *safehttp.BlockedAddressError
		if errors.As(err, &blocked) {
			return nil, "", &files.PermanentError{Underlying: errors.New("discord: attachment destination blocked")}
		}
		return nil, "", &files.TransientError{Underlying: errors.New("discord: attachment download failed")}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, "", files.ClassifyHTTPStatus(response.StatusCode, fmt.Errorf("discord: attachment HTTP %d", response.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxFileBytes+1))
	if err != nil {
		return nil, "", &files.TransientError{Underlying: errors.New("discord: incomplete attachment download")}
	}
	if len(body) > maxFileBytes {
		return nil, "", &files.PermanentError{Underlying: errors.New("discord: attachment exceeds size limit")}
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		mediaType = contentType
	}
	return body, mediaType, nil
}
