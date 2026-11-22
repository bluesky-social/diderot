# Configuration reference

`diderot serve` reads its configuration from environment variables. Parsing
and validation live in [`internal/config`](../internal/config); a few
listener and test settings are read where they are used. Every `DIDEROT_*`
variable that the code reads is listed below. A test
(`internal/config/reference_test.go`) fails when the code and this page
disagree.

Conventions used on this page:

- **Required** means the server refuses to start without it. **Required
  when** names the condition. **Needed when** means that without it the
  feature it belongs to is not started (the reason is logged as an error),
  but the server still starts. Otherwise the variable is optional, and unset
  means the default shown, or the feature it belongs to stays off.
- Setting a variable to the empty string is the same as leaving it unset.
- Feature switches named `*_ENABLED` turn on only for the exact value `true`.
  Other booleans accept Go's `strconv.ParseBool` spellings (`true`, `false`,
  `1`, `0`, ...) and fail startup on anything else.
- Durations use Go syntax (`90s`, `5m`, `12h`).
- Lists are comma-separated unless a row says otherwise.
- Settings marked **lenient** ignore a value that is unparseable or not
  positive and use the default instead of failing startup. Everything else
  fails startup on an invalid value.
- Models are not variables: the providers, the model each job uses (Ask,
  titles, summaries, captions, document extraction) and the embedding model
  are chosen on the Models page ([Inference](#inference)).

Most variables hold plain values. Secrets (tokens, keys, client secrets) are
marked as such. Load them from your secret store rather than a committed file.

## Where a setting lives

Diderot's configuration and credentials live in the database with the other
settings and are managed in the admin UI. A credential may instead come from
the deployment's secret store, through one mechanism: a variable its page
names (`DIDEROT_SLACK_BOT_TOKEN`, `DIDEROT_CONNECTION_<NAME>_TOKEN`), which,
when set, is the field's value. Nobody hand-edits a file or types a variable
name for anything the UI can own.

A setting stays a deployment variable for one of two reasons:

- **Boot.** The server needs it before it can read the database or serve
  the UI safely, or it decides who can reach the UI: an administrator who
  broke it in the UI could not get back in to fix it.
- **Infrastructure.** Wiring that is the deployment's business and the same
  for whoever administers it: addresses, buckets, mounted folders, capacity
  and resource bounds, probes.

Anything else is product configuration and belongs in the admin UI. A few
such settings are still variables or files; the table says which. When one
moves, its variable is removed outright ([Removed](#removed)).

| Variables | Kind | Why |
|---|---|---|
| `DIDEROT_DB_URL`, `DIDEROT_DB_IAM_AUTH` | Boot | The database itself. |
| `DIDEROT_CONFIG_KEYS`, `DIDEROT_CONFIG_KEYS_FILE` | Boot | They decrypt the secrets the database holds; storing them there would defeat them. |
| `DIDEROT_PUBLIC_URL` | Boot | The UI checks every change's origin against it and OIDC redirects to it. |
| `DIDEROT_DEPLOYMENT` | Boot | Decides whether `serve` may run here at all, before the database is opened. |
| `DIDEROT_AUTH_MODE`, `DIDEROT_OIDC_*`, `DIDEROT_AUTH_STATIC_TOKENS_FILE`, `DIDEROT_AUTH_NONE_EMAIL` | Boot | Who can authenticate, to the UI included. A wrong value saved in the UI would lock out the person who would fix it. |
| `DIDEROT_ADMIN_GROUPS`, `DIDEROT_BROWSER_*`, `DIDEROT_TRUSTED_PROXY_*` | Boot | They gate the UI itself: who is an administrator, how the browser signs in, and which proxy's headers are trusted. |
| `DIDEROT_GATEWAY_API_KEY` | Boot | Authenticates the agent gateway in front of `/mcp`. |
| `DIDEROT_LISTEN_ADDR`, `DIDEROT_INTERNAL_LISTEN_ADDR`, `DIDEROT_PAGERDUTY_LISTEN_ADDR`, `DIDEROT_METRICS_LISTEN_ADDR` | Infrastructure | Sockets. |
| `DIDEROT_LEADER_ELECTION`, `DIDEROT_LEADER_BACKEND` | Infrastructure | Process topology. |
| `DIDEROT_QDRANT_*` | Infrastructure | The vector store's address, credentials, TLS and storage shape. |
| `DIDEROT_IMAGES_*` | Infrastructure | The object store. |
| `DIDEROT_MARKDOWN_DIR` | Infrastructure | A folder the deployment mounts; the connector's page sets everything else. |
| `DIDEROT_EMBED_WORKERS`, `DIDEROT_EMBED_BATCH_SIZE`, `DIDEROT_CAPTION_WORKERS`, `DIDEROT_DOC_EXTRACT_WORKERS` | Infrastructure | Capacity. |
| `DIDEROT_ASK_READINESS_TIMEOUT`, `DIDEROT_ASK_READINESS_INTERVAL` | Infrastructure | The readiness probe. |
| `DIDEROT_SESSION_INGEST_TICK`, `_PAGE_SIZE`, `_BATCH_SIZE`, `_CHUNK_BATCH_SIZE`, `_CONCURRENCY`, `_CHUNK_CONCURRENCY` | Infrastructure | Capacity and cadence. |
| `DIDEROT_SESSION_INGEST_MAX_REVISION_BYTES`, `DIDEROT_SESSION_PUSH_MAX_DOCUMENT_BYTES`, `_MAX_CONCURRENT`, `_MAX_CONCURRENT_PER_OWNER`, `_MAX_INFLIGHT_BYTES` | Infrastructure | Resource bounds that protect the process. |
| `DIDEROT_SESSION_PUSH_REVISION_KEY` | Infrastructure | A server secret (an HMAC key), not something an administrator chooses. |
| `DIDEROT_ATLAS_BINARY` | Infrastructure | Where the migration tool is; `serve` runs it at startup. |
| Connector, tool connection and model provider secrets (`DIDEROT_SLACK_*_TOKEN`, `DIDEROT_NOTION_TOKEN`, `DIDEROT_LINEAR_OAUTH_SECRET`, `DIDEROT_LINEAR_TOKEN`, `DIDEROT_ZENDESK_OAUTH_SECRET`, `DIDEROT_DISCORD_BOT_TOKEN`, `DIDEROT_TAVILY_API_KEY`, `DIDEROT_SOURCEBOT_API_KEY`, `DIDEROT_PAGERDUTY_API_TOKEN`, `DIDEROT_PAGERDUTY_WEBHOOK_SECRETS`, `DIDEROT_SESSION_FEED_TOKEN`, `DIDEROT_CONNECTION_<NAME>_TOKEN`, `DIDEROT_PROVIDER_<NAME>_KEY`, `DIDEROT_PROVIDER_<NAME>_SECRET_HEADERS`) | Credential | The secret-store path for a field of the admin UI. |
| `DIDEROT_SELF_SERVICE_GROUPS`, `DIDEROT_AUTHORIZATION_POLICY_PATH` and its file | Product, still in the deployment | Who may manage their own agent sessions, and the grants and channel profiles: policy the Access page shows and does not yet edit. |
| `DIDEROT_SESSION_INGEST_POLICY_PATH` and its file, `DIDEROT_SESSION_INGEST_CODEX_APP`, `DIDEROT_SESSION_INGEST_EXCLUDE_APP` | Product, still in the deployment | Which agent sessions are indexed. |
| `DIDEROT_ASK_ENABLED` | Product, still in the deployment | Whether Ask runs at all. It also decides whether `/readyz` waits on Ask's model. |
| `DIDEROT_SESSION_INGEST_SOURCE`, `DIDEROT_SESSION_PUSH_OWNER_QUOTA_BYTES`, `DIDEROT_SESSION_PUSH_OWNER_QUOTA_REVISIONS`, `DIDEROT_SESSION_PUSH_SUPERSEDED_RETENTION` | Product, still in the deployment | Which session source runs, and push ingest's per-owner quotas and retention. The source choice mounts or unmounts the push route. |

## Core

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_DB_URL` | none | **Required.** Postgres connection string. The role needs DDL on its schema, and `CREATE` on the database for the migration tool's own `atlas_schema_revisions` schema (unless the URL sets a `search_path`, which keeps its table in that schema): the server applies pending migrations at startup. |
| `DIDEROT_DB_IAM_AUTH` | `false` | Log in with an Amazon RDS IAM token instead of a password. `DIDEROT_DB_URL` then names the user and carries no password. The region comes from the RDS hostname or the AWS SDK configuration. |
| `DIDEROT_PUBLIC_URL` | none | **Required.** The browser-reachable origin (`https://diderot.example.com`), with no path. The browser UI checks request origins against it, and notifications link to it. With browser sign-in it must be https, except on a loopback host. |
| `DIDEROT_DEPLOYMENT` | `kubernetes` | `kubernetes` refuses to start `serve` outside a pod, so a laptop pointed at the production database cannot run Slack beside the real deployment and split the bot's events. `standalone` declares a deliberate deployment anywhere else (Compose, a VM). It also changes the leader backend default and admits loopback MCP server URLs. |
| `DIDEROT_LISTEN_ADDR` | `:9091` | Public listener: the `/v1` API, `/mcp`, the browser UI (`/admin`, `/sessions`, `/auth`), and probes (`/healthz`, `/readyz`). |
| `DIDEROT_INTERNAL_LISTEN_ADDR` | `:9092` | Machine listener: the same handlers, except that it answers 404 for `/` and everything under `/admin`, `/sessions`, and `/auth`, before reading any identity header. Point machine clients and probes here. |
| `DIDEROT_PAGERDUTY_LISTEN_ADDR` | `:9093`; `127.0.0.1:9093` under `DIDEROT_AUTH_MODE=none` | PagerDuty webhook listener: `/webhooks/pagerduty` and nothing else. It answers 404 while the PagerDuty connector is disabled ([PagerDuty investigations](#pagerduty-investigations)). |
| `DIDEROT_METRICS_LISTEN_ADDR` | `:9094`; `127.0.0.1:9094` under `DIDEROT_AUTH_MODE=none` | Metrics listener: Prometheus metrics at `/metrics` and Go profiles at `/debug/pprof/`, and nothing else. Neither is served on the other listeners. Let only your metrics scraper reach it. |
| `DIDEROT_LEADER_ELECTION` | `true` | Any value other than `false` keeps election on. With `false`, every process runs the leader-only workers (Slack, embedder, connector syncs, session ingest, scheduled reports) inline. Never run two such processes against the same database: both would run the Slack connector. |
| `DIDEROT_LEADER_BACKEND` | `kubernetes`; `postgres` when standalone | `kubernetes` holds a Lease named `diderot-leader` in the pod's own namespace, or in `POD_NAMESPACE` when it is set; the pod's service account needs a Role there that may get, create, and update `coordination.k8s.io` leases. `postgres` holds a session advisory lock in the application database (Postgres 14+, a direct or session-pooled connection). A standalone deployment must use `postgres`. An election that cannot start (no service account credentials for `kubernetes`, a database older than Postgres 14 for `postgres`) stops `serve` with the reason; failures once it runs are retried. |

Outside the variables above, `serve` also reads `POD_NAME` (the leader
identity), `KUBERNETES_SERVICE_HOST` (to detect a pod), and the AWS SDK's
standard variables for S3 and RDS IAM.

## Vector store and embeddings

The embedding model, and the collection its vectors are in, are chosen on
the Models page ([Models](setup.md#models)), not here: the server starts
without one, and search waits for it.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_QDRANT_ADDR` | none | **Required.** Qdrant gRPC address (`host:6334`). Qdrant holds the search index that retrieval and Ask read and the embedder writes. |
| `DIDEROT_QDRANT_API_KEY` | none | Secret. Qdrant API key, sent with every request. Without TLS it crosses the network in plaintext, and the server warns at startup unless Qdrant is on `localhost` or a loopback IP. |
| `DIDEROT_QDRANT_TLS` | `false` | Connect to Qdrant over TLS 1.3, as Qdrant Cloud and a cluster with `service.enable_tls` require. Qdrant's certificate must name the host in `DIDEROT_QDRANT_ADDR` and chain to a system CA, or to `DIDEROT_QDRANT_CA_FILE`. |
| `DIDEROT_QDRANT_CA_FILE` | system CAs | PEM file of the CA certificates that sign Qdrant's certificate, for a cluster with a private CA. It replaces the system's CAs for Qdrant, and is read at startup: a file that can't be read or holds no certificate stops it. **Requires** `DIDEROT_QDRANT_TLS=true`. |
| `DIDEROT_QDRANT_SHARD_NUMBER` | `1` | Shards used when the collection is created; checked against an existing collection. |
| `DIDEROT_QDRANT_REPLICATION_FACTOR` | `1` | Replication factor used when the collection is created; checked against an existing collection. |
| `DIDEROT_EMBED_WORKERS` | `1` | **Lenient.** Concurrent embedding requests. |
| `DIDEROT_EMBED_BATCH_SIZE` | `32` | **Lenient.** Chunks per embedding request. |

### Qdrant with TLS and an API key

A hosted Qdrant Cloud cluster takes TLS and an API key on its gRPC port,
with a certificate from a public CA, so it needs no CA file:

```sh
DIDEROT_QDRANT_ADDR=<cluster-id>.<region>.<cloud>.cloud.qdrant.io:6334
DIDEROT_QDRANT_TLS=true
DIDEROT_QDRANT_API_KEY=<the cluster's API key>
```

A self-hosted cluster with `service.enable_tls` and a certificate from an
internal CA also needs that CA, mounted as a file:

```sh
DIDEROT_QDRANT_ADDR=qdrant.example.internal:6334
DIDEROT_QDRANT_TLS=true
DIDEROT_QDRANT_CA_FILE=/etc/diderot/qdrant-ca/ca.crt
DIDEROT_QDRANT_API_KEY=<the API key in Qdrant's service.api_key>
```

`serve`, once an embedding model is chosen, and `diderot qdrant-audit` check
Qdrant with one call as they open the collection and say so when it fails,
naming the setting that doesn't match: TLS
off against a Qdrant that requires it, TLS on against one that doesn't, a
certificate no trusted CA signed, or a certificate that doesn't name the
host in `DIDEROT_QDRANT_ADDR`. An API key works without TLS, so it can be
added before TLS is turned on, but unless Qdrant is on a loopback address
the server warns at every start until TLS is on.

## Inference

Model providers, the model each job uses, the embedding model, and Ask's
reasoning effort and tool-turn limit are chosen on the
[Models page](setup.md#models) and stored in the database; the server
starts with none. [Inference providers](design.md#inference-providers)
describes how. A job runs once its feature is on and it has a model.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_PROVIDER_<NAME>_KEY` | none | Secret. A model provider's **API key**, the provider's name in capitals, such as `DIDEROT_PROVIDER_OPENAI_KEY`, used instead of a pasted one; the provider's page names it. |
| `DIDEROT_PROVIDER_<NAME>_SECRET_HEADERS` | none | Secret. A model provider's **Secret request headers**, one `Name: value` per line, used instead of pasted ones. |
| `DIDEROT_GATEWAY_API_KEY` | none | Secret. The bearer an agent gateway in front of `/mcp` presents in place of a caller's own token (see [MCP endpoint](#mcp-endpoint)). Unset, `/mcp` accepts only the `/v1` bearer. It is not a model provider's key. |

## Authentication

[Authentication](auth.md) explains the modes. Each mode needs its own
settings, and the server refuses to start without them: in `oidc` mode, the
default, an issuer and an audience.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_AUTH_MODE` | `oidc` | `oidc`, `static`, or `none`. |
| `DIDEROT_OIDC_ISSUER` | none | **Required when** `DIDEROT_AUTH_MODE=oidc`. The `iss` that tokens must carry. Also the issuer that browser sign-in uses. |
| `DIDEROT_OIDC_AUDIENCE` | none | **Required when** `DIDEROT_AUTH_MODE=oidc`. The access-token `aud`. |
| `DIDEROT_OIDC_SCOPES` | `openid email profile` | Space- or comma-separated. Must include `openid`. The scopes that make your IdP put the email and groups claims in its tokens: the browser sign-in requests them, and in `oidc` mode `/mcp` advertises them to MCP clients ([MCP endpoint](auth.md#mcp-endpoint)). Add `groups` if your IdP releases groups only for that scope. Setting it with no OIDC sign-in configured stops startup. |
| `DIDEROT_OIDC_JWKS_URL` | discovery | Where signing keys are fetched. Unset uses OIDC discovery from the issuer. Set it when keys must come from another host, such as an in-cluster IdP service. |
| `DIDEROT_OIDC_GROUPS_CLAIM` | `groups` | Claim path for group membership: a literal claim name (namespaced URLs work), else dot-separated keys (`realm_access.roles`). |
| `DIDEROT_OIDC_EMAIL_CLAIM` | `email` | Claim path for the email, with the same rules. |
| `DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED` | `true` | Drop the token's email unless it carries `email_verified: true`. Set `false` only when your IdP assigns every email itself. |
| `DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS` | none | Comma- or space-separated bare domains. An OIDC identity (API or browser sign-in) whose email, after the verification rule, is outside them is refused with 403. Matching is exact and case-insensitive; list subdomains explicitly. Setting it with no OIDC surface configured stops startup. |
| `DIDEROT_OIDC_HOSTED_DOMAIN_CLAIM` | none | Claim that must also name an allowed domain, e.g. Google's `hd`. **Needs** `DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS`. |
| `DIDEROT_AUTH_STATIC_TOKENS_FILE` | none | **Required when** `DIDEROT_AUTH_MODE=static`. YAML file that maps SHA-256 token digests to an email and groups. |
| `DIDEROT_AUTH_NONE_EMAIL` | none | **Required when** `DIDEROT_AUTH_MODE=none`. The bare email every request acts as. In this mode every listener must bind a literal loopback IP. |
| `DIDEROT_ADMIN_GROUPS` | none | **Required.** Groups allowed to use the `/admin` browser UI, and Ask before the install is live. A value with no group names in it (`,`) denies everyone. |
| `DIDEROT_SELF_SERVICE_GROUPS` | none | **Required.** Groups allowed to use the owner-bound agent-session routes (`/v1/me/...`, the `/sessions` page). They also need a verified email. A value with no group names in it (`,`) denies everyone. |
| `DIDEROT_AUTHORIZATION_POLICY_PATH` | none | YAML authorization policy: grants to groups and channels, per-channel scope restrictions, and channel profiles. Unset, none of these apply. |

### Browser UI

The browser UI (`/admin`, `/sessions`) has one external identity source: an
authenticating proxy's headers, or the built-in OIDC sign-in. With
`DIDEROT_BROWSER_SESSION_KEYS` set it also has local sign-in in every auth
mode except `none`: the bootstrap admin while it is enabled, and pasted
static tokens in static mode ([auth](auth.md#local-sign-in)).

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_BROWSER_AUTH` | trusted proxy | Empty or `trusted-proxy` reads identity headers from an authenticating proxy. `oidc` is the built-in OIDC sign-in, and proxy headers are then never read on browser routes. Must be unset with `DIDEROT_AUTH_MODE=none`. |
| `DIDEROT_TRUSTED_PROXY_CIDRS` | none | Peers (CIDRs or bare IPs) whose identity headers are honored, matched against the connection's address, never `X-Forwarded-For`. Unset ignores identity headers entirely. **Required when** `DIDEROT_BROWSER_AUTH=trusted-proxy`. A `/0` prefix is refused. |
| `DIDEROT_TRUSTED_PROXY_EMAIL_HEADER` | none | **Required when** `DIDEROT_TRUSTED_PROXY_CIDRS` is set. Header carrying the caller's email. |
| `DIDEROT_TRUSTED_PROXY_GROUPS_HEADER` | none | Header carrying the caller's groups. |
| `DIDEROT_TRUSTED_PROXY_GROUPS_DELIMITER` | none | **Required when** the groups header is set. Separator between groups, for example `\|` or `,`. |
| `DIDEROT_TRUSTED_PROXY_MCP_EMAIL_HEADER` | the email header | Header carrying the caller's email on `/mcp`, which usually arrives through an agent gateway rather than the browser proxy. |
| `DIDEROT_BROWSER_OIDC_CLIENT_ID` | none | **Required when** `DIDEROT_BROWSER_AUTH=oidc`. A web client registered at `DIDEROT_OIDC_ISSUER` with redirect URI `DIDEROT_PUBLIC_URL/auth/callback`. |
| `DIDEROT_BROWSER_OIDC_CLIENT_SECRET` | none | Secret. Empty for a public client. |
| `DIDEROT_BROWSER_SESSION_KEYS` | none | Secret. Turns on the browser's own sign-in, in any auth mode except `none`, where it must be unset. **Required when** `DIDEROT_BROWSER_AUTH=oidc`; without it the bootstrap admin and static-token sign-in are unavailable. Space- or comma-separated base64 32-byte keys (`openssl rand -base64 32`). The first seals new cookies; all of them open existing ones. **Needs** `DIDEROT_PUBLIC_URL`. |
| `DIDEROT_BROWSER_SESSION_KEYS_FILE` | none | A file holding the value of `DIDEROT_BROWSER_SESSION_KEYS`, read at startup, for keys mounted as a file. Set one or the other, not both. |
| `DIDEROT_BROWSER_SESSION_TTL` | `12h` | Absolute session lifetime, at most `168h`. |
| `DIDEROT_BROWSER_SESSION_IDLE_TIMEOUT` | `1h` | A session unused this long ends. At most the TTL. |
| `DIDEROT_BROWSER_SESSION_INSECURE_COOKIE` | `false` | For local development over plain HTTP: drop `Secure` from the session cookie and the sign-in, sign-out, and `/sessions` CSRF cookies. Requires a `DIDEROT_PUBLIC_URL` on a literal loopback IP (`http://127.0.0.1:9091`). |

Setting any `DIDEROT_BROWSER_OIDC_*` variable without
`DIDEROT_BROWSER_AUTH=oidc`, or `DIDEROT_BROWSER_SESSION_TTL`, `_IDLE_TIMEOUT`,
or `_INSECURE_COOKIE` without `DIDEROT_BROWSER_SESSION_KEYS`, fails startup.
The OIDC sign-in also requires `DIDEROT_OIDC_ISSUER`.

### MCP endpoint

`/mcp` is always mounted. Until an embedding model is chosen on the Models
page, its search tools answer that search is not set up. A caller presents
the bearer `DIDEROT_AUTH_MODE` accepts on `/v1` and is that token's identity. With `DIDEROT_GATEWAY_API_KEY` set, an
agent gateway may present that key instead; the caller's identity is then
the `DIDEROT_TRUSTED_PROXY_MCP_EMAIL_HEADER` value from a peer inside
`DIDEROT_TRUSTED_PROXY_CIDRS`, and every tool call fails without one. See
[MCP endpoint](auth.md#mcp-endpoint).

## Ask

Ask is the agent loop behind `POST /v1/ask`, Slack and Discord mentions and
direct messages, and the MCP `ask` tool. Its model and every limit on what it does for one question
(reasoning effort, tool turns, service tier, reply length, and time limits)
are set on the [Models page](setup.md#models).

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_ASK_ENABLED` | off | Turn Ask on. Ask then answers once its model and the embedding model are chosen on the Models page; until then it says it is not set up. Off, the Models page shows Ask as unavailable and says to set this. |
| `DIDEROT_ASK_READINESS_TIMEOUT` | `5s` | **Lenient.** Bound on the readiness probe, a one-token inference that gates `/readyz` while Ask is on. |
| `DIDEROT_ASK_READINESS_INTERVAL` | `30s` | **Lenient.** How often that probe runs. |

Web search and code search are connectors, set up in the admin UI: **Web
search** (`/admin/connectors/tavily`) gives Ask `web_search` and `web_fetch`
through Tavily, and **Code search** (`/admin/connectors/sourcebot`) gives it
`code_search` and `code_read` through a Sourcebot deployment. Each Ask run
offers the tools of the connectors enabled as it starts, so enabling,
disabling, or changing one applies to the next run without a restart.

The MCP servers whose tools Ask calls, such as Grafana or Sentry, are
[tool connections](mcp-servers.md), added on the Connectors page under
**Tool connections**.

## Connector settings

Connectors configured in the admin UI (`/admin/connectors`) keep their
settings in the database. Each secret, such as a Slack token, is either set
by its variable below or pasted on the connector page. When the variable is
set, it is used, and the page shows that the deployment sets it. A connector
still runs only once it is enabled on its page. See [Slack app
setup](slack-app.md) for a walkthrough.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_CONFIG_KEYS` | none | Secret. Comma- or space-separated base64 keys of 32 bytes (`openssl rand -base64 32`) that encrypt pasted secrets with AES-256-GCM before they are stored. The first key encrypts and every key decrypts: to rotate, put a new key first, paste each secret again on its connector page, then drop the old key. Unset, secrets can't be pasted; set their variables instead. |
| `DIDEROT_CONFIG_KEYS_FILE` | none | A file holding the value of `DIDEROT_CONFIG_KEYS`, read at startup, for keys mounted as a file. Set one or the other, not both. |
| `DIDEROT_SLACK_BOT_TOKEN` | none | Secret. The Slack connector's **Bot token**, used instead of a pasted one. A change takes effect when the server restarts. |
| `DIDEROT_SLACK_APP_TOKEN` | none | Secret. The Slack connector's **App token**, likewise. |
| `DIDEROT_SLACK_USER_TOKEN` | none | Secret. The Slack connector's optional **User token**, likewise. |
| `DIDEROT_TAVILY_API_KEY` | none | Secret. The Web search connector's **API key**, likewise. |
| `DIDEROT_SOURCEBOT_API_KEY` | none | Secret. The Code search connector's **API key**, likewise. |
| `DIDEROT_PAGERDUTY_API_TOKEN` | none | Secret. The PagerDuty connector's **API token**, likewise. |
| `DIDEROT_PAGERDUTY_WEBHOOK_SECRETS` | none | Secret. The PagerDuty connector's **Webhook signing secrets**, likewise: comma-separated, so the new and old secret can both be accepted during a rotation. |

A [tool connection](mcp-servers.md)'s **Credential** is likewise set by
`DIDEROT_CONNECTION_<NAME>_TOKEN`, the connection's name in capitals, such
as `DIDEROT_CONNECTION_GRAFANA_TOKEN`; its page names the variable.

## Slack

Slack is configured on its connector page, `/admin/connectors/slack`: its
tokens (unless their [variables](#connector-settings) set them), channel mode,
the operators to notify, and document ingest ([Slack app
setup](slack-app.md)). Enabling it starts the Slack workers on the leader:
live events over Socket Mode, history sync, `@diderot` mentions, DMs, and the
thread-summary shortcuts. The shortcuts' model is chosen on the Models page;
without a provider serving `chat`, they reply that they are not configured.

## Images and documents

The models that caption images and read PDF pages without a usable text
layer are chosen on the Models page. A weak document model silently
corrupts scanned documents.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_IMAGES_BUCKET` | none | S3 bucket that mirrors attachments. Unset turns off the file proxy, image captions, document extraction, and image artifacts from Ask tools, and the Slack or Zendesk connector's Test fails while its **Index documents** is on. A bucket that is set but unusable does not stop startup; the features that need it fail when they first use it. |
| `DIDEROT_IMAGES_REGION` | AWS SDK | Signing region. Unset uses `AWS_REGION` or the shared AWS config. |
| `DIDEROT_IMAGES_ENDPOINT` | AWS | An http(s) endpoint for an S3-compatible store such as MinIO or R2. Credentials come from the AWS SDK's default chain. |
| `DIDEROT_IMAGES_PATH_STYLE` | `false` | Use path-style bucket addressing, which most S3-compatible stores need. |
| `DIDEROT_CAPTION_WORKERS` | `4` | **Lenient.** Concurrent caption requests. |
| `DIDEROT_DOC_EXTRACT_WORKERS` | `2` | **Lenient.** Concurrent document extractions. |

## Notion

Notion is configured on its connector page, `/admin/connectors/notion`.
Enabling it starts discovery every 30 minutes and a walk of the managed
roots every day. Managed roots can be added while it is off.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_NOTION_TOKEN` | none | Secret. The Notion connector's **Integration secret**, used instead of a pasted one. A change takes effect when the server restarts. |

## Linear

Linear is configured on its connector page, `/admin/connectors/linear`: a
credential and the teams to leave out. Enabling it starts an incremental sync
every five minutes, an hourly sweep that removes issues that moved to private
teams, and a daily reconcile. Credentials for another workspace start the
sync over.

The credential is one of two:

- **An OAuth application**, the one to use in a deployment. Create it in
  Linear's Settings under API, turn on client credentials tokens for it, and
  enter its client ID and client secret. Diderot signs in as the application
  with the client credentials grant and the `read` scope, so the sync depends
  on no member's account. Linear gives such a token 30 days; Diderot requests
  another an hour before that, and when Linear answers a request with a 401.
  The application reads every public team in the workspace.
- **A personal API key**, from Linear's Settings under Security & access. It
  reads what the member who made it reads, and stops working when that
  member's account is disabled.

With both set, Diderot uses the application, so you can add it, run the
page's test, and then clear the key. A client ID without its secret is an
error, not a reason to use the key.

The sweep removes every indexed issue the credential can't read, whatever
the reason. Before you change to a credential that sees fewer teams, or
narrow an application's team access in Linear, expect those teams' issues to
leave the index within the hour.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_LINEAR_OAUTH_SECRET` | none | Secret. The Linear connector's **OAuth application client secret**, used instead of a pasted one. A change takes effect when the server restarts. |
| `DIDEROT_LINEAR_TOKEN` | none | Secret. The Linear connector's **API key**, used instead of a pasted one. A change takes effect when the server restarts. |

## Zendesk

Zendesk is configured on its connector page, `/admin/connectors/zendesk`:
the subdomain, an OAuth client, the groups whose tickets are indexed, and
document ingest. Create the OAuth client in Zendesk's Admin Center with the
**Confidential** client kind: Diderot gets its tokens with the client
credentials grant and the `read` scope, so it reads what the client's owner
can. Every reader of `corpus:zendesk` sees what is indexed, so
list only groups all of them may read; tickets that move out of these groups
are removed. Enabling it polls every five minutes within 300 API requests a
minute. A second ticket stream, starting 30 days back when the connector
first syncs, imports recently updated tickets before the full-history
backfill reaches them. **Index documents** extracts the text of PDF, DOCX,
and XLSX attachments, independently of Slack's setting. **Backfill** on the
page reads every ticket and article
from the start again, beginning at the next sync pass.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_ZENDESK_OAUTH_SECRET` | none | Secret. The Zendesk connector's **OAuth client secret**, used instead of a pasted one. A change takes effect when the server restarts. |

## Discord

Discord is configured on its connector page, `/admin/connectors/discord`:
the bot token, how many days of history a channel's first read covers, and
document ingest. The channels to index, and who may read each, are chosen in
the page's **Channels** section ([Discord](discord.md)). Enabling it opens a
gateway connection for live messages, checks every five minutes for
messages it missed, and re-reads the last seven days once a day.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_DISCORD_BOT_TOKEN` | none | Secret. The Discord connector's **Bot token**, used instead of a pasted one. A change takes effect when the server restarts. |

## Markdown folder

The Markdown folder connector indexes the `.md` and `.markdown` files in one
folder the deployment mounts, one document per file. Its page,
`/admin/connectors/markdown`, sets how often the folder is read again and,
optionally, a web address each document links to under its path. The folder
itself is set only by the variable below, never on the page. Sources run on
the leader and any replica can become the leader, so mount the same folder at
the same path on every replica. Hidden files and folders and symlinks are
skipped, a file over 1 MiB or not UTF-8 text is skipped and counted, and a folder
with more than 10,000 Markdown files is not indexed.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_MARKDOWN_DIR` | none | The folder's path. Read where it is used: unset, the connector's test and run fail with "No folder is mounted for this deployment." Diderot only reads the folder, so it can be mounted read-only. |

## Agent sessions

Agent-session summaries come from a session source.
[Session ingest](session-ingest.md) describes the formats and the protocol.
`DIDEROT_SESSION_INGEST_SOURCE` picks the source: `session_feed`, the change
feed the Session feed connector reads (`bsky.session.v1`), or `push`, the HTTP
push endpoint. The two are mutually exclusive.

The feed is configured on the **Session feed** connector page,
`/admin/connectors/session_feed`: its URL, its token, and the public address
of a session viewer that citations link to. The feed is read while that
connector is enabled, and expanding a feed session's hit needs it too. Push
ingest runs on its own, whether or not the connector is enabled.

| Variable | Default | Notes |
|---|---|---|
| `DIDEROT_SESSION_INGEST_SOURCE` | `session_feed` | `session_feed` or `push`. Any other value stops startup. |
| `DIDEROT_SESSION_PUSH_REVISION_KEY` | none | Secret. **Needed when** the source is `push`; at least 32 bytes. Keys the HMAC that mints revision tokens. |
| `DIDEROT_SESSION_PUSH_MAX_DOCUMENT_BYTES` | `67108864` (64 MiB) | Largest pushed document after decompression. |
| `DIDEROT_SESSION_PUSH_OWNER_QUOTA_BYTES` | `4294967296` (4 GiB) | Stored bytes per owner. Exceeding it gets 507. |
| `DIDEROT_SESSION_PUSH_OWNER_QUOTA_REVISIONS` | `20000` | Stored revisions per owner. Exceeding it gets 507. |
| `DIDEROT_SESSION_PUSH_MAX_CONCURRENT` | `8` | Pushes in flight across the process; excess requests get 503 with `Retry-After`. |
| `DIDEROT_SESSION_PUSH_MAX_CONCURRENT_PER_OWNER` | `2` | Pushes in flight per owner; excess requests get 429 with `Retry-After`. |
| `DIDEROT_SESSION_PUSH_MAX_INFLIGHT_BYTES` | `1073741824` (1 GiB) | Bytes buffered across in-flight pushes. |
| `DIDEROT_SESSION_PUSH_SUPERSEDED_RETENTION` | `0` (keep) | How long superseded revisions are kept. Expanding a pruned revision returns a superseded note naming the current one. |
| `DIDEROT_SESSION_FEED_TOKEN` | none | Secret. The Session feed connector's **Token**, used instead of a pasted one. A change takes effect when the server restarts. |
| `DIDEROT_SESSION_INGEST_MAX_REVISION_BYTES` | none | **Needed when** the session feed is read. **Lenient.** Largest accepted session revision after decompression. Larger revisions are quarantined. |
| `DIDEROT_SESSION_INGEST_POLICY_PATH` | built-in | YAML eligibility policy: a `sessionIngest` block with `include`, `exclude`, and `ownerOnly`. An invalid policy, or one that still has a `summarize` block, keeps the worker from starting. The file is read when the server starts; a changed policy is then applied to the sessions already seen, without reading the feed again ([Session ingest](session-ingest.md#when-the-eligibility-policy-changes)). |
| `DIDEROT_SESSION_INGEST_CHUNK_CONCURRENCY` | `20` | Concurrent map and reduce model calls, shared across sessions. |
| `DIDEROT_SESSION_INGEST_CONCURRENCY` | `1` | **Lenient.** Sessions published in parallel per tick. |
| `DIDEROT_SESSION_INGEST_TICK` | `30s` | **Lenient.** Feed poll cadence. |
| `DIDEROT_SESSION_INGEST_PAGE_SIZE` | `100` | **Lenient.** Change rows fetched per page. |
| `DIDEROT_SESSION_INGEST_BATCH_SIZE` | `100` | **Lenient.** Sessions indexed per tick. Values above 500 also fall back to 100. |
| `DIDEROT_SESSION_INGEST_CHUNK_BATCH_SIZE` | `100` | **Lenient.** Summary-chunk events reconciled per tick. |
| `DIDEROT_SESSION_INGEST_CODEX_APP` | none | A client name added to the policy's include list. |
| `DIDEROT_SESSION_INGEST_EXCLUDE_APP` | none | An app name added to the policy's exclude list. |

## PagerDuty investigations

When a PagerDuty incident triggers, Diderot investigates it with Ask and
replies in the alert's Slack thread and as an incident note. PagerDuty is
set up on its connector page, `/admin/connectors/pagerduty`: the API token
and webhook signing secrets (unless their [variables](#connector-settings)
set them), the PagerDuty user notes are posted as, the alerts channel, and
the scopes an investigation may read.

The signed webhook listener runs on `DIDEROT_PAGERDUTY_LISTEN_ADDR` (`:9093`
by default) at `/webhooks/pagerduty`. While the connector is disabled it
answers 404; while it is enabled it checks each signature with
the secrets saved then and queues the page. Investigations run while the
PagerDuty and Slack connectors are both enabled, on a deployment with Ask and
a Grafana [tool connection](mcp-servers.md) that is on; until then pages stay
queued. The connector's Test says which of
these is missing, and while it is enabled its page says what it waits for.

## The client commands

The client commands (`setup`, `doctor`, `ask`, `sessions`; [The `diderot` command](cli.md))
keep their settings in their own file and read no server variable. One
variable belongs to a harness they configure:

| Variable | Notes |
|---|---|
| `DIDEROT_TOKEN` | Secret. The API token Codex sends to a token server: Codex takes a bearer only from the environment, so `setup` registers it to read this variable, and `doctor` checks that it is set. Not read by the server. |

## Development and tests

| Variable | Notes |
|---|---|
| `DIDEROT_TEST_DB_URL` | Postgres for integration tests. `just test` defaults it to the Compose database. Unset, database tests skip. |
| `DIDEROT_TEST_DB_PREFIX` | Overrides the per-checkout key in test database clone names. By default the key is derived from the checkout's path, so several checkouts can share one Postgres. |
| `DIDEROT_QDRANT_TEST_ADDR` | Qdrant for integration tests. `just test` defaults it to the Compose instance. |
| `DIDEROT_ATLAS_BINARY` | Path to the [`atlas`](https://atlasgo.io) binary that applies migrations, in `serve` and the tests. Default `/usr/local/bin/atlas`, where the container image has it; set it when you run `diderot serve` outside the image. `just install-tools` installs it to `~/bin/atlas`. |
| `DIDEROT_E2E_SLACK_CHANNEL_ID` | Channel for the opt-in end-to-end Slack tests (`just test-slack-e2e`). |
| `DIDEROT_E2E_SLACK_BOT_USER_ID` | The bot user those tests mention. |
| `DIDEROT_E2E_SLACK_WORKSPACE_HOST` | The workspace host (`example.slack.com`) those tests log permalinks with. |
| `DIDEROT_E2E_SLACK_BOT_TOKEN` | The bot token the Slack progress test (`just test-slack-progress`) posts its thread and sets its agent session status with. |
| `DIDEROT_E2E_SQL_COMMAND` | Shell command those tests run, with SQL on stdin, to check the deployment's database; it must print psql's default output, for example `psql "$DB_URL" -v ON_ERROR_STOP=1 -P pager=off -f -`. Unset, the database-checking tests skip. |
| `DIDEROT_DISABLE_CAPTION_INJECTION` | `1` disables the Ask caption barrier, so an end-to-end test can show it fails without it. Never set it in a deployment. |

The inference tests behind the `gateway` build tag
(`go test -tags gateway ./...`) send real requests to a model provider. Each
skips unless its provider and its model are named:

| Variable | Notes |
|---|---|
| `DIDEROT_TEST_PROVIDER_URL` | Base URL of an OpenAI-compatible provider or gateway, with any `/v1`. |
| `DIDEROT_TEST_PROVIDER_KEY` | Secret. Its API key. Never printed. |
| `DIDEROT_TEST_PROVIDER_DENIED_KEY` | Secret. Optional: a valid key the provider does not authorize for the embedding model, to check that it is refused. |
| `DIDEROT_TEST_MODEL_ASK` | The model ID each job's test runs on; a job without one skips. |
| `DIDEROT_TEST_MODEL_TITLES` | As above, for titles. |
| `DIDEROT_TEST_MODEL_SUMMARY_MAP` | As above, for session summaries, part by part. |
| `DIDEROT_TEST_MODEL_SUMMARY_REDUCE` | As above, for session summaries, whole session. |
| `DIDEROT_TEST_MODEL_CAPTIONS` | As above, for image captions. |
| `DIDEROT_TEST_MODEL_DOCUMENTS` | As above, for document extraction: a model that reads PDF files on chat completions. |
| `DIDEROT_TEST_MODEL_THREAD_SUMMARIES` | As above, for thread summaries. |
| `DIDEROT_TEST_MODEL_EMBEDDINGS` | As above, for the embedding model. |
| `DIDEROT_TEST_ASK_MODELS` | Comma-separated model IDs the Ask tests in `internal/ask` run on. |
| `DIDEROT_TEST_ASK_REASONING_EFFORT` | Optional: the reasoning effort those tests send. |
| `DIDEROT_TEST_ASK_SERVICE_TIER` | Optional: the service tier those tests send. |
| `DIDEROT_TEST_FORCED_REPORT_MODEL` | A model served on the Responses protocol, for the forced investigation report test. |
| `DIDEROT_TEST_FACTS_REPORT` | Optional: a file `TestGatewayPublishedFacts` writes its report to instead of the test log. |

## Removed

No release has removed a variable yet, so the server refuses none. When a
release removes one, because its setting moved to the admin UI or went
away, there is no fallback: every command that reads the server's
configuration (`serve`, `reset-admin`, and the other server-side commands)
refuses to start while the variable is set, naming it and where its setting
is now, so a half-finished move fails loudly instead of running without the
setting ([Upgrading](upgrading.md#settings-that-moved-or-were-removed)).
Each removed variable is then listed here with where its setting went.
