// Package llmclient is the bounded JSON transport every non-agentic inference
// path uses, plus the endpoint shape shared with the agentic model adapters.
package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// AuthStyle selects how an endpoint presents its API key.
type AuthStyle string

const (
	// AuthBearer sends Authorization: Bearer <key>. It is the default and
	// what OpenAI-compatible servers and model gateways expect.
	AuthBearer AuthStyle = "bearer"
	// AuthXAPIKey sends x-api-key: <key>, which the public Anthropic API
	// expects.
	AuthXAPIKey AuthStyle = "x-api-key"
)

// ParseAuthStyle accepts the empty string as AuthBearer.
func ParseAuthStyle(raw string) (AuthStyle, error) {
	switch AuthStyle(raw) {
	case "", AuthBearer:
		return AuthBearer, nil
	case AuthXAPIKey:
		return AuthXAPIKey, nil
	default:
		return "", fmt.Errorf("auth style %q is not %q or %q", raw, AuthBearer, AuthXAPIKey)
	}
}

// ChatTokenParam names the chat-completions request field that carries the
// output-token limit.
type ChatTokenParam string

const (
	// ChatMaxTokens is the original field, and the default.
	ChatMaxTokens ChatTokenParam = "max_tokens"
	// ChatMaxCompletionTokens replaces it, and is the only one OpenAI's
	// reasoning models accept. It counts reasoning tokens as well as visible
	// output.
	ChatMaxCompletionTokens ChatTokenParam = "max_completion_tokens"
)

// ParseChatTokenParam accepts the empty string as ChatMaxTokens.
func ParseChatTokenParam(raw string) (ChatTokenParam, error) {
	switch ChatTokenParam(raw) {
	case "", ChatMaxTokens:
		return ChatMaxTokens, nil
	case ChatMaxCompletionTokens:
		return ChatMaxCompletionTokens, nil
	default:
		return "", fmt.Errorf("chat token parameter %q is not %q or %q", raw, ChatMaxTokens, ChatMaxCompletionTokens)
	}
}

// ChatTokenLimit is an output-token limit encoded under one of the two
// fields. Embedded in a request struct, it encodes where the struct places
// it.
type ChatTokenLimit struct {
	MaxTokens           int `json:"max_tokens,omitempty"`
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
}

// Limit encodes n under p's field; the empty p is ChatMaxTokens.
func (p ChatTokenParam) Limit(n int) ChatTokenLimit {
	if p == ChatMaxCompletionTokens {
		return ChatTokenLimit{MaxCompletionTokens: n}
	}
	return ChatTokenLimit{MaxTokens: n}
}

// Endpoint is one inference base URL, the credential presented to it, and
// the extra headers every request to it carries. APIKey is empty for a
// server that takes no key: requests then carry no credential header.
type Endpoint struct {
	BaseURL string
	APIKey  string
	Auth    AuthStyle
	// Headers are sent on every request, after the client's own. They never
	// name the credential or content headers (CheckHeaderName).
	Headers http.Header
}

// Configured reports whether the endpoint has an address.
func (e Endpoint) Configured() bool { return e.BaseURL != "" }

// HTTPClient returns base (nil is a new client) made to send the endpoint's
// headers on every request, and, for an endpoint with no key, to send no
// credential header whatever the caller's client library adds. Every client
// of the endpoint is built on it. With a key and no headers it is base
// itself.
func (e Endpoint) HTTPClient(base *http.Client) *http.Client {
	if len(e.Headers) == 0 && e.APIKey != "" {
		return base
	}
	client := &http.Client{}
	if base != nil {
		copied := *base
		client = &copied
	}
	next := client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	client.Transport = endpointTransport{next: next, headers: e.Headers.Clone(), keyless: e.APIKey == ""}
	return client
}

// LibraryKey is the key to hand a client library that refuses to be built
// without one: the endpoint's own, or, for an endpoint with none, a stand-in
// that HTTPClient removes from every request before it is sent.
func (e Endpoint) LibraryKey() string {
	if e.APIKey == "" {
		return "no-key"
	}
	return e.APIKey
}

// endpointTransport adds an endpoint's headers to each request, and removes
// the credential headers for an endpoint with no key.
type endpointTransport struct {
	next    http.RoundTripper
	headers http.Header
	keyless bool
}

func (t endpointTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for name, values := range t.headers {
		req.Header[name] = slices.Clone(values)
	}
	if t.keyless {
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
	}
	return t.next.RoundTrip(req)
}

// reservedHeaders are the headers Diderot sets itself on a model request.
var reservedHeaders = []string{
	"Authorization", "X-Api-Key", "Content-Type", "Content-Length", "Host", "Accept-Encoding", "Connection",
	"Transfer-Encoding", "Anthropic-Version",
}

// CheckHeaderName says what is wrong with name as an extra request header,
// or nil.
func CheckHeaderName(name string) error {
	if !httpguts.ValidHeaderFieldName(name) {
		return fmt.Errorf("%q is not an HTTP header name", name)
	}
	if slices.Contains(reservedHeaders, http.CanonicalHeaderKey(name)) {
		return fmt.Errorf("%s is set by Diderot itself", http.CanonicalHeaderKey(name))
	}
	return nil
}

// ValidateBaseURL requires an http(s) URL with a host and no userinfo, query,
// or fragment. Errors never echo the URL, which may carry a credential.
func ValidateBaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid base URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("base URL must use http or https")
	}
	if parsed.User != nil {
		return errors.New("base URL must not contain userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("base URL must not contain a query or fragment")
	}
	return nil
}

type Config struct {
	BaseURL string
	// APIKey is the credential; empty sends none.
	APIKey string
	// Auth selects the credential header; empty means AuthBearer.
	Auth       AuthStyle
	HTTPClient *http.Client
	Headers    http.Header
}

type Client struct {
	baseURL string
	apiKey  string
	auth    AuthStyle
	http    *http.Client
	headers http.Header
}

func New(cfg Config) (*Client, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		return nil, errors.New("llmclient: base URL is required")
	}
	if err := ValidateBaseURL(baseURL); err != nil {
		return nil, fmt.Errorf("llmclient: %w", err)
	}
	// No key is a server that takes none: no credential header is sent.
	apiKey := strings.TrimSpace(cfg.APIKey)
	auth, err := ParseAuthStyle(string(cfg.Auth))
	if err != nil {
		return nil, fmt.Errorf("llmclient: %w", err)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	httpCopy := *httpClient
	httpCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, auth: auth,
		http: &httpCopy, headers: cfg.Headers.Clone(),
	}, nil
}

// WithExtraFields appends extra top-level fields to payload's JSON object
// after the fields payload already encodes, in sorted key order, so the
// payload's own field order is unchanged. A key payload already sets is an
// error rather than a silent override.
func WithExtraFields(payload any, extra map[string]any) (any, error) {
	if len(extra) == 0 {
		return payload, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("llmclient: encode request: %w", err)
	}
	var existing map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &existing); err != nil || existing == nil {
		return nil, errors.New("llmclient: extra fields need a JSON object payload")
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		if _, ok := existing[key]; ok {
			return nil, fmt.Errorf("llmclient: extra field %q conflicts with a request field", key)
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var out bytes.Buffer
	// json.Marshal of an object ends in exactly one closing brace with no
	// trailing whitespace.
	out.Write(encoded[:len(encoded)-1])
	for index, key := range keys {
		if index > 0 || len(existing) > 0 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		value, err := json.Marshal(extra[key])
		if err != nil {
			return nil, fmt.Errorf("llmclient: encode extra field %q: %w", key, err)
		}
		out.Write(name)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return json.RawMessage(out.Bytes()), nil
}

type HTTPError struct {
	StatusCode int
	Body       []byte
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("llmclient: status %d: %s", e.StatusCode, strings.TrimSpace(string(e.Body)))
}

type BodyError struct {
	Err error
}

func (e *BodyError) Error() string { return "llmclient: read response: " + e.Err.Error() }
func (e *BodyError) Unwrap() error { return e.Err }

func (c *Client) PostJSON(ctx context.Context, path string, payload any, maxResponseBytes int64) ([]byte, error) {
	if maxResponseBytes <= 0 {
		return nil, errors.New("llmclient: response limit must be positive")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("llmclient: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llmclient: build request: %w", err)
	}
	for name, values := range c.headers {
		req.Header[name] = append([]string(nil), values...)
	}
	req.Header.Set("Content-Type", "application/json")
	switch {
	case c.apiKey == "":
	case c.auth == AuthXAPIKey:
		req.Header.Set("X-Api-Key", c.apiKey)
	default:
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llmclient: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: snippet}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, &BodyError{Err: err}
	}
	if int64(len(raw)) > maxResponseBytes {
		return nil, &BodyError{Err: fmt.Errorf("response exceeds %d-byte read limit", maxResponseBytes)}
	}
	return raw, nil
}
