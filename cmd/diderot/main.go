// Command diderot is the Diderot server (`diderot serve`) with the few
// maintenance commands that run beside it with its configuration, and the
// client commands (setup, doctor, ask) that reach a server over MCP.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/diderot/internal/clientcmd"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// groupServer lists the commands that run where the server runs.
const groupServer = "server"

func buildRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "diderot",
		Short:        "Diderot: ask your organization's knowledge, and run the server that answers",
		Version:      Version,
		SilenceUsage: true,
		// main prints runErr itself; without this cobra also prints it,
		// so every failure appeared twice ("Error: ..." then "error: ...").
		SilenceErrors: true,
	}

	clientcmd.AddCommands(root, clientcmd.Options{Version: Version})
	root.AddGroup(&cobra.Group{ID: groupServer, Title: "Run a Diderot server (where it runs, with its configuration):"})
	for _, cmd := range []*cobra.Command{serveCmd(), probeCmd(), qdrantAuditCmd(), resetAdminCmd()} {
		cmd.GroupID = groupServer
		root.AddCommand(cmd)
	}
	root.AddCommand(uiDevCmd(), sessionDevCmd())

	return root
}

func main() {
	// The handler must write directly to stderr, NOT wrap
	// slog.Default().Handler(): the built-in default handler writes through
	// the log package, and slog.SetDefault bridges the log package back into
	// slog — wrapping it makes the first default-logger write re-enter
	// log.(*Logger).output and self-deadlock on its mutex.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	Version = clientcmd.ResolveVersion(Version)

	if runErr := clientcmd.Execute(context.Background(), buildRootCmd()); runErr != nil {
		fmt.Fprintln(os.Stderr, "error:", runErr)
		os.Exit(clientcmd.ExitCode(runErr))
	}
}
