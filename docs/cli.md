# The `diderot` command on your machine

The `diderot` you download has four commands, for people who use a Diderot
server:

| Command | What it does |
|---|---|
| `diderot setup <url>` | Signs in, registers Diderot's MCP server with the coding agents you name, and installs the Diderot agent skill for them. |
| `diderot doctor` | Checks the connection, the sign-in, and everything `setup` configured. It changes nothing. |
| `diderot ask "<question>"` | Asks Diderot and prints the answer with its citations. `--session` asks a follow-up in the same conversation. |
| `diderot sessions ...` | Ships the sessions your coding agents keep on this machine to the server, from a background service `setup` offers to install. |

Everything the first three do goes through the server's MCP endpoint,
`/mcp`, with the same tools an agent uses; `sessions` also pushes to the
server's session route beside it. The download is these commands alone,
about 10 MB. The server's own commands (`serve`, `probe`, `qdrant-audit`,
`reset-admin`) are in the server's container image, whose `diderot` has
these four as well; the download says so if you run one of them.

## Install

With [Homebrew](https://brew.sh), on macOS or Linux:

```sh
brew install bluesky-social/tap/diderot
```

Naming the formula in full taps `bluesky-social/tap` and trusts this
formula alone. Each release updates it, so `brew upgrade` brings the next
one. It installs the release's binary, which is not signed with an Apple
Developer ID; Homebrew does not quarantine what a formula downloads, so
macOS runs it without asking.

Without Homebrew, download the newest release's archive for your platform
(`darwin` or `linux`, `amd64` or `arm64`), check it against the release's
`checksums.txt`, and put `diderot` on your `PATH`:

```sh
platform=darwin_arm64
base=https://github.com/bluesky-social/diderot/releases/latest/download
curl -fsSLO "$base/checksums.txt"
archive=$(grep -o "diderot_[^ ]*_${platform}\.tar\.gz" checksums.txt)
curl -fsSLO "$base/$archive"
shasum -a 256 --ignore-missing -c checksums.txt
tar -xzf "$archive" diderot
install diderot ~/.local/bin/
```

Download it this way rather than with a browser, which quarantines it, and
macOS then refuses to run it. Each archive has a signed build provenance
attestation; with the GitHub CLI, check it with
`gh attestation verify "$archive" --repo bluesky-social/diderot`.

With Go installed, `go install github.com/bluesky-social/diderot@latest`
builds the same binary from source as `diderot`, in `$(go env GOPATH)/bin`.

Each archive also holds Diderot's licences and, under `third-party/`, those
of the libraries compiled into the binary; Homebrew puts the latter in
`$(brew --prefix)/share/doc/diderot`.

**Upgrading** is `brew upgrade diderot`, or installing the newer binary the
way you installed it. Then run `diderot setup <url>` again: it brings the
agent skill up to that version's text and leaves everything else that still
matches alone. Until then, `diderot doctor` reports the skill as differing.

## Set up

Your administrator gives you one URL: the Diderot server, or the gateway in
front of it. Then:

```sh
diderot setup https://diderot.example.com --harness claude-code,codex
```

`setup` works out how to sign in from the URL itself:

- **A server that takes an API token** asks for it, without echoing it.
  Off a terminal, pass it on stdin:
  `printf %s "$TOKEN" | diderot setup <url> --harness cursor --token-stdin`.
- **A server or gateway that signs in with OAuth** (one that publishes MCP
  authorization metadata: a gateway that does, or a Diderot server in
  `oidc` mode, [Authentication](auth.md#signing-in-to-mcp-in-oidc-mode))
  opens your browser. Off a terminal it refuses rather than wait for a
  browser; run it in a terminal. If the sign-in server does not let
  programs register themselves, `setup` says so: ask your administrator for
  the OAuth client ID and add `--client-id <id>`.
- **A server on your own machine in `none` mode** needs nothing.

It saves the sign-in in `~/.config/diderot/client.json` (under
`$XDG_CONFIG_HOME` when that is set), readable only by you. `ask` and
`doctor` use it, and renew an OAuth sign-in on their own; when it can no
longer be renewed they say to run `setup` again.

Then, for each harness you name (`--harness` with a comma-separated list; on
a terminal without it, `setup` lists them, marks the ones it finds, and
asks; `--harness none` only signs in):

| Harness | How it is registered | Skill |
|---|---|---|
| `claude-code` | `claude mcp add-json diderot … --scope user` | `~/.claude/skills/diderot` |
| `codex` | `codex mcp add diderot --url …` | `~/.agents/skills/diderot` |
| `cursor` | the `diderot` entry in `~/.cursor/mcp.json` | `~/.agents/skills/diderot` |
| `opencode` | the `diderot` entry under `mcp` in `~/.config/opencode/opencode.json` (or `.jsonc`) | `~/.agents/skills/diderot` |
| `pi` | `pi mcp add diderot --url …` (Pi 1.0 or later) | `~/.agents/skills/diderot` |

`setup` changes only the `diderot` entry: comments, formatting and other
servers in a configuration file stay as they were. An entry that already
matches is left alone, so running `setup` again is safe; it reports each
entry as added, updated or unchanged. It remembers every harness it has
configured for the server, rechecks them on later runs, and `doctor` checks
them all.

How each harness authenticates:

- **OAuth.** The harness signs itself in; `setup` registers the server and
  prints the harness's own sign-in command (`claude mcp login diderot`,
  `codex mcp login diderot`, `opencode mcp auth diderot`,
  `pi mcp login diderot`, or Cursor's MCP settings). `codex mcp add` starts
  Codex's sign-in itself, so off a terminal `setup` leaves a Codex entry that
  would change alone and exits 4.
- **API token.** The token goes into the harness's entry as an
  `Authorization` header. `setup` will not write it into an existing file
  other users can read; it says which file to `chmod 600`. Codex reads a
  token only from the environment: export `DIDEROT_TOKEN` where Codex runs.

## Check it

```sh
diderot doctor
```

It checks that the configuration exists, the server answers, the sign-in
works, the server speaks the MCP version `ask` needs and offers its tools,
and that each harness's entry and the skill are in place. It exits 1 if any
check fails and says what to run. A harness's own OAuth sign-in is kept by
that harness and is not something `doctor` can see.

## Ask

```sh
diderot ask "What did we decide about the billing migration?"
diderot ask --session 3f1c2a9e-... "Who owns it now?"
```

The answer prints with its citations, and the session ID to continue with
goes to stderr. With no question in the arguments, `ask` reads it from
stdin. An answer can take a few minutes; Ctrl-C cancels it on the server,
which stops working on it and records the question as cancelled.
Images the answer produced are listed; `--save-artifacts <dir>` writes them
there as `<id>.png` (or the image's own type), never over an existing file.

## Ship your sessions

Diderot can index the sessions of your coding agents: it summarizes each
one, and the summaries become searchable next to everything else, with the
transcript behind every hit ([Agent-session ingest](session-ingest.md)).
`diderot sessions` sends them from your machine, so a Diderot you run for
yourself needs nothing else to collect them. The server has to be ready for
them in two ways: session push turned on, and a model chosen for both
session-summary jobs on its Models page, one that passes each job's test;
[What the server needs](#what-the-server-needs) has both.

After signing in, `diderot setup` on a terminal lists the session
directories it found and asks:

```text
Coding-agent sessions on this machine:
  claude-code  /Users/you/.claude/projects (212 sessions)
  codex        /Users/you/.codex/sessions (48 sessions)
Not checked from here: the server summarizes sessions only once an
administrator has chosen a model for both session-summary jobs on its Models
page (https://diderot.example.com/admin/settings/models); the model has to
pass each job's test, which requires it to answer a forced tool call. Until
then sessions are stored but not searchable; diderot sessions status shows
what the server has indexed.
Ship them to diderot.example.com? A background service converts each session
on this machine and sends its full transcript to that server, where everyone
who can sign in to it can search and read it unless you choose Only me for
it on the server's /sessions page. [y/N]:
```

Yes installs and starts the background service. Anything else installs
nothing. Before asking, `setup` checks that the server would take your
sessions; if session push is off there, or your account may not push, it
says which and how to put it right, asks nothing, and installs nothing. Off
a terminal nothing is asked: `--sessions` says yes, and `--sessions=false`,
or no flag, says no. `diderot sessions install` does the same at any later
time.

`setup` ships from every session directory it finds and takes no `--path`
or `--exclude`. If one project's sessions should stay on your machine,
answer no at the prompt and run `diderot sessions install --exclude
<that project's directory>` instead.

**What is sent, and who can read it.** The whole transcript: every prompt,
answer, tool call, and tool output in the session file. Nothing is redacted,
by this command or by the server, so a secret a session printed goes too.
Images are left out: a screenshot you pasted into a session, or an image a
tool returned, becomes a note that an image of that type was there, and its
bytes stay on your machine. On the server a session belongs to the account
you signed in with, and everyone who can sign in to that server can find
and read it, until you make it owner-only with **Only me** on the server's
`/sessions` page. On a Diderot only you sign in to, that is only you. The
service ships only to the server you said yes for: if you later run
`diderot setup` for another server, it sends nothing until you say yes again
there, or run `diderot sessions install`.

**Where sessions are found.** A harness is offered when its session
directory exists:

| Harness | Directory | Sessions |
|---|---|---|
| `claude-code` | `~/.claude/projects` (`$CLAUDE_CONFIG_DIR/projects` when that is set) | each `<session id>.jsonl`; subagent transcripts are not sent |
| `codex` | `~/.codex/sessions` (`$CODEX_HOME/sessions` when that is set) | each `rollout-*-<session id>.jsonl` |
| `copilot-cli` | `~/.copilot/session-state` (`$COPILOT_HOME/session-state` when that is set) | each `<session id>/events.jsonl`; subagents are in the same file |
| `gemini-cli` | `~/.gemini/tmp` (`$GEMINI_CLI_HOME/.gemini/tmp` when that is set) | each `<project>/chats/session-*.jsonl`; subagent transcripts are not sent |
| `pi` | `~/.pi/agent/sessions` (`$PI_CODING_AGENT_DIR/sessions` when that is set, and `$PI_CODING_AGENT_SESSION_DIR` over both) | each `<timestamp>_<session id>.jsonl`; a session started with a `--session-id` that is not a UUID is not sent. A `sessionDir` in Pi's settings is not read: name it with `--path pi=<directory>` |
| `polytoken` | `~/.local/share/polytoken` (`$POLYTOKEN_DATA_PATH`, or `$XDG_DATA_HOME/polytoken`, when that is set) | each `sessions-v1/<session id>/log.jsonl`, and `sessions/<session id>/log.jsonl` from older releases; subagent transcripts are not sent |

To ship from somewhere else, or only part of it:

```sh
diderot sessions install \
  --path claude-code=$HOME/.claude/projects \
  --exclude $HOME/.claude/projects/-Users-you-private-project
```

`--path` replaces the directories found. `--path <harness>=<dir>` names
that harness's root, a directory laid out like the one the table shows for
it, not a directory below it: a session file is recognized by its path
inside the root. `--exclude` leaves out every session whose file is really
under a directory, including one reached through a symbolic link from
somewhere else, so it is the way to ship only part of a root. Both repeat,
and both belong to `diderot sessions install` alone.

**How it works.** Each session file is read and converted on your machine
to [ATIF](https://github.com/harbor-framework/harbor/blob/main/rfcs/0001-trajectory-format.md),
an open format for agent sessions that the server already reads, and the
converted session is what is sent: the server never sees the harness's own
file. The session files are only ever read. A session that has grown is
sent again whole and replaces the server's copy, so one session on your
machine is always one session on the server.

The background service looks for changes every minute and sends a session
once its file has gone five minutes without being written. It waits because
every send of a session in progress is another copy for the server to keep
and another summary for it to write; waiting for a pause sends a working
session a few times instead of every minute. `diderot sessions sync` is you
asking for everything now, so it does not wait and sends sessions in
progress as they stand. Either way, a last line the harness has not
finished writing is not sent; it goes with the next send, once it is
complete. When the server cannot be reached, the service waits longer
between tries, up to half an hour, and carries on from where it was when
the server is back.

| Command | What it does |
|---|---|
| `diderot sessions install` | Installs and starts the background service, replacing and restarting one already there. |
| `diderot sessions status` | Whether the service is installed and running, the directories it ships from, how many sessions were sent, how many of them the server has indexed, when a pass last succeeded, and the last pass's error if it failed. `--json` also lists each session file that was skipped and why. |
| `diderot sessions sync` | Sends everything new or changed now, sessions in progress included, prints what it sent, and exits. |
| `diderot sessions run` | What the service runs: a pass every minute, until stopped. |
| `diderot sessions uninstall` | Stops and removes the service. |

`diderot doctor` reports the service too, and fails when it is installed but
not running, or when its last pass failed.

The service is yours alone and needs no administrator rights:

| | macOS | Linux |
|---|---|---|
| Service | a launchd agent, `~/Library/LaunchAgents/com.github.bluesky-social.diderot.sessions.plist` | a systemd user unit, `~/.config/systemd/user/diderot-sessions.service` (under `$XDG_CONFIG_HOME` when that is set) |
| Starts | at login, and again if it fails | at login, and again if it fails; `loginctl enable-linger $USER` keeps it running while you are logged out |

On a Linux machine without systemd, `install` installs nothing and prints
the command to keep running under whatever you use instead
(`diderot sessions run`). There is no `diderot` for Windows; under WSL, use
the Linux one. The service runs the `diderot` you installed it with and
reads the sign-in from the file `setup` saved; the service definition holds
no credential. Beside that file, `sessions.json` lists the server and the
directories. Under `~/.local/state/diderot` (`$XDG_STATE_HOME/diderot` when
that is set), `sessions-state.json` records what was sent and
`sessions.log` is the service's log, one line per session sent and per
failed pass.

`uninstall` removes the service definition, `sessions.json`, and the log. It
keeps `sessions-state.json` (and the empty `sessions-state.json.lock` beside
it), so installing again does not send every session a second time; delete
it to start over. Sessions already on the server stay
there.

### What the server needs

- **Session push turned on**: `DIDEROT_SESSION_INGEST_SOURCE=push` and a
  `DIDEROT_SESSION_PUSH_REVISION_KEY`, where the server runs
  ([Push protocol](session-ingest.md#push-protocol); for the Compose
  quickstart, [Agent sessions](quickstart.md#agent-sessions)). `setup` and
  `install` check this and say so when it is off.
- **Your account allowed to push**: in a group the server's
  `DIDEROT_SELF_SERVICE_GROUPS` names. `setup` and `install` check this too.
- **A model for both session-summary jobs**, chosen on the server's Models
  page (`/admin/settings/models`). It has to pass each job's test, which the
  page runs when you save it: the model must answer a forced tool call, on
  the Anthropic Messages, OpenAI Responses, or Chat Completions API,
  whichever its provider serves. The `diderot` command cannot check this from your
  machine. Without it the server stores your sessions and summarizes none,
  so nothing is searchable: `diderot sessions status` and `diderot doctor`
  say how many of your sessions the server has indexed, and point at the
  Models page while that is none.
- **The server itself at the URL you gave `setup`**, or a gateway that also
  forwards `/v1/ingest/agent-sessions/` and `/v1/me/agent-sessions`:
  sessions do not travel through MCP.

The first run sends every session the directories hold, and the server's
summary model reads each one: on a machine with years of sessions, expect
that first pass to cost model calls in proportion.

A session the server refuses (larger than its limit, or one the converter
cannot read) is skipped and tried again when its file changes;
`diderot sessions status --json` lists them.

The server limits what one account may store
([Agent sessions](configuration.md#agent-sessions)): 4 GiB
(`DIDEROT_SESSION_PUSH_OWNER_QUOTA_BYTES`) and 20,000 copies of sessions
(`DIDEROT_SESSION_PUSH_OWNER_QUOTA_REVISIONS`), where every session is one
copy and every later send of a session that grew is one more. The copies
are what one person reaches first: starting 30 sessions a day, each sent
four times as it grows, uses 20,000 in about five and a half months. When a
limit is reached, `status` says the quota is full and nothing more is sent
until it is raised or earlier copies are let go. By default the server keeps
every earlier copy; with `DIDEROT_SESSION_PUSH_SUPERSEDED_RETENTION` set
(`168h`, say) it deletes copies that have been replaced for that long, and
they stop counting. What you give up: a citation Diderot handed out to an
earlier copy, older than that, expands to a note naming the session's
current copy instead of the records it pointed at. On a server of your own,
set it.

## Scripts and agents

When stdout is not a terminal, or with `--json`, every command prints one
JSON object on stdout; an error is `{"error": "...", "exit_code": n}`.
Progress goes to stderr. Nothing prompts off a terminal. Exit codes:

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Failure: a failed check, a failed answer, anything else |
| 2 | Bad flags or arguments |
| 3 | Sign-in needed, expired, or refused |
| 4 | Partly done: `setup` signed in but a harness or the skill failed, or `ask` answered but an image was not saved |
| 5 | Cancelled |

## What `setup` cannot do

A sign-in server that offers neither dynamic client registration nor a
registration of its own at the MCP URL (a gateway can offer one) needs a
client ID from your administrator: MCP gives a server no way to publish one.

For an OAuth client ID shared by several harnesses, the identity provider
must accept loopback redirects to `http://127.0.0.1:<port>/callback` and
`http://localhost:<port>/callback` on any port, the form the harnesses use
(`setup` points OpenCode at port 19876).
