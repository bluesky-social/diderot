package ask

import (
	"context"
	"fmt"
	"strings"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// AskAgents bundles the per-surface typed chat model agents and runners for
// the Eino Responses migration. Construction wires the shared Responses
// model into two stock ADK agents with surface-specific system prompts and
// non-streaming runners; it performs no I/O and adds no lifecycle,
// persistence, or metadata logic.
type AskAgents struct {
	HTTP *adk.TypedRunner[*schema.AgenticMessage]
	// Slack is the runner for the native Slack markdown-block surface.
	Slack *adk.TypedRunner[*schema.AgenticMessage]
}

var embeddedAskPrompts = promptcatalog.MustEmbedded()

var answerDisciplineAddendum = embeddedAskPrompts.MustText(promptcatalog.AskAnswerDiscipline)

// AnswerDisciplineInstructions exposes the deploy-managed correctness rules
// for prompt inspection surfaces and tests.
func AnswerDisciplineInstructions() string { return answerDisciplineAddendum }

var observabilityDelegationAddendum = embeddedAskPrompts.MustText(promptcatalog.AskObservabilityDelegation)

var terminalOutputAddendum = embeddedAskPrompts.MustText(promptcatalog.AskTerminalOutput)

// ObservabilityDelegationInstructions exposes the deploy-managed delegation
// addendum for prompt inspection surfaces and tests.
func ObservabilityDelegationInstructions() string { return observabilityDelegationAddendum }

func TerminalOutputInstructions() string { return terminalOutputAddendum }

// AskInstructions is the root prompt subset of one atomic prompt release.
type AskInstructions struct {
	Base string
	// Organization is the install's note about itself, empty when it has
	// none; it follows Base.
	Organization            string
	AnswerDiscipline        string
	TerminalOutput          string
	SurfaceHTTP             string
	SurfaceSlack            string
	ObservabilityDelegation string
}

func AskInstructionsFromBundle(bundle promptcatalog.Bundle) (AskInstructions, error) {
	base, err := bundle.Text(promptcatalog.AskBase)
	if err != nil {
		return AskInstructions{}, err
	}
	answer, err := bundle.Text(promptcatalog.AskAnswerDiscipline)
	if err != nil {
		return AskInstructions{}, err
	}
	terminal, err := bundle.Text(promptcatalog.AskTerminalOutput)
	if err != nil {
		return AskInstructions{}, err
	}
	httpPrompt, err := bundle.Text(promptcatalog.AskSurfaceHTTP)
	if err != nil {
		return AskInstructions{}, err
	}
	slackPrompt, err := bundle.Text(promptcatalog.AskSurfaceSlack)
	if err != nil {
		return AskInstructions{}, err
	}
	delegation, err := bundle.Text(promptcatalog.AskObservabilityDelegation)
	if err != nil {
		return AskInstructions{}, err
	}
	return AskInstructions{
		Base: base, Organization: bundle.NoteText(promptcatalog.AskOrganization),
		AnswerDiscipline: answer, TerminalOutput: terminal,
		SurfaceHTTP: httpPrompt, SurfaceSlack: slackPrompt,
		ObservabilityDelegation: delegation,
	}, nil
}

func defaultAskInstructions(basePrompt string) AskInstructions {
	instructions, err := AskInstructionsFromBundle(embeddedAskPrompts)
	if err != nil {
		panic(err)
	}
	instructions.Base = basePrompt
	return instructions
}

func agentInstructionWith(instructions AskInstructions, surface Surface, tools []tool.BaseTool) string {
	surfacePrompt := instructions.SurfaceHTTP
	if surface == SurfaceSlack {
		surfacePrompt = instructions.SurfaceSlack
	}
	instruction := instructions.Base
	if instructions.Organization != "" {
		instruction += "\n\n" + instructions.Organization
	}
	instruction += "\n\n" + instructions.AnswerDiscipline + "\n\n" + surfacePrompt
	if toolsIncludeInvestigate(tools) {
		instruction += "\n\n" + instructions.ObservabilityDelegation
	}
	instruction += "\n\n" + instructions.TerminalOutput
	return instruction
}

func agentInstruction(basePrompt string, surface Surface, tools []tool.BaseTool) string {
	return agentInstructionWith(defaultAskInstructions(basePrompt), surface, tools)
}

// toolsIncludeInvestigate reports whether tools contains the observability
// investigator delegation tool. Nil/unnamed tools are skipped rather than
// treated as an error here, mirroring ensureRespondTool's tolerance, since
// this only gates whether an instruction addendum is appended.
func toolsIncludeInvestigate(tools []tool.BaseTool) bool {
	for _, candidate := range tools {
		if candidate == nil {
			continue
		}
		info, err := candidate.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		if info.Name == InvestigateToolName {
			return true
		}
	}
	return false
}

// NewAskAgents validates its inputs and constructs the two ask agents/runners
// for the HTTP and Slack surfaces. Each agent gets the same basePrompt
// followed by the surface's own prompt, the same native
// tools, ExecuteSequentially=false (tools run in parallel), and
// MaxIterations as a hard guard on model generation cycles. Runners are
// non-streaming and have no checkpoint store; the returned struct holds no
// model or tool references beyond what the runners keep internally.
func NewAskAgents(ctx context.Context, mdl model.AgenticModel, tools []tool.BaseTool, basePrompt string, maxIterations int) (*AskAgents, error) {
	if strings.TrimSpace(basePrompt) == "" {
		return nil, fmt.Errorf("ask agents: basePrompt is required")
	}
	return NewAskAgentsWithInstructions(ctx, mdl, tools, defaultAskInstructions(basePrompt), maxIterations)
}

// NewAskAgentsWithInstructions constructs root agents from one resolved,
// immutable prompt release.
func NewAskAgentsWithInstructions(ctx context.Context, mdl model.AgenticModel, tools []tool.BaseTool, instructions AskInstructions, maxIterations int) (*AskAgents, error) {
	if mdl == nil {
		return nil, fmt.Errorf("ask agents: model is required")
	}
	for name, value := range map[string]string{
		"base": instructions.Base, "answer discipline": instructions.AnswerDiscipline,
		"terminal output": instructions.TerminalOutput,
		"HTTP surface":    instructions.SurfaceHTTP, "Slack surface": instructions.SurfaceSlack,
		"observability delegation": instructions.ObservabilityDelegation,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("ask agents: %s instruction is required", name)
		}
	}
	if maxIterations <= 0 {
		return nil, fmt.Errorf("ask agents: maxIterations must be positive")
	}
	wrappedTools, err := WrapRecoverableToolErrors(tools, RootToolResultMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("ask agents: apply tool error policy: %w", err)
	}

	newAgent := func(surface Surface) (*adk.TypedChatModelAgent[*schema.AgenticMessage], error) {
		agentCfg := &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
			Model:         mdl,
			Instruction:   agentInstructionWith(instructions, surface, tools),
			MaxIterations: maxIterations,
			ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools:               wrappedTools,
				UnknownToolsHandler: recordingUnknownToolResult,
				ExecuteSequentially: false,
			}},
			Handlers: []adk.TypedChatModelAgentMiddleware[*schema.AgenticMessage]{
				newAgenticBudgetMiddleware(),
				newStagedEffortMiddleware(),
				newAgenticRecordingMiddleware(),
				newAgenticToolPolicyMiddleware(),
				newAgenticOrgWordingMiddleware(),
				newAgenticStatusMiddleware(),
			},
		}
		return adk.NewTypedChatModelAgent(ctx, agentCfg)
	}

	httpAgent, err := newAgent(SurfaceHTTP)
	if err != nil {
		return nil, fmt.Errorf("ask agents: build HTTP agent: %w", err)
	}
	slackAgent, err := newAgent(SurfaceSlack)
	if err != nil {
		return nil, fmt.Errorf("ask agents: build Slack agent: %w", err)
	}

	newRunner := func(agent *adk.TypedChatModelAgent[*schema.AgenticMessage]) *adk.TypedRunner[*schema.AgenticMessage] {
		return adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{
			Agent:           agent,
			EnableStreaming: false,
			CheckPointStore: nil,
		})
	}

	agents := &AskAgents{
		HTTP:  newRunner(httpAgent),
		Slack: newRunner(slackAgent),
	}
	return agents, nil
}
