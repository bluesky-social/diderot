-- A picture stored inside a document (a screenshot pasted into a Word file)
-- is described like any other image of the event, and found by its caption.
-- This column says which document it came from, as event_chunks already says
-- for a document's text: re-reading an event's own attachments leaves the
-- picture alone, and removing the document removes it.
ALTER TABLE event_images
    ADD COLUMN event_document_id bigint REFERENCES event_documents (id) ON DELETE CASCADE;

CREATE INDEX event_images_by_document ON event_images (event_document_id)
    WHERE event_document_id IS NOT NULL;
