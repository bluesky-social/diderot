-- Ask answers in Discord: its runs, sessions and attachment deliveries are
-- recorded under the surface 'discord'.
ALTER TABLE ask_sessions DROP CONSTRAINT ask_sessions_surface_known;
ALTER TABLE ask_sessions ADD CONSTRAINT ask_sessions_surface_known
    CHECK ((surface IS NULL) OR (surface IN ('http', 'slack', 'discord', 'scheduled', 'pagerduty', 'unknown')));
ALTER TABLE ask_runs DROP CONSTRAINT ask_runs_surface_known;
ALTER TABLE ask_runs ADD CONSTRAINT ask_runs_surface_known
    CHECK (surface IN ('http', 'slack', 'discord', 'scheduled', 'pagerduty'));
ALTER TABLE ask_artifact_deliveries DROP CONSTRAINT ask_artifact_deliveries_surface_check;
ALTER TABLE ask_artifact_deliveries ADD CONSTRAINT ask_artifact_deliveries_surface_check
    CHECK (surface IN ('slack', 'discord', 'scheduled'));

-- Working records name their surface, so each surface's sweep clears only
-- its own conversations.
ALTER TABLE slack_working_threads ADD COLUMN surface text NOT NULL DEFAULT 'slack';
ALTER TABLE slack_working_threads ALTER COLUMN surface DROP DEFAULT;
COMMENT ON COLUMN slack_working_threads.surface IS 'The Ask surface whose conversation this is: slack or discord.';
