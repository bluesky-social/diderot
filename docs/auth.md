# Authentication and authorization

Diderot has three authenticated surfaces. The API and MCP share one bearer
credential; a browser credential is never accepted on either, and an API or
MCP credential is never a browser session.

| Surface | Who calls it | How the caller is identified |
|---|---|---|
| `/v1` API | Scripts and other programs | A bearer token, checked by the mode in `DIDEROT_AUTH_MODE`: `oidc`, `static`, or `none` |
| Browser UI (`/admin`, `/sessions`) | People in a browser | Identity headers from an authenticating proxy, the built-in OIDC sign-in, or local sign-in (the bootstrap admin, or a pasted static token) |
| `/mcp` | AI agents and MCP clients | The same bearer as `/v1`; or, through an agent gateway, the gateway's key plus an identity header from a trusted peer |

Every variable named here is listed in the
[configuration reference](configuration.md#authentication).

## Identity

A principal is a subject, an email, a display name, and a list of groups.
**The lowercased email is the identity key.** It must be the same address
that a person has in Slack, because Diderot uses it to:

- resolve Slack channel memberships, which decide which private channels
  that person's searches can read;
- own agent sessions: only the owner can change a session's visibility, or
  read it while it is owner-only;
- decide self-service access to the `/sessions` page.

Because the email grants access, only a **verified** email counts. In `oidc`
mode, a token without `email_verified: true` yields a principal with no email
(unless `DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED=false`). That caller gets no
Slack scopes, owns no sessions, and cannot use self-service. Diderot never
falls back to an unverified address. Set the variable to `false` only when
your IdP assigns every address itself and does not emit the claim. Emails in
`static` and `none` mode come from operator configuration and count as
verified.

## Authorization

- **Use routes** (`POST /v1/ask`, `/v1/retrieve`, `/v1/list`, and
  `/v1/expand-chunk`, and the `/mcp` tools) need only an authenticated
  caller. What they return is filtered by the caller's scopes.
- **Administration** is the `/admin` UI alone. It needs a browser sign-in and membership in a group listed in
  `DIDEROT_ADMIN_GROUPS`, which has no default. A value with no group names
  in it (`,`) denies everyone. The [bootstrap admin](#the-bootstrap-admin)
  is also an administrator while it is enabled. An administrator can read
  every Ask conversation there: its whole transcript and the images attached
  to it, whatever Slack channels the administrator is in.
- **Self-service routes** (`/v1/me/...` and the `/sessions` page) need a
  verified email and membership in `DIDEROT_SELF_SERVICE_GROUPS`, which has
  no default. Request input can never select another owner.

**During setup** a fresh install admits only administrators to Ask
(`/v1/ask` and the `/mcp` `ask` tool), and Slack Ask answers everyone with a
setup notice, until an administrator goes live. See [Setup](setup.md).

### Retrieval scopes

Every stored event carries one scope, and every search filters on the
caller's allowed scopes, both in the vector query and again when the text is
read from Postgres:

| Scope | Content | Who can read it |
|---|---|---|
| `public` | Anonymously readable content | Everyone |
| `internal` | Notion pages, Linear issues, Markdown folder documents, and agent sessions that are not owner-only | Every authenticated caller |
| `slack-channel:<id>` | Messages in one Slack channel | Anyone, for a public channel; members, for a private one |
| `owner:<email>` | An owner-only agent session, or a one-to-one DM with the bot | Its owner |
| `corpus:zendesk` | Support tickets and Help Center articles | Holders of a grant (below) |
| `discord-channel:<id>` | Messages of one Discord channel | Granted by the channel's visibility: to every authenticated caller, or to holders of `corpus:<name>` ([Discord](discord.md#who-can-read-it)); in Discord, to an answer's audience ([Ask in Discord](discord.md#ask-in-discord)) |

The allowed set depends on where the question is asked:

- **API and MCP:** the caller's own set. Private channels come from the
  caller's Slack memberships that the bot can also see, looked up by email
  and cached for five minutes, so removing someone from a channel takes
  effect within that window. A failed lookup falls back to `public` and
  `internal`.
- **A Slack channel:** everyone in the channel reads the answer, so the set
  depends on the channel, never on who asked: `public`, `internal`, public
  channels, and that channel's own history. A policy restriction can narrow
  it further.
- **A one-to-one DM:** the asker's set, including their own sessions and
  their DM history with the bot.
- **Discord:** what the answer's audience can read in Discord, from the
  server's roles and channel permissions read at each turn: `public` and
  Discord channel scopes, plus `internal` in a server an administrator
  marked trusted (its members are the organisation's own people), never a
  policy scope ([Ask in Discord](discord.md#who-an-answer-may-read),
  [trusted servers](discord.md#trusted-servers)).

Wherever the set comes from, the sources then add the scopes they grant to
it: a Discord channel's scope to the readers its visibility names.

### Authorization policy

`DIDEROT_AUTHORIZATION_POLICY_PATH` names an optional YAML policy. It grants
permissions to principals and restricts what answers in a given channel may
draw on. Principals are spelled `group:<group>` for IdP groups and
`slack-channel:<id>` for channels. The only
permission is `retrieve` on a corpus scope: `corpus:zendesk`, or one you name for Discord channels.

```yaml
authorization:
  grants:
    - principals: [group:support]
      permissions:
        - {action: retrieve, resource: corpus:zendesk}
  channel_profiles:
    support:
      # Answers in these channels may use Zendesk, public content, and the
      # channel's own history, and nothing else.
      channels: [C0EXAMPLE1, C0EXAMPLE2]
      allow_public: true
      include_own_channel: true
      allowed_scopes: [corpus:zendesk]
      permissions:
        - {action: retrieve, resource: corpus:zendesk}
```

`channel_restrictions` sets `allowed_scopes` and `allow_public` for a single
channel. A channel may appear in only one restriction or profile. The file
must be a single YAML document, and unknown keys fail startup.

## `/v1` bearer modes

### `oidc` (default)

The server verifies a JWT access token from your IdP: signature, issuer
(`DIDEROT_OIDC_ISSUER`), audience (`DIDEROT_OIDC_AUDIENCE`), and expiry.
Diderot checks only that the token is genuine. Who can get a token is
controlled by your IdP application's access policy. Without an issuer and an
audience, `serve` refuses to start and names both; for tokens you hand out
yourself instead, use [`static`](#static).

- **Keys** come from OIDC discovery, unless `DIDEROT_OIDC_JWKS_URL` names
  another location (an in-cluster IdP service, for example).
- **Claims.** `DIDEROT_OIDC_GROUPS_CLAIM` (default `groups`) and
  `DIDEROT_OIDC_EMAIL_CLAIM` (default `email`) are claim paths. A literal
  claim name is tried first, so a namespaced claim such as
  `https://example.com/groups` works as is. Otherwise the path is split on
  dots: Keycloak realm roles are `realm_access.roles`.
- **What the token must carry.** The server reads identity from the token
  alone and never calls the userinfo endpoint, so the token must be a JWT
  access token whose `aud` is `DIDEROT_OIDC_AUDIENCE`, and must hold the
  email and groups claims, and `email_verified` unless you turned that rule
  off ([Identity](#identity)). An ID token is not accepted. With an IdP
  whose access tokens are opaque (Google), use [`static`](#static) for the
  API and MCP; the [built-in browser sign-in](#built-in-sign-in-oidc) works with
  it either way.

A minimal server configuration, with a web client for the browser registered
at the IdP:

```sh
DIDEROT_AUTH_MODE=oidc
DIDEROT_OIDC_ISSUER=https://idp.example.com/
DIDEROT_OIDC_AUDIENCE=diderot            # the aud of the access tokens callers present
DIDEROT_ADMIN_GROUPS=diderot-admins
DIDEROT_SELF_SERVICE_GROUPS=diderot-users
DIDEROT_BROWSER_AUTH=oidc                # see Built-in sign-in below
DIDEROT_BROWSER_OIDC_CLIENT_ID=diderot-web
DIDEROT_BROWSER_OIDC_CLIENT_SECRET=<the web client's secret>
DIDEROT_BROWSER_SESSION_KEYS=<openssl rand -base64 32>
DIDEROT_PUBLIC_URL=https://diderot.example.com
```

A caller presents its access token as `Authorization: Bearer <token>` on
`/v1` and `/mcp`. Diderot issues no tokens and has no sign-in of its own for
programs; on `/mcp` it tells MCP clients where to get one
([Signing in to `/mcp` in `oidc` mode](#signing-in-to-mcp-in-oidc-mode)).

### `static`

`DIDEROT_AUTH_MODE=static` looks bearer tokens up by their SHA-256 digest in
`DIDEROT_AUTH_STATIC_TOKENS_FILE`:

```yaml
tokens:
  - sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
    email: alice@example.com
    groups: [diderot-admins]
```

Generate a token and its digest, put the digest in the file, and give the
token to its user:

```sh
token=$(openssl rand -hex 32)
printf '%s' "$token" | shasum -a 256
```

The user presents it as `Authorization: Bearer <token>` on `/v1` and `/mcp`,
and can paste it on the browser's sign-in page.

### `none`

`DIDEROT_AUTH_MODE=none` treats every request, browser included, as
`DIDEROT_AUTH_NONE_EMAIL` with every administrator and self-service group.
It is for a single person on one machine. `serve` refuses to start unless
every listener binds a literal loopback IP (`DIDEROT_LISTEN_ADDR=127.0.0.1:9091`,
`DIDEROT_INTERNAL_LISTEN_ADDR=127.0.0.1:9092`; the webhook and metrics
listeners bind loopback on their own in this mode). `DIDEROT_BROWSER_AUTH`
must be unset.

## Browser UI

`DIDEROT_BROWSER_AUTH` picks the browser's external identity source: an
authenticating proxy or the built-in OIDC sign-in, never both. With
`DIDEROT_BROWSER_SESSION_KEYS` set, the browser also has
[local sign-in](#local-sign-in) beside either source.

### Behind an authenticating proxy (`trusted-proxy`, the default)

Diderot reads the caller's email and groups from request headers set by a
proxy such as oauth2-proxy or an identity-aware gateway. The headers are
trusted only when the connection comes from a peer inside
`DIDEROT_TRUSTED_PROXY_CIDRS`. Diderot checks the connection's own address
and never `X-Forwarded-For`. With no CIDRs configured, identity headers are
ignored and the UI answers 401.

```sh
DIDEROT_TRUSTED_PROXY_CIDRS=10.0.0.0/16
DIDEROT_TRUSTED_PROXY_EMAIL_HEADER=X-Auth-Request-Email
DIDEROT_TRUSTED_PROXY_GROUPS_HEADER=X-Auth-Request-Groups
DIDEROT_TRUSTED_PROXY_GROUPS_DELIMITER=,
```

The header names have no defaults. Your proxy must strip any copies of these
headers that a client sends. A CIDR trusts every host inside it. If other
workloads share that range, restrict the network so that only the proxy can
reach `DIDEROT_LISTEN_ADDR`, and send machine traffic to
`DIDEROT_INTERNAL_LISTEN_ADDR`. That listener answers 404 for the browser
routes before it reads any identity header.

### Built-in sign-in (`oidc`)

For installs without a proxy, Diderot signs people in itself, using
authorization code + PKCE (S256), state, and nonce against
`DIDEROT_OIDC_ISSUER`.

1. Register a **web** client at your IdP with redirect URI
   `DIDEROT_PUBLIC_URL` + `/auth/callback`. `DIDEROT_PUBLIC_URL` must be
   https, except on a loopback host.
2. Set `DIDEROT_BROWSER_AUTH=oidc`, `DIDEROT_BROWSER_OIDC_CLIENT_ID`, and
   `DIDEROT_BROWSER_OIDC_CLIENT_SECRET` (empty for a public client).
3. Generate a session key with `openssl rand -base64 32` and set it in
   `DIDEROT_BROWSER_SESSION_KEYS`. The same keys seal
   [local sign-in](#local-sign-in) sessions.

The ID token goes through the same claim settings as `/v1` bearers, so the
IdP must put groups in the **ID token**. An identity without an accepted
email gets no session at all (403), because every browser action is
attributed to an email. `DIDEROT_OIDC_SCOPES` defaults to
`openid email profile`. Add `groups` if your IdP releases groups only for
that scope. The same scopes are what `/mcp` advertises to MCP clients in
`oidc` mode.

The session is a stateless, encrypted cookie (`__Host-diderot_session`:
HttpOnly, Secure, SameSite=Lax). It keeps only the groups that the
administrator and self-service allowlists name, so changing an allowlist takes
effect at the next sign-in. A session ends after
`DIDEROT_BROWSER_SESSION_IDLE_TIMEOUT` (1h) unused, or
`DIDEROT_BROWSER_SESSION_TTL` (12h) after sign-in. Signing out deletes the
cookie in that browser but cannot revoke a copy, and does not end the IdP
session.

**Rotating the session key.** The first key in
`DIDEROT_BROWSER_SESSION_KEYS` seals cookies, and every listed key opens
them. With more than one replica, rotate in three rollouts: append the new
key, then move it to the front, then remove the old key once a TTL has
passed. A replica that cannot open a cookie sends the browser to sign in
again.

For development on plain HTTP, use a literal loopback IP as the public URL
(`http://127.0.0.1:9091`) and set
`DIDEROT_BROWSER_SESSION_INSECURE_COOKIE=true`.

### Local sign-in

`DIDEROT_BROWSER_SESSION_KEYS` turns on the browser's own sign-in in every
auth mode except `none`, where it must be unset. It needs no identity
provider. The sign-in page (`/auth/login`, where an unauthenticated page load
is sent) offers each method that is available now; with the OIDC sign-in
and no local method available, it goes straight to the identity provider.
Every method sets the same session cookie, with the same lifetimes and key
rotation, as the OIDC sign-in. The sign-in forms check the request's origin
and a double-submit token, so another site cannot sign a browser in.

Without session keys there is no local sign-in, and a 401 from the browser
UI says to set them.

#### The bootstrap admin

A fresh install's sign-in page offers `admin` / `admin`, so the first
operator can reach `/admin` before any identity provider or token is set up
([Setup](setup.md)). The account:

- has no real identity: its subject is `bootstrap:admin` and its email
  `admin@bootstrap.invalid`, which matches no Slack profile and owns no
  sessions;
- belongs to no group, but the `/admin` UI admits it as an administrator
  while it is enabled. It is never an API, MCP, or self-service identity;
- is checked against the database on every request its session makes, so
  turning it off ends its open sessions at once on every replica.

Turning it off is a setup step, from a session that is not the bootstrap
admin, and permanent from the UI. `diderot reset-admin`, run on the
server with its database access, turns it back on.

#### Static tokens

In `static` mode the sign-in page also takes an API token from
`DIDEROT_AUTH_STATIC_TOKENS_FILE`, pasted into a form. The session records
the token's SHA-256, the value the token file holds, never the token. Every
request re-resolves it against the tokens the server loaded, so a token
removed from the file stops working at the next restart, and its email and
groups always come from the file.

#### Behind a proxy

With a trusted proxy, a valid local session wins over the proxy's headers,
and a browser without one, or whose session no longer verifies, is
identified by the headers. To act as yourself again after signing in as the
bootstrap admin, sign out.

## MCP endpoint

`/mcp` is a stateless, streamable-HTTP MCP server exposing `retrieve`,
`list`, `expand`, `attachment`, and `ask`. It is always mounted, on both API
listeners; until an embedding model is chosen, its search tools answer that
search is not set up. A request's bearer alone decides which
of two ways it authenticates:

- **Directly, as on `/v1`.** The bearer is whatever `DIDEROT_AUTH_MODE`
  accepts on `/v1`, verified by the same code: a static token names its
  mapped email and groups, an OIDC access token its verified principal, and
  `none` mode needs no bearer and answers as its one administrator. Identity
  headers are never read on this path, so a header beside a valid token
  changes nothing, whatever address sent it.

  ```sh
  claude mcp add --transport http diderot https://diderot.example.com/mcp \
    --header "Authorization: Bearer $DIDEROT_TOKEN"
  ```

  [`diderot setup`](cli.md) does this for each harness a person names. In
  `oidc` mode it and other MCP clients can also sign people in directly
  ([below](#signing-in-to-mcp-in-oidc-mode)).

- **Through an agent gateway**, when `DIDEROT_GATEWAY_API_KEY` is set. A
  gateway that authenticates people itself presents that key as its bearer
  and forwards the person's email in
  `DIDEROT_TRUSTED_PROXY_MCP_EMAIL_HEADER` (defaulting to the browser email
  header) and their groups in the groups header. Those headers are honored
  only on a request that carries the gateway key, and only from a peer in
  `DIDEROT_TRUSTED_PROXY_CIDRS`. The key alone identifies nobody: such a
  request can discover the tools, and every tool call fails.

  For `diderot setup` and MCP clients to sign people in through the gateway
  on their own, the gateway answers an unauthenticated request with `401`
  and MCP authorization metadata (RFC 9728 protected resource metadata
  naming the authorization server). Clients register by dynamic client
  registration at that server; if it has none, the gateway can serve its own
  authorization server metadata at the MCP URL with a registration endpoint
  that answers with a public client registered at the identity provider, or
  people pass that client's ID to `diderot setup --client-id`. A public
  client shared this way must accept the loopback redirects
  `http://127.0.0.1:<any port>/callback` and `http://localhost:<any port>/callback`
  ([The `diderot` command](cli.md#what-setup-cannot-do)).

A request with no bearer, or one neither path accepts, is answered `401`
with a `WWW-Authenticate: Bearer` challenge (in `oidc` mode it also names
the resource metadata and the scopes, below); a token whose identity the
deployment does not admit (outside `DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS`) is
`403`, as on `/v1`. A bearer token without an email authenticates but
identifies nobody, so its tool calls fail too.

Each call is filtered by the same scopes as the `/v1` API. `list` reads a
Slack channel only when the caller's scopes include that channel, and
answers for a channel they cannot read exactly as for one that does not
exist. `attachment` returns a file only when the caller can read the message
or page it is attached to. `ask` continues a conversation (`session_id`)
only for the caller who started it, and its images are MCP resources
(`diderot://ask/{session_id}/artifacts/{artifact_id}`) that only that
caller can read.

### Signing in to `/mcp` in `oidc` mode

In `oidc` mode `/mcp` follows the MCP authorization specification as an
OAuth resource server: it publishes OAuth 2.0 protected resource metadata
(RFC 9728) at `DIDEROT_PUBLIC_URL/.well-known/oauth-protected-resource/mcp`,
and its `401` challenge points there:

```http
WWW-Authenticate: Bearer realm="diderot", resource_metadata="https://diderot.example.com/.well-known/oauth-protected-resource/mcp", scope="openid email profile"
```

```json
{
  "resource": "https://diderot.example.com/mcp",
  "authorization_servers": ["<DIDEROT_OIDC_ISSUER>"],
  "scopes_supported": ["openid", "email", "profile"],
  "bearer_methods_supported": ["header"],
  "resource_name": "Diderot"
}
```

An MCP client, `diderot setup` among them, reads it, finds the issuer's
metadata, signs the person in in the browser, and presents the access token
it gets. The scopes are `DIDEROT_OIDC_SCOPES`: the ones that make your IdP
put the email and groups claims in its tokens. Nothing about who is accepted
changes: the token is checked exactly as above, so it must be a JWT access
token with `aud` equal to `DIDEROT_OIDC_AUDIENCE` and the identity claims.
Static and `none` modes publish no metadata.

What the IdP needs:

- **A client people's programs can use.** MCP clients first try dynamic
  client registration and client ID metadata documents at the issuer. Most
  IdPs used inside a company offer neither, so register one **public**
  client (no secret) with loopback redirect URIs on any port,
  `http://127.0.0.1:<port>/callback` and `http://localhost:<port>/callback`,
  and give people its client ID: `diderot setup <url> --client-id <id>`,
  and for a harness the same ID (`claude mcp add ... --client-id <id>`).
  The specification has no way for the server to publish a client ID, so
  Diderot does not.
- **Access tokens for Diderot.** The client's tokens must carry
  `aud: DIDEROT_OIDC_AUDIENCE`. With an IdP whose access-token audience is
  the client ID (Authentik is one), that means the public client above is
  the audience: set `DIDEROT_OIDC_AUDIENCE` to its client ID, or have the
  IdP add Diderot's audience to its tokens. Clients also send the RFC 8707
  `resource` parameter (`DIDEROT_PUBLIC_URL/mcp`); an IdP that honors it
  must still issue the configured audience.
- **The identity claims in the access token**, as for any `/v1` caller.

The metadata names `DIDEROT_PUBLIC_URL/mcp` as the resource, so people
connect to that URL. A deployment whose `/mcp` people reach through an
agent gateway publishes this too, but the gateway's own metadata is what
their clients see.
