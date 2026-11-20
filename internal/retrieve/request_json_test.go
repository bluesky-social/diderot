package retrieve

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// decodeJSONRequest converts a model-produced retrieve call as the Ask tool
// does: the JSON into RequestInput, then RequestFromInput.
func decodeJSONRequest(raw string, allowedScopes []string) (Request, error) {
	var in RequestInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return Request{}, err
	}
	return RequestFromInput(in, allowedScopes)
}

func TestDecodeJSONRequest(t *testing.T) {
	const raw = `{
		"question":"what changed?",
		"sources":["slack","notion"],
		"actors":["alex@example.com"],
		"occurred_after":"2026-07-01T00:00:00Z",
		"occurred_before":"2026-07-20T18:30:00Z",
		"top_k":"17"
	}`
	wantScopes := []string{"slack:C123", "public"}
	req, err := decodeJSONRequest(raw, wantScopes)
	if err != nil {
		t.Fatal(err)
	}
	if req.Question != "what changed?" || req.TopK != 17 {
		t.Fatalf("decoded request = %+v", req)
	}
	if !reflect.DeepEqual(req.Filters.Sources, []string{"slack", "notion"}) ||
		!reflect.DeepEqual(req.Filters.Actors, []string{"alex@example.com"}) ||
		!reflect.DeepEqual(req.Filters.AllowedScopes, wantScopes) {
		t.Fatalf("decoded filters = %+v", req.Filters)
	}
	wantAfter := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	wantBefore := time.Date(2026, 7, 20, 18, 30, 0, 0, time.UTC)
	if !req.Filters.OccurredAfter.Equal(wantAfter) || !req.Filters.OccurredBefore.Equal(wantBefore) {
		t.Fatalf("decoded time bounds = %s .. %s", req.Filters.OccurredAfter, req.Filters.OccurredBefore)
	}
}

func TestRequestFromInputMatchesJSONDecoder(t *testing.T) {
	typed, err := RequestFromInput(RequestInput{
		Question:       "what changed?",
		Sources:        []string{"slack", "notion"},
		Actors:         []string{"alex@example.com"},
		OccurredAfter:  "2026-07-01T00:00:00Z",
		OccurredBefore: "2026-07-20T18:30:00Z",
		TopK:           json.Number("17"),
	}, []string{"slack:C123", "public"})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeJSONRequest(`{
		"question":"what changed?",
		"sources":["slack","notion"],
		"actors":["alex@example.com"],
		"occurred_after":"2026-07-01T00:00:00Z",
		"occurred_before":"2026-07-20T18:30:00Z",
		"top_k":"17"
	}`, []string{"slack:C123", "public"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(typed, decoded) {
		t.Fatalf("typed request = %+v, decoded request = %+v", typed, decoded)
	}
}

func TestDecodeJSONRequestRejectsInvalidFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"top k", `{"question":"q","top_k":"many"}`},
		{"after", `{"question":"q","occurred_after":"last week"}`},
		{"before", `{"question":"q","occurred_before":"tomorrow"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeJSONRequest(tc.raw, nil); err == nil {
				t.Fatal("expected decode error")
			}
		})
	}
}
