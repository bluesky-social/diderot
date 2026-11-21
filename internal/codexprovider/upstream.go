package codexprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/openai/openai-go/v3/packages/ssestream"
)

const (
	codexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	codexModelsURL    = "https://chatgpt.com/backend-api/codex/models?client_version=0.170.0"
	maxStreamBytes    = 32 << 20
)

type codexBackend struct {
	client      *http.Client
	url         string
	modelsURL   string
	credentials *credentialStore
}

type backendError struct {
	status  int
	message string
}

func (e *backendError) Error() string { return e.message }

func (b *codexBackend) respond(ctx context.Context, body request) (json.RawMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := b.do(ctx, http.MethodPost, b.url, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	resp.Body = limitedBody{Reader: io.LimitReader(resp.Body, maxStreamBytes), Closer: resp.Body}
	return readResponse(ssestream.NewDecoder(resp))
}

func (b *codexBackend) do(ctx context.Context, method, url string, payload []byte) (*http.Response, error) {
	credential, err := b.credentials.get(ctx, "")
	if err != nil {
		return nil, err
	}
	resp, err := b.send(ctx, method, url, payload, credential)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		credential, err = b.credentials.get(ctx, credential.access)
		if err != nil {
			return nil, err
		}
		resp, err = b.send(ctx, method, url, payload, credential)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, upstreamStatus(resp.StatusCode)
	}
	return resp, nil
}

func (b *codexBackend) send(ctx context.Context, method, url string, payload []byte, credential accessCredential) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential.access)
	req.Header.Set("ChatGPT-Account-Id", credential.account)
	req.Header.Set("Originator", "diderot")
	if method == http.MethodPost {
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	return b.client.Do(req)
}

func upstreamStatus(status int) *backendError {
	switch status {
	case http.StatusTooManyRequests:
		return &backendError{status, "Codex rate or usage limit reached"}
	case http.StatusBadRequest:
		return &backendError{status, "Codex rejected the model request"}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &backendError{http.StatusBadGateway, "Codex rejected the configured ChatGPT account"}
	default:
		return &backendError{http.StatusBadGateway, fmt.Sprintf("Codex backend returned HTTP %d", status)}
	}
}

func readResponse(stream ssestream.Decoder) (json.RawMessage, error) {
	items := map[int]json.RawMessage{}
	for stream.Next() {
		var event struct {
			Type     string                     `json:"type"`
			Index    int                        `json:"output_index"`
			Item     json.RawMessage            `json:"item"`
			Response map[string]json.RawMessage `json:"response"`
		}
		data := bytes.TrimSpace(stream.Event().Data)
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return nil, &backendError{http.StatusBadGateway, "invalid event from Codex"}
		}
		switch event.Type {
		case "response.output_item.done":
			items[event.Index] = event.Item
		case "response.completed", "response.done", "response.incomplete":
			if event.Response == nil {
				break
			}
			if !present(event.Response["status"]) {
				status := "completed"
				if event.Type == "response.incomplete" {
					status = "incomplete"
				}
				event.Response["status"], _ = json.Marshal(status)
			}
			if !present(event.Response["output"]) || bytes.Equal(bytes.TrimSpace(event.Response["output"]), []byte("[]")) {
				indexes := make([]int, 0, len(items))
				for index := range items {
					indexes = append(indexes, index)
				}
				slices.Sort(indexes)
				output := make([]json.RawMessage, 0, len(indexes))
				for _, index := range indexes {
					output = append(output, items[index])
				}
				event.Response["output"], _ = json.Marshal(output)
			}
			return json.Marshal(event.Response)
		case "response.failed", "error":
			return nil, &backendError{http.StatusBadGateway, "Codex returned a failed response"}
		}
	}
	return nil, &backendError{http.StatusBadGateway, "Codex stream ended without a final response"}
}

type limitedBody struct {
	io.Reader
	io.Closer
}
