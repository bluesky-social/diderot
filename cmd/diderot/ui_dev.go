package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/bluesky-social/diderot/internal/adminui"
	"github.com/spf13/cobra"
)

func uiDevCmd() *cobra.Command {
	var fixture, listen string
	cmd := &cobra.Command{
		Use:   "ui-dev",
		Short: "serve Diderot's web UI locally",
		// A tool for developing Diderot (CONTRIBUTING.md), not for running it.
		Hidden: true,
		Long: `Serve Diderot's real embedded administrator templates and assets from a
reload-on-refresh JSON fixture. This command requires no credentials or database,
and binds to loopback unless --listen is explicitly changed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUIDev(cmd, fixture, listen)
		},
	}
	cmd.Flags().StringVar(&fixture, "fixture", "internal/adminui/testdata/ui-dev.json", "synthetic administrator UI fixture JSON")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8082", "HTTP listen address")
	return cmd
}

func runUIDev(cmd *cobra.Command, fixture, listen string) error {
	if _, err := net.ResolveTCPAddr("tcp", listen); err != nil {
		return fmt.Errorf("ui-dev listen address: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	handler, err := adminui.NewDevHandler(fixture, logger.With("component", "ui-dev"))
	if err != nil {
		return err
	}
	server := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	logger.Info("diderot ui-dev listening", "admin_url", "http://"+listen+"/admin/", "sessions_url", "http://"+listen+"/sessions", "fixture", fixture)
	select {
	case <-cmd.Context().Done():
		shutdownErr := server.Close()
		if shutdownErr != nil {
			return shutdownErr
		}
		return cmd.Context().Err()
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("ui-dev listen: %w", err)
	}
}
