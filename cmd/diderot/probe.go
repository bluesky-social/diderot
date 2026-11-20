package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/diderot/internal/config"
)

func probeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "probe <healthz|readyz>",
		Short: "Check the local diderot server from a Kubernetes exec probe",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "/" + args[0]
			if path != "/healthz" && path != "/readyz" {
				return fmt.Errorf("unsupported probe %q", args[0])
			}

			baseURL, err := localProbeBaseURL(diderotListenAddr())
			if err != nil {
				return err
			}

			client := &http.Client{Timeout: 2 * time.Second}
			return runProbe(cmd.Context(), client, baseURL, path)
		},
	}
}

func diderotListenAddr() string {
	if addr := os.Getenv("DIDEROT_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":9091"
}

func diderotInternalListenAddr() string {
	if addr := os.Getenv("DIDEROT_INTERNAL_LISTEN_ADDR"); addr != "" {
		return addr
	}
	return ":9092"
}

// listenerNames names serveListenAddrs' listeners, in order, for the logs.
var listenerNames = []string{"public", "internal", "pagerduty-webhook", "metrics"}

// serveListenAddrs lists every address `serve` listens on: public, internal,
// the PagerDuty webhook, and metrics. The webhook and metrics, unless
// DIDEROT_PAGERDUTY_LISTEN_ADDR and DIDEROT_METRICS_LISTEN_ADDR say
// otherwise, bind every interface, except under DIDEROT_AUTH_MODE=none,
// whose listeners must all be loopback.
func serveListenAddrs(cfg *config.Config) []string {
	pagerDuty, metrics := ":9093", ":9094"
	if cfg.AuthMode == config.AuthModeNone {
		pagerDuty, metrics = "127.0.0.1:9093", "127.0.0.1:9094"
	}
	if addr := os.Getenv("DIDEROT_PAGERDUTY_LISTEN_ADDR"); addr != "" {
		pagerDuty = addr
	}
	if addr := os.Getenv("DIDEROT_METRICS_LISTEN_ADDR"); addr != "" {
		metrics = addr
	}
	return []string{diderotListenAddr(), diderotInternalListenAddr(), pagerDuty, metrics}
}

func localProbeBaseURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("parse DIDEROT_LISTEN_ADDR: %w", err)
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func runProbe(ctx context.Context, client *http.Client, baseURL, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("probe %s: unexpected HTTP status %s", path, resp.Status)
	}
	return nil
}
