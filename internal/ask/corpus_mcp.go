package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

const tasksExtensionID = "io.modelcontextprotocol/tasks"

type mcpCallerIdentityKey struct{}

// WithMCPCaller binds the verified end-user identity and visibility scopes to
// an MCP request. Discovery may proceed without an identity, but every tool
// execution and task operation rejects an identity-less request.
func WithMCPCaller(ctx context.Context, email string, scopes []string) context.Context {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		scopes = []string{"mcp:no-verified-identity"}
	}
	ctx = context.WithValue(ctx, mcpCallerIdentityKey{}, email)
	return WithAllowedScopes(ctx, scopes)
}

type mcpAskBlockedKey struct{}

// WithMCPAskBlocked refuses the ask tool for this request with notice, which
// the caller sees as the tool's error. Retrieve, list, and expand are
// unaffected.
func WithMCPAskBlocked(ctx context.Context, notice string) context.Context {
	return context.WithValue(ctx, mcpAskBlockedKey{}, notice)
}

// MCPAskBlocked returns the notice WithMCPAskBlocked set, or "".
func MCPAskBlocked(ctx context.Context) string {
	notice, _ := ctx.Value(mcpAskBlockedKey{}).(string)
	return notice
}

func mcpCaller(ctx context.Context) (string, error) {
	email, _ := ctx.Value(mcpCallerIdentityKey{}).(string)
	if email == "" {
		return "", errors.New("verified user identity is required")
	}
	return email, nil
}

// CorpusAskRunner is the Ask surface required by MCP. The provider passed to
// NewCorpusMCPServer is evaluated for each call because the replica's Ask is
// attached after the HTTP handler is mounted.
type CorpusAskRunner interface {
	CreateOwnedSession(ctx context.Context, ownerEmail string) (uuid.UUID, error)
	// OwnsSession reports whether ownerEmail may continue sessionID and read
	// its artifacts. A session that does not exist answers false like one
	// that belongs to someone else.
	OwnsSession(ctx context.Context, sessionID uuid.UUID, ownerEmail string) (bool, error)
	ExecuteTurn(ctx context.Context, req TurnRequest) (TurnResult, error)
	ReadArtifact(ctx context.Context, sessionID, artifactID uuid.UUID) (ArtifactRecord, []byte, error)
	// Hold admits an ask task. ok is false while the runner takes no new
	// turns, as when its replica is shutting down; otherwise the runner,
	// before it stops, waits for the task to call release once its result
	// is recorded.
	Hold() (release func(), ok bool)
}

// AskInvocationNotifier is the best-effort side channel used to report
// accepted Ask and MCP corpus invocations to operator watchers. Implementations
// must not expose results; the input and optional session identity are enough
// for the usage signal.
type AskInvocationNotifier interface {
	NotifyAskInvocation(ctx context.Context, ownerEmail, sessionID, question string) error
	NotifyMCPInvocation(ctx context.Context, ownerEmail, toolName, sessionID, input string) error
}

// CorpusAskLease is the Ask an MCP ask task runs on. Its tasks run under
// Context and are interrupted when it ends; none starts after, nor while
// Runner holds no more tasks.
type CorpusAskLease struct {
	Runner   CorpusAskRunner
	Context  context.Context
	Notifier AskInvocationNotifier
}

type CorpusAskRunnerProvider func() CorpusAskLease

// mcpRetrieveToolDescription extends the shared retrieve description with the
// MCP-only exact-reference contract. Ask resolves handles from the user's turn
// before generation instead, so this sentence must not reach Ask's prompt.
const mcpRetrieveToolDescription = retrieveToolDescription + `

An exact handle in the question is read directly, as well as searched for: a Slack message link, a Linear issue URL, an
issue key such as ENG-123 for a Linear team the corpus indexes, a session UUID, 'event_id: N', 'chunk_id: N', or a
Zendesk ticket or article URL. Each comes back under 'references', ahead of the hits, with the handle as written, its
kind, event_id, source, url when it has one, content, and truncated when the content was cut to fit. If any handle
cannot be read for this caller, the whole call fails with "` + ReferenceUnavailableText + `" and returns no hits; ask
again without that handle to search by topic.`

// mcpExpandToolDescription replaces Ask's sentence about handles resolved
// before generation: over MCP they are resolved by retrieve.
const mcpExpandToolDescription = expandToolDescriptionHead + " Pass an attachment's `id` to `attachment` to get the image or the document's text. An item named exactly in a `retrieve` question has already been read through this same path and comes back under its `references`, marked `truncated` when it was cut to fit, so do not expand to find it. " + expandToolDescriptionTail

const mcpAskToolDescription = "Ask Diderot a question and get back a written answer with citations. Diderot does its own research across the corpus and whatever else the deployment gives it (code, the web, live telemetry), so use this for a question that needs several sources weighed together or an investigation, and use `retrieve` and `list` when you want the raw material yourself. Write the question so it stands alone: Diderot sees nothing of your conversation. Say whose work or which system you mean, the period, and the form of answer you need. The call starts an asynchronous task; poll it with tasks/get until it completes, which can take a few minutes. The result carries `session_id`: pass it back as `session_id` to ask a follow-up in the same conversation, where Diderot still has the earlier question, its research and its answer. Images the answer produced are listed under `artifacts` and returned as resource links; fetch one with resources/read on its `uri`."

var mcpAskSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {
      "type": "string",
      "description": "The question, written to stand alone: name the people, systems and period you mean, and the form of answer you need."
    },
    "session_id": {
      "type": "string",
      "format": "uuid",
      "description": "The 'session_id' of an earlier ask result of yours, to continue that conversation. Omit to start a new one."
    }
  },
  "required": ["question"],
  "additionalProperties": false
}`)

const mcpServerInstructions = "Diderot searches an organization's written record: Slack, coding-agent sessions, documents, issues and support tickets. Use `retrieve` to search by meaning, `list` to enumerate everything matching a filter or to read a Slack channel over a period, `expand` to read the full source behind a result, and `attachment` to fetch an image or document that an expand result lists. Use `ask` to have Diderot research a question and write a cited answer; it is asynchronous, so poll the returned task with tasks/get, and pass the result's session_id back to continue the conversation."

// mcpRetrieveOutput is MCP retrieve's result. References is omitted when the
// question names no explicit handle, leaving the shape identical to Ask's
// retrieveOutput.
type mcpRetrieveOutput struct {
	References []mcpResolvedReference `json:"references,omitempty"`
	Hits       []retrieveOutputHit    `json:"hits"`
}

type mcpResolvedReference struct {
	Handle    string        `json:"handle"`
	Kind      ReferenceKind `json:"kind"`
	EventID   int64         `json:"event_id"`
	Source    string        `json:"source"`
	URL       string        `json:"url,omitempty"`
	Content   string        `json:"content"`
	Truncated bool          `json:"truncated,omitempty"`
}

// NewCorpusMCPServer exposes Diderot's corpus tools and task-backed Ask over
// MCP. The official SDK supplies modern server/discover and stateless
// Streamable HTTP; corpusTaskStore implements the SEP-2663 Tasks extension.
// sources are the registered sources the corpus tools can filter by.
func NewCorpusMCPServer(
	retriever CorpusRetriever,
	sources source.Infos,
	expander SourceExpander,
	scopeLookup ScopeLookup,
	attachments AttachmentReader,
	references ReferencePrerequisiteResolver,
	linearTeams LinearTeamProvider,
	askRunner CorpusAskRunnerProvider,
	pool *pgxpool.Pool,
	lifecycle context.Context,
	version string,
) (*mcp.Server, error) {
	if pool == nil {
		return nil, errors.New("corpus MCP: task database is required")
	}
	return newCorpusMCPServer(retriever, sources, expander, scopeLookup, attachments, references, linearTeams, askRunner, newPostgresCorpusTaskBackend(pool), lifecycle, version)
}

func newCorpusMCPServer(
	retriever CorpusRetriever,
	sources source.Infos,
	expander SourceExpander,
	scopeLookup ScopeLookup,
	attachments AttachmentReader,
	references ReferencePrerequisiteResolver,
	linearTeams LinearTeamProvider,
	askRunner CorpusAskRunnerProvider,
	taskBackend corpusTaskBackend,
	lifecycle context.Context,
	version string,
) (*mcp.Server, error) {
	if lifecycle == nil {
		return nil, errors.New("corpus MCP: lifecycle context is required")
	}
	// expand gates every event on its scope before fetching content, so it
	// needs both halves; without them it would fail on every call.
	if expander == nil || scopeLookup == nil {
		return nil, errors.New("corpus MCP: expander and scope lookup are required")
	}
	if attachments == nil {
		return nil, errors.New("corpus MCP: attachment reader is required")
	}
	// retrieve reads explicit handles in the question exactly; without the
	// resolver every such question would have to fail closed.
	if references == nil {
		return nil, errors.New("corpus MCP: exact reference resolver is required")
	}
	if linearTeams == nil {
		return nil, errors.New("corpus MCP: linear team provider is required")
	}
	caps := &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}
	caps.AddExtension(tasksExtensionID, nil)
	srv := mcp.NewServer(&mcp.Implementation{Name: "diderot", Version: version}, &mcp.ServerOptions{
		Capabilities: caps,
		Instructions: mcpServerInstructions,
	})

	srv.AddTool(readOnlyTool("retrieve", mcpRetrieveToolDescription, retrieveSchema(sources)),
		func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			owner, err := mcpCaller(ctx)
			if err != nil {
				return toolError(err), nil
			}
			var input retrieve.RequestInput
			if err := decodeMCPArguments(request, &input); err != nil {
				return toolError(fmt.Errorf("invalid retrieve arguments: %w", err)), nil
			}
			req, err := retrieve.RequestFromInput(input, scopesFromContext(ctx))
			if err != nil {
				return toolError(err), nil
			}
			notifyMCPInvocation(askRunner, owner, "retrieve", "", input.Question)
			resolved, ok := prepareMCPReferences(ctx, references, linearTeams, input.Question)
			if !ok {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: ReferenceUnavailableText}},
				}, nil
			}
			out, err := runRetrieve(ctx, retriever, sources, req)
			return corpusMCPResult("retrieve", mcpRetrieveOutput{References: resolved, Hits: out.Hits}, err)
		})

	srv.AddTool(readOnlyTool("list", listToolDescription, listSchema(sources)),
		func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			owner, err := mcpCaller(ctx)
			if err != nil {
				return toolError(err), nil
			}
			var input listInput
			if err := decodeMCPArguments(request, &input); err != nil {
				return toolError(fmt.Errorf("invalid list arguments: %w", err)), nil
			}
			notifyMCPInvocation(askRunner, owner, "list", "", describeMCPListInput(input))
			out, err := runList(ctx, retriever, sources, input)
			return corpusMCPResult("list", out, err)
		})

	srv.AddTool(readOnlyTool("expand", mcpExpandToolDescription, expandSchema),
		func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			owner, err := mcpCaller(ctx)
			if err != nil {
				return toolError(err), nil
			}
			var input expandInput
			if err := decodeMCPArguments(request, &input); err != nil {
				return toolError(fmt.Errorf("invalid expand arguments: %w", err)), nil
			}
			notifyMCPInvocation(askRunner, owner, "expand", "", describeMCPExpandInput(input))
			out, err := expandFromInput(ctx, expander, scopeLookup, input)
			return corpusMCPResult("expand", out, err)
		})

	srv.AddTool(readOnlyTool("attachment", attachmentToolDescription, attachmentSchema),
		func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			owner, err := mcpCaller(ctx)
			if err != nil {
				return toolError(err), nil
			}
			var input attachmentInput
			if err := decodeMCPArguments(request, &input); err != nil {
				return toolError(fmt.Errorf("invalid attachment arguments: %w", err)), nil
			}
			notifyMCPInvocation(askRunner, owner, "attachment", "", "id "+input.ID)
			out, image, err := readAttachment(ctx, attachments, scopeLookup, input)
			if err != nil {
				return toolError(fmt.Errorf("attachment: %w", err)), nil
			}
			return attachmentMCPResult(out, image)
		})

	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "ask-artifact",
		Title:       "Ask artifact",
		Description: "An image an ask answer produced. Readable only by the caller who owns the ask session.",
		URITemplate: askArtifactURITemplate,
	}, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return readAskArtifactResource(ctx, askRunner, request.Params.URI)
	})

	srv.AddTool(&mcp.Tool{
		Name:        "ask",
		Description: mcpAskToolDescription,
		InputSchema: mcpAskSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: false, DestructiveHint: boolPtr(false),
			IdempotentHint: false, OpenWorldHint: boolPtr(false),
		},
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// corpusTaskStore intercepts valid Ask calls before this handler.
		return toolError(errors.New("ask task middleware unavailable")), nil
	})

	tasks := newCorpusTaskStore(taskBackend, askRunner, lifecycle)
	if err := tasks.attach(srv); err != nil {
		return nil, err
	}
	// Outermost: the go-sdk dispatches every method, including every tool
	// call, on its own goroutine with no recover of its own (jsonrpc2's
	// handleAsync), unlike net/http's per-request recovery for /v1/ask. A
	// panic in a synchronous handler here -- retrieve, list, expand,
	// attachment, tasks/*, or the ask task middleware's own pre-work --
	// would otherwise crash the whole replica. corpusTaskStore.middleware
	// still runs inside this (registered first, so it is wrapped), so a
	// panic launching the task is caught here too, in addition to the ask
	// task's own goroutine-level recovery in create.
	srv.AddReceivingMiddleware(recoverMCPMiddleware)
	return srv, nil
}

// recoverMCPMiddleware is the shared, outermost receiving middleware: see
// its registration above for why every method needs it.
func recoverMCPMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (result mcp.Result, err error) {
		defer worker.Recover(slog.Default(), "mcp-method", func() {
			result, err = nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error"}
		})
		return next(ctx, method, request)
	}
}

// prepareMCPReferences reads every explicit corpus handle in an MCP retrieve
// question through the same exact resolver Ask uses, scoped to the caller.
// ok=false means at least one handle is unavailable and the call must fail
// closed without semantic retrieval: an explicit handle must never silently
// become a semantic question. Missing, denied, deleted, and backend failures
// are indistinguishable to the caller; backend detail goes only to the log.
func prepareMCPReferences(ctx context.Context, resolver ReferencePrerequisiteResolver, linearTeams LinearTeamProvider, question string) ([]mcpResolvedReference, bool) {
	refs := parseExplicitReferences(ctx, linearTeams, question)
	if len(refs) == 0 {
		return nil, true
	}
	state, err := resolver.PrepareReferences(ctx, refs, scopesFromContext(ctx))
	if err != nil {
		slog.Default().Warn("corpus MCP: exact reference preparation failed; blocking retrieve", "err", err)
		return nil, false
	}
	if state.HasUnavailablePrimary() {
		return nil, false
	}
	out := make([]mcpResolvedReference, 0, len(state.Resolved))
	for _, r := range state.Resolved {
		out = append(out, mcpResolvedReference{
			Handle: r.Reference.Raw, Kind: r.Reference.Kind, EventID: r.EventID,
			Source: r.Source, URL: r.URL, Content: r.Content, Truncated: r.Truncated,
		})
	}
	return out, true
}

func readOnlyTool(name, description string, schema json.RawMessage) *mcp.Tool {
	return &mcp.Tool{
		Name: name, Description: description, InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true, DestructiveHint: boolPtr(false),
			IdempotentHint: true, OpenWorldHint: boolPtr(false),
		},
	}
}

func boolPtr(v bool) *bool { return &v }

func decodeMCPArguments(req *mcp.CallToolRequest, dst any) error {
	if len(req.Params.Arguments) == 0 {
		return nil
	}
	return json.Unmarshal(req.Params.Arguments, dst)
}

func toolError(err error) *mcp.CallToolResult {
	result := &mcp.CallToolResult{}
	result.SetError(err)
	return result
}

func corpusMCPResult(name string, output any, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return toolError(fmt.Errorf("%s: %w", name, err)), nil
	}
	b, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("encode %s result: %w", name, err)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: output,
	}, nil
}

type askMCPInput struct {
	Question  string `json:"question"`
	SessionID string `json:"session_id,omitempty"`
}

type askMCPOutput struct {
	SessionID        string           `json:"session_id"`
	Reply            string           `json:"reply"`
	IncompleteReason IncompleteReason `json:"incomplete_reason,omitempty"`
	Artifacts        []askMCPArtifact `json:"artifacts,omitempty"`
}

// askMCPArtifact describes one image of an ask answer. URI is an MCP
// resource the session's owner reads with resources/read.
type askMCPArtifact struct {
	URI       string `json:"uri"`
	MediaType string `json:"media_type"`
	ByteSize  int64  `json:"byte_size"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Title     string `json:"title"`
	AltText   string `json:"alt_text"`
}

// askArtifactURITemplate is the resource every ask artifact is read
// through; askArtifactURI and parseAskArtifactURI are its two directions.
const askArtifactURITemplate = "diderot://ask/{session_id}/artifacts/{artifact_id}"

func askArtifactURI(sessionID, artifactID uuid.UUID) string {
	return "diderot://ask/" + sessionID.String() + "/artifacts/" + artifactID.String()
}

func parseAskArtifactURI(uri string) (sessionID, artifactID uuid.UUID, ok bool) {
	rest, found := strings.CutPrefix(uri, "diderot://ask/")
	session, artifact, cut := strings.Cut(rest, "/artifacts/")
	if !found || !cut {
		return uuid.Nil, uuid.Nil, false
	}
	sessionID, sessionErr := uuid.Parse(session)
	artifactID, artifactErr := uuid.Parse(artifact)
	return sessionID, artifactID, sessionErr == nil && artifactErr == nil
}

// readAskArtifactResource serves resources/read for an ask artifact to the
// owner of its session. Every refusal is the same not-found: no identity, a
// malformed URI, another caller's session, an unknown artifact.
func readAskArtifactResource(ctx context.Context, provider CorpusAskRunnerProvider, uri string) (*mcp.ReadResourceResult, error) {
	owner, err := mcpCaller(ctx)
	sessionID, artifactID, ok := parseAskArtifactURI(uri)
	if err != nil || !ok || provider == nil {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	runner := provider().Runner
	if runner == nil {
		return nil, ErrUnavailable
	}
	owned, err := runner.OwnsSession(ctx, sessionID, owner)
	if err != nil {
		return nil, fmt.Errorf("read ask artifact: %w", err)
	}
	if !owned {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	record, data, err := runner.ReadArtifact(ctx, sessionID, artifactID)
	if IsArtifactNotFound(err) {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	if err != nil {
		return nil, fmt.Errorf("read ask artifact: %w", err)
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
		{URI: uri, MIMEType: record.MediaType, Blob: data},
	}}, nil
}

// askSessionForMCP resolves the session an ask call runs in before its task
// starts: uuid.Nil for a new conversation, or the caller's own session to
// continue. A session_id that is malformed, unknown, or someone else's is
// refused with one message.
func askSessionForMCP(ctx context.Context, runner CorpusAskRunner, owner, sessionID string) (uuid.UUID, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return uuid.Nil, nil
	}
	notFound := fmt.Errorf("session_id %q is not an ask session of yours; omit it to start a new conversation", sessionID)
	id, err := uuid.Parse(sessionID)
	if err != nil {
		return uuid.Nil, notFound
	}
	owned, err := runner.OwnsSession(ctx, id, owner)
	if err != nil {
		return uuid.Nil, fmt.Errorf("ask: check session: %w", err)
	}
	if !owned {
		return uuid.Nil, notFound
	}
	return id, nil
}

// runMCPAsk runs one ask turn in sessionID, which the caller owns, or in a
// new session when it is uuid.Nil.
func runMCPAsk(ctx context.Context, runner CorpusAskRunner, notifier AskInvocationNotifier, owner string, scopes []string, sessionID uuid.UUID, input askMCPInput) (*mcp.CallToolResult, error) {
	input.Question = strings.TrimSpace(input.Question)
	if input.Question == "" {
		return toolError(errors.New("question is required")), nil
	}
	if sessionID == uuid.Nil {
		created, err := runner.CreateOwnedSession(ctx, owner)
		if err != nil {
			return nil, fmt.Errorf("ask: create session: %w", err)
		}
		sessionID = created
	}
	notifyMCPInvocationWithNotifier(notifier, owner, "ask", sessionID.String(), input.Question)
	turn, err := runner.ExecuteTurn(ctx, TurnRequest{
		SessionID: sessionID, UserInput: input.Question, Surface: AskRunSurfaceHTTP,
		Asker: &AskerInfo{DisplayName: owner, Email: owner}, AllowedScopes: scopes,
	})
	if err != nil {
		return nil, fmt.Errorf("ask: %w", err)
	}
	out := askMCPOutput{SessionID: sessionID.String(), Reply: turn.Reply, IncompleteReason: turn.IncompleteReason}
	links := make([]mcp.Content, 0, len(turn.Artifacts))
	for _, artifact := range turn.Artifacts {
		described := askMCPArtifact{
			URI:       askArtifactURI(sessionID, artifact.ID),
			MediaType: artifact.MediaType, ByteSize: artifact.ByteSize,
			Width: artifact.Width, Height: artifact.Height,
			Title: artifact.Title, AltText: artifact.AltText,
		}
		out.Artifacts = append(out.Artifacts, described)
		links = append(links, &mcp.ResourceLink{
			URI: described.URI, Name: artifact.ID.String(), Title: artifact.Title,
			Description: artifact.AltText, MIMEType: artifact.MediaType, Size: &described.ByteSize,
		})
	}
	result, err := corpusMCPResult("ask", out, nil)
	if err != nil {
		return nil, err
	}
	result.Content = append(result.Content, links...)
	return result, nil
}

func notifyMCPInvocation(provider CorpusAskRunnerProvider, owner, toolName, sessionID, input string) {
	if provider == nil {
		return
	}
	notifyMCPInvocationWithNotifier(provider().Notifier, owner, toolName, sessionID, input)
}

func notifyMCPInvocationWithNotifier(notifier AskInvocationNotifier, owner, toolName, sessionID, input string) {
	if notifier == nil {
		return
	}
	go func() {
		// Reached from every MCP tool call (retrieve, list, expand,
		// attachment, ask), on its own goroutine outside both
		// recoverMCPMiddleware (that defer runs on the method-handler
		// goroutine, not this one) and the ask task's own recovery.
		defer worker.Recover(slog.Default(), "mcp-notify-invocation")
		notifyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = notifier.NotifyMCPInvocation(notifyCtx, owner, toolName, sessionID, input)
	}()
}

func describeMCPExpandInput(input expandInput) string {
	return "event_id " + input.EventID.String()
}

func describeMCPListInput(input listInput) string {
	// Cursor values are opaque pagination state, not useful operator context.
	input.Cursor = ""
	b, err := json.Marshal(input)
	if err != nil {
		return "(filters unavailable)"
	}
	return string(b)
}

// Ensure the detached task context does not inherit request cancellation while
// retaining only the authenticated identity boundary needed by Ask.
func detachedMCPContext(lifecycle context.Context, owner string, scopes []string) context.Context {
	return WithMCPCaller(lifecycle, owner, scopes)
}
