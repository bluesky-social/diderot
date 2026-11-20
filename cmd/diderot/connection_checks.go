package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/adminui"
	"github.com/bluesky-social/diderot/internal/askconn"
	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/worker"
)

// connectionCheckInterval is how often the leader tests each tool
// connection and model provider that is on.
const connectionCheckInterval = 10 * time.Minute

// checkActor is who a periodic check's record names.
const checkActor = "diderot"

// connectionChecks tests each tool connection and model provider that is
// on, as its page's Test does, and keeps the result under its row, where
// every replica's pages read it: a connection or provider that stops working
// after it was saved needs attention, and a provider's test is the listing
// of its models that the Models page offers and checks the models in use
// against.
func connectionChecks(store *settings.Store, pool *pgxpool.Pool, logger *slog.Logger) worker.Runner {
	logger = logger.With("component", "connection-checks")
	return worker.Runner{
		Name: "tool-connection-checks", Interval: connectionCheckInterval, BootTick: true, Logger: logger,
		Run: func(ctx context.Context) error {
			var failed error
			rows := append(store.FamilyRows(askconn.Prefix), store.FamilyRows(modelprovider.Prefix)...)
			for _, row := range rows {
				if !row.Row.Enabled {
					continue
				}
				check := adminui.ConnectionCheck{Version: row.Row.Version}
				record, err := store.Test(ctx, settings.Actor{Email: checkActor}, row.Row.Connector, nil)
				var fieldErrs settings.FieldErrors
				switch {
				case errors.As(err, &fieldErrs) || errors.Is(err, settings.ErrInvalid):
					record = settings.TestRecord{By: checkActor, At: time.Now().UTC(), TestReport: settings.TestReport{Checks: []settings.Check{{
						Name: "Credential", Detail: "Diderot can't read the saved credential.",
						Remedy: "Paste the credential again and save.",
					}}}}
				case errors.Is(err, settings.ErrUnknownConnector):
					continue
				case err != nil:
					failed = errors.Join(failed, fmt.Errorf("%s: %w", row.Row.Connector, err))
					continue
				}
				check.Test = record
				if !record.Passed {
					logger.Warn("connection check failed", "row", row.Row.Connector)
				}
				if err := saveConnectionCheck(ctx, pool, row.Row.Connector, check); err != nil {
					failed = errors.Join(failed, err)
				}
			}
			return failed
		},
	}
}

func saveConnectionCheck(ctx context.Context, pool *pgxpool.Pool, row string, check adminui.ConnectionCheck) error {
	value, err := json.Marshal(check)
	if err != nil {
		return fmt.Errorf("encode connection check: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO source_state (source, key, value, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (source, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		row, adminui.ConnectionCheckKey, value); err != nil {
		return fmt.Errorf("save connection check: %w", err)
	}
	return nil
}
