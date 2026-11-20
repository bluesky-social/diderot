-- Nothing ever wrote why a conversation ended: each question records its own
-- outcome and error in ask_runs, which is what the pages read.
ALTER TABLE ask_sessions DROP COLUMN failure_reason;
