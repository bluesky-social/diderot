package adminapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/bluesky-social/diderot/internal/sessioningest"
)

type myAgentSessionsResponse struct {
	Owner      string                       `json:"owner"`
	Sessions   []sessioningest.OwnerSession `json:"sessions"`
	NextBefore int64                        `json:"next_before,omitempty"`
}

func (s *Server) handleMyAgentSessions(w http.ResponseWriter, r *http.Request) {
	service := s.getSessionPrivacyService()
	if !requireBackend(w, service, "agent session privacy unavailable") {
		return
	}
	claims := operatorFromContext(r.Context())
	if claims == nil {
		http.Error(w, "team member access required", http.StatusForbidden)
		return
	}
	owner := sessioningest.CanonicalOwner(claims.Email)
	limit, err := parseNonNegativeInt(r.URL.Query().Get("limit"), "limit")
	if err != nil {
		badRequest(w, err)
		return
	}
	before, err := parseNonNegativeInt64(r.URL.Query().Get("before"), "before")
	if err != nil {
		badRequest(w, err)
		return
	}
	page, err := service.ListSessions(r.Context(), owner, limit, before)
	if err != nil {
		s.logger.Error("self-service session list failed", "owner", owner, "err", err)
		http.Error(w, "agent session query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	s.writeJSON(w, http.StatusOK, myAgentSessionsResponse{
		Owner: owner, Sessions: page.Sessions, NextBefore: page.NextBefore,
	})
}

type updateMyAgentSessionPrivacyRequest struct {
	Sessions  []sessioningest.SessionRef `json:"sessions"`
	OwnerOnly *bool                      `json:"owner_only"`
}

func (s *Server) handleMyAgentSessionPrivacy(w http.ResponseWriter, r *http.Request) {
	service := s.getSessionPrivacyService()
	if !requireBackend(w, service, "agent session privacy unavailable") {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req updateMyAgentSessionPrivacyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		badRequest(w, err)
		return
	}
	if err := ensureJSONEOF(dec); err != nil {
		badRequest(w, err)
		return
	}
	if req.OwnerOnly == nil {
		badRequest(w, fmt.Errorf("owner_only is required"))
		return
	}
	if len(req.Sessions) == 0 || len(req.Sessions) > 200 {
		badRequest(w, fmt.Errorf("select between 1 and 200 sessions"))
		return
	}
	for _, session := range req.Sessions {
		if session.Client == "" || session.SessionID == "" {
			badRequest(w, fmt.Errorf("client and session_id are required"))
			return
		}
	}
	claims := operatorFromContext(r.Context())
	if claims == nil {
		http.Error(w, "team member access required", http.StatusForbidden)
		return
	}
	owner := sessioningest.CanonicalOwner(claims.Email)
	result, err := service.SetSessions(r.Context(), owner, req.Sessions, *req.OwnerOnly)
	if err != nil {
		if errors.Is(err, sessioningest.ErrInvalidSessionSelection) || errors.Is(err, sessioningest.ErrSessionNotFound) {
			badRequest(w, err)
			return
		}
		s.logger.Error("self-service privacy update failed", "owner", owner, "err", err)
		http.Error(w, "agent session privacy update failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	s.writeJSON(w, http.StatusOK, result)
}

func parseNonNegativeInt(raw, name string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}

func parseNonNegativeInt64(raw, name string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("request body must contain one JSON object")
	}
	return err
}
