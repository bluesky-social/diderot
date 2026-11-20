package modelfacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Where each list is published.
const (
	ModelsDevURL = "https://models.dev/api.json"
	LiteLLMURL   = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
)

// stateSource is the source_state source the lists and their status are
// kept under: one row per list, and one for the status.
const (
	stateSource = "models-catalog"
	statusKey   = "status"
)

// RefreshAfter is how old a list is before it is read again; RetryAfter how
// long a failed read waits; StaleAfter how long reads may fail before the
// lists' age needs an administrator's attention.
const (
	RefreshAfter = 24 * time.Hour
	RetryAfter   = time.Hour
	StaleAfter   = 7 * 24 * time.Hour
)

// ListStatus is how the reads of one list are going.
type ListStatus struct {
	// FetchedAt is when the list was last read, or found unchanged.
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	// AttemptedAt is the latest read, and Error why it failed; empty when
	// it worked. FailingSince is the first of the failures in a row.
	AttemptedAt  time.Time `json:"attempted_at,omitzero"`
	Error        string    `json:"error,omitempty"`
	FailingSince time.Time `json:"failing_since,omitzero"`
	// ETag is the list's version as its server names it, sent back so an
	// unchanged list is not downloaded again.
	ETag string `json:"etag,omitempty"`
	// Entries is how many models the kept copy describes.
	Entries int `json:"entries,omitempty"`
}

// Status is each list's ListStatus, by source.
type Status map[string]ListStatus

// StaleSince is when the catalog went stale: the time since which a list
// could not be read, once that is longer than StaleAfter. ok is false while
// every list is fresh enough, and before any read was tried.
func (s Status) StaleSince(now time.Time) (since time.Time, source, reason string, ok bool) {
	for _, name := range []string{SourceModelsDev, SourceLiteLLM} {
		list, tried := s[name]
		if !tried || list.Error == "" {
			continue
		}
		from := list.FetchedAt
		if from.IsZero() {
			from = list.FailingSince
		}
		if !from.IsZero() && now.Sub(from) > StaleAfter && (!ok || from.Before(since)) {
			since, source, reason, ok = from, name, list.Error, true
		}
	}
	return since, source, reason, ok
}

// DB is the database the lists are kept in (*pgxpool.Pool).
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func readState(ctx context.Context, db DB, key string, into any) (bool, error) {
	var raw []byte
	err := db.QueryRow(ctx, `SELECT value FROM source_state WHERE source = $1 AND key = $2`, stateSource, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("model facts: read %s: %w", key, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return false, fmt.Errorf("model facts: decode %s: %w", key, err)
	}
	return true, nil
}

func writeState(ctx context.Context, db DB, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("model facts: encode %s: %w", key, err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO source_state (source, key, value, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (source, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		stateSource, key, raw); err != nil {
		return fmt.Errorf("model facts: save %s: %w", key, err)
	}
	return nil
}

// ReadStatus reads how the reads of each list are going.
func ReadStatus(ctx context.Context, db DB) (Status, error) {
	status := Status{}
	if _, err := readState(ctx, db, statusKey, &status); err != nil {
		return nil, err
	}
	return status, nil
}

// Fetcher reads the published lists and keeps them in the database.
type Fetcher struct {
	// HTTPClient makes the requests; nil is http.DefaultClient.
	HTTPClient *http.Client
	// URLs is where each list is read from, by source; a source left out
	// is read from where it is published.
	URLs map[string]string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

type listSource struct {
	name, url string
	parse     func([]byte) (List, error)
}

func (f Fetcher) sources() []listSource {
	sources := []listSource{
		{SourceModelsDev, ModelsDevURL, ParseModelsDev},
		{SourceLiteLLM, LiteLLMURL, ParseLiteLLM},
	}
	for index := range sources {
		if override := f.URLs[sources[index].name]; override != "" {
			sources[index].url = override
		}
	}
	return sources
}

// Refresh reads each list that is due: one never read, one read longer ago
// than RefreshAfter, and one whose latest read failed longer ago than
// RetryAfter; with force, every list. A list that can't be read keeps its
// last copy, and the status says why. The error is of the database only.
func (f Fetcher) Refresh(ctx context.Context, db DB, force bool) (Status, error) {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	status, err := ReadStatus(ctx, db)
	if err != nil {
		return nil, err
	}
	changed := false
	for _, source := range f.sources() {
		current := status[source.name]
		wait := RefreshAfter
		since := current.FetchedAt
		if current.Error != "" {
			wait, since = RetryAfter, current.AttemptedAt
		}
		if !force && !since.IsZero() && now().Sub(since) < wait {
			continue
		}
		changed = true
		at := now().UTC()
		list, etag, err := f.fetch(ctx, source, current.ETag)
		current.AttemptedAt = at
		switch {
		case errors.Is(err, errUnchanged):
			current.FetchedAt, current.Error, current.FailingSince = at, "", time.Time{}
		case err != nil:
			current.Error = err.Error()
			if current.FailingSince.IsZero() {
				current.FailingSince = at
			}
		default:
			list.FetchedAt = at
			if err := writeState(ctx, db, source.name, list); err != nil {
				return nil, err
			}
			current.FetchedAt, current.Error, current.FailingSince = at, "", time.Time{}
			current.ETag, current.Entries = etag, len(list.Entries)
		}
		status[source.name] = current
	}
	if changed {
		if err := writeState(ctx, db, statusKey, status); err != nil {
			return nil, err
		}
	}
	return status, nil
}

// errUnchanged is a list its server says has not changed since it was read.
var errUnchanged = errors.New("unchanged")

// fetch reads one list. The request says nothing about the deployment: it
// carries no credential, no cookie and no identifying header, only the
// version of the copy already held. Errors are plain words for the page.
func (f Fetcher) fetch(ctx context.Context, source listSource, etag string) (List, string, error) {
	client := f.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.url, nil)
	if err != nil {
		return List{}, "", errors.New("its address is not valid")
	}
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return List{}, "", errors.New("it did not answer in time")
		}
		return List{}, "", errors.New("it could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotModified && etag != "":
		return List{}, "", errUnchanged
	case resp.StatusCode != http.StatusOK:
		return List{}, "", fmt.Errorf("it answered HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil {
		return List{}, "", errors.New("its answer could not be read")
	}
	if len(raw) > responseLimit {
		return List{}, "", errors.New("its answer is too large")
	}
	list, err := source.parse(raw)
	if err != nil {
		return List{}, "", errors.New("its answer is not the list expected")
	}
	return list, resp.Header.Get("ETag"), nil
}

// Reader serves the catalog from the lists kept in the database, reading
// them again only when a list was fetched since.
type Reader struct {
	db DB

	mu      sync.Mutex
	stamp   string
	catalog *Catalog
}

// NewReader returns a reader of the lists kept in db.
func NewReader(db DB) *Reader { return &Reader{db: db, catalog: Merge()} }

// Load returns the catalog as last fetched, and how the reads are going.
// Before any list was read the catalog is empty.
func (r *Reader) Load(ctx context.Context) (*Catalog, Status, error) {
	status, err := ReadStatus(ctx, r.db)
	if err != nil {
		return nil, nil, err
	}
	stamp := ""
	for _, name := range []string{SourceModelsDev, SourceLiteLLM} {
		stamp += status[name].FetchedAt.Format(time.RFC3339Nano) + "|"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if stamp == r.stamp {
		return r.catalog, status, nil
	}
	var lists []List
	for _, name := range []string{SourceModelsDev, SourceLiteLLM} {
		var list List
		found, err := readState(ctx, r.db, name, &list)
		if err != nil {
			return nil, nil, err
		}
		if found {
			list.Source = name
			lists = append(lists, list)
		}
	}
	r.catalog, r.stamp = Merge(lists...), stamp
	return r.catalog, status, nil
}
