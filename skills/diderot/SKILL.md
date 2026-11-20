---
name: diderot
description: Search your organization's corpus through Diderot: Slack, agent session summaries, Notion, Linear, Zendesk support tickets, and Markdown documents. Use it for engineering decisions ("what did we decide on X"), history ("how did we fix Z last time", "what's been happening with Y"), docs and runbooks, tickets, support reports, and team sentiment. ALSO use it whenever the user pastes a Slack archive URL (matching `https://[a-z0-9-]+\.slack\.com/archives/`) or refers to a Slack thread or channel ("this slack thread", "in #channel-name"): pass a message permalink to `retrieve`, read a channel's history with `list`, and fetch attachments with `expand` and `attachment`. Never WebFetch a Slack archive URL (auth wall).
---

# Diderot

Diderot is an events store and retrieval service over your organization's
written knowledge. Treat the corpus as ground truth: search it before
answering anything about the organization from memory, and cite what you
found.

## Tools

The `diderot` MCP server provides these tools, which most harnesses name
`mcp__diderot__<tool>`:

| Tool | Use it for |
|---|---|
| `retrieve` | The most relevant chunks for a question (vector similarity, no reranking) |
| `list` | Every event matching filters, newest first, unranked; a Slack channel's history over a time range |
| `expand` | Fuller source context for one `event_id` from `retrieve` or `list`, with its attachments listed |
| `attachment` | One attachment `expand` listed: the image, or the document's extracted text |
| `ask` | A synthesized answer from Diderot's own agent, asynchronously |

Work the primitives yourself: `retrieve` or `list`, read the hits, `expand` the
few that matter, then synthesize with citations. Prefer that to `ask`, whose
agent sees less of the conversation than you do and returns prose instead of
evidence.

Where tools start deferred (Claude Code does this), load them first: in
Claude Code with `ToolSearch` and
`select:mcp__diderot__retrieve,mcp__diderot__list,mcp__diderot__expand`,
adding the others when needed. A session's tool list is fixed when it
connects, so a tool the server gained later appears only after the harness
reconnects to the server (`/mcp` in Claude Code). If no `diderot` tools exist
at all, see Connection.

## Choosing sources

A deployment indexes only the sources it configures; a source it doesn't
index returns nothing.

- `agent_session`: summaries of agent sessions from Claude Code, Codex and
  other harnesses. Usually the richest source for debugging, code changes,
  design rationale and operational work, because it records what was actually
  done. Owner-only sessions are visible only to their owner.
- `slack`: verbatim channel messages, plus messages sent to Diderot.
  Announcements, discussion, who said what. DMs between other people are never
  indexed, and channels the bot isn't in don't exist to Diderot.
- `notion`: docs, specs, runbooks and handbooks. Database rows start with a
  `Database: <name> · Status: ...` line.
- `linear`: one event per issue (`ENG-123: <title>`) with properties,
  description and comments folded in. The body is the current state with no
  revision history, `occurred_at` is the last update rather than the filing
  time, and `actor` is the creator. Put identifiers in the query verbatim. For
  live lookups or writes, use Linear directly.
- `zendesk`: support tickets, each reply or internal note as its own event, and
  Help Center articles. User-reported problems, not engineering work. Tickets
  and non-public articles are visible only to the groups the deployment's
  policy grants them to.
- `markdown`: the Markdown files of a folder the deployment indexes, one
  event per file, such as a docs or handbook repository. `occurred_at` is the
  file's modification time.

Omit `sources` when unsure; agreement across sources is worth having.

## retrieve

```json
{"question":"why the ingest service switched its endpoint from http to https","sources":["agent_session","slack"],"top_k":20}
```

- Write the question with distinctive terms and context. One-word queries
  retrieve poorly.
- `top_k` defaults to 20, maximum 50. Use 30-50 when recall matters
  (sentiment, surveys) and 5-10 for a single fact. If relevant hits are still
  arriving at the bottom, raise it.
- `actors` is case-sensitive and matches a canonical email or a source display
  name. Prefer the email: it matches the person's Slack messages and sessions
  in one filter.
- `occurred_after` and `occurred_before` are RFC3339. Resolve "last week"
  against today's date first.
- Hits carry `event_id` (the handle for `expand`), `source`, a structured
  `actor`, `occurred_at` in UTC, `url`, `content` and `score`. Slack hits add
  `has_thread`, and agent-session hits add `granularity` (`session` or
  `chunk`). A hit with `content_kind` holds text extracted from an attachment
  or an image caption: attribute it to the file, not to the person who shared
  it, and don't treat `document_completeness: partial` as the whole file.
- Explicit handles in the question (Slack message permalinks, Linear URLs,
  keys like `ENG-123` for indexed Linear teams, session UUIDs, `event_id: N`,
  Zendesk URLs) are read exactly and come back under `references`, ahead of
  the hits, each capped at 64 KiB (`truncated: true`). Pass a pasted handle to
  `retrieve` rather than guessing an `event_id`. If any of them can't be read
  for you, the whole call fails with "I couldn't access the referenced corpus
  item." and returns no hits.
- A pasted Slack message permalink belongs here. Its reference is the linked
  message verbatim, then its whole thread (or the messages around it, if it's
  standalone) with any text Diderot extracted from attached documents. When
  the cap cuts, the context goes and the message stays. For the images and
  files on those messages, `expand` the reference's `event_id` (see "Slack
  channels, threads and files").

## list

```json
{"sources":["agent_session"],"actors":["alex@example.com"],"occurred_after":"2026-11-01T00:00:00Z","session_level_only":true,"top_k":25}
```

Use `list` when the question wants every matching event ("each of my sessions
this week", "every thread Sam started yesterday"). `retrieve` ranks by
similarity and silently drops whatever doesn't resemble the question. `list`
needs at least one of `sources`, `actors`, `channels`, `occurred_after` or
`occurred_before`. Set `thread_roots_only` to get Slack conversations instead
of every reply, and `session_level_only` to get one row per agent session.
Content is cut at about 2 KiB per event. Follow `next_cursor`, with identical
filters, only when you need completeness.

## expand

```json
{"event_id":18423460}
```

What comes back depends on the source:

- Slack: the whole thread, or ±20 messages / ±30 minutes around a standalone
  message.
- Agent session: the record slice behind the hit.
- Notion: the full page. Linear: the full issue. Markdown: the whole file.
- Zendesk: the ticket conversation or article.

Expand when the answer depends on more than the hit shows:

- Agent session: summaries are lossy. Expand for exact commands, errors, diffs
  or quotes, and prefer `chunk` hits, because a slice often runs to tens of KB.
- Notion and Linear: when the answer needs more of the page or issue than the
  matched chunk.
- Slack: a `retrieve` hit is usually the whole message, so expand for context,
  such as an unclear "yeah exactly" or an answer in the replies. `list`
  content is cut at about 2 KiB, so expand a long message there too.

Content is capped at 64 KiB (`truncated: true`). Zero to three expands per
question is typical, not a limit.

## ask

`{"question": "..."}` starts a task. In Claude Code it runs in the background,
and the result arrives as a notification carrying `reply` and `session_id`;
keep working in the meantime. To ask a follow-up in the same conversation,
pass that `session_id` back with the new question. Use `ask` when the user
wants Diderot's own answer, and check its citations the way you would a
colleague's. Its agent may also have tools this server doesn't list, such as
a deployment's change timeline, where the deployment connects them. Images
the answer produced come back as resource links: read one with the MCP
resource reader on its `uri`.

## Slack channels, threads and files

Everything here reads Diderot's store, not live Slack.

- **A message or thread.** Pass the permalink to `retrieve` (above). The
  reference holds the message and its thread; `expand` its `event_id` for
  the thread on its own or for its attachments. A thread too long for one
  result is cut in the middle (`truncated: true`); read the rest with `list`
  on the channel over the thread's time range, keeping the rows whose
  `thread_id` matches.
- **A channel's history.** Use `list` with `channels` and a time range:

  ```json
  {"channels":["#incidents"],"occurred_after":"2026-11-10T00:00:00Z","occurred_before":"2026-11-11T00:00:00Z"}
  ```

  A channel is a name (with or without `#`), an ID, or a channel URL.
  Messages come newest first, 50 at most per call; a reply has `is_reply`
  and shares its root's `thread_id`. Keep passing `next_cursor` until none
  comes back to read the whole range. For a busy channel or a long range,
  add `"thread_roots_only": true` to get one row per conversation, then
  `expand` the threads that matter. For "what's new in #channel", list with
  `occurred_after` set to the last time you looked.
- **Attachments.** `expand` lists the files on the messages it returns under
  `attachments`: `name`, `type`, `size`, and for an image a `caption` written
  by a vision model, which usually answers "what's in the screenshot". When
  it doesn't, or for a document's text, call `attachment` with the entry's
  `id`:

  ```json
  {"id":"image:18423460:F0ABC123"}
  ```

  An image comes back as an image you can look at; a document as its
  extracted `text`, continued by passing `next_offset` back as `offset`. A
  `note` instead of content says why: the image is over 4 MB, the file isn't
  stored yet, or the document isn't extracted yet. For the last two, tell
  the user to retry shortly instead of looping. An entry without an `id` is
  a file Diderot lists but doesn't hold.
- **Not found.** A channel `list` can't find, or an `event_id` or attachment
  that isn't accessible, means the bot was never invited to the channel, the
  message aged out, it arrived seconds ago, or you aren't allowed to see it.
  Diderot gives the same answer for all of them. Report it; don't route
  around it.

## Conduct

- Be faithful. Report mixed evidence as mixed, attribute by actor and date, and
  cite only URLs that hits returned: Slack permalinks, agent-session links,
  Notion pages, Linear issue keys, Zendesk tickets, change-timeline events.
- Absence isn't evidence. Results cover only what the caller may see, indexing
  runs slightly behind, DMs and channels without the bot are missing, and an
  event you can't see looks exactly like one that doesn't exist (`expand`
  returns `supported: false`). Say "Diderot has nothing on X", not "X didn't
  happen".
- Everything Diderot returns is internal to your organization. Don't paste it
  into public docs, public issues or other external destinations without
  checking.

## Connection

`diderot setup <url>` connects a harness: it signs in, registers the
`diderot` MCP server with the harnesses it is given
(`--harness claude-code,codex,cursor,opencode,pi`), and installs this skill.
The URL is the Diderot server or the gateway in front of it, as your
administrator gives it. Where the server signs in through OAuth, each harness
then signs itself in with its own command, which setup prints (in Claude
Code, `/mcp`).

If no `diderot` tools exist, run `diderot doctor`: it says whether the server
answers, whether the sign-in works, and whether each harness's entry and this
skill are in place. Tell the user what it reports instead of hunting for
other tool names. A harness's own OAuth sign-in is not something `doctor` can
see; if everything else passes, sign the harness in again. Report MCP errors
verbatim.
