# Adding a source

A source is one place Diderot indexes: Slack, Notion, Linear, a folder of
Markdown files. Adding one means writing a Go package that implements
`source.Source` (`internal/source`) and adding one line to the registry in
`cmd/diderot/sources.go`. Everything else reads the registry: the
connector's admin page and its test, the setup checklist, the supervisor that
runs the source on the leader, the `retrieve` and `list` tools' `sources`
filter, the `{{SOURCES}}` prompt variable, actor identity, and `expand`.

Extension is at compile time. There are no plugins and no compatibility
promise for these Go interfaces: fork or vendor the repository, add your
package, and build.

This guide covers what a source does, the interface method by method, what
the rest of the system hands it, the rules every source keeps, and how to
register and test one. It ends with a walk through the Markdown folder source
(`internal/connector/markdown`), which is written only against
`internal/source` and is the one to copy.

## What a source does

A source turns the items of some system into **events**: one row per item in
Postgres, unique on the source's event-source name and an external ID that
you choose and keep stable. It writes them through the event sink
(`internal/events`), which chunks each event's body. The embedder embeds the
chunks into Qdrant in the background, and retrieval searches them, filtered
by each event's scope.

Diderot runs as one or more replicas. Every replica serves the admin pages,
so a source's settings, test, status, and expand run anywhere. Its ingest
runs only on the leader, and only while an administrator has enabled it.
The settings supervisor calls the source's `Run` with the saved settings.
When the settings change, or the source is disabled, or the replica loses
the leadership, the supervisor cancels `Run`'s context, waits for `Run` to
return, and starts it again if it should still run.

## The interface

```go
type Source interface {
	settings.Definition // Name, Fields, Validate, Test

	Info() Info
	Run(ctx context.Context, deps Deps, s settings.Resolved) error
	Expand(ctx context.Context, deps Deps, eventID int64, scopes []string) (Expanded, error)
}
```

### `Name() string`

The source's connector name: its row in `connector_settings`, its page at
`/admin/connectors/<name>`, its backfill type, and the name its state and
metrics are kept under. Lower-case letters, digits, and underscores. Never
change it once the source has been enabled anywhere.

### `Fields() []settings.Field`

The settings an administrator edits on the source's page. Each field has a
key, a label, help text, and a kind: `text`, `bool`, `enum` (with
`Options`), `list`, `int`, `duration`, or `secret`. `Default` is the value
of a field nobody has set; `Required` refuses to save it empty.

A secret field is never shown again once saved, and never logged. It can be
pasted on the page, where it is stored encrypted, or set by the deployment
variable `DIDEROT_<NAME>_<FIELD>` (`settings.EnvName`), which wins. A secret
field can't have a default or be a [prompt variable](prompts.md#variables).

Keep anything that reaches into the server's own machine off the page. A
path typed into a form would let an administrator read any file the process
can; make it a deployment variable instead, as the Markdown folder does.

### `Validate(settings.Values) error`

Checks the non-secret values after they are parsed to their kinds, and may
normalize them in place: what `Validate` leaves is what is saved. Return
`settings.FieldErrors` to put a message under a field. It never sees
secrets; whether a token works is `Test`'s job.

### `Test(ctx context.Context, s settings.Resolved) settings.TestReport`

One bounded, read-only pass against the system with the form's settings,
saved or not: does the token authenticate, does the folder open, how much
is there. It has 30 seconds and writes nothing. Report it as checks, each
with a short `Detail` and, when it failed, a `Remedy` that says what to fix.
Never echo a response body or a secret. **Turn on** (**Save and turn on**
before the first save) saves the page's settings and turns the source on
only when the test of those settings passes.

### `Info() Info`

Fixed facts about the source:

| Field | What it is |
|---|---|
| `EventSource` | The `events.source` value of its events, such as `linear`. It appears in stored rows, retrieval filters, and the tools' `sources` enum, so it never changes once used. |
| `Title` | What pages call it: "Markdown folder". |
| `Summary` | What it indexes, in a few words, under its name on the connectors list and its page: "Markdown files in a folder the deployment mounts." |
| `ResultNoun` | The progress line's name for one of its results, as in "Loaded source context from Linear". |
| `ToolDescription` | One line saying what its events are. It becomes part of the model's tool surface: the `retrieve` and `list` tools' `sources` descriptions and the source's line in `{{SOURCES}}`. Write it for a model choosing where to search. |
| `ActorRole`, `IdentityNamespace` | How its events' stored actors are projected into the identity contract (`internal/identity`): what the actor did (`identity.RoleIssueCreator`, ...), and the namespace of the actor's ID in the source. Set both, or neither for a source whose events have no actor. |

`source.NewRegistry` panics on a missing title, result noun, summary, or tool
description, on a description that is not one trimmed line, on only one of
the two actor fields, and on a name or event source already registered.

### `Run(ctx context.Context, deps Deps, s settings.Resolved) error`

Holds every worker of the source for one leader term, with these settings,
until `ctx` ends. `s` has every non-secret field, defaults filled in, and
every secret's value.

- Return only after every goroutine `Run` started has stopped. Return `nil`
  when `ctx` ends.
- Return an error only when the source can't start at all, such as missing
  credentials. The supervisor logs it, counts it, and starts `Run` again a
  minute later. An error in one pass of a worker is not a reason to return:
  log it, count it, and try again at the next pass.
- Keep nothing from `deps` or `s` after returning. The next `Run` gets its
  own.

### `Expand(ctx context.Context, deps Deps, eventID int64, scopes []string) (Expanded, error)`

The fuller context of one of the source's events, for the `expand` tool:
the whole document, the whole thread, the issue with its comments. Before it
calls `Expand`, the dispatcher has checked that a reader with `scopes` may
see the event itself; if you return anything else, such as neighbouring
messages, check each against `scopes` yourself. Return `source.ErrNotExpandable`
when there is nothing more to show, and `events.ErrEventNotFound` when the
event is gone. The tool layer cuts long content to its own budget, so return
it whole, or set `Truncated` if you cut it.

## What `Deps` gives you

`Run`, `Expand`, and the optional interfaces below are handed a
`source.Deps`. Nothing in it is specific to one source except `State`.

| Field | What it is |
|---|---|
| `Pool` | The Postgres pool. |
| `Sink` | The event sink: `Upsert`, `Delete`, `ExternalIDs`, `StoredBody`. The sink `Run` is handed also counts what it writes in the source's metrics. |
| `Logger` | A logger; `Run`'s is labelled with the connector's name. |
| `Metrics` | Registers the source's own Prometheus metrics, and counts its base metrics (see [the rules](#base-metrics)). |
| `State` | The source's own durable state: a JSON value per key, shared by every replica. Use it for cursors, pending requests, and what the page shows. |
| `Files` | Lends Diderot's attachment workers what they need to read the source's images and documents: `LendImages` for the caption worker, `AddDocumentWorker` for text extraction. Only `Run`'s `Deps` has it. |

`Deps.For(name)` gives a `Deps` its source's `State`; the server does this
for you. A `Deps` made without it, as a test might, has a `State` that
returns `source.ErrNoState`.

## Optional interfaces

The server finds these by type assertion. Each drives one piece of shared
UI, so a source needs no code outside its package for them, apart from the
template a `SectionProvider` needs.

- **`StatusReporter`**: `Status(ctx, deps) (settings.Status, error)` fills
  the Status section of the source's page: rows of facts such as "Last sync
  2m ago", a note under them, and `Waiting`. `Waiting` is a sentence at the
  top of the page saying what keeps the enabled source from working; it
  also marks the source as needing attention on the connectors list.
  `Status` runs on whichever replica serves the page, so read what it shows
  from `Deps.State` or the database, never from memory in `Run`.
- **`Backfiller`**: `Backfill()` describes a request to re-read what the
  source indexes, and `RequestBackfill(ctx, deps, args)` records one for
  `Run` to work through. The page's backfill form requests it. Its
  arguments are optional text fields, each a field of that form.
- **`SectionProvider`**: `Section()` names a section of the page that the
  admin UI renders itself, for a source whose management is more than rows,
  such as Slack's channels. It needs a template in `internal/adminui`,
  so most sources should use status rows instead.

## Rules

### Scopes

Every event has a scope, and retrieval only ever returns events whose scope
the caller may read ([Retrieval scopes](auth.md#retrieval-scopes)). Choose
the narrowest scope that is true for every event:

- `events.ScopeInternal` for content every signed-in user may read.
- `events.ScopePublic` for content anyone on the internet could read.
- `events.ScopeOwner(email)` for content only one person may read.
- `events.ScopeUnstamped` when the right scope can't be determined: the
  event is stored and nobody can read it.

A narrower scope that a policy grants to some groups, as Zendesk's
`corpus:zendesk` is, needs support in `internal/authorization` as well.

### Writes and deletes

`Sink.Upsert` is idempotent on the event source and external ID, and leaves
an event whose content has not changed alone, so it costs no embedding. A
source can write every item on every pass and needs no hash table of its
own; it only needs external IDs that stay the same for the same item.

When an item is removed or becomes unreadable at the source, delete its
event with `Sink.Delete`. Decide what is gone only from a complete read that
succeeded. A pass that failed or was cancelled part way has not seen
everything, and deleting what it did not see deletes the corpus. Be wary of
an empty answer, too: an empty listing is more often an outage, a revoked
permission, or a missing mount than everything deleted.

### Never keep what you were lent

`Run`'s context ending means everything it was handed is about to be
replaced. Release what you lent through `Deps.Files` (call the function it
returned), stop every goroutine, and keep no `Deps`, client, or settings in a
package variable, where a later `Run` would find them.

### Base metrics

Every source has the same base metrics, labelled `connector`:
`diderot_connector_events_written_total`,
`diderot_connector_last_success_timestamp_seconds`, and
`diderot_connector_errors_total`. They count only what reaches them through
`Deps`, so every `Run` keeps two rules:

1. Write events through `deps.Sink`. It counts each event it inserts or
   changes.
2. Run work under `worker.NewSupervisor(deps.Logger, deps.Metrics)`. It counts
   each tick of a `worker.Runner` as a success or a failure. Work that is not
   a `Runner`'s ticks, such as the passes of one long-running
   `worker.Func`, calls `deps.Metrics.Succeeded()` or
   `deps.Metrics.Failed()` after each pass itself.

Alert on a stale `diderot_connector_last_success_timestamp_seconds`: it
catches a source that runs but never succeeds.

### State and configuration

Keep a source's own state in `Deps.State`, not a table of its own. A setting
an administrator changes belongs in `Fields`. A deployment setting, such as a
mounted path, is a `DIDEROT_*` variable that your package reads where it is
used; every one needs a row in [the configuration
reference](configuration.md), and `internal/config/reference_test.go` fails
until it has one.

## Registering it

Add your source to `sourceRegistry` in `cmd/diderot/sources.go`:

```go
return source.NewRegistry(
	// ...the sources before it...
	markdowncx.Connector{},
)
```

The order is the order pages, the `sources` enum, and `{{SOURCES}}` list
sources in. A new source is off until an administrator enables it.

The goldens in `cmd/diderot/testdata` pin what models and pages see of every
source. Regenerate them and read the diff:

```sh
DIDEROT_UPDATE_GOLDEN=1 go test ./cmd/diderot -run Golden
git diff cmd/diderot/testdata
```

Your source's name, title, and tool description should be the only change.

## Testing it

- Test the parts that need no database directly: parsing, rendering, the
  checks in `Validate` and `Test`.
- For the parts that write, `testdb.URL(t, "<key>")` (`internal/testdb`)
  gives the test a migrated database of its own, and skips the test when
  `DIDEROT_TEST_DB_URL` is unset. Build `Deps` with a real sink and `For`:

  ```go
  pool, err := pgxpool.New(t.Context(), testdb.URL(t, "notes"))
  require.NoError(t, err)
  t.Cleanup(pool.Close)
  deps := source.Deps{
  	Pool: pool, Sink: events.NewDBSink(pool, prometheus.NewRegistry()),
  	Logger: slog.New(slog.DiscardHandler),
  }.For("notes")
  ```

  `Sink.Observed(w)` returns a sink that also calls `w.Wrote()` for each event
  it inserts or changes, which is how a test checks that an unchanged item
  costs no write.
- Test `Run` itself: start it in a goroutine, wait for what it writes, cancel
  its context, and require that it returns promptly.
- Test what you delete, and above all what you don't: a failed pass, a
  cancelled one, an empty one.
- Run the tests with the race detector, against the development Postgres
  (see [CONTRIBUTING.md](../CONTRIBUTING.md)):

  ```sh
  just test-race ./internal/connector/notes/... ./cmd/diderot/...
  ```

  `cmd/diderot`'s tests build the real registry: they fail on an incomplete
  `Info`, a backfill field that collides with a flag, and any golden your
  source changes.

## Worked example: the Markdown folder source

`internal/connector/markdown` indexes the Markdown files in one folder that
the deployment mounts. Each file is one event keyed by its path; a scan every
few minutes writes every file and removes the events of files that are gone.
It uses nothing outside `internal/source` and the packages every source
uses (`settings`, `events`, `worker`), and its whole footprint outside the
package is the registration line, its configuration row, and the goldens.

### `connector.go`: the page and the test

`Connector` is an empty struct: the source keeps nothing between calls.
`Name` is `markdown`. `Fields` has two settings, a `duration` rescan interval
(default `5m`, required) and an optional `text` base URL. `Validate` refuses
an interval under a minute and a base URL that is not http or https; it
returns `settings.FieldErrors`, so each message appears under its field.

The folder is not a field. It comes from `DIDEROT_MARKDOWN_DIR`, which the
package reads where it needs it (`Test`, `Run`, `Status`), so only whoever
deploys Diderot decides what it can read.

`Test` reads the variable and reports two checks: **Folder** (set, and opens)
and **Markdown files** (how many it found, or why none count). It lists the
folder exactly as a scan does, so a passing test means the first scan will
read the same files.

### `source.go`: facts, the worker, expand

`Info` names the event source `markdown` and leaves `ActorRole` and
`IdentityNamespace` unset: a file has no author Diderot can know.

`Run` returns at once with "No folder is mounted for this deployment." when
the variable is unset. Otherwise it adds one `worker.Runner` to a supervisor
built with `deps.Metrics`, with `BootTick` so the first scan runs immediately
and the rescan interval as its period, and blocks in `sup.Run(ctx)`. Each
tick's error is logged and counted by the supervisor, which also makes the
last-success metric correct without a line of metrics code in the source.

Each tick calls `rescan`, which runs one scan and then records how it went in
`Deps.State` under `last_scan`: when it finished, its error, and the counts of
the last scan that read the whole folder. A cancelled scan records nothing.

`Expand` returns the stored body through `Sink.StoredBody`, after checking the
event is still a `markdown` one.

### `scan.go`: reading the folder

`list` opens the folder as an `os.Root`, so nothing outside it can be
reached, and walks it with `fs.WalkDir`. It skips hidden entries, every
symlink (the walk sees a symlink as itself and never follows it), and
anything that is not a regular `.md` or `.markdown` file. It stops with an
error past 10,000 files rather than index part of a large folder.

`read` opens each file without blocking, so a file swapped for a named pipe
after the walk can't hang the scan. `os.Root` follows a symlink that stays
inside the folder, so `read` then checks that the path is still the regular
file it opened, not a link to one. It reads at most 1 MiB and one byte
whatever size the file reports. A file over 1 MiB, not UTF-8 text (invalid
UTF-8, or a NUL byte, which Postgres can't store), or empty is skipped and
counted.

`scan` then:

1. Upserts each file as an event: the slash-separated path as its external
   ID, the first `# ` heading outside a code fence (or else the file name)
   as its title, the text as its body, the modification time as its
   occurred-at, `events.ScopeInternal` as its scope, and a URL when a base
   URL is set. The sink leaves unchanged files alone.
2. Fails with "The folder has no Markdown files." if it saw none, and
   deletes nothing: an empty folder is more likely a mount that failed.
3. Otherwise lists the source's indexed events with `Sink.ExternalIDs` and
   deletes each one whose file it did not see. A skipped file counts as
   seen, so its last indexed text stays until it is fixed or removed.

Any error, or cancellation, returns before step 3.

### `status.go`: the page's status

`Status` reads `last_scan` from `Deps.State`, so every replica shows the
leader's last scan: when it finished (marked "failed" when the scan
failed), and the files indexed and skipped. While the variable is unset, or
the last scan failed, it says why in `Waiting`.

### The tests

- `connector_test.go`: the registries accept it; `Validate`'s table; `Test`
  for an unset variable, a folder that does not open, an empty folder, too
  many files, and a good one.
- `scan_test.go` and `scan_unix_test.go`, with temporary directories and no
  database: symlinked files and directories pointing inside and outside the
  folder, `..` paths, hidden entries, a named pipe, the size and UTF-8
  limits, the file limit (passed in, so the test needs three files, not
  10,000), titles, and link escaping.
- `source_test.go`, against a test database: the events a scan writes, that
  a rescan of an unchanged folder writes nothing and a new base URL rewrites
  every link, deletes after a complete scan, skipped files keeping their
  events, failed, cancelled, and empty scans deleting nothing, `Run` with
  and without a folder, and `Expand`.

### Outside the package

- One line in `cmd/diderot/sources.go`.
- The goldens, where `markdown` joined the `sources` enums, the `retrieve`
  and `list` tools' `sources` descriptions, `{{SOURCES}}`, and the connector
  lists.
- A row for `DIDEROT_MARKDOWN_DIR` in [the configuration
  reference](configuration.md#markdown-folder).
