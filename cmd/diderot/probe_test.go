package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunProbe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantErr    string
	}{
		{name: "healthy", statusCode: http.StatusNoContent},
		{name: "unhealthy", statusCode: http.StatusServiceUnavailable, wantErr: "503 Service Unavailable"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("path = %q, want /healthz", r.URL.Path)
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			err := runProbe(context.Background(), server.Client(), server.URL, "/healthz")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("runProbe() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("runProbe() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestProbeCommandRejectsUnknownProbe(t *testing.T) {
	t.Parallel()

	cmd := probeCmd()
	cmd.SetArgs([]string{"metrics"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `unsupported probe "metrics"`) {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestLocalProbeBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		listenAddr string
		want       string
		wantErr    bool
	}{
		{name: "empty host", listenAddr: ":9091", want: "http://127.0.0.1:9091"},
		{name: "IPv4 wildcard", listenAddr: "0.0.0.0:8080", want: "http://127.0.0.1:8080"},
		{name: "IPv6 wildcard", listenAddr: "[::]:7070", want: "http://127.0.0.1:7070"},
		{name: "specific host", listenAddr: "localhost:6060", want: "http://localhost:6060"},
		{name: "invalid", listenAddr: "9091", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := localProbeBaseURL(tt.listenAddr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("localProbeBaseURL(%q) error = nil", tt.listenAddr)
				}
				return
			}
			if err != nil {
				t.Fatalf("localProbeBaseURL(%q) error = %v", tt.listenAddr, err)
			}
			if got != tt.want {
				t.Fatalf("localProbeBaseURL(%q) = %q, want %q", tt.listenAddr, got, tt.want)
			}
		})
	}
}

func TestDiderotListenAddr(t *testing.T) {
	t.Setenv("DIDEROT_LISTEN_ADDR", "")
	if got := diderotListenAddr(); got != ":9091" {
		t.Fatalf("diderotListenAddr() = %q, want :9091", got)
	}

	t.Setenv("DIDEROT_LISTEN_ADDR", ":8080")
	if got := diderotListenAddr(); got != ":8080" {
		t.Fatalf("diderotListenAddr() = %q, want :8080", got)
	}
}

func TestDiderotInternalListenAddr(t *testing.T) {
	t.Setenv("DIDEROT_INTERNAL_LISTEN_ADDR", "")
	if got := diderotInternalListenAddr(); got != ":9092" {
		t.Fatalf("diderotInternalListenAddr() = %q, want :9092", got)
	}

	t.Setenv("DIDEROT_INTERNAL_LISTEN_ADDR", ":9092")
	if got := diderotInternalListenAddr(); got != ":9092" {
		t.Fatalf("diderotInternalListenAddr() = %q, want :9092", got)
	}
}
