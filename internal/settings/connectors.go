package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestTimeout bounds one connector Test.
const TestTimeout = 30 * time.Second

// ErrUnknownConnector names a connector the registry doesn't have.
var ErrUnknownConnector = errors.New("settings: unknown connector")

// Row is one connector's stored settings. It never holds a plaintext
// secret.
type Row struct {
	Connector string
	Enabled   bool
	Settings  map[string]any
	Secrets   map[string]SecretState
	Version   int64
	LastTest  *TestRecord
	UpdatedBy string
	UpdatedAt time.Time
}

// Draft is unsaved form input for Test: fields given override the saved
// ones, and secrets left out keep their saved value.
type Draft struct {
	Settings map[string]any         `json:"settings"`
	Secrets  map[string]SecretInput `json:"secrets"`
}

// View is a connector's page or API representation. Secrets show where
// their value comes from, never the value.
type View struct {
	Name     string      `json:"name"`
	Enabled  bool        `json:"enabled"`
	Version  int64       `json:"version"`
	Fields   []FieldView `json:"fields"`
	Settings Values      `json:"settings"`
	// Secrets has an entry for each secret field that is set.
	Secrets map[string]SecretView `json:"secrets"`
	// CanStoreSecrets reports that a secret can be pasted and saved: the
	// install has DIDEROT_CONFIG_KEYS.
	CanStoreSecrets bool `json:"can_store_secrets"`
	// LastTest is the test the last save ran on what it saved.
	LastTest  *TestRecord `json:"last_test,omitempty"`
	UpdatedBy string      `json:"updated_by"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// FieldView describes one field for forms.
type FieldView struct {
	Key        string    `json:"key"`
	Label      string    `json:"label"`
	Help       string    `json:"help,omitempty"`
	Kind       FieldKind `json:"kind"`
	Required   bool      `json:"required,omitempty"`
	Discovered bool      `json:"discovered,omitempty"`
	Options    []string  `json:"options,omitempty"`
	// OptionLabels and SlackChannel are the Field's.
	OptionLabels []string `json:"option_labels,omitempty"`
	SlackChannel bool     `json:"slack_channel,omitempty"`
	PromptVar    string   `json:"prompt_var,omitempty"`
	// Env names a secret field's deployment variable, which sets the field
	// in place of a pasted value whenever it is set.
	Env string `json:"env,omitempty"`
}

// SecretView is where a secret field's value comes from.
type SecretView struct {
	// Env names the deployment variable that sets the value. It wins over a
	// saved one.
	Env string `json:"env,omitempty"`
	// Saved reports a pasted value stored encrypted.
	Saved bool `json:"saved,omitempty"`
}

func (s *Store) definition(name string) (Definition, error) {
	if def, ok := s.registry.Get(name); ok {
		return def, nil
	}
	if family := s.familyOf(name); family != nil {
		return family.Define(name), nil
	}
	return nil, fmt.Errorf("%w %q", ErrUnknownConnector, name)
}

// readRow reads one connector row; lock is empty or a locking clause.
func readRow(ctx context.Context, q queryRower, name, lock string) (Row, error) {
	row := Row{Connector: name}
	var settings, secrets, lastTest []byte
	err := q.QueryRow(ctx, `
		SELECT enabled, settings, secrets, version, last_test, updated_by, updated_at
		  FROM connector_settings WHERE connector = $1 `+lock, name).
		Scan(&row.Enabled, &settings, &secrets, &row.Version, &lastTest, &row.UpdatedBy, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, fmt.Errorf("%w %q: its row is missing", ErrUnknownConnector, name)
	}
	if err != nil {
		return Row{}, fmt.Errorf("settings: read %s: %w", name, err)
	}
	if err := json.Unmarshal(settings, &row.Settings); err != nil {
		return Row{}, fmt.Errorf("settings: decode %s settings: %w", name, err)
	}
	if err := json.Unmarshal(secrets, &row.Secrets); err != nil {
		return Row{}, fmt.Errorf("settings: decode %s secrets: %w", name, err)
	}
	if lastTest != nil {
		if err := json.Unmarshal(lastTest, &row.LastTest); err != nil {
			return Row{}, fmt.Errorf("settings: decode %s last test: %w", name, err)
		}
	}
	return row, nil
}

// resolve reads row's settings with every secret's value.
func (s *Store) resolve(def Definition, row Row) (Resolved, error) {
	stored, pasted, err := s.applySecrets(def, row, nil, Actor{})
	if err != nil {
		return Resolved{}, err
	}
	secrets, err := s.secretValues(def, stored, pasted)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{
		Connector: row.Connector, Version: row.Version, Enabled: row.Enabled,
		values: normalizeValues(def, row.Settings), secrets: secrets, envs: s.secretEnvs(def),
	}, nil
}

// applySecrets applies inputs to row's saved secrets and returns the ones to
// store. A pasted value is sealed with the config keys, and also returned in
// pasted so a test needn't open it again. A field its deployment variable
// sets takes no pasted value.
func (s *Store) applySecrets(def Definition, row Row, inputs map[string]SecretInput, actor Actor) (map[string]SecretState, map[string]string, error) {
	stored := map[string]SecretState{}
	pasted := map[string]string{}
	errs := FieldErrors{}
	now := s.now().UTC()
	for _, field := range def.Fields() {
		if field.Kind != KindSecret {
			continue
		}
		key, env := field.Key, SecretEnv(def, field.Key)
		if state, ok := row.Secrets[key]; ok {
			stored[key] = state
		}
		input, changed := inputs[key]
		value := strings.TrimSpace(input.Value)
		switch {
		case !changed:
		case input.Clear && value != "":
			errs[key] = "give a value or clear it, not both"
			continue
		case input.Clear:
			delete(stored, key)
		case s.secrets.Env[env] != "":
			errs[key] = "is set by " + env + "; change it there"
			continue
		case value == "":
			errs[key] = "enter a value"
			continue
		case s.secrets.Keys == nil:
			errs[key] = "can't be saved without DIDEROT_CONFIG_KEYS; set it, or set " + env
			continue
		default:
			ciphertext, err := s.secrets.Keys.Seal(value, secretAAD(def.Name(), key))
			if err != nil {
				return nil, nil, fmt.Errorf("settings: encrypt %s.%s: %w", def.Name(), key, err)
			}
			stored[key] = SecretState{Ciphertext: ciphertext, Fingerprint: Fingerprint(value), SetBy: actor.Email, SetAt: now}
			pasted[key] = value
		}
		if _, ok := stored[key]; field.Required && !ok && s.secrets.Env[env] == "" {
			errs[key] = "is required"
		}
	}
	if len(errs) > 0 {
		return nil, nil, errs
	}
	return stored, pasted, nil
}

// secretEnvs names, by secret field, the deployment variable that sets it,
// for each one a variable sets.
func (s *Store) secretEnvs(def Definition) map[string]string {
	envs := map[string]string{}
	for _, field := range def.Fields() {
		if field.Kind == KindSecret && s.secrets.env(def, field.Key) != "" {
			envs[field.Key] = SecretEnv(def, field.Key)
		}
	}
	return envs
}

// secretValues reads every set secret field's value: its deployment
// variable, or else the stored value, opened with the config keys unless
// pasted holds it.
func (s *Store) secretValues(def Definition, stored map[string]SecretState, pasted map[string]string) (map[string]string, error) {
	values := map[string]string{}
	errs := FieldErrors{}
	for _, field := range def.Fields() {
		if field.Kind != KindSecret {
			continue
		}
		key := field.Key
		if value := s.secrets.env(def, key); value != "" {
			values[key] = value
			continue
		}
		if value, ok := pasted[key]; ok {
			values[key] = value
			continue
		}
		state, saved := stored[key]
		switch {
		case !saved:
		case s.secrets.Keys == nil:
			errs[key] = "the saved value can't be read without DIDEROT_CONFIG_KEYS"
		default:
			value, err := s.secrets.Keys.Open(state.Ciphertext, secretAAD(def.Name(), key))
			if err != nil {
				errs[key] = err.Error()
				continue
			}
			values[key] = value
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return values, nil
}

// takeDiscovered fills discovered fields from a passing test of values.
func takeDiscovered(def Definition, values Values, test TestRecord) error {
	if !test.Passed {
		return nil
	}
	for _, field := range def.Fields() {
		fact, ok := test.Fact(field.Key)
		if !field.Discovered || !ok {
			continue
		}
		value, err := ValidateText(field.Key, fact, MaxTextRunes)
		if err != nil {
			return err
		}
		values[field.Key] = value
	}
	return nil
}

func (s *Store) view(def Definition, row Row) View {
	values := normalizeValues(def, row.Settings)
	view := View{
		Name: def.Name(), Enabled: row.Enabled, Version: row.Version, Settings: values,
		Secrets: map[string]SecretView{}, CanStoreSecrets: s.secrets.Keys != nil, LastTest: row.LastTest,
		UpdatedBy: row.UpdatedBy, UpdatedAt: row.UpdatedAt,
	}
	for _, field := range def.Fields() {
		fieldView := FieldView{
			Key: field.Key, Label: field.Label, Help: field.Help, Kind: field.Kind, Required: field.Required,
			Discovered: field.Discovered, Options: field.Options, OptionLabels: field.OptionLabels,
			SlackChannel: field.SlackChannel, PromptVar: field.PromptVar,
		}
		if field.Kind == KindSecret {
			fieldView.Env = SecretEnv(def, field.Key)
		}
		view.Fields = append(view.Fields, fieldView)
		if field.Kind != KindSecret {
			continue
		}
		var secret SecretView
		if s.secrets.env(def, field.Key) != "" {
			secret.Env = SecretEnv(def, field.Key)
		}
		_, secret.Saved = row.Secrets[field.Key]
		if secret != (SecretView{}) {
			view.Secrets[field.Key] = secret
		}
	}
	return view
}

// View reads one connector from the database (not the cache, so a page
// shows the version it posts back).
func (s *Store) View(ctx context.Context, name string) (View, error) {
	def, err := s.definition(name)
	if err != nil {
		return View{}, err
	}
	row, err := readRow(ctx, s.pool, name, "")
	if err != nil {
		return View{}, err
	}
	return s.view(def, row), nil
}

// List reads every registered connector.
func (s *Store) List(ctx context.Context) ([]View, error) {
	views := []View{}
	for _, def := range s.registry.Definitions() {
		view, err := s.View(ctx, def.Name())
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// Save writes a connector's settings at version and tests what it wrote.
// Fields left out of settings keep their saved value, and secrets left out
// keep theirs; a pasted secret is encrypted before it is stored. The test
// becomes the row's last test, and a passing one fills the discovered
// fields. With enable, a passing test also turns the connector on. A failing
// test turns nothing off and still saves the settings, so nothing pasted is
// lost; invalid input saves and tests nothing. The audit entry holds old and
// new values for non-secret fields, only "changed" for secrets, and the
// test's outcome.
func (s *Store) Save(ctx context.Context, actor Actor, name string, version int64, settings map[string]any, secrets map[string]SecretInput, enable bool) (View, error) {
	def, err := s.definition(name)
	if err != nil {
		return View{}, err
	}
	if actor, err = normalizeActor(actor); err != nil {
		return View{}, err
	}
	row, err := readRow(ctx, s.pool, name, "")
	if err != nil {
		return View{}, err
	}
	// A stale form fails before the test; write checks the version again.
	if row.Version != version {
		return View{}, ErrConflict
	}
	values, err := parseValues(def, settings, normalizeValues(def, row.Settings))
	if err != nil {
		return View{}, err
	}
	stored, pasted, err := s.applySecrets(def, row, secrets, actor)
	if err != nil {
		return View{}, err
	}
	test, err := s.test(ctx, def, row, values, stored, pasted, actor)
	if err != nil {
		return View{}, err
	}
	if err := takeDiscovered(def, values, test); err != nil {
		return View{}, err
	}
	action := "connectors." + name + ".save"
	if enable {
		action = "connectors." + name + ".enable"
	}
	return s.write(ctx, actor, def, version, action, func(row *Row) {
		row.Settings, row.Secrets, row.LastTest = values, stored, &test
		row.Enabled = row.Enabled || (enable && test.Passed)
	})
}

// Disable turns a connector off. Its settings and ingested data stay.
func (s *Store) Disable(ctx context.Context, actor Actor, name string, version int64) (View, error) {
	def, err := s.definition(name)
	if err != nil {
		return View{}, err
	}
	if actor, err = normalizeActor(actor); err != nil {
		return View{}, err
	}
	return s.write(ctx, actor, def, version, "connectors."+name+".disable", func(row *Row) {
		row.Enabled = false
	})
}

func normalizeActor(actor Actor) (Actor, error) {
	actor.Email = strings.ToLower(strings.TrimSpace(actor.Email))
	if actor.Email == "" {
		return Actor{}, errors.New("settings: an actor is required")
	}
	return actor, nil
}

// write applies change to the row at version, bumps the version, and audits
// the change in one transaction. actor is normalized.
func (s *Store) write(ctx context.Context, actor Actor, def Definition, version int64, action string, change func(*Row)) (View, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return View{}, fmt.Errorf("settings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := readRow(ctx, tx, def.Name(), "FOR UPDATE")
	if err != nil {
		return View{}, err
	}
	if current.Version != version {
		return View{}, ErrConflict
	}
	next := current
	next.Settings, next.Secrets = maps.Clone(current.Settings), maps.Clone(current.Secrets)
	change(&next)
	settings, err := json.Marshal(next.Settings)
	if err != nil {
		return View{}, fmt.Errorf("settings: encode %s: %w", def.Name(), err)
	}
	secrets, err := json.Marshal(next.Secrets)
	if err != nil {
		return View{}, fmt.Errorf("settings: encode %s secrets: %w", def.Name(), err)
	}
	var lastTest []byte
	if next.LastTest != nil {
		if lastTest, err = json.Marshal(next.LastTest); err != nil {
			return View{}, fmt.Errorf("settings: encode %s test: %w", def.Name(), err)
		}
	}
	next.UpdatedBy = actor.Email
	err = tx.QueryRow(ctx, `
		UPDATE connector_settings
		   SET enabled = $2, settings = $3, secrets = $4, last_test = $5, version = version + 1,
		       updated_by = $6, updated_at = now()
		 WHERE connector = $1
		RETURNING version, updated_at`, def.Name(), next.Enabled, settings, secrets, lastTest, actor.Email).
		Scan(&next.Version, &next.UpdatedAt)
	if err != nil {
		return View{}, fmt.Errorf("settings: update %s: %w", def.Name(), err)
	}
	changedSecrets := map[string]string{}
	for _, field := range def.Fields() {
		if field.Kind == KindSecret && current.Secrets[field.Key] != next.Secrets[field.Key] {
			changedSecrets[field.Key] = "changed"
		}
	}
	details := map[string]any{
		"version": next.Version,
		"changes": changedFields(normalizeValues(def, current.Settings), normalizeValues(def, next.Settings)),
	}
	if len(changedSecrets) > 0 {
		details["secrets"] = changedSecrets
	}
	if current.Enabled != next.Enabled {
		details["enabled"] = next.Enabled
	}
	if next.LastTest != current.LastTest {
		details["test_passed"] = next.LastTest.Passed
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return View{}, fmt.Errorf("settings: encode audit: %w", err)
	}
	if err := audit(ctx, tx, actor, action, def.Name(), encoded); err != nil {
		return View{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return View{}, fmt.Errorf("settings: commit: %w", err)
	}
	s.storeConnector(def, next)
	return s.view(def, next), nil
}

// Test runs the connector's Test against draft, or the saved settings when
// draft is nil. It writes nothing: only a save keeps its test.
func (s *Store) Test(ctx context.Context, actor Actor, name string, draft *Draft) (TestRecord, error) {
	def, err := s.definition(name)
	if err != nil {
		return TestRecord{}, err
	}
	row, err := readRow(ctx, s.pool, name, "")
	if err != nil {
		return TestRecord{}, err
	}
	values := normalizeValues(def, row.Settings)
	inputs := map[string]SecretInput{}
	if draft != nil {
		if values, err = parseValues(def, draft.Settings, values); err != nil {
			return TestRecord{}, err
		}
		inputs = draft.Secrets
	}
	stored, pasted, err := s.applySecrets(def, row, inputs, Actor{})
	if err != nil {
		return TestRecord{}, err
	}
	return s.test(ctx, def, row, values, stored, pasted, actor)
}

// test runs def's Test, within TestTimeout, on values and the secrets that
// stored and pasted resolve to, and scrubs every secret value from the
// report.
func (s *Store) test(ctx context.Context, def Definition, row Row, values Values, stored map[string]SecretState, pasted map[string]string, actor Actor) (TestRecord, error) {
	secrets, err := s.secretValues(def, stored, pasted)
	if err != nil {
		return TestRecord{}, err
	}
	resolved := Resolved{
		Connector: def.Name(), Version: row.Version, Enabled: row.Enabled,
		values: values, secrets: secrets, envs: s.secretEnvs(def),
	}
	testCtx, cancel := context.WithTimeout(ctx, TestTimeout)
	report := def.Test(testCtx, resolved)
	cancel()
	record := TestRecord{
		TestReport: scrubReport(report, secrets),
		By:         strings.ToLower(strings.TrimSpace(actor.Email)), At: s.now().UTC(),
	}
	record.Passed = record.TestReport.Passed()
	return record, nil
}

// scrubReport replaces any secret value a connector let into its report.
func scrubReport(report TestReport, secrets map[string]string) TestReport {
	scrub := func(text string) string {
		for _, value := range secrets {
			if value != "" {
				text = strings.ReplaceAll(text, value, "[secret]")
			}
		}
		return text
	}
	out := TestReport{}
	for _, check := range report.Checks {
		check.Name, check.Detail, check.Remedy = scrub(check.Name), scrub(check.Detail), scrub(check.Remedy)
		out.Checks = append(out.Checks, check)
	}
	for _, fact := range report.Facts {
		fact.Label, fact.Value = scrub(fact.Label), scrub(fact.Value)
		out.Facts = append(out.Facts, fact)
	}
	return out
}
