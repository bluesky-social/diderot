-- When each item's work last failed. The three work queues kept a failure
-- count and the last error but not when it happened, so the Failures page
-- could date a failure only by the item's own date at its source.
--
-- The column is written with last_failure, by the database clock, and
-- cleared with it. It stays NULL for a failure recorded before this
-- migration: nothing else in the row says when that was.
ALTER TABLE event_chunks ADD COLUMN last_failed_at timestamptz;
ALTER TABLE event_images ADD COLUMN last_failed_at timestamptz;
ALTER TABLE event_documents ADD COLUMN last_failed_at timestamptz;

-- The Failures page reads the most recently failed items first. Chunks are
-- the one queue large enough to need an index for that; it holds only the
-- chunks that are failing.
CREATE INDEX event_chunks_failing ON event_chunks (last_failed_at DESC NULLS LAST, occurred_at DESC, id DESC)
    WHERE embedded_at IS NULL AND last_failure IS NOT NULL;
