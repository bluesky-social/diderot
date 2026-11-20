// Command diderot is the diderot people install: the client commands alone
// (setup, doctor, ask, sessions), which reach a Diderot server over MCP and,
// for sessions, its session push route. It is the
// module's root package so that `go install github.com/bluesky-social/diderot@vX.Y.Z`
// installs it as diderot, and it is what the release archives carry. The
// server and its maintenance commands are cmd/diderot, in the container
// image; this binary leaves out their whole dependency graph (main_test.go
// checks).
package main

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/diderot/internal/clientcmd"
)

// Version is set at build time via -ldflags.
var Version = "dev"

func buildRootCmd() *cobra.Command {
	version := clientcmd.ResolveVersion(Version)
	root := &cobra.Command{
		Use:     "diderot",
		Short:   "Diderot: ask your organization's knowledge from the shell and your coding agents",
		Version: version,
		// main prints the error itself.
		SilenceUsage: true, SilenceErrors: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	clientcmd.AddCommands(root, clientcmd.Options{Version: version})
	return root
}

func main() {
	if len(os.Args) > 1 && slices.Contains(clientcmd.ServerCommands, os.Args[1]) {
		fmt.Fprintf(os.Stderr, "error: diderot %s runs where the Diderot server runs, and is in the server's container image, "+
			"not in this download, which has only setup, doctor, ask, and sessions\n", os.Args[1])
		os.Exit(2)
	}
	if err := clientcmd.Execute(context.Background(), buildRootCmd()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(clientcmd.ExitCode(err))
	}
}
