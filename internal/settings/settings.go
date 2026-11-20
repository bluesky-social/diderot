// Package settings stores the connector_settings rows administrators edit in
// the browser and caches them on every
// replica for request paths, which must not query the database per request.
//
// The `org` row holds the organization's name and description and the
// install's setup state, and the
// `models` row the model each inference role uses and Ask's limits, and the
// `session_summaries` row when agent sessions are summarized. Every
// other row is a registered connector's settings and secrets, or one of a
// Family's rows, which administrators add and remove. Every write bumps the
// row's version, is checked against the version the writer loaded, and is
// recorded in admin_audit_log in the same transaction. On the leader a
// Supervisor runs each enabled connector and restarts it when its version
// changes.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/modelregistry"
)

// OrgConnector is the pseudo-connector row that holds organization-wide
// settings and the setup state.
const OrgConnector = "org"

// ModelsConnector is the pseudo-connector row that holds the model each
// inference role uses and Ask's limits, edited on the Models page.
const ModelsConnector = "models"

// Ask's tool-turn limit: the default, and the most the Models page accepts.
const (
	DefaultAskMaxToolTurns = 8
	MaxAskToolTurns        = 50
)

// Install states.
const (
	// StateSetup admits only administrators to Ask.
	StateSetup = "setup"
	// StateLive opens Ask to everyone the deployment's auth admits.
	StateLive = "live"
)

// SetupNotice is what every Ask surface answers while the install is in setup
// and the caller may not use Ask.
const SetupNotice = "Diderot is still being set up. Ask will open once an administrator finishes setup."

// PollInterval is how often every replica checks for rows another replica
// changed.
const PollInterval = 15 * time.Second

var (
	// ErrConflict means the row changed since the writer loaded it.
	ErrConflict = errors.New("settings: changed since loaded; reload and retry")
	// ErrInvalid wraps a value a write refused.
	ErrInvalid = errors.New("settings: invalid value")
)

// Org is the org row's settings document. ORG_NAME and ORG_DESCRIPTION,
// named as the prompt variables they become, and the dashboard link are the
// editable fields; every other field is written only by its own setup action.
type Org struct {
	Name        string `json:"ORG_NAME"`
	Description string `json:"ORG_DESCRIPTION"`
	// DashboardURL is the operational dashboard every admin page links to,
	// empty for none.
	DashboardURL string `json:"dashboard_url,omitempty"`

	State      string     `json:"state"`
	WentLiveBy string     `json:"went_live_by,omitempty"`
	WentLiveAt *time.Time `json:"went_live_at,omitempty"`

	BootstrapAdminEnabled bool `json:"bootstrap_admin_enabled"`

	PromptsExampleAcceptedBy string     `json:"prompts_example_accepted_by,omitempty"`
	PromptsExampleAcceptedAt *time.Time `json:"prompts_example_accepted_at,omitempty"`
	AccessAcknowledgedBy     string     `json:"access_acknowledged_by,omitempty"`
	AccessAcknowledgedAt     *time.Time `json:"access_acknowledged_at,omitempty"`

	ModelsTest *ModelsTest `json:"models_test,omitempty"`
}

// Live reports whether Ask is open to everyone.
func (o Org) Live() bool { return o.State == StateLive }

// ModelsTest is the result of the last "Test models" run.
type ModelsTest struct {
	Passed bool       `json:"passed"`
	By     string     `json:"by"`
	At     time.Time  `json:"at"`
	Roles  []RoleTest `json:"roles"`
}

// RoleTest is one role's outcome. Error is a short classification (an HTTP
// status, a timeout), never a provider response or a credential.
type RoleTest struct {
	Role   string `json:"role"`
	Model  string `json:"model"`
	Passed bool   `json:"passed"`
	Error  string `json:"error,omitempty"`
}

// OrgRow is the org row with its concurrency token.
type OrgRow struct {
	Org
	Version   int64     `json:"version"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Models is the models row's settings document. Roles maps a role (a
// modelregistry.Role) to the modelregistry.Ref of the model chosen for it; a
// role with no entry is off. Available holds an entry for every model a job
// has been given, with what its tests found and an administrator's
// corrections. An empty effort is the model's default, and zero turns is
// DefaultAskMaxToolTurns.
type Models struct {
	Roles     map[string]string     `json:"roles,omitempty"`
	Available []modelregistry.Model `json:"available,omitempty"`
	// Embedding is the embedding model and its vector collection; nil
	// until an administrator chooses one, and search is off until then.
	Embedding *Embedding `json:"embedding,omitempty"`
	// Filling is an embedding model chosen to replace Embedding, or as the
	// first while passages already have vectors elsewhere. The leader's
	// embedder embeds every passage into its collection; search keeps using
	// Embedding meanwhile, and the leader makes Filling the Embedding once
	// its collection holds every passage.
	Filling            *Embedding `json:"filling,omitempty"`
	AskReasoningEffort string     `json:"ask_reasoning_effort,omitempty"`
	AskMaxToolTurns    int        `json:"ask_max_tool_turns,omitempty"`
	// Ask's other limits (AskLimits). A zero value, or one outside the
	// range the Models page accepts, is the limit's default; an empty tier
	// is the provider's own.
	AskServiceTier          string   `json:"ask_service_tier,omitempty"`
	AskMaxOutputTokens      int      `json:"ask_max_output_tokens,omitempty"`
	AskModelTimeout         Duration `json:"ask_model_timeout,omitempty"`
	AskTurnDeadline         Duration `json:"ask_turn_deadline,omitempty"`
	AskInvestigatorDeadline Duration `json:"ask_investigator_deadline,omitempty"`
	AskInvestigatorGrace    Duration `json:"ask_investigator_grace,omitempty"`
	// PublishedFactsOff stops Diderot reading the published model lists
	// (models.dev and LiteLLM's), for a server with no way out to the
	// internet. Off, nothing kept from them is used either: facts come from
	// each provider's own list and each job's test.
	PublishedFactsOff bool `json:"published_facts_off,omitempty"`
}

// Embedding is the embedding model search and the embedder use, and the
// vector collection its vectors live in, which is named after the model and
// its dimension unless an existing collection was adopted.
type Embedding struct {
	// Provider and Model name the model (a modelregistry.Ref's parts).
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Dimensions is the vector size one embedding call returned when the
	// model was chosen.
	Dimensions int    `json:"dimensions"`
	Collection string `json:"collection"`
}

// Ref is the embedding model's modelregistry.Ref.
func (e Embedding) Ref() string { return modelregistry.Ref(e.Provider, e.Model) }

// MaxToolTurns is Ask's tool-turn limit: the saved value while it is in
// range, otherwise the default.
func (m Models) MaxToolTurns() int {
	if m.AskMaxToolTurns < 1 || m.AskMaxToolTurns > MaxAskToolTurns {
		return DefaultAskMaxToolTurns
	}
	return m.AskMaxToolTurns
}

// ModelsRow is the models row with its concurrency token.
type ModelsRow struct {
	Models
	Version   int64     `json:"version"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// document is a pseudo-connector row's settings with its concurrency token.
type document[T any] struct {
	doc       T
	version   int64
	updatedBy string
	updatedAt time.Time
}

func orgRow(d document[Org]) OrgRow {
	return OrgRow{Org: d.doc, Version: d.version, UpdatedBy: d.updatedBy, UpdatedAt: d.updatedAt}
}

func modelsRow(d document[Models]) ModelsRow {
	return ModelsRow{Models: d.doc, Version: d.version, UpdatedBy: d.updatedBy, UpdatedAt: d.updatedAt}
}

// Actor is who makes a change, for updated_by and the audit log.
type Actor struct {
	Email     string
	RequestID string
}

// ValidateText checks one editable single-line value: no line breaks, no
// template delimiters (values are substituted into prompts), and bounded.
func ValidateText(field, value string, maxRunes int) (string, error) {
	value = strings.TrimSpace(value)
	switch {
	case strings.ContainsAny(value, "\r\n"):
		return "", fmt.Errorf("%w: %s must be a single line", ErrInvalid, field)
	case strings.Contains(value, "{{") || strings.Contains(value, "}}"):
		return "", fmt.Errorf("%w: %s must not contain {{ or }}", ErrInvalid, field)
	case len([]rune(value)) > maxRunes:
		return "", fmt.Errorf("%w: %s must be at most %d characters", ErrInvalid, field, maxRunes)
	}
	return value, nil
}

// Store reads and writes connector_settings and caches every row.
type Store struct {
	pool     *pgxpool.Pool
	logger   *slog.Logger
	registry *Registry
	families []*Family
	secrets  Secrets
	now      func() time.Time

	mu         sync.RWMutex
	org        OrgRow
	models     ModelsRow
	connectors map[string]cachedConnector

	sessionSummaries SessionSummariesRow
}

// cachedConnector is one connector row as of its version, with its settings
// resolved when it is enabled. err is why resolution failed.
type cachedConnector struct {
	row      Row
	resolved Resolved
	err      error
}

// NewStore returns a store with an empty cache; call Load before serving.
func NewStore(pool *pgxpool.Pool, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{pool: pool, logger: logger, now: time.Now, connectors: map[string]cachedConnector{}}
}

// WithConnectors adds the registered connectors and the way to read their
// secrets. Call it before Load; without it the store serves the org row
// only.
func (s *Store) WithConnectors(registry *Registry, secrets Secrets) *Store {
	s.registry, s.secrets = registry, secrets
	return s
}

// Registry returns the registered connectors.
func (s *Store) Registry() *Registry { return s.registry }

// systemActor prefixes the updated_by of a row only Diderot itself wrote: the
// org row a migration created (diderot:migration), and the rows Load creates
// (diderot:registry).
const systemActor = "diderot:"

// registryActor is who Load records as creating a row.
const registryActor = systemActor + "registry"

// ChangedBy is who last changed a row, as pages show it:
// updatedBy, or empty while only Diderot itself has written the row.
func ChangedBy(updatedBy string) string {
	if strings.HasPrefix(updatedBy, systemActor) {
		return ""
	}
	return updatedBy
}

// Load creates the models and session_summaries rows, empty (every value on
// its default), and a row for every registered connector that has none,
// then fills the cache. The org row is created by migration, so its absence
// is an error.
func (s *Store) Load(ctx context.Context) error {
	names := []string{ModelsConnector, SessionSummariesConnector}
	for _, def := range s.registry.Definitions() {
		names = append(names, def.Name())
	}
	for _, name := range names {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO connector_settings (connector, settings, secrets, version, updated_by, updated_at)
			VALUES ($1, '{}', '{}', 1, $2, now())
			ON CONFLICT (connector) DO NOTHING`, name, registryActor); err != nil {
			return fmt.Errorf("settings: create the %s row: %w", name, err)
		}
	}
	return s.refresh(ctx, true)
}

// Run polls every row's version until ctx ends and reloads rows another
// replica changed. Version pairs, not a maximum, are compared, so a commit
// that lands out of order is never skipped.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.refresh(ctx, false); err != nil && ctx.Err() == nil {
				s.logger.Warn("settings refresh failed; serving cached values", "err", err)
			}
		}
	}
}

func (s *Store) refresh(ctx context.Context, all bool) error {
	rows, err := s.pool.Query(ctx, `SELECT connector, version FROM connector_settings`)
	if err != nil {
		return fmt.Errorf("settings: list versions: %w", err)
	}
	versions := map[string]int64{}
	var connector string
	var version int64
	_, err = pgx.ForEachRow(rows, []any{&connector, &version}, func() error {
		versions[connector] = version
		return nil
	})
	if err != nil {
		return fmt.Errorf("settings: list versions: %w", err)
	}
	latest, ok := versions[OrgConnector]
	if !ok {
		return errors.New("settings: the org row is missing")
	}
	if all || s.Org().Version != latest {
		org, err := readOrg(ctx, s.pool, "")
		if err != nil {
			return err
		}
		s.storeOrg(org)
	}
	latest, ok = versions[ModelsConnector]
	if !ok {
		return errors.New("settings: the models row is missing")
	}
	if all || s.Models().Version != latest {
		models, err := readDocument[Models](ctx, s.pool, ModelsConnector, "")
		if err != nil {
			return err
		}
		s.storeModels(modelsRow(models))
	}
	latest, ok = versions[SessionSummariesConnector]
	if !ok {
		return errors.New("settings: the session_summaries row is missing")
	}
	if all || s.SessionSummaries().Version != latest {
		summaries, err := readDocument[SessionSummaries](ctx, s.pool, SessionSummariesConnector, "")
		if err != nil {
			return err
		}
		s.storeSessionSummaries(sessionSummariesRow(summaries))
	}
	for _, def := range s.registry.Definitions() {
		version, ok := versions[def.Name()]
		if !ok {
			continue
		}
		if entry, ok := s.cached(def.Name()); ok && !all && entry.row.Version == version {
			continue
		}
		row, err := readRow(ctx, s.pool, def.Name(), "")
		if err != nil {
			return err
		}
		s.storeConnector(def, row)
	}
	return s.refreshFamily(ctx, versions, all)
}

// storeOrg caches row unless the cache already holds a newer version.
func (s *Store) storeOrg(row OrgRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row.Version >= s.org.Version {
		s.org = row
	}
}

// storeModels caches row unless the cache already holds a newer version.
func (s *Store) storeModels(row ModelsRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row.Version >= s.models.Version {
		s.models = row
	}
}

// storeConnector caches row, resolving it when enabled, unless the cache
// already holds a newer version.
func (s *Store) storeConnector(def Definition, row Row) {
	entry := cachedConnector{row: row}
	if row.Enabled {
		entry.resolved, entry.err = s.resolve(def, row)
		if entry.err != nil {
			s.logger.Error("connector settings can't be resolved; request paths treat the connector as off",
				"connector", row.Connector, "version", row.Version, "err", entry.err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.connectors[row.Connector]; !ok || row.Version >= current.row.Version {
		s.connectors[row.Connector] = entry
	}
}

func (s *Store) cached(name string) (cachedConnector, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.connectors[name]
	return entry, ok
}

// Connector returns the named connector's resolved settings from the cache
// while it is enabled and its secrets could be read. Request paths call it
// per request.
func (s *Store) Connector(name string) (Resolved, bool) {
	entry, ok := s.cached(name)
	if !ok || !entry.row.Enabled || entry.err != nil {
		return Resolved{}, false
	}
	return entry.resolved, true
}

// PromptValues returns every connector prompt variable's value: the cached
// setting of an enabled connector, otherwise the field's default.
func (s *Store) PromptValues() map[string]string {
	values := map[string]string{}
	for _, def := range s.registry.Definitions() {
		resolved, enabled := s.Connector(def.Name())
		for _, field := range def.Fields() {
			if field.PromptVar == "" {
				continue
			}
			value, _ := defaultValue(field).(string)
			if enabled {
				value = resolved.String(field.Key)
			}
			values[field.PromptVar] = value
		}
	}
	return values
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// readDocument reads a pseudo-connector row; lock is empty or a locking
// clause.
func readDocument[T any](ctx context.Context, q queryRower, connector, lock string) (document[T], error) {
	var row document[T]
	var raw []byte
	err := q.QueryRow(ctx, `
		SELECT settings, version, updated_by, updated_at
		  FROM connector_settings WHERE connector = $1 `+lock, connector).
		Scan(&raw, &row.version, &row.updatedBy, &row.updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return document[T]{}, fmt.Errorf("settings: the %s row is missing", connector)
	}
	if err != nil {
		return document[T]{}, fmt.Errorf("settings: read %s: %w", connector, err)
	}
	if err := json.Unmarshal(raw, &row.doc); err != nil {
		return document[T]{}, fmt.Errorf("settings: decode %s: %w", connector, err)
	}
	return row, nil
}

// readOrg reads the org row; lock is empty or a locking clause.
func readOrg(ctx context.Context, q queryRower, lock string) (OrgRow, error) {
	row, err := readDocument[Org](ctx, q, OrgConnector, lock)
	return orgRow(row), err
}

// Org returns the cached org row.
func (s *Store) Org() OrgRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.org
}

// OrgName returns the cached organization name, empty when none is saved.
func (s *Store) OrgName() string {
	return s.Org().Name
}

// AskAllowed is the Ask gate every surface applies at its entry: Ask is open
// to everyone once the install is live, and only to administrators before.
// Surfaces that cannot tell an administrator (Slack, scheduled work) pass
// false.
func (s *Store) AskAllowed(admin bool) bool {
	return admin || s.Org().Live()
}

// ReadOrg reads the org row from the database, bypassing the cache.
func (s *Store) ReadOrg(ctx context.Context) (OrgRow, error) {
	return readOrg(ctx, s.pool, "")
}

// BootstrapAdminEnabled reads the bootstrap admin switch from the database,
// not the cache, so turning it off ends its sessions on every replica at
// once.
func (s *Store) BootstrapAdminEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE((settings->>'bootstrap_admin_enabled')::boolean, false)
		  FROM connector_settings WHERE connector = $1`, OrgConnector).Scan(&enabled)
	if err != nil {
		return false, fmt.Errorf("settings: read bootstrap admin: %w", err)
	}
	return enabled, nil
}

// UpdateOrg applies change to the org row when its version is still
// version, then bumps the version and records the change. It returns
// ErrConflict when another write landed first, and change's own error
// unchanged.
func (s *Store) UpdateOrg(ctx context.Context, version int64, actor Actor, action string, change func(*Org) error) (OrgRow, error) {
	row, err := updateDocument(ctx, s.pool, OrgConnector, &version, actor, action, change)
	if err != nil {
		return OrgRow{}, err
	}
	s.storeOrg(orgRow(row))
	return orgRow(row), nil
}

// ForceUpdateOrg is UpdateOrg without the version check, for break-glass
// commands run on the server.
func (s *Store) ForceUpdateOrg(ctx context.Context, actor Actor, action string, change func(*Org) error) (OrgRow, error) {
	row, err := updateDocument(ctx, s.pool, OrgConnector, nil, actor, action, change)
	if err != nil {
		return OrgRow{}, err
	}
	s.storeOrg(orgRow(row))
	return orgRow(row), nil
}

// Models returns the cached models row.
func (s *Store) Models() ModelsRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.models
}

// ReadModels reads the models row from the database, bypassing the cache.
func (s *Store) ReadModels(ctx context.Context) (ModelsRow, error) {
	row, err := readDocument[Models](ctx, s.pool, ModelsConnector, "")
	return modelsRow(row), err
}

// UpdateModels is UpdateOrg for the models row.
func (s *Store) UpdateModels(ctx context.Context, version int64, actor Actor, action string, change func(*Models) error) (ModelsRow, error) {
	row, err := updateDocument(ctx, s.pool, ModelsConnector, &version, actor, action, change)
	if err != nil {
		return ModelsRow{}, err
	}
	s.storeModels(modelsRow(row))
	return modelsRow(row), nil
}

// updateDocument applies change to a pseudo-connector row, when version is
// nil or still its version, bumps the version, and records the change in
// one transaction.
func updateDocument[T any](ctx context.Context, pool *pgxpool.Pool, connector string, version *int64, actor Actor, action string, change func(*T) error) (document[T], error) {
	actor.Email = strings.ToLower(strings.TrimSpace(actor.Email))
	if actor.Email == "" {
		return document[T]{}, errors.New("settings: an actor is required")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return document[T]{}, fmt.Errorf("settings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := readDocument[T](ctx, tx, connector, "FOR UPDATE")
	if err != nil {
		return document[T]{}, err
	}
	if version != nil && current.version != *version {
		return document[T]{}, ErrConflict
	}
	// Change a deep copy, so the audit compares against the document as read.
	encoded, err := json.Marshal(current.doc)
	if err != nil {
		return document[T]{}, fmt.Errorf("settings: encode %s: %w", connector, err)
	}
	next := document[T]{updatedBy: actor.Email}
	if err := json.Unmarshal(encoded, &next.doc); err != nil {
		return document[T]{}, fmt.Errorf("settings: decode %s: %w", connector, err)
	}
	if err := change(&next.doc); err != nil {
		return document[T]{}, err
	}
	if encoded, err = json.Marshal(next.doc); err != nil {
		return document[T]{}, fmt.Errorf("settings: encode %s: %w", connector, err)
	}
	err = tx.QueryRow(ctx, `
		UPDATE connector_settings
		   SET settings = $2, version = version + 1, updated_by = $3, updated_at = now()
		 WHERE connector = $1
		RETURNING version, updated_at`, connector, encoded, actor.Email).
		Scan(&next.version, &next.updatedAt)
	if err != nil {
		return document[T]{}, fmt.Errorf("settings: update %s: %w", connector, err)
	}
	details, err := json.Marshal(map[string]any{
		"version": next.version, "changes": changedFields(current.doc, next.doc),
	})
	if err != nil {
		return document[T]{}, fmt.Errorf("settings: encode audit: %w", err)
	}
	if err := audit(ctx, tx, actor, action, connector, details); err != nil {
		return document[T]{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return document[T]{}, fmt.Errorf("settings: commit: %w", err)
	}
	return next, nil
}

// audit records one succeeded change to a connector_settings row, in the
// writer's transaction.
func audit(ctx context.Context, tx pgx.Tx, actor Actor, action, connector string, details []byte) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_audit_log
		       (actor_email, action, target, request_id, status, details, finished_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), 'succeeded', $5, now())`,
		actor.Email, action, "connector_settings:"+connector,
		strings.TrimSpace(actor.RequestID), details); err != nil {
		return fmt.Errorf("settings: audit: %w", err)
	}
	return nil
}

// changedFields maps each key whose value changed to its old and new value.
// Callers pass only non-secret values.
func changedFields(before, after any) map[string]map[string]any {
	oldFields, newFields := fieldMap(before), fieldMap(after)
	changes := map[string]map[string]any{}
	for key, value := range newFields {
		if old, ok := oldFields[key]; !ok || !reflect.DeepEqual(old, value) {
			changes[key] = map[string]any{"old": oldFields[key], "new": value}
		}
	}
	for key, old := range oldFields {
		if _, ok := newFields[key]; !ok {
			changes[key] = map[string]any{"old": old, "new": nil}
		}
	}
	return changes
}

func fieldMap(v any) map[string]any {
	encoded, _ := json.Marshal(v)
	var fields map[string]any
	_ = json.Unmarshal(encoded, &fields)
	return fields
}
