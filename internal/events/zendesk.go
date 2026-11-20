package events

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (sink *DBSink) ZendeskEventID(ctx context.Context, canonicalURL string, allowedScopes []string) (int64, error) {
	var eventID int64
	err := sink.pool.QueryRow(ctx, `SELECT id FROM events
		WHERE source = 'zendesk' AND url = $1 AND NOT deleted AND scope = ANY($2::text[])
		AND (external_id = thread_id OR starts_with(external_id, 'article:'))
		ORDER BY (scope = 'public') DESC, id LIMIT 1`, canonicalURL, ReadableScopes(allowedScopes)).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrEventNotFound
	}
	return eventID, err
}
