# Agent-session ingest

Diderot indexes transcripts of coding-agent sessions (Claude Code, Codex,
and others) next to Slack and the other sources. Each session is
summarized with map and reduce model calls, and the summaries become
`agent_session` events. Every event carries a record range
(`session_id`, `record_start`, `record_end`), so `expand` can return the
exact transcript records behind a hit.

## Formats

Diderot defines no wire format for agent sessions. It reads formats that
agent harnesses already produce, through one decoder per format, into a
private record model. That model belongs to Diderot and may change; nothing
outside Diderot should depend on it.

| Format name | Source | How it arrives |
|---|---|---|
| `bsky.session.v1` | NDJSON from a session service's feed | The session feed |
| `claude-code-jsonl` | Claude Code's `~/.claude/projects/<project>/<session>.jsonl` | Push |
| `codex-rollout-jsonl` | Codex CLI's `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` | Push |
| `atif-json` | ATIF trajectory (Harbor's Agent Trajectory Interchange Format, v1.8) | Push |
| `hf-sts-jsonl` | Hugging Face Session Trace Simple Format | Push |

ATIF and STS let a harness without a decoder of its own arrive in a format
it may already emit. Their record IDs are `positional` by default (ATIF's
ordinal `step_id`, STS line position); an ATIF producer can carry a stable
per-step ID in the step's `extra.stable_step_id`. `bsky.session.v1` carries
its own owner and revision in its envelope, so it arrives only through the
session feed and cannot be pushed.

`internal/sessiondecode/FORMATS.md` documents, for each format, the source
versions its fixtures cover, how records and their IDs are derived, and
which branch of a session is kept.

### From your own machine

`diderot sessions` ([Ship your sessions](cli.md#ship-your-sessions)) pushes
the Claude Code and Codex sessions on a person's machine, from a background
service that `diderot setup` offers to install. It reads each session file
with the same decoder the server has for that harness, converts the result
to ATIF on that machine, and pushes it as `atif-json`, so the server stores
and decodes ATIF and never receives the harness's own file. Each record
becomes one ATIF step. A record ID that came from the harness travels in the
step's `extra.stable_step_id`, so it is the same ID as a push of the file
itself would give; a positional ID becomes the step's ordinal. The harness's
name and version are the trajectory's `agent`. ATIF has no field for two
things, which are left out: the flag that marks a tool result as an error
(its output is still there), and the phase of a record that is not a system
record. Images are left out as every decoder leaves them out: a pasted
screenshot or an image a tool returned is a placeholder with its media type,
and here its bytes never leave the machine. A tool call whose arguments are not a JSON object (a patch passed as
text) is carried as `{"input": "<the text>"}`.

It follows the [push protocol](#push-protocol) as any client does: the
session's ID is the one in the file's name, a session is created with
`If-None-Match: *`, and one that grew is sent again whole with `If-Match`
naming the revision it replaces. It sends a growing session once its file
has gone five minutes without a write, so that a session in progress is one
new revision each time its agent pauses and not one a minute. It never
sends a last line that has no newline yet: that line goes with the next
push, once it is complete. When the server holds a revision other than the
one `diderot sessions` remembers (its state file was lost, or an answer
was), it takes the current revision from the 412 and replaces that. Nothing
is redacted on the way:
[redaction](#redaction-is-the-producers-job) is the producer's job,
and here the producer is the person whose sessions they are.

### Decoder contract

- **Deterministic IDs.** The same bytes always produce the same record IDs.
- **Native IDs first.** A record's ID comes from the source's own
  identifier for that entry when there is one (a Claude Code line's `uuid`).
  When one entry becomes several records, each gets a suffix from its kind
  and position. A tool call and its output share a pairing key, so neither
  uses that key alone as its ID.
- **Fidelity.** Every ID is marked `native` (the source keeps the identity
  across rewrites) or `positional` (stable only while the source only
  appends).
- **Tool calls and results stay paired** by the native call ID.
- **The active path only.** A decoder emits the live branch of the snapshot
  it was given, without rewound branches or subagent side chains. An
  explicit compaction marker becomes a `system` record. A decoder never
  reconstructs history that is no longer in the file.
- **Content passes through** apart from normalization into the record
  model. Inline image bytes are replaced by a placeholder.
- **All or nothing.** A document that breaks a structural assumption (an
  unparseable line, a tool result with no matching call, a missing ID the
  decoder relies on, a duplicate record ID) is rejected as a whole, never
  partly decoded. A newer source version is decoded rather than rejected,
  as long as it keeps those assumptions.

## Redaction is the producer's job

**Diderot does not redact session content and does not check that it was
redacted.** Everything a decoder receives is stored, summarized, embedded,
and shown to readers who are allowed to see that session. Secrets, tokens,
credentials, and personal data must be removed before a document reaches
Diderot, by whatever captures the session: a session service, an uploader,
or your own tooling.

## Push protocol

Push ingest is off by default. It needs `DIDEROT_SESSION_INGEST_SOURCE=push`
and `DIDEROT_SESSION_PUSH_REVISION_KEY` ([Agent
sessions](configuration.md#agent-sessions)). A pushed session reaches the
corpus only once it is summarized, which takes one more thing: a model
chosen for both session summary jobs on the Models page. Any model that
calls tools will do, on the Anthropic Messages, OpenAI Responses, or Chat
Completions API. Until then a push is still accepted
and stored, but not summarized or searchable, and the Operations Status page
(`/admin/operations`) says what is missing.

An administrator can pause summaries under Operations, Jobs and maintenance
(`/admin/operations/jobs`), to stop their model calls without a restart, and
resume them there. Sessions still arrive while summaries are paused,
and are summarized once they resume. The page says whether summaries run and, if
not, why.

While summaries can't run, the owner's `/sessions` page lists each session
that isn't indexed as `not indexed` instead of `pending`, and says in a
sentence that those sessions are stored but not searchable, and why. For
administrators, the Session feed connector's page says why, and so does the
Session summaries row of the Operations Status page while sessions wait.

A client uploads one session document per request:

```http
PUT /v1/ingest/agent-sessions/{session_id}
Authorization: Bearer <token>
Diderot-Session-Format: claude-code-jsonl
If-None-Match: *
Content-Encoding: gzip

<the session file's bytes>
```

- **Who.** The caller must pass the self-service gate: a verified email and
  membership in `DIDEROT_SELF_SERVICE_GROUPS`. The caller becomes the
  session's owner. The payload can never name an owner. A later push to the
  same session by anyone else gets 403.
- **Session ID.** `{session_id}` must be a canonical UUID.
- **Format.** `Diderot-Session-Format` names the format. The first accepted
  push binds the session to it, and a later push naming another format gets
  409. `Content-Encoding: gzip` is accepted, and every size limit and hash
  applies to the decompressed bytes.
- **Snapshots.** Every push replaces the whole session with the document it
  carries. Send the full file each time, not a diff.

With curl, creating a Claude Code session from its transcript file:

```sh
curl -sS -X PUT "$DIDEROT_API_URL/v1/ingest/agent-sessions/$SESSION_ID" \
  -H "Authorization: Bearer $DIDEROT_TOKEN" \
  -H 'Diderot-Session-Format: claude-code-jsonl' \
  -H 'If-None-Match: *' \
  --data-binary @"$HOME/.claude/projects/<project>/$SESSION_ID.jsonl"
```

### Revisions and preconditions

Each accepted document becomes an immutable **revision**. Its token is
`hmac-sha256:<hex>`, keyed by a server secret, over the owner, the session
ID, the format, and the decompressed bytes. The key means a token cannot be
used to confirm a guess about a session's contents.

| Request | Result |
|---|---|
| `If-None-Match: *` and the session does not exist | 201, created |
| `If-None-Match: *` and the session exists | 412 with the current `ETag`. The body says whether your bytes equal the current revision, so a create retried after a lost response learns that it landed. |
| `If-Match: "<revision>"` naming the current revision | 200, updated |
| `If-Match` with a body equal to the current revision | 2xx; an update retried after a lost response is safe |
| `If-Match` naming any other revision | 412 with the current `ETag` |
| Neither header | 428 |
| Another owner's session | 403 |
| A format change, or a session that the feed owns | 409 |

A successful `PUT` returns `{"revision": "..."}` in its body. `GET` and
`HEAD` on the session URL return the current revision as a strong `ETag`,
and `GET` also returns the stored document.

If the current revision goes A, then B, then A again, a writer still
holding A passes `If-Match`. A push replaces the whole snapshot, so nothing
that B carried is lost.

### Durability and admission

Before it answers, Diderot authenticates the caller, checks the
precondition and the size limit, and decodes the document. Then, in one
transaction, it rechecks the precondition, stores the received bytes and
the decoded records, advances the session to the new revision, and queues
it for summarization. Summaries are produced afterwards.

- **Stored bytes** are split into content-defined chunks and deduplicated
  across a session's revisions, so an append-only transcript costs about its
  final size.
- **Decoded records are immutable.** They are keyed by revision and decoder
  version. A decoder upgrade adds a new materialization rather than
  rewriting the old one, and the stored bytes let Diderot re-decode a
  session without the producer resending it. Citations keep resolving to the
  records they were built from.
- **Retention.** Earlier revisions are kept for the life of the session by
  default, because citations to them have already left Diderot (in Slack
  answers and MCP results). An operator can set a retention window after
  which superseded revisions are pruned.
- **Quotas.** Each owner has a limit on stored bytes and on revisions.
  Exceeding one gets 507.
- **Concurrency.** Uploads run under per-owner and process-wide concurrency
  limits and a bound on bytes in flight. Excess requests get 429 (per owner)
  or 503 (process-wide) with `Retry-After`. They are never queued in memory.

### Feed or push

A deployment takes sessions from exactly one source: the session feed or
push. They cannot run together, and a session written by one source can
never be updated by the other. The source, the revision key, and the push
limits are set under [Agent sessions](configuration.md#agent-sessions).

A feed page can name a session_id whose binding belongs to push (a
database that ran push and was later switched to the feed, or a rolling
deploy that mixes the two). It can also carry a row whose own data fails
validation before it reaches the database: an empty owner, a malformed
source-timestamp pair, a NUL byte or invalid UTF-8 in a text field, an
oversized session_id/owner/client, or a record or user-turn count outside
the database column's range.

Each such refusal depends only on that one change, so it never clears on
retry. The drain logs and skips just that one change, commits the rest of
the page, and advances the cursor past it, so one bad session_id or row
never stalls ingestion for every owner. `reason` is `writer_conflict`,
`invalid_scope`, or `malformed_row` in both the log line and the
`diderot_sessioningest_feed_changes_skipped_total` counter. The binding
check also recognizes a `format_conflict`, but the feed always binds the
same format, so a push-held binding fails the writer check first and the
feed never produces one.

Push never touches a feed session, and the feed never takes over a push
session. A `writer_conflict` skip is that rule being applied. Moving a
session's binding to the other writer is a deliberate operator decision,
which the skip does not make. Deleting a binding's row cascades to that
writer's own state (deleting a push binding deletes that session's
`session_push_chunks`/`session_push_blobs`/`session_push_revisions`), so it
is not something to do routinely from this log line alone.

## The session feed

The feed source polls a session service's commit-ordered change feed
(`GET /api/v1/session-changes`), fetches each changed session's redacted
`bsky.session.v1` revision (`GET /api/v1/sessions/{id}/records/redacted`),
and summarizes it. Expanding a hit fetches the record range from the same
service. The feed's URL, token, and session viewer are set on the
**Session feed** connector page, `/admin/connectors/session_feed`, and the
feed is read while that connector is enabled. An eligibility policy
(`DIDEROT_SESSION_INGEST_POLICY_PATH`) chooses which owners, harnesses, and
apps are indexed. The other settings are in
[Agent sessions](configuration.md#agent-sessions). Summarizing a feed's
sessions takes the same things as summarizing pushed ones
([Push protocol](#push-protocol)).

The feed's API is not specified beyond the endpoints named here, and the
`bsky.session.v1` mapping notes in `internal/sessiondecode/FORMATS.md`.
Unless you run a session service that already serves this API, use push.

The policy's `exclude.sessionIDs` list makes reversible, exact exclusions of
individual sessions. Removing an ID re-admits the session the next time the
policy is applied; the session service's copy is never deleted.

### When the eligibility policy changes

The policy file is read when the server starts. When the policy decides
differently from the one last applied, the server checks every session the
feed has already reported against it: a session it now excludes is removed from
search, one it now admits is queued for a summary, and one whose audience
`ownerOnly` changes is restricted or released. A session it decides as
before is left untouched.

This uses what Diderot already holds about each session, so the feed is not
read again and its position does not move back. New sessions keep arriving
while it runs. It takes seconds for tens of thousands of sessions, plus the
time to remove or summarize the ones that changed, and the server's log has
a line when it starts and one when it finishes, with how many sessions were
checked and how many changed. Upgrading Diderot never triggers more than
that check.

## How a growing session is summarized

A session's transcript is split into chunks of about 30 KiB at record
boundaries, from the start. Each chunk is summarized once by the map model,
and the reduce model writes the session's summary from those chunk
summaries. Both kinds of summary are searchable: a hit on a chunk expands to
that chunk's records, and a hit on the session summary to the whole session.

A session that is still being worked on is summarized again when it has
been quiet for 10 minutes and has grown by at least 2 user turns, 12
records, or 32 KiB, or once it has waited 6 hours, whichever comes first.
An administrator changes these under **When sessions are summarized** on the
same Jobs and maintenance page; a change applies within a minute, with no
restart. The last
revision of a session that has stopped growing is summarized in the same
way, so its summary covers the whole session.

Adding records to a session leaves its earlier chunks unchanged: only the
last chunk grows, and new chunks follow it. A new pass therefore maps only
that last chunk and the new ones, and the reduce model updates the
published session summary from those chunk summaries rather than reading
every chunk's summary again. The cost of summarizing a session grows with
its length, not with the number of times it is summarized.

A session is summarized from the start again only when its earlier content
changed, when it was rewound to fewer records, or when the summary prompts
or the map or reduce model changed. A prompt change made in
the admin UI therefore re-summarizes each session the next time it changes,
not all at once.

### What the map model reads

The map model reads a chunk in a lean form of its records. The stored
records, the chunks, and what `expand` returns keep every record whole; only
the request is smaller, and only by what carries nothing to summarize:

| In each record | The map model reads |
|---|---|
| Its ID, timestamps, and model names | nothing |
| A tool's output | the output, named by the tool that produced it |
| A tool call's arguments | the arguments as JSON, not as escaped text |
| Text of 256 bytes or more that repeats the text of an earlier record of the same role in the session, such as instructions a harness sends again on every call | a note saying so |
| Continuations, call outcomes, and capture notices | their first 160 bytes |
| Everything else | all of it |

Apart from the rows above, content is never cut short: what a session read
and ran is often what it is later searched for. Any single record is already
bounded at 32 KiB when the session is read.

A chunk summarized under different rules (by another version of Diderot)
keeps its summary: the rules apply to chunks the map model reads from then
on, and changing them re-summarizes nothing.

### Repairing summaries

The summary model sometimes writes a session summary's search text and
context line inside its narrative, after a stray `</narrative>`, and leaves
the fields they belong in empty. Diderot moves them back to their fields
before saving a summary. Summaries saved without that step can be repaired
under Operations, Jobs and maintenance, **Repair session summaries**, which
says, before it starts, how many summaries written in the last 30 days need
it. A
summary whose fields are all in its own text is put right with no model
call; one whose fields cannot be recovered is written again from the chunk
summaries already stored, with one call to the session summary model. Each
repaired session is published again and its search text indexed again.
Running it again repairs only what is still broken. A session with a newer
revision waiting is left to its next summary, which reads the earlier
summary's text in its repaired form.

## Indexing failures

A session revision that fails indexing leaves the oldest-first batch instead
of re-occupying it every tick. It is tried five times in all, the same
budget an image caption or a document gets: after a failure it waits 10
minutes, then 20, 40 and 80, and the fifth failure parks it. Nothing tries a
parked revision again until the session changes or an administrator retries
it, so a session the model refuses is not sent to it for ever. A revision
larger than `DIDEROT_SESSION_INGEST_MAX_REVISION_BYTES` is parked at once,
because revisions are immutable; the feed rejects it before download when
the feed's `content_bytes` already exceeds the cap. A revision with no
records is parked at once too: it has nothing to summarize and never will.
A new revision clears both states.

Failing and parked sessions are listed under **Session summaries** on the
admin Failures page (Operations > Failures), with the cause and when each
last failed. For a cause that retrying can get past (a provider outage that
outlasted the retries, for example), **Retry** there puts that cause's
parked sessions back. Parked sessions are not counted as waiting on the
Status page, the session connector's page, or an owner's My sessions page.
`diderot_sessioningest_backoff_sessions` and
`diderot_sessioningest_quarantined_sessions` count them, and
`session_ingest_candidates.index_last_error` records why. To put back a
revision the page offers no retry for (after raising the size cap, say):

```sql
UPDATE session_ingest_candidates
SET index_quarantined_at = NULL, index_retry_at = NULL, index_failure_count = 0
WHERE session_id = '<uuid>';
```

## Visibility

An indexed session is visible to every authenticated user (scope
`internal`) unless it is owner-only (scope `owner:<email>`). That holds for
a pushed session too, whoever pushed it and with whatever client. On a
server that only you can sign in to, every authenticated user means only
you. The `/sessions` page calls
owner-only **Only me**. An owner can
make individual sessions owner-only on the `/sessions` page, or through the
`/v1/me/agent-sessions` and `/v1/me/agent-session-privacy` routes. The
eligibility policy's `ownerOnly.principals` list makes every session of the
listed owners owner-only.
