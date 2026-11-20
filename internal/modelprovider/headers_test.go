package modelprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

func TestParseHeaders(t *testing.T) {
	headers, err := ParseHeaders([]string{"X-Team: search", "", " x-route :  a:b "})
	require.NoError(t, err)
	assert.Equal(t, "search", headers.Get("X-Team"))
	assert.Equal(t, "a:b", headers.Get("X-Route"), "a value may hold a colon")
	for _, bad := range []string{"no colon", "X-Empty:", "Authorization: Bearer x", "bad name: v"} {
		_, err := ParseHeaders([]string{"X-Ok: 1", bad})
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "line 2")
		assert.NotContains(t, err.Error(), "Bearer x", "a refused line is never echoed: it may be a credential")
	}
}

// TestProviderHeadersAndNoKey: a provider's extra headers, plain and secret,
// ride its requests; a server marked as taking no key is listed without one.
func TestProviderHeadersAndNoKey(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "local"}}})
	}))
	defer server.Close()
	def := Definitions{Standalone: true}.Define(Row("p"))
	values := func(extra settings.Values) settings.Values {
		out := settings.Values{FieldKind: "openai", FieldBaseURL: server.URL + "/v1"}
		for key, value := range extra {
			out[key] = value
		}
		return out
	}

	// No key, and not marked as taking none: the test says so, and offers
	// the mark.
	report := def.Test(context.Background(), settings.NewResolved(Row("p"), values(nil), nil))
	require.Len(t, report.Checks, 1)
	assert.False(t, report.Checks[0].Passed)
	assert.Contains(t, report.Checks[0].Remedy, "tick It takes no API key")
	assert.Nil(t, got, "nothing is sent without a key by accident")

	keyless := settings.NewResolved(Row("p"), values(settings.Values{
		FieldNoKey: true, FieldHeaders: []string{"X-Team: search", "X-Shared: plain"},
	}), map[string]string{FieldSecretHeaders: "X-Api-Token: s3cret\nX-Shared: secret", FieldKey: "left over"})
	s := FromResolved(keyless)
	assert.True(t, s.Provider.Keyless)
	assert.Empty(t, s.Provider.Endpoint.APIKey, "a server that takes no key is sent none, whatever is saved")
	assert.Empty(t, s.Provider.Unusable())
	report = def.Test(context.Background(), keyless)
	require.Len(t, report.Checks, 1)
	assert.True(t, report.Checks[0].Passed, report.Checks[0].Detail)
	assert.Empty(t, got.Get("Authorization"))
	assert.Equal(t, "search", got.Get("X-Team"))
	assert.Equal(t, "s3cret", got.Get("X-Api-Token"))
	assert.Equal(t, "secret", got.Get("X-Shared"), "a secret header wins over a plain one of its name")

	// Secret headers that are not headers fail the test, without their text.
	broken := settings.NewResolved(Row("p"), values(settings.Values{FieldNoKey: true}), map[string]string{FieldSecretHeaders: "s3cret-with-no-name"})
	report = def.Test(context.Background(), broken)
	require.Len(t, report.Checks, 1)
	assert.False(t, report.Checks[0].Passed)
	assert.Contains(t, report.Checks[0].Detail, "secret request headers: line 1")
	assert.NotContains(t, report.Checks[0].Detail, "s3cret")
	assert.Nil(t, FromResolved(broken).Provider.Endpoint.Headers, "and no request carries half of them")
	assert.Equal(t, "has request headers that can't be read", FromResolved(broken).Provider.Unusable(), "nor is anything sent without them")

	// The settings refuse plain headers that are not headers, and the mark
	// on a kind that always takes a key.
	err := def.Validate(values(settings.Values{FieldHeaders: []string{"Authorization: Bearer x"}}))
	var fields settings.FieldErrors
	require.ErrorAs(t, err, &fields)
	assert.Contains(t, fields[FieldHeaders], "line 1")
	err = def.Validate(settings.Values{FieldKind: "anthropic", FieldBaseURL: "https://api.anthropic.com", FieldNoKey: true})
	require.ErrorAs(t, err, &fields)
	assert.Equal(t, "Anthropic always takes a key.", fields[FieldNoKey])

	env := ReadEnv([]string{"DIDEROT_PROVIDER_LAB_KEY=k", "DIDEROT_PROVIDER_LAB_SECRET_HEADERS=X-A: 1", "DIDEROT_PROVIDER_LAB_OTHER=x", "PATH=/bin"})
	assert.Equal(t, map[string]string{"DIDEROT_PROVIDER_LAB_KEY": "k", "DIDEROT_PROVIDER_LAB_SECRET_HEADERS": "X-A: 1"}, env)
	assert.Equal(t, "DIDEROT_PROVIDER_LAB_SECRET_HEADERS", EnvName("lab", FieldSecretHeaders))
}
