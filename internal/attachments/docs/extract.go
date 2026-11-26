// Package docs extracts PDF, DOCX, and XLSX attachments into document chunks.
package docs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

// maxOutputTokens bounds one sparse-page response.
const maxOutputTokens = 16000

// perCallTimeout prevents a gateway call from holding a worker forever.
const perCallTimeout = 20 * time.Minute

// MaxDocumentBytes bounds the in-memory fetch, mirror, and request path.
const MaxDocumentBytes = 25 << 20 // 25 MiB

// maxResponseBytes bounds a malformed or hostile gateway response.
const maxResponseBytes int64 = 8 << 20 // 8 MiB

// PageMarker is what we ask the model to emit between pages, and what
// the splitter keys on to produce page-level chunks. Deliberately
// unlikely to occur in document text.
const PageMarker = "--- PAGE %d ---"

// pageMarkerPrefix is the literal, format-free prefix used for
// scanning. Kept in sync with PageMarker by construction in tests.
const pageMarkerPrefix = "--- PAGE "

// ExtractPrompt is the active extraction instruction, rendered for one
// request.
func ExtractPrompt() string { return promptcatalog.ActiveText(promptcatalog.ConnectorDocumentExtract) }

// Result is one document's extraction outcome.
type Result struct {
	// Markdown is the transcribed document text, page markers included.
	Markdown string
	// PageCount is how many pages the document has. Zero for formats
	// without pages (spreadsheets, docx). On the model path it can only be
	// how many page markers came back — a self-report; on the pdfium path
	// it is pdfium's own count, available pre-call and free, which is what
	// makes event_documents.pages_extracted a measurement rather than a
	// claim.
	PageCount int
	// PagesExtracted is how many of those pages produced content we are
	// willing to call the page's content. The numerator to PageCount's
	// denominator: a row where it comes up short settles
	// incomplete_reason='missing_pages'. Equal to PageCount whenever
	// every page was covered.
	PagesExtracted int
	// ModelID identifies the producing model; lands on
	// event_documents.extract_model.
	ModelID string
	// Truncated is true when the response stopped at maxOutputTokens plus
	// any reasoning allowance.
	// The partial text is still kept (a truncated 40-page contract is
	// more useful than nothing) but it also correlates with repetition
	// loops, so it's recorded.
	Truncated bool
	// Token usage from the model call.
	InputTokens  int
	OutputTokens int
	// CostUSD is the accumulated model cost.
	CostUSD     float64
	CostLimited bool
	// Pictures are the images stored inside the document, for the image
	// caption queue. Only the Word parser finds any.
	Pictures []Picture
}

// Extractor turns document bytes into markdown.
type Extractor interface {
	Extract(ctx context.Context, docFormat string, filename string, data []byte) (*Result, error)
	ModelID() string
}

// GatewayExtractorConfig configures the model-backed PDF extractor.
type GatewayExtractorConfig struct {
	// BaseURL is the provider endpoint, including any /v1 prefix.
	// Required.
	BaseURL string
	// APIKey is the credential presented to the endpoint. Empty sends
	// none, for a server that takes no key.
	APIKey string
	// Auth selects the credential header; empty means bearer.
	Auth llmclient.AuthStyle
	// ModelID is the model every request names. Required: there is no
	// built-in model.
	ModelID string
	// Responses selects native Responses file input.
	Responses bool
	// TokenParam is the model registry's chat_token_param for ModelID: the
	// field that carries the output limit. Empty means max_tokens.
	TokenParam llmclient.ChatTokenParam
	// ReasoningTokenAllowance is the model registry's
	// chat_reasoning_token_allowance for ModelID, added to the output limit.
	ReasoningTokenAllowance int
	// ExtraBody is the model registry's chat_extra_body for ModelID, merged
	// into every request after the extraction fields.
	ExtraBody map[string]any
	// HTTPClient overrides the default client. Tests inject one pointed
	// at an httptest.Server. Per-call deadlines are applied via context
	// (see perCallTimeout), not via HTTPClient.Timeout, so the default
	// client carries no timeout of its own.
	HTTPClient *http.Client
	// Headers are extra headers every request carries: the provider's.
	Headers http.Header
}

// GatewayExtractor transcribes sparse PDF pages through the model’s configured protocol.
type GatewayExtractor struct {
	cfg    GatewayExtractorConfig
	client *llmclient.Client
	logger *slog.Logger
	prompt string
}

// NewGatewayExtractor validates config without making a network call.
func NewGatewayExtractor(cfg GatewayExtractorConfig, logger *slog.Logger) (*GatewayExtractor, error) {
	if cfg.ModelID == "" {
		return nil, errors.New("docs: gateway extractor: a model ID is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	client, err := llmclient.New(llmclient.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Auth: cfg.Auth, HTTPClient: cfg.HTTPClient, Headers: cfg.Headers})
	if err != nil {
		return nil, fmt.Errorf("docs: gateway extractor: %w", err)
	}
	if cfg.TokenParam, err = llmclient.ParseChatTokenParam(string(cfg.TokenParam)); err != nil {
		return nil, fmt.Errorf("docs: gateway extractor: %w", err)
	}
	if cfg.ReasoningTokenAllowance < 0 {
		return nil, errors.New("docs: gateway extractor: reasoning token allowance must not be negative")
	}
	extractor := &GatewayExtractor{cfg: cfg, client: client, logger: logger}
	if !cfg.Responses {
		if _, err := llmclient.WithExtraFields(extractor.request(nil), cfg.ExtraBody); err != nil {
			return nil, fmt.Errorf("docs: gateway extractor: %w", err)
		}
	}
	return extractor, nil
}

// ModelID reports the configured model, for event_documents.extract_model.
func (e *GatewayExtractor) ModelID() string { return e.cfg.ModelID }

// Snapshot freezes the active extraction prompt for one document run. This
// prevents a prompt activation midway through a multi-page PDF from mixing two
// cache identities in one checkpoint set.
func (e *GatewayExtractor) Snapshot() Extractor {
	copy := *e
	copy.prompt = ExtractPrompt()
	return &copy
}

// CheckpointIdentity fingerprints every model-visible setting that can change
// a sparse-page transcription while keeping the human-readable model prefix.
// TokenParam is left out: it moves the limit to another field name without
// changing the limit.
func (e *GatewayExtractor) CheckpointIdentity() string {
	prompt := e.prompt
	if prompt == "" {
		prompt = ExtractPrompt()
	}
	values := []string{
		"document-extract-checkpoint-v1",
		e.cfg.ModelID,
		fmt.Sprintf("max-output-tokens=%d", maxOutputTokens),
		prompt,
	}
	// Appended only when present so identities minted without extra fields
	// or an allowance stay valid. The allowance raises the limit a page can
	// be truncated at.
	if e.cfg.Responses {
		values = append(values, "protocol=responses")
	}
	if len(e.cfg.ExtraBody) > 0 {
		extra, _ := json.Marshal(e.cfg.ExtraBody)
		values = append(values, "extra-body="+string(extra))
	}
	if e.cfg.ReasoningTokenAllowance > 0 {
		values = append(values, fmt.Sprintf("reasoning-token-allowance=%d", e.cfg.ReasoningTokenAllowance))
	}
	h := sha256.New()
	for _, value := range values {
		_, _ = fmt.Fprintf(h, "%d:", len(value))
		_, _ = h.Write([]byte(value))
	}
	return e.cfg.ModelID + "@sha256:" + hex.EncodeToString(h.Sum(nil))
}

// BaseURL reports the configured chat-completions endpoint.
func (e *GatewayExtractor) BaseURL() string { return e.cfg.BaseURL }

// Content contains one file part and one text part.
type gatewayRequest struct {
	Model string `json:"model"`
	llmclient.ChatTokenLimit
	Messages []gatewayMessage `json:"messages"`
}

func (e *GatewayExtractor) request(content []any) gatewayRequest {
	return gatewayRequest{
		Model:          e.cfg.ModelID,
		ChatTokenLimit: e.cfg.TokenParam.Limit(maxOutputTokens + e.cfg.ReasoningTokenAllowance),
		Messages:       []gatewayMessage{{Role: "user", Content: content}},
	}
}

type gatewayMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

// gatewayResponse is the subset of the gateway response used by extraction.
type gatewayResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Extract sends one document to the gateway and returns its markdown.
func (e *GatewayExtractor) Extract(
	ctx context.Context,
	docFormat string,
	filename string,
	data []byte,
) (*Result, error) {
	if len(data) == 0 {
		return nil, &files.PermanentError{Underlying: errors.New("docs: empty document")}
	}
	if len(data) > MaxDocumentBytes {
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: %d bytes exceeds the %d-byte limit", len(data), MaxDocumentBytes)}
	}
	mime, ok := docFormatMIME(docFormat)
	if !ok {
		return nil, &files.PermanentError{Underlying: fmt.Errorf(
			"docs: unsupported document format %q", docFormat)}
	}
	ctx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	prompt := e.prompt
	if prompt == "" {
		prompt = ExtractPrompt()
	}
	fileData := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	if e.cfg.Responses {
		filename = safeDocumentName(strings.TrimSuffix(filename, filepath.Ext(filename))) + "." + docFormat
	} else {
		filename = safeDocumentName(filename)
	}
	answer, err := e.transcribe(ctx, prompt, filename, fileData)
	if err != nil {
		var statusError *llmclient.HTTPError
		var bodyError *llmclient.BodyError
		switch {
		case errors.As(err, &statusError):
			return nil, classifyGatewayError(statusError.StatusCode, statusError.Body, nil)
		case errors.As(err, &bodyError):
			return nil, &files.TransientError{Underlying: err}
		default:
			return nil, classifyGatewayError(0, nil, err)
		}
	}

	text := strings.TrimSpace(stripCodeFence(answer.Text))
	if text == "" {
		return nil, errors.New("docs: extractor returned empty text")
	}

	// PagesExtracted equals PageCount here by construction: the only page
	// count this transport knows is the markers it got back, so every page
	// it knows about is a page it transcribed. The two diverge on the
	// pdfium arm, which knows the real denominator independently.
	markers := countPageMarkers(text)
	return &Result{
		Markdown:       text,
		PageCount:      markers,
		PagesExtracted: markers,
		ModelID:        e.cfg.ModelID,
		Truncated:      answer.Truncated,
		InputTokens:    answer.InputTokens,
		OutputTokens:   answer.OutputTokens,
	}, nil
}

func (e *GatewayExtractor) transcribe(ctx context.Context, prompt, filename, fileData string) (llmclient.ResponsesTextResult, error) {
	if e.cfg.Responses {
		return e.client.TextResponse(ctx, llmclient.ResponsesTextRequest{
			Model: e.cfg.ModelID, MaxOutputTokens: maxOutputTokens + e.cfg.ReasoningTokenAllowance,
			Input: []llmclient.ResponsesMessage{{Role: "user", Content: []llmclient.ResponsesPart{
				{Type: "input_file", FileData: fileData, Filename: filename},
				{Type: "input_text", Text: prompt},
			}}},
		}, maxResponseBytes)
	}
	content := []any{
		map[string]any{"type": "file", "file": map[string]string{"filename": filename, "file_data": fileData}},
		map[string]string{"type": "text", "text": prompt},
	}
	payload, err := llmclient.WithExtraFields(e.request(content), e.cfg.ExtraBody)
	if err != nil {
		return llmclient.ResponsesTextResult{}, fmt.Errorf("docs: %w", err)
	}
	raw, err := e.client.PostJSON(ctx, "/chat/completions", payload, maxResponseBytes)
	if err != nil {
		return llmclient.ResponsesTextResult{}, err
	}
	var answer gatewayResponse
	if err := json.Unmarshal(raw, &answer); err != nil {
		return llmclient.ResponsesTextResult{}, fmt.Errorf("docs: decode gateway response: %w", err)
	}
	if len(answer.Choices) == 0 {
		return llmclient.ResponsesTextResult{}, errors.New("docs: gateway response carried no choices")
	}
	return llmclient.ResponsesTextResult{
		Text: answer.Choices[0].Message.Content, Truncated: answer.Choices[0].FinishReason == "length",
		InputTokens: answer.Usage.PromptTokens, OutputTokens: answer.Usage.CompletionTokens,
	}, nil
}

// stripCodeFence removes one fence only when it wraps the entire response.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	// Drop the opening fence line (which may carry a language tag).
	nl := strings.IndexByte(t, '\n')
	if nl < 0 {
		return s
	}
	rest := t[nl+1:]
	// Require the closing fence to be the very end, and require that
	// no other fence appears in between — otherwise this is a document
	// whose first line happens to open a code block.
	closing := strings.LastIndex(rest, "```")
	if closing < 0 || strings.TrimSpace(rest[closing+3:]) != "" {
		return s
	}
	if strings.Contains(rest[:closing], "```") {
		return s
	}
	return strings.TrimSpace(rest[:closing])
}

// countPageMarkers counts "--- PAGE N ---" lines.
func countPageMarkers(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), pageMarkerPrefix) {
			n++
		}
	}
	return n
}

// safeDocumentName bounds and sanitizes the user-controlled gateway filename.
func safeDocumentName(filename string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range filename {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case r == '-' || r == ' ':
			// Collapse runs: a run of hyphens or spaces is noise, and
			// collapsing keeps the sanitized name compact.
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
		if b.Len() >= 200 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "document"
	}
	return out
}

// docFormatMIME maps supported formats to gateway data-URL MIME types.
func docFormatMIME(f string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "pdf":
		return "application/pdf", true
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document", true
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", true
	default:
		return "", false
	}
}

// FormatForMediaType maps a source-reported mimetype to the document
// format we'll send, or "" when the type isn't ingestable.
//
// Thin alias for events.DocFormatForMediaType, which owns the mapping
// because the connectors that record documents (Slack, Zendesk) need
// it too. Re-exported here so callers reading the extractor don't have
// to know that.
func FormatForMediaType(mediaType, filename string) string {
	return events.DocFormatForMediaType(mediaType, filename)
}

// ExtensionFor returns the S3-key extension for a doc format.
// Cosmetic — operators reading the bucket appreciate the hint.
func ExtensionFor(docFormat string) string {
	if f := strings.ToLower(strings.TrimSpace(docFormat)); f != "" {
		return "." + f
	}
	return ""
}

// classifyGatewayError separates retryable transport/load failures from
// request failures that will repeat unchanged.
func classifyGatewayError(status int, body []byte, err error) error {
	if err != nil {
		// context.Canceled shows up here whenever the caller's context is
		// cancelled mid-request — leadership loss, pod shutdown, the
		// worker's own timeout wrapper firing on a sibling goroutine. None
		// of that is a property of the document, so it must not burn the
		// failure budget any more than a network blip does.
		if errors.Is(err, context.Canceled) || isNetworkError(err) {
			return &files.TransientError{Underlying: err}
		}
		return err
	}

	msg := truncatedBody(body)
	switch {
	// 408/425 are the gateway (or an intermediary) saying the request
	// itself timed out or arrived too early for a retried body — both
	// transport conditions, same bucket as 429/5xx.
	case status == http.StatusRequestTimeout, status == http.StatusTooEarly,
		status == http.StatusTooManyRequests, status >= 500 && status < 600:
		return &files.TransientError{Underlying: fmt.Errorf(
			"docs: gateway returned HTTP %d: %s", status, msg)}
	case status == http.StatusBadRequest,
		status == http.StatusRequestEntityTooLarge,
		status == http.StatusUnsupportedMediaType,
		status == http.StatusUnprocessableEntity:
		return &files.PermanentError{Underlying: fmt.Errorf(
			"docs: gateway rejected the document with HTTP %d: %s", status, msg)}
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// A config slip (bad/expired key) must not poison every document
		// in the queue — the same reasoning as the deleted Converse
		// code's AccessDeniedException handling. Fixing the key fixes
		// this without touching the row.
		return &files.TransientError{Underlying: fmt.Errorf(
			"docs: gateway auth failed with HTTP %d: %s", status, msg)}
	case status == http.StatusNotFound:
		// A wrong model id is config too, same argument as 401/403.
		return &files.TransientError{Underlying: fmt.Errorf(
			"docs: gateway returned HTTP %d (bad model id?): %s", status, msg)}
	default:
		return fmt.Errorf("docs: gateway returned HTTP %d: %s", status, msg)
	}
}

// truncatedBody keeps gateway errors useful without storing an unbounded body.
func truncatedBody(body []byte) string {
	const capBytes = 512
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(empty body)"
	}
	if len(s) > capBytes {
		return s[:capBytes] + "…(truncated)"
	}
	return s
}

// isNetworkError detects "the connection didn't make it / didn't answer
// in time" failures. Mirrors files.IsNetworkError's shape, but unlike
// it also treats EOF as transient, for the reason below.
//
// This is only ever called on the error e.http.Do returns — i.e. before
// any response was received at all, never on a body-read or JSON-decode
// failure (those call sites wrap directly rather than routing through
// this helper; see their own comments). An EOF at this call site
// unambiguously means the peer closed the connection before answering,
// which is transient, so io.EOF/io.ErrUnexpectedEOF are matched by type
// rather than excluded.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
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
