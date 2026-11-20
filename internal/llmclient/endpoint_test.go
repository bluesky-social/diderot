package llmclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEndpointHeadersAndNoKey(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	// A client given no key sends no credential.
	client, err := New(Config{BaseURL: server.URL, Headers: http.Header{"X-Team": {"search"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.PostJSON(context.Background(), "/chat", map[string]any{}, 1024); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "" || got.Get("X-Api-Key") != "" || got.Get("X-Team") != "search" {
		t.Fatalf("headers: %v", got)
	}

	// A client library that insists on a key is given a stand-in, which the
	// endpoint's HTTP client removes; its headers are added.
	keyless := Endpoint{BaseURL: server.URL, Headers: http.Header{"X-Route": {"a"}}}
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	req.Header.Set("Authorization", "Bearer "+keyless.LibraryKey())
	req.Header.Set("X-Api-Key", keyless.LibraryKey())
	resp, err := keyless.HTTPClient(nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got.Get("Authorization") != "" || got.Get("X-Api-Key") != "" || got.Get("X-Route") != "a" {
		t.Fatalf("headers: %v", got)
	}
	if req.Header.Get("Authorization") == "" {
		t.Fatal("the caller's request was changed")
	}

	// With a key and no headers there is nothing to add: the caller's
	// client is used as it is.
	base := &http.Client{}
	keyed := Endpoint{BaseURL: server.URL, APIKey: "k"}
	if keyed.HTTPClient(base) != base || keyed.LibraryKey() != "k" {
		t.Fatal("a keyed endpoint without headers changed the client")
	}

	for _, name := range []string{"Authorization", "x-api-key", "Content-Type", "bad name", ""} {
		if CheckHeaderName(name) == nil {
			t.Errorf("accepted the header %q", name)
		}
	}
	if err := CheckHeaderName("X-Team"); err != nil {
		t.Fatal(err)
	}
}
