package main

import (
	"fmt"
	"os"
	"os/user"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/dbconn"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
)

// resetAdminCmd is break-glass for an install whose administrators are
// locked out. It runs where the server runs, with its database access, never
// through the API.
func resetAdminCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset-admin",
		Short: "Re-enable the bootstrap admin (admin / admin); run on the server with its database access",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			pool, err := dbconn.NewPool(cmd.Context(), dbconn.Options{URL: cfg.DBURL, IAMAuth: cfg.DBIAMAuth})
			if err != nil {
				return fmt.Errorf("connect Postgres: %w", err)
			}
			defer pool.Close()
			row, err := setup.ResetBootstrapAdmin(cmd.Context(), settings.NewStore(pool, nil), serverActor())
			if err != nil {
				return err
			}
			fmt.Printf("The bootstrap admin is enabled (version %d). Sign in at /admin as admin / admin, restore another administrator, and turn it off again.\n", row.Version)
			if len(cfg.BrowserSessionKeys) == 0 {
				fmt.Println("DIDEROT_BROWSER_SESSION_KEYS is not set here: the server needs it to offer the bootstrap admin.")
			}
			return nil
		},
	}
}

// serverActor names who ran a server-side command in the audit log.
func serverActor() string {
	name := "unknown"
	if current, err := user.Current(); err == nil && current.Username != "" {
		name = current.Username
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return "cli:" + name + "@" + host
}
