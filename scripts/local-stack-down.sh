#!/usr/bin/env bash
# Remove everything scripts/local-stack-up.sh made: stop the server, remove
# the Compose project's containers and volumes, and delete the state
# directory with its keys and token.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
STATE=${DIDEROT_LOCAL_STACK_DIR:-"$HOME/.local/state/diderot-local-stack"}
PROJECT=${DIDEROT_LOCAL_STACK_PROJECT:-diderot-local}

if [[ -f "$STATE/serve.pid" ]]; then
    pid=$(cat "$STATE/serve.pid")
    if kill "$pid" 2>/dev/null; then
        for _ in $(seq 1 30); do
            kill -0 "$pid" 2>/dev/null || break
            sleep 1
        done
        kill -9 "$pid" 2>/dev/null || true
    fi
fi

docker compose -p "$PROJECT" -f "$ROOT/compose.local-stack.yaml" down --volumes --remove-orphans

# Remove only a directory this script's partner made.
if [[ -d "$STATE" ]]; then
    if [[ -f "$STATE/tokens.yaml" || -f "$STATE/serve.pid" || -f "$STATE/config-keys" ]]; then
        rm -rf "$STATE"
    else
        echo "not removing $STATE: it doesn't look like a local stack's state directory" >&2
    fi
fi
echo "Local stack removed."
