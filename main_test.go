package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientPackages are the only packages of this module the client binary
// may contain: the client commands, the client, the harness writers, the
// embedded skill, and what ships sessions (with the session decoders it
// converts them with).
var clientPackages = []string{
	"github.com/bluesky-social/diderot",
	"github.com/bluesky-social/diderot/internal/clientcmd",
	"github.com/bluesky-social/diderot/internal/client",
	"github.com/bluesky-social/diderot/internal/harness",
	"github.com/bluesky-social/diderot/internal/sessiondecode",
	"github.com/bluesky-social/diderot/internal/sessionship",
	"github.com/bluesky-social/diderot/skills",
}

// serverDependencies are modules only the server needs. Any of them in the
// client's graph means a server package crept in by another route.
var serverDependencies = []string{
	"github.com/jackc/pgx", "github.com/qdrant/", "github.com/slack-go/", "github.com/aws/",
	"github.com/cloudwego/", "github.com/klippa-app/", "github.com/tetratelabs/wazero", "k8s.io/",
	"github.com/xuri/excelize", "github.com/zricethezav/gitleaks", "ariga.io/", "github.com/prometheus/",
	"google.golang.org/grpc", "github.com/anthropics/", "github.com/openai/",
}

// TestServerCommandsSayWhereTheyAre: a server command run on the client-only
// binary says it is in the server's image, with a usage exit code, and the
// help lists the client commands alone.
func TestServerCommandsSayWhereTheyAre(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "diderot")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	require.NoError(t, err, "%s", out)

	out, err = exec.Command(bin, "serve").CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 2, exit.ExitCode())
	assert.Contains(t, string(out), "diderot serve runs where the Diderot server runs, and is in the server's container image")

	out, err = exec.Command(bin, "--help").CombinedOutput()
	require.NoError(t, err)
	for _, name := range []string{"setup", "doctor", "ask", "sessions"} {
		assert.Contains(t, string(out), "  "+name+" ")
	}
	assert.NotContains(t, string(out), "  serve ")
}

// TestClientBuildHasNoServerDependencies keeps the downloadable binary the
// client alone: its dependency graph, for every platform the releases build,
// holds none of the server's packages.
func TestClientBuildHasNoServerDependencies(t *testing.T) {
	for _, target := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}} {
		t.Run(target[0]+"/"+target[1], func(t *testing.T) {
			cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
			cmd.Env = append(cmd.Environ(), "GOOS="+target[0], "GOARCH="+target[1], "CGO_ENABLED=0")
			out, err := cmd.Output()
			require.NoError(t, err)
			for _, pkg := range strings.Fields(string(out)) {
				if strings.HasPrefix(pkg, "github.com/bluesky-social/diderot") {
					assert.Contains(t, clientPackages, pkg, "a server package in the client binary")
				}
				for _, server := range serverDependencies {
					assert.False(t, strings.HasPrefix(pkg, server), "%s is a server dependency", pkg)
				}
			}
		})
	}
}
