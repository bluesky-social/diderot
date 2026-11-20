-- A turn whose caller went away (an API client that closed its connection,
-- an MCP client that cancelled its task) is its own outcome: not an answer,
-- and not a failure.
ALTER TABLE ask_runs DROP CONSTRAINT ask_runs_outcome_known;
ALTER TABLE ask_runs ADD CONSTRAINT ask_runs_outcome_known
    CHECK (outcome IN ('answered', 'incomplete', 'error', 'stopped', 'cancelled'));
