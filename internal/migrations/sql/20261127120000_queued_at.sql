-- When each item of work entered its queue: when the row was created, and
-- again each time it is put back (an administrator's retry, a re-embed,
-- describing an image again).
--
-- The Status page said a queue had stalled by the age of the waiting item's
-- event, so a new image on a page indexed months ago counted as waiting for
-- months. It now measures the wait itself.
--
-- A row that exists already gets the time of this migration. For one that
-- is waiting that is a late start, which delays a stalled notice by at most
-- the wait the page allows; nothing else reads it.
ALTER TABLE event_chunks ADD COLUMN queued_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE event_images ADD COLUMN queued_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE event_documents ADD COLUMN queued_at timestamptz NOT NULL DEFAULT now();
