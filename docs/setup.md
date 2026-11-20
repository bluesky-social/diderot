# Setup

A fresh install starts in **setup**: ingestion and embedding run, but only
administrators can use Ask. The setup checklist at `/admin/settings` says what
is done, what is missing, and links to the page that does each step; until
the install goes live, `/admin` opens on it instead of the Overview. When
every required step is done, an administrator goes live.

## First sign-in

The server needs a few settings before anyone can reach the admin UI: the
database, Qdrant, the listen addresses, `DIDEROT_PUBLIC_URL`, the auth mode,
`DIDEROT_ADMIN_GROUPS` and `DIDEROT_SELF_SERVICE_GROUPS`, and
`DIDEROT_BROWSER_SESSION_KEYS` (generate one
with `openssl rand -base64 32`). `serve` refuses to start without the
required ones and names what is missing. Connector secrets, such as
Slack's tokens, need `DIDEROT_CONFIG_KEYS` to be pasted on their pages, or
their own variables set in the deployment
([connector settings](configuration.md#connector-settings)). The
[quickstart](quickstart.md) sets all of them. Everything else is on the
setup pages, models included: the server needs none to start.

A fresh install has a **bootstrap admin**: open `/admin` and sign in as
`admin` / `admin`. It works in every auth mode except `none`, so the first
operator never has to set up an identity provider for the browser, or hand
out a token, just to reach the UI. It needs `DIDEROT_BROWSER_SESSION_KEYS`;
without it the sign-in page says so, and `serve` logs it at startup. The auth
mode itself must still be configured, or `serve` refuses to start: `static`
with its token file, or `oidc` with `DIDEROT_OIDC_ISSUER` and
`DIDEROT_OIDC_AUDIENCE`.

The bootstrap admin is an administrator only while it is enabled, which the
server checks against the database on every request it makes. While it is
enabled, every admin page shows a red banner and `serve` logs a warning on
each start. Turning it off is a required step.

## The checklist

Required steps:

| Step | Page | Done when |
|---|---|---|
| Organization | `/admin/settings/organization` | A name is saved. The name appears in Ask's built-in tool descriptions and in Slack's replies to external members, without a restart (other replicas pick it up within 15 seconds). Prompts can use both values as the `{{ORG_NAME}}` and `{{ORG_DESCRIPTION}}` [variables](prompts.md#variables). Neither value may span lines or contain `{{` or `}}`. The optional **Operational dashboard** is a link every admin page shows at the bottom. |
| Models | `/admin/settings/models` | The last test passed, on the models in use now. The page adds model providers, and chooses the embedding model, the model each job uses, and Ask's limits ([Models](#models)). Saving runs the test, and so does **Test models**: one tiny request to each saved model and the embedding model, within 30 seconds, recording pass or fail per job, never the provider's response. |
| Sources | `/admin/connectors` | A source is on: a connector (Slack, Discord, Notion, Linear, Zendesk, the session feed, or a Markdown folder), or agent-session push ([Agent-session ingest](session-ingest.md)). Fill in a connector's page and click **Turn on** ([Slack app setup](slack-app.md) covers Slack, [Discord](discord.md) Discord). While setup is unfinished, a source's page links back to the checklist and on to the next step. Push counts while someone may push and pushed sessions are summarized; otherwise the step says why it doesn't. |
| Prompts | `/admin/prompts` | The active prompt release is not the shipped example, or an administrator clicked **Use the example as-is**. |
| Access | `/admin/settings/access` | An administrator clicked **Mark access as reviewed** on the summary of who can use Diderot: the auth mode, browser sign-in, administrator and self-service groups, the authorization policy file, and how many Slack channels have Ask on. |
| Replace the bootstrap admin | `/admin/settings` | The bootstrap admin is off. |

To turn the bootstrap admin off, first sign in another way: through your
identity provider (`DIDEROT_BROWSER_AUTH=oidc`), your authenticating proxy,
or, in static mode, by pasting an API token on the sign-in page. The step
shows its button only to another sign-in, which proves another administrator
can reach the page before the default account goes away. Turning it off is permanent
from the UI; see [Locked out](#locked-out).

Optional steps are shown with their state but never required: web search
(Tavily, `/admin/connectors/tavily`), code search (Sourcebot,
`/admin/connectors/sourcebot`), MCP tools for Ask, scheduled reports, and
PagerDuty investigations (`/admin/connectors/pagerduty`). The connector steps
are done while their connector is on.

Every action posts the version of the settings the page loaded. If another
administrator changed them since, the action is refused and the page asks
you to look again. Every change is recorded in the audit log
(`/admin/operations/audit`).

## Models

`/admin/settings/models` sets up every model Diderot uses. The server starts
with none: until there is an embedding model, search and Ask answer that
setup is not finished and indexed content waits to be embedded; until a job
has a model, it does not run. Everything here applies without a restart, on
every replica within about 15 seconds.

**Providers.** **Add a provider** asks for its kind (**OpenAI-compatible**,
for OpenAI and anything that speaks its API: gateways and proxies such as
LiteLLM or OpenRouter, Gemini's OpenAI-compatible API, local servers such as
Ollama, vLLM, LM Studio or text-embeddings-inference; or **Anthropic**), a
name, its base URL (with any `/v1`), and its API key. The key is stored
encrypted, which needs `DIDEROT_CONFIG_KEYS`; to keep it in your secret
store instead, leave it empty and set `DIDEROT_PROVIDER_<NAME>_KEY` in the
deployment. For a server that takes no key (a local model server, an
embedder beside Diderot), tick **It takes no API key**: requests then carry
none. A provider that should have a key and has none is not sent requests
without one. **Extra request headers** are sent with every request to the
provider, one `Name: value` per line; a header that is a credential goes
under **Secret request headers**, which are stored encrypted like the key
and never shown again (or set `DIDEROT_PROVIDER_<NAME>_SECRET_HEADERS`).
**Add and turn on** lists the provider's models and turns it on when that
works. Its page shows
the models it offers, with what its list says about each (name, context,
image input, price, retirement date), and takes **Models offered**: leave it
empty to offer every model the provider lists, or enter IDs to offer only
those, models its list lacks, or every model of a provider with no list (a
local embedding server, for example). The kind and base URL can't be changed
later; a new address is a new provider. Diderot lists each provider's models
again every 10 minutes. A provider that is turned off or removed stops the
jobs whose model it serves until they are given another.

**Published model facts.** What a model takes (images, tools, a
temperature, which reasoning efforts), its limits, its price and when its
provider retires it are published, so Diderot looks them up instead of
trying requests to find out. The leader reads two public lists once a day
and keeps them in the database: [models.dev](https://models.dev), and
LiteLLM's model list for the two things models.dev has no field for, the
date a provider retires a model and the size of an embedding model's
vectors. The requests ask for the lists and say nothing about your server.
A list that can't be read keeps its last copy, and the Models page says so
only after a week of failures. **Turn published facts off** on a server
with no way out to the internet: facts then come from each provider's own
list and each job's test, as below.

Where sources say different things, the first of these wins: what an
administrator set on the model's page, the provider's own list, the
published catalog, and last what the job's test found. A model is matched
to a catalog entry only when its ID is exactly one the catalog lists for
the provider that serves it, or for its maker. A gateway's route suffix
(`gpt-4o:api`) is read through when the gateway lists the model without it
or the suffix names a provider, and so is a provider prefix
(`vertex_ai/gemini-pro`, `accounts/fireworks/models/...`). Anything looser,
such as a dated snapshot or another spelling, is offered on the model's
page as a suggestion for you to confirm, and is not used until you do. A
model no catalog lists is not an error.

When a job is first given a model, what is published goes on the model's
entry and the job's test finds the rest (on a gateway, the protocol it
serves the model on). After that nothing changes what the entry says except
you: the model's page shows each fact, where it came from, and what is
published now when that differs, with **Use the published facts** to take
them, which lists what would change for the jobs using the model first.

**Retirement.** A model in use that its provider's list or the catalog
marks deprecated, or that retires within 90 days, needs attention on the
Models page and the Overview, naming the job ("Image captions:
gemini-3.6-flash retires on 2026-11-19 (tomorrow) ... Choose another
model"). A retirement further off is a note under the job. When a gateway
does not say who serves a model, a retirement at one of the clouds that
resell it is reported too, until you say on the model's page who serves it.
The leader also logs it and sets the `diderot_model_attention{job}` metric,
for an alert to carry it to whoever does not open these pages.

**Price and spend.** Each job shows its model's price per million tokens
when it is published. Ask and document extraction also show what their
recorded use over the last 30 days came to; the other jobs record none.

**Embeddings.** The embedding model turns every indexed passage, and every
question, into the vectors search compares. Choosing it sends it one passage
to learn its vectors' size, then creates a collection for them in Qdrant,
named after the model and the size. An install whose passages were embedded
before (an upgrade) either keeps them, by entering the existing collection
under **Keep an existing collection**, or confirms that every passage is
embedded again, in which case search stays off until every passage is in
the new collection. Choosing the model already in use changes nothing.
Choosing another model later always asks to confirm: every passage is
embedded again into that model's own collection while search keeps
answering from the present one, without the passages indexed in the
meantime, and search moves to the new collection by itself once it holds
every passage. The Models page shows how far that is, and the change can't
be undone part way. The old collection is left in Qdrant for the operator
to delete.

**Model for each job.** Ask, titles for Ask conversations and agent
sessions, session summaries (map and reduce), image captions, document
extraction, and Slack thread summaries each take any model the enabled
providers offer, or **Off**. A job that the deployment does not run says
why (a feature switched off, or no file storage) and has no list. Ask also
has its **Reasoning effort** (the model's default, or one of `none`,
`minimal`, `low`, `medium`, `high`, `xhigh`, `max`) and its **Maximum tool
turns** per question (1 to 50, default 8). Under **Speed, length and time
limits** are the rest of what bounds one question: the provider's **Service
tier**, the longest reply in tokens (default 32768), how long one model
request (5 minutes) and one question (10 minutes) may take, and how long the
investigator Ask hands live-telemetry questions to may research (4 minutes)
and then write its report (2 minutes). Each time is typed as a length such
as `90 seconds`, from 10 seconds to 1 day. A change applies to the next
question, on every server, within a minute. Session summaries run once both
of their jobs have a model here; an administrator can pause them under
Operations, Jobs and maintenance.

**Save** first sends each newly chosen model that job's own small request,
and Ask's model the chosen effort; if one fails
(`model-x didn't pass the test for Ask: HTTP 404 Not Found. Nothing was
saved.`), nothing is saved. The test finds what each job needs to know:
which protocol the model answers on (Responses or Messages for Ask), whether
it takes images (captions and PDF pages), and the chat request shape it
accepts (`max_tokens` or `max_completion_tokens`, with or without a
temperature). After a save every model is tested once more and the result
recorded, as **Test models** does.

**What Diderot knows about each model** lists every model a job has been
given. A model's page shows what its tests found and lets an administrator
correct it, or set what no test finds: the reasoning efforts it accepts,
extra fields for its chat requests (a thinking budget, for example), the
reasoning token allowance added to chat output limits, and Responses prompt
cache retention.

The page, and the Overview, say when something needs attention: a provider
whose model list fails, a job whose saved model can't be used, or a model
in use that its provider lists as deprecated or retiring, or that its
provider no longer lists at all.

A changed model restarts the leader's background work (Slack and the
workers) with the new models, as a new leader term would; a Slack question
being answered right now may need to be asked again. A changed Ask model is
built into Ask on every replica; a question already running through the API
or MCP finishes on the model it started with. Every replica's readiness
check follows the Ask model: `/readyz` waits for one passing request to it,
and answers 200 at once while Ask has no model.

Some changes are refused or need confirming, because they stop work in
progress. Turning a provider off names the jobs it serves, and search when
it serves the embedding model, and asks. A provider that anything still uses
can't be removed, nor its key while it is on. On a model's page, a change
to what the jobs using the model send is listed and asks to be confirmed;
one that would stop such a job (unticking **Accepts images** on the caption
model) is refused until the job has another model. A job's test never
changes what is set there: it only fills in what is not known yet.

## Go live

**Go live** unlocks when every required step is done. It records who went
live and when. Until then:

- `/v1/ask` and the `/mcp` `ask` tool admit administrators only.
  Everyone else gets "Diderot is still being set up" (HTTP 503, or the tool's
  error).
- Slack answers every mention or DM with the same notice, administrators
  included: a Slack user carries no Diderot groups, so administrators try Ask
  over MCP or the API during setup.
- Scheduled reports and PagerDuty investigations do not run; their work waits.

Retrieval (`/v1/retrieve`, `/v1/list`, `/v1/expand-chunk`, and the MCP
corpus tools), ingestion, embedding, and the admin UI work throughout.

**Go back to setup** returns a live install to setup, for example to rework it
without users. After going live, a step that becomes incomplete (the last
source turned off, say) shows on the checklist but takes nothing offline.

Every replica caches the state and checks for changes every 15 seconds, so
a change made on one replica reaches the others within that time.

## Locked out

`diderot reset-admin` re-enables the bootstrap admin. It runs where the
server runs, with the server's configuration and database access (in
Kubernetes, `kubectl exec` into a pod; in the quickstart,
`docker compose exec diderot diderot reset-admin`), and is recorded in
the audit ledger. Sign in as `admin` / `admin`, restore another
administrator, and turn it off again.
