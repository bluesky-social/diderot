package adminui

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestAskSessionsPagerDutySurfaceFilter(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	server := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	incidentSession := uuid.New()
	interactiveSession := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO ask_sessions (id, surface) VALUES ($1,'pagerduty'), ($2,'http')`, incidentSession, interactiveSession)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM ask_sessions WHERE id=ANY($1)`, []uuid.UUID{incidentSession, interactiveSession})
	})
	listing, _, err := server.loadAskSessions(ctx, httptest.NewRequest("GET", "/admin/ask/sessions?where=pagerduty", nil))
	require.NoError(t, err)
	require.Equal(t, "pagerduty", listing.Where)
	require.NotEmpty(t, listing.Sessions)
	found := false
	for _, session := range listing.Sessions {
		require.Equal(t, "PagerDuty alert", session.Place.Label)
		require.NotEqual(t, interactiveSession.String(), session.ID)
		if session.ID == incidentSession.String() {
			found = true
		}
	}
	require.True(t, found)
}
