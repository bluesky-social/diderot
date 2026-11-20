package adminui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestAdminMetricLabelsStayBounded(t *testing.T) {
	tests := []struct {
		method, path, page, action string
	}{
		{http.MethodGet, "/admin/", "overview", "view"},
		{http.MethodGet, "/admin/ask/sessions/1f1fab90-87e8-4b3e-9f7d-082fd73899f4", "ask", "view"},
		{http.MethodPost, "/admin/connectors/slack/channels/C0123456789/backfill", "connectors", "backfill"},
		{http.MethodPost, "/admin/connectors/slack", "connectors", "save"},
		{http.MethodPost, "/admin/connectors/slack/test", "connectors", "test"},
		{http.MethodPost, "/admin/connectors/slack/enable", "connectors", "enable"},
		{http.MethodPost, "/admin/reports", "reports", "create"},
		{http.MethodPost, "/admin/reports/1f1fab90-87e8-4b3e-9f7d-082fd73899f4", "reports", "update"},
		{http.MethodPost, "/admin/connectors/notion/roots", "connectors", "root_add"},
		{http.MethodPost, "/admin/connectors/notion/roots/remove", "connectors", "remove"},
		{http.MethodPost, "/admin/connectors/notion/backfill", "connectors", "backfill"},
		{http.MethodPost, "/admin/operations/reembed", "operations", "reembed"},
		{http.MethodGet, "/admin/operations/audit?actor=operator@example.com", "audit", "view"},
		{http.MethodGet, "/admin/operations", "operations", "view"},
		{http.MethodGet, "/admin/settings", "settings", "view"},
		{http.MethodPost, "/admin/settings/models", "settings", "mutate"},
		{http.MethodPost, "/admin/settings/actions/go-live", "settings", "mutate"},
		{http.MethodGet, "/admin/static/admin.css", "static", "view"},
		{http.MethodDelete, "/admin/future/tenant-specific-value", "unknown", "mutate"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			page, action := adminMetricLabels(httptest.NewRequest(tt.method, tt.path, nil))
			assert.Equal(t, tt.page, page)
			assert.Equal(t, tt.action, action)
			assert.NotContains(t, page, "1f1fab90")
			assert.NotContains(t, action, "C012")
		})
	}
}

func TestObserveAdminRecordsStatusAndDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := newAdminMetrics(reg)
	s := &Server{metrics: metrics}
	handler := s.observeAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusTeapot)
	}))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/connectors/slack/channels/C0123456789/backfill", nil))

	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.requests.WithLabelValues("connectors", "backfill", "POST", "418")))
	assert.Equal(t, 1, testutil.CollectAndCount(metrics.duration, "diderot_admin_ui_request_duration_seconds"))
}

func TestAuthedBrowserMetricsIncludeAuthorizationFailures(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := newAdminMetrics(reg)
	s := &Server{authenticator: testBrowserAuth(), metrics: metrics, adminGroups: map[string]struct{}{"admins": {}}}

	w := httptest.NewRecorder()
	s.authedBrowser(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		w, httptest.NewRequest(http.MethodGet, "/admin/ask/sessions", nil),
	)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.requests.WithLabelValues("ask", "view", "GET", "401")))

	w = httptest.NewRecorder()
	s.authedBrowser(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		w, httptest.NewRequest("CUSTOM-METHOD", "/admin/future", nil),
	)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.requests.WithLabelValues("unknown", "mutate", "other", "401")))
}
