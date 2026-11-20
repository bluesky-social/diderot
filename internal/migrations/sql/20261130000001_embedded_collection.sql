-- Which vector collection a chunk's vector was written to. Changing the
-- embedding model fills a new collection while search keeps using the old
-- one, and this is how the embedder knows which chunks the new collection
-- still lacks. NULL is a chunk embedded before this column existed: its
-- vector is in the collection that was in use then.
ALTER TABLE event_chunks ADD COLUMN embedded_collection text;
