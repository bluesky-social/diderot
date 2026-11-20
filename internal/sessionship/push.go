package sessionship

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/client"
	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

// Pusher sends session documents to a Diderot server's push route.
type Pusher struct {
	// MCPURL is the server URL setup saved; the push route is beside it.
	MCPURL string
	HTTP   *http.Client
	// Authorization returns the Authorization header value, or "".
	Authorization func(ctx context.Context) (string, error)
}

// RejectedError means the server will not take this document, and sending
// the same bytes again cannot change that.
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return e.Reason }

// BusyError means the server asked to be tried again later.
type BusyError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("the server is busy (HTTP %d)", e.Status)
}

// pushRoute is where sessions are pushed; a session's ID follows it.
const pushRoute = "/v1/ingest/agent-sessions/"

// ServerURL is a page or route of the server whose MCP endpoint is mcpURL,
// such as /admin/settings/models.
func ServerURL(mcpURL, route string) (string, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return "", err
	}
	route, query, _ := strings.Cut(route, "?")
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/mcp") + route
	u.RawQuery = query
	return u.String(), nil
}

// pushResponse is the JSON the server's session routes answer with.
type pushResponse struct {
	Revision           string `json:"revision"`
	Error              string `json:"error"`
	Detail             string `json:"detail"`
	CurrentRevision    string `json:"current_revision"`
	BodyMatchesCurrent bool   `json:"body_matches_current"`
	Limit              int64  `json:"limit"`
	// Sessions is the account's own sessions (GET /v1/me/agent-sessions).
	Sessions []struct {
		Indexed       bool `json:"indexed"`
		SummaryFailed bool `json:"summary_failed"`
		Excluded      bool `json:"excluded"`
		Deleted       bool `json:"deleted"`
	} `json:"sessions"`
}

func (p *Pusher) do(ctx context.Context, method, route string, header http.Header, body []byte) (int, http.Header, pushResponse, error) {
	target, err := ServerURL(p.MCPURL, route)
	if err != nil {
		return 0, nil, pushResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, pushResponse{}, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if p.Authorization != nil {
		value, err := p.Authorization(ctx)
		if err != nil {
			return 0, nil, pushResponse{}, err
		}
		if value != "" {
			req.Header.Set("Authorization", value)
		}
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return 0, nil, pushResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed pushResponse
	if json.Unmarshal(raw, &parsed) != nil {
		// Not the push route's JSON: the gate's or a proxy's plain text.
		parsed = pushResponse{Detail: strings.TrimSpace(string(raw))}
	}
	return resp.StatusCode, resp.Header, parsed, nil
}

// documentRefusals are the push route's own answers about one document, by
// status and error code (sessioningest's pushhttp.go). Only these are
// recorded against a session file. Any other refusal, such as a 404 or 403
// from a proxy in front of the server, says nothing about the document and
// fails the pass, which is tried again.
var documentRefusals = map[int]string{
	http.StatusBadRequest:            "invalid_session_id",
	http.StatusForbidden:             "forbidden",
	http.StatusConflict:              "conflict",
	http.StatusRequestEntityTooLarge: "too_large",
	http.StatusUnprocessableEntity:   "undecodable",
}

// serverError turns a response that is not about one document into an error
// for the whole pass; nil when the status is about the document itself.
func (p *Pusher) serverError(status int, header http.Header, r pushResponse) error {
	switch {
	case status == http.StatusUnauthorized:
		return &client.SignInError{Reason: "the server refused the saved sign-in; run diderot setup " + p.MCPURL}
	case status == http.StatusForbidden && r.Error == "":
		return fmt.Errorf("the server does not let this account push sessions (%s): its administrator must put the account in a group named by DIDEROT_SELF_SERVICE_GROUPS", r.Detail)
	case status == http.StatusNotFound && r.Error == "":
		return fmt.Errorf("%s does not accept pushed sessions: session push is off on the server "+
			"(where the server runs, set DIDEROT_SESSION_INGEST_SOURCE=push and DIDEROT_SESSION_PUSH_REVISION_KEY), "+
			"or this URL is a gateway that forwards only MCP", p.MCPURL)
	case status == http.StatusInsufficientStorage:
		return fmt.Errorf("the server's storage quota for this account's sessions is full " +
			"(DIDEROT_SESSION_PUSH_OWNER_QUOTA_BYTES or DIDEROT_SESSION_PUSH_OWNER_QUOTA_REVISIONS where the server runs)")
	case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
		seconds, _ := strconv.Atoi(header.Get("Retry-After"))
		return &BusyError{Status: status, RetryAfter: time.Duration(seconds) * time.Second}
	case status >= 500:
		return &client.HTTPError{Status: status, Body: r.Detail}
	}
	return nil
}

// Check asks the server whether this account can push sessions to it,
// without sending one.
func (p *Pusher) Check(ctx context.Context) error {
	status, header, r, err := p.do(ctx, http.MethodGet, pushRoute+"00000000-0000-0000-0000-000000000000", nil, nil)
	if err != nil {
		return err
	}
	if err := p.serverError(status, header, r); err != nil {
		return err
	}
	// The route answers for a session that does not exist with its own 404.
	if status == http.StatusOK || (status == http.StatusNotFound && r.Error == "not_found") {
		return nil
	}
	return &client.HTTPError{Status: status, Body: r.Detail}
}

// Indexed is what the server has done with the account's sessions it was
// asked about.
type Indexed struct {
	// Listed is how many sessions the server named: the account's most
	// recently changed, at most 200.
	Listed int `json:"listed"`
	// Indexed sessions are summarized and searchable.
	Indexed int `json:"indexed"`
	// Waiting sessions are stored and not summarized yet.
	Waiting int `json:"waiting"`
	// Failed sessions could not be summarized, and the server has stopped
	// trying.
	Failed int `json:"failed"`
}

// Indexed asks the server what became of the account's most recently
// changed sessions. Pushing a session only stores it; this is how the
// client learns whether the server went on to summarize it.
func (p *Pusher) Indexed(ctx context.Context) (Indexed, error) {
	status, header, r, err := p.do(ctx, http.MethodGet, "/v1/me/agent-sessions?limit=200", nil, nil)
	if err != nil {
		return Indexed{}, err
	}
	if err := p.serverError(status, header, r); err != nil {
		return Indexed{}, err
	}
	if status != http.StatusOK {
		return Indexed{}, &client.HTTPError{Status: status, Body: r.Detail}
	}
	var out Indexed
	for _, s := range r.Sessions {
		switch {
		case s.Deleted || s.Excluded:
			continue
		case s.Indexed:
			out.Indexed++
		case s.SummaryFailed:
			out.Failed++
		default:
			out.Waiting++
		}
		out.Listed++
	}
	return out, nil
}

// Put sends one session's ATIF document. revision is the server's revision
// this document replaces, or "" for a session not sent before; the result is
// the revision the document became. A push replaces the whole session, so
// when the server holds a revision other than the one named (the state file
// was lost, or the answer to an earlier push was), Put names that one and
// sends again: the file here is the session's only source.
func (p *Pusher) Put(ctx context.Context, sessionID string, document []byte, revision string) (string, error) {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	if _, err := zw.Write(document); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	for attempt := 0; ; attempt++ {
		header := http.Header{
			"Diderot-Session-Format": {sessiondecode.FormatATIF},
			"Content-Type":           {"application/json"},
			"Content-Encoding":       {"gzip"},
		}
		if revision == "" {
			header.Set("If-None-Match", "*")
		} else {
			header.Set("If-Match", `"`+revision+`"`)
		}
		status, respHeader, r, err := p.do(ctx, http.MethodPut, pushRoute+sessionID, header, zipped.Bytes())
		if err != nil {
			return "", err
		}
		if err := p.serverError(status, respHeader, r); err != nil {
			return "", err
		}
		switch {
		case (status == http.StatusOK || status == http.StatusCreated) && r.Revision != "":
			return r.Revision, nil
		case status == http.StatusPreconditionFailed && r.BodyMatchesCurrent && r.CurrentRevision != "":
			return r.CurrentRevision, nil
		case status == http.StatusPreconditionFailed && r.CurrentRevision != "" && attempt == 0:
			revision = r.CurrentRevision
			continue
		case status == http.StatusPreconditionFailed:
			// Something else is writing this session; the next pass tries again.
			return "", &client.HTTPError{Status: status, Body: "the session changed on the server while it was being sent"}
		case r.Error == "" || documentRefusals[status] != r.Error:
			// Not the push route's word on this document.
		case status == http.StatusForbidden:
			return "", &RejectedError{Reason: "the session belongs to another account on the server"}
		case status == http.StatusRequestEntityTooLarge:
			return "", &RejectedError{Reason: fmt.Sprintf("larger than the server accepts (%d bytes)", r.Limit)}
		default:
			return "", &RejectedError{Reason: strings.TrimSuffix(r.Error+": "+r.Detail, ": ")}
		}
		return "", &client.HTTPError{Status: status, Body: strings.TrimSpace(r.Error + " " + r.Detail)}
	}
}
