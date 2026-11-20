#!/usr/bin/env bash
# Start a throwaway local Diderot from this checkout: its own Postgres,
# Qdrant and S3-compatible file store (compose.local-stack.yaml, under its own
# Compose project and ports), and `diderot serve` built from the working tree,
# with the admin UI on localhost. scripts/local-stack-down.sh removes all of
# it. Nothing is shared with the development databases or the quickstart.
#
# Inference is yours to point at: add a model provider and choose the models
# on the Models page (/admin/settings/models) once it is up. A provider's key
# may instead come from DIDEROT_PROVIDER_<NAME>_KEY in the environment this
# script runs in, which reaches only the server process.
#
# State (keys, the API token, the server's binary, log and PID) lives in
# DIDEROT_LOCAL_STACK_DIR, by default ~/.local/state/diderot-local-stack.
# Sign in to the admin UI with the API token: `cat "$DIR/api-token"`.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
STATE=${DIDEROT_LOCAL_STACK_DIR:-"$HOME/.local/state/diderot-local-stack"}
PROJECT=${DIDEROT_LOCAL_STACK_PROJECT:-diderot-local}
PG_PORT=${DIDEROT_LOCAL_PG_PORT:-25432}
QDRANT_PORT=${DIDEROT_LOCAL_QDRANT_PORT:-26334}
FILES_PORT=${DIDEROT_LOCAL_FILES_PORT:-29090}
PORT=${DIDEROT_LOCAL_PORT:-29091}
EMAIL=${DIDEROT_LOCAL_EMAIL:-admin@example.com}

if [[ -f "$STATE/serve.pid" ]] && kill -0 "$(cat "$STATE/serve.pid")" 2>/dev/null; then
    echo "a local stack server is already running (PID $(cat "$STATE/serve.pid")); run scripts/local-stack-down.sh first" >&2
    exit 1
fi

export DIDEROT_LOCAL_STACK_PROJECT=$PROJECT DIDEROT_LOCAL_PG_PORT=$PG_PORT \
    DIDEROT_LOCAL_QDRANT_PORT=$QDRANT_PORT DIDEROT_LOCAL_FILES_PORT=$FILES_PORT
docker compose -p "$PROJECT" -f "$ROOT/compose.local-stack.yaml" up -d --wait

mkdir -p "$STATE"
chmod 700 "$STATE"
umask 077
# key FILE writes a new 32-byte key to FILE unless it has one.
key() {
    if [[ ! -s "$1" ]]; then
        head -c 32 /dev/urandom | base64 > "$1"
    fi
}
key "$STATE/browser-session-keys"
key "$STATE/config-keys"
if [[ ! -s "$STATE/api-token" ]]; then
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n' > "$STATE/api-token"
fi
digest=$(printf %s "$(cat "$STATE/api-token")" | shasum -a 256 | cut -d ' ' -f 1)
printf 'tokens:\n  - sha256: %s\n    email: "%s"\n    groups: [diderot-admins, diderot-users]\n' "$digest" "$EMAIL" > "$STATE/tokens.yaml"

go -C "$ROOT" build -o "$STATE/diderot" ./cmd/diderot

ATLAS=${DIDEROT_ATLAS_BINARY:-"$HOME/bin/atlas"}
(
    export DIDEROT_DEPLOYMENT=standalone DIDEROT_LEADER_ELECTION=false
    export DIDEROT_DB_URL="postgres://diderot:diderot@127.0.0.1:$PG_PORT/diderot?sslmode=disable"
    export DIDEROT_QDRANT_ADDR="127.0.0.1:$QDRANT_PORT"
    export DIDEROT_LISTEN_ADDR="127.0.0.1:$PORT"
    export DIDEROT_INTERNAL_LISTEN_ADDR="127.0.0.1:$((PORT + 1))"
    export DIDEROT_PAGERDUTY_LISTEN_ADDR="127.0.0.1:$((PORT + 2))"
    export DIDEROT_METRICS_LISTEN_ADDR="127.0.0.1:$((PORT + 3))"
    export DIDEROT_PUBLIC_URL="http://localhost:$PORT"
    export DIDEROT_AUTH_MODE=static DIDEROT_AUTH_STATIC_TOKENS_FILE="$STATE/tokens.yaml"
    export DIDEROT_ADMIN_GROUPS=diderot-admins DIDEROT_SELF_SERVICE_GROUPS=diderot-users
    export DIDEROT_BROWSER_SESSION_KEYS_FILE="$STATE/browser-session-keys"
    export DIDEROT_CONFIG_KEYS_FILE="$STATE/config-keys"
    export DIDEROT_ASK_ENABLED=true
    export DIDEROT_IMAGES_BUCKET=diderot-files DIDEROT_IMAGES_ENDPOINT="http://127.0.0.1:$FILES_PORT"
    export DIDEROT_IMAGES_PATH_STYLE=true DIDEROT_IMAGES_REGION=us-east-1
    # The file store accepts any credentials; these keep the server off any
    # AWS profile in the environment.
    unset AWS_PROFILE AWS_SESSION_TOKEN
    export AWS_ACCESS_KEY_ID=local AWS_SECRET_ACCESS_KEY=local AWS_REGION=us-east-1
    if [[ -x "$ATLAS" ]]; then
        export DIDEROT_ATLAS_BINARY="$ATLAS"
    fi
    nohup "$STATE/diderot" serve > "$STATE/serve.log" 2>&1 &
    echo $! > "$STATE/serve.pid"
)

for _ in $(seq 1 120); do
    if ! kill -0 "$(cat "$STATE/serve.pid")" 2>/dev/null; then
        echo "diderot serve exited during startup; the end of $STATE/serve.log:" >&2
        tail -20 "$STATE/serve.log" >&2
        exit 1
    fi
    if curl -fsS "http://127.0.0.1:$PORT/healthz" > /dev/null 2>&1; then
        echo "Diderot is up: http://localhost:$PORT/admin (sign in with the API token: cat $STATE/api-token)"
        echo "MCP: http://localhost:$PORT/mcp   Log: $STATE/serve.log"
        exit 0
    fi
    sleep 1
done
echo "diderot serve did not become healthy; see $STATE/serve.log" >&2
exit 1
