package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/source"
)

// workspaceKey is the State key of the workspace the sync state belongs to.
const workspaceKey = "workspace"

const organizationQuery = `query DiderotWorkspace { organization { id } }`

// OrganizationID returns the ID of the workspace the API key belongs to.
func (c *Client) OrganizationID(ctx context.Context) (string, error) {
	data, _, err := c.Do(ctx, organizationQuery, nil)
	if err != nil {
		return "", fmt.Errorf("linear workspace: %w", err)
	}
	var answer struct {
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return "", fmt.Errorf("linear workspace: decode response: %w", err)
	}
	if answer.Organization.ID == "" {
		return "", errors.New("linear workspace: no organization id")
	}
	return answer.Organization.ID, nil
}

// startWorkspace records the workspace client's API key belongs to. When the
// sync state was kept for another workspace, it starts the sync over: the
// watermark and a backfill in progress mean nothing in this one.
func startWorkspace(ctx context.Context, pool *pgxpool.Pool, state source.State, client *Client) (reset bool, err error) {
	id, err := client.OrganizationID(ctx)
	if err != nil {
		return false, err
	}
	var recorded string
	found, err := state.Get(ctx, workspaceKey, &recorded)
	if err != nil {
		return false, err
	}
	if found && recorded == id {
		return false, nil
	}
	if found {
		if _, err := pool.Exec(ctx, `
			UPDATE linear_sync
			   SET last_sync_at = NULL, backfill_requested_at = NULL, backfill_cursor = NULL,
			       backfill_project = NULL, backfill_project_id = NULL
			 WHERE id = 1`); err != nil {
			return false, fmt.Errorf("linear workspace: reset sync state: %w", err)
		}
	}
	return found, state.Set(ctx, workspaceKey, id)
}
