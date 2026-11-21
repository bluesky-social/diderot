# Prompts

Every instruction Diderot sends to a model is a named prompt in one catalog.
The catalog holds the Ask agent's role and research policy, how it writes
for Slack and for the API, MCP and `diderot ask`, the observability
investigator, session summaries, titles, document extraction, image
captions, and scheduled report templates. Tool descriptions
and schemas are the exception: they stay in Go beside their tools, and the
Prompts page's **Tools Ask can use** tab shows them as the model reads them.

The catalog lives in the database. Each version of it is an immutable
**release** (the admin UI calls it a version), and one release is active
(live) at a time. You change prompts in the admin UI.

## The example

Diderot ships an example catalog in [`prompts/`](../prompts/), written for
no organization in particular: it fills in your organization's name and
sources through [variables](#variables) and uses made-up channels, people,
and services in its examples. On first boot, with no releases in the
database, Diderot stores it as release 1 and activates it. The admin UI
marks it **example**.

Read through the example before going live and edit what doesn't fit your
organization. What is true only of your organization belongs in the
[organization note](#organization-note) or on the connection it concerns,
not in the shipped prompts: then every improvement a new version ships can
be taken as it is.

An install with an active release is never reseeded: a new Diderot version
with a changed example stores nothing, and your prompts stay as you left
them. The Prompts page says how many prompts differ from the text the
running version ships, and **Shipped text** lists them with what taking
the shipped text would change: prompts whose text differs, prompts new in
this version, and prompts it no longer uses. Tick some or all of them to put
the shipped text in the [draft](#changing-prompts).

## Changing prompts

`/admin/prompts` lists every prompt, grouped by what it is for, with
whether its text is the shipped text, when it last changed, and by whom.
Every change goes through one **draft**, shared by all administrators, and
goes live in one step:

1. Open a prompt, change its text, and choose **Save to draft**. Diderot
   checks the text (only the [organization note](#organization-note) may
   be empty, and every variable must be a known one) and keeps it in the
   draft. Nothing changes for anyone yet.
   Save as many prompts as the change needs; **Shipped text** and **Put
   this text in the draft** on an earlier version add to the same draft.
2. Open **Draft**. It shows each change against the live text, line by
   line, and the text as the model receives it, with its variables filled
   in. It also says what going live redoes: a change to the agent-session
   summary prompts has each session summarized again the next time that
   session changes. The page estimates that work from the sessions
   summarized in the last seven days.
3. Add a note saying what changed and why, and choose **Make the draft
   live**. Diderot stores the live catalog with the draft applied as one new
   release, activates it, and empties the draft. Conversations that start
   from then on use it.

Saving, discarding, and making the draft live each apply only to what the
page showed.
If someone else changed the draft or the live prompts in the meantime,
nothing changes and the page says so: a prompt's page keeps your text so you
can copy it, and the draft page asks you to look again. If the live text of
a prompt changes after its change was saved to the draft (someone made an
older version live), that change is marked out of date and the draft cannot
go live until you save it again or discard it.

**Discard this change** on a prompt's page drops its change from the draft;
**Discard the draft** drops every change.

Only administrators (`DIDEROT_ADMIN_GROUPS`) can open these pages. Every
change to the draft and every activation is recorded in the audit log
(`/admin/operations/audit`).

## Organization note

**About your organization** (`ask.organization`) holds what Ask needs to
know about your organization that no shipped prompt can say: internal sites
and tools, and where to look for what. Ask adds it right after **Role and
research**. Diderot ships none: until you write one, nothing is added. It is
edited and goes live through the draft like any other prompt. Saving it
empty removes it, and **Shipped text** never lists it.

Facts about a connection belong to that connection, not to a prompt: the
Grafana datasources the observability investigator queries are pinned on
its Grafana [tool connection](mcp-servers.md#kinds), and a Sentry connection
is already limited to one organization and project.

## History and revert

Below the editor, **Version history** lists each time that prompt's text
changed, newest first, with who made it live, when, and the note. Opening a
version shows what it changed, line by line, against the text before it, and
**Put this text in the draft** copies it into the draft. A prompt's removal,
such as an emptied organization note, is not listed there; the **Version
history** tab shows it.

The **Version history** tab lists every change of the live catalog and the
prompts each one changed. Opening a version shows what it changed against
the version it replaced; **Make version N live** there rolls back every
prompt at once, without going through the draft. If the draft changes
prompts whose text is different in that version, the version's page says
how many of those changes will be marked out of date.

## Variables

A prompt can contain `{{NAME}}` placeholders, filled in on every model
request:

| Variable | Value |
|---|---|
| `{{ORG_NAME}}` | The organization's name, from `/admin/settings/organization`. Until one is saved, "the organization". |
| `{{ORG_DESCRIPTION}}` | The one-line description from `/admin/settings/organization`. Empty until one is saved. |
| `{{MAX_TITLE_RUNES}}` | The longest title, in characters, that the title prompts may ask for (72). |
| `{{SOURCES}}` | One Markdown list item per source this build indexes, enabled or not: its `sources` filter value and what it holds, the same sentence the `retrieve` tool's `sources` parameter gives. The example's base prompt lists the corpus with it. |
| `{{SLACK_WORKSPACE_NAME}}` | The Slack workspace's name, which the Slack connector's Test finds (`/admin/connectors/slack`). Empty while Slack is disabled. |
| `{{GRAFANA_DATASOURCES}}` | One line naming the Grafana datasources the observability investigator's queries always use, from the **Datasources** pinned on the Grafana [tool connection](mcp-servers.md#kinds). Empty when none is pinned. The example's Grafana rules begin with it. |

A connector field can be a variable, as `{{SLACK_WORKSPACE_NAME}}` is; its
connector page says so. A secret never is. While its connector is disabled,
a variable has the field's default, usually empty, so word prompts to read
well without it.

The list is fixed. Saving a prompt that uses any other
placeholder fails, including near-misses such as `{{ ORG_NAME }}` or
`{{org_name}}`. Other text with braces, such as a Helm `{{ .Values.x }}`
example, is left alone.

Filling in is plain text replacement in one pass: there are no conditionals
or loops, and a value is never itself scanned for placeholders. Write each
sentence so it still reads well with the default value. The example says
"{{ORG_NAME}}" wherever it names the organization.

## Pinning

An Ask conversation (a Slack or Discord thread or DM, a `/v1/ask` session,
or an MCP `ask` task) pins the release that was active on its first turn. Follow-up turns and
the observability investigator use that release, so an edit never changes
instructions halfway through a conversation; it applies to conversations
that start after it. Variables are filled in on every turn, so a changed
organization name applies from the next turn.

Work outside Ask conversations (session summaries and titles, document
extraction, image captions) uses the active release. Each
replica picks up a newly activated release within about ten seconds.
