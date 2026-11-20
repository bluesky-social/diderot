package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/askconn"
)

// TestGrafanaDatasourcesNamesOnlyPinsThatHoldForEveryQuery: each Grafana
// connection enforces its own pins, so the investigator is told a kind's
// datasource only when every Grafana connection that is on pins it alike.
func TestGrafanaDatasourcesNamesOnlyPinsThatHoldForEveryQuery(t *testing.T) {
	tickets := askconn.Connection{Name: "tickets", Kind: askconn.KindGeneric, Datasources: map[string]string{"prometheus": "ignored"}}
	grafana := askconn.Connection{Name: "grafana", Kind: askconn.KindGrafana, Datasources: map[string]string{"loki": "logs-main", "pyroscope": "profiles"}}
	require.Equal(t, map[string]string{"loki": "logs-main", "pyroscope": "profiles"},
		grafanaDatasources([]askconn.Connection{grafana, tickets}))

	staging := askconn.Connection{Name: "staging", Kind: askconn.KindGrafana, Datasources: map[string]string{"loki": "logs-staging", "pyroscope": "profiles"}}
	require.Equal(t, map[string]string{"pyroscope": "profiles"}, grafanaDatasources([]askconn.Connection{grafana, staging}),
		"two connections pinning Loki apart name neither")

	unpinned := askconn.Connection{Name: "other", Kind: askconn.KindGrafana, Datasources: map[string]string{}}
	require.Empty(t, grafanaDatasources([]askconn.Connection{grafana, unpinned}), "a connection that leaves a kind open names none")
	require.Empty(t, grafanaDatasources(nil))
}
