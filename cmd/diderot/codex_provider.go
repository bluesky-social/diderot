package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
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
			if cfg.Timeout <= 0 || cfg.MaxConcurrent < 1 {
				return errors.New("set a positive timeout and a positive concurrency limit")
			}
			cfg.Model = strings.TrimSpace(cfg.Model)
			token, err := os.ReadFile(tokenFile)
			if err != nil {
				return fmt.Errorf("read provider token: %w", err)
			}
			cfg.Token = strings.TrimSpace(string(token))
			if len(cfg.Token) < 32 || strings.ContainsAny(cfg.Token, " \t\r\n") {
				return errors.New("the provider token must contain at least 32 non-whitespace characters")
			}
			if _, err := os.Stat(cfg.AuthFile); err != nil {
				return fmt.Errorf("locate ChatGPT auth file: %w", err)
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
	cmd.Flags().StringVar(&cfg.AuthFile, "auth-file", "", "Path to a dedicated Codex ChatGPT auth.json")
	cmd.Flags().StringVar(&cfg.Model, "model", "", "Expose only this model (default: all selectable account models)")
	cmd.Flags().DurationVar(&cfg.Timeout, "timeout", 90*time.Second, "Deadline for a Codex request")
	cmd.Flags().IntVar(&cfg.MaxConcurrent, "max-concurrent", 2, "Maximum simultaneous Codex inference requests")
	_ = cmd.MarkFlagRequired("token-file")
	_ = cmd.MarkFlagRequired("auth-file")
	cmd.AddCommand(codexLoginCmd())
	return cmd
}

func codexLoginCmd() *cobra.Command {
	var authFile string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign into the Codex provider with a ChatGPT device code",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if err := codexprovider.DeviceLogin(ctx, authFile, cmd.OutOrStdout()); err != nil {
				return err
			}
			cmd.Println("Codex sign-in complete.")
			return nil
		},
	}
	cmd.Flags().StringVar(&authFile, "auth-file", "", "Path to the provider's dedicated Codex auth.json")
	_ = cmd.MarkFlagRequired("auth-file")
	return cmd
}
