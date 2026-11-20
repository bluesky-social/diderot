package slack

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"
)

// errNoMember: no member of the workspace has the operator's email.
var errNoMember = errors.New("no member of this workspace has this email")

// operatorUserID resolves an operator's email to a Slack user ID: the
// slack_users directory first, then Slack's users.lookupByEmail, which needs
// users:read.email and never returns deactivated users. A nil pool, or a
// directory that can't be read, goes straight to Slack.
func operatorUserID(ctx context.Context, pool *pgxpool.Pool,
	lookupByEmail func(context.Context, string) (*slack.User, error), email string,
) (string, error) {
	if pool != nil {
		var userID string
		err := pool.QueryRow(ctx, `
			SELECT user_id FROM slack_users
			 WHERE lower(email) = lower($1) AND NOT is_bot
			 ORDER BY fetched_at DESC LIMIT 1`, email).Scan(&userID)
		if err == nil {
			return userID, nil
		}
	}
	user, err := lookupByEmail(ctx, email)
	var response slack.SlackErrorResponse
	switch {
	case errors.As(err, &response) && response.Err == "users_not_found":
		return "", errNoMember
	case err != nil:
		return "", err
	case user.IsBot || user.ID == "":
		return "", errNoMember
	}
	return user.ID, nil
}

// operatorIDs resolves the operators to notify to Slack user IDs, at the
// time of each notification. An email that doesn't resolve is skipped, with
// a warning once per run, so the others are still notified.
func (c *Consumer) operatorIDs(ctx context.Context) []string {
	if c.api == nil {
		return nil
	}
	var ids []string
	for _, email := range c.notifyEmails {
		id, err := operatorUserID(ctx, c.pool, c.lookupUserByEmail, email)
		if err != nil {
			if _, warned := c.unresolvedOperators.LoadOrStore(email, struct{}{}); !warned {
				c.logger.Warn("operators to notify: skipping this email", "email", email, "err", err)
			}
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func (c *Consumer) lookupUserByEmail(ctx context.Context, email string) (*slack.User, error) {
	var user *slack.User
	err := WithSlackRetry(ctx, c.limiter, methodUserLookupByEmail, func() error {
		var apiErr error
		user, apiErr = c.api.GetUserByEmailContext(ctx, email)
		return apiErr
	})
	return user, err
}
