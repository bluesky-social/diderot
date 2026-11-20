package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// ToolPresentation carries the application-facing status metadata for one
// native Eino tool. It deliberately contains no schema, name, or invocation:
// those live on the Eino tool itself and are validated centrally at
// registration. Every callback is optional; a nil field means the tool has
// nothing useful to render for that surface.
type ToolPresentation struct {
	// Status renders the operator-facing progress line for one invocation
	// from the raw model-produced tool input (args JSON).
	Status func(input json.RawMessage) StatusLine

	// StatusSources extracts URL references from a successful tool output.
	StatusSources func(output json.RawMessage) []StatusSource

	// StatusOutput returns a short, safe summary of a successful tool
	// output. Implementations must never leak raw tool output or model
	// reasoning: return a concise description (e.g. hit counts, page
	// titles) and fall back to "" for malformed or unsummarizable output.
	StatusOutput func(output json.RawMessage) string
}

// RegisteredTool pairs one native Eino tool with its application-facing
// presentation metadata. The presentation entry is keyed by the Eino tool's
// Info() name, so it must describe exactly the tool it sits next to.
type RegisteredTool struct {
	Tool         tool.BaseTool
	Presentation ToolPresentation
}

// OptionalTool is a tool Ask offers only while it is available. Each run asks
// once, as it starts: a run that starts while Available reports false neither
// lists the tool to the model nor runs it.
type OptionalTool interface {
	tool.BaseTool
	Available() bool
}

// optionalTool is a native tool that follows settings which change while the
// server runs: available decides whether a run offers it, and info describes
// it as of the run's start.
type optionalTool struct {
	tool.InvokableTool
	info      func(context.Context) (*schema.ToolInfo, error)
	available func() bool
}

func (t *optionalTool) Info(ctx context.Context) (*schema.ToolInfo, error) { return t.info(ctx) }

func (t *optionalTool) Available() bool { return t.available() }

// errEmptyToolInfo is returned when an Eino BaseTool reports an Info() result
// that cannot be registered: a nil or empty Name, an empty Desc, or a nil
// parameter schema. Eino itself treats a nil ParamsOneOf as "takes no
// arguments", but Diderot's registered tools are passed to models through the
// JSON-schema serialization path, which requires a concrete schema.
var errEmptyToolInfo = errors.New("tool info is empty: name, description, and parameter schema are required")

// NewToolRegistry validates the given registrations and returns the ordered
// Eino tool list plus presentation metadata keyed by Eino tool name.
//
// It calls each tool's Info(ctx) and rejects:
//   - nil tools and presentation entries that do not correspond one-to-one
//     with the tool list;
//   - Info() errors, nil info, empty names, empty descriptions, and nil
//     parameter schemas;
//   - schemas that fail to serialize as valid JSON Schema;
//   - duplicate names.
//
// The returned tool list preserves registration order; presentation lookup is
// via the presentation map.
func NewToolRegistry(ctx context.Context, registrations []RegisteredTool) ([]tool.BaseTool, map[string]ToolPresentation, error) {
	tools := make([]tool.BaseTool, 0, len(registrations))
	presentations := make(map[string]ToolPresentation, len(registrations))
	seen := make(map[string]struct{}, len(registrations))

	for i, reg := range registrations {
		index := i + 1
		if reg.Tool == nil {
			return nil, nil, fmt.Errorf("tool registration %d: nil tool", index)
		}
		if reg.Presentation.Status == nil && reg.Presentation.StatusSources == nil && reg.Presentation.StatusOutput == nil {
			return nil, nil, fmt.Errorf("tool registration %d: missing presentation", index)
		}

		info, err := reg.Tool.Info(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("tool registration %d: info: %w", index, err)
		}
		if err := validateToolInfo(info); err != nil {
			return nil, nil, fmt.Errorf("tool registration %d: %w", index, err)
		}

		if _, exists := seen[info.Name]; exists {
			return nil, nil, fmt.Errorf("tool registration %d: duplicate tool name %q", index, info.Name)
		}

		tools = append(tools, reg.Tool)
		presentations[info.Name] = reg.Presentation
		seen[info.Name] = struct{}{}
	}
	return tools, presentations, nil
}

// validateToolInfo checks the requirements common to every registered tool:
// a non-nil ToolInfo with a non-empty name and description, and a parameter
// schema that serializes to valid JSON Schema.
func validateToolInfo(info *schema.ToolInfo) error {
	if info == nil {
		return errEmptyToolInfo
	}
	if strings.TrimSpace(info.Name) == "" {
		return fmt.Errorf("%w: name is empty", errEmptyToolInfo)
	}
	if !validResponsesToolName(info.Name) {
		return fmt.Errorf("tool %q: name must be 1-64 ASCII letters, digits, underscores, or hyphens", info.Name)
	}
	if strings.TrimSpace(info.Desc) == "" {
		return fmt.Errorf("tool %q: %w: description is empty", info.Name, errEmptyToolInfo)
	}
	if info.ParamsOneOf == nil {
		return fmt.Errorf("tool %q: %w: parameter schema is nil", info.Name, errEmptyToolInfo)
	}
	modelSchema, err := info.ToJSONSchema()
	if err != nil {
		return fmt.Errorf("tool %q: invalid JSON-schema conversion: %w", info.Name, err)
	}
	if modelSchema == nil {
		return fmt.Errorf("tool %q: %w: parameter schema is empty", info.Name, errEmptyToolInfo)
	}
	rawSchema, err := json.Marshal(modelSchema)
	if err != nil {
		return fmt.Errorf("tool %q: invalid JSON-schema serialization: %w", info.Name, err)
	}
	var modelParameters map[string]any
	if err := json.Unmarshal(rawSchema, &modelParameters); err != nil {
		return fmt.Errorf("tool %q: parameter schema must serialize as a JSON object: %w", info.Name, err)
	}
	if modelParameters == nil {
		return fmt.Errorf("tool %q: %w: parameter schema is empty", info.Name, errEmptyToolInfo)
	}
	if schemaType, _ := modelParameters["type"].(string); schemaType != string(schema.Object) {
		return fmt.Errorf("tool %q: parameter schema type must be %q, got %v", info.Name, schema.Object, modelParameters["type"])
	}
	return nil
}

func validResponsesToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
