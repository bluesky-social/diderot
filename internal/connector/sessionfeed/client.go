// Package sessionfeed is the session feed connector: the client for a session
// feed's session-change, immutable Bsky Session v1 revision, range-expansion,
// and usage endpoints, and the connector's settings page.
package sessionfeed

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/safehttp"
)

// Client is a thin wrapper around a session feed's HTTP surface, which it
// calls with the bearer token the feed issued for Diderot. Goroutine-safe.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a client. baseURL is the feed's base URL
// (e.g. https://sessions.example.com). token is the bearer;
// httpClient is optional (default 60s timeout).
func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		// 60s is generous for the read endpoints; the chunks/summary
		// fetches should be sub-second, the sessions list is ~100
		// rows of metadata. If the feed is overloaded we'd rather
		// fail and retry than hang the puller.
		httpClient = safehttp.NewClient(safehttp.Options{Timeout: 60 * time.Second})
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    httpClient,
	}
}

// DiderotAskWorkNamespace is the work-reference namespace Diderot uses to
// query authoritative accounting for its own ask sessions. An ask UUID is a
// Diderot application work reference, never a canonical feed session; the
// work-usage endpoint separates those two identities.
const DiderotAskWorkNamespace = "diderot.ask"

// WorkReference is the requested accounting scope for a work-usage query.
// The namespace is resolved server-side by a registered adapter; neither
// field is a caller-chosen billing authority.
type WorkReference struct {
	Namespace   string `json:"namespace"`
	ReferenceID string `json:"reference_id"`
}

// Usage is the feed's authoritative versioned accounting response. Session
// usage identifies by SessionID; work usage identifies by WorkReference.
// Exactly one of the two is set on a well-formed response.
//
// Amounts are exact decimal strings, never binary floating-point totals;
// Diderot renders them verbatim and never prices tokens locally. Unknown
// billing and unpriced attempts are visible counts, not free usage, and
// CoverageComplete is false when gaps remain.
type Usage struct {
	SchemaVersion               int            `json:"schema_version"`
	SessionID                   string         `json:"session_id,omitempty"`
	WorkReference               *WorkReference `json:"work_reference,omitempty"`
	Currency                    string         `json:"currency"`
	MeteredCostUSD              string         `json:"metered_cost_usd"`
	InferenceCount              int64          `json:"inference_count"`
	AttemptCount                int64          `json:"attempt_count"`
	IncludedAttemptCount        int64          `json:"included_attempt_count"`
	UnknownBillingAttemptCount  int64          `json:"unknown_billing_attempt_count"`
	UnpricedMeteredAttemptCount int64          `json:"unpriced_metered_attempt_count"`
	InputTokens                 int64          `json:"input_tokens"`
	OutputTokens                int64          `json:"output_tokens"`
	CacheReadTokens             int64          `json:"cache_read_tokens"`
	CacheWriteTokens            int64          `json:"cache_write_tokens"`
	HasEstimates                bool           `json:"has_estimates"`
	CoverageComplete            bool           `json:"coverage_complete"`
	EffectiveRevision           string         `json:"effective_revision"`
	ObservedThrough             *time.Time     `json:"observed_through,omitempty"`
}

// GetSessionUsage returns the feed's authoritative accounting summary for one
// canonical session. The feed always echoes the requested session_id on
// success; a response naming a different session is treated as a proxy/
// caching fault and hard-fails.
func (c *Client) GetSessionUsage(ctx context.Context, sessionID string) (*Usage, error) {
	u := fmt.Sprintf("%s/api/v1/sessions/%s/usage", c.baseURL, url.PathEscape(sessionID))
	var out Usage
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, fmt.Errorf("get session usage: %w", err)
	}
	if out.SessionID != sessionID {
		return nil, fmt.Errorf("get session usage: response session_id %q does not match requested %q", out.SessionID, sessionID)
	}
	return &out, nil
}

// GetWorkUsage returns the feed's authoritative accounting summary for one
// work reference, keyed by (namespace, reference_id). Diderot uses the
// diderot.ask namespace for its ask-usage view. The feed echoes the requested
// scope; a mismatching work_reference is treated as a proxy/caching fault.
func (c *Client) GetWorkUsage(ctx context.Context, namespace, referenceID string) (*Usage, error) {
	u := fmt.Sprintf("%s/api/v1/work-usage/%s/%s",
		c.baseURL, url.PathEscape(namespace), url.PathEscape(referenceID))
	var out Usage
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, fmt.Errorf("get work usage: %w", err)
	}
	if out.WorkReference == nil ||
		out.WorkReference.Namespace != namespace ||
		out.WorkReference.ReferenceID != referenceID {
		return nil, fmt.Errorf("get work usage: response work_reference %+v does not match requested %s/%s",
			out.WorkReference, namespace, referenceID)
	}
	return &out, nil
}

// GetSessionRecordRange fetches a revision-pinned, half-open Bsky Session v1
// record range. The canonical session UUID is the whole content selector;
// revision pins the immutable document and [start,end) selects the records.
func (c *Client) GetSessionRecordRange(ctx context.Context, sessionID, revision string, start, end int) (string, error) {
	q := url.Values{}
	q.Set("revision", revision)
	q.Set("start", strconv.Itoa(start))
	q.Set("end", strconv.Itoa(end))
	u := fmt.Sprintf("%s/api/v1/sessions/%s/records/redacted?%s",
		c.baseURL, url.PathEscape(sessionID), q.Encode())
	body, err := c.getRaw(ctx, u)
	if err != nil {
		return "", fmt.Errorf("get session record range (session=%s [%d,%d)): %w",
			sessionID, start, end, err)
	}
	return string(body), nil
}

// getRaw is the bearer-auth path used by endpoints that return
// non-JSON bodies (transcript text, file dumps). Mirrors getJSON
// for HTTPError handling but skips JSON decode.
func (c *Client) getRaw(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	const maxBody = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			URL:        u,
			Body:       string(body),
		}
	}
	return body, nil
}

// getJSON is the shared bearer-auth + JSON-decode path. Surfaces
// non-2xx responses as an *HTTPError so callers can match status
// codes (404 on a deleted session, 401 on a bad token) without
// string-matching error messages.
func (c *Client) getJSON(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 16 MiB body cap — defensive bound on a misbehaving upstream.
	// Real responses are at most a few hundred KB (a session-level
	// narrative + paginated chunk list).
	const maxBody = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return &HTTPError{
			StatusCode: resp.StatusCode,
			URL:        u,
			Body:       string(body),
		}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode json: %w", err)
	}
	return nil
}

// HTTPError carries the non-2xx response status + body so callers
// can branch on specific codes (404 = session deleted, skip;
// 5xx = transient, retry; 401 = config bug, fail loud).
type HTTPError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *HTTPError) Error() string {
	preview := e.Body
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	return fmt.Sprintf("session feed http %d at %s: %s", e.StatusCode, e.URL, preview)
}

// ErrRevisionTooLarge marks a session revision whose size exceeds the
// caller's byte cap. Revisions are immutable, so the same revision can
// never succeed on retry; callers treat this as a permanent failure.
var ErrRevisionTooLarge = errors.New("session revision exceeds byte cap")

// IsNotFound reports whether err is an HTTPError with a 404 status.
// The puller uses this to skip sessions deleted between list-time
// and fetch-time without aborting the whole sweep.
func IsNotFound(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.StatusCode == http.StatusNotFound
}

// IsAuth reports a fleet-wide authentication or authorization failure. These
// statuses are not transient for ordinary request retries, but durable workers
// must retain their queue rows so token rotation or policy repair can recover.
func IsAuth(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.StatusCode == http.StatusUnauthorized || he.StatusCode == http.StatusForbidden
}

// IsTransient reports whether err looks worth retrying. 5xx, 408,
// 429 are transient; 4xx outside that range is a client/config
// bug we shouldn't retry through.
func IsTransient(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case http.StatusRequestTimeout,
			http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	// Network errors, ctx-cancel, etc. The caller's outer ctx
	// handles cancellation; for here we treat as transient since
	// a TCP reset mid-page is exactly the kind of thing a retry
	// fixes.
	return err != nil
}

// SessionChange is one row of the feed's change feed
// (GET /api/v1/session-changes). Every returned row is the LATEST
// descriptor for that session, not an intermediate event — several
// updates to one active session overwrite the same row, so consumers
// observe at most the latest state per change_seq (R12). Fields match
// the agreed wire shape verbatim so a wire change shows up as a
// json-tag diff rather than silent drift.
type SessionChange struct {
	ChangeSeq       int64     `json:"change_seq"`
	SessionID       string    `json:"session_id"`
	Owner           string    `json:"owner"`
	OwnerKind       string    `json:"owner_kind"`
	Client          string    `json:"client"`
	Role            string    `json:"role"`
	OriginApp       string    `json:"origin_app"`
	Revision        string    `json:"revision"`
	RewriteEpoch    int64     `json:"rewrite_epoch"`
	RecordCount     int       `json:"record_count"`
	UserTurnCount   int       `json:"user_turn_count"`
	ContentBytes    int64     `json:"content_bytes"`
	SourceStartedAt time.Time `json:"source_started_at"`
	SourceUpdatedAt time.Time `json:"source_updated_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Complete        bool      `json:"complete"`
	Deleted         bool      `json:"deleted"`
}

// SessionChangesPage is one ordered page of the change feed. The
// feed is a stateless ordered query (WHERE change_seq > after ORDER
// BY change_seq LIMIT n); consumers drain pages by repeatedly asking
// for changes after the highest change_seq they've committed.
//
// The feed always returns an explicit `next_cursor` (the highest
// change_seq in the page, or the requested `after` on an empty page).
// Consumers MUST advance their durable cursor from NextCursor, never
// by re-deriving it from local max values: an explicit cursor that
// skips or regresses is a contract violation and must be rejected.
type SessionChangesPage struct {
	Changes []SessionChange `json:"changes"`
	// NextCursor is the feed's explicit cursor for the NEXT page
	// (highest change_seq served, or the requested after when empty).
	NextCursor int64 `json:"next_cursor"`
}

// ListSessionChanges fetches one ordered page of the change feed
// returning the latest descriptor for every session whose change_seq
// is > after, ascending and capped at limit. after is diderot's OWN
// durable cursor — the feed keeps no consumer table, acknowledgement,
// lease, or cursor GC. A new consumer starts at after=0, which
// enumerates every current head before converging on live changes.
//
// Sequence gaps are harmless: ordering is by change_seq, which is
// monotonic. The page's explicit NextCursor is authoritative for the
// resume position and is validated below (it must never regress below
// the requested cursor or trail the greatest served change_seq).
func (c *Client) ListSessionChanges(ctx context.Context, after int64, limit int) (*SessionChangesPage, error) {
	q := url.Values{}
	if after > 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	u := c.baseURL + "/api/v1/session-changes"
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	var out SessionChangesPage
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, fmt.Errorf("list session changes: %w", err)
	}
	if err := validateChangesPage(&out, after); err != nil {
		return nil, err
	}
	return &out, nil
}

// validateChangesPage enforces the explicit next_cursor contract:
//   - NextCursor must never be lower than the requested after (a
//     regressing page would cause the consumer to replay forever or
//     skip work).
//   - NextCursor must be >= every served change_seq (an inconsistent
//     page that reports a cursor behind its own highest row would
//     re-deliver rows next page, a silent double-processing hazard).
//
// An omitted (zero) NextCursor on a non-empty page is treated as a
// contract violation: the feed must always state its cursor.
func validateChangesPage(p *SessionChangesPage, after int64) error {
	if len(p.Changes) == 0 {
		// Empty page: the feed echoes the requested cursor. A cursor
		// lower than `after` still cannot be valid (would regress).
		if p.NextCursor < after {
			return fmt.Errorf("session changes: empty page next_cursor %d regresses below requested after %d", p.NextCursor, after)
		}
		return nil
	}
	if p.NextCursor < after {
		return fmt.Errorf("session changes: next_cursor %d regresses below requested after %d", p.NextCursor, after)
	}
	for _, c := range p.Changes {
		if c.ChangeSeq > p.NextCursor {
			return fmt.Errorf("session changes: next_cursor %d trails served change_seq %d", p.NextCursor, c.ChangeSeq)
		}
	}
	return nil
}

// GetSessionRevision fetches the sanitized Bsky Session v1 revision
// document for one (session_id, revision) pair. The feed serves
// immutable redacted revisions; this is the ONLY content surface
// feed to diderot — never raw audit records. Returns the raw bytes
// (transparently gunzipped when the server encodes the body gzip).
//
// Body cap is 256 MiB: sessions can
// run into tens of MB after images inline, and the immutable revision
// NDJSON is a whole-document read, not a sliced range. The
// post-decompression read is bounded to maxRevisionBytes (defaulting
// to the transport cap) so a decompression bomb can't allocate an
// unbounded buffer before the worker's MaxRevisionBytes check runs.
func (c *Client) GetSessionRevisionBytes(ctx context.Context, sessionID, revision string, maxRevisionBytes int64) ([]byte, error) {
	u := fmt.Sprintf("%s/api/v1/sessions/%s/records/redacted",
		c.baseURL, url.PathEscape(sessionID))
	q := url.Values{}
	if revision != "" {
		q.Set("revision", revision)
	}
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/x-ndjson, application/gzip, application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	const maxBody int64 = 256 << 20
	readCap := maxBody
	if maxRevisionBytes > 0 && maxRevisionBytes < readCap {
		readCap = maxRevisionBytes
	}
	// Reject a declared over-cap body before reading it: an oversized
	// revision otherwise costs a full readCap download (and the transfer
	// time) just to learn it will be discarded.
	if resp.StatusCode < 400 && resp.ContentLength > readCap {
		return nil, fmt.Errorf("%w: session revision response declares %d bytes, exceeds %d-byte cap",
			ErrRevisionTooLarge, resp.ContentLength, readCap)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, readCap+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		const previewCap = 8 << 10
		preview := body
		if len(preview) > previewCap {
			preview = preview[:previewCap]
		}
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			URL:        u,
			Body:       string(preview),
		}
	}
	if int64(len(body)) > readCap {
		return nil, fmt.Errorf("%w: session revision response exceeds %d-byte cap", ErrRevisionTooLarge, readCap)
	}
	if isGzip(resp.Header.Get("Content-Encoding")) || hasGzipMagic(body) {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("decode gzip revision: %w", err)
		}
		defer func() { _ = zr.Close() }()
		cap := int64(maxBody)
		if maxRevisionBytes > 0 {
			cap = maxRevisionBytes
		}
		out, err := io.ReadAll(io.LimitReader(zr, cap+1))
		if err != nil {
			return nil, fmt.Errorf("decompress revision: %w", err)
		}
		if int64(len(out)) > cap {
			return nil, fmt.Errorf("%w: session revision decompressed to %d bytes, exceeds %d-byte cap", ErrRevisionTooLarge, len(out), cap)
		}
		return out, nil
	}
	return body, nil
}

// GetSessionRevision fetches the sanitized Bsky Session v1 revision
// document for one (session_id, revision) pair, defaulting the
// post-decompression cap to the client's transport cap (256 MiB).
func (c *Client) GetSessionRevision(ctx context.Context, sessionID, revision string) ([]byte, error) {
	return c.GetSessionRevisionBytes(ctx, sessionID, revision, 0)
}

func isGzip(enc string) bool {
	return strings.Contains(strings.ToLower(enc), "gzip")
}

func hasGzipMagic(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}
