-- Slack threads where an Ask turn has told Slack it is working. Slack shows
-- "Working…" on such a thread until the app says the turn is done, or for an
-- hour, so a turn whose process dies must not leave that to the hour.
--
-- The turn's process inserts the row before it sets the status and renews
-- expires_at while the turn runs. It deletes the row once Slack confirms the
-- thread is no longer working. When it cannot confirm that, it marks the row
-- finished, if the turn said what it had to (its answer or error, or its
-- asker stopped it), and lets the row expire. The Slack leader clears every
-- expired row: it sets the thread's status back to ready and, for a turn
-- that never finished, tells the thread the answer was interrupted.
--
-- stop_requested is set when someone presses Slack's stop button on the
-- thread. The turn's process sees it when it next renews the row and stops
-- the turn, whichever replica runs it.
CREATE TABLE slack_working_threads (
    channel_id     text        NOT NULL,
    thread_ts      text        NOT NULL,
    holder         uuid        NOT NULL,
    expires_at     timestamptz NOT NULL,
    finished       boolean     NOT NULL DEFAULT false,
    stop_requested boolean     NOT NULL DEFAULT false,
    PRIMARY KEY (channel_id, thread_ts)
);
COMMENT ON TABLE slack_working_threads IS 'Slack threads an Ask turn has marked as working, so a turn that dies is cleared and a stop reaches it.';

-- A turn stopped by the person who asked is its own outcome: not an answer,
-- and not a failure.
ALTER TABLE ask_runs DROP CONSTRAINT ask_runs_outcome_known;
ALTER TABLE ask_runs ADD CONSTRAINT ask_runs_outcome_known
    CHECK (outcome IN ('answered', 'incomplete', 'error', 'stopped'));
