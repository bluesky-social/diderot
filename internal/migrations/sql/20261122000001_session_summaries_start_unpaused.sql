-- Agent-session summaries run whenever the deployment allows them and a model
-- can write them; the switch on the Operations page is a pause an operator
-- turns on. The baseline seeded that switch paused.
ALTER TABLE session_ingest_control ALTER COLUMN summarization_enabled SET DEFAULT true;

-- Every server start writes a row of connector_settings for each connector
-- it registers, and no migration before this one writes any row there but
-- org. So a database with no other row is a new install that no server has
-- started on yet, where nobody can have chosen the pause: clear the seed
-- there. Anywhere else the switch stays as it is, since a pause there may be
-- an operator's.
UPDATE session_ingest_control SET summarization_enabled = true, updated_at = now()
 WHERE NOT EXISTS (SELECT 1 FROM connector_settings WHERE connector <> 'org');
