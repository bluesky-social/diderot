// Package migrations embeds the atlas SQL migration files into the
// diderot binary and exposes a single Apply function that the
// process runs on startup. No separate migrate-job, no init
// container — one binary, one container.
//
// The first migration is a baseline that creates the whole schema of a new
// install. It carries the version of the last migration of the history it
// replaced, so a database that applied all of that history already records
// the version and Atlas skips the baseline there. A database that stopped
// partway through that history has a schema but not the version; the
// baseline's first statement refuses it and names the release to deploy
// first.
//
// The role story used to separate diderot_migrate (DDL) from
// diderot_app (DML). That separation was retired in favor of a
// single diderot_app role with CREATE on the schema; Apply runs
// whatever creds the caller passes (in practice cfg.DBURL, the
// application's own role). If a least-privilege need resurfaces later,
// wire a second URL back in.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"ariga.io/atlas-go-sdk/atlasexec"
)

// migrationsFS is the collection of *.sql files + atlas.sum shipped
// alongside the diderot binary. Atlas reads them through the
// atlasexec.WithMigrations(fs.FS) option, which unpacks them into
// the atlasexec working directory on each Apply call.
//
//go:embed sql/*.sql sql/atlas.sum
var migrationsFS embed.FS

// defaultAtlasBinaryPath is the absolute path of the atlas binary
// inside the runtime image. The Dockerfile copies /bin/atlas from
// the arigaio/atlas:1.2.0-alpine stage into /usr/local/bin/atlas,
// so this path is guaranteed present in every built image.
//
// Local dev typically has atlas at ~/bin/atlas (see the install-tools
// recipe in justfile). To run `go run ./cmd/diderot serve` on a
// laptop, set DIDEROT_ATLAS_BINARY to that path (or symlink
// /usr/local/bin/atlas). We don't fall back to $PATH lookup because
// an ambient atlas pointing at a different version is worse than a
// clean error.
const defaultAtlasBinaryPath = "/usr/local/bin/atlas"

// atlasBinary returns the atlas binary path to invoke, honoring
// $DIDEROT_ATLAS_BINARY if set. Returns an error if neither the
// override nor the default resolves to an executable file.
func atlasBinary() (string, error) {
	path := os.Getenv("DIDEROT_ATLAS_BINARY")
	if path == "" {
		path = defaultAtlasBinaryPath
	}
	if _, err := exec.LookPath(path); err != nil {
		return "", fmt.Errorf("atlas binary %q not executable (set DIDEROT_ATLAS_BINARY if installed elsewhere): %w", path, err)
	}
	return path, nil
}

// Apply runs `atlas migrate apply` against the given database URL
// using the embedded migration directory. Idempotent + safe to call
// from every replica concurrently — atlas acquires a postgres
// advisory lock internally before touching schema state.
//
// Returns the number of migrations applied (0 means "already up to
// date"). Logs the atlas result via the supplied logger.
func Apply(ctx context.Context, dbURL string, logger *slog.Logger) (int, error) {
	return apply(ctx, dbURL, subFS(migrationsFS, "sql"), logger)
}

// apply is Apply with the migration directory given, so tests can apply
// directories other than the embedded one.
func apply(ctx context.Context, dbURL string, dir fs.FS, logger *slog.Logger) (int, error) {
	if dbURL == "" {
		return 0, fmt.Errorf("migrations: empty db url")
	}
	if logger == nil {
		logger = slog.Default()
	}

	// atlasexec unpacks dir into workdir.Path() every call. Cheap (few
	// KB of SQL), but we defer Close to clean up the tmpdir afterward.
	workdir, err := atlasexec.NewWorkingDir(atlasexec.WithMigrations(dir))
	if err != nil {
		return 0, fmt.Errorf("migrations: working dir: %w", err)
	}
	defer func() { _ = workdir.Close() }()

	binPath, err := atlasBinary()
	if err != nil {
		return 0, fmt.Errorf("migrations: %w", err)
	}
	client, err := atlasexec.NewClient(workdir.Path(), binPath)
	if err != nil {
		return 0, fmt.Errorf("migrations: atlas client (binary at %q): %w",
			binPath, err)
	}

	start := time.Now()
	result, err := client.MigrateApply(ctx, &atlasexec.MigrateApplyParams{
		URL: dbURL,
	})
	if err != nil {
		return 0, fmt.Errorf("migrations: apply: %w", err)
	}
	applied := len(result.Applied)
	logger.Info("migrations applied",
		"applied", applied,
		"current", result.Current,
		"target", result.Target,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return applied, nil
}

// subFS returns an fs.FS rooted at the given prefix inside root.
// atlas doesn't want to see the `sql/` prefix; it wants the
// directory listing flat. Equivalent to fs.Sub but explicit to
// avoid the import dance.
func subFS(root embed.FS, prefix string) *stripFS {
	return &stripFS{root: root, prefix: prefix + "/"}
}

// SumHash returns a short stable fingerprint of the embedded
// migration directory — an FNV-64a over sql/atlas.sum, hex-encoded.
// atlas.sum hashes every migration file, so any migration change
// (add, edit, rehash) changes this value. internal/testdb keys the
// per-schema-version test base database on it: a checkout with
// different migrations gets a different base DB instead of silently
// reusing one migrated for another version's schema.
func SumHash() string {
	data, err := migrationsFS.ReadFile("sql/atlas.sum")
	if err != nil {
		// Unreachable: atlas.sum is in the go:embed directive, so a
		// build that compiles has it. Guard anyway; a constant name
		// beats a panic in test bootstrap.
		return "nosum"
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return fmt.Sprintf("%016x", h.Sum64())
}
