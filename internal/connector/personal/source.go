// Package personal indexes private mail and calendars through read-only
// connections to an account configured by the administrator.
package personal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

const (
	fieldOwner    = "owner_email"
	fieldUsername = "username"
	fieldPassword = "password"
	fieldInterval = "poll_interval"
)

func accountFields() []settings.Field {
	return []settings.Field{
		{Key: fieldOwner, Label: "Diderot owner email", Kind: settings.KindText, Required: true,
			Help: "The verified email used to sign in to Diderot. Only this identity can retrieve the indexed content."},
		{Key: fieldUsername, Label: "Account username", Kind: settings.KindText, Required: true},
		{Key: fieldPassword, Label: "Password or app password", Kind: settings.KindSecret, Required: true},
		{Key: fieldInterval, Label: "Poll every", Kind: settings.KindDuration, Required: true, Default: "5m",
			Help: "Poll interval of at least 1 minute."},
	}
}

func accountErrors(values settings.Values) settings.FieldErrors {
	errs := settings.FieldErrors{}
	if _, err := auth.ConfiguredEmail(settingsString(values, fieldOwner)); err != nil {
		errs[fieldOwner] = "enter the owner's sign-in email address"
	}
	if d, err := time.ParseDuration(settingsString(values, fieldInterval)); err == nil && d < time.Minute {
		errs[fieldInterval] = "must be at least 1 minute"
	}
	return errs
}

func settingsString(values settings.Values, key string) string {
	s, _ := values[key].(string)
	return s
}

func ownerEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// namespace keeps records from different accounts or collections distinct,
// even when the upstream reuses IDs. The owner is part of the namespace so
// reconfiguring a connector never changes the audience of an existing row.
func namespace(parts ...string) string {
	key, _ := json.Marshal(parts)
	return fmt.Sprintf("%x:", sha256.Sum256(key))
}

func cleanText(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ToValidUTF8(text, "�"), "\x00", ""))
}

func run(ctx context.Context, deps source.Deps, name string, cfg settings.Resolved, poll func(context.Context) error) error {
	sup := worker.NewSupervisor(deps.Logger, deps.Metrics)
	sup.AddRunner(worker.Runner{
		Name: name + "-sync", Interval: max(cfg.Duration(fieldInterval), time.Minute), BootTick: true, Run: poll,
	})
	sup.Run(ctx)
	return nil
}

func expand(ctx context.Context, deps source.Deps, eventID int64, name string) (source.Expanded, error) {
	stored, err := deps.Sink.StoredBody(ctx, eventID)
	if err != nil {
		return source.Expanded{}, err
	}
	if stored.Source != name {
		return source.Expanded{}, source.ErrNotExpandable
	}
	return source.Expanded{Content: stored.Body}, nil
}

// removeMissing runs only after a complete upstream listing and successful
// writes. A failed or partial scan cannot make existing items disappear.
func removeMissing(ctx context.Context, sink *events.DBSink, name string, existing []string, seen map[string]bool) error {
	for _, id := range existing {
		if seen[id] {
			continue
		}
		if _, err := sink.Delete(ctx, name, id); err != nil {
			return err
		}
	}
	return nil
}
