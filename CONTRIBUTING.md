# Contributing to Diderot

Thanks for your interest in Diderot. Bug reports, fixes, and improvements are
welcome. For a large change, open an issue first so we can agree on the
approach before you write the code.

To report a security vulnerability, follow [SECURITY.md](SECURITY.md)
instead of opening an issue.

## Development setup

You need:

- Go, at the version in [`go.mod`](go.mod)
- Docker with Compose, for the local Postgres and Qdrant
- [`just`](https://github.com/casey/just)
- `golangci-lint`, `gotestsum`, and `atlas`, which `just install-tools`
  installs (`atlas` goes to `~/bin/atlas`, checksum-pinned)

```sh
just install-tools   # once
just up              # start Postgres (localhost:15432) and Qdrant (localhost:16334)
go build ./...
```

To run a server locally, see the [quickstart](docs/quickstart.md). Every
setting is in the [configuration reference](docs/configuration.md), and
[`docs/design.md`](docs/design.md) explains how the parts fit together.

## Tests and lint

```sh
just test            # every package, against the Compose stack
just test ./internal/retrieve/...   # one package
just lint            # golangci-lint
```

`just test` starts or reuses the Compose stack and points the integration
tests at it, through `DIDEROT_TEST_DB_URL`, `DIDEROT_QDRANT_TEST_ADDR`, and
`DIDEROT_ATLAS_BINARY`. Without `DIDEROT_TEST_DB_URL`, the database tests
skip. Each package that needs a database gets its own clone, so packages run
in parallel.

CI runs the tests with the race detector, the linter, a build, and security
checks. Before you push, make sure these pass:

```sh
go build ./... && go vet ./... && just lint && just test
```

A change to configuration should update
[`docs/configuration.md`](docs/configuration.md) in the same pull request;
`internal/config/reference_test.go` fails when a variable the code reads has
no row there. A schema change is a new migration in
`internal/migrations/sql/` (applied by Atlas when the server starts); never
edit a migration that has already been released.

## Developing the browser UI

A credential-free fixture server renders the real embedded templates and
assets without Postgres, proxy identity headers, or Diderot credentials:

```sh
just run diderot ui-dev
# administrator pages: http://127.0.0.1:8082/admin/
# personal session page: http://127.0.0.1:8082/sessions
```

`diderot --help` leaves out `ui-dev`, like the other tools for developing
Diderot (`session-dev`); they run when typed. The fixture server's synthetic
fixture is
[`internal/adminui/testdata/ui-dev.json`](internal/adminui/testdata/ui-dev.json).
The server rereads it on every request, so HTML, CSS, JavaScript, and fixture
edits appear on refresh. The fixture links Ask, source, report,
operations, audit, and personal session views, and covers transcript tool
calls and results, malformed blocks, failures, usage, and overflow. The
per-session privacy actions work in memory, so the whole form and CSRF flow
can be developed locally; every other mutation is disabled. `ui-dev` binds to
loopback; `--fixture` loads another redacted fixture and `--listen` picks the
address. A test rejects common secret markers in the committed fixture, and
any email address or web address in it that is not a placeholder or a public
service.

## Private session fixtures

To check the session parser, chunker, and summarizer against real cleaned
session documents without committing them, set `DIDEROT_SESSION_DOC_FIXTURES`
to a colon-separated list of `.ndjson.gz` paths outside the repository and run:

```sh
go test -race ./internal/sessioningest -run '^TestCapturedSessionDocument' -count=1 -v
```

The parser and chunker test needs only those files. The summarizer test also
needs `DIDEROT_TEST_DB_URL` and `DIDEROT_ATLAS_BINARY`; it uses canned model
responses, not live inference. These tests check record coverage and map-input
delivery, not summary quality. Keep the fixtures out of commits and reviews.

## Pull requests

- **Title.** Pull requests are squash-merged, and the title becomes the commit
  message and the changelog entry. Titles follow
  [Conventional Commits](https://www.conventionalcommits.org/): `feat: ...`,
  `fix: ...`, `docs: ...`, or a subsystem such as `ask: ...`. A CI check
  enforces the prefix. Mark a breaking change with `!` (`feat!: ...`) or a
  `BREAKING CHANGE:` footer.
- **Review.** Every pull request needs an approving review from a code owner
  of each path it touches (see [`.github/CODEOWNERS`](.github/CODEOWNERS)),
  and passing CI. Branches must be up to date with `main` before they merge.
- **Scope.** Keep a pull request to one change, with tests for new behavior.
  Describe what changed and why, and how you tested it.
- **Fixtures.** Never commit real transcripts, credentials, tokens, database
  URLs, or private message content, not even in test fixtures. Use synthetic
  data, such as the fixtures under `internal/adminui/testdata/` and
  `internal/sessionfixture/testdata/`.

Releases are cut by [release-please](https://github.com/googleapis/release-please)
from the merged titles. Contributors do not need to do anything for a
release.

## License

Diderot is dual-licensed under the [MIT](LICENSE-MIT) and
[Apache 2.0](LICENSE-APACHE) licenses (see [LICENSE-DUAL](LICENSE-DUAL)).
By contributing, you agree that your contributions are licensed under both.
