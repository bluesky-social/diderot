-- The schema of a new Diderot install.
--
-- This file carries the version of the last migration of the history it
-- replaced. A database that applied that history has the version recorded, so
-- Atlas never runs this file there and goes on to the migrations after it.

-- A database that has a Diderot schema but not this version stopped partway
-- through the replaced history. Refuse it before anything below runs; the
-- whole file is one transaction, so nothing is left behind.
DO $$
BEGIN
    IF to_regclass('events') IS NOT NULL THEN
        RAISE EXCEPTION 'This database was migrated by a Diderot release older than v0.9.0. Deploy v0.9.0 first and let it start once, then deploy this release.';
    END IF;
END
$$;

-- ===========================================================================
-- Corpus
-- ===========================================================================
-- Every source writes its documents into one event log. An event is split
-- into chunks; the embedder fills each chunk's vector in Qdrant and stamps
-- embedded_at. Images and documents attached to an event are mirrored and
-- turned into chunks of their own.

-- One row per ingested document: a Slack message, an agent-session summary, a
-- Notion page, a Linear issue, a Zendesk ticket, or a Markdown file. Every
-- sink write upserts on (source, external_id).
CREATE TABLE events (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source            text NOT NULL,
    external_id       text NOT NULL,
    occurred_at       timestamptz NOT NULL,
    updated_at        timestamptz,
    deleted           boolean NOT NULL DEFAULT false,
    actor             text,
    actor_id          text,
    thread_id         text,
    title             text,
    body              text NOT NULL,
    url               text,
    raw               jsonb NOT NULL,
    session_id        text,
    embedding_text    text,
    actor_canonical   text NOT NULL DEFAULT '',
    image_count       integer NOT NULL DEFAULT 0,
    scope             text NOT NULL DEFAULT 'unstamped',
    content_hash      text GENERATED ALWAYS AS (md5(body)) STORED NOT NULL,
    ingested_at       timestamptz NOT NULL DEFAULT now(),
    eval_content_hash text GENERATED ALWAYS AS (md5(((((octet_length(COALESCE(title, '')))::text || ':') || COALESCE(title, '')) || body))) STORED NOT NULL,
    record_start      integer,
    record_end        integer,
    UNIQUE (source, external_id)
);
CREATE INDEX events_ingested_at ON events (ingested_at);
CREATE INDEX idx_events_actor ON events (source, actor_id)
    WHERE actor_id IS NOT NULL;
CREATE INDEX idx_events_actor_canonical ON events (actor_canonical)
    WHERE actor_canonical <> '';
CREATE INDEX idx_events_actor_canonical_occurred_at ON events (actor_canonical, occurred_at DESC)
    WHERE actor_canonical <> '';
CREATE INDEX idx_events_agent_session_external_id_prefix ON events USING spgist (external_id)
    WHERE (source = 'agent_session') AND (deleted = false);
CREATE INDEX idx_events_scope ON events (scope);
CREATE INDEX idx_events_session_id ON events (session_id, record_start)
    WHERE session_id IS NOT NULL;
CREATE INDEX idx_events_source_occurred_at ON events (source, occurred_at DESC);
CREATE INDEX idx_events_thread ON events (thread_id)
    WHERE thread_id IS NOT NULL;

-- Files attached to an event (PDF, DOCX, XLSX): the mirrored bytes and the
-- state of their text extraction.
CREATE TABLE event_documents (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id          bigint NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    source_file_id    text NOT NULL,
    occurred_at       timestamptz NOT NULL,
    filename          text NOT NULL,
    media_type        text NOT NULL,
    doc_format        text,
    storage_key       text,
    bytes             bigint,
    sha256            bytea,
    extract_model     text,
    extracted_at      timestamptz,
    page_count        integer,
    pages_extracted   integer,
    extract_cost_usd  numeric(12,6),
    rejected_reason   text,
    incomplete_reason text,
    claimed_at        timestamptz,
    failure_count     integer NOT NULL DEFAULT 0,
    last_failure      text,
    UNIQUE (event_id, source_file_id),
    CHECK (extract_cost_usd >= 0),
    CHECK (incomplete_reason IN ('cost_limit', 'output_truncated', 'missing_pages')),
    CHECK (page_count >= 0),
    CHECK (pages_extracted >= 0)
);
CREATE INDEX event_documents_by_source_file_id ON event_documents (source_file_id);
CREATE INDEX event_documents_incomplete ON event_documents (incomplete_reason)
    WHERE incomplete_reason IS NOT NULL;
CREATE INDEX event_documents_pending ON event_documents (occurred_at DESC, id)
    WHERE (extracted_at IS NULL) AND (failure_count < 5);

-- One extracted page of a document, saved before the next paid page is
-- requested so a crash resumes where it stopped.
CREATE TABLE event_document_extract_parts (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_document_id bigint NOT NULL REFERENCES event_documents(id) ON DELETE CASCADE,
    source_sha256     bytea NOT NULL,
    extract_model     text NOT NULL,
    page_number       integer NOT NULL,
    markdown          text NOT NULL,
    cost_usd          numeric(12,6) NOT NULL,
    truncated         boolean NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT event_document_extract_parts_event_document_id_source_sha25_key UNIQUE (event_document_id, source_sha256, extract_model, page_number),
    CHECK (cost_usd >= 0),
    CHECK (page_number > 0)
);

-- Vector points whose chunk was deleted, waiting for the embedder to remove
-- them from Qdrant.
CREATE TABLE qdrant_delete_queue (
    point_id        bigint PRIMARY KEY,
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text
);
CREATE INDEX idx_qdrant_delete_queue_ready ON qdrant_delete_queue (next_attempt_at, enqueued_at);

-- Queues a deleted chunk's vector point for removal from Qdrant.
CREATE FUNCTION enqueue_qdrant_point_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO qdrant_delete_queue (point_id)
    VALUES (OLD.id)
    ON CONFLICT (point_id) DO NOTHING;
    RETURN OLD;
END;
$$;

-- The retrievable pieces of an event. A chunk carries the event's scope and
-- occurrence time so retrieval can filter without a join. Deleting a chunk
-- queues its vector point for removal.
CREATE TABLE event_chunks (
    id                         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id                   bigint NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    chunk_idx                  integer NOT NULL,
    content                    text NOT NULL,
    embedded_at                timestamptz,
    embedder_id                text,
    failure_count              integer NOT NULL DEFAULT 0,
    last_failure               text,
    context_prefix             text,
    kind                       text NOT NULL DEFAULT 'text',
    event_document_id          bigint REFERENCES event_documents(id) ON DELETE CASCADE,
    occurred_at                timestamptz NOT NULL,
    scope_sync_needed          boolean NOT NULL DEFAULT false,
    scope_sync_attempts        integer NOT NULL DEFAULT 0,
    scope_sync_next_attempt_at timestamptz NOT NULL DEFAULT now(),
    scope_sync_last_error      text,
    CONSTRAINT event_chunks_document_id_iff_document_kind CHECK ((kind = 'document_text') = (event_document_id IS NOT NULL)),
    CONSTRAINT event_chunks_kind_valid CHECK (kind IN ('text', 'image_caption', 'document_text'))
);
CREATE UNIQUE INDEX event_chunks_document_chunk_idx ON event_chunks (event_document_id, chunk_idx)
    WHERE event_document_id IS NOT NULL;
CREATE INDEX event_chunks_embedded_at ON event_chunks (embedded_at)
    WHERE embedded_at IS NOT NULL;
CREATE UNIQUE INDEX event_chunks_event_id_kind_chunk_idx ON event_chunks (event_id, kind, chunk_idx)
    WHERE kind <> 'document_text';
CREATE INDEX idx_chunks_unembedded ON event_chunks (occurred_at DESC, id DESC)
    WHERE (embedded_at IS NULL) AND (failure_count < 5);
CREATE INDEX idx_event_chunks_event_id ON event_chunks (event_id);
CREATE INDEX idx_event_chunks_scope_sync_pending ON event_chunks (scope_sync_next_attempt_at, id)
    WHERE scope_sync_needed AND (embedded_at IS NOT NULL);
CREATE TRIGGER event_chunks_enqueue_qdrant_delete AFTER DELETE ON event_chunks
    FOR EACH ROW EXECUTE FUNCTION enqueue_qdrant_point_delete();

-- Images attached to an event: the mirrored bytes and the caption the vision
-- model wrote, which becomes a chunk.
CREATE TABLE event_images (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id        bigint NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    source_image_id text NOT NULL,
    storage_key     text,
    media_type      text NOT NULL,
    bytes           bigint,
    sha256          bytea,
    caption_model   text,
    captioned_at    timestamptz,
    unread_flagged  boolean NOT NULL DEFAULT false,
    truncated_at    timestamptz,
    chunk_id        bigint REFERENCES event_chunks(id) ON DELETE SET NULL,
    failure_count   integer NOT NULL DEFAULT 0,
    last_failure    text,
    claimed_at      timestamptz,
    occurred_at     timestamptz NOT NULL,
    UNIQUE (event_id, source_image_id),
    CONSTRAINT event_images_media_type_is_image CHECK (lower(media_type) LIKE 'image/%')
);
CREATE INDEX event_images_by_source_image_id ON event_images (source_image_id);
CREATE INDEX event_images_escalation_candidates ON event_images (event_id)
    WHERE (caption_model IS DISTINCT FROM 'opus-4.7') AND (truncated_at IS NULL);
CREATE INDEX event_images_uncaptioned ON event_images (occurred_at DESC, id)
    WHERE (captioned_at IS NULL) AND (failure_count < 5);

-- One row per retrieve or list call, with its results, for evaluation.
CREATE TABLE retrieve_log (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    called_at    timestamptz NOT NULL DEFAULT now(),
    endpoint     text NOT NULL,
    question     text,
    filters      jsonb NOT NULL DEFAULT '{}'::jsonb,
    results      jsonb NOT NULL,
    latency_ms   integer NOT NULL,
    caller_sub   text NOT NULL,
    caller_email text,
    CHECK (endpoint IN ('retrieve', 'list'))
);
CREATE INDEX idx_retrieve_log_called_at ON retrieve_log (called_at DESC);

-- ===========================================================================
-- Connectors
-- ===========================================================================
-- Connector configuration administrators edit in the browser, and the durable
-- state sources keep for themselves.

-- One row per connector, plus the `org` row that holds the organization's
-- name and the install's setup state. version is the optimistic-concurrency
-- token and the change signal replicas poll.
CREATE TABLE connector_settings (
    connector  text PRIMARY KEY,
    enabled    boolean NOT NULL DEFAULT false,
    settings   jsonb NOT NULL,
    secrets    jsonb NOT NULL,
    version    bigint NOT NULL,
    last_test  jsonb,
    updated_by text NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK (btrim(connector) <> ''),
    CHECK (btrim(updated_by) <> ''),
    CHECK (version > 0)
);
-- A new install starts in setup, with the bootstrap administrator on.
INSERT INTO connector_settings (connector, settings, secrets, version, updated_by, updated_at)
VALUES ('org', '{"state": "setup", "ORG_NAME": "", "ORG_DESCRIPTION": "", "bootstrap_admin_enabled": true}', '{}', 1, 'diderot:migration', now());

-- A source's own durable state, such as a sync cursor, a pending request, or
-- the account its saved credentials belong to: one JSON value per key. source
-- is the connector name.
CREATE TABLE source_state (
    source     text NOT NULL,
    key        text NOT NULL,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, key),
    CHECK (btrim(key) <> ''),
    CHECK (btrim(source) <> '')
);

-- ===========================================================================
-- Slack
-- ===========================================================================

-- Slack-specific fields of a Slack event.
CREATE TABLE slack_message_details (
    event_id    bigint PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
    channel_id  text NOT NULL,
    user_id     text,
    thread_ts   text,
    is_edit     boolean NOT NULL DEFAULT false,
    has_files   boolean NOT NULL DEFAULT false,
    occurred_at timestamptz NOT NULL
);
CREATE INDEX idx_slack_message_details_channel ON slack_message_details (channel_id);
CREATE INDEX idx_slack_message_details_channel_event_id ON slack_message_details (channel_id, event_id);
CREATE INDEX idx_slack_message_details_channel_occurred_at ON slack_message_details (channel_id, occurred_at DESC, event_id DESC);
CREATE INDEX idx_slack_message_details_channel_thread ON slack_message_details (channel_id, thread_ts)
    WHERE thread_ts IS NOT NULL;

-- The workspace's users, refreshed from users.list; resolves user IDs to
-- names and emails.
CREATE TABLE slack_users (
    user_id             text PRIMARY KEY,
    name                text,
    display_name        text,
    real_name           text,
    email               text,
    is_bot              boolean NOT NULL DEFAULT false,
    fetched_at          timestamptz NOT NULL DEFAULT now(),
    team_id             text,
    is_stranger         boolean NOT NULL DEFAULT false,
    is_restricted       boolean NOT NULL DEFAULT false,
    is_ultra_restricted boolean NOT NULL DEFAULT false,
    tz                  text,
    tz_label            text,
    tz_offset           integer
);

-- The workspace's user groups, for resolving group mentions.
CREATE TABLE slack_usergroups (
    usergroup_id text PRIMARY KEY,
    name         text,
    handle       text,
    fetched_at   timestamptz NOT NULL DEFAULT now()
);

-- Bot identities, for resolving bot authors to a name and app.
CREATE TABLE slack_bots (
    bot_id     text PRIMARY KEY,
    name       text,
    app_id     text,
    user_id    text,
    deleted    boolean NOT NULL DEFAULT false,
    fetched_at timestamptz NOT NULL DEFAULT now()
);

-- Tells replicas that a channel's name or visibility changed.
CREATE FUNCTION notify_slack_public_scopes() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP <> 'UPDATE' THEN
        PERFORM pg_notify('diderot_slack_public_scopes', '');
    ELSIF ROW(OLD.channel_id, OLD.name, OLD.is_private)
          IS DISTINCT FROM ROW(NEW.channel_id, NEW.name, NEW.is_private) THEN
        PERFORM pg_notify('diderot_slack_public_scopes', '');
    END IF;
    RETURN NULL;
END;
$$;

-- Channel names and visibility, refreshed from the Slack API. A change
-- notifies replicas to recompute which scopes are public.
CREATE TABLE slack_channel_info (
    channel_id            text PRIMARY KEY,
    name                  text,
    is_private            boolean NOT NULL DEFAULT false,
    is_archived           boolean NOT NULL DEFAULT false,
    fetched_at            timestamptz NOT NULL DEFAULT now(),
    is_ext_shared         boolean,
    is_pending_ext_shared boolean
);
CREATE INDEX idx_slack_channel_info_name ON slack_channel_info (name)
    WHERE name <> '';
CREATE TRIGGER slack_public_scopes_changed AFTER INSERT OR DELETE OR UPDATE ON slack_channel_info
    FOR EACH ROW EXECUTE FUNCTION notify_slack_public_scopes();

-- Per-channel operator settings: whether Ask answers there, whether external
-- members may ask, and attachment ingest.
CREATE TABLE slack_channel_config (
    channel_id                  text PRIMARY KEY,
    allow_external_ask          boolean NOT NULL DEFAULT false,
    has_external_members        boolean,
    external_members_checked_at timestamptz,
    updated_by                  text,
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    ask_enabled                 boolean NOT NULL DEFAULT false,
    external_members            text[] NOT NULL DEFAULT '{}'::text[],
    doc_ingest_enabled          boolean NOT NULL DEFAULT false,
    doc_backfill_cursor         bigint,
    CHECK ((doc_backfill_cursor IS NULL) OR (doc_backfill_cursor >= 0))
);
COMMENT ON COLUMN slack_channel_config.ask_enabled IS 'When false (default), maybeRouteAsk silently drops @-mentions in this channel. Operator opts a channel in via `diderot slack channels config allow-ask <id>`.';
CREATE INDEX idx_slack_channel_config_external ON slack_channel_config (allow_external_ask, has_external_members);

-- One row per channel the ingest worker sweeps, with the state of the sweep
-- in flight.
CREATE TABLE slack_channel_sync (
    channel_id         text PRIMARY KEY,
    last_synced_at     timestamptz,
    cursor             text,
    in_progress_at     timestamptz,
    worker_token       uuid,
    last_error         text,
    ingest_enabled     boolean NOT NULL DEFAULT true,
    oldest_ts          double precision,
    last_deep_sweep_at timestamptz
);
COMMENT ON TABLE slack_channel_sync IS 'Continuous-worker state for slack ingest. One row per known channel. last_synced_at NULL = never swept (highest priority). cursor + in_progress_at + worker_token form the in-flight sweep state; worker uses worker_token guard on every state-mutating UPDATE to detect stolen claims. ingest_enabled is the per-row effective gate; slack_ingest_blacklist is the cross-cutting policy table.';
COMMENT ON COLUMN slack_channel_sync.last_synced_at IS 'Wall clock at successful completion of the most recent sweep, NULL = never swept. Live socket-mode events priority-bump this via LEAST(last_synced_at, now() - overlap).';
COMMENT ON COLUMN slack_channel_sync.cursor IS 'Saved pagination cursor mid-sweep. NULL when idle. Persisted every page so restarts resume cleanly.';
COMMENT ON COLUMN slack_channel_sync.in_progress_at IS 'Wall clock at start (or last heartbeat) of the in-flight sweep. NULL when idle. Heartbeats every page; stale-recovery threshold is sized to "longest plausible single page," not "longest plausible total sweep."';
COMMENT ON COLUMN slack_channel_sync.worker_token IS 'Random uuid assigned at claim time. Every state-mutating UPDATE in the worker carries a worker_token = $token guard.';
COMMENT ON COLUMN slack_channel_sync.ingest_enabled IS 'Per-row effective gate. Worker pick query filters this out. Distinct from slack_ingest_blacklist (cross-cutting policy table); reconcile reconciles the two.';
COMMENT ON COLUMN slack_channel_sync.oldest_ts IS 'Unix-seconds float of the originating oldest_ts bound for the in-flight sweep, set alongside cursor every page. NULL when no sweep is mid-flight or the sweep was started without an oldest bound (full re-fetch). Preserves Slack history semantics across crash + resume.';
COMMENT ON COLUMN slack_channel_sync.last_deep_sweep_at IS 'Wall clock at successful completion of the most recent deep sweep (oldest floor at now-7d instead of the normal overlap). NULL = never deep-swept. When NULL or older than the deep-sweep cadence (24h), the next claim runs that channel as a deep sweep so old thread roots with new replies re-surface for the gated thread-root harvest.';
CREATE INDEX slack_channel_sync_by_age ON slack_channel_sync (COALESCE(last_synced_at, '-infinity'::timestamptz), in_progress_at) INCLUDE (channel_id)
    WHERE ingest_enabled;

-- Threads whose replies are waiting to be fetched.
CREATE TABLE slack_thread_sync (
    channel_id          text NOT NULL,
    thread_ts           text NOT NULL,
    enqueued_at         timestamptz NOT NULL DEFAULT now(),
    in_progress_at      timestamptz,
    worker_token        uuid,
    cursor              text,
    last_error          text,
    done_at             timestamptz,
    latest_reply        text,
    walked_latest_reply text,
    activity_ts         text GENERATED ALWAYS AS (COALESCE(latest_reply, thread_ts)) STORED NOT NULL,
    PRIMARY KEY (channel_id, thread_ts)
);
COMMENT ON TABLE slack_thread_sync IS 'Pass-2 (conversations.replies) queue. Pass-1 history sweep upserts thread roots here on discovery; a sibling worker drains using the same global limiter. Re-discovery upserts re-open completed rows (new replies since last walk) and invalidate any in-flight worker_token to prevent the dead walker from erasing the re-walk request.';
COMMENT ON COLUMN slack_thread_sync.worker_token IS 'Random uuid assigned at claim time. The discovery upsert NULLs this on re-open to invalidate any in-flight worker — without that, a worker mid-walk would still satisfy markDone''s worker_token guard and write done_at=now(), erasing the re-walk request and losing the new replies forever.';
COMMENT ON COLUMN slack_thread_sync.done_at IS 'NULL while not-yet-walked or actively re-opened. Set to now() at successful walk completion. The discovery upsert resets this to NULL on re-discovery (parent showed up with new replies).';
COMMENT ON COLUMN slack_thread_sync.latest_reply IS 'Pending value: parent latest_reply (Slack ts) observed by the sweep that enqueued/re-opened this row. text for lossless lexicographic comparison (Slack ts is fixed-width 10.6 until year 2286). Set on insert + re-open; left alone while a walk is in flight.';
COMMENT ON COLUMN slack_thread_sync.walked_latest_reply IS 'Confirmed value: promoted from latest_reply at markDone. The enqueue gate re-opens a completed row only when the freshly-observed parent latest_reply > walked_latest_reply. Conservative — promoted from the enqueue-time pending value, so a mid-walk reply triggers one redundant (NoChange) re-walk rather than being lost.';
COMMENT ON COLUMN slack_thread_sync.activity_ts IS 'Generated thread activity recency (Slack ts, fixed-width text so lexicographic==numeric). COALESCE(latest_reply, thread_ts) is the later of the root and Slack''s reported latest reply. Pure source data, never a processing timestamp.';
CREATE INDEX slack_thread_sync_pending ON slack_thread_sync (activity_ts DESC, thread_ts DESC, channel_id, in_progress_at)
    WHERE done_at IS NULL;

-- How far each `diderot slack tail` session has read in a channel.
CREATE TABLE slack_tail_cursors (
    session         text NOT NULL,
    channel_id      text NOT NULL,
    cursor_event_id bigint NOT NULL,
    last_used_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session, channel_id)
);

-- Channels an operator excluded from ingest, whatever the bot's membership.
CREATE TABLE slack_ingest_blacklist (
    id           serial PRIMARY KEY,
    channel_id   text NOT NULL,
    channel_name text,
    reason       text NOT NULL DEFAULT '',
    added_by     text,
    added_at     timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE slack_ingest_blacklist IS 'Explicit do-not-ingest override list. Bot membership normally drives ingest; this is the small exceptions list.';
COMMENT ON COLUMN slack_ingest_blacklist.channel_id IS 'Stable Slack channel identity used for all blacklist matching.';
COMMENT ON COLUMN slack_ingest_blacklist.channel_name IS 'Informational channel name captured when the entry was added; renames do not change identity.';
CREATE UNIQUE INDEX slack_ingest_blacklist_by_channel_id ON slack_ingest_blacklist (channel_id);

-- ===========================================================================
-- Notion
-- ===========================================================================

-- The Notion worker's single state row: loop watermarks and the replay lease.
CREATE TABLE notion_sync (
    id                    integer DEFAULT 1 PRIMARY KEY,
    last_search_at        timestamptz,
    last_reconcile_at     timestamptz,
    search_runs_total     bigint NOT NULL DEFAULT 0,
    reconcile_runs_total  bigint NOT NULL DEFAULT 0,
    backfill_requested_at timestamptz,
    backfill_cursor       text,
    last_discovery_at     timestamptz,
    discovery_runs_total  bigint NOT NULL DEFAULT 0,
    integration_id        text,
    CHECK (id = 1)
);
COMMENT ON TABLE notion_sync IS 'Singleton state row for the notion ingest worker. last_search_at and last_reconcile_at advance per loop tick; backfill_started_at is a 10-minute lease taken by the CLI to pause loops during a backfill run.';
COMMENT ON COLUMN notion_sync.backfill_requested_at IS 'Non-null while the leader worker is consuming durable Notion search pages for an operator replay.';
COMMENT ON COLUMN notion_sync.last_discovery_at IS 'Best-effort data-source Search watermark; never an ingestion completeness boundary.';
COMMENT ON COLUMN notion_sync.integration_id IS 'Notion /users/me bot ID for the credential that established last_discovery_at.';
INSERT INTO notion_sync (id) VALUES (1);

-- One row per Notion sync run, with its counts and outcome.
CREATE TABLE notion_sync_runs (
    run_id          text PRIMARY KEY,
    run_type        text NOT NULL,
    started_at      timestamptz NOT NULL,
    completed_at    timestamptz,
    status          text NOT NULL,
    attempted       bigint NOT NULL DEFAULT 0,
    created_count   bigint NOT NULL DEFAULT 0,
    updated_count   bigint NOT NULL DEFAULT 0,
    unchanged_count bigint NOT NULL DEFAULT 0,
    hidden_count    bigint NOT NULL DEFAULT 0,
    failed_count    bigint NOT NULL DEFAULT 0,
    error           text NOT NULL DEFAULT '',
    CHECK (run_type IN ('reconcile', 'discovery', 'backfill')),
    CHECK (status IN ('running', 'complete', 'partial', 'failed'))
);

-- The pages and data sources whose trees Diderot ingests.
CREATE TABLE notion_roots (
    object_type      text NOT NULL DEFAULT 'auto',
    object_id        text NOT NULL,
    label            text NOT NULL DEFAULT '',
    added_at         timestamptz NOT NULL DEFAULT now(),
    added_by         text NOT NULL DEFAULT '',
    last_walked_at   timestamptz,
    last_walk_status text NOT NULL DEFAULT 'never',
    last_walk_error  text NOT NULL DEFAULT '',
    PRIMARY KEY (object_type, object_id),
    CHECK (object_type IN ('auto', 'page', 'database', 'data_source', 'view'))
);
COMMENT ON TABLE notion_roots IS 'Authoritative Notion corpus roots. Resource type is explicit when known; migrated bare UUIDs remain auto-probed.';

-- The last-seen state of each Notion page under a root: its remote edit time
-- and whether Diderot can still read it.
CREATE TABLE notion_page_state (
    page_id               text PRIMARY KEY,
    remote_last_edited_at timestamptz,
    last_seen_at          timestamptz NOT NULL,
    last_seen_run_id      text REFERENCES notion_sync_runs(run_id),
    access_state          text NOT NULL DEFAULT 'active',
    last_error            text NOT NULL DEFAULT '',
    source_metadata       jsonb NOT NULL DEFAULT '{}'::jsonb,
    CHECK (access_state IN ('active', 'inaccessible', 'trashed', 'removed', 'empty'))
);

-- Which roots reach each page; a page leaves the corpus when no root reaches
-- it.
CREATE TABLE notion_page_roots (
    page_id          text NOT NULL REFERENCES notion_page_state(page_id) ON DELETE CASCADE,
    root_object_type text NOT NULL,
    root_object_id   text NOT NULL,
    last_seen_run_id text NOT NULL REFERENCES notion_sync_runs(run_id),
    PRIMARY KEY (page_id, root_object_type, root_object_id),
    FOREIGN KEY (root_object_type, root_object_id) REFERENCES notion_roots(object_type, object_id) ON DELETE CASCADE
);
CREATE INDEX notion_page_roots_by_root ON notion_page_roots (root_object_type, root_object_id, last_seen_run_id);

-- Pages and data sources Notion search returned, for auditing coverage.
CREATE TABLE notion_discovered_resources (
    object_type      text NOT NULL,
    object_id        text NOT NULL,
    parent_id        text NOT NULL DEFAULT '',
    title            text NOT NULL DEFAULT '',
    url              text NOT NULL DEFAULT '',
    first_seen_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at     timestamptz NOT NULL DEFAULT now(),
    last_seen_run_id text REFERENCES notion_sync_runs(run_id),
    PRIMARY KEY (object_type, object_id),
    CHECK (object_type IN ('database', 'data_source', 'view'))
);
COMMENT ON TABLE notion_discovered_resources IS 'Best-effort Notion Search discoveries for coverage auditing only; rows are never ingested without a managed-root path.';

-- ===========================================================================
-- Linear
-- ===========================================================================

-- The Linear worker's single state row: watermarks, visibility sweeps, and
-- the replay lease.
CREATE TABLE linear_sync (
    id                    integer DEFAULT 1 PRIMARY KEY,
    last_sync_at          timestamptz,
    last_reconcile_at     timestamptz,
    visibility_swept_at   timestamptz,
    sync_runs_total       bigint NOT NULL DEFAULT 0,
    reconcile_runs_total  bigint NOT NULL DEFAULT 0,
    visibility_runs_total bigint NOT NULL DEFAULT 0,
    backfill_cursor       text,
    backfill_requested_at timestamptz,
    backfill_project      text,
    backfill_project_id   text,
    CHECK (id = 1)
);
COMMENT ON TABLE linear_sync IS 'Singleton state row for the linear ingest worker. Watermarks, visibility sweep timestamps, and backfill lease.';
COMMENT ON COLUMN linear_sync.backfill_requested_at IS 'Non-null while the leader worker is consuming durable Linear issue pages for an operator replay.';
INSERT INTO linear_sync (id) VALUES (1);

-- ===========================================================================
-- Zendesk
-- ===========================================================================

-- The incremental-export cursor and watermark per tenant and stream.
CREATE TABLE zendesk_sync (
    tenant     text NOT NULL,
    stream     text NOT NULL,
    cursor     text NOT NULL DEFAULT '',
    watermark  bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, stream)
);

-- Tickets and articles whose fetch failed, retried after not_before.
CREATE TABLE zendesk_retry (
    tenant     text NOT NULL,
    kind       text NOT NULL,
    source_id  bigint NOT NULL,
    attempts   integer NOT NULL DEFAULT 0,
    not_before timestamptz NOT NULL DEFAULT (now() + '00:15:00'::interval),
    PRIMARY KEY (tenant, kind, source_id),
    CHECK (kind IN ('ticket', 'article')),
    CHECK (source_id > 0)
);

-- Attachments of Zendesk events, fetched by the attachment workers.
CREATE TABLE zendesk_files (
    source_file_id text PRIMARY KEY,
    event_id       bigint NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    content_url    text NOT NULL,
    media_type     text NOT NULL
);
CREATE INDEX zendesk_files_event ON zendesk_files (event_id);

-- ===========================================================================
-- Prompts
-- ===========================================================================
-- Prompt bundles are released from the prompt catalog. A release and its
-- entries are immutable, and activation moves one pointer, so related prompts
-- never drift apart.

-- Prompt releases are append-only.
CREATE FUNCTION reject_prompt_release_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'prompt release history is immutable';
END;
$$;

-- One immutable prompt bundle.
CREATE TABLE prompt_releases (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_revision text NOT NULL UNIQUE,
    bundle_sha256   text NOT NULL,
    schema_version  integer NOT NULL,
    created_by      text NOT NULL,
    note            text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (bundle_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK (btrim(created_by) <> ''),
    CHECK (btrim(source_revision) <> '')
);
CREATE INDEX prompt_releases_bundle_sha256 ON prompt_releases (bundle_sha256);
CREATE TRIGGER prompt_releases_immutable BEFORE DELETE OR UPDATE ON prompt_releases
    FOR EACH ROW EXECUTE FUNCTION reject_prompt_release_mutation();

-- The prompts of one release.
CREATE TABLE prompt_release_entries (
    release_id  bigint NOT NULL REFERENCES prompt_releases(id) ON DELETE RESTRICT,
    prompt_key  text NOT NULL,
    source_path text NOT NULL,
    description text NOT NULL DEFAULT '',
    body        text NOT NULL,
    body_sha256 text NOT NULL,
    PRIMARY KEY (release_id, prompt_key),
    CHECK (btrim(body) <> ''),
    CHECK (body_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK (btrim(prompt_key) <> ''),
    CHECK (btrim(source_path) <> '')
);
CREATE TRIGGER prompt_release_entries_immutable BEFORE DELETE OR UPDATE ON prompt_release_entries
    FOR EACH ROW EXECUTE FUNCTION reject_prompt_release_mutation();

-- The single active release.
CREATE TABLE prompt_active_release (
    singleton    boolean DEFAULT true PRIMARY KEY,
    release_id   bigint NOT NULL REFERENCES prompt_releases(id) ON DELETE RESTRICT,
    activated_by text NOT NULL,
    activated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (btrim(activated_by) <> ''),
    CHECK (singleton)
);

-- Every change of the active release.
CREATE TABLE prompt_release_activations (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    from_release_id bigint REFERENCES prompt_releases(id) ON DELETE RESTRICT,
    to_release_id   bigint NOT NULL REFERENCES prompt_releases(id) ON DELETE RESTRICT,
    activated_by    text NOT NULL,
    activated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (btrim(activated_by) <> '')
);
CREATE TRIGGER prompt_release_activations_immutable BEFORE DELETE OR UPDATE ON prompt_release_activations
    FOR EACH ROW EXECUTE FUNCTION reject_prompt_release_mutation();

-- The archived Ask prompt history is append-only.
CREATE FUNCTION reject_legacy_ask_prompt_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'legacy Ask prompt history is immutable';
END;
$$;

-- Immutable Ask prompt versions from before catalog releases, kept as
-- provenance for the sessions that used them.
CREATE TABLE legacy_ask_prompts (
    id              bigint PRIMARY KEY,
    body            text NOT NULL,
    created_by      text,
    note            text,
    created_at      timestamptz NOT NULL,
    recovery_source text NOT NULL,
    recovered_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (btrim(body) <> ''),
    CHECK (id > 0),
    CHECK (btrim(recovery_source) <> '')
);
COMMENT ON TABLE legacy_ask_prompts IS 'Recovered immutable Ask-only prompt versions used before atomic Git-authored catalog releases.';
CREATE TRIGGER legacy_ask_prompts_immutable BEFORE DELETE OR UPDATE ON legacy_ask_prompts
    FOR EACH ROW EXECUTE FUNCTION reject_legacy_ask_prompt_mutation();

-- ===========================================================================
-- Agent sessions
-- ===========================================================================
-- Summaries of coding-agent sessions, from a session feed or from HTTP push.
-- Every writer binds a session to an owner, a writer kind, and a format; the
-- summarization worker turns the current revision of each candidate into
-- events.

-- The feed's change cursor. policy_hash fingerprints the include/exclude
-- policy; a change resets the cursor so the feed is re-read under the new
-- policy.
CREATE TABLE session_ingest_cursor (
    id          boolean DEFAULT true PRIMARY KEY,
    change_seq  bigint NOT NULL DEFAULT 0,
    policy_hash text NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT session_ingest_cursor_single CHECK (id)
);
INSERT INTO session_ingest_cursor (id) VALUES (true);

-- The operator's switch for paid summarization. It starts paused; the
-- deployment setting can only allow it, never override a pause.
CREATE TABLE session_ingest_control (
    id                    boolean DEFAULT true PRIMARY KEY,
    summarization_enabled boolean NOT NULL DEFAULT false,
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (id)
);
INSERT INTO session_ingest_control (id, summarization_enabled) VALUES (true, false);

-- Binds each session_id to its owner, writer (feed or push), and format on
-- first write; later writes must match.
CREATE TABLE session_ingest_bindings (
    session_id      text PRIMARY KEY,
    owner_canonical text NOT NULL,
    writer          text NOT NULL,
    format          text NOT NULL,
    head_revision   text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((writer = 'push') OR (head_revision IS NULL)),
    CHECK (writer IN ('feed', 'push'))
);

-- The latest known revision of each session and its indexing state.
CREATE TABLE session_ingest_candidates (
    owner                       text NOT NULL,
    client                      text NOT NULL,
    session_id                  text NOT NULL,
    owner_kind                  text NOT NULL DEFAULT '',
    role                        text NOT NULL DEFAULT '',
    origin_app                  text NOT NULL DEFAULT '',
    revision                    text NOT NULL DEFAULT '',
    rewrite_epoch               bigint NOT NULL DEFAULT 0,
    change_seq                  bigint NOT NULL DEFAULT 0,
    record_count                integer NOT NULL DEFAULT 0,
    user_turn_count             integer NOT NULL DEFAULT 0,
    content_bytes               bigint NOT NULL DEFAULT 0,
    updated_at                  timestamptz,
    complete                    boolean NOT NULL DEFAULT false,
    deleted                     boolean NOT NULL DEFAULT false,
    published_revision          text NOT NULL DEFAULT '',
    published_rewrite_epoch     bigint NOT NULL DEFAULT 0,
    published_record_count      integer NOT NULL DEFAULT 0,
    published_user_turn_count   integer NOT NULL DEFAULT 0,
    published_content_bytes     bigint NOT NULL DEFAULT 0,
    published_at                timestamptz,
    first_pending_at            timestamptz,
    eligible_at                 timestamptz,
    excluded_at                 timestamptz,
    corpus_tombstone_pending    timestamptz,
    updated_in_db               timestamptz NOT NULL DEFAULT now(),
    source_started_at           timestamptz,
    source_updated_at           timestamptz,
    published_source_started_at timestamptz,
    published_source_updated_at timestamptz,
    visibility_scope            text NOT NULL DEFAULT 'internal',
    owner_canonical             text NOT NULL,
    privacy_owner_only          boolean,
    index_failure_revision      text NOT NULL DEFAULT '',
    index_failure_count         integer NOT NULL DEFAULT 0,
    index_last_error            text,
    index_last_failed_at        timestamptz,
    index_retry_at              timestamptz,
    index_quarantined_at        timestamptz,
    PRIMARY KEY (owner, client, session_id),
    CONSTRAINT session_ingest_candidates_index_failure_count_nonnegative CHECK (index_failure_count >= 0),
    CONSTRAINT session_ingest_candidates_published_source_time_pair CHECK (((published_source_started_at IS NULL) AND (published_source_updated_at IS NULL)) OR ((published_source_started_at IS NOT NULL) AND (published_source_updated_at IS NOT NULL) AND (published_source_updated_at >= published_source_started_at))),
    CONSTRAINT session_ingest_candidates_source_time_pair CHECK (((source_started_at IS NULL) AND (source_updated_at IS NULL)) OR ((source_started_at IS NOT NULL) AND (source_updated_at IS NOT NULL) AND (source_updated_at >= source_started_at))),
    CONSTRAINT session_ingest_candidates_visibility_scope CHECK ((visibility_scope = 'internal') OR (starts_with(visibility_scope, 'owner:') AND (length(visibility_scope) > length('owner:'))))
);
COMMENT ON COLUMN session_ingest_candidates.visibility_scope IS 'Durable corpus audience: internal, or owner:<canonical owner> for owner-only agent sessions.';
COMMENT ON COLUMN session_ingest_candidates.owner_canonical IS 'Canonical owner identity used only for verified self-service owner joins.';
COMMENT ON COLUMN session_ingest_candidates.privacy_owner_only IS 'Explicit owner self-service override for this session; NULL uses deployment policy.';
COMMENT ON COLUMN session_ingest_candidates.index_failure_revision IS 'Revision the index_* failure state describes; ignored once the observed head moves past it.';
COMMENT ON COLUMN session_ingest_candidates.index_failure_count IS 'Consecutive failed index attempts for index_failure_revision.';
COMMENT ON COLUMN session_ingest_candidates.index_last_error IS 'Bounded diagnostic from the latest failed index attempt.';
COMMENT ON COLUMN session_ingest_candidates.index_retry_at IS 'Earliest time a transiently failed revision is selectable again (capped exponential backoff).';
COMMENT ON COLUMN session_ingest_candidates.index_quarantined_at IS 'Set only for a permanent failure of an immutable revision (e.g. over the byte cap); transient failures never quarantine. Cleared manually or by a new revision.';
CREATE INDEX session_ingest_candidates_identity ON session_ingest_candidates (owner, client, session_id);
CREATE INDEX session_ingest_candidates_owner_canonical_recent ON session_ingest_candidates (owner_canonical, change_seq DESC);
CREATE INDEX session_ingest_candidates_owner_only_scope ON session_ingest_candidates (owner, client, session_id, visibility_scope)
    WHERE (visibility_scope <> 'internal') AND (deleted = false) AND (excluded_at IS NULL) AND (published_revision <> '');
CREATE INDEX session_ingest_candidates_pending ON session_ingest_candidates (first_pending_at)
    WHERE (excluded_at IS NULL) AND (deleted = false) AND (complete = true);
CREATE INDEX session_ingest_candidates_session_id ON session_ingest_candidates (session_id);

-- The revision of each session currently published to the corpus.
CREATE TABLE session_ingest_published (
    owner             text NOT NULL,
    client            text NOT NULL,
    session_id        text NOT NULL,
    revision          text NOT NULL,
    rewrite_epoch     bigint NOT NULL DEFAULT 0,
    content_digest    text NOT NULL DEFAULT '',
    published_at      timestamptz NOT NULL DEFAULT now(),
    superseded_at     timestamptz,
    tombstoned_at     timestamptz,
    source_started_at timestamptz,
    source_updated_at timestamptz,
    PRIMARY KEY (owner, client, session_id, revision),
    CONSTRAINT session_ingest_published_source_time_order CHECK (((source_started_at IS NULL) AND (source_updated_at IS NULL)) OR ((source_started_at IS NOT NULL) AND (source_updated_at IS NOT NULL) AND (source_updated_at >= source_started_at)))
);
CREATE INDEX session_ingest_published_identity ON session_ingest_published (owner, client, session_id, published_at);

-- The summary of each session's published revision.
CREATE TABLE session_ingest_summaries (
    owner               text NOT NULL,
    client              text NOT NULL,
    session_id          text NOT NULL,
    revision            text NOT NULL,
    rewrite_epoch       bigint NOT NULL DEFAULT 0,
    narrative           text NOT NULL,
    embedding_text      text,
    context_hint        text,
    tags                text[] NOT NULL DEFAULT '{}'::text[],
    model_id            text NOT NULL DEFAULT '',
    input_tokens        integer NOT NULL DEFAULT 0,
    output_tokens       integer NOT NULL DEFAULT 0,
    record_count        integer NOT NULL DEFAULT 0,
    summarized_at       timestamptz NOT NULL DEFAULT now(),
    input_digest        text NOT NULL DEFAULT '',
    title               text,
    title_model         text,
    title_input_digest  text,
    title_generated_at  timestamptz,
    title_attempted_at  timestamptz,
    title_failure_count integer NOT NULL DEFAULT 0,
    title_last_error    text,
    PRIMARY KEY (owner, client, session_id),
    CONSTRAINT session_ingest_summaries_title_failure_count_nonnegative CHECK (title_failure_count >= 0),
    CONSTRAINT session_ingest_summaries_title_nonempty CHECK ((title IS NULL) OR (btrim(title) <> ''))
);
COMMENT ON COLUMN session_ingest_summaries.title IS 'Short model-generated display title for the summary bound by title_input_digest.';
COMMENT ON COLUMN session_ingest_summaries.title_input_digest IS 'Summary input_digest for which title was generated; mismatches are never displayed.';
CREATE INDEX session_ingest_summaries_identity ON session_ingest_summaries (owner, client, session_id);
CREATE INDEX session_ingest_summary_title_queue ON session_ingest_summaries (title_attempted_at NULLS FIRST, summarized_at DESC)
    WHERE ((title IS NULL) OR (title_input_digest IS DISTINCT FROM input_digest)) AND (title_failure_count < 5);

-- Summaries of the record ranges a session is split into, reused while their
-- content is unchanged.
CREATE TABLE session_ingest_chunk_summaries (
    owner            text NOT NULL,
    client           text NOT NULL,
    session_id       text NOT NULL,
    record_start     integer NOT NULL,
    record_end       integer NOT NULL,
    content_hash     text NOT NULL DEFAULT '',
    evidence         text[] NOT NULL DEFAULT '{}'::text[],
    summary          text NOT NULL,
    model_id         text NOT NULL DEFAULT '',
    input_tokens     integer NOT NULL DEFAULT 0,
    output_tokens    integer NOT NULL DEFAULT 0,
    created_at       timestamptz NOT NULL DEFAULT now(),
    map_input_digest text NOT NULL DEFAULT '',
    occurred_at      timestamptz,
    PRIMARY KEY (owner, client, session_id, record_start)
);

-- Map-step summaries saved while a long session is summarized, so a retry
-- resumes from them.
CREATE TABLE session_ingest_map_checkpoints (
    session_id       text NOT NULL,
    record_start     integer NOT NULL,
    owner            text NOT NULL,
    client           text NOT NULL,
    record_end       integer NOT NULL,
    content_hash     text NOT NULL,
    map_input_digest text NOT NULL,
    evidence         text[] NOT NULL DEFAULT '{}'::text[],
    summary          text NOT NULL,
    model_id         text NOT NULL,
    PRIMARY KEY (session_id, record_start),
    CHECK ((record_start >= 0) AND (record_end > record_start))
);

-- Content-defined chunks of pushed bytes, deduplicated within one session.
CREATE TABLE session_push_chunks (
    session_id text NOT NULL REFERENCES session_ingest_bindings(session_id) ON DELETE CASCADE,
    digest     bytea NOT NULL,
    data       bytea NOT NULL,
    PRIMARY KEY (session_id, digest)
);

-- A pushed document or the decoded records of one materialization: an ordered
-- list of chunks.
CREATE TABLE session_push_blobs (
    id         bigserial PRIMARY KEY,
    session_id text NOT NULL REFERENCES session_ingest_bindings(session_id) ON DELETE CASCADE,
    byte_size  bigint NOT NULL,
    sha256     bytea NOT NULL,
    CHECK (byte_size >= 0)
);

-- The chunks of each blob, in order.
CREATE TABLE session_push_blob_chunks (
    blob_id    bigint NOT NULL REFERENCES session_push_blobs(id) ON DELETE CASCADE,
    ordinal    integer NOT NULL,
    session_id text NOT NULL,
    digest     bytea NOT NULL,
    PRIMARY KEY (blob_id, ordinal),
    FOREIGN KEY (session_id, digest) REFERENCES session_push_chunks(session_id, digest)
);
CREATE INDEX session_push_blob_chunks_chunk ON session_push_blob_chunks (session_id, digest);

-- One row per distinct accepted revision of a pushed session. pruned_at marks
-- a superseded revision whose bytes retention removed.
CREATE TABLE session_push_revisions (
    session_id      text NOT NULL REFERENCES session_ingest_bindings(session_id) ON DELETE CASCADE,
    revision        text NOT NULL,
    owner_canonical text NOT NULL,
    format          text NOT NULL,
    source_version  text NOT NULL DEFAULT '',
    byte_size       bigint NOT NULL,
    body_blob_id    bigint REFERENCES session_push_blobs(id),
    accepted_at     timestamptz NOT NULL DEFAULT now(),
    superseded_at   timestamptz,
    pruned_at       timestamptz,
    PRIMARY KEY (session_id, revision),
    CHECK (byte_size >= 0),
    CHECK ((pruned_at IS NULL) = (body_blob_id IS NOT NULL))
);
CREATE INDEX session_push_revisions_prunable ON session_push_revisions (superseded_at)
    WHERE (superseded_at IS NOT NULL) AND (pruned_at IS NULL);

-- The decoded records of one revision under one decoder version. A decoder
-- upgrade adds a row and never rewrites one.
CREATE TABLE session_push_materializations (
    session_id        text NOT NULL,
    revision          text NOT NULL,
    decoder_version   text NOT NULL,
    format            text NOT NULL,
    source_version    text NOT NULL DEFAULT '',
    harness           text NOT NULL DEFAULT '',
    record_count      integer NOT NULL,
    user_turn_count   integer NOT NULL,
    content_bytes     bigint NOT NULL,
    warning_records   integer NOT NULL DEFAULT 0,
    started_at        timestamptz NOT NULL,
    source_updated_at timestamptz NOT NULL,
    records_blob_id   bigint NOT NULL REFERENCES session_push_blobs(id),
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, revision, decoder_version),
    CHECK (source_updated_at >= started_at),
    CHECK (content_bytes >= 0),
    CHECK (record_count > 0),
    CHECK (user_turn_count >= 0),
    FOREIGN KEY (session_id, revision) REFERENCES session_push_revisions(session_id, revision) ON DELETE CASCADE
);

-- Stored bytes and revisions per owner, for push quotas.
CREATE TABLE session_push_owner_usage (
    owner_canonical text PRIMARY KEY,
    stored_bytes    bigint NOT NULL DEFAULT 0,
    revisions       bigint NOT NULL DEFAULT 0,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (revisions >= 0),
    CHECK (stored_bytes >= 0)
);

-- An owner's own choice to keep their sessions owner-only, overriding the
-- deployment default.
CREATE TABLE session_owner_privacy (
    owner      text PRIMARY KEY,
    owner_only boolean NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by text NOT NULL,
    CHECK (owner <> ''),
    CHECK (updated_by <> '')
);

-- Every change to session_owner_privacy.
CREATE TABLE session_owner_privacy_audit (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner             text NOT NULL,
    owner_only        boolean NOT NULL,
    actor             text NOT NULL,
    candidate_updates bigint NOT NULL,
    event_updates     bigint NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX session_owner_privacy_audit_owner_recent ON session_owner_privacy_audit (owner, created_at DESC, id DESC);

-- Every change to a single session's owner-only override.
CREATE TABLE session_privacy_audit (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner         text NOT NULL,
    client        text NOT NULL,
    session_id    text NOT NULL,
    owner_only    boolean NOT NULL,
    actor         text NOT NULL,
    event_updates bigint NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (actor <> ''),
    CHECK (client <> ''),
    CHECK (owner <> ''),
    CHECK (session_id <> '')
);
CREATE INDEX session_privacy_audit_owner_recent ON session_privacy_audit (owner, created_at DESC, id DESC);

-- The archived session events and chunks are append-only.
CREATE FUNCTION reject_legacy_session_archive_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'legacy session supersession history is immutable';
END;
$$;

-- Session events superseded by a canonical publication, kept immutably.
CREATE TABLE legacy_session_event_supersessions (
    event_id       bigint PRIMARY KEY REFERENCES events(id) ON DELETE RESTRICT,
    owner          text NOT NULL,
    client         text NOT NULL,
    session_id     text NOT NULL,
    revision       text NOT NULL,
    event_snapshot jsonb NOT NULL,
    superseded_at  timestamptz NOT NULL DEFAULT now(),
    reason         text NOT NULL DEFAULT 'native_bsky_session_v1',
    CHECK (btrim(reason) <> '')
);
CREATE INDEX legacy_session_event_supersessions_identity ON legacy_session_event_supersessions (owner, client, session_id, revision);
CREATE TRIGGER legacy_session_event_supersessions_immutable BEFORE DELETE OR UPDATE ON legacy_session_event_supersessions
    FOR EACH ROW EXECUTE FUNCTION reject_legacy_session_archive_mutation();

-- The chunks of superseded session events, kept immutably.
CREATE TABLE legacy_session_event_chunk_archive (
    chunk_id       bigint PRIMARY KEY,
    event_id       bigint NOT NULL REFERENCES legacy_session_event_supersessions(event_id) ON DELETE RESTRICT,
    chunk_snapshot jsonb NOT NULL,
    archived_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX legacy_session_event_chunk_archive_event ON legacy_session_event_chunk_archive (event_id, chunk_id);
CREATE TRIGGER legacy_session_event_chunk_archive_immutable BEFORE DELETE OR UPDATE ON legacy_session_event_chunk_archive
    FOR EACH ROW EXECUTE FUNCTION reject_legacy_session_archive_mutation();

-- ===========================================================================
-- Ask
-- ===========================================================================
-- An Ask session is one conversation with the Ask agent: a Slack thread, a
-- DM, an HTTP or MCP caller's session, a scheduled report, or an alert
-- investigation.

-- One Ask conversation. The first model turn pins its prompt release.
CREATE TABLE ask_sessions (
    id                 uuid DEFAULT gen_random_uuid() PRIMARY KEY,
    created_at         timestamptz NOT NULL DEFAULT now(),
    last_turn_at       timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz,
    failure_reason     text,
    owner_email        text,
    title              text,
    title_model        text,
    title_generated_at timestamptz,
    title_attempted_at timestamptz,
    title_last_error   text,
    surface            text,
    initial_model_id   text,
    prompt_release_id  bigint REFERENCES prompt_releases(id),
    initial_scopes     text[],
    CONSTRAINT ask_sessions_initial_model_nonempty CHECK ((initial_model_id IS NULL) OR (btrim(initial_model_id) <> '')),
    CONSTRAINT ask_sessions_initial_scopes_nonempty CHECK ((initial_scopes IS NULL) OR (cardinality(initial_scopes) > 0)),
    CONSTRAINT ask_sessions_surface_known CHECK ((surface IS NULL) OR (surface IN ('http', 'slack', 'scheduled', 'pagerduty', 'unknown'))),
    CONSTRAINT ask_sessions_title_nonempty CHECK ((title IS NULL) OR (btrim(title) <> ''))
);
COMMENT ON COLUMN ask_sessions.title IS 'Display title for this Diderot Ask only. NULL displays its UUID.';
COMMENT ON COLUMN ask_sessions.title_model IS 'Generator model id, or deterministic:fallback when retries were exhausted.';
COMMENT ON COLUMN ask_sessions.title_attempted_at IS 'Claimed once after a successful answer; failures do not retry.';
COMMENT ON COLUMN ask_sessions.surface IS 'Ingress surface of the first recorded turn: http, slack, scheduled, or unknown for unmatched historical rows. NULL only before a new session records its first turn.';
COMMENT ON COLUMN ask_sessions.initial_model_id IS 'Configured inference model for the first recorded turn; NULL for historical sessions.';
COMMENT ON COLUMN ask_sessions.prompt_release_id IS 'Atomic prompt release pinned by the first model turn and reused by every continuation and delegated child.';
COMMENT ON COLUMN ask_sessions.initial_scopes IS 'Allowed corpus scopes of the first recorded turn, written with surface. NULL for sessions that predate it. A Slack turn may continue a pagerduty session only when its own scopes contain all of these.';
CREATE INDEX ask_sessions_created_desc ON ask_sessions (created_at DESC, id DESC);
CREATE INDEX ask_sessions_surface_created_desc ON ask_sessions (surface, created_at DESC, id DESC)
    WHERE surface IS NOT NULL;
CREATE INDEX ask_sessions_title_queue ON ask_sessions (created_at)
    WHERE (title IS NULL) AND (title_attempted_at IS NULL);

-- The persisted conversation, one message per turn; follow-ups replay it.
CREATE TABLE ask_messages (
    id                     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id             uuid NOT NULL REFERENCES ask_sessions(id) ON DELETE CASCADE,
    turn                   integer NOT NULL,
    role                   text NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    input_tokens           integer,
    output_tokens          integer,
    cache_read_tokens      integer,
    cache_write_tokens     integer,
    incomplete_reason      text,
    message_schema_version smallint NOT NULL,
    message                jsonb NOT NULL,
    CONSTRAINT ask_messages_incomplete_reason_check CHECK ((incomplete_reason IS NULL) OR ((role = 'assistant') AND (incomplete_reason IN ('max_tokens', 'stop_sequence', 'malformed_tool_use', 'guardrail_intervened', 'content_filtered', 'malformed_model_output', 'model_context_window_exceeded', 'unknown_stop_reason', 'tool_budget_exhausted', 'synthesis_error', 'reference_unavailable', 'stop_without_respond')))),
    CONSTRAINT ask_messages_message_role_matches CHECK ((message ->> 'role') = role),
    CHECK (role IN ('user', 'assistant')),
    CONSTRAINT ask_messages_schema_version_v1 CHECK (message_schema_version = 1)
);
COMMENT ON COLUMN ask_messages.input_tokens IS 'Bedrock Converse Usage.InputTokens for the turn. NULL on user-role rows or pre-migration assistant rows.';
COMMENT ON COLUMN ask_messages.output_tokens IS 'Bedrock Converse Usage.OutputTokens for the turn. NULL on user-role rows or pre-migration assistant rows.';
COMMENT ON COLUMN ask_messages.cache_read_tokens IS 'Bedrock Converse Usage.CacheReadInputTokens (prompt-cache hits) for the turn.';
COMMENT ON COLUMN ask_messages.cache_write_tokens IS 'Reserved for a future provider-neutral source. Ask Responses rows leave this NULL; Agent Gateway is authoritative.';
COMMENT ON COLUMN ask_messages.message_schema_version IS 'Version of the bounded Eino AgenticMessage JSON persisted in message.';
COMMENT ON COLUMN ask_messages.message IS 'Validated Eino AgenticMessage JSON; no legacy Bedrock content envelope.';
CREATE INDEX ask_messages_created_at ON ask_messages (created_at);
CREATE UNIQUE INDEX idx_ask_messages_session ON ask_messages (session_id, turn);

-- Maps a Slack thread, or a whole DM (thread_ts 'dm'), to its Ask session.
CREATE TABLE ask_slack_sessions (
    channel_id      text NOT NULL,
    thread_ts       text NOT NULL,
    session_id      uuid NOT NULL REFERENCES ask_sessions(id) ON DELETE CASCADE,
    context_through timestamptz,
    PRIMARY KEY (channel_id, thread_ts)
);
CREATE INDEX idx_ask_slack_sessions_by_session ON ask_slack_sessions (session_id);

-- Claims a Slack message before its Ask turn starts, so a redelivered event
-- never answers twice.
CREATE TABLE ask_turn_claims (
    channel_id text NOT NULL,
    message_ts text NOT NULL,
    claimed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, message_ts)
);

-- The Slack messages an answer was posted as, so a reaction resolves to its
-- session and turn.
CREATE TABLE ask_answer_messages (
    channel_id  text NOT NULL,
    message_ts  text NOT NULL,
    session_id  uuid NOT NULL REFERENCES ask_sessions(id) ON DELETE CASCADE,
    turn        integer NOT NULL,
    chunk_index integer NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, message_ts)
);
CREATE INDEX ask_answer_messages_session ON ask_answer_messages (session_id);

-- Reactions on answer messages.
CREATE TABLE ask_feedback (
    session_id uuid NOT NULL REFERENCES ask_sessions(id) ON DELETE CASCADE,
    turn       integer NOT NULL,
    channel_id text NOT NULL,
    message_ts text NOT NULL,
    reactor    text NOT NULL,
    emoji      text NOT NULL,
    verdict    text NOT NULL,
    added_at   timestamptz NOT NULL DEFAULT now(),
    removed_at timestamptz,
    PRIMARY KEY (session_id, turn, message_ts, reactor, emoji),
    CHECK (verdict IN ('up', 'down', 'other'))
);
CREATE INDEX ask_feedback_verdict ON ask_feedback (verdict)
    WHERE removed_at IS NULL;

-- Events an answer cited.
CREATE TABLE ask_citations (
    session_id uuid NOT NULL REFERENCES ask_sessions(id) ON DELETE CASCADE,
    turn       integer NOT NULL,
    event_id   bigint NOT NULL,
    url        text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, turn, event_id)
);

-- Images attached to an Ask turn, and what textual context about each reached
-- the model.
CREATE TABLE ask_input_images (
    session_id        uuid NOT NULL REFERENCES ask_sessions(id) ON DELETE CASCADE,
    source            text NOT NULL,
    source_event_id   bigint NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    source_message_id text NOT NULL,
    source_turn_id    text NOT NULL,
    source_image_id   text NOT NULL,
    filename          text,
    media_type        text,
    permalink         text,
    caption_snapshot  text,
    delivery_status   text NOT NULL,
    failure_reason    text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, source_turn_id, source_image_id),
    CHECK (delivery_status IN ('captioned', 'unavailable'))
);
CREATE INDEX ask_input_images_event ON ask_input_images (source_event_id, source_image_id);

-- One execution of a user turn: model, outcome, and timing.
CREATE TABLE ask_runs (
    id               uuid PRIMARY KEY,
    session_id       uuid NOT NULL REFERENCES ask_sessions(id) ON DELETE CASCADE,
    user_turn        integer,
    surface          text NOT NULL,
    model_id         text NOT NULL,
    reasoning_effort text,
    service_tier     text,
    started_at       timestamptz NOT NULL,
    completed_at     timestamptz NOT NULL,
    duration_ms      bigint NOT NULL,
    outcome          text NOT NULL,
    error            text,
    CONSTRAINT ask_runs_duration_nonnegative CHECK (duration_ms >= 0),
    CONSTRAINT ask_runs_error_bounded CHECK ((error IS NULL) OR (length(error) <= 4096)),
    CONSTRAINT ask_runs_model_nonempty CHECK (model_id <> ''),
    CONSTRAINT ask_runs_outcome_known CHECK (outcome IN ('answered', 'incomplete', 'error')),
    CONSTRAINT ask_runs_surface_known CHECK (surface IN ('http', 'slack', 'scheduled', 'pagerduty')),
    CONSTRAINT ask_runs_user_turn_nonnegative CHECK ((user_turn IS NULL) OR (user_turn >= 0))
);
COMMENT ON TABLE ask_runs IS 'One durable execution trace per Ask user turn.';
CREATE INDEX ask_runs_session_started_idx ON ask_runs (session_id, started_at, id);

-- Each model request a run made, with its latency, usage, and service tier.
CREATE TABLE ask_inference_calls (
    run_id                 uuid NOT NULL REFERENCES ask_runs(id) ON DELETE CASCADE,
    sequence               integer NOT NULL,
    workload               text NOT NULL,
    model_id               text NOT NULL,
    requested_effort       text,
    requested_service_tier text,
    actual_service_tier    text,
    response_id            text,
    started_at             timestamptz NOT NULL,
    completed_at           timestamptz NOT NULL,
    duration_ms            bigint NOT NULL,
    input_tokens           bigint,
    output_tokens          bigint,
    cache_read_tokens      bigint,
    reasoning_tokens       bigint,
    outcome                text NOT NULL,
    error                  text,
    PRIMARY KEY (run_id, sequence),
    CONSTRAINT ask_inference_duration_nonnegative CHECK (duration_ms >= 0),
    CONSTRAINT ask_inference_error_bounded CHECK ((error IS NULL) OR (length(error) <= 4096)),
    CONSTRAINT ask_inference_outcome_known CHECK (outcome IN ('ok', 'error')),
    CONSTRAINT ask_inference_sequence_nonnegative CHECK (sequence >= 0),
    CONSTRAINT ask_inference_tokens_nonnegative CHECK (((input_tokens IS NULL) OR (input_tokens >= 0)) AND ((output_tokens IS NULL) OR (output_tokens >= 0)) AND ((cache_read_tokens IS NULL) OR (cache_read_tokens >= 0)) AND ((reasoning_tokens IS NULL) OR (reasoning_tokens >= 0))),
    CONSTRAINT ask_inference_workload_known CHECK (workload IN ('root', 'observability_child'))
);
COMMENT ON TABLE ask_inference_calls IS 'Per-request model latency, usage, and served-tier telemetry for root and observability-child calls.';

-- Each tool call a run made, with its latency and outcome.
CREATE TABLE ask_tool_calls (
    run_id       uuid NOT NULL REFERENCES ask_runs(id) ON DELETE CASCADE,
    sequence     integer NOT NULL,
    workload     text NOT NULL,
    call_id      text,
    tool_name    text NOT NULL,
    started_at   timestamptz NOT NULL,
    completed_at timestamptz NOT NULL,
    duration_ms  bigint NOT NULL,
    outcome      text NOT NULL,
    error        text,
    PRIMARY KEY (run_id, sequence),
    CONSTRAINT ask_tool_duration_nonnegative CHECK (duration_ms >= 0),
    CONSTRAINT ask_tool_error_bounded CHECK ((error IS NULL) OR (length(error) <= 4096)),
    CONSTRAINT ask_tool_name_nonempty CHECK (tool_name <> ''),
    CONSTRAINT ask_tool_outcome_known CHECK (outcome IN ('ok', 'error')),
    CONSTRAINT ask_tool_sequence_nonnegative CHECK (sequence >= 0),
    CONSTRAINT ask_tool_workload_known CHECK (workload IN ('root', 'observability_child'))
);
COMMENT ON TABLE ask_tool_calls IS 'Per-call tool latency and outcome telemetry for root and observability-child calls.';

-- Content-addressed files Ask tools produced; the bytes live in object
-- storage.
CREATE TABLE ask_artifacts (
    id              uuid PRIMARY KEY,
    session_id      uuid NOT NULL REFERENCES ask_sessions(id) ON DELETE CASCADE,
    tool_call_id    text NOT NULL,
    content_sha256  text NOT NULL,
    storage_key     text NOT NULL,
    media_type      text NOT NULL,
    byte_size       bigint NOT NULL,
    width           integer NOT NULL,
    height          integer NOT NULL,
    source_tool     text NOT NULL,
    status          text NOT NULL,
    idempotency_key text NOT NULL UNIQUE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((byte_size > 0) AND (byte_size <= 10485760)),
    CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK ((height > 0) AND (height <= 4096)),
    CHECK (idempotency_key ~ '^[0-9a-f]{64}$'),
    CHECK (media_type = 'image/png'),
    CHECK (source_tool = 'grafana_get_panel_image'),
    CHECK (status IN ('ready', 'unavailable')),
    CHECK (storage_key <> ''),
    CHECK (tool_call_id <> ''),
    CHECK ((width > 0) AND (width <= 4096))
);
CREATE INDEX ask_artifacts_session_call ON ask_artifacts (session_id, tool_call_id);

-- The artifacts an answer presents, in order.
CREATE TABLE ask_result_artifacts (
    session_id     uuid NOT NULL REFERENCES ask_sessions(id) ON DELETE CASCADE,
    assistant_turn integer NOT NULL,
    "position"     integer NOT NULL,
    artifact_id    uuid NOT NULL REFERENCES ask_artifacts(id) ON DELETE RESTRICT,
    title          text NOT NULL,
    alt_text       text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, assistant_turn, "position"),
    UNIQUE (session_id, assistant_turn, artifact_id),
    CHECK ((alt_text <> '') AND (char_length(alt_text) <= 600)),
    CHECK (assistant_turn >= 0),
    CHECK (("position" >= 0) AND ("position" < 4)),
    CHECK ((title <> '') AND (char_length(title) <= 160)),
    FOREIGN KEY (session_id, assistant_turn) REFERENCES ask_messages(session_id, turn) ON DELETE CASCADE
);

-- Delivery of each answer artifact to a surface, keyed for idempotency.
CREATE TABLE ask_artifact_deliveries (
    session_id     uuid NOT NULL,
    assistant_turn integer NOT NULL,
    artifact_id    uuid NOT NULL REFERENCES ask_artifacts(id) ON DELETE CASCADE,
    surface        text NOT NULL,
    target         text NOT NULL,
    external_id    text,
    status         text NOT NULL,
    failure_reason text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, assistant_turn, artifact_id, surface, target),
    CHECK (status IN ('delivered', 'failed')),
    CHECK (surface IN ('slack', 'scheduled')),
    CHECK (target <> ''),
    CONSTRAINT ask_artifact_deliveries_session_id_assistant_turn_artifact_fkey FOREIGN KEY (session_id, assistant_turn, artifact_id) REFERENCES ask_result_artifacts(session_id, assistant_turn, artifact_id) ON DELETE CASCADE
);

-- The observability routing decision recorded for a turn.
CREATE TABLE ask_observability_steering (
    session_id     uuid NOT NULL REFERENCES ask_sessions(id) ON DELETE CASCADE,
    user_turn      integer NOT NULL,
    intent         text NOT NULL,
    router_version text NOT NULL,
    router_hash    text NOT NULL,
    bundle_version text NOT NULL,
    bundle_hash    text NOT NULL,
    card_ids       text[] NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, user_turn),
    CHECK (bundle_hash ~ '^[0-9a-f]{64}$'),
    CHECK (intent IN ('none', 'alert', 'slo', 'exploration', 'incident')),
    CHECK (router_hash ~ '^[0-9a-f]{64}$'),
    CHECK (user_turn >= 0)
);

-- The pre-release Ask prompt version each older session used.
CREATE TABLE legacy_ask_session_prompts (
    session_id       uuid PRIMARY KEY REFERENCES ask_sessions(id) ON DELETE RESTRICT,
    legacy_prompt_id bigint NOT NULL REFERENCES legacy_ask_prompts(id) ON DELETE RESTRICT,
    recovery_source  text NOT NULL,
    recovered_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (btrim(recovery_source) <> '')
);
COMMENT ON TABLE legacy_ask_session_prompts IS 'Recovered per-session Ask prompt provenance from the retired ask_sessions.initial_prompt_id column.';
CREATE INDEX legacy_ask_session_prompts_prompt ON legacy_ask_session_prompts (legacy_prompt_id);
CREATE TRIGGER legacy_ask_session_prompts_immutable BEFORE DELETE OR UPDATE ON legacy_ask_session_prompts
    FOR EACH ROW EXECUTE FUNCTION reject_legacy_ask_prompt_mutation();

-- MCP task state, shared by every replica because a task's requests can land
-- on any of them.
CREATE TABLE mcp_tasks (
    id               uuid PRIMARY KEY,
    owner_email      text NOT NULL,
    status           text NOT NULL,
    status_message   text,
    created_at       timestamptz NOT NULL,
    last_updated_at  timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    poll_interval_ms bigint,
    result           jsonb,
    error            jsonb,
    CONSTRAINT mcp_tasks_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT mcp_tasks_owner_nonempty CHECK (btrim(owner_email) <> ''),
    CONSTRAINT mcp_tasks_poll_interval_positive CHECK ((poll_interval_ms IS NULL) OR (poll_interval_ms > 0)),
    CONSTRAINT mcp_tasks_status_known CHECK (status IN ('working', 'input_required', 'completed', 'failed', 'cancelled')),
    CONSTRAINT mcp_tasks_terminal_payload CHECK (((status = 'completed') AND (result IS NOT NULL) AND (error IS NULL)) OR ((status = 'failed') AND (result IS NULL) AND (error IS NOT NULL)) OR ((status <> ALL (ARRAY['completed', 'failed'])) AND (result IS NULL) AND (error IS NULL)))
);
CREATE INDEX mcp_tasks_expires_at_idx ON mcp_tasks (expires_at);

-- ===========================================================================
-- Retrieval evaluation
-- ===========================================================================
-- Golden question sets, relevance judgments, and evaluation runs.

-- A question's golden-set split never changes once assigned.
CREATE FUNCTION eval_reject_split_reassignment() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.eval_split IS NOT NULL AND NEW.eval_split IS DISTINCT FROM OLD.eval_split THEN
        RAISE EXCEPTION 'eval question split assignments are immutable';
    END IF;
    RETURN NEW;
END
$$;

-- Questions in a golden set, curated from real asks.
CREATE TABLE eval_questions (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    golden_set         text NOT NULL,
    question           text NOT NULL,
    source_kind        text NOT NULL,
    origin_session     uuid REFERENCES ask_sessions(id),
    origin_user_turn   integer,
    origin_answer_turn integer,
    status             text NOT NULL DEFAULT 'active',
    excluded_reason    text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    notes              text,
    curated_at         timestamptz,
    curated_by         text,
    eval_split         text,
    curation_method    text,
    applied_triage_id  bigint,
    CONSTRAINT eval_questions_curation_fields_together CHECK (((curated_at IS NULL) AND (curated_by IS NULL) AND (curation_method IS NULL) AND (applied_triage_id IS NULL)) OR ((curated_at IS NOT NULL) AND (curated_by IS NOT NULL) AND (curation_method = 'human') AND (applied_triage_id IS NULL)) OR ((curated_at IS NOT NULL) AND (curated_by IS NOT NULL) AND (curation_method = 'model') AND (applied_triage_id IS NOT NULL))),
    CHECK (curation_method IN ('human', 'model')),
    CHECK (eval_split IN ('development', 'confirmation')),
    CHECK (source_kind IN ('mined', 'synthetic', 'manual')),
    CHECK (status IN ('active', 'excluded'))
);
CREATE UNIQUE INDEX idx_eval_questions_origin ON eval_questions (golden_set, origin_session, origin_answer_turn)
    WHERE origin_session IS NOT NULL;
CREATE INDEX idx_eval_questions_pending_curation ON eval_questions (golden_set, id)
    WHERE (status = 'active') AND (curated_at IS NULL);
CREATE INDEX idx_eval_questions_set ON eval_questions (golden_set, status);
CREATE INDEX idx_eval_questions_split ON eval_questions (golden_set, eval_split, id)
    WHERE status = 'active';
CREATE TRIGGER eval_questions_split_immutable BEFORE UPDATE OF eval_split ON eval_questions
    FOR EACH ROW EXECUTE FUNCTION eval_reject_split_reassignment();

-- A golden-set split is append-only.
CREATE FUNCTION eval_reject_split_manifest_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'eval golden-set split manifests are immutable';
END
$$;

-- The immutable tuning/confirmation split of a golden set.
CREATE TABLE eval_golden_set_splits (
    golden_set           text PRIMARY KEY,
    seed                 text NOT NULL,
    confirmation_percent integer NOT NULL,
    assigned_at          timestamptz NOT NULL DEFAULT now(),
    assigned_by          text NOT NULL,
    CHECK ((confirmation_percent >= 1) AND (confirmation_percent <= 50))
);
CREATE TRIGGER eval_golden_set_splits_immutable BEFORE DELETE OR UPDATE ON eval_golden_set_splits
    FOR EACH ROW EXECUTE FUNCTION eval_reject_split_manifest_mutation();

-- One labeling pass, by a person or a judge model.
CREATE TABLE eval_label_runs (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    golden_set         text NOT NULL,
    label_source       text NOT NULL,
    judge_model        text,
    rubric_version     text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    agreement_exact    numeric,
    agreement_within_1 numeric,
    anchor_label_count integer,
    notes              text,
    created_by         text,
    CHECK (label_source IN ('cited', 'judge', 'human'))
);

-- The relevance grade of one event for one question.
CREATE TABLE eval_judgments (
    question_id              bigint NOT NULL REFERENCES eval_questions(id),
    event_id                 bigint NOT NULL,
    grade                    smallint NOT NULL,
    label_run_id             bigint NOT NULL REFERENCES eval_label_runs(id),
    judged_content_hash      text NOT NULL,
    judged_at                timestamptz NOT NULL DEFAULT now(),
    judged_eval_content_hash text,
    PRIMARY KEY (question_id, event_id, label_run_id),
    CHECK ((grade >= 0) AND (grade <= 3))
);
COMMENT ON COLUMN eval_judgments.judged_content_hash IS 'Body-only events.content_hash captured when the judgment was written.';
COMMENT ON COLUMN eval_judgments.judged_eval_content_hash IS 'Title+body events.eval_content_hash captured when known; NULL for historical rows whose title identity cannot be reconstructed.';
CREATE INDEX idx_eval_judgments_run ON eval_judgments (label_run_id);

-- Events pooled as candidates for judgment.
CREATE TABLE eval_pool_candidates (
    question_id bigint NOT NULL REFERENCES eval_questions(id),
    event_id    bigint NOT NULL,
    origin      text NOT NULL,
    rank        smallint,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (question_id, event_id, origin)
);

-- One evaluation of a retriever configuration against a golden set.
CREATE TABLE eval_runs (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    golden_set         text NOT NULL,
    ran_at             timestamptz NOT NULL DEFAULT now(),
    git_sha            text,
    retriever_tag      text NOT NULL,
    params             jsonb NOT NULL,
    results            jsonb NOT NULL,
    label_run_ids      jsonb NOT NULL,
    effective_set_hash text NOT NULL,
    pool_coverage      numeric NOT NULL,
    decision_grade     boolean NOT NULL,
    override_reason    text
);
CREATE INDEX idx_eval_runs_set ON eval_runs (golden_set, ran_at DESC);

-- One model-assisted triage pass over a golden set's questions.
CREATE TABLE eval_question_triage_runs (
    id                      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    golden_set              text NOT NULL,
    model                   text NOT NULL,
    rubric_version          text NOT NULL,
    audit_percent           integer NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    finished_at             timestamptz,
    questions_triaged       integer NOT NULL DEFAULT 0,
    questions_failed        integer NOT NULL DEFAULT 0,
    applied_at              timestamptz,
    applied_by              text,
    audited_high_confidence integer,
    audit_agreement         numeric,
    superseded_at           timestamptz,
    superseded_by           text,
    superseded_reason       text,
    rubric_prompt           text NOT NULL,
    CHECK ((audit_percent >= 1) AND (audit_percent <= 100)),
    CHECK ((applied_at IS NULL) = (applied_by IS NULL)),
    CONSTRAINT eval_question_triage_runs_superseded_fields CHECK (((superseded_at IS NULL) AND (superseded_by IS NULL) AND (superseded_reason IS NULL)) OR ((superseded_at IS NOT NULL) AND (superseded_by IS NOT NULL) AND (superseded_reason IS NOT NULL))),
    CONSTRAINT eval_question_triage_runs_terminal_state CHECK (NOT ((applied_at IS NOT NULL) AND (superseded_at IS NOT NULL)))
);
CREATE INDEX idx_eval_question_triage_runs_open ON eval_question_triage_runs (golden_set, id DESC)
    WHERE (applied_at IS NULL) AND (superseded_at IS NULL);

-- The triage recommendation for one question.
CREATE TABLE eval_question_triage (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id         bigint NOT NULL REFERENCES eval_question_triage_runs(id),
    question_id    bigint NOT NULL REFERENCES eval_questions(id),
    recommendation text NOT NULL,
    confidence     text NOT NULL,
    reason         text NOT NULL,
    audit_required boolean NOT NULL DEFAULT false,
    raw_response   text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, question_id),
    CHECK (confidence IN ('high', 'medium', 'low')),
    CHECK (recommendation IN ('approve', 'exclude'))
);
CREATE INDEX idx_eval_question_triage_audit ON eval_question_triage (run_id, audit_required, question_id);
CREATE INDEX idx_eval_question_triage_latest ON eval_question_triage (question_id, run_id DESC);

ALTER TABLE eval_questions
    ADD CONSTRAINT eval_questions_applied_triage_id_fkey FOREIGN KEY (applied_triage_id) REFERENCES eval_question_triage(id);

-- ===========================================================================
-- Scheduled reports
-- ===========================================================================

-- Reports that run an Ask prompt on a schedule and post the answer to Slack.
CREATE TABLE scheduled_reports (
    id                     uuid DEFAULT gen_random_uuid() PRIMARY KEY,
    name                   text NOT NULL UNIQUE,
    prompt                 text NOT NULL,
    cadence                text NOT NULL,
    weekday                smallint,
    schedule_hour          smallint NOT NULL,
    schedule_minute        smallint NOT NULL,
    timezone               text NOT NULL,
    delivery_kind          text NOT NULL,
    slack_target           text NOT NULL,
    owner_email            text NOT NULL,
    catchup_window_seconds integer NOT NULL DEFAULT 21600,
    enabled                boolean NOT NULL DEFAULT true,
    next_run_at            timestamptz NOT NULL,
    last_success_at        timestamptz,
    last_error             text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    created_by             text NOT NULL,
    CHECK (cadence IN ('daily', 'weekdays', 'weekly')),
    CONSTRAINT scheduled_reports_cadence_weekday_check CHECK (((cadence IN ('daily', 'weekdays')) AND (weekday IS NULL)) OR ((cadence = 'weekly') AND (weekday IS NOT NULL))),
    CHECK (catchup_window_seconds > 0),
    CHECK (delivery_kind IN ('slack_channel', 'slack_dm')),
    CHECK ((schedule_hour >= 0) AND (schedule_hour <= 23)),
    CHECK ((schedule_minute >= 0) AND (schedule_minute <= 59)),
    CHECK ((weekday >= 1) AND (weekday <= 7))
);
CREATE INDEX scheduled_reports_due ON scheduled_reports (next_run_at)
    WHERE enabled;

-- One run of a scheduled report.
CREATE TABLE scheduled_report_runs (
    id                 uuid DEFAULT gen_random_uuid() PRIMARY KEY,
    report_id          uuid NOT NULL REFERENCES scheduled_reports(id) ON DELETE CASCADE,
    trigger            text NOT NULL,
    scheduled_for      timestamptz NOT NULL,
    window_start       timestamptz NOT NULL,
    window_end         timestamptz NOT NULL,
    prompt_snapshot    text NOT NULL,
    status             text NOT NULL,
    attempts           integer NOT NULL DEFAULT 0,
    claimed_at         timestamptz,
    finished_at        timestamptz,
    ask_session_id     uuid REFERENCES ask_sessions(id) ON DELETE SET NULL,
    output             text,
    delivery_ts        text,
    error              text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    ask_assistant_turn integer,
    delivery_kind      text,
    slack_target       text,
    UNIQUE (report_id, scheduled_for),
    CHECK (ask_assistant_turn >= 0),
    CHECK (attempts >= 0),
    CHECK (window_start < window_end),
    CONSTRAINT scheduled_report_runs_delivery_audience CHECK (((delivery_kind IS NULL) AND (slack_target IS NULL)) OR ((delivery_kind IS NOT NULL) AND (slack_target IS NOT NULL) AND (((delivery_kind = 'slack_channel') AND (slack_target ~ '^[CG][A-Z0-9]+$')) OR ((delivery_kind = 'slack_dm') AND (slack_target ~ '^[UW][A-Z0-9]+$'))))),
    CHECK (status IN ('pending', 'running', 'generated', 'succeeded', 'failed', 'skipped')),
    CHECK (trigger IN ('scheduled', 'manual'))
);
CREATE INDEX scheduled_report_runs_queue ON scheduled_report_runs (scheduled_for, created_at)
    WHERE status IN ('pending', 'running', 'generated');
CREATE INDEX scheduled_report_runs_report ON scheduled_report_runs (report_id, scheduled_for DESC);

-- ===========================================================================
-- PagerDuty
-- ===========================================================================

-- One Ask investigation per PagerDuty alert, and its delivery to the incident
-- and to Slack.
CREATE TABLE pagerduty_investigations (
    event_id          text PRIMARY KEY,
    incident_id       text NOT NULL,
    occurred_at       timestamptz NOT NULL,
    event             jsonb NOT NULL,
    session_id        uuid REFERENCES ask_sessions(id),
    output            text,
    note_id           text,
    slack_channel_id  text,
    slack_thread_ts   text,
    slack_message_ts  text,
    state             text NOT NULL DEFAULT 'pending',
    attempts          integer NOT NULL DEFAULT 0,
    delivery_attempts integer NOT NULL DEFAULT 0,
    worker_token      uuid,
    lease_until       timestamptz,
    next_attempt_at   timestamptz NOT NULL DEFAULT now(),
    last_error        text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    completed_at      timestamptz,
    assistant_turn    integer NOT NULL DEFAULT 0,
    UNIQUE (incident_id, occurred_at),
    CHECK (assistant_turn >= 0),
    CHECK (state IN ('pending', 'running', 'done', 'failed'))
);
CREATE INDEX pagerduty_investigations_queue ON pagerduty_investigations (next_attempt_at, created_at)
    WHERE state IN ('pending', 'running');

-- ===========================================================================
-- Administration
-- ===========================================================================

-- Every state-changing administrator action, from the browser or the API.
CREATE TABLE admin_audit_log (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    actor_email text NOT NULL,
    action      text NOT NULL,
    target      text NOT NULL,
    request_id  text,
    status      text NOT NULL DEFAULT 'started',
    details     jsonb NOT NULL DEFAULT '{}'::jsonb,
    error       text,
    CHECK (status IN ('started', 'succeeded', 'failed'))
);
CREATE INDEX admin_audit_log_action_recent_idx ON admin_audit_log (action, created_at DESC, id DESC);
CREATE INDEX admin_audit_log_actor_recent_idx ON admin_audit_log (actor_email, created_at DESC, id DESC);
CREATE INDEX admin_audit_log_recent_idx ON admin_audit_log (created_at DESC, id DESC);
CREATE INDEX admin_audit_log_request_id_idx ON admin_audit_log (request_id)
    WHERE request_id IS NOT NULL;
CREATE INDEX admin_audit_log_status_recent_idx ON admin_audit_log (status, created_at DESC, id DESC);
CREATE INDEX admin_audit_log_target_recent_idx ON admin_audit_log (target, created_at DESC, id DESC);

-- Long-running administrator jobs: who started each, its progress, and its
-- outcome.
CREATE TABLE admin_jobs (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind                text NOT NULL,
    status              text NOT NULL,
    requested_by        text NOT NULL,
    request             jsonb NOT NULL DEFAULT '{}'::jsonb,
    progress            jsonb NOT NULL DEFAULT '{}'::jsonb,
    requested_at        timestamptz NOT NULL DEFAULT now(),
    started_at          timestamptz,
    heartbeat_at        timestamptz NOT NULL DEFAULT now(),
    stale_after_seconds integer NOT NULL,
    finished_at         timestamptz,
    error               text,
    CONSTRAINT admin_jobs_finished_state CHECK (((status IN ('queued', 'running')) AND (finished_at IS NULL)) OR ((status IN ('succeeded', 'failed', 'canceled')) AND (finished_at IS NOT NULL))),
    CHECK (kind <> ''),
    CHECK (requested_by <> ''),
    CHECK (stale_after_seconds > 0),
    CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled'))
);
COMMENT ON TABLE admin_jobs IS 'Durable, attributable progress and outcome ledger for long-running administrator work.';
COMMENT ON COLUMN admin_jobs.heartbeat_at IS 'Last proof that the process executing this job was alive; a new start may fail an expired active row before claiming the kind.';
COMMENT ON COLUMN admin_jobs.stale_after_seconds IS 'Executor-owned lease duration used to classify and recover an active row whose heartbeat stops.';
CREATE UNIQUE INDEX admin_jobs_one_active_kind ON admin_jobs (kind)
    WHERE status IN ('queued', 'running');
CREATE INDEX admin_jobs_recent ON admin_jobs (requested_at DESC, id DESC);
