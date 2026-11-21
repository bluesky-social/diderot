package files

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/safehttp"
)

// GatewayCaptionerConfig configures the provider used for image captioning.
type GatewayCaptionerConfig struct {
	BaseURL string
	APIKey  string
	// Auth selects the credential header; empty means bearer.
	Auth    llmclient.AuthStyle
	ModelID string
	// Responses selects the native Responses endpoint for this model.
	Responses bool
	// TokenParam is the model registry's chat_token_param for ModelID: the
	// field that carries the output limit. Empty means max_tokens.
	TokenParam llmclient.ChatTokenParam
	// ReasoningTokenAllowance is the model registry's
	// chat_reasoning_token_allowance for ModelID, added to the output limit.
	ReasoningTokenAllowance int
	// OmitTemperature is the model registry's chat_omit_temperature for
	// ModelID: leave temperature off instead of sending zero.
	OmitTemperature bool
	// ExtraBody is the model registry's chat_extra_body for ModelID, merged
	// into every request after the caption fields.
	ExtraBody map[string]any
	// HTTPClient is optional; defaults to a 60s-timeout client.
	HTTPClient *http.Client
	// Headers are extra headers every request carries: the provider's.
	Headers http.Header
}

const (
	// DefaultCaptionModelID is the caption model used when none is
	// configured: a cheap vision model that scored well in a caption
	// evaluation with no hallucinated-text failures.
	DefaultCaptionModelID = "gemini-3.6-flash"
)

// CaptionPrompt is the active image caption instruction, rendered for one
// request.
func CaptionPrompt() string { return promptcatalog.ActiveText(promptcatalog.ConnectorImageCaption) }

// CaptionResult is the structured output of one Captioner call,
// before flattening into the chunk-content shape. Workers persist
// the parsed fields as a flat string in event_chunks.content (so
// the embedder can index them) AND set the unread_flagged column
// on event_images so operators can identify which images the cheap
// tier flagged as not-fully-read (e.g. for a manual requeue).
type CaptionResult struct {
	// Caption is the 1-2 sentence factual description.
	Caption string
	// Text is the verbatim transcription of any readable text in
	// the image. Empty when the model wrote "none".
	Text string
	// Unread is true when the model flagged some text as not-fully-
	// readable. Surfaces as event_images.unread_flagged.
	Unread bool
	// Tags is the model-assigned content classification.
	Tags []string
	// ModelID identifies which model produced this result. Lands
	// on event_images.caption_model, for operator visibility.
	ModelID string
}

// Captioner is the interface the worker uses. The interface lets tests
// substitute a fake without the worker branching on backend type.
type Captioner interface {
	// Caption sends image bytes + the production prompt to the VLM
	// and returns the parsed structured result. mediaType is the
	// Slack-reported Content-Type; backends use it to pick the
	// right inline-data mime field.
	Caption(ctx context.Context, mediaType string, data []byte) (*CaptionResult, error)
	// ModelID is the canonical caption_model string persisted to
	// event_images.caption_model.
	ModelID() string
}

// GatewayCaptioner sends an inline image and a prompt using the model’s
// configured protocol.
type GatewayCaptioner struct {
	cfg    GatewayCaptionerConfig
	client *llmclient.Client
}

// NewGatewayCaptioner validates config without making a network request.
func NewGatewayCaptioner(cfg GatewayCaptionerConfig) (*GatewayCaptioner, error) {
	if cfg.ModelID == "" {
		cfg.ModelID = DefaultCaptionModelID
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = safehttp.NewClient(safehttp.Options{Timeout: 60 * time.Second})
	}
	client, err := llmclient.New(llmclient.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Auth: cfg.Auth, HTTPClient: cfg.HTTPClient, Headers: cfg.Headers})
	if err != nil {
		return nil, fmt.Errorf("gateway captioner: %w", err)
	}
	if cfg.TokenParam, err = llmclient.ParseChatTokenParam(string(cfg.TokenParam)); err != nil {
		return nil, fmt.Errorf("gateway captioner: %w", err)
	}
	if cfg.ReasoningTokenAllowance < 0 {
		return nil, errors.New("gateway captioner: reasoning token allowance must not be negative")
	}
	captioner := &GatewayCaptioner{cfg: cfg, client: client}
	// Reject a registry extra field that collides with a caption field here,
	// so a configuration error cannot poison every image row at request time.
	if !cfg.Responses {
		if _, err := llmclient.WithExtraFields(captioner.request(nil), cfg.ExtraBody); err != nil {
			return nil, fmt.Errorf("gateway captioner: %w", err)
		}
	}
	return captioner, nil
}

func (c *GatewayCaptioner) ModelID() string { return c.cfg.ModelID }

// Caption sends the image as a data URL plus the caption prompt. Temperature zero and the 4096-token limit preserve the direct
// Vertex request semantics through gateway translation; the registry's
// chat_extra_body supplies model-specific fields such as Gemini's explicit
// zero thinking budget, chat_token_param and chat_omit_temperature adapt the
// limit and temperature to models that reject them, and
// chat_reasoning_token_allowance raises the limit for a reasoning model.
func (c *GatewayCaptioner) Caption(ctx context.Context, mediaType string, data []byte) (*CaptionResult, error) {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if !strings.HasPrefix(mediaType, "image/") {
		return nil, fmt.Errorf("gateway captioner: non-image mediaType %q", mediaType)
	}

	imageURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
	if c.cfg.Responses {
		body := llmclient.ResponsesTextRequest{
			Model: c.cfg.ModelID, MaxOutputTokens: captionMaxTokens + c.cfg.ReasoningTokenAllowance,
			Instructions: CaptionPrompt(), Input: []llmclient.ResponsesMessage{{Role: "user", Content: []llmclient.ResponsesPart{
				{Type: "input_image", ImageURL: imageURL},
				{Type: "input_text", Text: "Describe and transcribe the image."},
			}}},
		}
		result, err := c.client.TextResponse(ctx, body, 4<<20)
		if err != nil {
			return nil, captionRequestError(err)
		}
		return c.parse(result.Text), nil
	}

	reqBody := c.request([]gatewayCaptionContent{
		{Type: "image_url", ImageURL: &gatewayImageURL{URL: imageURL}},
		{Type: "text", Text: CaptionPrompt()},
	})
	payload, err := llmclient.WithExtraFields(reqBody, c.cfg.ExtraBody)
	if err != nil {
		return nil, fmt.Errorf("gateway captioner: %w", err)
	}
	respBytes, err := c.client.PostJSON(ctx, "/chat/completions", payload, 4<<20)
	if err != nil {
		return nil, captionRequestError(err)
	}

	var gr gatewayCaptionResponse
	if err := json.Unmarshal(respBytes, &gr); err != nil {
		return nil, fmt.Errorf("gateway captioner: parse response: %w", err)
	}
	if len(gr.Choices) == 0 {
		return nil, errors.New("gateway captioner: response carried no choices")
	}
	text := strings.TrimSpace(gr.Choices[0].Message.Content)
	if text == "" {
		err := fmt.Errorf("gateway captioner: empty text (finish_reason=%s)", gr.Choices[0].FinishReason)
		if gr.Choices[0].FinishReason == "" || strings.EqualFold(gr.Choices[0].FinishReason, "unspecified") {
			return nil, &TransientError{Underlying: err}
		}
		return nil, err
	}
	return c.parse(text), nil
}

func (c *GatewayCaptioner) parse(text string) *CaptionResult {
	result := parseCaptionResponse(strings.TrimSpace(text))
	result.ModelID = c.cfg.ModelID
	return result
}

func captionRequestError(err error) error {
	var statusError *llmclient.HTTPError
	if errors.As(err, &statusError) {
		return classifyHTTPStatus(statusError.StatusCode, err)
	}
	return classifyGatewayCaptionError(fmt.Errorf("gateway captioner: request: %w", err))
}

// captionMaxTokens bounds one caption response.
const captionMaxTokens = 4096

func (c *GatewayCaptioner) request(content []gatewayCaptionContent) gatewayCaptionRequest {
	request := gatewayCaptionRequest{
		Model:          c.cfg.ModelID,
		Messages:       []gatewayCaptionMessage{{Role: "user", Content: content}},
		ChatTokenLimit: c.cfg.TokenParam.Limit(captionMaxTokens + c.cfg.ReasoningTokenAllowance),
	}
	if !c.cfg.OmitTemperature {
		request.Temperature = new(float64)
	}
	return request
}

type gatewayCaptionRequest struct {
	Model    string                  `json:"model"`
	Messages []gatewayCaptionMessage `json:"messages"`
	llmclient.ChatTokenLimit
	// Temperature is nil only for a model whose registry entry sets
	// chat_omit_temperature.
	Temperature *float64 `json:"temperature,omitempty"`
}

type gatewayCaptionMessage struct {
	Role    string                  `json:"role"`
	Content []gatewayCaptionContent `json:"content"`
}

type gatewayCaptionContent struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	ImageURL *gatewayImageURL `json:"image_url,omitempty"`
}

type gatewayImageURL struct {
	URL string `json:"url"`
}

type gatewayCaptionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// gluedUnread matches an UNREAD marker the model wrote at the end of a
// line of text instead of at the start of its own line, as in
// "Contact supportUNREAD: yes".
var gluedUnread = regexp.MustCompile(`(?im)(\S)[ \t]*(UNREAD:[ \t]*(?:yes|no))[ \t\r]*$`)

// parseCaptionResponse splits the model's 4-section labeled output
// into CaptionResult fields. Tolerates light formatting variation
// (extra blank lines between sections, mixed-case labels, missing
// optional sections, an UNREAD marker glued to the end of the line
// before it) but expects at least the CAPTION line.
func parseCaptionResponse(text string) *CaptionResult {
	out := &CaptionResult{}
	// Move a glued UNREAD marker onto its own line, where the walk
	// below reads it like any other.
	text = gluedUnread.ReplaceAllString(text, "$1\n$2")
	// Walk the sections by line, accumulating content for the
	// current one. The model occasionally emits multi-line TEXT
	// sections (which is correct — verbatim screenshots of code
	// are inherently multi-line); accumulate until the next label
	// or end of input.
	type sectionID int
	const (
		secNone sectionID = iota
		secCaption
		secText
		secUnread
		secTags
	)
	current := secNone
	var captionB, textB, unreadB, tagsB strings.Builder
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		upper := strings.ToUpper(trim)
		switch {
		case strings.HasPrefix(upper, "CAPTION:"):
			current = secCaption
			captionB.WriteString(strings.TrimSpace(trim[len("CAPTION:"):]))
		case strings.HasPrefix(upper, "TEXT:"):
			current = secText
			textB.WriteString(strings.TrimSpace(trim[len("TEXT:"):]))
		case strings.HasPrefix(upper, "UNREAD:"):
			current = secUnread
			unreadB.WriteString(strings.TrimSpace(trim[len("UNREAD:"):]))
		case strings.HasPrefix(upper, "TAGS:"):
			current = secTags
			tagsB.WriteString(strings.TrimSpace(trim[len("TAGS:"):]))
		default:
			switch current {
			case secCaption:
				if captionB.Len() > 0 {
					captionB.WriteString(" ")
				}
				captionB.WriteString(trim)
			case secText:
				if textB.Len() > 0 {
					textB.WriteString("\n")
				}
				textB.WriteString(line) // preserve original whitespace inside TEXT
			case secUnread:
				unreadB.WriteString(trim)
			case secTags:
				if tagsB.Len() > 0 {
					tagsB.WriteString(" ")
				}
				tagsB.WriteString(trim)
			}
		}
	}
	out.Caption = strings.TrimSpace(captionB.String())
	rawText := strings.TrimSpace(textB.String())
	if !strings.EqualFold(rawText, "none") && rawText != "" {
		out.Text = rawText
	}
	out.Unread = strings.EqualFold(strings.TrimSpace(unreadB.String()), "yes")
	for _, tag := range strings.Split(tagsB.String(), ",") {
		t := strings.TrimSpace(tag)
		if t != "" {
			out.Tags = append(out.Tags, t)
		}
	}
	return out
}

// FlattenCaption produces the single string the embedder indexes
// for an image_caption chunk. Shape per the design doc:
//
//	[Image: <filename or none>]
//	<caption>
//
//	Text:
//	<verbatim text>
//
//	Tags: <comma-separated tags>
//
// The image filename is optional — agent_session images don't
// have one. Empty Text is dropped (no "Text:\n\n" eyesore). Empty
// Tags is dropped too. Caller passes filename ("" when absent).
func FlattenCaption(filename string, r *CaptionResult) string {
	var sb strings.Builder
	if filename != "" {
		fmt.Fprintf(&sb, "[Image: %s]\n", filename)
	} else {
		sb.WriteString("[Image]\n")
	}
	sb.WriteString(r.Caption)
	if r.Text != "" {
		sb.WriteString("\n\nText:\n")
		sb.WriteString(r.Text)
	}
	if len(r.Tags) > 0 {
		sb.WriteString("\n\nTags: ")
		sb.WriteString(strings.Join(r.Tags, ", "))
	}
	return sb.String()
}

// MaxCaptionRunes is the hard cap on the flattened-string length
// the worker writes to event_chunks.content. Matches the design
// doc's 1500-rune limit for v1's single-chunk-per-image shape;
// over-budget rows set event_images.truncated_at and append the
// [TRUNCATED] marker.
const MaxCaptionRunes = 1500

// ApplyTruncation enforces the per-chunk cap on a flattened
// caption string. Returns the (possibly-truncated) text and a
// boolean signaling whether truncation occurred. Truncates on a
// rune boundary to keep a multi-byte codepoint from being split.
func ApplyTruncation(s string) (string, bool) {
	runes := []rune(s)
	if len(runes) <= MaxCaptionRunes {
		return s, false
	}
	const marker = "\n[TRUNCATED]"
	keepRunes := MaxCaptionRunes - len([]rune(marker))
	if keepRunes < 0 {
		keepRunes = 0
	}
	return string(runes[:keepRunes]) + marker, true
}

// classifyGatewayCaptionError wraps gateway-side network/timeout errors as
// transient. HTTP-status-derived errors are already classified by
// classifyHTTPStatus; this catches the "do() returned an error"
// path (DNS, dial, broken pipe).
func classifyGatewayCaptionError(err error) error {
	if err == nil {
		return nil
	}
	if isNetworkError(err) {
		return &TransientError{Underlying: err}
	}
	return err
}
