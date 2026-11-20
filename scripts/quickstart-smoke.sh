#!/usr/bin/env bash
# End-to-end smoke test of the Docker Compose quickstart. Builds and starts
# compose.quickstart.yaml with internal/e2e/quickstart/compose.smoke.yaml, which
# swaps the model provider and embedder for a deterministic stub, with no token
# or keys given, as a fresh .env has. It reads the API token the init container
# generated, checks that a second init run keeps the token and keys, then runs
# TestQuickstartSmoke (sign in to the browser admin with the token and open the
# setup checklist; ingest a fixture, wait for embedding, retrieve, ask; list
# the fixture as an MCP client) and removes the stack and its volumes.
#
# Needs Docker Engine 25+ with Compose v2.24.4+ (for `!override`) and Go. No
# secrets, no model provider.
#
#   scripts/quickstart-smoke.sh            # or: just quickstart-smoke
#   KEEP_STACK=1 scripts/quickstart-smoke.sh   # leave the stack up afterwards
#
# The stack runs as its own Compose project (QUICKSTART_SMOKE_PROJECT, default
# diderot-quickstart-smoke) with ephemeral loopback ports, so it does not
# collide with a running quickstart or the development databases.
set -euo pipefail

cd "$(dirname "$0")/.."

project="${QUICKSTART_SMOKE_PROJECT:-diderot-quickstart-smoke}"
# An empty env file keeps a developer's .env (real keys, COMPOSE_FILE) out of
# the smoke; the override pins everything else the stack reads.
empty_env="$(mktemp)"
compose=(docker compose --project-name "$project" --env-file "$empty_env"
    -f compose.quickstart.yaml -f internal/e2e/quickstart/compose.smoke.yaml)

status=1
cleanup() {
    if [ "$status" -ne 0 ]; then
        echo "--- quickstart smoke failed; service status and logs follow" >&2
        "${compose[@]}" ps -a >&2 || true
        "${compose[@]}" logs --no-color --tail 200 >&2 || true
    fi
    if [ "${KEEP_STACK:-}" = 1 ]; then
        echo "--- leaving the stack up: ${compose[*]} down -v" >&2
    else
        "${compose[@]}" down -v --remove-orphans >&2 || true
    fi
    rm -f "$empty_env"
}
trap cleanup EXIT

"${compose[@]}" up --detach --build --wait --wait-timeout 600

# The documented way to read the generated token. A second run of the init
# script must keep the token and both keys.
token="$("${compose[@]}" run --rm -T init cat /keys/api-token)"
generated() {
    "${compose[@]}" run --rm -T init sh -c 'cat /keys/api-token /keys/browser-session-keys /keys/config-keys | sha256sum'
}
before="$(generated)"
"${compose[@]}" run --rm -T init >/dev/null
if [ "$(generated)" != "$before" ]; then
    echo "a second init run replaced the generated token or keys" >&2
    exit 1
fi

# `compose port` prints host:port for a published container port.
published_port() {
    "${compose[@]}" port "$1" "$2" | sed 's/.*://'
}
api_port="$(published_port diderot 9091)"
db_port="$(published_port postgres 5432)"

DIDEROT_QUICKSTART_SMOKE_URL="http://127.0.0.1:${api_port}" \
    DIDEROT_QUICKSTART_SMOKE_TOKEN="$token" \
    DIDEROT_QUICKSTART_SMOKE_DB_URL="postgres://diderot:diderot@127.0.0.1:${db_port}/diderot?sslmode=disable" \
    go test -count=1 -v -run '^TestQuickstartSmoke$' ./internal/e2e/quickstart/
status=0
