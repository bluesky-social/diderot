-- A session_ingest_candidates row is keyed on (owner, client, session_id),
-- but session_ingest_bindings -- the sole durable authority for a session's
-- current owner -- is keyed on session_id alone. Before this release, a
-- feed rebind (the same session_id reported under a new owner) moved the
-- binding but left the former owner's candidate row in place: its INSERT
-- ... ON CONFLICT targets the old composite key, which no longer matches
-- the new owner, so it created a second row instead of retiring the first.
-- That stale row let the former owner keep acting on the session through
-- the self-service privacy endpoint and the policy/scope reconcile passes.
--
-- The write paths now retire a candidate row themselves when a rebind
-- orphans it (internal/sessioningest/store.go's retireRebindStaleCandidatesTx),
-- so this is a one-time cleanup of whatever a prior release already left
-- behind: any candidate row whose owner_canonical disagrees with its
-- session's binding is gone, the same outcome the write path now keeps
-- current. A candidate with no binding row at all (there is nothing to
-- disagree with) is left untouched.
DELETE FROM session_ingest_candidates c
 WHERE EXISTS (
     SELECT 1 FROM session_ingest_bindings b
      WHERE b.session_id = c.session_id
        AND b.owner_canonical IS DISTINCT FROM c.owner_canonical
 );

-- A session the feed moved between owners before session_ingest_bindings
-- existed (migration 20261114000001, no backfill) can carry the same
-- disagreement with no binding row to catch it: two candidate rows for one
-- session_id, one per owner, and the DELETE above leaves both alone because
-- neither disagrees with a binding that does not exist. The former owner's
-- row still passes applySessionPrivacyTx's bindingless guard and the
-- reconcile passes' plain candidate scan, so it is the same cross-owner
-- hijack this release fixes, just for a session old enough to predate the
-- bindings table.
--
-- Keep only the feed's latest report for such a session_id -- the row with
-- the highest change_seq among the disagreeing ones -- and retire every
-- earlier, disagreeing-owner row's per-identity derived state the same way
-- retireRebindStaleCandidatesTx does for a live rebind: delete its
-- summaries, chunk summaries and map checkpoints, and mark its published
-- ledger rows superseded so nothing re-derives from them. A tie at the
-- highest change_seq is left alone (nothing to prefer between them); that
-- has not been observed and this is a one-time cleanup, not the ongoing
-- write-path guard.
WITH stale AS (
    DELETE FROM session_ingest_candidates c
     WHERE NOT EXISTS (
         SELECT 1 FROM session_ingest_bindings b WHERE b.session_id = c.session_id
     )
     AND EXISTS (
         SELECT 1 FROM session_ingest_candidates n
          WHERE n.session_id = c.session_id
            AND n.owner_canonical <> c.owner_canonical
            AND n.change_seq > c.change_seq
     )
    RETURNING owner, client, session_id
), del_summaries AS (
    DELETE FROM session_ingest_summaries s
     USING stale
     WHERE s.session_id = stale.session_id AND s.owner = stale.owner AND s.client = stale.client
    RETURNING 1
), del_chunk_summaries AS (
    DELETE FROM session_ingest_chunk_summaries s
     USING stale
     WHERE s.session_id = stale.session_id AND s.owner = stale.owner AND s.client = stale.client
    RETURNING 1
), del_map_checkpoints AS (
    DELETE FROM session_ingest_map_checkpoints m
     USING stale
     WHERE m.session_id = stale.session_id AND m.owner = stale.owner AND m.client = stale.client
    RETURNING 1
), superseded AS (
    UPDATE session_ingest_published p
       SET superseded_at = now()
      FROM stale
     WHERE p.session_id = stale.session_id AND p.owner = stale.owner AND p.client = stale.client
       AND p.superseded_at IS NULL AND p.tombstoned_at IS NULL
    RETURNING 1
)
SELECT count(*) FROM stale;
