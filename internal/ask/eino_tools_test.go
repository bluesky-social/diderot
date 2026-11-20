package ask

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

type registryArgs struct {
	Query string `json:"query"`
}

type registryResult struct {
	Count int `json:"count"`
}

func registryTool(name, desc string) tool.BaseTool {
	bt, err := utils.InferTool(name, desc, func(_ context.Context, _ registryArgs) (registryResult, error) {
		return registryResult{Count: 1}, nil
	})
	if err != nil {
		panic(err)
	}
	return bt
}

func visibleRegistryTool(name, desc string) tool.BaseTool {
	return utils.NewTool(&schema.ToolInfo{
		Name: name,
		Desc: desc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"query": {Type: schema.String, Desc: "search query"},
		}),
	}, func(_ context.Context, _ registryArgs) (registryResult, error) {
		return registryResult{Count: 1}, nil
	})
}

func registryPresentation(name string) ToolPresentation {
	status := func(input json.RawMessage) StatusLine {
		sl := StatusLine{Emoji: "gear", Verb: "registry-tool"}
		var probe registryArgs
		if json.Unmarshal(input, &probe) != nil {
			return sl
		}
		sl.Detail = truncateStatusDetail(probe.Query)
		return sl
	}
	sources := func(output json.RawMessage) []StatusSource {
		return []StatusSource{{URL: "https://diderot.example/" + name}}
	}
	summary := func(output json.RawMessage) string {
		var out registryResult
		if json.Unmarshal(output, &out) != nil {
			return ""
		}
		return formatStatusCount(out.Count, "registry result", "registry results")
	}
	return ToolPresentation{Status: status, StatusSources: sources, StatusOutput: summary}
}

func TestNewToolRegistryValid(t *testing.T) {
	ctx := context.Background()
	registrations := []RegisteredTool{
		{Tool: registryTool("alpha_tool", "alpha description"), Presentation: registryPresentation("alpha_tool")},
		{Tool: visibleRegistryTool("beta_tool", "beta description"), Presentation: registryPresentation("beta_tool")},
		{Tool: registryTool("gamma_tool", "gamma description"), Presentation: registryPresentation("gamma_tool")},
	}

	tools, presentations, err := NewToolRegistry(ctx, registrations)
	if err != nil {
		t.Fatalf("NewToolRegistry: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("got %d tools, want 3", len(tools))
	}

	// Registration order must be preserved in the tool list.
	wantOrder := []string{"alpha_tool", "beta_tool", "gamma_tool"}
	for i, tool := range tools {
		info, err := tool.Info(ctx)
		if err != nil {
			t.Fatalf("tool %d Info: %v", i, err)
		}
		if info.Name != wantOrder[i] {
			t.Errorf("tool %d name = %q, want %q", i, info.Name, wantOrder[i])
		}
	}

	if len(presentations) != 3 {
		t.Fatalf("got %d presentations, want 3", len(presentations))
	}
	for _, name := range wantOrder {
		p, ok := presentations[name]
		if !ok {
			t.Fatalf("presentation %q missing", name)
		}
		if p.Status == nil || p.StatusSources == nil || p.StatusOutput == nil {
			t.Fatalf("presentation %q has nil callbacks", name)
		}
		line := p.Status(json.RawMessage(`{"query":"registry alpha"}`))
		if line.Emoji != "gear" || line.Verb != "registry-tool" || line.Detail != "registry alpha" {
			t.Errorf("presentation %q status = %+v, want gear/registry-tool/registry alpha", name, line)
		}
		srcs := p.StatusSources(json.RawMessage(`{"count":1}`))
		if len(srcs) != 1 || srcs[0].URL != "https://diderot.example/"+name {
			t.Errorf("presentation %q sources = %+v", name, srcs)
		}
		wantSummary := "Found 1 registry result"
		if got := p.StatusOutput(json.RawMessage(`{"count":1}`)); got != wantSummary {
			t.Errorf("presentation %q summary = %q, want %q", name, got, wantSummary)
		}
	}
}

func TestNewToolRegistryRejects(t *testing.T) {
	ctx := context.Background()
	badInfo := &errInfoTool{infoErr: true}
	dupTool := registryTool("dup_tool", "dup description")
	noSchema := utils.NewTool(&schema.ToolInfo{Name: "no_schema", Desc: "no schema"}, func(_ context.Context, _ registryArgs) (registryResult, error) {
		return registryResult{}, nil
	})
	noDesc := utils.NewTool(&schema.ToolInfo{
		Name:        "no_desc",
		Desc:        "   ",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"query": {Type: schema.String}}),
	}, func(_ context.Context, _ registryArgs) (registryResult, error) {
		return registryResult{}, nil
	})
	emptySchema := utils.NewTool(&schema.ToolInfo{
		Name:        "empty_schema",
		Desc:        "empty schema",
		ParamsOneOf: &schema.ParamsOneOf{},
	}, func(_ context.Context, _ registryArgs) (registryResult, error) {
		return registryResult{}, nil
	})
	emptyJSONSchema := utils.NewTool(&schema.ToolInfo{
		Name:        "empty_json_schema",
		Desc:        "empty JSON schema",
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&jsonschema.Schema{}),
	}, func(_ context.Context, _ registryArgs) (registryResult, error) {
		return registryResult{}, nil
	})
	nonObjectSchema := utils.NewTool(&schema.ToolInfo{
		Name: "non_object_schema",
		Desc: "non-object JSON schema",
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&jsonschema.Schema{
			Type: "string",
		}),
	}, func(_ context.Context, _ registryArgs) (registryResult, error) {
		return registryResult{}, nil
	})

	tests := []struct {
		name          string
		registrations []RegisteredTool
		want          string
	}{
		{name: "empty registration list", registrations: nil, want: ""},
		{name: "nil tool", registrations: []RegisteredTool{{Presentation: registryPresentation("nil_tool")}}, want: "nil tool"},
		{name: "missing presentation", registrations: []RegisteredTool{{Tool: registryTool("no_pres", "no pres")}}, want: "missing presentation"},
		{name: "empty presentation", registrations: []RegisteredTool{{Tool: registryTool("empty_pres", "empty pres"), Presentation: ToolPresentation{}}}, want: "missing presentation"},
		{name: "info error", registrations: []RegisteredTool{{Tool: badInfo, Presentation: registryPresentation("bad_info")}}, want: "info"},
		{name: "nil info", registrations: []RegisteredTool{{Tool: errInfoTool{}, Presentation: registryPresentation("nil_info")}}, want: "tool info is empty"},
		{name: "empty name", registrations: []RegisteredTool{{Tool: registryTool("  ", "desc"), Presentation: registryPresentation("   ")}}, want: "name is empty"},
		{name: "surrounding whitespace in name", registrations: []RegisteredTool{{Tool: registryTool(" bad_name", "desc"), Presentation: registryPresentation("bad_name")}}, want: "name must be"},
		{name: "embedded whitespace in name", registrations: []RegisteredTool{{Tool: registryTool("bad name", "desc"), Presentation: registryPresentation("bad_name")}}, want: "name must be"},
		{name: "punctuation in name", registrations: []RegisteredTool{{Tool: registryTool("bad.name", "desc"), Presentation: registryPresentation("bad_name")}}, want: "name must be"},
		{name: "overlong name", registrations: []RegisteredTool{{Tool: registryTool(strings.Repeat("a", 65), "desc"), Presentation: registryPresentation("long")}}, want: "name must be"},
		{name: "empty description", registrations: []RegisteredTool{{Tool: noDesc, Presentation: registryPresentation("no_desc")}}, want: "description is empty"},
		{name: "nil parameter schema", registrations: []RegisteredTool{{Tool: noSchema, Presentation: registryPresentation("no_schema")}}, want: "parameter schema is nil"},
		{name: "empty parameter schema", registrations: []RegisteredTool{{Tool: emptySchema, Presentation: registryPresentation("empty_schema")}}, want: "parameter schema is empty"},
		{name: "empty JSON schema", registrations: []RegisteredTool{{Tool: emptyJSONSchema, Presentation: registryPresentation("empty_json_schema")}}, want: "JSON object"},
		{name: "non-object JSON schema", registrations: []RegisteredTool{{Tool: nonObjectSchema, Presentation: registryPresentation("non_object_schema")}}, want: "type must be"},
		{name: "duplicate names", registrations: []RegisteredTool{{Tool: dupTool, Presentation: registryPresentation("dup_tool")}, {Tool: dupTool, Presentation: registryPresentation("dup_tool")}}, want: "duplicate tool name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools, presentations, err := NewToolRegistry(ctx, tt.registrations)
			if err != nil {
				if tt.want == "" || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("NewToolRegistry error = %v, want substring %q", err, tt.want)
				}
				return
			}
			if tt.want != "" {
				t.Fatalf("NewToolRegistry succeeded, want error containing %q", tt.want)
			}
			if len(tools) != 0 || len(presentations) != 0 {
				t.Fatalf("got %d tools and %d presentations for empty registration list, want none", len(tools), len(presentations))
			}
		})
	}
}

func TestNewToolRegistryInvalidJSONSchema(t *testing.T) {
	ctx := context.Background()
	info := &schema.ToolInfo{
		Name: "bad_schema",
		Desc: "bad schema",
		// Setting both Type forms makes the schema's JSON marshaler error,
		// so the registry's serialization check must reject this tool.
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&jsonschema.Schema{
			Type:         "object",
			TypeEnhanced: []string{"object"},
		}),
	}
	badSchema := utils.NewTool(info, func(_ context.Context, _ registryArgs) (registryResult, error) {
		return registryResult{}, nil
	})

	tools, presentations, err := NewToolRegistry(ctx, []RegisteredTool{{Tool: badSchema, Presentation: registryPresentation("bad_schema")}})
	if err == nil {
		t.Fatal("NewToolRegistry succeeded, want invalid JSON-schema serialization error")
	}
	if !strings.Contains(err.Error(), "invalid JSON-schema serialization") {
		t.Fatalf("error = %v, want invalid JSON-schema serialization", err)
	}
	if tools != nil || presentations != nil {
		t.Fatalf("got tools/presentations on error, want nil")
	}
}

// errInfoTool is a BaseTool that fails or returns nil ToolInfo, for
// registration validation tests.
type errInfoTool struct {
	infoErr bool
}

func (t errInfoTool) Info(context.Context) (*schema.ToolInfo, error) {
	if t.infoErr {
		return nil, errors.New("boom")
	}
	return nil, nil
}
