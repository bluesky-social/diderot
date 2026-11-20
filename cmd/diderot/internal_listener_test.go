package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bluesky-social/diderot/internal/httpserver"
)

func TestInternalServiceHandlerBlocksBrowserRoutes(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := internalServiceHandler(next)

	for _, path := range []string{
		"/", "/admin", "/admin/", "/admin/prompts", "/admin/static/admin.css",
		"/sessions", "/sessions/privacy", "/sessions/static/admin.css",
		"/./admin/prompts", "//admin/prompts", "/v1/../admin/prompts",
		"/auth", "/auth/login", "/auth/callback", "/auth/logout", "/v1/../auth/callback",
	} {
		t.Run("blocks "+path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want 404", path, response.Code)
			}
		})
	}

	for _, path := range []string{"/healthz", "/readyz", "/mcp", "/v1/retrieve"} {
		t.Run("allows "+path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusNoContent {
				t.Fatalf("GET %s status = %d, want 204", path, response.Code)
			}
		})
	}
}

// Metrics and pprof are served only by the metrics listener: the mux the
// public and machine listeners start from has the probes and nothing else,
// so whoever reaches the API cannot read process internals.
func TestMetricsAndPprofOnlyOnTheMetricsListener(t *testing.T) {
	readiness := httpserver.NewReadiness()
	readiness.SetReady("db", true)
	api := probeMux(readiness)
	metrics := metricsHandler()
	status := func(handler http.Handler, path string) int {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response.Code
	}

	for _, path := range []string{"/metrics", "/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline"} {
		if code := status(api, path); code != http.StatusNotFound {
			t.Errorf("API listener GET %s = %d, want 404", path, code)
		}
		if code := status(metrics, path); code != http.StatusOK {
			t.Errorf("metrics listener GET %s = %d, want 200", path, code)
		}
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if code := status(api, path); code != http.StatusOK {
			t.Errorf("API listener GET %s = %d, want 200", path, code)
		}
		if code := status(metrics, path); code != http.StatusNotFound {
			t.Errorf("metrics listener GET %s = %d, want 404", path, code)
		}
	}
}
