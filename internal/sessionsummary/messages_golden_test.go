package sessionsummary

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var updateMessagesGolden = flag.Bool("update-messages-golden", false,
	"rewrite the recorded Messages requests; only when a change to them is intended")

// TestMessagesRequestsAreByteIdentical pins the exact requests the map and
// reduce stages send on Messages: method, path, every header, and every byte
// of the body, recorded from the code that spoke Messages alone. A deployment
// that summarizes on Messages reuses its prompt cache only while these stay
// the same, so the recorded files change only together with a deliberate
// change to the request.
func TestMessagesRequestsAreByteIdentical(t *testing.T) {
	bundle := PromptBundle{
		MapSystem:    "Summarize this part of the session.\nSecond line, with \"quotes\" and <angle> & ampersand.",
		ReduceSystem: "Combine the part summaries into one.\nUnicode: naïve — 日本語.",
		LanguageEnum: slices.Clone(languageEnum),
		ActivityEnum: slices.Clone(activityEnum),
	}
	stages := []struct {
		name   string
		answer string
		call   func(*GatewayClient) error
	}{
		{
			name:   "map",
			answer: `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"record_chunk_summary","input":{"evidence":["a","b","c"],"summary":"mapped"}}],"usage":{"input_tokens":1,"output_tokens":1}}`,
			call: func(client *GatewayClient) error {
				_, _, err := MapCall(t.Context(), client, bundle, CallOptions{ModelID: "claude-haiku-4-5", MaxTokens: 1024},
					[]byte("{\"role\":\"user\",\"content\":\"fix <the> bug & \\\"ship\\\" it\"}\n{\"role\":\"assistant\",\"content\":\"done — ✓\"}\n"), 2, 5)
				return err
			},
		},
		{
			name:   "reduce",
			answer: `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"record_session_summary","input":{"evidence":["a","b","c"],"narrative":"n","embedding_text":"e","context_hint":"c","tags":{}}}],"usage":{"input_tokens":1,"output_tokens":1}}`,
			call: func(client *GatewayClient) error {
				_, _, err := ReduceCall(t.Context(), client, bundle, CallOptions{ModelID: "claude-sonnet-5", MaxTokens: 4096},
					[]ChunkSummary{
						{Evidence: StringList{"internal/a.go", "go test ./..."}, Summary: "First part: \"quoted\" <tag> & more."},
						{Evidence: StringList{"error: boom"}, Summary: "Second part — unicode ✓."},
						{Label: "1-2", Evidence: StringList{"earlier"}, Summary: "An earlier summary standing in for parts."},
					})
				return err
			},
		},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			var got bytes.Buffer
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request: %v", err)
				}
				got.Reset()
				fmt.Fprintf(&got, "%s %s\n", r.Method, r.URL.RequestURI())
				for _, name := range slices.Sorted(maps.Keys(r.Header)) {
					fmt.Fprintf(&got, "%s: %s\n", name, strings.Join(r.Header[name], ", "))
				}
				got.WriteString("\n")
				got.Write(body)
				_, _ = w.Write([]byte(stage.answer))
			}))
			defer server.Close()
			client, err := NewGatewayClient(GatewayClientConfig{
				BaseURL: server.URL, APIKey: "golden-key", Headers: http.Header{"X-Provider-Extra": {"one"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			// The request is not one byte string: the Anthropic SDK writes
			// the tool schema's top-level keys in map order, so the stage
			// sends one of a few orderings. Every ordering it can send is
			// recorded, and the set must stay exactly the same.
			seen := map[string]bool{}
			for range 200 {
				if err := stage.call(client); err != nil {
					t.Fatal(err)
				}
				seen[got.String()] = true
			}
			sent := slices.Sorted(maps.Keys(seen))
			pattern := filepath.Join("testdata", "messages_request_"+stage.name+".*.golden")
			if *updateMessagesGolden {
				old, _ := filepath.Glob(pattern)
				for _, path := range old {
					_ = os.Remove(path)
				}
				for index, request := range sent {
					path := strings.Replace(pattern, "*", strconv.Itoa(index+1), 1)
					if err := os.WriteFile(path, []byte(request), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			paths, err := filepath.Glob(pattern)
			if err != nil || len(paths) == 0 {
				t.Fatalf("no recorded requests match %s (%v)", pattern, err)
			}
			var want []string
			for _, path := range paths {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				want = append(want, string(raw))
			}
			slices.Sort(want)
			if !slices.Equal(want, sent) {
				t.Fatalf("the %s stage's Messages requests changed: %d recorded, %d sent.\nrecorded:\n%s\n\nsent:\n%s",
					stage.name, len(want), len(sent), strings.Join(want, "\n---\n"), strings.Join(sent, "\n---\n"))
			}
		})
	}
}
