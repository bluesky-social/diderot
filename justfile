set shell := ["bash", "-cu"]

default: lint test

install-tools:
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
    go install gotest.tools/gotestsum@v1.13.0
    # atlas no longer publishes the `cmd/atlas` Go submodule past
    # v0.13.1; the upstream install script fetches from an unsigned
    # mirror, so we pull directly from release.ariga.io with a
    # checksum pin. Update the version + all SHAs together.
    @mkdir -p ~/bin
    @ARCH=$(uname -s)-$(uname -m); \
    case "$ARCH" in \
      Darwin-arm64) SUFFIX=darwin-arm64; SHA=c0d8443a603991c337341a303abf3812cb9f12d74d0ec0d585245735f18c1224 ;; \
      Darwin-x86_64) SUFFIX=darwin-amd64; SHA=47f98e6f0d6a203f6c84e643f1ad99519fa8c2b5b59c177af3c449bd3ae0bb5a ;; \
      Linux-x86_64) SUFFIX=linux-amd64; SHA=c12b889e3349f0e5610aec32fe327e5a6911a0e472754a0c381c30c7c0630e88 ;; \
      *) echo "unsupported platform: $ARCH (supported: Darwin-arm64, Darwin-x86_64, Linux-x86_64)"; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/atlas "https://release.ariga.io/atlas/atlas-$SUFFIX-v1.2.0"; \
    if command -v sha256sum >/dev/null; then echo "$SHA  /tmp/atlas" | sha256sum -c -; \
    else echo "$SHA  /tmp/atlas" | shasum -a 256 -c -; fi; \
    mv /tmp/atlas ~/bin/atlas; \
    chmod +x ~/bin/atlas; \
    ~/bin/atlas version

# The development databases. `-f compose.yaml` keeps a quickstart .env, whose
# COMPOSE_FILE names compose.quickstart.yaml, from redirecting these.

up:
    docker compose -f compose.yaml up -d --remove-orphans
    until docker exec -i diderot-postgres-1 psql -U diderot diderot -c 'SELECT 1' &>/dev/null; do echo "waiting for postgres..."; sleep 1; done
    echo "postgres is ready"

down:
    docker compose -f compose.yaml down --remove-orphans

# Build and start the Docker Compose quickstart against a stub model provider,
# ingest a fixture, retrieve it, ask about it, and tear the stack down
# (docs/quickstart.md). KEEP_STACK=1 leaves it running.
[doc('End-to-end smoke test of the Docker Compose quickstart (stub model provider)')]
quickstart-smoke:
    ./scripts/quickstart-smoke.sh

psql *ARGS:
    docker exec -it diderot-postgres-1 psql -U diderot diderot {{ARGS}}

lint:
    golangci-lint run --timeout 1m ./...

test *ARGS="./...":
    # Start (or reuse) the local integration stack, then run against the
    # compose port mappings by default. Callers can still override either
    # address for a non-standard local setup.
    just up
    # Packages run in PARALLEL (default -p = GOMAXPROCS). Each
    # integration-test package gets its own isolated Postgres database
    # — a per-package clone of a migrated base DB — so cross-package
    # TRUNCATEs can't wipe each other's fixtures. See internal/testdb.
    # The base DB is bootstrapped lazily on first use under a Postgres
    # advisory lock, so no separate setup recipe is needed.
    #
    # internal/migrations.Apply needs the atlas binary; point it at
    # the local install.
    DIDEROT_TEST_DB_URL="${DIDEROT_TEST_DB_URL:-postgres://diderot:diderot@localhost:15432/diderot?sslmode=disable}" \
        DIDEROT_QDRANT_TEST_ADDR="${DIDEROT_QDRANT_TEST_ADDR:-localhost:16334}" \
        DIDEROT_ATLAS_BINARY="${DIDEROT_ATLAS_BINARY:-$HOME/bin/atlas}" \
        gotestsum --format-hide-empty-pkg --format-icons hivis -- -count=1 {{ARGS}}

test-race *ARGS="./...":
    just test -race {{ARGS}}

build:
    mkdir -p bin
    go build -o bin/ ./cmd/...

run CMD *ARGS:
    go run ./cmd/{{CMD}} {{ARGS}}

# Run the real-Slack end-to-end suite against a running deployment (or one
# scenario via FILTER). Posts as a user token into a test channel, waits for the
# bot, then checks the deployment's database. Needs DIDEROT_SLACK_XOXP_TOKEN,
# DIDEROT_E2E_SLACK_CHANNEL_ID, DIDEROT_E2E_SLACK_BOT_USER_ID,
# DIDEROT_E2E_SLACK_WORKSPACE_HOST, and DIDEROT_E2E_SQL_COMMAND in the environment
# (the tests skip without them; see docs/configuration.md). Calls real Slack
# and the deployment's models. Never in CI.
#   just test-slack-e2e                           # regression scenarios (expected green)
#   just test-slack-e2e TestSlackCaption          # caption barrier only
#   just test-slack-e2e TestEditMentionAcceptance # a mention added by an edit
test-slack-e2e FILTER="TestSlack":
    DIDEROT_E2E_SLACK=1 go test \
        -tags e2e \
        -run {{ quote(FILTER) }} \
        ./internal/e2e/... \
        -v -count=1 -timeout 10m

# Exercise a thread's Slack agent session status in the e2e test channel, as the
# bot alone. Needs DIDEROT_E2E_SLACK_BOT_TOKEN in the environment, plus the
# test-slack-e2e channel settings. The default run lasts 6m10s; pass a shorter
# duration only for smoke testing.
#   just test-slack-progress
#   just test-slack-progress 30s
test-slack-progress DURATION="6m10s":
    DIDEROT_E2E_SLACK_PROGRESS=1 DIDEROT_SLACK_PROGRESS_DURATION={{ quote(DURATION) }} go test \
        -tags e2e \
        -run '^TestSlackProgressSurfaceAcceptance$' \
        ./internal/e2e/slack \
        -v -count=1 -timeout 10m
