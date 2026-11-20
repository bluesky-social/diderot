# Tool connections

Ask can call the tools of MCP servers you run, which is how a deployment
adds integrations (observability, ticketing, internal APIs) without changing
code. Each server is a **tool connection**, added and managed in the admin
UI under **Connectors**, **Tool connections**. Ask needs to be on
(`DIDEROT_ASK_ENABLED=true`); nothing else is set in the deployment.

This page covers Ask as an MCP *client*. Diderot's own MCP endpoint, `/mcp`,
is described in [Authentication](auth.md#mcp-endpoint).

## Adding a connection

1. On the Connectors page, choose **Add a connection**, pick its kind
   (Grafana, Sentry, or MCP server), and name it. The name, lowercase
   letters, digits or `_`, appears in logs and Ask's progress lines and
   can't be changed.
2. On its page, enter the server's address (for Sentry, the organization
   and project) and its credential.
3. **Test** connects and lists every tool the server offers, each marked
   with whether Ask may call it (see [read-only tools](#read-only-tools)).
   Tick the tools Ask may call. For Grafana, choose the datasources to pin.
4. **Save and turn on** saves the settings, tests them again, and turns the
   connection on when the test passes.

Every replica builds Ask again within seconds of a saved change: a new
question uses the new tools, and a question under way finishes with what it
started with. No restart is needed. Every change is recorded in the audit
log (Operations, Audit log).

## Settings

- **Server address.** It must be https. Plain http is allowed only to a
  `*.svc.cluster.local` Service, or to a loopback address with
  `DIDEROT_DEPLOYMENT=standalone`. Redirects are refused, so a credential
  never follows one.
- **Credential.** Sent as `Authorization: <scheme> <credential>`, the scheme
  being Bearer unless **More settings** says otherwise. It is stored
  encrypted with `DIDEROT_CONFIG_KEYS` and never shown again. To keep it in
  your secret store instead, set the variable the page names,
  `DIDEROT_CONNECTION_<NAME>_TOKEN` (the connection's name in capitals),
  from that store; while it is set, it is the credential, and the page says
  so. A change to the variable takes effect when the server restarts.
- **Who uses its tools.** Ask, which calls them while it researches a
  question; the observability investigator, `investigate_observability`, a
  bounded child agent Ask hands a question about live systems to, which
  researches in its own context and returns one report; or both, over one
  connection. Callers whose access is limited to the corpus never see them.
  The investigator exists only while a connection for it is on. Grafana and
  Sentry connections serve the investigator only.
- **Tools Ask may call.** An allowlist: tools the server offers that are not
  ticked are never exposed, and a ticked tool the server stops offering
  keeps the whole connection out until it is fixed. A tool name may not
  repeat across connections or shadow one of Ask's own tools.
- **More settings.** The authorization scheme; an identity header, which
  sends Diderot's service identity, `svc:diderot`, for gateways that require
  an authenticated actor (it may not name a transport header such as
  `Authorization`, `Content-Type`, `Host` or `Mcp-*`); the request timeout
  (2 minutes unless set); and allowing tools that are not marked read-only.

## Read-only tools

Ask calls a connection's tools with its credential on behalf of anyone who
asks, so a ticked tool attaches only when one of these holds:

- it is in the kind's reviewed read-only set;
- the server annotates it `readOnlyHint: true` without a `destructiveHint`
  that is true;
- the connection allows tools that are not marked read-only, which logs a
  warning for each such tool when Ask is built.

## Kinds

A kind selects code reviewed against one upstream.

- **MCP server** (generic) has no reviewed set and adds no prompts.
  Admission relies on the tools' annotations or the opt-in.
- **Grafana** expects Grafana and Tempo tools under `grafana_` and `tempo_`
  prefixes, the way an MCP gateway that federates both servers exposes
  them. It carries a reviewed read-only set
  (`internal/ask/observability_mcp.go`), checks that the alert-rules tool
  (`grafana_alerting_rules_read`, so mcp-grafana 2.0 or later) exposes only
  read operations, stores `grafana_get_panel_image` renders as Ask
  artifacts (which needs `DIDEROT_IMAGES_BUCKET`), and gives the
  investigator its Grafana prompts. **Datasources** pins the Grafana
  datasource that each kind of query uses: Prometheus, Loki and Pyroscope
  (Tempo tools take none). When the server offers Grafana's datasource list
  as a read-only tool, the test lists the datasources to choose from;
  otherwise enter each UID. Every tool of a pinned kind queries that
  datasource: the investigator is not offered the datasource argument at
  all, Diderot sets it on every call, and
  [`{{GRAFANA_DATASOURCES}}`](prompts.md#variables) names the pins to it.
- **Sentry** is Sentry's hosted MCP server. Enter the organization and
  project; Diderot connects to
  `https://mcp.sentry.dev/mcp/<organization>/<project>?skills=inspect`
  (another server address can be set under **More settings**), which pins
  the credential to one organization, one project, and the read-only skill.
  It has no reviewed set, and an annotation counts only with an explicit
  `destructiveHint: false`. The investigator gets its Sentry prompts.

## When a connection fails

- A test that fails says which check failed and what to fix. A save whose
  test fails keeps the settings and leaves the connection as it was.
- Each replica connects on its own when it builds Ask. A connection that
  can't connect, or whose tools fail admission, is logged and left out of
  every agent it serves; the rest of Ask stays up, and the replica tries
  again five minutes later.
- The leader tests every connection that is on every 10 minutes. One whose
  latest check, or last saved test, failed shows **needs attention** on the
  Connectors page, on its page, and on the Overview.
- [PagerDuty investigations](configuration.md#pagerduty-investigations)
  need a Grafana connection that is on.

## Deployment history

Diderot has no deployment-history client of its own. A service that serves
a change timeline over MCP is an ordinary connection. Letting both Ask and
the investigator use it lets Ask answer change-history questions itself and
the investigator correlate changes with telemetry. The example prompts'
change-history guidance applies whenever Ask or the investigator has such a
tool.
