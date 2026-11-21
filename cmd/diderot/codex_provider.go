package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bluesky-social/diderot/internal/codexprovider"
	"github.com/spf13/cobra"
)

func codexProviderCmd() *cobra.Command {
	var cfg codexprovider.Config
	var listen, tokenFile string
	cmd := &cobra.Command{
		Use:   "codex-provider",
		Short: "Serve a Diderot inference provider backed by signed-in Codex",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cfg.Timeout <= 0 || cfg.MaxConcurrent < 1 || strings.TrimSpace(cfg.Model) == "" {
				return errors.New("set a model, a positive timeout and a positive concurrency limit")
			}
			token, err := os.ReadFile(tokenFile)
			if err != nil {
				return fmt.Errorf("read provider token: %w", err)
			}
			cfg.Token = strings.TrimSpace(string(token))
			if len(cfg.Token) < 32 || strings.ContainsAny(cfg.Token, " \t\r\n") {
				return errors.New("the provider token must contain at least 32 non-whitespace characters")
			}
			binary, err := exec.LookPath(cfg.Binary)
			if err != nil {
				return fmt.Errorf("find Codex executable: %w", err)
			}
			cfg.Binary, err = filepath.Abs(binary)
			if err != nil {
				return err
			}
			listener, err := net.Listen("tcp", listen)
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			server := &http.Server{
				Handler: codexprovider.NewHandler(cfg), ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
				WriteTimeout: cfg.Timeout + 30*time.Second,
				BaseContext:  func(net.Listener) context.Context { return ctx },
			}
			shutdownDone := make(chan struct{})
			stop := context.AfterFunc(ctx, func() {
				defer close(shutdownDone)
				shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := server.Shutdown(shutdown); err != nil {
					_ = server.Close()
				}
			})
			cmd.PrintErrln("Codex provider listening on", listener.Addr())
			err = server.Serve(listener)
			if !stop() {
				<-shutdownDone
			}
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:9460", "HTTP listen address")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "File containing the provider bearer token")
	cmd.Flags().StringVar(&cfg.Binary, "codex-bin", "codex", "Path to the Codex executable")
	cmd.Flags().StringVar(&cfg.Model, "model", "", "Codex model to expose in Diderot")
	cmd.Flags().DurationVar(&cfg.Timeout, "timeout", 90*time.Second, "Deadline for a Codex request")
	cmd.Flags().IntVar(&cfg.MaxConcurrent, "max-concurrent", 2, "Maximum simultaneous Codex invocations")
	_ = cmd.MarkFlagRequired("token-file")
	_ = cmd.MarkFlagRequired("model")
	return cmd
}
