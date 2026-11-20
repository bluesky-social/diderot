package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Answer is a completed ask: the reply, which carries its citations, the
// session to continue, and the images it produced.
type Answer struct {
	SessionID        string     `json:"session_id"`
	Reply            string     `json:"reply"`
	IncompleteReason string     `json:"incomplete_reason,omitempty"`
	Artifacts        []Artifact `json:"artifacts,omitempty"`
}

// Artifact is one image of an answer, readable as an MCP resource. Path is
// set once it is saved.
type Artifact struct {
	URI       string `json:"uri"`
	MediaType string `json:"media_type"`
	ByteSize  int64  `json:"byte_size"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Title     string `json:"title,omitempty"`
	AltText   string `json:"alt_text,omitempty"`
	Path      string `json:"path,omitempty"`
}

// ToolError is a tool result the server marked as an error, such as a
// session that is not the caller's, or Ask still closed during setup.
type ToolError struct{ Message string }

func (e *ToolError) Error() string { return e.Message }

// taskPayload covers both a tools/call that started a task and a tasks/get.
type taskPayload struct {
	TaskID         string              `json:"taskId"`
	Status         string              `json:"status"`
	StatusMessage  string              `json:"statusMessage"`
	PollIntervalMS *int64              `json:"pollIntervalMs"`
	Result         *mcp.CallToolResult `json:"result"`
	Error          *RPCError           `json:"error"`
}

// Ask asks question, continuing sessionID when it is set, and waits for the
// answer. When ctx ends first, the task is cancelled on the server.
func (c *MCP) Ask(ctx context.Context, question, sessionID string) (*Answer, error) {
	args := map[string]any{"question": question}
	if sessionID != "" {
		args["session_id"] = sessionID
	}
	var started json.RawMessage
	if err := c.Call(ctx, "tools/call", "ask", map[string]any{"name": "ask", "arguments": args}, &started); err != nil {
		return nil, err
	}
	var task taskPayload
	if err := json.Unmarshal(started, &task); err != nil {
		return nil, fmt.Errorf("decode the ask result: %w", err)
	}
	if task.TaskID == "" {
		// Refused before a task existed: a tool error result.
		var result mcp.CallToolResult
		if err := json.Unmarshal(started, &result); err != nil {
			return nil, fmt.Errorf("decode the ask result: %w", err)
		}
		return answerFrom(&result)
	}
	for task.Status == "working" {
		wait := time.Second
		if task.PollIntervalMS != nil && *task.PollIntervalMS > 0 {
			wait = time.Duration(*task.PollIntervalMS) * time.Millisecond
		}
		select {
		case <-ctx.Done():
			c.cancelTask(task.TaskID)
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		id := task.TaskID
		task = taskPayload{}
		if err := c.Call(ctx, "tasks/get", id, map[string]any{"taskId": id}, &task); err != nil {
			if ctx.Err() != nil {
				c.cancelTask(id)
				return nil, ctx.Err()
			}
			return nil, err
		}
		task.TaskID = id
	}
	switch task.Status {
	case "completed":
		if task.Result == nil {
			return nil, errors.New("the ask task completed without a result")
		}
		return answerFrom(task.Result)
	case "failed":
		if task.Error != nil {
			return nil, &ToolError{Message: task.Error.Message}
		}
		return nil, &ToolError{Message: "the ask task failed: " + task.StatusMessage}
	case "cancelled":
		return nil, &ToolError{Message: "the ask task was cancelled: " + task.StatusMessage}
	default:
		return nil, fmt.Errorf("the ask task is %q, which this client does not handle", task.Status)
	}
}

// cancelTask asks the server to stop a task, on a context of its own: the
// caller's has already ended.
func (c *MCP) cancelTask(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Call(ctx, "tasks/cancel", id, map[string]any{"taskId": id}, nil)
}

func answerFrom(result *mcp.CallToolResult) (*Answer, error) {
	if result.IsError {
		var texts []string
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				texts = append(texts, text.Text)
			}
		}
		return nil, &ToolError{Message: strings.Join(texts, "\n")}
	}
	b, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return nil, err
	}
	var answer Answer
	if err := json.Unmarshal(b, &answer); err != nil || answer.SessionID == "" {
		return nil, fmt.Errorf("the ask result has no answer: %s", firstLine(b))
	}
	return &answer, nil
}

var artifactURI = regexp.MustCompile(`^diderot://ask/[0-9a-f-]{36}/artifacts/([0-9a-f-]{36})$`)

var artifactExtensions = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp",
}

// SaveArtifact reads the artifact and writes it into dir as
// <artifact id><extension>. The name comes from the URI's own UUID, never
// from anything else the server sends, and an existing file is never
// replaced.
func (c *MCP) SaveArtifact(ctx context.Context, artifact Artifact, dir string) (string, error) {
	match := artifactURI.FindStringSubmatch(artifact.URI)
	if match == nil {
		return "", fmt.Errorf("not an ask artifact: %q", artifact.URI)
	}
	var read mcp.ReadResourceResult
	if err := c.Call(ctx, "resources/read", artifact.URI, map[string]any{"uri": artifact.URI}, &read); err != nil {
		return "", fmt.Errorf("read %s: %w", artifact.URI, err)
	}
	if len(read.Contents) == 0 || read.Contents[0].Blob == nil {
		return "", fmt.Errorf("read %s: no image came back", artifact.URI)
	}
	content := read.Contents[0]
	ext, ok := artifactExtensions[content.MIMEType]
	if !ok {
		ext = ".bin"
	}
	path := filepath.Join(dir, match[1]+ext)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // the name is a UUID the URI pattern checked
	if err != nil {
		return "", err
	}
	if _, err := f.Write(content.Blob); err != nil {
		_ = f.Close()
		return "", err
	}
	return path, f.Close()
}
