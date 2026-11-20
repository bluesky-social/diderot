package adminui

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type adminMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newAdminMetrics(reg prometheus.Registerer) *adminMetrics {
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "diderot_admin_ui_requests_total",
		Help: "Administrator browser requests by bounded page, action, method, and response status.",
	}, []string{"page", "action", "method", "status"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "diderot_admin_ui_request_duration_seconds",
		Help:    "Administrator browser request duration by bounded page, action, and method.",
		Buckets: prometheus.ExponentialBucketsRange(0.005, 30, 12),
	}, []string{"page", "action", "method"})
	return &adminMetrics{
		requests: registerAdminCounterVec(reg, requests),
		duration: registerAdminHistogramVec(reg, duration),
	}
}

func registerAdminCounterVec(reg prometheus.Registerer, collector *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return collector
}

func registerAdminHistogramVec(reg prometheus.Registerer, collector *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.HistogramVec)
		}
		panic(err)
	}
	return collector
}

func (s *Server) observeAdmin(next http.Handler) http.Handler {
	if s.metrics == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, action := adminMetricLabels(r)
		method := boundedAdminMethod(r.Method)
		started := time.Now()
		statusWriter := &adminStatusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(statusWriter, r)
		s.metrics.duration.WithLabelValues(page, action, method).Observe(time.Since(started).Seconds())
		s.metrics.requests.WithLabelValues(page, action, method, strconv.Itoa(statusWriter.status)).Inc()
	})
}

func boundedAdminMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
		return method
	default:
		return "other"
	}
}

type adminStatusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *adminStatusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *adminStatusWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func adminMetricLabels(r *http.Request) (page, action string) {
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	page = "unknown"
	if path == "admin" {
		page = "overview"
	} else if len(parts) >= 2 && parts[0] == "admin" {
		switch parts[1] {
		case "ask":
			page = "ask"
		case "connectors":
			page = "connectors"
		case "prompts":
			page = "prompts"
		case "reports":
			page = "reports"
		case "operations":
			page = "operations"
			if len(parts) >= 3 && parts[2] == "audit" {
				page = "audit"
			}
		case "settings":
			page = "settings"
		case "static":
			page = "static"
		}
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return page, "view"
	}
	action = "mutate"
	for _, candidate := range []string{
		"config", "blacklist", "membership", "backfill", "enabled", "run", "delete", "remove",
		"test", "enable", "disable",
	} {
		if strings.HasSuffix(path, "/"+candidate) {
			return page, candidate
		}
	}
	if page == "connectors" && len(parts) == 3 {
		return page, "save"
	}
	if page == "reports" {
		if len(parts) == 2 {
			return page, "create"
		}
		if len(parts) == 3 {
			return page, "update"
		}
	}
	if page == "connectors" && strings.HasSuffix(path, "/roots") {
		return page, "root_add"
	}
	if page == "operations" && strings.HasSuffix(path, "/reembed") {
		return page, "reembed"
	}
	return page, action
}
