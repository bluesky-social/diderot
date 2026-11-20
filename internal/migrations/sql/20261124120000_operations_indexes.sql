-- Chunks not yet in the search index, including those that failed every
-- attempt: the Status and Failures pages count and list the ones that gave
-- up, which idx_chunks_unembedded leaves out.
CREATE INDEX event_chunks_not_embedded ON event_chunks (occurred_at DESC, id DESC)
    WHERE embedded_at IS NULL;

-- The Overview counts and charts Ask's questions by when they were asked.
CREATE INDEX ask_runs_started_at ON ask_runs (started_at);
