package ask

import (
	"encoding/json"
	"testing"
)

func TestToolStatusSources(t *testing.T) {
	tests := []struct {
		name   string
		srcs   func(output json.RawMessage) []StatusSource
		output string
		want   []StatusSource
	}{
		{
			name: "retrieve",
			srcs: func(output json.RawMessage) []StatusSource { return retrieveStatusSources(output, testSources) },
			output: `{"hits":[
				{"source":"slack","channel":"infra","actor":{"display":"Priya","canonical":{"namespace":"","id":""},"source_identity":{"namespace":"","id":""},"role":"message_author"},"url":"https://slack.example/1"},
				{"source":"notion","url":"https://notion.example/2"},
				{"source":"linear"}
			]}`,
			want: []StatusSource{
				{URL: "https://slack.example/1", Label: "#infra · Priya"},
				{URL: "https://notion.example/2", Label: "Notion"},
			},
		},
		{
			name: "list",
			srcs: func(output json.RawMessage) []StatusSource { return listStatusSources(output, testSources) },
			output: `{"hits":[
				{"source":"linear","title":"ENG-123","url":"https://linear.example/123"}
			]}`,
			want: []StatusSource{{URL: "https://linear.example/123", Label: "ENG-123"}},
		},
		{
			name: "web search",
			srcs: webSearchStatusSources,
			output: `{"results":[
				{"title":"Slack docs","url":"https://slack.dev/docs"},
				{"title":"missing"}
			]}`,
			want: []StatusSource{{URL: "https://slack.dev/docs", Label: "Slack docs"}},
		},
		{
			name:   "web fetch",
			srcs:   webFetchStatusSources,
			output: `{"url":"https://slack.dev/page","ok":true}`,
			want:   []StatusSource{{URL: "https://slack.dev/page"}},
		},
		{
			name: "code search",
			srcs: codeSearchStatusSources,
			output: `{"files":[
				{"repo":"diderot","path":"main.go","url":"https://github.com/file","chunks":[
					{"start_line":42,"url":"https://github.com/file#L42"}
				]},
				{"repo":"diderot","path":"README.md","url":"https://github.com/readme","chunks":[]}
			]}`,
			want: []StatusSource{
				{URL: "https://github.com/file#L42", Label: "diderot/main.go"},
				{URL: "https://github.com/readme", Label: "diderot/README.md"},
			},
		},
		{
			name:   "code read",
			srcs:   codeReadStatusSources,
			output: `{"repo":"diderot","path":"main.go","ok":true,"url":"https://github.com/main"}`,
			want:   []StatusSource{{URL: "https://github.com/main", Label: "diderot/main.go"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.srcs(json.RawMessage(tt.output))
			if len(got) != len(tt.want) {
				t.Fatalf("sources = %+v, want %+v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("source %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
			if got := tt.srcs(json.RawMessage(`not json`)); got != nil {
				t.Errorf("malformed output sources = %+v, want nil", got)
			}
		})
	}
}

func TestToolStatusOutputs(t *testing.T) {
	tests := []struct {
		name   string
		out    func(output json.RawMessage) string
		output string
		want   string
	}{
		{"retrieve", retrieveStatusOutput, `{"hits":[{},{}]}`, "Found 2 relevant results"},
		{"list", listStatusOutput, `{"count":1,"next_cursor":"next"}`, "Found 1 matching event; more available"},
		{"expand", func(output json.RawMessage) string { return expandStatusOutput(output, testSources) },
			`{"source":"slack","supported":true}`, "Loaded source context from Slack"},
		{"web search", webSearchStatusOutput, `{"results":[{},{}]}`, "Found 2 web results"},
		{"web fetch", webFetchStatusOutput, `{"ok":true,"truncated":true}`, "Loaded page content (truncated)"},
		{"code search", codeSearchStatusOutput, `{"total_matches":7,"files":[{},{}]}`, "Found 7 matches across 2 files"},
		{"code read", codeReadStatusOutput, `{"repo":"diderot","path":"main.go","ok":true}`, "Loaded diderot/main.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.out(json.RawMessage(tt.output)); got != tt.want {
				t.Errorf("StatusOutput = %q, want %q", got, tt.want)
			}
			if got := tt.out(json.RawMessage(`not json`)); got != "" {
				t.Errorf("malformed StatusOutput = %q, want empty", got)
			}
		})
	}
}

func TestFailedFetchAndReadHaveNoStatusSources(t *testing.T) {
	if got := webFetchStatusSources(json.RawMessage(`{"url":"https://example.com","ok":false}`)); got != nil {
		t.Errorf("failed web fetch sources = %+v", got)
	}
	if got := codeReadStatusSources(json.RawMessage(`{"url":"https://example.com","ok":false}`)); got != nil {
		t.Errorf("failed code read sources = %+v", got)
	}
}
