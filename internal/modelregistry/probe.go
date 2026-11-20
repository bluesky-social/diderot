package modelregistry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

// TestOptions are what a role's test sends beyond the role's own request:
// Ask's reasoning effort and service tier as the Models page would save
// them.
type TestOptions struct {
	Effort      string
	ServiceTier string
	// HTTPClient is the client requests go through; nil is the default.
	HTTPClient *http.Client
	// PreferMessages makes Ask's test try the Messages protocol first on a
	// model that answers on neither agentic protocol yet: what is published
	// says it is Anthropic's.
	PreferMessages bool
	// Trace, when set, is filled with what the test did.
	Trace *TestTrace
}

// TestTrace is what a role's test did.
type TestTrace struct {
	// Requests is how many requests it sent. One is the job's own request,
	// sent as the entry describes it: a confirmation. More means the model
	// refused that and the test looked for what it takes.
	Requests int
	// Learned names the facts (Fact constants) the test found out, which
	// nothing published or stated had said.
	Learned []string
}

// TestError is a role's test that failed. Reason is a short classification
// (an HTTP status, a timeout, a refused request shape), never a provider's
// response text or a credential.
type TestError struct {
	Reason string
}

func (e *TestError) Error() string { return e.Reason }

// DefaultReasoningTokenAllowance is the allowance a model gets when its
// test finds it needs max_completion_tokens, which counts reasoning: OpenAI
// recommends reserving 25,000 tokens for reasoning and output.
const DefaultReasoningTokenAllowance = 25000

// testTokens is the output limit of a test request: the smallest the
// Responses API accepts.
const testTokens = 16

// thinkingTestTokens is the output limit of a Messages test that turns
// thinking on, which needs room to think before the reply.
const thinkingTestTokens = 1024

// forcedTestTokens is the output limit of a session-summary test, whose
// answer must carry the forced tool call: room for a model that reasons
// before it calls, and for a server that only reports a call it can read to
// its end.
const forcedTestTokens = 1024

const testPrompt = "Reply with OK."

// TestForRole sends m the requests role makes, on p, and returns m with what
// the test learned added: the protocol that answered, and for chat the
// output-limit field and temperature it takes and whether it accepts
// images. A session-summary test also requires the answer to carry the tool
// call the job forces, which is all a summary is read from.
//
// A test only adds. It finds a fact for a model that has no entry yet, and
// records it as the test's (Model.Sources); on a model that has an entry it
// sends the request exactly as the entry describes it, and a refusal fails
// the test: it never looks for another shape, because one refused request
// may be a passing fault and the entry is what the model's jobs send now.
// A fact that was stated, by an administrator or by whoever wrote the
// entry, is never changed.
func TestForRole(ctx context.Context, p Provider, m Model, role Role, opts TestOptions) (Model, error) {
	if reason := p.Unusable(); reason != "" {
		return m, &TestError{Reason: "the provider " + reason}
	}
	client, err := llmclient.New(llmclient.Config{
		BaseURL: p.Endpoint.BaseURL, APIKey: p.Endpoint.APIKey, Auth: p.Kind.Auth(),
		HTTPClient: p.Endpoint.HTTPClient(opts.HTTPClient),
		Headers:    http.Header{"Anthropic-Version": {anthropicVersion}},
	})
	if err != nil {
		return m, &TestError{Reason: "the provider's base URL is not usable"}
	}
	requests := 0
	t := tester{ctx: ctx, client: client, provider: p, requests: &requests}
	if opts.Trace != nil {
		defer func() { opts.Trace.Requests = requests }()
	}
	learned := m
	learned.Protocols = slices.Clone(m.Protocols)
	learned.ReasoningEfforts = slices.Clone(m.ReasoningEfforts)
	learned.TestedFor = slices.Clone(m.TestedFor)
	// Everything about a model nothing was said about is the tests' to
	// find, now and for its later jobs.
	if m.fresh() {
		for _, fact := range RequestFacts {
			learned.learn(fact)
		}
	}
	switch role {
	case RoleAsk:
		err = t.ask(&learned, opts)
	case RoleTitles:
		err = t.titles(&learned)
	case RoleSessionSummaryMap, RoleSessionSummaryReduce:
		err = t.summaries(&learned)
	case RoleImageCaptions, RoleDocumentExtraction, RoleThreadSummaries:
		err = t.chat(&learned, NeedsImages(role), false)
	default:
		return m, &TestError{Reason: "unknown job"}
	}
	if err != nil {
		return m, err
	}
	if !slices.Contains(learned.TestedFor, role) {
		learned.TestedFor = append(learned.TestedFor, role)
	}
	if opts.Trace != nil {
		opts.Trace.Learned = found(m, learned)
		if role == RoleSessionSummaryMap || role == RoleSessionSummaryReduce {
			// Their chat request carries no temperature, so their test
			// finds nothing about it.
			opts.Trace.Learned = slices.DeleteFunc(opts.Trace.Learned, func(fact string) bool { return fact == FactTemperature })
		}
	}
	return learned, nil
}

// found lists the facts whose value the test set: on an entry nothing had
// said anything about, every fact its request relied on.
func found(before, after Model) []string {
	var facts []string
	add := func(fact string, changed bool) {
		if changed && after.Learned(fact) {
			facts = append(facts, fact)
		}
	}
	add(FactProtocols, len(after.Protocols) != len(before.Protocols))
	add(FactImages, after.Images != before.Images)
	add(FactEfforts, !slices.Equal(after.ReasoningEfforts, before.ReasoningEfforts))
	add(FactServiceTier, after.ServiceTier != before.ServiceTier)
	// The chat shape is found when the model first answers on chat.
	firstChat := after.Supports(ProtocolChat) && !before.Supports(ProtocolChat)
	add(FactTokenParam, firstChat || after.ChatTokenParam != before.ChatTokenParam)
	add(FactTemperature, firstChat || after.ChatOmitTemperature != before.ChatOmitTemperature)
	add(FactAllowance, after.ChatReasoningTokenAllowance != before.ChatReasoningTokenAllowance)
	return facts
}

type tester struct {
	ctx      context.Context
	client   *llmclient.Client
	provider Provider
	// requests counts the requests sent.
	requests *int
}

// post sends one request and classifies its failure.
func (t tester) post(path string, body any) error {
	_, err := t.call(path, body)
	return err
}

// call is post, returning the answer.
func (t tester) call(path string, body any) ([]byte, error) {
	*t.requests++
	raw, err := t.client.PostJSON(t.ctx, path, body, 1<<20)
	return raw, classify(t.ctx, err)
}

// summaries sends the forced tool call session summaries make, on the
// protocol they call the model on (Model.RoleProtocol), and requires the
// call in the answer. A model whose entry lists one of their protocols is
// sent that request and nothing else. A model that lists none yet is tried
// on each protocol its provider's kind serves, in SummaryProtocols order,
// until it answers one with the call: a protocol the provider refuses, or
// on which the model answers without calling the tool, moves on to the
// next.
func (t tester) summaries(m *Model) error {
	if protocol, ok := m.RoleProtocol(RoleSessionSummaryMap); ok {
		return t.forcedCall(m, protocol)
	}
	var first error
	for _, protocol := range SummaryProtocols {
		if !slices.Contains(t.provider.Kind.Protocols(), protocol) {
			continue
		}
		err := t.forcedCall(m, protocol)
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
		if !errors.Is(err, errNoToolCall) && !refused(err, http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed) {
			return err
		}
	}
	return first
}

// forcedCall sends the summaries' forced tool call on protocol, and lists
// the protocol when the answer carries the call.
func (t tester) forcedCall(m *Model, protocol Protocol) error {
	switch protocol {
	case ProtocolMessages:
		return t.forcedMessages(m)
	case ProtocolResponses:
		return t.forcedResponses(m, forcedTestTokens, true)
	}
	return t.chat(m, false, true)
}

// errNoToolCall is a model that answered a forced tool call without making
// it.
var errNoToolCall = &TestError{Reason: "it answered without calling the tool the job requires it to call: session summaries need a model that calls tools when told to"}

// calledTool reports whether a protocol's answer carries a call to
// functionTool.
func calledTool(protocol Protocol, raw []byte) bool {
	var answer struct {
		// Messages.
		Content []struct{ Type, Name string } `json:"content"`
		// Responses.
		Output []struct{ Type, Name string } `json:"output"`
		// Chat.
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct{ Name string } `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return false
	}
	switch protocol {
	case ProtocolMessages:
		for _, block := range answer.Content {
			if block.Type == "tool_use" && block.Name == testToolName {
				return true
			}
		}
	case ProtocolResponses:
		for _, item := range answer.Output {
			if item.Type == "function_call" && item.Name == testToolName {
				return true
			}
		}
	case ProtocolChat:
		for _, choice := range answer.Choices {
			for _, call := range choice.Message.ToolCalls {
				if call.Function.Name == testToolName {
					return true
				}
			}
		}
	}
	return false
}

// forcedMessages sends the summaries' forced tool call on Messages.
func (t tester) forcedMessages(m *Model) error {
	body := t.messagesBody(m.ID, functionTool(ProtocolMessages), forcedChoice(ProtocolMessages), "")
	body["max_tokens"] = forcedTestTokens
	raw, err := t.call(messagesPath(t.provider.Endpoint.BaseURL), body)
	if err != nil {
		return err
	}
	if !calledTool(ProtocolMessages, raw) {
		return errNoToolCall
	}
	addProtocol(m, ProtocolMessages)
	return nil
}

// ask tries the agentic protocols the provider's kind serves, the one the
// model was already tested on first.
func (t tester) ask(m *Model, opts TestOptions) error {
	if opts.Effort != "" && !m.SupportsReasoningEffort(opts.Effort) && !m.learnable(FactEfforts) {
		if len(m.ReasoningEfforts) == 0 {
			return &TestError{Reason: "its entry says it takes no reasoning effort: choose the model's default, or correct the entry on the model's page"}
		}
		return &TestError{Reason: fmt.Sprintf("its entry says it takes the reasoning efforts %s, not %s: correct the entry on the model's page if that is wrong",
			strings.Join(m.ReasoningEfforts, ", "), opts.Effort)}
	}
	if opts.ServiceTier != "" && !m.ServiceTier && !m.learnable(FactServiceTier) {
		return &TestError{Reason: "Ask's service tier is set, and its entry says it takes none: leave the service tier not set, or correct the entry on the model's page if that is wrong"}
	}
	candidates := []Protocol{ProtocolResponses, ProtocolMessages}
	switch {
	case t.provider.Kind == KindAnthropic:
		candidates = []Protocol{ProtocolMessages}
	case opts.PreferMessages:
		candidates = []Protocol{ProtocolMessages, ProtocolResponses}
	}
	if known, ok := m.AgenticProtocol(); ok && slices.Contains(candidates, known) {
		candidates = append([]Protocol{known}, slices.DeleteFunc(candidates, func(p Protocol) bool { return p == known })...)
	}
	var first error
	for _, protocol := range candidates {
		var err error
		if protocol == ProtocolResponses {
			_, err = t.responses(m.ID, functionTool(ProtocolResponses), nil, opts.Effort, opts.ServiceTier, testTokens)
		} else {
			if opts.ServiceTier != "" {
				err = &TestError{Reason: "a service tier needs a model that answers on Responses: leave the service tier not set for this model"}
			} else {
				err = t.messages(m.ID, functionTool(ProtocolMessages), nil, opts.Effort)
			}
		}
		if err == nil {
			// Both were checked above to be the test's to add.
			if opts.Effort != "" && !slices.Contains(m.ReasoningEfforts, opts.Effort) {
				m.ReasoningEfforts = append(m.ReasoningEfforts, opts.Effort)
				m.learn(FactEfforts)
			}
			if opts.ServiceTier != "" && !m.ServiceTier {
				m.ServiceTier = true
				m.learn(FactServiceTier)
			}
			addProtocol(m, protocol)
			return nil
		}
		if first == nil {
			first = err
		}
		if !refused(err, http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed) {
			return err
		}
	}
	return first
}

// titles sends the title worker's forced tool call.
func (t tester) titles(m *Model) error { return t.forcedResponses(m, testTokens, false) }

// forcedResponses sends the forced tool call titles and session summaries
// make on Responses, with effort low when the entry lists it. For a model
// whose efforts are the test's to find it tries low and then without, and
// lists low when the model takes it. With called, the answer must carry the
// tool call.
func (t tester) forcedResponses(m *Model, tokens int, called bool) error {
	tool, choice := functionTool(ProtocolResponses), forcedChoice(ProtocolResponses)
	send := func(effort string) error {
		raw, err := t.responses(m.ID, tool, choice, effort, "", tokens)
		if err == nil && called && !calledTool(ProtocolResponses, raw) {
			return errNoToolCall
		}
		return err
	}
	switch {
	case m.SupportsReasoningEffort("low"):
		if err := send("low"); err != nil {
			return err
		}
	case m.learnable(FactEfforts) && !m.Supports(ProtocolResponses):
		err := send("low")
		if err == nil {
			m.ReasoningEfforts = append(m.ReasoningEfforts, "low")
			m.learn(FactEfforts)
			break
		}
		if !refused(err, http.StatusBadRequest) {
			return err
		}
		fallthrough
	default:
		if err := send(""); err != nil {
			return err
		}
	}
	addProtocol(m, ProtocolResponses)
	return nil
}

// chatShape is one way a chat request may carry its output limit and
// temperature.
type chatShape struct {
	param           llmclient.ChatTokenParam
	omitTemperature bool
}

// chatShapes are tried in order; an entry that already names a field or
// leaves temperature off is tried that way first.
var chatShapes = []chatShape{
	{llmclient.ChatMaxTokens, false}, {llmclient.ChatMaxCompletionTokens, false},
	{llmclient.ChatMaxTokens, true}, {llmclient.ChatMaxCompletionTokens, true},
}

// chat sends a chat request, with an image when images, and with the
// summaries' forced tool call, which the answer must carry, when forced. A
// model that already answers on chat is sent the request its entry describes
// and nothing else. A model that does not yet is tried in each shape that is
// the test's to find, until it accepts one. A forced request carries no
// temperature, as the summaries' own carry none, so it finds the
// output-limit field alone.
func (t tester) chat(m *Model, images, forced bool) error {
	if images && !m.Images && !m.learnable(FactImages) {
		return &TestError{Reason: "its entry says it does not accept images, which this job sends: correct the entry on the model's page if that is wrong"}
	}
	known := chatShape{m.ChatTokenParam, m.ChatOmitTemperature}
	if known.param == "" {
		known.param = llmclient.ChatMaxTokens
	}
	shapes := []chatShape{known}
	if !m.Supports(ProtocolChat) {
		for _, shape := range chatShapes {
			if shape == known ||
				forced && shape.omitTemperature != known.omitTemperature ||
				shape.param != known.param && !m.learnable(FactTokenParam) ||
				shape.omitTemperature != known.omitTemperature && !m.learnable(FactTemperature) {
				continue
			}
			shapes = append(shapes, shape)
		}
	}
	var first error
	for _, shape := range shapes {
		err := t.chatRequest(m, shape, images, forced)
		if err != nil {
			if first == nil {
				first = err
			}
			if !refused(err, http.StatusBadRequest) {
				return err
			}
			continue
		}
		if !m.Supports(ProtocolChat) {
			// The shape the model took is the test's finding, where the
			// entry did not already state it.
			if m.learnable(FactTokenParam) {
				m.ChatTokenParam = shape.param
				if shape.param == llmclient.ChatMaxTokens {
					m.ChatTokenParam = ""
				}
				m.learn(FactTokenParam)
			}
			if m.learnable(FactTemperature) && !forced {
				m.ChatOmitTemperature = shape.omitTemperature
				m.learn(FactTemperature)
			}
			if shape.param == llmclient.ChatMaxCompletionTokens && m.ChatReasoningTokenAllowance == 0 && m.learnable(FactAllowance) {
				m.ChatReasoningTokenAllowance = DefaultReasoningTokenAllowance
				m.learn(FactAllowance)
			}
		}
		if images && !m.Images {
			m.Images = true
			m.learn(FactImages)
		}
		addProtocol(m, ProtocolChat)
		return nil
	}
	switch {
	case m.Supports(ProtocolChat) && refused(first, http.StatusBadRequest):
		return &TestError{Reason: "it refused the request its jobs send now (HTTP 400). Nothing about it was changed: if the model itself changed, correct its entry on the model's page"}
	case images:
		return &TestError{Reason: "it refused every request with an image (HTTP 400): it may not accept images"}
	}
	return first
}

func (t tester) chatRequest(m *Model, shape chatShape, images, forced bool) error {
	content := any(testPrompt)
	if images {
		content = []map[string]any{
			{"type": "text", "text": "Describe this image in one word."},
			{"type": "image_url", "image_url": map[string]string{"url": testImage}},
		}
	}
	body := map[string]any{
		"model":    m.ID,
		"messages": []map[string]any{{"role": "user", "content": content}},
	}
	limit := testTokens
	if forced {
		limit = forcedTestTokens
		body["tools"] = []map[string]any{functionTool(ProtocolChat)}
		body["tool_choice"] = forcedChoice(ProtocolChat)
	}
	if shape.param == llmclient.ChatMaxCompletionTokens {
		// The limit counts reasoning: leave room for a little.
		limit += 1024
		body["max_completion_tokens"] = limit
	} else {
		body["max_tokens"] = limit
	}
	if !shape.omitTemperature && !forced {
		body["temperature"] = 0
	}
	for key, value := range m.ChatFields() {
		if _, set := body[key]; !set {
			body[key] = value
		}
	}
	raw, err := t.call("/chat/completions", body)
	if err == nil && forced && !calledTool(ProtocolChat, raw) {
		return errNoToolCall
	}
	return err
}

// responses sends one Responses request offering tool, and forcing it when
// choice is set.
func (t tester) responses(model string, tool, choice map[string]any, effort, tier string, tokens int) ([]byte, error) {
	body := map[string]any{
		"model": model, "input": testPrompt, "max_output_tokens": tokens, "store": false,
		"tools": []map[string]any{tool},
	}
	if choice != nil {
		body["tool_choice"] = choice
	}
	if effort != "" {
		body["reasoning"] = map[string]string{"effort": effort}
	}
	if tier != "" {
		body["service_tier"] = tier
	}
	return t.call("/responses", body)
}

// messages sends one Messages request offering tool, and forcing it when
// choice is set: effort "none" turns thinking off and any other effort turns
// adaptive thinking on at that effort, as Ask does.
func (t tester) messages(model string, tool, choice map[string]any, effort string) error {
	return t.post(messagesPath(t.provider.Endpoint.BaseURL), t.messagesBody(model, tool, choice, effort))
}

func (t tester) messagesBody(model string, tool, choice map[string]any, effort string) map[string]any {
	body := map[string]any{
		"model": model, "max_tokens": testTokens,
		"messages": []map[string]any{{"role": "user", "content": testPrompt}},
		"tools":    []map[string]any{tool},
	}
	if choice != nil {
		body["tool_choice"] = choice
	}
	switch effort {
	case "":
	case "none":
		body["thinking"] = map[string]string{"type": "disabled"}
	default:
		body["max_tokens"] = thinkingTestTokens
		body["thinking"] = map[string]string{"type": "adaptive"}
		body["output_config"] = map[string]string{"effort": effort}
	}
	return body
}

// messagesPath is the Messages path under a base URL that may or may not
// end in /v1.
func messagesPath(base string) string {
	if strings.HasSuffix(strings.TrimRight(base, "/"), "/v1") {
		return "/messages"
	}
	return "/v1/messages"
}

// testToolName names functionTool.
const testToolName = "lookup"

// functionTool is a tool a request offers without forcing it, as Ask's do.
func functionTool(protocol Protocol) map[string]any {
	schema := map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}
	switch protocol {
	case ProtocolMessages:
		return map[string]any{"name": testToolName, "description": "Look something up.", "input_schema": schema}
	case ProtocolChat:
		return map[string]any{"type": "function", "function": map[string]any{
			"name": testToolName, "description": "Look something up.", "parameters": schema,
		}}
	}
	return map[string]any{"type": "function", "name": testToolName, "description": "Look something up.", "parameters": schema}
}

// forcedChoice forces functionTool, as session titles and summaries force
// theirs.
func forcedChoice(protocol Protocol) map[string]any {
	switch protocol {
	case ProtocolMessages:
		return map[string]any{"type": "tool", "name": testToolName, "disable_parallel_tool_use": true}
	case ProtocolChat:
		return map[string]any{"type": "function", "function": map[string]any{"name": testToolName}}
	}
	return map[string]any{"type": "function", "name": testToolName}
}

// addProtocol lists a protocol the model answered on. On a model nothing
// was said about, the list is the test's finding; one that was stated only
// grows.
func addProtocol(m *Model, protocol Protocol) {
	if m.Supports(protocol) {
		return
	}
	if m.fresh() || m.Learned(FactProtocols) {
		m.learn(FactProtocols)
	}
	m.Protocols = append(m.Protocols, protocol)
}

// refused reports whether err is one of statuses.
func refused(err error, statuses ...int) bool {
	var testErr *TestError
	if !errors.As(err, &testErr) {
		return false
	}
	for _, status := range statuses {
		if testErr.Reason == statusReason(status) {
			return true
		}
	}
	return false
}

func statusReason(status int) string {
	return fmt.Sprintf("HTTP %d %s", status, http.StatusText(status))
}

// classify turns a request's error into a TestError without the provider's
// response body, the endpoint URL, or a credential.
func classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var status *llmclient.HTTPError
	var urlErr *url.Error
	var netErr net.Error
	switch {
	case errors.As(err, &status):
		return &TestError{Reason: statusReason(status.StatusCode)}
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return &TestError{Reason: "no answer in time"}
	case errors.As(err, &urlErr), errors.As(err, &netErr):
		return &TestError{Reason: "could not reach the provider"}
	}
	return &TestError{Reason: "unexpected response"}
}

// testImage is a small PNG, as a data URL, that caption tests send.
var testImage = func() string {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for x := range 32 {
		for y := range 32 {
			img.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}()
