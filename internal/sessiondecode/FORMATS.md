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

## `copilot-cli-events-jsonl` (decoder version 1)

GitHub Copilot CLI's `~/.copilot/session-state/<session id>/events.jsonl`
(`$COPILOT_HOME/session-state` when that is set): one session event per
line, `{"type","data","id","timestamp","parentId"}`, plus `agentId` on a
sub-agent's events. GitHub publishes no description of the file itself;
the event shapes are the `session-events.schema.json` inside the Copilot
CLI 1.0.114 release (the `@github/copilot-<platform>` binary unpacks it to
`pkg/<platform>/1.0.114/schemas/` in Copilot's cache directory), and
`github.com/microsoft/copilot-sdk` (MIT) publishes types generated from
the same schema (`go/zsession_events.go`,
`nodejs/src/generated/session-events.ts`; commit `d73005f`, built against
CLI 1.0.115-6). The file layout and everything the schema leaves open were
read from sessions Copilot CLI 1.0.114 wrote (key names only; fixtures are
synthetic).

- **Ids.** Every event has a UUID `id`; ids are `native`. An event that
  becomes one record keeps the bare id. An `assistant.message` that carries
  reasoning text splits into `<id>:reasoning:0` and `<id>:assistant:1`.
- **Active path.** The events in file order. A rewind rewrites the file
  without the rewound events, so the file is always the live branch;
  `parentId` is not followed (side events hang off the chain, and
  interactive model-call events start their own). A sub-agent's events
  share the file under its `agentId` and are skipped; the parent's `task`
  tool call and result carry what it returned. The schema describes log
  rotation: a numbered archive beside `events.jsonl`, whose new head
  replays retained history marked `checkpoint` after a
  `session.journal_link`. The decoder reads the one file it is given and
  keeps checkpoint events; no rotation was seen in real files.
- **Events.** `user.message` is a user record from `content`, which is what
  the person typed (`transformedContent`, the same text wrapped in the
  context the harness adds, is not read); attachments with an `image/*`
  `mimeType` become image placeholders, and other attachments are left
  out. `assistant.message` emits `reasoningText` as a reasoning record, then
  `content` and `toolRequests` (`toolCallId`, `name`, `arguments`: a JSON
  object, or the text of a custom tool's input) as one assistant record
  with `model` and `phase`; `reasoningOpaque`, `encryptedContent`, and
  `reasoningBlocks` are not read. `tool.execution_complete` is a `tool`
  record paired by `toolCallId`, with `result.content` (else
  `error.message`) as text, each `result.binaryResultsForLlm` image as a
  placeholder, and `success: false` marking an error; a shell command that
  exits non-zero still has `success: true`. A completion marked
  `isUserRequested` (a tool the person ran with `!`) answers no model
  request, so it is a `system` record (phase `tool_result`); the
  `tool.user_requested` before it is a user record
  `[<toolName>] <arguments>`. `abort` is a `system` record
  (phase `abort`) naming its `reason`. A successful
  `session.compaction_complete` is a `system` record (phase `compaction`)
  carrying `summaryContent`; history before it stays.
  `assistant.turn_start`, `assistant.turn_end`, `tool.execution_start` (it
  repeats the request), and `system.message` (the harness's system prompt)
  emit nothing; any other `user.*`, `assistant.*`, `tool.*`, or `system.*`
  event becomes a notice. Every other namespace (`session.*` apart from
  the two above, `model.*`, `instruction.*`, `hook.*`, `permission.*`,
  `subagent.*`, `skill.*`, `mcp.*`, ...) is harness state, injected
  context, or telemetry and emits nothing. `session.binary_asset` holds
  image bytes and is never read. An emitted event without an `id`, a tool
  request without a `toolCallId`, or an event without a `type` fails the
  document.
- **Session.** `SourceVersion` is `session.start`'s `copilotVersion`; the
  harness is `copilot-cli`. A session that ended before any conversation
  (a refused model, say) has no records and fails as such.

## `gemini-cli-session-jsonl` (decoder version 1)

Gemini CLI's `~/.gemini/tmp/<project>/chats/session-<started>-<first eight
characters of the session ID>.jsonl` (under `$GEMINI_CLI_HOME` when that is
set). Mapped from Gemini CLI's own writer and loader,
`packages/core/src/services/chatRecordingService.ts` and
`chatRecordingTypes.ts` in `github.com/google-gemini/gemini-cli` at v0.70.0
(commit `8468962`), and checked against files Gemini CLI 0.70.0 wrote (key
names only; fixtures are synthetic). The format has no version field, so
`SourceVersion` is empty. A subagent's transcript
(`chats/<parent session ID>/<session ID>.jsonl`, `kind: subagent`) has the
same format.

- **Log.** The file is a log of edits to the conversation. The first line is
  the session's metadata (`sessionId`, `projectHash`, …). A line with a
  non-empty `id` is a message, and a message written again (a model turn
  gains its tool calls, then their status) replaces the earlier copy in
  place. `$set`
  updates the metadata. `$rewindTo` drops the named message and every
  message after it (all of them when it names none the log holds). `$patch`
  replaces the `content` of the messages it names (`id` and `content`, or
  each of `updates`), removes `removeIds`, and moves `orderIds` to the end.
  Any other line is counted as a warning and otherwise ignored, as Gemini
  CLI's loader ignores it.
- **Active path.** The decoder replays the log as Gemini CLI's loader does
  and emits the messages left, in order. So a rewound turn is gone, and so
  is the history before a compression. Gemini CLI compresses by replacing
  the model's history with a summary turn, an acknowledgement, and the most
  recent turns, all recorded as new messages (the recent turns under new
  ids), and a `$patch` whose `removeIds` drops every message they replace.
  The summarized turns are not emitted: the summary stands in for them.
  This differs from the Claude Code and Codex decoders, which keep the
  history before a compaction on the path. Those harnesses keep the turns
  they carry over as the same entries, while Gemini CLI records them again,
  so bringing back the removed messages would emit each kept turn twice,
  under its old id and its new one. Gemini CLI also removes messages that
  are not in the model's history when it syncs the log with it (after a
  resume or a rewind), such as `info`, `warning`, and `error` messages.
- **Ids.** Message `id`s are `native`. A message that becomes several
  records gets `<id>:<kind>:<n>`, `n` being the record's position among them
  (`reasoning`, `assistant`, `tool_result`, `user`, `system`).
- **Messages.** Content is text, one part, or a list of text and parts.
  In a `user` message each `functionResponse` part is a `tool` record paired
  by its `id`: the `output` and `error` strings of its `response` are the
  text, an `error` marks the result an error, any other response is kept as
  JSON text, and its `parts` are placeholders. The rest is what was typed:
  `displayContent` when present (the content then has `@` references
  expanded), with a placeholder for each image in the content (Gemini CLI
  saves a pasted image to a file and references it with `@`), else the
  remaining parts. Text that starts with
  `<session_context>` or `<hook_context>` is context Gemini CLI injected and
  becomes `system` (phase `context`); text holding `<state_snapshot>` is the
  compression summary and becomes `system` (phase `compaction_summary`). A
  `gemini` message whose content has `thought` or `functionCall` parts is
  read from those parts alone: thought text is the `reasoning` record,
  `functionCall`s (`id`, `name`, `args`) the calls, the rest the reply.
  Otherwise the content is the reply, `thoughts` (`**subject**
  description`) the `reasoning` record, and `toolCalls` the calls. The
  `result` copies inside `toolCalls` are not read: results come only from
  `functionResponse` parts. `info`, `warning`, and `error` messages become
  `system` records with the type as phase; an empty one is skipped. Any
  other message type, inline data that is not an image, and parts of an
  unknown kind become notices. A message of a known type without an `id`, a
  call without an `id`, an emitted message without a timestamp, and a file
  whose metadata has no `sessionId` fail the document.
- **Session.** The model is each message's `model`. `Document.SessionID` is
  the metadata's `sessionId`; `diderot sessions` pushes the session under
  it, since the file name holds only its first eight characters.

## `polytoken-jsonl` (decoder version 1)

Polytoken's `sessions-v1/<session>/log.jsonl` under its data directory
(`$POLYTOKEN_DATA_PATH`, else `$XDG_DATA_HOME/polytoken`, else
`~/.local/share/polytoken`). Polytoken documents where this file lives and
what it is for (`docs.polytoken.dev/using-polytoken/troubleshooting/`,
"Inspecting a session on disk") but not its record format. The event kinds
and fields the decoder reads were taken from session files written by
Polytoken 0.9.10 and by releases up to 0.8.9 (key names only; fixtures are
synthetic). The meaning of a user event's `kind` (`human`, `reiterated`) is
`MessageUserKind` in the 0.9.10 daemon's OpenAPI document (`polytoken print
openapi`).

- **Lines.** A current line is an envelope (`index`, `lineage`,
  `committed_at`, `attribution`, `public_payload`, `message`) whose `message`
  is one session event. Releases up to 0.8.9 wrote the bare event, under
  `sessions/<session>/log.jsonl`, until Polytoken migrates the session on
  resume; such a line is decoded the same way. `public_payload`, the
  daemon's public projection of the same event, is not read.
- **Ids.** An envelope's event is `<lineage>:<index>` (`native`: the lineage
  namespaces the index across rewind, clear and compaction). A bare event is
  `line:<n>` (`positional`). An event split into several records gets
  `<base>:<kind>:<position>` (`reasoning`, `assistant`, `tool_result`).
- **Active path.** The file in order: a rewind removes the later events from
  the file, and a compaction or clear keeps what came before it.
- **Events.** `user` with `kind: human` is a `user` record holding what the
  person typed; Polytoken keeps injected context in events of its own. A
  prompt's images are logged as `image_reference` events with the same
  `prompt_id` just before it and become image placeholders in that record;
  the image's `data` and `path` are never read, and an `image_reference`
  that meets no later user event of its prompt (the copy re-attached after a
  compaction) is dropped. Any other `kind` (`reiterated`: a goal continuation
  or a parent's input to a subagent) becomes `system` with the kind as phase.
  `assistant` splits at each `thinking` or `open_ai_reasoning_opaque` block
  into a `reasoning` record: the thinking block's `text`, or the opaque
  block's readable `summary` when it has one. A thinking block's `signature`
  and an opaque block's `data` are provider continuation state and are never
  read. `text` and `tool_use` blocks merge into one assistant record whose
  tool calls carry the native `id`, `name`, and `input` JSON. `tool_result`
  has one `results` entry per call; each becomes a `tool` record paired by
  `tool_use_id`, with `is_error`, and a cancelled call is an ordinary error
  result. Result content is an object with `text`, `image` (a placeholder;
  its `data` is dropped), or both; any other shape is kept as JSON text
  without inline payloads and counts as a warning. `system_reminder`
  (`reminder.body`) and `tool_exposure_reminder` (`text`) are what the harness
  tells the model, project instruction files included: `system` records,
  phase `system_reminder`. `compaction_fencepost` becomes `system` (phase
  `compaction`) carrying its `summary`; its `continuation_prompt`, which
  repeats the conversation, is not read. `context_cleared` becomes `system`
  (phase `context_cleared`), and `subagent_complete` becomes `system` (phase
  `subagent_complete`) carrying `result_summary`; a subagent's own transcript
  is a separate file (`subagent-*.jsonl`) and is not decoded. Bookkeeping
  emits nothing: `state_update`, `session_lifecycle`, `model_switch_v2`,
  `facet_switch`, `subagent_spawn_v1`/`subagent_spawn_v2` (the launch its tool
  call already shows), `notification` (which a reminder delivers), and
  `classifier_decision_v1`. Any other event kind or block type becomes a
  notice.
- **Model and times.** The model is the envelope's `attribution.model`; for a
  bare event, the `selection.to_model` of the latest `model_switch_v2`. A
  record's time is the event's `emitted_at`, a reminder's own
  `reminder.emitted_at`, or else the envelope's `committed_at`.
- **Session.** The file declares no Polytoken version, so `SourceVersion` is
  empty. A log holding only `session_lifecycle` (a session opened and never
  used) has no records and is rejected. An envelope without `lineage` or
  `index`, an event without `type`, or a mapped event whose fields have the
  wrong type fails the document.

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
