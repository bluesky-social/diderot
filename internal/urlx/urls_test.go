package urlx

import (
	"reflect"
	"testing"
)

func TestExtractURLs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "markdown link",
			text: "see [the thread](https://example.slack.com/archives/C0EXAMPLE01/p1700000000123456) for detail",
			want: []string{"https://example.slack.com/archives/C0EXAMPLE01/p1700000000123456"},
		},
		{
			name: "slack mrkdwn link with label",
			text: "per <https://example.com/page|the design doc>, this holds",
			want: []string{"https://example.com/page"},
		},
		{
			name: "slack mrkdwn bare link",
			text: "raw ref <https://example.com/x>",
			want: []string{"https://example.com/x"},
		},
		{
			name: "bare URL with trailing period",
			text: "documented at https://example.com/doc.",
			want: []string{"https://example.com/doc"},
		},
		{
			name: "parenthesized bare URL",
			text: "(see https://example.com/y)",
			want: []string{"https://example.com/y"},
		},
		{
			name: "dedup preserves first-appearance order",
			text: "[a](https://b.example/2) then https://a.example/1 then <https://b.example/2|again>",
			want: []string{"https://b.example/2", "https://a.example/1"},
		},
		{
			name: "session URL with query",
			text: "[session](https://sessions.internal/sessions/abc-123?owner=a%40example.com&client=codex&start=4&end=9)",
			want: []string{"https://sessions.internal/sessions/abc-123?owner=a%40example.com&client=codex&start=4&end=9"},
		},
		{
			name: "no urls",
			text: "nothing to cite here",
			want: nil,
		},
		{
			name: "http scheme",
			text: "legacy http://old.example.com/z link",
			want: []string{"http://old.example.com/z"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractURLs(tc.text)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExtractURLs(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
