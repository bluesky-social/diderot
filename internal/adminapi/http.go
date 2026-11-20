package adminapi

import (
	"encoding/json"
	"net/http"
	"reflect"
)

// http.go holds the small response/request helpers the handlers
// share. The goal is that a handler body reads as parse/validate →
// backend fetch → business call → writeJSON, with the JSON
// encode/decode and nil-backend boilerplate factored out here.

// writeJSON sets the JSON content type, writes status, and encodes
// v. Encode errors are logged but not propagated — by the time an
// encode fails the client has already received the status + headers,
// so there's nothing actionable to return.
//
// This is the single response helper for the package; pass
// http.StatusOK for the common case. Replaces the two earlier
// variants (the status-less *Server.writeJSON and the package-level
// writeJSON(w, status, body)) that had drifted apart.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Warn("writeJSON: encode failed", "err", err)
	}
}

// readJSON decodes the request body into v. It returns the decode
// error verbatim; callers map it to a 400 via badRequest so the wire
// shape stays consistent ("bad request: <detail>").
func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// badRequest writes a 400 with the "bad request: <detail>" shape the
// handlers converged on. Use it for body-decode failures and for
// field-validation errors that carry their own message via err.
func badRequest(w http.ResponseWriter, err error) {
	http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
}

// requireBackend returns true when the backend b is wired. When it's
// nil it writes a 503 with unavailableMsg and returns false, so the
// handler can `if !requireBackend(...) { return }`.
//
// The package's backends are a mix of interface-typed fields (e.g.
// SyncWorker, Expander) and one pointer-typed field (*pgxpool.Pool).
// A plain any(b)==nil check would mishandle the pointer case — a nil
// *pgxpool.Pool boxed into an interface is non-nil — so isNilBackend
// reflects on the kind to catch nil interfaces and nil pointers
// alike.
func requireBackend[T any](w http.ResponseWriter, b T, unavailableMsg string) bool {
	if isNilBackend(b) {
		http.Error(w, unavailableMsg, http.StatusServiceUnavailable)
		return false
	}
	return true
}

// isNilBackend reports whether v is an untyped nil or a nil pointer/
// interface/map/etc. boxed in an interface. Used by requireBackend so
// the typed-nil footgun doesn't let an unset *pgxpool.Pool pass the
// wired check.
func isNilBackend(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}
