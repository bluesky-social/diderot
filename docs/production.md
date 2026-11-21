# Running Diderot in production

This page is for deploying Diderot for a team: what it needs, how its parts
behave, and a complete Kubernetes example. The [quickstart](quickstart.md)
covers one machine with Docker Compose; [Setup](setup.md) covers what to do
in the admin UI once the server is up. Every `DIDEROT_*` variable named here
is in the [configuration reference](configuration.md).

## What you run

**The server.** One image runs everything: the HTTP API, the browser UI, the
MCP endpoint, and the background workers. Each release is published as
`ghcr.io/bluesky-social/diderot:v<version>`, for `linux/amd64` and
`linux/arm64`, and every commit to `main` under its full commit SHA.
`latest` is the newest release; deploy a version tag rather than `latest`,
so a restart cannot upgrade the server, and its database, on its own. Each
image has a signed build provenance attestation; check one with
`gh attestation verify oci://ghcr.io/bluesky-social/diderot:v<version> --repo bluesky-social/diderot`.

To run a change of your own, build it from a release tag with the
repository's `Dockerfile`:

```sh
git checkout v<version>
docker build --build-arg VERSION=v<version> -t registry.example.com/diderot:v<version> .
```

The build cross-compiles, so one builder makes both architectures without
emulation:
`docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=v<version> -t registry.example.com/diderot:v<version> --push .`

The image is `gcr.io/distroless/static` with the static `diderot` binary at
`/usr/local/bin/diderot` (the server and its maintenance commands) and the
[Atlas](https://atlasgo.io) community edition at `/usr/local/bin/atlas`,
which applies the migrations embedded in `diderot`. The entrypoint is
`diderot` and the default argument `serve`; pass another command
(`qdrant-audit`, `reset-admin`) to run that instead. The image's health
check runs `diderot probe healthz`; readiness (`probe readyz`) is the
orchestrator's to schedule, since it can cost a model call. The container
runs as `nonroot`, UID and GID 65532, and has no shell. Diderot's licences and those of everything
compiled into it are under `/usr/share/doc/diderot/`.

**Postgres 14 or later.** Postgres holds every event, setting, secret (sealed),
and Ask conversation; it is the source of truth. The Postgres leader election
needs 14 for `idle_session_timeout` and refuses an older server; the project
tests against Postgres 18.

- The server applies pending migrations every time it starts, so its role
  needs to create tables, not just write rows. The simplest grant is to make
  it the owner of the database. Atlas, which applies the migrations, keeps
  its revision table in a schema of its own, `atlas_schema_revisions`. A
  role that doesn't own the database therefore needs `CREATE` on the
  database as well as on the schema the tables go in, unless
  `DIDEROT_DB_URL` sets `search_path` (then Atlas's table goes in that
  schema too).
- Each replica holds up to 32 pooled connections, one more for `LISTEN`,
  and, with the Postgres leader election, one for the election's lock. The
  leader holds one more while Slack is on, to hear when a channel is blocked
  or unblocked.
  Commands you run beside it (`qdrant-audit`, `reset-admin`) open
  their own.
- Connect directly or through a session-mode pooler. Every replica keeps a
  `LISTEN` connection, migrations take an advisory lock, and the Postgres
  election holds a session-level advisory lock on a dedicated connection; a
  transaction-mode pooler (PgBouncer's `pool_mode = transaction`) breaks
  all three.
- TLS and certificate checks are set in the URL (`sslmode=verify-full`).
  Amazon RDS IAM authentication is supported (`DIDEROT_DB_IAM_AUTH`).

**Qdrant.** Qdrant holds the vectors that retrieval searches; Postgres can
rebuild them ([Backup and restore](#backup-and-restore)). The project runs
Qdrant 1.20. Diderot talks to its gRPC port (6334) only. It creates one
collection when an administrator chooses the embedding model on the Models
page, named after the model and its dimension
(`diderot_chunks_<model>_<dim>`). Choosing another model starts a new
collection beside the old one and fills it. An API key
(`DIDEROT_QDRANT_API_KEY`) and TLS
(`DIDEROT_QDRANT_TLS`, `DIDEROT_QDRANT_CA_FILE`) are supported; see
[Qdrant with TLS and an API key](configuration.md#qdrant-with-tls-and-an-api-key).
Each replica checks Qdrant as it opens the collection. Until it can open
the collection, search is off and the replica says so in its log.

**A model provider.** Providers, their keys and models are set up on the
Models page after the server starts
([Inference providers](design.md#inference-providers)); the deployment
configures none. You need at least an embedding model served over the
OpenAI embeddings API, and, for Ask, a model served over the OpenAI
Responses API or the Anthropic Messages API. Every replica embeds questions
for retrieval and runs the readiness check, so every replica needs to reach
the providers. A key may be pasted on the provider's page (pasting needs
the config key, `DIDEROT_CONFIG_KEYS`) or set by
`DIDEROT_PROVIDER_<NAME>_KEY` from your secret store.

**Object storage, if you want attachments.** Image captions and PDF, DOCX,
and XLSX text extraction mirror files into an S3-compatible bucket
(`DIDEROT_IMAGES_BUCKET`; `DIDEROT_IMAGES_ENDPOINT` and
`DIDEROT_IMAGES_PATH_STYLE` for MinIO, R2, and the like). Credentials come
from the AWS SDK's default chain: `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY`, a web identity token, or an instance role. Startup
fails without a region (`DIDEROT_IMAGES_REGION` or `AWS_REGION`), but the
server doesn't try the bucket until it mirrors a file, so a missing bucket
or a denied permission shows up later as failed captions and extractions.
Check that the server's credentials can read and write the bucket. Without
a bucket, attachments are not indexed.

**Outbound access** to whatever the connectors you enable talk to: Slack
(Socket Mode is an outbound WebSocket, so Slack needs no inbound route),
Notion, Linear, Zendesk, Tavily, your Sourcebot, and the servers of your
[tool connections](mcp-servers.md).

## Replicas and the leader

Every replica serves HTTP, answers Ask (`POST /v1/ask`, and so `diderot
ask`, and the MCP `ask` tool), checks the model provider for readiness, and
polls the database for setting changes. Work that must run once per
deployment runs only on the elected leader: the Slack connector, and with
it the answers to `@diderot` in Slack, the embedder, every connector's
sync, session ingest and summaries, scheduled reports, PagerDuty
investigations, and the workers that keep Qdrant in step with Postgres.

**What more replicas buy.** Behind a load balancer, Ask over the API and
MCP, retrieval, `list`, `expand`, and the UI spread over the replicas. Each
runs its own Ask turns, an MCP `ask` task started on one replica can be
polled on any, and two requests that continue one session never run at
once, whichever replicas they reach (the second waits for the first, as it
would on one replica). A second replica is also a standby for the leader's
work. What more replicas don't buy: the leader's work runs on one replica
at a time, so Slack answers, ingest, and embedding have one pod's capacity.

**Operator actions work on every replica.** The Slack channel controls
(the channel list, the per-channel switches, blocking a channel, history replays,
inviting and removing the bot) write the database, which the leader's
workers follow, and call Slack with the saved tokens, so the pages answer
the same whichever replica serves them. While Slack is off they say so. A
block or unblock reaches the leader's ingest at once. **Start re-embed** under
Operations, Jobs and maintenance, records the request in the database and the
leader runs it. A request made while no replica leads waits for the next
leader. A run the leader stops as it gives up its work ends as `canceled`;
one whose leader dies shows as `stalled` once its heartbeat is 30 seconds
old. In either case, request the re-embed again. Progress reads the same
from every replica. Each replica also sends the
operators' Slack DMs about the API and MCP requests it serves.

**Each replica connects to the tool connections itself**
([Tool connections](mcp-servers.md)), when it starts and again when the Ask
model or a connection changes. A server that is down for one replica is
logged there and left out of that replica's Ask until the replica connects
to it again, five minutes later; the other replicas keep it.

### Leader election

`DIDEROT_LEADER_BACKEND` picks one of two mechanisms:

- **`kubernetes`** (the default when `DIDEROT_DEPLOYMENT=kubernetes`, which
  is itself the default): a Lease named `diderot-leader` in the pod's
  namespace, or in `POD_NAMESPACE`. Each pod identifies itself by `POD_NAME`.
  The pod's service account needs `get`, `create`, and `update` on
  `leases` in `coordination.k8s.io` (the Role in the example below). The
  Lease lasts 15 seconds and is renewed every 2.
- **`postgres`** (the default when `DIDEROT_DEPLOYMENT=standalone`): a
  session advisory lock held on a dedicated connection. The holder checks
  it every 2 seconds and stops leading if a check fails or runs late, the
  server drops a silent lock session after 15 seconds, and a new holder
  waits 9 seconds before it starts work. It needs no permissions beyond the
  database's, which makes it the choice outside Kubernetes (VMs, Compose,
  other schedulers), or in a cluster where you can't grant the Role.

Neither has fencing tokens: a leader that freezes (a suspended VM, say) for
longer than its term can act once on waking before it notices. An election
that can't start (no service account token for `kubernetes`, Postgres older
than 14 for `postgres`) stops the server with the reason.

`DIDEROT_DEPLOYMENT=kubernetes` also refuses to start `serve` outside a pod
(it checks `KUBERNETES_SERVICE_HOST`), so a laptop holding production
credentials can't join the deployment and split its Slack events. Set
`DIDEROT_DEPLOYMENT=standalone` anywhere else.

**Failover.** A leader that stops cleanly gives up the Lease or lock at
once. With `kubernetes` another replica takes the Lease at its next attempt,
within a few seconds, and leads at once; with `postgres`, it leads 9 seconds
after it takes the lock. A leader that dies leaves a Lease to expire (15
seconds after its last renewal). A Postgres lock is freed as soon as Postgres
sees the connection close, which a killed process does at once, or after 15
seconds of silence when the network or the process hangs; the successor
then waits its 9 seconds. The new leader starts every worker from scratch,
and each resumes from its durable state: Slack's tail sweep fills the gap,
and unembedded chunks wait in Postgres. When a leader stops cleanly, a
Slack answer it is writing finishes on the old process while the new leader
starts. The leader logs `became leader`. To see which replica leads, read the Lease's
holder (`kubectl get lease diderot-leader -o jsonpath='{.spec.holderIdentity}'`),
or, with `postgres`, ask the database:

```sql
SELECT a.application_name FROM pg_locks l JOIN pg_stat_activity a USING (pid)
WHERE l.locktype = 'advisory' AND l.granted AND a.application_name LIKE 'diderot-leader %';
```

**Settings reach every replica by polling.** Connector settings, model
choices, and the setup state live in the database, and every replica
checks them every 15 seconds. When an enabled connector's settings change,
the leader restarts that connector with them; when a model choice changes,
it restarts all of its work. When the Ask model changes, every replica
builds its Ask again; a turn already running finishes on the model it
started with.

**The Markdown folder** (`DIDEROT_MARKDOWN_DIR`) is read by whichever
replica leads, so mount the same folder at the same path on every replica.

`DIDEROT_LEADER_ELECTION=false` runs the leader's work in every process with
no election. Use it only for a single process that nothing else will ever
run beside; two such processes both run the Slack connector.

## Listeners

All four listeners speak plain HTTP; terminate TLS in front of them.

| Port | Setting | Serves | Who should reach it |
|---|---|---|---|
| 9091 | `DIDEROT_LISTEN_ADDR` | Everything: `/v1`, `/mcp`, the browser UI (`/`, `/admin`, `/sessions`, `/auth`), `/healthz`, `/readyz` | People, through your TLS ingress or authenticating proxy, and nothing else |
| 9092 | `DIDEROT_INTERNAL_LISTEN_ADDR` | The same, except that `/`, `/admin`, `/sessions`, and `/auth` answer 404 before any identity header is read | Machine clients: scripts and MCP clients inside your network, an agent gateway for `/mcp`, health probes |
| 9093 | `DIDEROT_PAGERDUTY_LISTEN_ADDR` | `POST /webhooks/pagerduty`, signed; 404 while the PagerDuty connector is disabled | PagerDuty, and only if you use PagerDuty investigations |
| 9094 | `DIDEROT_METRICS_LISTEN_ADDR` | `/metrics` and `/debug/pprof/` | Your metrics scraper only |

- **Never expose 9094.** It has no authentication, and pprof serves
  profiles of the running process.
- **9091 trusts identity headers** from peers inside
  `DIDEROT_TRUSTED_PROXY_CIDRS` when the browser UI sits behind an
  authenticating proxy ([Authentication](#authentication)). Anything inside
  those CIDRs that can reach 9091 can sign in as anyone, so let only the
  proxy reach it.
- The webhook listener can't be turned off. Two replicas that share a
  network namespace (two processes on one host, or `hostNetwork`) need
  different addresses for every listener, this one included
  (`DIDEROT_PAGERDUTY_LISTEN_ADDR`).

### Health

- **`/healthz`** answers 200 whenever the listener is up. Use it for
  liveness and startup.
- **`/readyz`** answers 200 once the replica has started, except while
  `DIDEROT_ASK_ENABLED=true`: then it answers 503 until the replica has
  built its Ask once, which connects to each
  [tool connection](mcp-servers.md) that is on (one that doesn't answer
  delays it by that connection's request timeout), and until a request
  of a few tokens to Ask's model succeeds. It repeats that request every
  `DIDEROT_ASK_READINESS_INTERVAL` (30 seconds by default; each one is
  billed), and answers 503 while it fails. It also answers 503, with the
  reason in its body (`reasons`) and in the replica's log, while the
  replica can't run the models as they are saved: a provider that is on
  and in use has no key (its `DIDEROT_PROVIDER_<NAME>_KEY` was left out, or
  the saved key can't be read), a job's saved model has no usable entry,
  jobs are saved with no embedding model while passages have vectors (the
  `models` row was overwritten), or the embedding model's Qdrant
  collection can't be opened or has another shape. So a rollout stops when
  its new replicas would come up with search or a job off, as it does when
  they can't start. None of these can be caused from the admin pages, which
  refuse the changes that would cause them. A provider an administrator
  turned off, a job given no model, and a server nobody has set up yet are
  all ready. Postgres is checked once, at startup, and Qdrant when the
  collection is opened, and never again: during a database outage `/readyz`
  stays 200 while requests fail. A provider outage or a revoked key, on the
  other hand, makes every replica unready at once, which takes the UI and
  the API out of your load balancer even though retrieval doesn't need that
  model. That is what you want during a rollout (a bad key stops it); if you
  would rather stay up, probe readiness with `/healthz` and alert on
  `diderot_ask_gateway_last_success_timestamp_seconds` instead.
- Probe port 9092 with HTTP probes. `diderot probe healthz` and
  `diderot probe readyz` are the same checks as exec commands, for a
  runtime that can only run a command in the container (the image has no
  shell or curl): they request the public listener, `DIDEROT_LISTEN_ADDR`
  (with `127.0.0.1` in place of an empty or all-interfaces host), with a
  2-second timeout, and exit non-zero on anything but a 2xx.
- **Startup.** Nothing listens until the server has applied migrations
  (bounded at 2 minutes), loaded its settings, and seeded the prompt
  catalog. On an empty database this takes
  seconds, since nearly the whole schema is one baseline migration; give the startup probe
  a few minutes anyway, for the migrations a later release brings. A
  failure in any of these steps exits the process with the reason in its
  log. Qdrant is not one of these steps. Once an embedding model is chosen,
  the replica opens the model's collection after it starts listening and
  tries again every 15 seconds, and `/readyz` answers 503 until the
  collection is open.
- **Shutdown.** On SIGTERM a replica starts no new Ask turn: a `/v1/ask`
  request or MCP `ask` call that reaches it from then on is refused with
  "ask is unavailable on this replica; retry" (a 503 for `/v1/ask`), and
  each connection closes once it has answered the request in progress, so
  the client's next request reaches another replica. What is already in
  progress has until one deadline, 14 minutes 30 seconds after SIGTERM, to
  finish: the leader's work drains first, Slack answers among it, and then
  the replica's Ask turns (`/v1/ask` requests and MCP `ask` tasks, whose
  answers any replica's `tasks/get` returns). The listeners serve requests
  all the while, and `/readyz` stays 200. At the deadline the Ask turns
  still running are cancelled, with 5 seconds to end, which frees their
  sessions for another replica at once; a `/v1/ask` cut off this way
  answers 500, and an MCP `ask` task fails with "task execution
  interrupted; retry ask". Then the listeners stop, giving the requests in
  progress 10 seconds before closing their connections. A scheduled report
  or PagerDuty investigation in progress stops at SIGTERM, and the next
  leader runs it again. A replica with nothing in progress exits within
  seconds. Give the pod a 900-second termination grace period; a shorter
  one can cut off an answer being written.

## Configuration and secrets

A minimal environment for a deployment with OIDC for the API and the
built-in OIDC sign-in for the browser:

```sh
DIDEROT_DB_URL=postgres://diderot:<password>@db.example.com:5432/diderot?sslmode=verify-full
DIDEROT_QDRANT_ADDR=qdrant.example.com:6334
DIDEROT_QDRANT_API_KEY=<qdrant api key>
DIDEROT_QDRANT_TLS=true
DIDEROT_PUBLIC_URL=https://diderot.example.com
DIDEROT_ASK_ENABLED=true
DIDEROT_AUTH_MODE=oidc
DIDEROT_OIDC_ISSUER=https://idp.example.com/
DIDEROT_OIDC_AUDIENCE=diderot
DIDEROT_ADMIN_GROUPS=diderot-admins
DIDEROT_SELF_SERVICE_GROUPS=diderot-users
DIDEROT_BROWSER_AUTH=oidc
DIDEROT_BROWSER_OIDC_CLIENT_ID=diderot-web
DIDEROT_BROWSER_OIDC_CLIENT_SECRET=<web client secret>
DIDEROT_BROWSER_SESSION_KEYS_FILE=/etc/diderot/keys/browser-session-keys
DIDEROT_CONFIG_KEYS_FILE=/etc/diderot/keys/config-keys
```

Outside Kubernetes, add `DIDEROT_DEPLOYMENT=standalone`. Models are not
set in the environment: add a provider and choose the models on the Models
page once the server is up. Choose the embedding model before you index
much: choosing another one later embeds everything again.

**Secrets** are environment variables, except three that can be files: the
two keyrings (`DIDEROT_CONFIG_KEYS_FILE`, `DIDEROT_BROWSER_SESSION_KEYS_FILE`)
and the static token file. Everything else (the database password, the
Qdrant API key, the OIDC client secret, `DIDEROT_GATEWAY_API_KEY`, and the
model provider and connector secrets a variable sets) is read from its
variable only. Every file and variable
is read once, at startup.

**The config key** (`DIDEROT_CONFIG_KEYS`) encrypts the secrets that
administrators paste in the admin UI: connector secrets, model provider
keys, and tool connections' credentials (AES-256-GCM, in Postgres). It
protects nothing else. If you lose it, those secrets can't be read: for a
connector, the server logs that its settings can't be resolved and treats
it as off until someone pastes its secrets again. Back it up apart
from the database. It can hold several keys: the first encrypts, and every
one decrypts. To rotate, put a new key first, paste each secret again, then
remove the old key; nothing re-encrypts in bulk. Without any config key,
secrets can't be pasted, and each must come from its own variable
(`DIDEROT_SLACK_BOT_TOKEN` and the others under
[Connector settings](configuration.md#connector-settings)). A variable
always wins over a pasted value.

**The browser session keys** (`DIDEROT_BROWSER_SESSION_KEYS`) seal sign-in
cookies. Losing them signs everyone out, nothing more. Rotate them in three
rollouts ([auth](auth.md#built-in-sign-in-oidc)). Generate either kind with
`openssl rand -base64 32`.

**Files**, mounted read-only: the authorization policy
(`DIDEROT_AUTHORIZATION_POLICY_PATH`, needed only for Zendesk grants and
per-channel restrictions; [Authorization policy](auth.md#authorization-policy)),
and, in `static` mode, the token file
(`DIDEROT_AUTH_STATIC_TOKENS_FILE`). Changing one takes a restart.

**Connector credentials** (Slack, Notion, Linear, Zendesk, Tavily, and the
rest) are pasted on the connectors' pages, or set by their variables in the
deployment. A connector's other settings (channels, teams, intervals) live
only in the database.

## Authentication

[Authentication and authorization](auth.md) is the reference; the decisions
for a deployment are these.

- **API and MCP:** `DIDEROT_AUTH_MODE=oidc` with your IdP's issuer and the
  audience of the access tokens callers present. Diderot checks only that a token is
  genuine, and any holder of one can ask about and search Notion, Linear,
  Markdown, public Slack channels, and shared agent sessions
  ([Retrieval scopes](auth.md#retrieval-scopes)). `DIDEROT_ADMIN_GROUPS` and
  `DIDEROT_SELF_SERVICE_GROUPS` gate administration and session
  self-service, not reading. So restrict who can get a token in your IdP's
  application, and if the issuer signs in accounts outside your organization
  (Google, for one), set `DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS`. `static` mode
  (a file of token digests) also works for a small install; adding or
  removing a token takes a restart. Never use `none`: it makes every request
  an administrator, which is why it refuses to listen on anything but
  loopback.
- **Browser UI:** either the built-in OIDC sign-in
  (`DIDEROT_BROWSER_AUTH=oidc`, a web client with the redirect URI
  `DIDEROT_PUBLIC_URL/auth/callback`), or an authenticating proxy that sets
  email and group headers (`DIDEROT_TRUSTED_PROXY_CIDRS` and the header
  names). The proxy's headers count only when the connection's own peer
  address is inside those CIDRs; `X-Forwarded-For` is never read. A CIDR
  trusts every address in it, so in a cluster, where the CIDR is usually the
  pod network, a NetworkPolicy that admits only the proxy to port 9091 is
  what keeps other workloads from forging the headers. The proxy must also
  strip any copies of those headers that clients send.
- **`/mcp`** accepts the same bearer as `/v1`, on both API listeners, so
  anyone who can reach either port with a valid API token can use it as
  themselves. To put an agent gateway in front instead, set
  `DIDEROT_GATEWAY_API_KEY`. The gateway presents that key as its bearer,
  plus the caller's email in a header, which counts only on a request with
  the key and from a trusted peer ([MCP endpoint](auth.md#mcp-endpoint)). A
  gateway that allowlists tool names needs `attachment` beside `retrieve`,
  `list`, `expand`, and `ask`, and must pass `resources/read` for Ask's
  images to be readable. In `oidc` mode `/mcp` also publishes where to sign
  in, so MCP clients reaching it directly can sign people in with your IdP
  ([Signing in to `/mcp`](auth.md#signing-in-to-mcp-in-oidc-mode)).
- **The bootstrap admin.** A fresh install lets anyone who reaches `/admin`
  sign in as `admin` / `admin` until it is turned off. Turn it off before
  the UI is reachable by others: sign in through your IdP or proxy as a
  member of `DIDEROT_ADMIN_GROUPS` and use the setup checklist
  ([Setup](setup.md#the-checklist)). If every administrator
  is locked out, `diderot reset-admin`, run in a pod with the server's
  environment (`kubectl exec deploy/diderot -- diderot reset-admin`),
  turns it back on.

## Kubernetes example

A complete deployment in the namespace `diderot`, with OIDC for the API and
the browser, Postgres and Qdrant outside the namespace, and one replica. Fill
in the `<...>` values. The Secret holds plain values for illustration; load
them from your secret store instead.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: diderot
  namespace: diderot
---
# The Lease election: client-go reads, creates, and renews one Lease.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: diderot-leader-election
  namespace: diderot
rules:
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    verbs: [create]
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    resourceNames: [diderot-leader]
    verbs: [get, update]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: diderot-leader-election
  namespace: diderot
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: diderot-leader-election
subjects:
  - kind: ServiceAccount
    name: diderot
    namespace: diderot
---
apiVersion: v1
kind: Secret
metadata:
  name: diderot-env
  namespace: diderot
type: Opaque
stringData:
  DIDEROT_DB_URL: postgres://diderot:<password>@db.example.com:5432/diderot?sslmode=verify-full
  DIDEROT_QDRANT_API_KEY: <qdrant api key>
  # A model provider's key, if not pasted on its page: the provider named
  # openai on the Models page takes it.
  DIDEROT_PROVIDER_OPENAI_KEY: <openai api key>
  DIDEROT_BROWSER_OIDC_CLIENT_SECRET: <web client secret>
---
# Keep a copy of config-keys outside the cluster: without it, secrets pasted
# in the admin UI can't be read.
apiVersion: v1
kind: Secret
metadata:
  name: diderot-keys
  namespace: diderot
type: Opaque
stringData:
  config-keys: <openssl rand -base64 32>
  browser-session-keys: <openssl rand -base64 32>
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: diderot
  namespace: diderot
spec:
  # More replicas serve Ask, retrieval and MCP side by side; the leader's
  # work runs on one of them. See "Replicas and the leader".
  replicas: 1
  # Migrations run as the new pod starts, and nothing guarantees an older
  # release runs against a newer schema, so stop the old pod first.
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: diderot
  template:
    metadata:
      labels:
        app.kubernetes.io/name: diderot
    spec:
      serviceAccountName: diderot
      # Work in progress gets up to 14m30s, Ask 5s more, the listeners 10s.
      terminationGracePeriodSeconds: 900
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: diderot
          image: ghcr.io/bluesky-social/diderot:v<version>
          args: [serve]
          ports:
            - {name: public, containerPort: 9091}
            - {name: machine, containerPort: 9092}
            - {name: webhook, containerPort: 9093}
            - {name: metrics, containerPort: 9094}
          env:
            - name: POD_NAME
              valueFrom:
                fieldRef: {fieldPath: metadata.name}
            - {name: DIDEROT_PUBLIC_URL, value: https://diderot.example.com}
            - {name: DIDEROT_QDRANT_ADDR, value: qdrant.example.com:6334}
            - {name: DIDEROT_QDRANT_TLS, value: "true"}
            - {name: DIDEROT_ASK_ENABLED, value: "true"}
            - {name: DIDEROT_AUTH_MODE, value: oidc}
            - {name: DIDEROT_OIDC_ISSUER, value: https://idp.example.com/}
            - {name: DIDEROT_OIDC_AUDIENCE, value: diderot}
            - {name: DIDEROT_ADMIN_GROUPS, value: diderot-admins}
            - {name: DIDEROT_SELF_SERVICE_GROUPS, value: diderot-users}
            - {name: DIDEROT_BROWSER_AUTH, value: oidc}
            - {name: DIDEROT_BROWSER_OIDC_CLIENT_ID, value: diderot-web}
            - {name: DIDEROT_BROWSER_SESSION_KEYS_FILE, value: /etc/diderot/keys/browser-session-keys}
            - {name: DIDEROT_CONFIG_KEYS_FILE, value: /etc/diderot/keys/config-keys}
            # Let Go collect garbage before the container's memory limit.
            - {name: GOMEMLIMIT, value: 1800MiB}
          envFrom:
            - secretRef:
                name: diderot-env
          startupProbe:
            httpGet: {path: /healthz, port: machine}
            periodSeconds: 5
            failureThreshold: 60
          livenessProbe:
            httpGet: {path: /healthz, port: machine}
            periodSeconds: 10
            failureThreshold: 3
          readinessProbe:
            httpGet: {path: /readyz, port: machine}
            periodSeconds: 10
            failureThreshold: 3
          # A starting point, not a measurement; see "Sizing". Push ingest,
          # when it is on, may buffer up to 1 GiB by default.
          resources:
            requests: {cpu: 500m, memory: 512Mi}
            limits: {memory: 2Gi}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
          volumeMounts:
            - {name: keys, mountPath: /etc/diderot/keys, readOnly: true}
            # Atlas unpacks the migrations into a temporary directory.
            - {name: tmp, mountPath: /tmp}
      volumes:
        - name: keys
          secret:
            secretName: diderot-keys
        - name: tmp
          emptyDir: {}
---
# For your ingress or authenticating proxy.
apiVersion: v1
kind: Service
metadata:
  name: diderot-public
  namespace: diderot
spec:
  selector:
    app.kubernetes.io/name: diderot
  ports:
    - {name: public, port: 80, targetPort: public}
---
# For machine clients inside the cluster.
apiVersion: v1
kind: Service
metadata:
  name: diderot-machine
  namespace: diderot
spec:
  selector:
    app.kubernetes.io/name: diderot
  ports:
    - {name: machine, port: 80, targetPort: machine}
---
# Who may connect to the pod. The labels are examples: use your ingress
# controller's and scraper's. Add an egress policy as well if your cluster
# restricts egress (Postgres, Qdrant, the model provider, the Kubernetes API
# for the Lease, DNS, each connector's API, and, unless published model facts
# are turned off on the Models page, models.dev and raw.githubusercontent.com,
# which the leader reads once a day).
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: diderot
  namespace: diderot
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: diderot
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels: {kubernetes.io/metadata.name: ingress}
          podSelector:
            matchLabels: {app.kubernetes.io/name: ingress-nginx}
      ports:
        - {port: 9091}
    - from:
        - namespaceSelector:
            matchLabels: {diderot.example.com/machine-client: "true"}
      ports:
        - {port: 9092}
    - from:
        - namespaceSelector:
            matchLabels: {kubernetes.io/metadata.name: monitoring}
      ports:
        - {port: 9094}
```

Then route `https://diderot.example.com` to `diderot-public` through your
ingress with TLS. The PagerDuty webhook, if you use it, needs a route from
the internet to port 9093's `/webhooks/pagerduty` and a matching
NetworkPolicy rule.

## Operating it

**Upgrades.** Read the release notes and [Upgrading](upgrading.md) first.
The server applies pending migrations as it starts, and a migration is never
undone, so back up the database before a release that has one. With one
replica and `Recreate`, the old pod drains, then the new one migrates and
starts; the API is down in between. With `RollingUpdate`, the old pods keep
running against the migrated schema until they are replaced, and the
project does not guarantee that this works.

**Rolling back.** Deploying an older image does not undo the newer
release's migrations, and the older release starts against the newer schema
without complaint: its migrations are all applied, and it does not look at
the ones it doesn't know. Whether it then works is not guaranteed. To go
back across a release with a migration, restore the backup taken before the
upgrade the way [Backup and restore](#backup-and-restore) says, which
includes rebuilding Qdrant, and run the older image on it. Whatever was
written after the backup is lost.

### Backup and restore

- **Back up Postgres** with your provider's snapshots and point-in-time
  recovery, or with `pg_dump --format=custom --file=diderot.dump diderot`.
  Every event, setting, sealed secret, and conversation is there, and so is
  the migration history.
- **Back up the config key** (and the browser session keys, the static token
  file, and any other secret you generated), apart from the database.
- **Back up the attachments bucket**, if you set one. Postgres keeps only
  each file's key; the file proxy reads the bytes from the bucket.
- **Qdrant needs no backup: it is rebuilt from Postgres.** Start the server
  on an empty Qdrant (it creates the collection), then run
  `diderot qdrant-audit` where the server runs (`kubectl exec deploy/diderot
  -- diderot qdrant-audit`). It queues every chunk the collection lacks, and
  the leader's embedder embeds each one again through your embedding
  provider. That costs a full re-embedding of the corpus, and takes about
  `diderot_corpus_chunks` ÷ `DIDEROT_EMBED_BATCH_SIZE` (32) × the time of one
  embedding request (`diderot_embedder_batch_duration_seconds`) ÷
  `DIDEROT_EMBED_WORKERS` (1). Until it finishes, searches quietly find
  less.
- **To restore**, stop the server, load the backup into an empty database
  (`pg_restore --no-owner --dbname=diderot diderot.dump`, as the server's
  role), point the server at an empty Qdrant collection (delete the old one),
  start it with the same config key, and rebuild Qdrant as above. Don't put
  back a Qdrant snapshot from another moment instead. The restored database
  hands out chunk IDs again that a later snapshot holds for other text, and
  `qdrant-audit` compares only point IDs and permission scopes, never the
  vectors, so it can't find them.

### Watching it

Metrics are on port 9094 at `/metrics`. The ones that say whether Diderot is
working:

| Metric | Watch for |
|---|---|
| `diderot_embedder_queue_pending` | Growing without falling: the embedder is behind or stuck, and new content is not searchable. |
| `diderot_embedder_queue_stranded` | Above zero: chunks the embedding model rejected five times. |
| `diderot_model_attention{job}` | 1: the model that job uses is deprecated, retires within 90 days, or is gone from its provider's list. The Models page says which and when; alert on it so a retirement is not found by the job failing. |
| `diderot_worker_last_success_timestamp_seconds{worker}` | Stale: a background worker has stopped succeeding. |
| `diderot_connector_last_success_timestamp_seconds{connector}`, `diderot_connector_errors_total` | Stale or rising: a connector is failing. |
| `diderot_ask_gateway_last_success_timestamp_seconds` | Stale: the readiness check can't reach Ask's model. |
| `diderot_ask_turns_total`, `diderot_ask_turn_duration_seconds` | Ask's volume, outcomes, and latency. Alert on the `error` outcome only: `stopped` (the asker pressed stop) and `cancelled` (the API or MCP caller went away) are not failures, and the model requests and tool calls they cut short are counted under those outcomes too ([how a turn ends](design.md#how-a-turn-ends)). |
| `diderot_handler_panics_total{site}` | Above zero: a bug panicked on a Slack/Discord event handler or Ask turn, or an MCP ask task. The panic is recovered and logged with its stack, so the one event, turn, or task fails instead of crashing the replica. No restart or CrashLoopBackOff alert sees it, so this metric is the only signal. It is registered on first use, so a rate-based alert can miss a series' first occurrence per process. |
| `diderot_qdrant_delete_queue_depth` | Growing: deletions are not reaching Qdrant. |
| `diderot_corpus_events`, `diderot_corpus_chunks` | The corpus size by source. |

The embedder, worker, and connector metrics are updated only by the
leader's work, but a replica that loses leadership and keeps running goes on
exporting the values it last had, and no metric names the leader. With more
than one replica, alert on the newest timestamp across replicas
(`max by (worker)`, `max by (connector)`), and read the queue gauges as
one replica's snapshot. Ask's metrics (`diderot_ask_*`) count each
replica's own turns: sum them across replicas. **Logs** are JSON, one object per line on stdout, at
info level; there is no setting for the format or level.

**Removing a source's data.** There is no supported way to delete
everything a source ingested. Turning a connector off stops its syncs and
keeps what it stored. Some sources remove items on their own: a file removed
from the Markdown folder, a Linear issue moved to a private team, a Zendesk
ticket moved out of the indexed groups.

**Sizing.** What grows with the corpus:

- Postgres: one row per item (a message, page, issue, ticket, file, or
  session summary) with its source payload, one chunk row for about every
  1,000 characters of text, Ask conversations, and pushed sessions' stored
  documents and records.
- Qdrant: one point per chunk. Its RAM holds the int8-quantized vectors and
  the HNSW graph, so it grows with the chunk count and the embedding
  model's dimension; the full vectors and payloads are on disk.
- Embedding cost: every new or changed chunk, and every question, is one
  embedding input.
- Server memory depends on load more than on the corpus: concurrent Ask
  turns, PDF extraction (`DIDEROT_DOC_EXTRACT_WORKERS`), image captions, and
  pushed sessions, which may buffer up to
  `DIDEROT_SESSION_PUSH_MAX_INFLIGHT_BYTES` (1 GiB by default) at once. Set
  the container's memory limit with that in mind, and `GOMEMLIMIT` a little
  below it so Go collects garbage before the limit is reached.

## Not covered and known limits

- There is no Helm chart; adapt the example above.
- No TLS on any listener; terminate it in front.
- A replica that dies mid-turn holds that session for up to 30 seconds,
  until its lease expires, and an MCP `ask` task it was running reports
  `working` until the task's one-hour expiry.
- The PagerDuty webhook listener is always open (on 9093 unless
  `DIDEROT_PAGERDUTY_LISTEN_ADDR` moves it); it answers 404 while the
  PagerDuty connector is off.
- One Slack workspace per deployment, and one deployment per Slack app.
- Migrations can't be rolled back, and a rolling update runs the old
  release against the new schema.
- No supported way to delete a source's data.
- Logs are JSON at info level only.
