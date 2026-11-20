package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// ErrToolInvariant marks a tool failure that must abort the agent run rather
// than being shown to the model as a recoverable result. Tool implementations
// should wrap this sentinel only for violated server-side invariants, never for
// bad model arguments or ordinary dependency failures.
var ErrToolInvariant = errors.New("ask tool invariant")

// Result size limits. A tool result stays in the conversation and is paid for
// again on every later model call, so no tool may return without a bound.
// The built-in tools bound themselves below these (expand and code_read at
// 64 KiB of content, list by rows, code_search by its own budget); the root
// limit sits above the largest result those bounds allow, so that it only
// ever cuts a tool with no limit of its own. The investigator's tools are
// another server's (Grafana, Sentry) and have no limit at all: a single step
// of an investigation can return hundreds of thousands of tokens.
const (
	RootToolResultMaxBytes  = 128 << 10
	ChildToolResultMaxBytes = 64 << 10
)

// WrapRecoverableToolErrors applies Ask's tool contract to native Eino tools.
//
// Errors: ordinary invocation and argument-decoding errors become JSON
// results the model can inspect and recover from. Cancellation, deadlines,
// Eino interrupt/rerun signals, and ErrToolInvariant still abort the run.
//
// Arguments: a property the model set to null, and which the tool's schema
// does not require, is removed before the tool runs (dropUnsetArguments).
//
// Results: text longer than maxResultBytes is cut there, with a line that
// says how much was cut and how to ask for less.
//
// Ask tools are deliberately invokable rather than streamable. Rejecting any
// other shape at startup keeps this policy explicit instead of growing a
// second generic tool abstraction beside Eino.
func WrapRecoverableToolErrors(tools []tool.BaseTool, maxResultBytes int) ([]tool.BaseTool, error) {
	if maxResultBytes <= 0 {
		return nil, errors.New("wrap ask tools: a result size limit is required")
	}
	wrapped := make([]tool.BaseTool, len(tools))
	for i, base := range tools {
		if base == nil {
			return nil, fmt.Errorf("wrap ask tool %d: nil tool", i+1)
		}
		// The wrapper stays optional when its tool is.
		available := func() bool { return true }
		if optional, ok := base.(OptionalTool); ok {
			available = optional.Available
		}
		if enhanced, ok := base.(tool.EnhancedInvokableTool); ok {
			wrapped[i] = &recoverableEnhancedTool{
				info:           base.Info,
				invoke:         enhanced.InvokableRun,
				available:      available,
				maxResultBytes: maxResultBytes,
			}
			continue
		}
		invokable, ok := base.(tool.InvokableTool)
		if !ok {
			return nil, fmt.Errorf("wrap ask tool %d: %T is not invokable or enhanced-invokable", i+1, base)
		}
		wrapped[i] = &recoverableTool{
			info:           base.Info,
			invoke:         invokable.InvokableRun,
			available:      available,
			maxResultBytes: maxResultBytes,
		}
	}
	return wrapped, nil
}

type recoverableEnhancedTool struct {
	info           func(context.Context) (*schema.ToolInfo, error)
	invoke         func(context.Context, *schema.ToolArgument, ...tool.Option) (*schema.ToolResult, error)
	available      func() bool
	maxResultBytes int
}

func (t *recoverableEnhancedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.info(ctx)
}

func (t *recoverableEnhancedTool) Available() bool { return t.available() }

func (t *recoverableEnhancedTool) InvokableRun(ctx context.Context, argument *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if argument != nil {
		set := *argument
		set.Text = dropUnsetArguments(set.Text, toolParameters(ctx, t.info))
		argument = &set
	}
	result, err := t.invoke(ctx, argument, opts...)
	if _, ok := compose.IsInterruptRerunError(err); ok {
		return result, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if err == nil {
		return boundToolResultParts(result, t.maxResultBytes), nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrToolInvariant) {
		return result, err
	}
	payload, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		return result, fmt.Errorf("encode recoverable enhanced tool error: %w", marshalErr)
	}
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: boundToolResult(string(payload), t.maxResultBytes)}}}, nil
}

type recoverableTool struct {
	info           func(context.Context) (*schema.ToolInfo, error)
	invoke         func(context.Context, string, ...tool.Option) (string, error)
	available      func() bool
	maxResultBytes int
}

func (t *recoverableTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.info(ctx)
}

func (t *recoverableTool) Available() bool { return t.available() }

func (t *recoverableTool) InvokableRun(ctx context.Context, arguments string, opts ...tool.Option) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	arguments = dropUnsetArguments(arguments, toolParameters(ctx, t.info))
	result, err := t.invoke(ctx, arguments, opts...)
	if _, ok := compose.IsInterruptRerunError(err); ok {
		return result, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if err == nil {
		return boundToolResult(result, t.maxResultBytes), nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrToolInvariant) {
		return result, err
	}
	// Eino's local-function adapter wraps both decoder and function errors with
	// implementation details such as "[LocalFunc]" and the tool name. Retain
	// the most specific wrapped cause, which is the same application/decoder
	// error the legacy Ask loop exposed to the model.
	visible := err
	if strings.HasPrefix(visible.Error(), "[LocalFunc]") {
		if unwrapped := errors.Unwrap(visible); unwrapped != nil {
			visible = unwrapped
		}
	}
	payload, marshalErr := json.Marshal(map[string]string{"error": visible.Error()})
	if marshalErr != nil {
		return result, fmt.Errorf("encode recoverable tool error: %w", marshalErr)
	}
	return boundToolResult(string(payload), t.maxResultBytes), nil
}

// toolParameters returns a tool's parameter schema as decoded JSON, or nil
// when the tool cannot say. It is read on each call because an optional
// tool's description follows settings that change while the server runs.
func toolParameters(ctx context.Context, info func(context.Context) (*schema.ToolInfo, error)) map[string]any {
	described, err := info(ctx)
	if err != nil || described == nil || described.ParamsOneOf == nil {
		return nil
	}
	parameters, err := described.ToJSONSchema()
	if err != nil || parameters == nil {
		return nil
	}
	raw, err := json.Marshal(parameters)
	if err != nil {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded
}

// boundToolResult cuts a result longer than limit bytes at a character
// boundary and says so on a last line: how long the result was, how much of
// it is shown, and that the way to the rest is a narrower request. The cut
// depends only on the text, so the same result is always cut the same way.
func boundToolResult(result string, limit int) string {
	if len(result) <= limit {
		return result
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(result[cut]) {
		cut--
	}
	return result[:cut] + toolResultCutLine(len(result), cut)
}

func toolResultCutLine(size, shown int) string {
	return fmt.Sprintf(
		"\n…[cut here: this result is %d bytes and only the first %d are shown. The rest was not read. Ask again for less: a tighter filter, a shorter time range, fewer rows or series, or a lower limit.]",
		size, shown)
}

// boundToolResultParts holds the text of a multimodal result to limit bytes
// in all: the parts share the limit in order, so a result cannot exceed it
// by arriving in pieces. The part that crosses the limit is cut there, with
// the line boundToolResult writes giving the size of the text as a whole,
// and the text parts after it are left out. Images and other parts stay.
func boundToolResultParts(result *schema.ToolResult, limit int) *schema.ToolResult {
	if result == nil {
		return nil
	}
	total := 0
	for _, part := range result.Parts {
		if part.Type == schema.ToolPartTypeText {
			total += len(part.Text)
		}
	}
	if total <= limit {
		return result
	}
	bounded := *result
	bounded.Parts = make([]schema.ToolOutputPart, 0, len(result.Parts))
	remaining, cut := limit, false
	for _, part := range result.Parts {
		switch {
		case part.Type != schema.ToolPartTypeText:
		case cut:
			continue
		case len(part.Text) <= remaining:
			remaining -= len(part.Text)
		default:
			kept := remaining
			for kept > 0 && !utf8.RuneStart(part.Text[kept]) {
				kept--
			}
			part.Text = part.Text[:kept] + toolResultCutLine(total, limit-remaining+kept)
			cut = true
		}
		bounded.Parts = append(bounded.Parts, part)
	}
	return &bounded
}

const maxUnknownToolNameRunes = 128

// UnknownToolResult is installed as Eino's ToolsNodeConfig.UnknownToolsHandler.
// It never echoes arguments and bounds the untrusted model-produced name.
func UnknownToolResult(ctx context.Context, name, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "<empty>"
	}
	runes := []rune(name)
	if len(runes) > maxUnknownToolNameRunes {
		name = string(runes[:maxUnknownToolNameRunes]) + "…"
	}
	payload, err := json.Marshal(map[string]string{
		"error": fmt.Sprintf("unknown tool %q; choose one of the advertised tools", name),
	})
	if err != nil {
		return "", fmt.Errorf("encode unknown-tool result: %w", err)
	}
	return string(payload), nil
}
