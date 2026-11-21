# Quickstart

Run Diderot on one machine with Docker Compose, point it at your Linear,
Notion, Slack, or a folder of Markdown files, and ask questions about what it
ingests. The stack is self-contained: the only outside service it needs is
one model provider, which you add in the admin UI.

| Service | What it is | Exposed |
|---|---|---|
| `diderot` | The server, built from this checkout | `127.0.0.1:9091` |
| `postgres` | Postgres 18, the event store | no |
| `qdrant` | Qdrant, the vector index | no |
| `embedder` | [text-embeddings-inference](https://github.com/huggingface/text-embeddings-inference) on CPU, serving `mixedbread-ai/mxbai-embed-large-v1` (1024 dimensions) | no |
| `init` | One-shot, before each start: generates the API token and the server's keys on the first start, and writes the server's token file | no |

Every image is pinned by digest and the local embedding model by commit. The
files are `compose.quickstart.yaml` and `.env.quickstart.example` (optional
settings).

## What you need

- Docker Engine 25 or later with Compose v2.24.4 or later (Docker Desktop,
  Colima, or Docker Engine on Linux). Give Docker about 4 GB of memory; the
  embedder alone uses about 3 GB. Colima's 2 GB default is too small: run
  `colima stop`, then `colima start --memory 6`.
- About 9 GB of free disk: about 1.8 GB of images, 1.3 GB for the
  embedding model, and about 5.5 GB of build cache that building the
  server image leaves behind, plus whatever you ingest.
- An API key for OpenAI or Anthropic, or another provider
  ([Model providers](#model-providers)), which you paste in the admin UI.
  The stack starts without one (see
  [What leaves the machine](#what-leaves-the-machine)).
- Something to ingest: a Linear API key, a Notion integration, or a Slack app
  (see [Adding sources](#adding-sources)). Without one, the
  [Markdown folder](#markdown-folder) connector indexes Diderot's own
  documentation, so there is something to ask about.

## From zero to a first answer

1. Start it:

   ```sh
   git clone https://github.com/bluesky-social/diderot.git
   cd diderot
   docker compose -f compose.quickstart.yaml up -d --wait
   ```

   The first run builds the server image and downloads the local embedding
   model (about 1.3 GB), which takes a few minutes. It also generates an API
   token and the keys that seal browser sessions and encrypt the keys you
   paste in the admin UI, and keeps them in the stack's `keys` volume.
   `--wait` returns when every service is healthy; nothing about models is
   needed yet. To use plain `docker compose` commands in this directory,
   `cp .env.quickstart.example .env`, which names the quickstart's compose
   file and holds its optional settings.

2. Open <http://localhost:9091/admin> and sign in as `admin` / `admin`. It
   opens on the setup checklist ([Setup](setup.md)).

3. Set up the models on **Models** (`/admin/settings/models`):
   **Add a provider** with your key (OpenAI: kind **OpenAI-compatible**,
   base URL `https://api.openai.com/v1`; Anthropic: kind **Anthropic**, base
   URL `https://api.anthropic.com`, plus the local embedder:
   [Model providers](#model-providers)). It lists the provider's models.
   Then choose the **Embeddings** model, and a model for each job, and
   **Save**: each is tested as it is saved, and a wrong key or a model
   your account cannot use is reported on the page.

   The rest of the checklist: name your organization, turn on a source at
   **Connectors** ([Adding sources](#adding-sources)), use the example
   prompts as-is or edit them, review who has access, and go live. One step
   replaces the `admin` account: sign out, sign back in with the API token,
   and turn `admin` off. Print the token with:

   ```sh
   docker compose -f compose.quickstart.yaml run --rm init cat /keys/api-token
   ```

   Until you go live, only administrators can use Ask. The API token is an
   administrator, so the next step works during setup too.

4. Ask. Sources sync in the background once they are on, and each item is
   searchable as soon as the embedder has indexed it.

   ```sh
   export DIDEROT_TOKEN="$(docker compose -f compose.quickstart.yaml run --rm -T init cat /keys/api-token)"
   curl -sS http://localhost:9091/v1/ask \
     -H "Authorization: Bearer $DIDEROT_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"question": "What did we decide about the billing migration?"}'
   ```

   The reply is JSON with the answer in `reply`. To see what retrieval finds
   without calling a model, post the same body to `/v1/retrieve`. Until the
   server has built Ask, a moment after it starts listening, Ask answers
   `503 ask is unavailable on this replica; retry`; retry.

5. Connect an MCP client. The server speaks MCP (streamable HTTP) at
   `/mcp`, and the API token is its credential too. With the
   [`diderot` command](cli.md) on your machine, in the shell where step 4
   set `DIDEROT_TOKEN`, one command registers the server with your coding
   agents and installs the agent skill:

   ```sh
   printf %s "$DIDEROT_TOKEN" | diderot setup http://localhost:9091 \
     --harness claude-code --token-stdin
   diderot doctor
   diderot ask "What did we decide about the billing migration?"
   ```

   For any other client, the equivalent configuration, with the token
   pasted in:

   ```json
   {
     "mcpServers": {
       "diderot": {
         "type": "http",
         "url": "http://localhost:9091/mcp",
         "headers": { "Authorization": "Bearer <your API token>" }
       }
     }
   }
   ```

   The client then has five tools:

   - `retrieve`: semantic search over everything indexed; a Slack permalink
     or another exact handle in the question is read exactly.
   - `list`: every event matching a filter, newest first, and the way to
     read a Slack channel's history over a time range.
   - `expand`: the full thread, page, issue, or session around one result,
     with its attachments listed.
   - `attachment`: one of those attachments, as an image or as a document's
     extracted text.
   - `ask`: a full Diderot answer, as in step 4; pass its `session_id` back
     to ask a follow-up. It runs as an MCP task, so it needs a client that
     implements the MCP Tasks extension; the other four work in any client.

   Each call sees what the token's identity may see
   ([Retrieval scopes](auth.md#retrieval-scopes)).

## Adding sources

Sources and tools are set up in the admin UI, at
<http://localhost:9091/admin/connectors>, and apply without a restart. Open
one, paste its token, and click **Save and turn on**: it saves the form, tests it,
and turns the connector on if the test passes. If the test fails, what
you entered stays saved and the page says what to fix.

### Markdown folder

This one needs no account. The quickstart mounts this checkout's `docs/`
folder read-only at `/srv/docs` and sets `DIDEROT_MARKDOWN_DIR` to it. Open
<http://localhost:9091/admin/connectors/markdown>, set **Link documents to**
to `https://github.com/bluesky-social/diderot/blob/main/docs/` so citations
link to the published files, and click **Save and turn on**. (Changing the
link later re-indexes every file.) Diderot reads the folder at once and again
every five minutes. Each `.md` or `.markdown` file is one document, and a
file you delete is removed at the next read. The CPU embedder takes a few
minutes to index these docs (about three on an Apple-silicon laptop). Once it
has caught up, ask about Diderot itself, from your MCP client or with:

```sh
curl -sS http://localhost:9091/v1/ask \
  -H "Authorization: Bearer $DIDEROT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"question": "How do I add a source to Diderot?"}'
```

To index your own folder instead, mount it at `/srv/docs` in an override
file (see [Running it](#running-it)), which replaces the docs mount, and run
`docker compose up -d`:

```yaml
services:
  diderot:
    volumes:
      - /path/to/notes:/srv/docs:ro
```

At the next read, the docs leave the index and your files enter it. Clear
**Link documents to**, or set it to where your notes are published. A folder
with no Markdown files changes nothing: the docs stay indexed, and the
connector's page says the folder has no Markdown files. On macOS, that is
what an unshared folder looks like: the folder must be one your Docker
runtime shares with its VM. Colima shares your home folder by default and
mounts any other path as an empty folder; Docker Desktop lists its shared
folders under its file-sharing settings.

### Linear

Create a personal API key in Linear (Settings, then Security & access) and
paste it on the Linear connector page. Diderot syncs the issues and comments
of every team the key can see, except private teams, every five minutes, with
an hourly sweep that drops issues moved into private teams and a daily full
reconcile. A personal key stops working when your Linear account is
disabled, so a deployment should sign in as an OAuth application instead
([Linear](configuration.md#linear)).

### Notion

Create an internal integration at <https://www.notion.so/profile/integrations>,
give it read access, and paste its secret on the Notion connector page. Share
the pages you want indexed with it (a page's `...` menu, Connections); Notion
shows an integration only the pages shared with it.

Diderot ingests only the page trees you declare as roots. Notion's search
finds shared pages, but only to list them as candidates. Declare each tree
under **Pages Diderot indexes** on the connector page
(<http://localhost:9091/admin/connectors/notion>), which also lists the
shared pages and databases Notion's search found, then click **Read now**
instead of waiting for the daily read.

### Slack

Create a Slack app from `slack-app-manifest.yaml`, install it to your
workspace, and copy its tokens, following steps 1 and 2 of [Slack app
setup](slack-app.md). Diderot connects out to Slack, so the server needs no
public URL. Then open <http://localhost:9091/admin/connectors/slack>:

1. Paste the bot token (`xoxb-...`) and the app-level token (`xapp-...`).
2. Click **Save and turn on**. It saves the tokens, checks them, the bot's
   permissions, and its channels, finds your workspace's name and host, and
   turns Slack on. If a check fails, the tokens stay saved and the page says
   what to fix.

Set `DIDEROT_QUICKSTART_EMAIL` in `.env` to your Slack account's email, and
run `docker compose up -d` again: Diderot maps a caller to private-channel
access by email.
Invite the bot to the channels it should read (`/invite @diderot`). It ingests
the channels it is a member of, backfills their history, and answers when
mentioned or messaged directly. Run only one deployment per Slack app: two
servers holding the same tokens split Slack's events between them.

The *Summarize thread* shortcuts use the model the Models page chooses for
thread summaries. A reasoning model, whose test finds it needs
`max_completion_tokens`, gets a 25,000-token reasoning allowance on top of
the 2,000-token summary limit (see
[Inference providers](design.md#inference-providers)).

Image captions and PDF, DOCX, and XLSX text extraction need an S3-compatible
bucket and a chat model for them (see
[Images and documents](configuration.md#images-and-documents)). The
quickstart leaves both off, so attachments are not indexed.

### Agent sessions

With session push on and a summary model chosen (both below),
`diderot setup` offers to ship the sessions your own coding agents keep on
your machine, from a background service
([Ship your sessions](cli.md#ship-your-sessions)). On a terminal it asks.
The command in step 5 reads the token from a pipe, where nothing is asked,
so say yes with `--sessions`:

```sh
printf %s "$DIDEROT_TOKEN" | diderot setup http://localhost:9091 \
  --harness claude-code --token-stdin --sessions
diderot sessions status
```

It refuses, and says why, while session push is off. The quickstart leaves
it off because it needs a secret key that the server reads only from its
environment, and the quickstart generates no such key for you; the
settings below turn it on.

Coding-agent transcripts (Claude Code, Codex, and other formats) can be pushed
to `PUT /v1/ingest/agent-sessions/{id}`; see
[Agent-session ingest](session-ingest.md). Each session is summarized before
it is indexed, and summaries need a model that calls tools, on the Anthropic
Messages, OpenAI Responses, or Chat Completions API, chosen
for both session-summary jobs, as well as the
[Agent sessions](configuration.md#agent-sessions) settings. The quickstart
sets none of them. Add them in an override file (see
[Running it](#running-it)):

```yaml
services:
  diderot:
    environment:
      DIDEROT_SESSION_INGEST_SOURCE: push
      DIDEROT_SESSION_PUSH_REVISION_KEY: ${DIDEROT_SESSION_PUSH_REVISION_KEY}
```

and set `DIDEROT_SESSION_PUSH_REVISION_KEY` in `.env` to a random value of at
least 32 bytes (`openssl rand -base64 48`), then run `docker compose up -d`.
The curl example in [Agent-session ingest](session-ingest.md#push-protocol)
pushes a session, with `DIDEROT_API_URL=http://localhost:9091` and the
`DIDEROT_TOKEN` from step 4. Until a model is chosen for both
session-summary jobs, a push is still accepted and stored, but not
summarized, and the owner's <http://localhost:9091/sessions> lists the
session as `not indexed`. The summary jobs force a tool call, and saving a
model for one tests exactly that, so a model that rejects forced tool use is
refused with the reason. Once pushed sessions are summarized, push ingest on
its own completes the setup checklist's Sources step.

### Web search

Ask can search the open web through [Tavily](https://tavily.com). Open
<http://localhost:9091/admin/connectors/tavily>, paste a Tavily API key
(`tvly-...`), and click **Save and turn on**. Ask's next answer can use the
web; turning the connector off takes the web away again, both without a
restart.

To keep the key out of the database, pass it to the server as
`DIDEROT_TAVILY_API_KEY` instead. Compose passes only the variables the
`diderot` service lists, so a line in `.env` alone does not reach it: add it
to an override file (see [Running it](#running-it)),

```yaml
services:
  diderot:
    environment:
      DIDEROT_TAVILY_API_KEY: ${DIDEROT_TAVILY_API_KEY}
```

set `DIDEROT_TAVILY_API_KEY` in `.env`, run `docker compose up -d`, and
click **Save and turn on** on the connector's page. Every connector secret
works this way; [Connector settings](configuration.md#connector-settings)
lists their variables.

Code search (a [Sourcebot](https://sourcebot.dev) deployment) and PagerDuty
investigations are connectors too, at `/admin/connectors/sourcebot` and
`/admin/connectors/pagerduty`; the quickstart runs neither service.
PagerDuty also sends its webhooks to port 9093
(`DIDEROT_PAGERDUTY_LISTEN_ADDR`), which the quickstart does not publish.

## Model providers

Every model is chosen on the Models page (`/admin/settings/models`), and
nothing about models is in the environment or a file. A provider is
anything that serves OpenAI's API or Anthropic's: add it with its base URL
and key, and its models appear in every job's list. Each job's model is
tested when you save it, so a model that cannot do a job is refused with the
reason. [Models](setup.md#models) describes the page.

**OpenAI.** Kind **OpenAI-compatible**, base URL `https://api.openai.com/v1`,
your API key. One key serves every job, embeddings included
(`text-embedding-3-small`).

**Anthropic.** Kind **Anthropic**, base URL `https://api.anthropic.com`, your
API key. Anthropic serves no embeddings, so add the bundled local embedder
too (below). Titles need the OpenAI Responses API and thread summaries Chat
Completions, so with only Anthropic those two stay off: conversations have
no titles, and Slack's *Summarize thread* shortcuts reply that they are not
configured.

**The local embedder.** The `embedder` service computes embeddings on this
machine, so indexing sends no corpus text to a provider. Add it as a
provider of kind **OpenAI-compatible**, base URL `http://embedder/v1`, any
key, offering `mixedbread-ai/mxbai-embed-large-v1` under **Models offered**
(it has no model list); this link fills the form in:
<http://localhost:9091/admin/settings/providers/new?kind=openai&name=local&base_url=http://embedder/v1&models=mixedbread-ai/mxbai-embed-large-v1>.
Then choose that model under **Embeddings**.

**Anything else that speaks OpenAI's API** (Azure OpenAI, OpenRouter, a
LiteLLM proxy, Ollama, vLLM, LM Studio). Its base URL includes any `/v1`.
Ask needs a model served over the Responses API or the Anthropic Messages
API; a Chat Completions-only model can do the other jobs but not Ask. In
this standalone stack a plain `http://` address is accepted, such as
`http://host.docker.internal:11434/v1` for Ollama on the host.

**A different local embedding model.** Set `DIDEROT_QUICKSTART_EMBED_MODEL`
and `DIDEROT_QUICKSTART_EMBED_REVISION` (a commit of the model's Hugging
Face repository) in `.env`, run `docker compose up -d --wait`, offer the new
model ID on the `local` provider, and choose it under **Embeddings**.
`BAAI/bge-m3` is multilingual and reads up to 8k tokens per chunk, but needs
much more memory than the default model (in a 6 GB Docker VM it is killed
for memory, exit code 137, as it warms up) and downloads about 2.3 GB more.
Choose before you ingest if you can: another embedding model embeds every
passage again, into a collection of its own.

### Signed-in Codex

Sign the server's Codex adapter in to ChatGPT using a device code. Open the
displayed link in your browser and enter the one-time code. The adapter
writes the credentials to its dedicated auth file and refreshes them in
place. Keep the directory private and writable by the adapter.

```sh
diderot codex-provider login --auth-file /path/to/dedicated/codex/auth.json
(umask 077; openssl rand -hex 32 > codex-provider.token)
diderot codex-provider \
  --auth-file /path/to/dedicated/codex/auth.json --token-file codex-provider.token
```

ChatGPT may require you to enable device code login in account security
settings first.

In **Models → Inference providers**, add an **OpenAI-compatible** provider
with base URL `http://127.0.0.1:9460/v1` for a standalone server on the
same host. Paste the provider token as its API key.

The provider lists the selectable models for its signed-in account, with
their supported and default reasoning efforts for direct inference. Codex
harness modes such as Ultra are omitted. Use `--model` to limit the
provider to one model, or **Models offered** to limit the picker in Diderot.
Choose a listed model for **Ask**, **Titles**, or the two session summary
jobs. Keep your separate provider for **Embeddings**. The adapter consumes
the Codex event stream and gives Diderot a complete Responses result.
Diderot executes corpus functions itself. The Codex backend receives the
questions, excerpts and transcripts passed to whichever jobs use it.
The adapter does not pass through public API output caps or cache retention
settings. Its backend is a ChatGPT client interface and may change.

## What leaves the machine

With the `embedder` container chosen for embeddings, embeddings are
computed locally, so indexing text from a Markdown folder, Linear, Notion,
Zendesk, Discord, or Slack sends nothing to the model provider; with a
provider's embedding model, every indexed passage goes to that provider. Agent-session summaries, if you set them up, send each
transcript to the summary models before it is indexed, and image captions
and PDF extraction, which need storage the quickstart doesn't have, would
send images and pages. These requests reach the provider, which bills them:

- Each Ask question: one model call per round of searching and reading, up
  to **Maximum tool turns** (8 unless you change it on the Models page),
  plus one to write the answer. Each carries the question, the conversation
  so far, and the corpus text its searches returned, so whatever Ask reads
  to answer, private Slack messages included, goes to the provider.
- A short title for each new Ask conversation, from the **Titles** model.
- The readiness check: a request of a few tokens to Ask's model when the
  server starts and every five minutes after, once Ask has a model.
- **Test models**, and saving models: one small request per model, and a
  provider's model list when it is added and every 10 minutes after.
- With Slack on, each *Summarize thread* shortcut sends that thread to the
  thread-summaries model. Scheduled reports and PagerDuty investigations,
  when you set them up, are Ask runs.

The other outbound connections are the ones you set up: the API of each
connector you enable, Tavily for web search, and, on the first start, the
download of the embedding model from Hugging Face.

## Authentication

The quickstart uses `DIDEROT_AUTH_MODE=static`. `init` generates the API
token on the first start, unless `DIDEROT_QUICKSTART_TOKEN` in `.env` sets
one, and writes only its SHA-256 to the server's token file, under the
identity `DIDEROT_QUICKSTART_EMAIL`, in the groups that grant administrator
and self-service access. Print a generated token with
`docker compose run --rm init cat /keys/api-token`. To rotate it, set
`DIDEROT_QUICKSTART_TOKEN` to a new value (`openssl rand -hex 32`) and run
`docker compose up -d`; if you later remove that line, `init` generates a
new token. For more people, give each their own token: write a
file listing each token's `sha256`, `email`, and `groups` (format under
[`static`](auth.md#static)), mount it into the `diderot` service through an
override file (see [Running it](#running-it)), point the server at it, and
run `docker compose up -d`:

```yaml
services:
  diderot:
    volumes:
      - /path/to/tokens.yaml:/etc/diderot/tokens.yaml:ro
    environment:
      DIDEROT_AUTH_STATIC_TOKENS_FILE: /etc/diderot/tokens.yaml
```

The server runs as UID 65532, which must be able to read the file. It holds
only digests, so on Linux `chmod 644` it (a file only you can read is
refused). It replaces the generated token file, so the generated API token
stops working unless you list it too: copy its entry from
`docker compose run --rm init cat /keys/tokens.yaml`. Put administrators in
`diderot-admins` and people who manage their own agent sessions in
`diderot-users`, the groups the quickstart's `DIDEROT_ADMIN_GROUPS` and
`DIDEROT_SELF_SERVICE_GROUPS` name; once the install is live, any listed
token can ask and search. The server reads the file at startup, so run
`docker compose restart diderot` after changing it.

`init` generates two keys the same way and keeps them in the `keys` volume:
the browser session key, which seals sign-ins at `/admin`, and the config
key, which encrypts the secrets pasted on connector pages. Every later start
reuses them. To use your own, set `DIDEROT_BROWSER_SESSION_KEYS` or
`DIDEROT_CONFIG_KEYS` in `.env` (`openssl rand -base64 32` each) and run
`docker compose up -d`. Replacing the config key makes the secrets saved
under the old one unreadable: list the new key first and the old one after
it (`new,old`; print the generated key with
`docker compose run --rm init cat /keys/config-keys`), run
`docker compose up -d`, paste each secret again, then drop the old key.

`DIDEROT_AUTH_MODE=none` needs no token: every request acts as one
administrator. Because of that, the server refuses to start unless every
listener binds a loopback address, which rules it out inside a container (a
published port reaches the container's own network interface, not its
loopback). Use it when running `diderot serve` directly on your machine, with
`DIDEROT_AUTH_NONE_EMAIL`, `DIDEROT_LISTEN_ADDR=127.0.0.1:9091`, and
`DIDEROT_INTERNAL_LISTEN_ADDR=127.0.0.1:9092`. Outside the image, the server
also needs the `atlas` binary to apply migrations (`DIDEROT_ATLAS_BINARY`,
see [Development and tests](configuration.md#development-and-tests)).

The administrator pages at `/admin` use local sign-in: the bootstrap admin
(`admin` / `admin`) until setup turns it off, and any token from the token
file pasted on the sign-in page ([Local sign-in](auth.md#local-sign-in)). If
you lock yourself out, `docker compose exec diderot diderot reset-admin`
turns `admin` back on.

For a deployment other people use, sign callers in with your identity
provider (`DIDEROT_AUTH_MODE=oidc`; see [Authentication](auth.md)).

## Running it

- Logs: `docker compose logs -f diderot`.
- Stop, keeping data: `docker compose down`. Delete everything, including
  ingested data, the generated token and keys, and the downloaded model:
  `docker compose down -v`; add `--rmi local` to remove the images the stack
  built as well.
- Back up the database (Postgres holds every event and setting) and the
  `keys` volume (without the config key, pasted secrets can't be read),
  with the stack running. The dump holds everything ingested and the keys
  archive holds secrets, so both are written readable only by you. Keep
  `.env` too if it sets a token or keys.

  ```sh
  (umask 077; docker compose exec -T postgres pg_dump -U diderot --format=custom diderot > diderot.dump)
  (umask 077; docker compose run --rm -T init tar -C /keys -cf - . > diderot-keys.tar)
  ```

  To restore into a fresh stack (empty volumes, after `docker compose down -v`
  or on another machine), load both before the server first starts, then
  rebuild Qdrant's index from Postgres:

  ```sh
  docker compose up -d --wait postgres
  docker compose run --rm -T init tar -C /keys -xf - < diderot-keys.tar
  docker compose exec -T postgres pg_restore -U diderot --no-owner --dbname=diderot < diderot.dump
  docker compose up -d --wait
  docker compose exec diderot diderot qdrant-audit
  ```

  `qdrant-audit` queues every chunk for the embedder again (a few minutes for
  Diderot's own docs). Until it finishes, searches find less.
- Update: `git pull`, then `docker compose up -d --build --wait`. Database
  migrations run when the server starts and can't be undone, so read
  [Upgrading](upgrading.md) first.
- Further settings: sources are configured on their pages under
  `/admin/connectors`, and every setting in the
  [configuration reference](configuration.md) is an environment variable on
  the `diderot` service. To add some without editing tracked files, put them
  in a `compose.quickstart.override.yaml`:

  ```yaml
  services:
    diderot:
      environment:
        DIDEROT_CAPTION_WORKERS: "8"
  ```

  and list both files in `.env`:
  `COMPOSE_FILE=compose.quickstart.yaml:compose.quickstart.override.yaml`.
- The server re-checks the model provider every five minutes with a one-token
  request (`DIDEROT_ASK_READINESS_INTERVAL`) and reports unready while that
  fails.
- The API listens on 127.0.0.1 only and speaks plain HTTP. Before exposing it
  to a network, put a TLS-terminating proxy in front and change the `ports`
  binding. If port 9091 is taken, set `DIDEROT_QUICKSTART_PORT` in `.env`
  and use that port in place of 9091 in the URLs on this page.

## Troubleshooting

- `docker compose up` fails with `service "init" didn't complete
  successfully: exit 1`: run `docker compose logs init`. If it says
  `DIDEROT_QUICKSTART_TOKEN must be at least 32 characters`, the token in
  `.env` is too short. Generate one with `openssl rand -hex 32`, or delete
  the line to use the generated token.
- `docker compose run --rm init cat /keys/api-token` finds no file: `.env`
  sets `DIDEROT_QUICKSTART_TOKEN`, and that is the token.
- Ask answers `503` "Diderot is still being set up", or Slack does: the
  install has not gone live, and the caller is not an administrator (Slack
  never is). Finish `/admin/settings`.
- The `diderot` service is unhealthy: `docker compose logs diderot`. Once
  Ask has a model, a failed model call (`ask gateway readiness`) means a
  wrong key, a model your account cannot use, or no network; the Models
  page's **Test models** says which. A configuration error stops the server
  at startup and names the variable.
- Search and Ask answer that they are not set up: no embedding model, or no
  Ask model, is chosen yet on the Models page.
- `docker compose up --wait` reports the `embedder` unhealthy, and
  `docker compose ps -a` shows it restarting with exit code 137: it ran out
  of memory. Give Docker more, or go back to the default embedding model.
- Ask answers but finds nothing: the source may still be syncing or embedding.
  `/v1/retrieve` shows what is indexed, and the logs show each sync worker's
  progress.

## How this is tested

`just quickstart-smoke` (or `scripts/quickstart-smoke.sh`) builds this stack
with `internal/e2e/quickstart/compose.smoke.yaml`, which adds a
deterministic stub model provider in place of a real one and of the
embedder. Like a fresh install, it sets no token or keys: it reads the
generated token as above, checks that another `init` run keeps the token
and keys, signs in to the browser admin with the token and opens the setup
checklist, checks that search answers it is not set up, adds the stub as a
provider, chooses its embedding model and Ask's model on the Models page as
an installer does, then ingests a small fixture,
waits for it to be embedded, retrieves it, checks that an Ask answer quotes
it, and lists it as an MCP client. The
`quickstart` GitHub Actions workflow runs the same script on every pull
request, with no secrets.

## Beyond one machine

To run Diderot for a team, with your own Postgres and Qdrant, an identity
provider, and Kubernetes or another scheduler, see
[Running in production](production.md).
