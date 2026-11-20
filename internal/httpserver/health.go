// Package httpserver contains the liveness and readiness handlers that the
// public and machine listeners serve (docs/configuration.md#core).
package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

func HealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintln(w, "ok")
	})
}

// Readiness tracks named subsystem readiness. SetReady("foo", true)
// marks "foo" ready; ServeHTTP returns 200 only when every tracked
// subsystem is ready, 503 with a JSON body listing which aren't
// otherwise.
type Readiness struct {
	mu     sync.RWMutex
	checks map[string]bool
	// reasons says, for a check that is not ready, why.
	reasons map[string]string
}

func NewReadiness() *Readiness {
	return &Readiness{checks: map[string]bool{}, reasons: map[string]string{}}
}

func (r *Readiness) SetReady(name string, ready bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks[name] = ready
	delete(r.reasons, name)
}

// SetNotReady marks the check not ready and says why; the reason is in the
// probe's answer, for whoever reads why a rollout stopped.
func (r *Readiness) SetNotReady(name, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks[name] = false
	r.reasons[name] = reason
}

// NotReady returns the reason of each check that is not ready and has one.
func (r *Readiness) NotReady() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reasons := map[string]string{}
	for name, reason := range r.reasons {
		if !r.checks[name] {
			reasons[name] = reason
		}
	}
	return reasons
}

func (r *Readiness) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var notReady []string
	for name, ok := range r.checks {
		if !ok {
			notReady = append(notReady, name)
		}
	}
	sort.Strings(notReady)

	if len(r.checks) == 0 || len(notReady) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		reasons := map[string]string{}
		for _, name := range notReady {
			if reason := r.reasons[name]; reason != "" {
				reasons[name] = reason
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "not ready",
			"not_ready": notReady,
			"reasons":   reasons,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
