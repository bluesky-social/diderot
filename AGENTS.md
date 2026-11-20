# AGENTS.md (diderot)

Guidance for coding agents and contributors working in this repository.
`CLAUDE.md` is a symlink to this file. User documentation lives in
[`docs/`](docs/); [CONTRIBUTING.md](CONTRIBUTING.md) covers setup and pull
requests.

Code comments don't cite documents outside this repository: a comment
states the constraint itself.

## What this is

Diderot answers questions about an organization's written knowledge. Connectors
ingest sources into one typed Postgres event log (`events.source`): `slack`,
`agent_session` (summaries of coding-agent sessions, from a session feed or
HTTP push), `notion`, `linear`, `zendesk`, `markdown` (the files of a
mounted folder), and `discord` (the chosen channels of the servers a bot is
in). Harness and inference provider
are provenance, not separate session sources. Postgres is the source of truth;
Qdrant holds the embedded chunks.

**Retrieval is vector-only**: embed the question, ANN over Qdrant, cosine rank,
dedup by `event_id` (`internal/retrieve/retrieve.go`, `internal/qdrantstore/`).
There is no reranker, no `tsvector` column, and no lexical or fusion leg.
Don't call retrieval "hybrid".

The Ask agent (`/v1/ask`, `@diderot` in Slack, the MCP `ask` task) is an Eino
loop that calls its model through the provider-native protocol its test on
the Models page found. Its tools: `retrieve` / `list` / `expand` over the corpus,
`web_search` / `web_fetch` (Tavily), `code_search` / `code_read` (Sourcebot),
and the tools of the MCP servers administrators add as tool connections
([docs/mcp-servers.md](docs/mcp-servers.md)), on the root agent, on
`investigate_observability` (a delegated child agent), or on both.
Optional tools are registered only when their configuration is present; the
web and code tools follow their connectors (Web search, Code search), so each
run offers them only while those are enabled. Every tool implements
`Status()` for the Slack progress line. Integrations that
speak MCP are tool connections, not new Go clients. Product configuration
(what an administrator chooses, credentials included) lives in the database
settings and the admin UI, never in a new variable or file; see
[Where a setting lives](docs/configuration.md#where-a-setting-lives).

[`docs/design.md`](docs/design.md) describes the system as built. Keep it
current when you change behavior it describes.

## Layout

```text
cmd/diderot/          the server (`serve`) and its maintenance commands, with the
                      client commands mounted too
main.go               (the module root) the release download and `go install` target:
                      the client commands (`setup`, `doctor`, `ask`, `sessions`) alone; its
                      test fails if the server's packages enter its dependency
                      graph (docs/cli.md)
internal/
  adminapi/           the /v1 HTTP API, /mcp authentication, and the in-process
                      Slack, re-embed and image services the admin UI acts through
  adminaction/        every state-changing administrator action, with its audit row
  adminui/            the browser admin and /sessions UI, and the ui-dev fixture server
  ask/                the Ask agent, its tools, the corpus MCP server
  auth/  authorization/
                      server authentication, scope and policy decisions
  config/             environment configuration (docs/configuration.md is its reference)
  source/             the Source interface every indexed source implements, its optional
                      status, backfill and page-section interfaces, its own state, and the registry
  asktool/            the Tool interface for connectors that give Ask tools (web, code search), and the registry
  askconn/            tool connections: the MCP servers administrators give Ask, their settings and Test
  automation/         the Automation interface for connectors that run Ask on a trigger (PagerDuty), and the registry
  connector/          slack, notion, linear, zendesk, sessionfeed, markdown, discord
  attachments/        the caption (files/) and document (docs/) workers that sources lend fetchers to
  clientcmd/          the client commands both binaries mount
  client/             the client's sign-in (MCP authorization discovery, OAuth, saved token)
                      and its MCP calls, Ask's task included
  harness/            registering the MCP server and installing the skill for each coding-agent harness
  sessionship/        `diderot sessions`: finding a harness's session files, converting them to
                      ATIF, pushing them, and the launchd/systemd background service
  safehttp/           the factory every connector and tool HTTP client is built through
  ratelimit/          the token bucket Notion, Linear, and Zendesk share
  sessiondecode/      session document decoders (FORMATS.md)
  sessioningest/      session feed, push ingest, summarization worker
  events/  chunk/  embedder/  qdrantstore/  retrieve/
                      the sink, chunking, embedding, vector store, retrieval
  modelregistry/  modelprovider/  llmclient/
                      models (listing, each job's test, entries), the provider
                      settings family, the shared HTTP client
  modelfacts/         the published model lists (models.dev, LiteLLM): fetching,
                      keeping, and matching a provider's model to an entry
  promptcatalog/      prompt releases, validation, variables (docs/prompts.md)
  scheduledreport/  pagerduty/
                      recurring reports, alert investigations
  migrations/         Atlas migrations (sql/), applied when the server starts
  leader/             leader election (Kubernetes Lease or Postgres advisory lock)
  testdb/             per-package test database clones
  e2e/                the quickstart smoke test and the opt-in real-Slack suite
prompts/              the example prompt catalog a fresh database seeds
skills/diderot/       the agent skill that teaches the MCP tools, embedded in the binary
                      (`skills/skills.go`) for `diderot setup` to install
quickstart/  compose.quickstart.yaml
                      the Docker Compose quickstart (docs/quickstart.md)
compose.yaml          development Postgres and Qdrant
```

## Build and test

- `just install-tools` once: golangci-lint, gotestsum, and a checksum-pinned
  `atlas` in `~/bin/atlas`.
- `just up` starts the development Postgres (`localhost:15432`) and Qdrant
  (`localhost:16334`) from `compose.yaml`.
- `just test [packages]` starts or reuses that stack and runs the suite with
  `DIDEROT_TEST_DB_URL`, `DIDEROT_QDRANT_TEST_ADDR`, and `DIDEROT_ATLAS_BINARY`
  defaulted to it. Packages run in parallel, each against its own clone of a
  migrated base database (`internal/testdb`). Clone names carry a per-checkout
  key (`diderot_test_<key>_<pkg>`), so several checkouts or worktrees can
  share one Postgres. Clones outlive their tests for inspection; each test
  process drops, in the background, any clone over an hour old that no
  running test holds or is connected to, so clones from old runs and deleted
  worktrees don't pile up. Compose pins container names, so in a second
  checkout don't run `just up`; run against the existing stack instead:

  ```sh
  DIDEROT_TEST_DB_URL='postgres://diderot:diderot@localhost:15432/diderot?sslmode=disable' \
  DIDEROT_QDRANT_TEST_ADDR=localhost:16334 DIDEROT_ATLAS_BINARY=$HOME/bin/atlas \
    gotestsum -- -count=1 ./...
  ```

  Without `DIDEROT_TEST_DB_URL`, database tests skip.
- `just lint` runs golangci-lint. Before pushing:
  `go build ./... && go vet ./... && just lint && just test`.
- `just quickstart-smoke` builds the Compose quickstart against a stub model
  provider, ingests a fixture, retrieves and asks about it, and tears the stack
  down. Run it when you touch the quickstart, the Dockerfile, or startup.
- `just run diderot <args>` runs the binary from the working tree: the
  client commands `setup`, `doctor`, `ask`, and `sessions`; the server commands `serve`,
  `probe`, `qdrant-audit`, and `reset-admin`; and the development servers
  `ui-dev` and `session-dev`. People and agents use Slack and the MCP server
  at `/mcp` (directly or through the client commands), and administrators
  the `/admin` UI, whose Ask session page
  (`/admin/ask/sessions/<id>`) shows one session's timing, turns, tool calls
  and their inputs, and the answer.
- Opt-in suites, never in CI: `just test-slack-e2e` (`-tags e2e`, a real Slack
  workspace and a running deployment) and the `-tags gateway` inference tests
  (a real model gateway). Their environment is in
  [docs/configuration.md](docs/configuration.md#development-and-tests).

## Conventions

- **No compatibility layers.** The project's supported consumers are the MCP
  surface, the `/v1` use routes, and the admin UI and client commands
  shipped from this repository, so an endpoint, schema, tool shape, wire format, Go interface,
  table column, or setting changes in one commit together with its in-repo
  consumers. Don't keep an old path as an
  alias, add deprecation shims, or dual-support an old shape. A breaking change
  is marked in its commit (`feat!:` or a `BREAKING CHANGE:` footer) so the
  release notes call it out.
- **The client commands use `/mcp` and nothing else.** What `setup`,
  `doctor`, or `ask` needs from a server comes through MCP; never add a `/v1`
  route or another side API for them. They must work against a gateway in
  front of Diderot, which forwards only MCP. The one exception is
  `diderot sessions`, which pushes to the session push route
  (`PUT /v1/ingest/agent-sessions/{id}`) beside the MCP endpoint, because
  that route is how a session reaches a server; it needs the server itself,
  or a gateway that forwards that route too.
- **A harness's session files are read in one place.** `internal/sessiondecode`
  holds the only decoder for each harness's format. The server runs it on a
  pushed file; `diderot sessions` runs the same decoder on the person's
  machine and sends the result as ATIF (`sessiondecode.EncodeATIF`). Shipping
  from a new harness is a decoder there and an entry in
  `sessionship.Harnesses`.
- **Downtime over transitional code.** When a change needs a migration,
  backfill, or reindex, prefer a stop-and-restart over dual-write windows or
  partial-migration states. Ingest catches up after a restart (the Slack tail
  sweep, the session feed, the embedder backlog), so an outage loses nothing
  durable.
- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org/).
  Pull requests are squash-merged and the title becomes the changelog entry, so
  write it as one (CONTRIBUTING.md).
  A branch landed with a merge commit at its tip is invisible to the release
  tool: it reads commit titles, and changes that touch only `docs/` or
  `.github/` are left out of releases.
- **Tests** come with behavior changes. Fixtures are synthetic: never commit
  real transcripts, credentials, tokens, database URLs, or private messages.
- **Configuration** is read from `DIDEROT_*` variables in `internal/config`,
  except a source's own deployment variable, such as `DIDEROT_MARKDOWN_DIR`,
  which its package reads. Every variable the code reads needs a row in
  [docs/configuration.md](docs/configuration.md);
  `TestConfigurationReferenceMatchesCode` fails otherwise.
- **Schema changes** are new files in `internal/migrations/sql/`. Never edit a
  released migration: Atlas checksums them. The first file is a baseline that
  stands in for the history before it, under that history's last version;
  `TestBaselineSchemaMatchesGolden` pins the schema it builds.
- **Prompts.** Model instruction text lives in the prompt catalog, not in Go;
  see [docs/prompts.md](docs/prompts.md). The database is the only source at
  runtime; `prompts/` is the example a fresh install seeds, and editing it
  changes no existing install. Its rendered bundle hash is pinned in
  `prompts/prompts_test.go`. Tool descriptions and schemas stay in Go beside
  their tools.

## Architecture invariants

- **Sources are sticky.** `events.source` values appear in migrations,
  indexes, retrieval filters, the tools' `sources` enum, and tests. Renaming one
  needs a migration.
- **The sink is idempotent** on `(source, external_id)` and short-circuits on
  unchanged content, so a re-ingest costs no embedding.
- **The sink makes the event row and its attachments storable.** Before the
  insert, `events.DBSink` removes NUL bytes and replaces invalid UTF-8
  (`events.SanitizeText`) in every `events` text field and the raw JSON
  document, and `ReconcileDocuments`/`ReconcileImages` do the same for a
  document's filename and either call's media type, because Postgres
  rejects all of this outright (text: SQLSTATE 22021; jsonb: 22P05) and a
  connector's sync loop has no transient-vs-permanent way to tell that
  failure apart from a real outage. The dirty-check and no-change
  short-circuit compare the sanitized form on both sides, so sanitizing
  never turns a re-ingest of the same dirty item into needless
  re-embedding. A connector whose "nothing to index" guard trims Body
  before deciding must trim the *sanitized* form
  (`strings.TrimSpace(events.SanitizeText(body))`), not the raw one — a
  NUL-only body isn't whitespace, so a plain `TrimSpace` guard lets it
  through to Upsert, which then sanitizes it to "" and fails `validate()`'s
  "Body is required" on every retry, forever (Discord, Notion and Slack all
  needed this).
  `internal/source.State.Set`/`State.Update` (the per-source cursor,
  directory and selection state every connector keeps in `source_state`,
  itself jsonb) sanitize the same way, with the sink's own parse-aware
  JSON sanitizer (`events.SanitizeJSON`), before the write reaches
  Postgres — a Discord thread/category/role name, say, is exactly as
  capable of carrying a NUL byte as a message body, and `source_state`
  rejects it the same way (22P05). Neither covers a side-write table or
  state store a connector or worker fills itself: sanitize what goes into
  one with `events.SanitizeText`/`events.SanitizeJSON` too, as Zendesk's
  `zendesk_files`, the session summary and title calls, and the admin
  audit log do, rather than inventing a second rule.
- **Lock the event row first.** A transaction that writes an event's
  `event_chunks`, `event_images`, or `event_documents` rows takes
  `SELECT ... FROM events WHERE id = $1 FOR UPDATE` before it touches any of
  them, and never takes the event row while holding a child row. The sink's
  upsert, `ReconcileImages`, `ReconcileDocuments`,
  `ReconcileDocumentImagesTx`, and the caption and document workers' commits
  all do; a writer that locks child rows in data-dependent order instead
  deadlocks against them (SQLSTATE 40P01). A transaction that locks several
  rows of one table and no event row (the embedder's chunk batch) locks them
  in `id` order. Session ingest takes the owner's advisory lock
  (`lockOwnerPrivacyTx`) before the session's `session_ingest_bindings` row
  and before any candidate row. Don't wrap a step of such a transaction in a
  savepoint to survive its error: the retry re-takes locks out of order.
  Validate in Go before the first lock instead.
- **An event is deleted only by a tombstone through the sink** (`Delete`,
  `DeleteInTx`, `DeletePattern`): `deleted = true`, the body cleared, every
  chunk row removed, and the `event_chunks` delete trigger queues the Qdrant
  points in the same transaction. Event rows are never removed. A source
  tombstones only its own `(source, external_id)` values. The one writer
  outside the sink is the session publisher retiring the legacy events a published
  revision supersedes (`internal/sessioningest/publish.go`).
- **Chunk kinds have separate lifetimes.** The sink's upsert replaces only
  `kind = 'text'` chunks, so an edit never erases captions or document text.
  An `image_caption` chunk lives and dies with its `event_images` row, and
  `document_text` chunks with their `event_document_id`. Only a tombstone
  removes every kind at once.
- **Absence is evidence of a delete only after a complete read.** A source
  may delete something because the upstream stopped returning it only when
  that same pass read the whole listing itself: no error, no resumed cursor,
  no truncated page. Notion deletes a removed root's pages only when no
  surviving root still reaches them; Slack does not delete on absence. A
  failed or partial fetch deletes nothing and does not move a cursor or
  watermark past the item that failed.
- **A background job never brings an event back.** A job that rewrites an
  event from an earlier read ties its write to what it read, so it can't
  resurrect a deleted event or revert an edit made meanwhile: re-embed
  skips a row changed since its read (`CheckUnchangedSince`), and the caption
  and document commits re-read `deleted` under the event lock.
- **The tool layer is source-agnostic.** `expand` dispatches by source on the
  server; the model never picks a flavor. Don't narrow the `retrieve` / `list`
  `sources` enum: a source missing from it is invisible to filtered queries, and
  the model otherwise defaults to Slack.
- **Expand checks the event's scope before any source reads it**
  (`cmd/diderot/expander.go`), so a source's `Expand` needs no check of its
  own for the event itself, only for anything else it returns (Slack's
  thread neighbours, pushed sessions' records).
- **A source reaches the rest of the system only through `source.Deps` and
  its optional interfaces** (`StatusReporter`, `Backfiller`,
  `SectionProvider`). New state goes in `Deps.State`, not a table of its
  own; base metrics come from building its workers with `Deps.Metrics` and
  writing through `Deps.Sink`. [docs/adding-a-source.md](docs/adding-a-source.md)
  is the guide, and `internal/connector/markdown` the template; if a new
  source needs a change outside its package and its registration line, fix
  the interface.
- **Actors are stored verbatim** per source. The tool layer projects them once
  into the structured identity contract in `internal/identity`; don't
  pretty-format actor strings elsewhere.
- **The embedder must not fall behind silently.** It fills
  `event_chunks.embedded_at` and Qdrant; if it stalls, retrieval returns empty
  results without an obvious error.
- **Session formats are decoded, never defined**, by diderot
  (`internal/sessiondecode`, [FORMATS.md](internal/sessiondecode/FORMATS.md)).
  The record model is private; a decoder change bumps its `Version()`, which
  produces a new immutable materialization instead of rewriting stored
  records. The feed's `bsky.session.v1` path (`DecodePinned`) must keep its
  acceptance rules and error text; prove equivalence on the synthetic fixtures
  before changing it. Every candidate write binds `session_id` to an owner, a
  writer (`feed` or `push`), and a format in `session_ingest_bindings`: push may
  never touch a feed session or another owner's, and the feed stays the
  authority for its own sessions' owners.
- **Agent-session rows carry a range handle**
  `(session_id, record_start, record_end)`, so expansion reads a
  revision-aware record range and never depends on a chunk ID that could move.

### Ask sessions

A Slack thread is one Ask session: `ask_slack_sessions(channel_id, thread_ts)`
maps it to a `session_id` on the first mention (`LoadOrCreateForSlackThread`).
A one-to-one DM uses the synthetic thread key `dm`, so the whole DM is one
session while replies stay attached to the triggering message. Follow-ups
replay the persisted history, so the model sees earlier questions, tool calls,
and answers; continuity comes from that transcript, not provider prompt
caches. Channel turns need an @-mention; DMs route every eligible message.
Different threads are different sessions.

To find a session from a Slack permalink such as
`https://example.slack.com/archives/C0123456789/p1700000000000100`, drop the
`p` and put a dot before the last six digits (`1700000000.000100`), then look
up `ask_slack_sessions` by `(channel_id, thread_ts)`. The Ask sessions page
(`/admin/ask/sessions`) takes a pasted permalink and does both;
`internal/adminapi.ParseSlackPermalink` is the Go helper.

Who may end a turn early depends on the surface
([docs/design.md](docs/design.md#how-a-turn-ends)). A `/v1/ask` or MCP `ask`
turn stops when its caller cancels, with the cause `ask.ErrTurnCancelled`
(outcome `cancelled`). A Slack or Discord turn never runs on the context of
the event that asked: only its asker's stop (`ask.ErrTurnStopped`, outcome
`stopped`) or its connector run ending cuts it short. Neither outcome is a
failure; a new way to end a turn is a new cause beside these, read by
`turnEnding` in `internal/ask/service.go`.

### Slack delivery

- With Slack's Delayed Events setting on, Socket Mode redelivers events missed
  during a short disconnect, staggered over minutes; Slack retries for up to
  24 hours. Redelivery is not deterministic.
- The SyncWorker tail sweep is the correctness backstop for messages: gaps
  longer than redelivery covers, anything Slack drops, and the edits that
  redelivery doesn't replay. Of deletes it repairs only a thread root that
  still has replies, which history returns as a top-level `tombstone`; a
  message history no longer returns at all (a reply, or a root without
  replies) stays stored when its delete was missed live. Only redelivery re-triggers Ask for a
  mention posted while the bot was down; `ask_turn_claims`
  (`(channel_id, message_ts)`, claimed before the turn starts) makes that safe
  at any delay.
- Any event-driven side effect needs a non-event backstop. Channel discovery
  through `member_joined_channel` is best-effort; the five-minute reconcile
  loop in the Slack source's Run (`cmd/diderot/slack_connector.go`) that
  polls `users.conversations` is authoritative.
- Ack timeouts (3 seconds) matter only within a connection, where Slack's
  retry is reliable.
- An Ask turn's progress is the thread's agent session
  (`agents.sessions.setStatus`: processing, then active). Slack keeps a
  session processing for an hour and never clears it on a post, so a turn
  records its thread in `slack_working_threads` before setting it and the
  leader's sweep clears rows whose turn died (`agent_sessions.go`). The stop
  button (`agent_session_stopped`) cancels the turn with `ask.ErrTurnStopped`,
  directly when it runs on the leader and through the row when a draining
  replica runs it.
- Slack disables event delivery for an app when 95% of deliveries fail over an
  hour (the bot scaled to zero, or crash-looping). Alert on an hour of zero
  live events. `diderot_slack_socket_retries_total{retry_reason}` separates
  ack-timeout retries from queue replays.
- `member_joined_channel` needs `channels:read`, `groups:read`, and
  `mpim:read` ([docs/slack-app.md](docs/slack-app.md)).
- `slack-app-manifest.yaml` is what installers paste into Slack. Tests pin its
  bot scopes to `requiredBotScopes` and its events and shortcuts to what the
  consumer handles, so a new Slack method or event changes the manifest, that
  list, and the permission table in docs/slack-app.md together.

### Thread summaries

The `Summarize thread` and `Summarize privately` message shortcuts
(`summarize_thread`, `summarize_thread_private` in `slack-app-manifest.yaml`;
`internal/connector/slack/summarize.go`) fetch a thread, collect 📌-reacted
action items, and summarize through the model the Models page
(`/admin/settings/models`) chooses for thread summaries. Without a provider serving
chat, the shortcuts reply "not configured".

- They ride the Socket Mode interactive dispatch: the envelope is acked before
  the handler runs, and the run-state drain waits for accepted handlers at
  shutdown.
- Slack calls go through the shared tiered limiter.
- Output passes through `sanitizeSlackMarkdown` (plain `@word` for a
  recognized mention/broadcast/subteam form; an `&lt;` HTML entity only for a
  malformed `<!`/`<@` opener with no valid id or closing bracket, since
  leaving that raw would still leave the dangerous substring in place), so a
  prompt-injected thread can't make the summary notify a channel.
- Truncation preserves pinned lines at both the fetch cap and the prompt
  budget. A 4-slot semaphore rejects visibly when full, the pipeline has a
  90-second deadline, and posts use the leader-scoped context: never
  `context.Background()`, which would escape cancellation on lease loss, and
  never the 90-second child, whose expiry must not drop a finished summary.
- Threads only: a standalone message or a root with no replies is rejected
  with guidance. They're message shortcuts because slash commands can't run in
  threads.
- Outcomes are counted in `diderot_slack_summarize_total{outcome}`.

### Document ingest

Hosted PDF, DOCX, and XLSX attachments in opted-in Slack channels become
`event_chunks.kind='document_text'` rows.

- Chunks inherit the parent Slack event's permission scope. Each also has
  `event_document_id`, so replacing one attachment can't erase a sibling.
- XLSX and DOCX use local parsers. PDF pages use pdfium text layers; only pages
  under 400 text-layer characters go to the document model.
- Pictures stored inside a DOCX become `event_images` rows of the same event,
  already mirrored, with `event_document_id` set; the caption worker describes
  them. `ReconcileImages` never touches a row with `event_document_id`, and a
  caption chunk does not cascade from its image row: delete it first
  (`events.DeleteDocumentImages`).
- Every paid PDF page is checkpointed before the next begins. Page costs are
  summed per document, and new model work stops at $10. A crash resumes at the
  next missing page.
- State is derived: pending has no `extracted_at`, rejected has
  `rejected_reason`, partial has `incomplete_reason`, and complete has neither.
  There is no `completeness` column.
- Ask waits briefly for referenced documents while workers poll every two
  seconds. There is no document-specific NOTIFY path.
- Enabling `doc_ingest_enabled` always starts history reconciliation by setting
  `doc_backfill_cursor = 0`.
- Manual replay is only the backfill controls on a channel's admin page and
  **Read every channel's documents again** under Operations, Jobs and
  maintenance, which rebuild derived state from stored Slack events.
- Slack's `mode=hosted` decides whether bytes are fetchable. External links,
  snippets, email objects, Canvas, legacy Office formats, and `text/*` are out
  of scope.
- Gateway integration tests must assert document-specific sentinel text. An
  HTTP 200 or a plausible token count doesn't prove the file arrived.

## Auth model

[docs/auth.md](docs/auth.md) is the full reference. The invariants:

- Each surface has its own authenticator (`internal/auth`, assembled in
  `cmd/diderot/serve_auth.go`), and a credential for one surface is never
  accepted on another: a bearer for `/v1` (`DIDEROT_AUTH_MODE` = `oidc`,
  `static`, or `none`), trusted-proxy headers or the built-in OIDC sign-in for
  the browser (`DIDEROT_BROWSER_AUTH`, never both), and trusted-proxy headers
  behind the gateway key for `/mcp`.
- Trusted-proxy headers count only when the immediate peer (`RemoteAddr`, never
  `X-Forwarded-For`) is inside `DIDEROT_TRUSTED_PROXY_CIDRS`. In a cluster, a
  network policy that admits only the proxy to the UI port is still the
  boundary that stops other workloads forging them.
- `none` mode refuses to start unless every listener binds a loopback IP.
- An unverified email is no email: no Slack scopes, session ownership, or
  self-service, never a fallback.
- The `/admin` UI also needs a group in `DIDEROT_ADMIN_GROUPS`; the owner-bound `/v1/me/*` routes and `/sessions` need
  a verified email and a group in `DIDEROT_SELF_SERVICE_GROUPS`, and request
  input can never select another owner. An empty allowlist fails closed.
- Administration is the `/admin` UI alone: no route under `/v1` needs an
  administrator, and `TestMountedRoutes` pins the routes `/v1` has. A new
  administrator capability is a page under `/admin`, behind the browser
  sign-in, CSRF token, and `DIDEROT_ADMIN_GROUPS`.
- A state-changing administrator action is one function in
  `internal/adminaction`, which the `/admin` UI calls and which records it in
  `admin_audit_log`. Handlers don't call the store or write the audit row
  themselves; `TestEveryMutatingAdminRouteWritesOneAuditRow` fails for a
  mutating `/admin` route without an audit case.
- There is no break-glass bearer. If a push-style peer needs access, give it a
  scoped machine identity rather than weakening a human authentication path.
- `/v1/expand-chunk` is a use route: expanding a hit is corpus use, never
  administrator authority.
