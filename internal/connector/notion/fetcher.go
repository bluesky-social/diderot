package notion

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	files "github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/safehttp"
)

// maxImageBytes bounds both the data: URI decode and the HTTP GET
// path. A hostile external URL could serve an infinite stream and
// OOM the worker; a malformed/huge data URI has the same risk
// without ever making an HTTP request. Matches what a captioner
// can realistically handle (hosted vision models accept up to roughly
// 20MB), with headroom over that ceiling without trusting the remote.
const maxImageBytes = 25 * 1024 * 1024

// Fetcher resolves notion image bytes for the captioner worker.
// Implements files.SourceFetcher; the Notion source lends it to the
// caption worker while it runs (source.Attachments.LendImages).
//
// Per-fetch flow:
//
//  1. Split source_image_id into (block_id, contentKey) via
//     SplitSourceImageID.
//  2. notion.Client.GetBlock(block_id) → fresh signed URL
//     (type=file) or stable external URL (type=external).
//  3. Verify the freshly-computed contentKey matches the suffix.
//     Mismatch → TransientError (see design doc § "Image edits
//     within a stable block"); five retries later the worker
//     poisons the row, the next walker tick creates a new row
//     with the correct contentKey.
//  4. HTTP GET the bytes via the SSRF-safe http.Client.
//
// Two HTTP clients live on the Fetcher:
//
//   - api: notion.Client used for blocks.retrieve; rate limiter
//     shared with the rest of the connector.
//   - bytesClient: hits arbitrary hosts (S3 for type=file,
//     third-party for type=external) using a restricted dialer
//     that blocks RFC1918, loopback, link-local, CGNAT (100.64.0.0/10),
//     0.0.0.0/8, etc. Required to mitigate the SSRF risk
//     introduced by accepting author-controlled URLs from
//     type=external blocks.
type Fetcher struct {
	api         *Client
	bytesClient *http.Client
	logger      *slog.Logger
}

// NewFetcher constructs a Fetcher.
//
// api is the notion.Client used for blocks.retrieve; its rate
// limiter governs API call cadence, shared with the walker.
// bytesClient is the SSRF-safe HTTP client used for the bytes
// download; if nil, NewFetcher builds a PublicOnly one with a 60s
// timeout.
func NewFetcher(api *Client, bytesClient *http.Client, logger *slog.Logger) *Fetcher {
	if logger == nil {
		logger = slog.Default()
	}
	if bytesClient == nil {
		bytesClient = safehttp.NewClient(safehttp.Options{
			Timeout:    60 * time.Second,
			PublicOnly: true,
			// Each redirect target is dialed through the same
			// blocklist; what the cap adds is a defense-in-depth
			// bound against a public redirect that leads back
			// inside the trust boundary at the HTTP layer (e.g. a
			// public domain that CNAMEs back to internal).
			CheckRedirect: safehttp.MaxRedirects(5),
		})
	}
	return &Fetcher{api: api, bytesClient: bytesClient, logger: logger}
}

// FetchImage implements files.SourceFetcher. sourceImageID is the
// composite "<block_id>:<contentKey>" emitted by ExtractImages.
func (f *Fetcher) FetchImage(ctx context.Context, sourceImageID string, _ string) ([]byte, string, error) {
	blockID, wantContentKey, ok := SplitSourceImageID(sourceImageID)
	if !ok || blockID == "" || wantContentKey == "" {
		return nil, "", fmt.Errorf("notion: malformed source_image_id %q", sourceImageID)
	}

	block, err := f.api.GetBlock(ctx, blockID)
	if err != nil {
		return nil, "", classifyNotionAPIErr(fmt.Errorf("blocks.retrieve %s: %w", blockID, err))
	}
	if block.Type != "image" {
		// Block was edited from an image to something else; the
		// row is now stale. Treat as terminal — next walker tick
		// will reconcile via DELETE.
		return nil, "", fmt.Errorf("notion: block %s is now type=%q, not image", blockID, block.Type)
	}

	gotContentKey, ok := ContentKeyForBlock(block)
	if !ok {
		return nil, "", fmt.Errorf("notion: block %s has no recoverable image url", blockID)
	}
	if gotContentKey != wantContentKey {
		// contentKey mismatch: either Notion API consistency lag
		// (search returned the walker an updated state but
		// blocks.retrieve hasn't caught up yet — retry will
		// resolve), or the author actively edited between walker
		// and worker (next walker tick will INSERT a new row
		// with the correct contentKey, original row poisons after
		// maxTransientRetries=5). Either way: transient.
		return nil, "", &files.TransientError{
			Underlying: fmt.Errorf("notion: contentKey mismatch (want=%s got=%s)",
				wantContentKey, gotContentKey),
		}
	}

	rawURL, isNotionFile, err := imageURLFromBlock(block)
	if err != nil {
		return nil, "", err
	}

	// data: URIs carry the bytes inline — no HTTP round trip, and
	// no SSRF surface since nothing is fetched. Decode directly. A
	// malformed data: URI (bad base64, non-base64 payload, oversized)
	// is terminal — the block's payload doesn't change on retry —
	// so wrap as *PermanentError to jump straight to the poison cap.
	if data, ct, ok, err := decodeDataURI(rawURL); ok {
		if err != nil {
			return nil, "", &files.PermanentError{
				Underlying: fmt.Errorf("notion data uri %s: %w", blockID, err),
			}
		}
		f.logger.Debug("notion: decoded data uri",
			"block_id", blockID, "bytes", len(data), "content_type", ct)
		return data, ct, nil
	}

	// Relative/schemeless URLs (and any non-http(s) scheme, e.g. a
	// stray ftp:// URL) have nothing this fetcher can retrieve —
	// bytesClient only ever succeeds against http(s). Permanent:
	// retrying won't produce a different result, so jump straight
	// to the poison cap instead of burning 5 transient-retry cycles.
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" ||
		(!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
		return nil, "", &files.PermanentError{
			Underlying: fmt.Errorf("notion: block %s has an unfetchable image url %q", blockID, rawURL),
		}
	}

	// Bytes GET via the SSRF-safe client. A Notion-hosted file URL can expire
	// between blocks.retrieve and this request; on 403/404 refresh the block
	// once and retry with the new signature.
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, "", fmt.Errorf("build request: %w", err)
		}
		resp, err = f.bytesClient.Do(req)
		if err != nil {
			return nil, "", classifyHTTPErr(fmt.Errorf("notion bytes %s: %w", blockID, err))
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		_ = resp.Body.Close()
		if attempt == 0 && isNotionFile && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
			refreshed, refreshErr := f.api.GetBlock(ctx, blockID)
			if refreshErr != nil {
				return nil, "", classifyNotionAPIErr(fmt.Errorf("refresh blocks.retrieve %s: %w", blockID, refreshErr))
			}
			refreshedKey, ok := ContentKeyForBlock(refreshed)
			if !ok || refreshedKey != wantContentKey {
				return nil, "", &files.TransientError{Underlying: fmt.Errorf("notion: refreshed contentKey mismatch (want=%s got=%s)", wantContentKey, refreshedKey)}
			}
			var refreshedIsFile bool
			rawURL, refreshedIsFile, refreshErr = imageURLFromBlock(refreshed)
			if refreshErr != nil {
				return nil, "", refreshErr
			}
			isNotionFile = refreshedIsFile
			continue
		}
		return nil, "", classifyHTTPStatus(resp.StatusCode,
			fmt.Errorf("notion bytes %s: status %d", blockID, resp.StatusCode))
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		return nil, "", &files.TransientError{Underlying: fmt.Errorf("notion bytes %s: signed URL refresh exhausted", blockID)}
	}
	defer func() { _ = resp.Body.Close() }()
	// Cap body read to bound memory: a hostile external URL
	// could serve an infinite stream and OOM the worker. Cap
	// matches what a captioner can realistically handle: hosted
	// vision models accept images of roughly 5 to 20MB. 25MB gives
	// headroom over that without trusting the remote.
	limited := http.MaxBytesReader(nil, resp.Body, maxImageBytes)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", classifyHTTPErr(fmt.Errorf("read body %s: %w", blockID, err))
	}
	ct := resp.Header.Get("Content-Type")
	f.logger.Debug("notion: fetched bytes",
		"block_id", blockID, "bytes", len(data), "content_type", ct)
	return data, ct, nil
}

// decodeDataURI decodes a "data:<mediatype>;base64,<payload>" URI
// directly, with no HTTP round trip. Returns ok=false when rawURL
// isn't a data: URI at all (the normal case — caller falls through
// to the HTTP path). When ok=true and err is non-nil, rawURL WAS a
// data: URI but malformed (non-base64, or bad base64 payload) —
// terminal, not worth retrying.
func decodeDataURI(rawURL string) (data []byte, mediatype string, ok bool, err error) {
	// URI schemes are case-insensitive per RFC 3986 §3.1 — "DATA:..."
	// is just as valid as "data:...". The later http(s)-scheme check
	// uses EqualFold for the same reason; match that here so an
	// uppercase-scheme data URI doesn't fall through and get
	// permanently poisoned as an unfetchable non-http(s) URL.
	const prefix = "data:"
	if len(rawURL) < len(prefix) || !strings.EqualFold(rawURL[:len(prefix)], prefix) {
		return nil, "", false, nil
	}
	rest := rawURL[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return nil, "", true, errors.New("data uri: no comma separator")
	}
	meta, payload := rest[:comma], rest[comma+1:]
	// MIME transfer-encoding tokens are case-insensitive (RFC 2045
	// §6.1) — "data:image/png;BASE64,..." is just as valid as the
	// lowercase form. Match on the last ";"-delimited segment with
	// EqualFold rather than a literal HasSuffix so an uppercase or
	// mixed-case marker doesn't misclassify as non-base64 and get
	// permanently poisoned.
	semi := strings.LastIndexByte(meta, ';')
	if semi < 0 || !strings.EqualFold(meta[semi+1:], "base64") {
		// Non-base64 data URIs (percent-encoded text payloads) are
		// not a shape we expect for image bytes — treat as
		// unsupported rather than guessing at a decode.
		return nil, "", true, fmt.Errorf("data uri: non-base64 payload (mediatype=%q)", meta)
	}
	mediatype = meta[:semi]
	if mediatype == "" {
		mediatype = "application/octet-stream"
	}
	if len(payload) > (maxImageBytes*4)/3+4 {
		return nil, "", true, fmt.Errorf("data uri: payload exceeds %d byte cap", maxImageBytes)
	}
	decoded, decErr := base64.StdEncoding.DecodeString(payload)
	if decErr != nil {
		return nil, "", true, fmt.Errorf("data uri: base64 decode: %w", decErr)
	}
	if len(decoded) > maxImageBytes {
		return nil, "", true, fmt.Errorf("data uri: decoded payload exceeds %d byte cap", maxImageBytes)
	}
	return decoded, mediatype, true, nil
}

// imageURLFromBlock returns the URL from a freshly-retrieved
// image block.
func imageURLFromBlock(b *Block) (string, bool, error) {
	raw, ok := b.Payload["image"]
	if !ok {
		return "", false, fmt.Errorf("notion: block %s has no image payload", b.ID)
	}
	var body struct {
		Type string `json:"type"`
		File *struct {
			URL string `json:"url"`
		} `json:"file,omitempty"`
		External *struct {
			URL string `json:"url"`
		} `json:"external,omitempty"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", false, fmt.Errorf("notion: parse image payload: %w", err)
	}
	switch {
	case body.File != nil && body.File.URL != "":
		return body.File.URL, true, nil
	case body.External != nil && body.External.URL != "":
		return body.External.URL, false, nil
	}
	return "", false, fmt.Errorf("notion: block %s has no image url", b.ID)
}

// classifyNotionAPIErr classifies a notion.Client.Do error
// (blocks.retrieve, etc.) as transient or permanent. Per design
// doc:
//
//   - 401/403: TransientError (token rotation is operator-
//     recoverable; poisoning would be silent data loss).
//   - 429 / 5xx / network: TransientError.
//   - 404 / other: pass through; worker's bumpFailure handles
//     ("file_not_found"-class permanents land via
//     isPermanentTerminal in the attachments/files package).
//   - everything else: pass-through.
func classifyNotionAPIErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case containsStatus(msg, 401), containsStatus(msg, 403),
		containsStatus(msg, 429),
		containsStatus(msg, 500), containsStatus(msg, 502),
		containsStatus(msg, 503), containsStatus(msg, 504):
		return &files.TransientError{Underlying: err}
	}
	if strings.Contains(msg, "rate_limited") || strings.Contains(msg, "rate limit") {
		return &files.TransientError{Underlying: err}
	}
	if isNetworkErr(err) {
		return &files.TransientError{Underlying: err}
	}
	return err
}

// containsStatus checks whether the error string contains an HTTP
// status code as a standalone token. Defensive against false
// positives where a different number happens to appear in the
// message — wrap it with spaces.
func containsStatus(msg string, code int) bool {
	tok := fmt.Sprintf(" %d", code)
	return strings.Contains(msg, tok)
}

// classifyHTTPErr handles errors from the bytes-GET path
// (network blip, dial refused, etc.).
func classifyHTTPErr(err error) error {
	if err == nil {
		return nil
	}
	if isNetworkErr(err) {
		return &files.TransientError{Underlying: err}
	}
	return err
}

// classifyHTTPStatus wraps a status-code-bearing error as
// transient for 5xx / 429. 4xx other than 429 is terminal —
// signed URL has gone bad, but the contentKey verification step
// above is the honest race-detector. We don't transient-loop on
// 4xx because a permanently-bad URL would burn worker capacity
// indefinitely.
func classifyHTTPStatus(status int, err error) error {
	if err == nil {
		return nil
	}
	if status == http.StatusTooManyRequests || (status >= 500 && status < 600) {
		return &files.TransientError{Underlying: err}
	}
	return err
}

// isNetworkErr detects timeouts and other "connection didn't
// make it" failures.
func isNetworkErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "no such host"):
		return true
	}
	return false
}

// _ time.Duration ensures the import stays used after future
// edits even if the constructor accepts the timeout differently.
var _ time.Duration
