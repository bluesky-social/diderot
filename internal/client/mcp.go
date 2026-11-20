package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ProtocolVersion is the MCP revision the client speaks: the stateless one
// whose Tasks extension the server's ask needs.
const ProtocolVersion = "2026-07-28"

const tasksExtension = "io.modelcontextprotocol/tasks"

// maxResponse bounds one response body; an ask artifact is the largest.
const maxResponse = 64 << 20

// MCP is a client for one MCP endpoint over the stateless protocol: every
// JSON-RPC request is one POST carrying its own _meta. It is written here
// rather than taken from the Go SDK because the SDK decodes tools/call into a
// CallToolResult and drops the task the server's ask returns.
type MCP struct {
	URL     string
	HTTP    *http.Client
	Version string
	// Authorization returns the Authorization header value, or "".
	Authorization func(ctx context.Context) (string, error)

	nextID atomic.Int64
}

// RPCError is a JSON-RPC error the server answered with.
type RPCError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

// HTTPError is a response that was neither a JSON-RPC answer nor a sign-in
// refusal.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// Call sends method with params and decodes the result into out. name is the
// request's Mcp-Name header: the tool for tools/call, the URI for
// resources/read, the task for tasks/*.
func (c *MCP) Call(ctx context.Context, method, name string, params map[string]any, out any) error {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		mcp.MetaKeyProtocolVersion: ProtocolVersion,
		mcp.MetaKeyClientInfo:      map[string]any{"name": "diderot", "version": c.Version},
		mcp.MetaKeyClientCapabilities: map[string]any{
			"extensions": map[string]any{tasksExtension: map[string]any{}},
		},
	}
	id := c.nextID.Add(1)
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", ProtocolVersion)
	req.Header.Set("Mcp-Method", method)
	if name != "" {
		req.Header.Set("Mcp-Name", name)
	}
	if c.Authorization != nil {
		value, err := c.Authorization(ctx)
		if err != nil {
			return err
		}
		if value != "" {
			req.Header.Set("Authorization", value)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return &SignInError{Reason: "the server refused the sign-in (HTTP 401): " + firstLine(raw)}
		case http.StatusForbidden:
			return &SignInError{Reason: "the server refused this account (HTTP 403): " + firstLine(raw)}
		default:
			return &HTTPError{Status: resp.StatusCode, Body: firstLine(raw)}
		}
	}
	result, err := readResponse(resp.Header.Get("Content-Type"), io.LimitReader(resp.Body, maxResponse), id)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(result, out); err != nil {
		return fmt.Errorf("decode the %s result: %w", method, err)
	}
	return nil
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// readResponse finds the JSON-RPC response with id in a JSON body or an SSE
// stream, and returns its result or its error. It stops reading a stream at
// that response: a server may keep the stream open after it.
func readResponse(contentType string, body io.Reader, id int64) (json.RawMessage, error) {
	want := []byte(fmt.Sprint(id))
	match := func(data []byte) (json.RawMessage, bool, error) {
		var msg rpcMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return nil, false, fmt.Errorf("not a JSON-RPC message: %w", err)
		}
		if !bytes.Equal(bytes.TrimSpace(msg.ID), want) {
			return nil, false, nil
		}
		if msg.Error != nil {
			return nil, true, msg.Error
		}
		return msg.Result, true, nil
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType != "text/event-stream" {
		raw, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("read the response: %w", err)
		}
		result, ok, err := match(raw)
		if err == nil && !ok {
			err = errors.New("the response answers another request")
		}
		return result, err
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxResponse)
	var data []byte
	// event ends one event: the response, or something else to skip.
	event := func() (json.RawMessage, bool, error) {
		if len(data) == 0 {
			return nil, false, nil
		}
		result, ok, err := match(data)
		data = nil
		return result, ok, err
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if result, ok, err := event(); ok || err != nil {
				return result, err
			}
			continue
		}
		if rest, found := bytes.CutPrefix(line, []byte("data:")); found {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(rest, []byte(" "))...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the event stream: %w", err)
	}
	if result, ok, err := event(); ok || err != nil {
		return result, err
	}
	return nil, errors.New("the event stream ended without a response")
}

func firstLine(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// Discover asks the server what it is and checks that it speaks
// ProtocolVersion.
func (c *MCP) Discover(ctx context.Context) (*mcp.DiscoverResult, error) {
	var result mcp.DiscoverResult
	if err := c.Call(ctx, "server/discover", "", nil, &result); err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == mcp.CodeUnsupportedProtocolVersion {
			return nil, fmt.Errorf("the server does not speak MCP %s, which ask needs: %w", ProtocolVersion, err)
		}
		return nil, err
	}
	if !slices.Contains(result.SupportedVersions, ProtocolVersion) {
		return nil, fmt.Errorf("the server speaks MCP %s, not %s, which ask needs",
			strings.Join(result.SupportedVersions, ", "), ProtocolVersion)
	}
	return &result, nil
}

// Tools lists the server's tool names.
func (c *MCP) Tools(ctx context.Context) ([]string, error) {
	var names []string
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.Call(ctx, "tools/list", "", params, &page); err != nil {
			return nil, err
		}
		for _, tool := range page.Tools {
			names = append(names, tool.Name)
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			return names, nil
		}
		cursor = page.NextCursor
	}
}
