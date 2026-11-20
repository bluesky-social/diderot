package clientcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/diderot/internal/client"
)

func askCmd(opts Options) *cobra.Command {
	var jsonOut bool
	var session, saveDir string
	cmd := &cobra.Command{
		Use:   "ask [question...]",
		Short: "Ask Diderot a question and print its answer",
		Long: `Asks the server setup saved, waits for the answer, and prints it with its
citations. With no arguments the question is read from stdin. The answer's
session ID continues the conversation: pass it to --session with a
follow-up. Images the answer produced are listed; --save-artifacts writes
them into a directory. Ctrl-C cancels the question on the server.`,
		Example: `  diderot ask "What did we decide about the billing migration?"
  diderot ask --session 3f1c... "And who owns it now?"`,
		GroupID: GroupID,
	}
	cmd.RunE = clientRunE(opts, &jsonOut, func(cmd *cobra.Command, args []string, out *cliIO) error {
		question := strings.TrimSpace(strings.Join(args, " "))
		if question == "" && !out.interactive {
			b, err := io.ReadAll(io.LimitReader(out.stdin, 1<<20))
			if err != nil {
				return err
			}
			question = strings.TrimSpace(string(b))
		}
		if question == "" {
			return usageError("ask needs a question, as arguments or on stdin")
		}
		path, cfg, err := loadClientConfig()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if !out.json {
			out.progressf("Asking %s; an answer can take a few minutes...\n", hostOf(cfg.URL))
		}
		m := out.mcp(path, cfg)
		answer, err := m.Ask(ctx, question, strings.TrimSpace(session))
		var signIn *client.SignInError
		var toolErr *client.ToolError
		switch {
		case errors.As(err, &signIn):
			return &client.SignInError{Reason: signIn.Reason + "; run diderot setup " + cfg.URL}
		case errors.As(err, &toolErr):
			return &exitError{code: exitFailure, err: fmt.Errorf("diderot: %s", toolErr.Message)}
		case err != nil:
			return err
		}
		// The answer is printed whatever happens to its images.
		failed := false
		if saveDir != "" && len(answer.Artifacts) > 0 {
			if err := os.MkdirAll(saveDir, 0o755); err != nil { //nolint:gosec // a directory the person named
				failed = true
				out.progressf("could not create %s: %v\n", saveDir, err)
				saveDir = ""
			}
		}
		if saveDir != "" {
			for i := range answer.Artifacts {
				saved, err := m.SaveArtifact(ctx, answer.Artifacts[i], saveDir)
				if err != nil {
					failed = true
					out.progressf("could not save %s: %v\n", answer.Artifacts[i].URI, err)
					continue
				}
				answer.Artifacts[i].Path = saved
			}
		}
		if out.json {
			if err := out.result(answer); err != nil {
				return err
			}
		} else {
			printAnswer(out, answer)
		}
		if failed {
			return &exitError{code: exitPartial, err: errors.New("the answer came back, but not every image was saved"), reported: true}
		}
		return nil
	})
	cmd.Flags().StringVar(&session, "session", "", "continue the conversation with this session ID")
	cmd.Flags().StringVar(&saveDir, "save-artifacts", "", "write the answer's images into this directory")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the answer as JSON (the default when stdout is not a terminal)")
	return cmd
}

func printAnswer(out *cliIO, answer *client.Answer) {
	out.printf("%s\n", strings.TrimRight(answer.Reply, "\n"))
	if answer.IncompleteReason != "" {
		out.printf("\n(The answer is incomplete: %s.)\n", answer.IncompleteReason)
	}
	if len(answer.Artifacts) > 0 {
		out.printf("\nImages:\n")
		for _, a := range answer.Artifacts {
			where := a.URI
			if a.Path != "" {
				where = a.Path
			}
			title := a.Title
			if title == "" {
				title = a.AltText
			}
			out.printf("  %s: %s\n", title, where)
		}
	}
	out.progressf("\nSession %s. Follow up with: diderot ask --session %s \"...\"\n", answer.SessionID, answer.SessionID)
}
