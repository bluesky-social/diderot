package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSessionPrivacy struct {
	listOwner   string
	setOwner    string
	setValue    bool
	setSessions []sessioningest.SessionRef
	setErr      error
}

func (f *fakeSessionPrivacy) SetSessions(_ context.Context, owner string, sessions []sessioningest.SessionRef, value bool) (sessioningest.SessionPrivacyUpdate, error) {
	f.setOwner, f.setSessions, f.setValue = owner, sessions, value
	if f.setErr != nil {
		return sessioningest.SessionPrivacyUpdate{}, f.setErr
	}
	return sessioningest.SessionPrivacyUpdate{OwnerOnly: value, SessionUpdates: len(sessions)}, nil
}

type sessionPrivacyFakeAdapter struct{ *fakeSessionPrivacy }

func (f sessionPrivacyFakeAdapter) ListSessions(_ context.Context, owner string, _ int, _ int64) (sessioningest.OwnerSessionPage, error) {
	f.listOwner = owner
	return sessioningest.OwnerSessionPage{Sessions: []sessioningest.OwnerSession{{
		Client: "codex", SessionID: "ses_mine", Title: "Repair session privacy", ChangeSeq: 4,
	}}}, nil
}

func newSessionPrivacyAPITestServer(t *testing.T, claims *auth.Principal, privacy SessionPrivacyService) http.Handler {
	s := NewServer(fakeVerifier{validToken: "token", claims: claims}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetSelfServiceGroups([]string{"everyone"})
	s.SetSessionPrivacyService(privacy)
	mux := http.NewServeMux()
	require.NoError(t, s.Mount(mux))
	return mux
}

func TestSessionPrivacyAPIUsesOnlyVerifiedOwner(t *testing.T) {
	backend := &fakeSessionPrivacy{}
	h := newSessionPrivacyAPITestServer(t, &auth.Principal{
		Subject: "user", Email: " Me@Example.com ", Groups: []string{"everyone"},
	}, sessionPrivacyFakeAdapter{backend})

	req := httptest.NewRequest(http.MethodGet, "/v1/me/agent-sessions?limit=20", nil)
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "me@example.com", backend.listOwner)
	var response myAgentSessionsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Len(t, response.Sessions, 1)
	assert.Equal(t, "me@example.com", response.Owner)
	assert.Equal(t, "ses_mine", response.Sessions[0].SessionID)
	assert.Equal(t, "Repair session privacy", response.Sessions[0].Title)

	req = httptest.NewRequest(http.MethodPut, "/v1/me/agent-session-privacy", strings.NewReader(`{"sessions":[{"client":"codex","session_id":"ses_mine"}],"owner_only":false}`))
	req.Header.Set("Authorization", "Bearer token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "me@example.com", backend.setOwner)
	assert.False(t, backend.setValue)
	assert.Equal(t, []sessioningest.SessionRef{{Client: "codex", SessionID: "ses_mine"}}, backend.setSessions)
}

func TestSessionPrivacyAPIRequiresTeamGroupAndEmail(t *testing.T) {
	for _, claims := range []*auth.Principal{
		{Subject: "outsider", Email: "user@example.com", Groups: []string{"other"}},
		{Subject: "no-email", Groups: []string{"everyone"}},
	} {
		h := newSessionPrivacyAPITestServer(t, claims, sessionPrivacyFakeAdapter{&fakeSessionPrivacy{}})
		req := httptest.NewRequest(http.MethodGet, "/v1/me/agent-sessions", nil)
		req.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
	}
}

func TestSessionPrivacyAPIRejectsCallerSuppliedOwner(t *testing.T) {
	backend := &fakeSessionPrivacy{}
	h := newSessionPrivacyAPITestServer(t, &auth.Principal{
		Subject: "user", Email: "me@example.com", Groups: []string{"everyone"},
	}, sessionPrivacyFakeAdapter{backend})
	req := httptest.NewRequest(http.MethodPut, "/v1/me/agent-session-privacy",
		strings.NewReader(`{"sessions":[{"client":"codex","session_id":"ses_mine"}],"owner_only":true,"owner":"other@example.com"}`))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, backend.setOwner)
}

func TestSessionPrivacyAPIMapsInvalidSelectionToBadRequest(t *testing.T) {
	backend := &fakeSessionPrivacy{setErr: sessioningest.ErrInvalidSessionSelection}
	h := newSessionPrivacyAPITestServer(t, &auth.Principal{
		Subject: "user", Email: "me@example.com", Groups: []string{"everyone"},
	}, sessionPrivacyFakeAdapter{backend})
	req := httptest.NewRequest(http.MethodPut, "/v1/me/agent-session-privacy",
		strings.NewReader(`{"sessions":[{"client":"codex","session_id":"ses_mine"}],"owner_only":true}`))
	req.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
