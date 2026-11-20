package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// FieldProject is the backfill's argument: the project to re-read.
const FieldProject = "project"

// Backfill is a re-read of every issue, or of one project's.
func (Connector) Backfill() source.Backfill {
	return source.Backfill{
		Summary: "Re-read every issue, or one project's.",
		Confirm: "Re-read the issues", Action: "Re-read",
		Fields: []settings.Field{{Key: FieldProject, Label: "Project", Kind: settings.KindText,
			Help: "Leave empty to re-read every project"}},
	}
}

// RequestBackfill resets the durable Linear page cursor. The project may be
// a project name or UUID; the sync worker resolves it once.
func (Connector) RequestBackfill(ctx context.Context, deps source.Deps, args settings.Values) (time.Time, error) {
	project, _ := args[FieldProject].(string)
	var requestedAt time.Time
	err := deps.Pool.QueryRow(ctx, `
		UPDATE linear_sync
		   SET backfill_requested_at = clock_timestamp(),
		       backfill_cursor = NULL,
		       backfill_project = nullif($1, ''),
		       backfill_project_id = NULL
		 WHERE id = 1
		RETURNING backfill_requested_at
	`, project).Scan(&requestedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("request linear backfill: %w", err)
	}
	return requestedAt, nil
}

const backfillQuery = `
query Backfill($after: String, $first: Int!) {
  issues(orderBy: updatedAt first: $first after: $after includeArchived: true) {
    nodes {
      id identifier title description url createdAt updatedAt priority
      creator { id name displayName email }
      assignee { id name displayName email }
      state { name type }
      team { id name key private }
      project { id name }
      labels { nodes { name } }
      comments(first: 50) {
        nodes { id body createdAt updatedAt user { id name displayName email } }
        pageInfo { hasNextPage endCursor }
      }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

const backfillProjectQuery = `
query BackfillProject($after: String, $first: Int!, $projectID: ID!) {
  issues(
    filter: { project: { id: { eq: $projectID } } }
    orderBy: updatedAt first: $first after: $after includeArchived: true
  ) {
    nodes {
      id identifier title description url createdAt updatedAt priority
      creator { id name displayName email }
      assignee { id name displayName email }
      state { name type }
      team { id name key private }
      project { id name }
      labels { nodes { name } }
      comments(first: 50) {
        nodes { id body createdAt updatedAt user { id name displayName email } }
        pageInfo { hasNextPage endCursor }
      }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

type TeamVolume struct {
	TeamKey      string
	TeamName     string
	IssueCount   int
	CommentCount int
	Private      bool
	Excluded     bool
}

type BackfillResult struct {
	TotalIssues    int
	TotalComments  int
	TotalUpserted  int
	TotalNoChange  int
	TotalRefused   int
	TotalErrors    int
	TeamVolumes    []TeamVolume
	ComplexityUsed int
}

type BackfillOptions struct {
	DryRun           bool
	PageSize         int
	ResumeFromCursor string
	ProjectID        string
}

// BackfillBatch is the durable boundary for one Linear issues page.
type BackfillBatch struct {
	BackfillResult
	NextCursor string
	Done       bool
	complexity ComplexityInfo
}

type BackfillRunner struct {
	client *Client
	worker *SyncWorker
}

func NewBackfillRunner(client *Client, worker *SyncWorker) *BackfillRunner {
	return &BackfillRunner{client: client, worker: worker}
}

// Run is the in-process traversal used by tests and callers that need a
// complete result. Production operator replay persists NextCursor in
// linear_sync after each RunPage call.
func (r *BackfillRunner) Run(ctx context.Context, opts BackfillOptions) (BackfillResult, error) {
	cursor := opts.ResumeFromCursor
	var out BackfillResult
	volumes := map[string]*TeamVolume{}
	for {
		batch, err := r.RunPage(ctx, cursor, opts.ProjectID, opts.PageSize, opts.DryRun)
		if err != nil {
			return out, err
		}
		out.TotalIssues += batch.TotalIssues
		out.TotalComments += batch.TotalComments
		out.TotalUpserted += batch.TotalUpserted
		out.TotalNoChange += batch.TotalNoChange
		out.TotalRefused += batch.TotalRefused
		out.TotalErrors += batch.TotalErrors
		out.ComplexityUsed += batch.ComplexityUsed
		for _, v := range batch.TeamVolumes {
			if volumes[v.TeamKey] == nil {
				copy := v
				volumes[v.TeamKey] = &copy
			} else {
				volumes[v.TeamKey].IssueCount += v.IssueCount
				volumes[v.TeamKey].CommentCount += v.CommentCount
			}
		}
		if batch.Done {
			break
		}
		cursor = batch.NextCursor
	}
	for _, v := range volumes {
		out.TeamVolumes = append(out.TeamVolumes, *v)
	}
	return out, nil
}

// RunPage fetches and reconciles exactly one GraphQL issues page.
// Replaying a page is harmless because upsertIssue uses stable issue IDs.
func (r *BackfillRunner) RunPage(ctx context.Context, cursor, projectID string, pageSize int, dryRun bool) (BackfillBatch, error) {
	if pageSize <= 0 {
		pageSize = syncPageSize
	}
	query := backfillQuery
	vars := map[string]any{"first": pageSize}
	if cursor != "" {
		vars["after"] = cursor
	}
	if projectID != "" {
		query = backfillProjectQuery
		vars["projectID"] = projectID
	}
	return r.runIssuePage(ctx, query, vars, dryRun)
}

// runIssuePage is the shared source-level batch used by automatic
// incremental/initial sync and operator replay.
func (r *BackfillRunner) runIssuePage(ctx context.Context, query string, vars map[string]any, dryRun bool) (BackfillBatch, error) {
	data, ci, err := r.client.Do(ctx, query, vars)
	if err != nil {
		return BackfillBatch{}, fmt.Errorf("linear API: %w", err)
	}
	var resp struct {
		Issues struct {
			Nodes    []Issue  `json:"nodes"`
			PageInfo PageInfo `json:"pageInfo"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return BackfillBatch{}, fmt.Errorf("decode response: %w", err)
	}
	batch := BackfillBatch{
		BackfillResult: BackfillResult{ComplexityUsed: ci.RequestedTokens},
		NextCursor:     resp.Issues.PageInfo.EndCursor,
		Done:           !resp.Issues.PageInfo.HasNextPage,
		complexity:     ci,
	}
	volumes := map[string]*TeamVolume{}
	for i := range resp.Issues.Nodes {
		issue := &resp.Issues.Nodes[i]
		commentCount := 0
		commentOverflowFailed := false
		// A refused issue (private/excluded team) is never written, so its
		// comments are never read back either: fetching the overflow page
		// for it would be a wasted API call, and worse, a failure on that
		// wasted call must not stand between this issue and the privacy
		// guard below (see upsertIssue's guardReason check, which runs
		// unconditionally further down regardless of commentOverflowFailed).
		refused := r.worker.guardReason(issue) != ""
		if issue.Comments != nil {
			commentCount = len(issue.Comments.Nodes)
			if !dryRun && !refused && issue.Comments.PageInfo.HasNextPage {
				if err := r.worker.fetchRemainingComments(ctx, issue); err != nil {
					batch.TotalErrors++
					commentOverflowFailed = true
					r.worker.logger.Warn("linear: comment fetch failed; issue not written this page so the next sync retries it",
						"issue_id", issue.ID, "err", err)
				}
				commentCount = len(issue.Comments.Nodes)
			}
		}
		batch.TotalIssues++
		batch.TotalComments += commentCount
		tk := teamKey(issue)
		if tk == "" {
			tk = "(no team)"
		}
		if volumes[tk] == nil {
			name := tk
			if issue.Team != nil && issue.Team.Name != "" {
				name = issue.Team.Name
			}
			volumes[tk] = &TeamVolume{
				TeamKey: tk, TeamName: name,
				Private:  issue.Team != nil && issue.Team.Private,
				Excluded: r.worker.isExcluded(issue),
			}
		}
		volumes[tk].IssueCount++
		volumes[tk].CommentCount += commentCount
		if dryRun {
			continue
		}
		if commentOverflowFailed {
			// Never write an issue whose comment set is known to be
			// incomplete: that would silently replace a previously
			// complete stored body with a truncated one, or persist a
			// new issue as if its truncated body were current.
			// batch.TotalErrors above already holds the caller's
			// watermark/cursor back so the next run retries the whole
			// fetch for this issue.
			continue
		}
		outcome, err := r.worker.upsertIssue(ctx, issue)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return batch, err
			}
			r.worker.metrics.syncUpserts.WithLabelValues("err").Inc()
			r.worker.logger.Error("linear: issue upsert failed",
				"issue_id", issue.ID, "identifier", issue.Identifier, "err", err)
			batch.TotalErrors++
			continue
		}
		switch outcome {
		case outcomeInserted, outcomeUpdated:
			batch.TotalUpserted++
		case outcomeNoChange:
			batch.TotalNoChange++
		case outcomeRefused:
			batch.TotalRefused++
		}
	}
	for _, v := range volumes {
		batch.TeamVolumes = append(batch.TeamVolumes, *v)
	}
	return batch, nil
}
