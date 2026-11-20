package sessioningest

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"
)

// PushFormatHeader names the pushed document's format.
const PushFormatHeader = "Diderot-Session-Format"

// pushMemoryFactor approximates the peak memory one push holds per received
// byte: the body, its decoded records, and their stored encoding.
const pushMemoryFactor = 3

// PushLimits bounds concurrent push work. Requests beyond them are refused
// with 429 (per owner) or 503 (process-wide) and Retry-After; nothing waits
// in memory.
type PushLimits struct {
	MaxConcurrent         int
	MaxConcurrentPerOwner int
	// MaxInflightBytes bounds the memory admitted pushes may hold at once.
	MaxInflightBytes int64
}

// PushHandler serves PUT/GET/HEAD /v1/ingest/agent-sessions/{session_id}.
// The caller authenticates the request and passes the owner taken from the
// authenticated principal; nothing in the request names an owner.
type PushHandler struct {
	store  *PushStore
	limits PushLimits
	logger *slog.Logger
	memory *semaphore.Weighted

	mu       sync.Mutex
	inflight int
	perOwner map[string]int
}

// NewPushHandler validates limits against the store's document cap.
func NewPushHandler(store *PushStore, limits PushLimits, logger *slog.Logger) (*PushHandler, error) {
	if limits.MaxConcurrent <= 0 || limits.MaxConcurrentPerOwner <= 0 {
		return nil, errors.New("sessioningest: push concurrency limits must be positive")
	}
	if limits.MaxInflightBytes < store.MaxDocumentBytes()*pushMemoryFactor {
		return nil, fmt.Errorf("sessioningest: push in-flight memory %d cannot admit one maximum document (%d bytes x %d)",
			limits.MaxInflightBytes, store.MaxDocumentBytes(), pushMemoryFactor)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PushHandler{store: store, limits: limits, logger: logger,
		memory: semaphore.NewWeighted(limits.MaxInflightBytes), perOwner: map[string]int{}}, nil
}

// Store is the push store behind the handler.
func (h *PushHandler) Store() *PushStore { return h.store }

type pushError struct {
	Error              string `json:"error"`
	Detail             string `json:"detail,omitempty"`
	CurrentRevision    string `json:"current_revision,omitempty"`
	BodyMatchesCurrent *bool  `json:"body_matches_current,omitempty"`
	Quota              string `json:"quota,omitempty"`
	Limit              int64  `json:"limit,omitempty"`
	Used               int64  `json:"used,omitempty"`
}

func writePushJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func quoteETag(revision string) string { return `"` + revision + `"` }

// parseIfMatch returns the strong entity tags of an If-Match value. Weak tags
// never match a strong comparison and are dropped.
func parseIfMatch(header string) []string {
	var tags []string
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
		case part == "*":
			tags = append(tags, "*")
		case strings.HasPrefix(part, "W/"):
		default:
			tags = append(tags, strings.Trim(part, `"`))
		}
	}
	return tags
}

func pushSessionID(r *http.Request) (string, bool) {
	id := r.PathValue("session_id")
	parsed, err := uuid.Parse(id)
	return id, err == nil && parsed.String() == id
}

// admit reserves a concurrency slot and memory for one push. release must be
// called when the push is done.
func (h *PushHandler) admit(w http.ResponseWriter, owner string, reserve int64) (func(), bool) {
	h.mu.Lock()
	switch {
	case h.inflight >= h.limits.MaxConcurrent:
		h.mu.Unlock()
		w.Header().Set("Retry-After", "5")
		writePushJSON(w, http.StatusServiceUnavailable, pushError{Error: "busy"})
		return nil, false
	case h.perOwner[owner] >= h.limits.MaxConcurrentPerOwner:
		h.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		writePushJSON(w, http.StatusTooManyRequests, pushError{Error: "too_many_concurrent_pushes"})
		return nil, false
	}
	if !h.memory.TryAcquire(reserve) {
		h.mu.Unlock()
		w.Header().Set("Retry-After", "5")
		writePushJSON(w, http.StatusServiceUnavailable, pushError{Error: "busy"})
		return nil, false
	}
	h.inflight++
	h.perOwner[owner]++
	h.mu.Unlock()
	return func() {
		h.memory.Release(reserve)
		h.mu.Lock()
		h.inflight--
		if h.perOwner[owner]--; h.perOwner[owner] == 0 {
			delete(h.perOwner, owner)
		}
		h.mu.Unlock()
	}, true
}

// ServePut handles one push. owner is the authenticated principal's email.
func (h *PushHandler) ServePut(w http.ResponseWriter, r *http.Request, owner string) {
	owner = CanonicalOwner(owner)
	sessionID, ok := pushSessionID(r)
	if !ok {
		writePushJSON(w, http.StatusBadRequest, pushError{Error: "invalid_session_id", Detail: "session_id must be a canonical lowercase UUID"})
		return
	}
	format := r.Header.Get(PushFormatHeader)
	if _, ok := h.store.Decoder(format); !ok {
		writePushJSON(w, http.StatusBadRequest, pushError{Error: "unsupported_format",
			Detail: fmt.Sprintf("%s must be one of %s", PushFormatHeader, strings.Join(h.store.Formats(), ", "))})
		return
	}
	ifMatch, ifNoneMatch := r.Header.Get("If-Match"), r.Header.Get("If-None-Match")
	switch {
	case ifMatch == "" && ifNoneMatch == "":
		writePushJSON(w, http.StatusPreconditionRequired, pushError{Error: "precondition_required",
			Detail: "send If-None-Match: * to create or If-Match: <revision> to update"})
		return
	case ifMatch != "" && ifNoneMatch != "":
		writePushJSON(w, http.StatusBadRequest, pushError{Error: "conflicting_preconditions"})
		return
	case ifNoneMatch != "" && strings.TrimSpace(ifNoneMatch) != "*":
		writePushJSON(w, http.StatusBadRequest, pushError{Error: "unsupported_precondition", Detail: "only If-None-Match: * is supported"})
		return
	}
	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		writePushJSON(w, http.StatusUnsupportedMediaType, pushError{Error: "unsupported_content_encoding"})
		return
	}

	maxBytes := h.store.MaxDocumentBytes()
	if encoding == "" || encoding == "identity" {
		if r.ContentLength > maxBytes {
			writePushJSON(w, http.StatusRequestEntityTooLarge, pushError{Error: "too_large", Limit: maxBytes})
			return
		}
	}
	reserve := maxBytes
	if (encoding == "" || encoding == "identity") && r.ContentLength >= 0 {
		reserve = r.ContentLength
	}
	release, ok := h.admit(w, owner, max(reserve, 1)*pushMemoryFactor)
	if !ok {
		return
	}
	defer release()

	// Ownership, writer, and format are checked before reading the body.
	head, err := h.store.Head(r.Context(), owner, sessionID)
	switch {
	case errors.Is(err, ErrPushNotFound):
	case err != nil:
		h.writeError(w, err)
		return
	case head.Format != format:
		h.writeError(w, &PushConflictError{Reason: "session is bound to format " + head.Format})
		return
	}

	var reader io.Reader = r.Body
	if encoding == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			writePushJSON(w, http.StatusBadRequest, pushError{Error: "invalid_gzip", Detail: err.Error()})
			return
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		writePushJSON(w, http.StatusBadRequest, pushError{Error: "unreadable_body", Detail: err.Error()})
		return
	}
	if int64(len(body)) > maxBytes {
		writePushJSON(w, http.StatusRequestEntityTooLarge, pushError{Error: "too_large", Limit: maxBytes})
		return
	}

	outcome, err := h.store.Accept(r.Context(), PushInput{
		Owner: owner, SessionID: sessionID, Format: format,
		Create: ifNoneMatch != "", IfMatch: parseIfMatch(ifMatch), Body: body,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	status := http.StatusOK
	if outcome.Created {
		status = http.StatusCreated
	}
	h.logger.Info("sessioningest: push accepted", "session", sessionID, "format", format,
		"bytes", len(body), "status", status, "already_applied", outcome.AlreadyApplied)
	writePushJSON(w, status, map[string]string{"revision": outcome.Revision})
}

func (h *PushHandler) writeError(w http.ResponseWriter, err error) {
	var (
		conflict     *PushConflictError
		precondition *PushPreconditionError
		quota        *PushQuotaError
		decode       *PushDecodeError
	)
	switch {
	case errors.Is(err, ErrPushForbidden):
		writePushJSON(w, http.StatusForbidden, pushError{Error: "forbidden", Detail: "the session belongs to another owner"})
	case errors.Is(err, ErrPushNotFound):
		writePushJSON(w, http.StatusNotFound, pushError{Error: "not_found"})
	case errors.As(err, &conflict):
		writePushJSON(w, http.StatusConflict, pushError{Error: "conflict", Detail: conflict.Reason})
	case errors.As(err, &precondition):
		if precondition.Current != "" {
			w.Header().Set("ETag", quoteETag(precondition.Current))
		}
		matches := precondition.BodyMatchesCurrent
		writePushJSON(w, http.StatusPreconditionFailed, pushError{Error: "precondition_failed",
			CurrentRevision: precondition.Current, BodyMatchesCurrent: &matches})
	case errors.As(err, &quota):
		writePushJSON(w, http.StatusInsufficientStorage, pushError{Error: "quota_exceeded",
			Quota: quota.Quota, Limit: quota.Limit, Used: quota.Used})
	case errors.As(err, &decode):
		writePushJSON(w, http.StatusUnprocessableEntity, pushError{Error: "undecodable", Detail: decode.Err.Error()})
	default:
		h.logger.Error("sessioningest: push failed", "err", err)
		writePushJSON(w, http.StatusInternalServerError, pushError{Error: "internal"})
	}
}

// ServeGet answers GET and HEAD with the head revision's bytes and its strong
// ETag. HEAD and a matching If-None-Match never read the bytes; a GET that
// does is admitted like a push.
func (h *PushHandler) ServeGet(w http.ResponseWriter, r *http.Request, owner string) {
	owner = CanonicalOwner(owner)
	sessionID, ok := pushSessionID(r)
	if !ok {
		writePushJSON(w, http.StatusBadRequest, pushError{Error: "invalid_session_id"})
		return
	}
	head, size, err := h.store.HeadSize(r.Context(), owner, sessionID)
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("ETag", quoteETag(head.Revision))
	w.Header().Set(PushFormatHeader, head.Format)
	// RFC 9110 §13.2.2: If-Match (strong) first, then If-None-Match (weak).
	if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
		matched := false
		for _, tag := range parseIfMatch(ifMatch) {
			matched = matched || tag == "*" || tag == head.Revision
		}
		if !matched {
			writePushJSON(w, http.StatusPreconditionFailed, pushError{Error: "precondition_failed", CurrentRevision: head.Revision})
			return
		}
	}
	for _, tag := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		tag = strings.Trim(strings.TrimPrefix(strings.TrimSpace(tag), "W/"), `"`)
		if tag == "*" || tag == head.Revision {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	release, ok := h.admit(w, owner, max(size, 1))
	if !ok {
		return
	}
	defer release()
	body, err := h.store.RevisionBody(r.Context(), sessionID, head.Revision)
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
