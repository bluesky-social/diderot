package askchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

// The thread summary every chat surface offers ("Summarize thread"): a
// surface fetches the thread into ThreadMessages and posts the result; the
// retention cap that keeps every pinned message, the transcript within its
// budget, and the model call are shared.

// SummaryMaxMessages caps how many thread messages are retained for
// the prompt. Threads longer than this are summarized over the most
// recent messages (plus every pinned message — see KeepNewestAndPinned),
// with a note in the output.
const SummaryMaxMessages = 200

// SummaryMaxFetchedMessages is a hard pagination stop: the number of
// messages FETCHED (not retained) after which fetchThread stops
// requesting pages. Separate from SummaryMaxMessages (the retained
// cap) because pin-preservation can retain more than the cap, and
// because pagination itself costs Slack API calls and time — a huge
// thread must not be paged to exhaustion before the LLM call gets its
// share of the 90s budget.
const SummaryMaxFetchedMessages = 2000

// SummaryMaxPromptChars bounds the transcript sent to the LLM. Long
// threads are truncated from the front (keeping recent context) with a
// note in the output.
const SummaryMaxPromptChars = 60_000

// SummaryMaxMessageChars caps a single message's text in the
// transcript. Without it one enormous message could consume the whole
// prompt budget — or make a pinned message unfittable, which would
// break the one-action-item-per-pin contract.
const SummaryMaxMessageChars = 2000

// SummaryMaxOutputTokens bounds one summary generation. Slack message
// limits sit around 40k characters; a 2-5-sentence summary plus one
// bullet per pin fits comfortably inside ~2k tokens, and the bound keeps
// a runaway or looping model from producing an unpostable response (the
// prompt contract asks for concise output; this is the hard backstop). A
// model's reasoning allowance is added on top, so for a reasoning model the
// backstop is looser by that much.
const SummaryMaxOutputTokens = 2000

// ThreadMessage is one message of a fetched thread, flattened for
// rendering and pin detection. DroppedBefore counts non-pinned messages
// the fetch cap dropped immediately before this retained message (0 on
// an uncapped fetch); RenderTranscript surfaces it as an inline gap
// marker.
type ThreadMessage struct {
	Timestamp     string
	UserID        string
	Username      string
	Text          string
	Pinned        bool
	DroppedBefore int
}

// KeepNewestAndPinned enforces the fetch cap while guaranteeing every
// pinned message survives: it keeps the newest `cap` messages plus any
// pinned messages that fall outside that window. Order is preserved
// (chronological). Non-pinned messages dropped between two retained
// ones are visible as gaps via their differing indices; RenderTranscript
// annotates them.
func KeepNewestAndPinned(all []ThreadMessage, cap int) []ThreadMessage {
	if cap < 0 {
		// A negative cap has no documented meaning; treat it like cap=0
		// (keep only pinned messages) rather than letting tailStart below
		// exceed len(all) and run the pinned-scan loop past the end of
		// the slice.
		cap = 0
	}
	if len(all) <= cap {
		return all
	}
	tailStart := len(all) - cap
	keep := make(map[int]bool, len(all))
	for i := tailStart; i < len(all); i++ {
		keep[i] = true
	}
	for i := 0; i < tailStart; i++ {
		if all[i].Pinned {
			keep[i] = true
		}
	}
	out := make([]ThreadMessage, 0, len(keep))
	dropped := 0
	for i, m := range all {
		if keep[i] {
			// Accumulate: fetchThread applies the cap per page, so this
			// function runs repeatedly over already-filtered output —
			// overwrite would lose gap counts from earlier applications.
			m.DroppedBefore += dropped
			dropped = 0
			out = append(out, m)
		} else {
			// A dropped message may itself carry DroppedBefore from an
			// earlier pass (retained then fallen out of the newest
			// window as later pages arrived). Its accumulated count
			// transfers into the pending run — a bare dropped++ would
			// under-report the gap marker to the model.
			dropped += 1 + m.DroppedBefore
		}
	}
	return out
}

// RenderTranscript formats the thread for the LLM under title ("Slack
// thread in channel #ops"). Returns the transcript
// and a truncation note (empty when nothing was dropped).
//
// Truncation is pin-preserving at BOTH layers: the fetch cap
// (KeepNewestAndPinned) retains pinned messages outside the newest
// window, and the prompt-size budget here always includes every pinned
// line, spending the remaining budget on the NEWEST non-pinned lines.
// Elided runs (from either layer) are marked inline so the model knows
// messages were dropped rather than the thread skipping. A single
// message's text is capped at SummaryMaxMessageChars.
func RenderTranscript(title string, msgs []ThreadMessage) (string, string) {
	header := title + ":\n\n"

	renderLine := func(m ThreadMessage) string {
		text := strings.TrimSpace(m.Text)
		if len(text) > SummaryMaxMessageChars {
			text = strings.ToValidUTF8(text[:SummaryMaxMessageChars], "") + " […truncated…]"
		}
		pin := ""
		if m.Pinned {
			pin = " [PINNED]"
		}
		author := m.Username
		if author == "" {
			author = m.UserID
		}
		return fmt.Sprintf("%s%s: %s", author, pin, text)
	}

	lines := make([]string, len(msgs))
	total := len(header)
	for i, m := range msgs {
		lines[i] = renderLine(m)
		total += len(lines[i]) + 1
	}
	if total <= SummaryMaxPromptChars {
		return renderWithGaps(header, msgs, lines, alwaysInclude(len(msgs))), ""
	}

	// Budgeted selection: pinned lines are unconditionally included
	// (the contract outranks the byte cap; a pathological number of
	// huge pins can overshoot it, which the gateway tolerates), then
	// the newest non-pinned lines fill the rest.
	budget := SummaryMaxPromptChars - len(header)
	include := make([]bool, len(msgs))
	for i, m := range msgs {
		if m.Pinned {
			include[i] = true
			budget -= len(lines[i]) + 1
		}
	}
	for i := len(msgs) - 1; i >= 0 && budget > 0; i-- {
		if include[i] {
			continue
		}
		// Best-effort fill, newest-preferring: an unfittable newest
		// line does NOT stop the scan — shorter older lines may still
		// fit and are worth including.
		if len(lines[i])+1 > budget {
			continue
		}
		include[i] = true
		budget -= len(lines[i]) + 1
	}

	note := fmt.Sprintf("(transcript truncated: %d of %d messages retained — oldest non-pinned dropped to fit %d chars)",
		countTrue(include), len(msgs), SummaryMaxPromptChars)
	return renderWithGaps(header, msgs, lines, include), note
}

// alwaysInclude returns an all-true inclusion set.
func alwaysInclude(n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = true
	}
	return out
}

func countTrue(b []bool) int {
	n := 0
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

// renderWithGaps emits the included messages in chronological order,
// merging fetch-layer gaps (DroppedBefore) and prompt-layer exclusions
// into one inline marker per elided run. A trailing dropped run is
// flushed with its own marker so no elision is silently lost.
func renderWithGaps(header string, msgs []ThreadMessage, lines []string, include []bool) string {
	var sb strings.Builder
	sb.WriteString(header)
	pending := 0 // unreported dropped messages (fetch- or prompt-layer)
	flush := func() {
		if pending > 0 {
			fmt.Fprintf(&sb, "[… %d earlier message(s) dropped — not pinned …]\n", pending)
			pending = 0
		}
	}
	for i, m := range msgs {
		pending += m.DroppedBefore
		if include[i] {
			flush()
			fmt.Fprintf(&sb, "[%d] %s\n", i+1, lines[i])
		} else {
			pending++
		}
	}
	// Trailing dropped run (everything after the last included line).
	flush()
	return sb.String()
}

// SummarizerConfig configures the thread summarizer.
type SummarizerConfig struct {
	// SystemPrompt is the summary's instructions: the surface's, in its
	// dialect. Required.
	SystemPrompt string
	// BaseURL is the endpoint of the model's provider in the model registry.
	BaseURL string
	// APIKey is the credential presented to the endpoint.
	APIKey string
	// Auth selects the credential header; empty means bearer.
	Auth llmclient.AuthStyle
	// ModelID is the model the Models page chose for thread summaries.
	// Required.
	ModelID string
	// Responses selects the native Responses endpoint.
	Responses bool
	// TokenParam is the model registry's chat_token_param for ModelID: the
	// field that carries the output limit. Empty means max_tokens.
	TokenParam llmclient.ChatTokenParam
	// ReasoningTokenAllowance is the model registry's
	// chat_reasoning_token_allowance for ModelID, added to the output limit.
	ReasoningTokenAllowance int
	// ExtraBody is the model registry's chat_extra_body for ModelID.
	ExtraBody map[string]any
	// HTTPClient overrides the default client. Tests inject one pointed
	// at an httptest.Server. The pipeline deadline is applied via
	// context, not via HTTPClient.Timeout.
	HTTPClient *http.Client
	// Headers are extra headers every request carries: the provider's.
	Headers http.Header
}

// summaryMaxResponseBytes bounds a malformed or hostile gateway
// response.
const summaryMaxResponseBytes int64 = 4 << 20

// Summarizer sends a rendered thread transcript to the model and returns its summary.
type Summarizer struct {
	client     *llmclient.Client
	modelID    string
	tokenParam llmclient.ChatTokenParam
	maxTokens  int
	extraBody  map[string]any
	prompt     string
	responses  bool
}

// NewSummarizer validates cfg without making a network request.
func NewSummarizer(cfg SummarizerConfig) (*Summarizer, error) {
	client, err := llmclient.New(llmclient.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Auth: cfg.Auth, HTTPClient: cfg.HTTPClient, Headers: cfg.Headers})
	if err != nil {
		return nil, fmt.Errorf("summarizer: %w", err)
	}
	modelID := strings.TrimSpace(cfg.ModelID)
	tokenParam, err := llmclient.ParseChatTokenParam(string(cfg.TokenParam))
	if err != nil {
		return nil, fmt.Errorf("summarizer: %w", err)
	}
	if cfg.ReasoningTokenAllowance < 0 {
		return nil, errors.New("summarizer: reasoning token allowance must not be negative")
	}
	s := &Summarizer{
		client: client, modelID: modelID, tokenParam: tokenParam,
		maxTokens: SummaryMaxOutputTokens + cfg.ReasoningTokenAllowance, extraBody: cfg.ExtraBody,
		prompt: cfg.SystemPrompt, responses: cfg.Responses,
	}
	if !cfg.Responses {
		if _, err := llmclient.WithExtraFields(s.request(""), cfg.ExtraBody); err != nil {
			return nil, fmt.Errorf("summarizer: %w", err)
		}
	}
	if modelID == "" {
		return nil, errors.New("summarizer: a model is required")
	}
	if strings.TrimSpace(cfg.SystemPrompt) == "" {
		return nil, errors.New("summarizer: a system prompt is required")
	}
	return s, nil
}

// ModelID reports the configured model (for the output footer).
func (s *Summarizer) ModelID() string { return s.modelID }

// SummaryUsage is the token accounting returned by the gateway.
type SummaryUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// summaryResponse is the subset of the OpenAI chat-completions
// response the summarizer needs.
type summaryResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage SummaryUsage `json:"usage"`
}

func (s *Summarizer) request(transcript string) map[string]any {
	return map[string]any{
		"model":              s.modelID,
		string(s.tokenParam): s.maxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": s.prompt},
			{"role": "user", "content": transcript},
		},
	}
}

// Summarize calls the gateway with the rendered transcript and returns
// the model's summary text plus token usage.
func (s *Summarizer) Summarize(ctx context.Context, transcript string) (string, SummaryUsage, error) {
	if s.responses {
		result, err := s.client.TextResponse(ctx, llmclient.ResponsesTextRequest{
			Model: s.modelID, Instructions: s.prompt, Input: transcript, MaxOutputTokens: s.maxTokens,
		}, summaryMaxResponseBytes)
		if err != nil {
			return "", SummaryUsage{}, fmt.Errorf("gateway request: %w", err)
		}
		return result.Text, SummaryUsage{PromptTokens: result.InputTokens, CompletionTokens: result.OutputTokens}, nil
	}

	payload, err := llmclient.WithExtraFields(s.request(transcript), s.extraBody)
	if err != nil {
		return "", SummaryUsage{}, err
	}
	body, err := s.client.PostJSON(ctx, "/chat/completions", payload, summaryMaxResponseBytes)
	if err != nil {
		return "", SummaryUsage{}, fmt.Errorf("gateway request: %w", err)
	}
	var out summaryResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", SummaryUsage{}, fmt.Errorf("decode gateway response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", SummaryUsage{}, errors.New("gateway response had no choices")
	}
	return out.Choices[0].Message.Content, out.Usage, nil
}
