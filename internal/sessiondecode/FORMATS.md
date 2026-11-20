# Session format mapping notes

Each decoder maps one existing agent-session format onto diderot's private
record model (`Record`). The contract is in `decoder.go`. Every decoder keeps
fixtures under `testdata/<format>/` for the source versions below; a document
from a newer version is decoded, not rejected, unless it breaks one of the
structural assumptions listed here. The source version each document declared
is recorded on every stored revision.

Common rules:

- Records need an RFC 3339 timestamp; an emitted entry without one fails the
  document.
- Inline image bytes never reach a record. An image becomes
  `{"type":"image","media_type":…,"available":false}`.
- Content a decoder does not recognize becomes a text notice and counts as a
  warning record; it does not fail the document. A structured tool result
  kept as JSON text has `data:` URLs and `data`/`base64`/`bytes` strings
  replaced by a notice.
- A tool result whose call id was not emitted earlier fails the document.
- Duplicate record ids fail the document.

## `bsky.session.v1`

A session feed's sanitized NDJSON (Bsky Session v1). Line 1 is the envelope;
each later line is one record with a contiguous `index` and a stable `id`
(`native`). The session feed validates through `DecodePinned`, which pins
`session_id` and `revision` and keeps the feed's historical acceptance
(it does not check id uniqueness or tool pairing). `Decode` adds both.

## `claude-code-jsonl` (decoder version 1)

Claude Code's `~/.claude/projects/<project>/<session>.jsonl`. Verified
against the entry shapes of Claude Code 2.1.92–2.1.323 (key names only;
fixtures are synthetic).

- **Ids.** Every conversation entry has a `uuid`; ids are `native` because
  Claude Code never edits an entry. An entry that becomes one record keeps the
  bare uuid. An entry split into several records gets
  `<uuid>:<kind>:<block index>` for each part (`reasoning`, `assistant`,
  `user`, `system`, `tool_result`).
- **Active path.** The leaf is the last entry in file order that has a `uuid`
  and is not `isSidechain`. The path follows `parentUuid` back to the root;
  at a `compact_boundary` (null `parentUuid`) it follows `logicalParentUuid`,
  so compacted history still in the file stays on the path. Rewound branches
  and sidechain (subagent) entries are not on the path. A parent missing from
  the file ends the path; a parent cycle fails the document. When a uuid
  appears twice (a resumed session rewrites entries), the later line wins.
- **Entries.** `user` with string content is one user record. Array content
  splits at each `tool_result` block, which becomes a `tool` record keyed by
  `tool_use_id`; adjacent text and image blocks stay together. `isMeta` user
  entries become `system` records with phase `meta`; the `isCompactSummary`
  entry becomes `system` with phase `compaction_summary`. `assistant` content
  splits at each `thinking` block (a `reasoning` record); text and
  `tool_use`/`server_tool_use` blocks merge into one assistant record whose
  `tool_calls` carry the native `id`, `name`, and raw `input` JSON.
  `redacted_thinking` is dropped. `system` entries with string content become
  `system` records with the subtype as phase; `compact_boundary` is always
  emitted (phase `compaction`). `attachment` and `progress` entries carry the
  chain but emit nothing; any other entry type on the path becomes a notice.
  A `user`, `assistant`, or `system` entry without a `uuid` or `message`
  fails the document.
- **Session.** `SourceVersion` is the last `version` on the path; started and
  updated times are the earliest and latest record timestamps.

## `codex-rollout-jsonl` (decoder version 1)

Codex CLI's `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`, the wrapped form
where every line is `{"timestamp","type","payload"}`. Verified against the
item shapes of Codex CLI 0.146.0–0.164.0 (key names only; fixtures are
synthetic). The pre-wrapper rollout form is rejected.

- **Ids.** A response item's `id` (`msg_…`, `rs_…`, `fc_…`, `fco_…`) is used
  when present (`native`). Otherwise a call takes `<call_id>:call` and an
  output `<call_id>:output` (`native`; `call_id` is a pairing key, so the two
  sides never share an id). Anything else is `line:<n>` (`positional`).
- **Active path.** A rollout is linear; every `response_item` is on it.
  `event_msg` lines duplicate items for the UI and are ignored, as are token,
  state, and settings lines. A `compacted` line becomes a `system` record
  (phase `compaction`) carrying its summary message; its
  `replacement_history` is not re-emitted, and items before it stay.
- **Items.** `message` maps `user`/`assistant` roles directly and
  `developer`/`system` to `system` (the role kept as phase). `reasoning`
  emits its summary and reasoning text, and is skipped when only encrypted
  content is present. `function_call`, `custom_tool_call`, and
  `local_shell_call` become assistant records with one tool call keyed by
  `call_id` (a `namespace` prefixes the name); `*_output` items become `tool`
  records paired by `call_id`, with an object output's `success:false`
  marking an error. `web_search_call` is an assistant record. `agent_message`
  becomes `system` (phase `agent_message`). Unknown item types become a
  `system` notice. The model comes from the latest `turn_context`.
- **Session.** `SourceVersion` is `session_meta.cli_version`.

## `atif-json` (decoder version 1)

Harbor's Agent Trajectory Interchange Format, one JSON document per run.
Pinned to the RFC at
`github.com/harbor-framework/harbor/blob/main/rfcs/0001-trajectory-format.md`
as of 2026-11-12 (current `ATIF-v1.8`). Any `ATIF-v1.*` `schema_version` is
accepted; the one breaking change inside 1.x (v1.7's `SubagentTrajectoryRef`
resolution key) does not touch this mapping, because refs are carried as
text, not resolved.

- **Ids.** `positional` by default: `step:<step_id>`. A producer may put a
  stable per-step id in `step.extra.stable_step_id` (a string); that id is
  then used and marked `native`. A step split into several records gets
  `<base>:<kind>:<part index>` (`reasoning`, `assistant`, `observation`, …).
- **Steps.** `step_id` must be the ordinal 1, 2, 3, …; anything else fails
  the document. `source: user` → `user`; `system` → `system`, with
  `extra.context_management.type` (`compaction`, `pruning`, `injection`) as
  the phase, so a compaction is a `system` record; `agent` → a `reasoning`
  record for `reasoning_content`, then an assistant record carrying
  `message` and `tool_calls` (`tool_call_id`, `function_name`, compacted
  `arguments`). Each `observation.results[]` entry becomes a `tool` record
  paired by `source_call_id` (it must name an earlier call), or a `system`
  record (phase `observation`) when it names none; a `subagent_trajectory_ref`
  is kept as text. Content may be a string or ContentPart array: `text`
  passes through, `image` becomes an image placeholder (the `source.path` is
  not followed), `audio` and unknown parts become notices.
- **Active path.** The steps in order. `subagent_trajectories` and
  `continued_trajectory_ref` are not followed; `is_copied_context` steps
  are kept.
- **Times.** `timestamp` is optional in ATIF. A step without one takes the
  latest earlier timestamp (the first steps take the first one); a
  trajectory with no timestamp at all fails. Times are normalized to UTC.
- **Session.** `SourceVersion` is `schema_version`; the harness is
  `agent.name`, else `atif`; the model is the step's `model_name`, else
  `agent.model_name`.

## `hf-sts-jsonl` (decoder version 1)

Hugging Face's Session Trace Simple Format, pinned to
`huggingface.co/docs/hub/en/session-traces-format` as of 2026-11-12. The
format has no version field, so `SourceVersion` is empty.

- **Ids.** Always `positional`: `line:<n>`, the 1-based line number. Message
  lines have no id and no documented extension slot. An assistant message with
  `reasoningContent` splits into `line:<n>:reasoning:0` and
  `line:<n>:assistant:1`.
- **Lines.** Line 1 must be the header (`type: session`, `harness`, `id`);
  `harness` becomes the session's harness. Every later `type: message` line
  maps `role` directly (`user`, `assistant`, `system`, `tool`);
  `toolCalls[].id` / `function.name` / `function.arguments` become tool calls
  and a `tool` message's `toolCallId` pairs it. Content is text (an array of
  text blocks is accepted leniently). Other line types and roles become
  notices.
- **Times.** `timestamp` (epoch milliseconds) is optional and filled as for
  ATIF; a session with no timestamp fails.
