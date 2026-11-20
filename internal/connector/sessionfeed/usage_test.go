package sessionfeed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetSessionUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/sessions/ask%2F1/usage", r.URL.EscapedPath())
		assert.Equal(t, "Bearer skt_test", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":1,"session_id":"ask/1","currency":"USD","metered_cost_usd":"1.25","inference_count":4,"attempt_count":4,"included_attempt_count":2,"unknown_billing_attempt_count":0,"unpriced_metered_attempt_count":2,"input_tokens":100,"output_tokens":10,"cache_read_tokens":1000,"cache_write_tokens":26,"has_estimates":false,"coverage_complete":false,"effective_revision":"rev","observed_through":"2026-08-03T12:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)

	usage, err := NewClient(srv.URL, "skt_test", srv.Client()).GetSessionUsage(context.Background(), "ask/1")
	require.NoError(t, err)
	assert.Equal(t, 1, usage.SchemaVersion)
	assert.Equal(t, "ask/1", usage.SessionID)
	assert.Equal(t, "USD", usage.Currency)
	assert.Equal(t, "1.25", usage.MeteredCostUSD)
	assert.Equal(t, int64(4), usage.InferenceCount)
	assert.Equal(t, int64(4), usage.AttemptCount)
	assert.Equal(t, int64(2), usage.IncludedAttemptCount)
	assert.Equal(t, int64(0), usage.UnknownBillingAttemptCount)
	assert.Equal(t, int64(2), usage.UnpricedMeteredAttemptCount)
	assert.Equal(t, int64(100), usage.InputTokens)
	assert.Equal(t, int64(10), usage.OutputTokens)
	assert.Equal(t, int64(1000), usage.CacheReadTokens)
	assert.Equal(t, int64(26), usage.CacheWriteTokens)
	assert.False(t, usage.HasEstimates)
	assert.False(t, usage.CoverageComplete)
	assert.Equal(t, "rev", usage.EffectiveRevision)
	require.NotNil(t, usage.ObservedThrough)
}

func TestGetWorkUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/work-usage/diderot.ask/ask%2F1", r.URL.EscapedPath())
		assert.Equal(t, "Bearer skt_test", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":1,"work_reference":{"namespace":"diderot.ask","reference_id":"ask/1"},"currency":"USD","metered_cost_usd":"1.25","attempt_count":4,"coverage_complete":true}`))
	}))
	t.Cleanup(srv.Close)

	usage, err := NewClient(srv.URL, "skt_test", srv.Client()).GetWorkUsage(context.Background(), "diderot.ask", "ask/1")
	require.NoError(t, err)
	assert.Empty(t, usage.SessionID)
	require.NotNil(t, usage.WorkReference)
	assert.Equal(t, "diderot.ask", usage.WorkReference.Namespace)
	assert.Equal(t, "ask/1", usage.WorkReference.ReferenceID)
	assert.Equal(t, "1.25", usage.MeteredCostUSD)
	assert.True(t, usage.CoverageComplete)
}

func TestGetSessionUsageSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL, "skt_test", srv.Client()).GetSessionUsage(context.Background(), "ask-1")
	require.Error(t, err)
	assert.True(t, IsTransient(err))
}

func TestGetSessionUsageSurfacesNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such session", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL, "skt_test", srv.Client()).GetSessionUsage(context.Background(), "ask-1")
	require.Error(t, err)
	assert.True(t, IsNotFound(err))
}

func TestGetSessionUsageRejectsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL, "skt_test", srv.Client()).GetSessionUsage(context.Background(), "ask-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode json")
}

func TestGetSessionUsageRejectsMismatchedSessionID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"some-other-session","metered_cost_usd":"1.25"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL, "skt_test", srv.Client()).GetSessionUsage(context.Background(), "ask-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `session_id "some-other-session" does not match requested "ask-1"`)
}

func TestGetWorkUsageRejectsMismatchedReference(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"work_reference":{"namespace":"other","reference_id":"y"}}`))
	}))
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL, "skt_test", srv.Client()).GetWorkUsage(context.Background(), "diderot.ask", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match requested diderot.ask/x")
}

// Missing accounting is not an error: the feed answers 200 with the requested
// id and an empty metered string so the UI can render the incomplete state.
func TestGetSessionUsageEchoesZeroTotals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"ask-1","currency":"USD","metered_cost_usd":"","coverage_complete":false}`))
	}))
	t.Cleanup(srv.Close)

	usage, err := NewClient(srv.URL, "skt_test", srv.Client()).GetSessionUsage(context.Background(), "ask-1")
	require.NoError(t, err)
	assert.Equal(t, "ask-1", usage.SessionID)
	assert.Empty(t, usage.MeteredCostUSD)
	assert.False(t, usage.CoverageComplete)
}
