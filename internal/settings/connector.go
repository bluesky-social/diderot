package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

// FieldKind is how a field is entered, checked, and stored.
type FieldKind string

const (
	KindText     FieldKind = "text"
	KindBool     FieldKind = "bool"
	KindEnum     FieldKind = "enum"
	KindList     FieldKind = "list"
	KindInt      FieldKind = "int"
	KindDuration FieldKind = "duration"
	KindSecret   FieldKind = "secret"
)

// Field is one setting on a connector's page. Fields drive the generated
// form, input parsing, and prompt variables.
type Field struct {
	Key, Label, Help string
	Kind             FieldKind
	Required         bool
	// Default is the value of an unset field: a string for text, enum and
	// duration, a bool, a []string, or an int64. Nil is the kind's zero.
	Default any
	// Options are an enum's choices, and OptionLabels what a page calls
	// each, in the same order; without labels a page shows the options.
	Options      []string
	OptionLabels []string
	// SlackChannel marks a text field that holds a Slack channel's ID. A
	// page takes the channel's name, ID or link for it and shows its name.
	SlackChannel bool
	// Discovered fields are read-only facts Test finds at the source, such
	// as Slack's workspace name. A save whose test passes keeps them.
	Discovered bool
	// PromptVar exposes the field as a {{NAME}} prompt variable. Never on a
	// secret.
	PromptVar string
}

// MaxTextRunes bounds every text and list value.
const MaxTextRunes = 500

// Definition is a connector as every replica knows it: its fields, their
// validation, and its pre-flight test.
type Definition interface {
	// Name is the connector_settings key, such as "slack".
	Name() string
	Fields() []Field
	// Validate checks parsed non-secret values beyond their kinds, and may
	// normalize them in place: values is what gets saved and tested. Return
	// FieldErrors to mark fields on the form.
	Validate(Values) error
	// Test makes one bounded, read-only pass against the source with s,
	// which may be an unsaved draft. It reports structured checks and never
	// echoes a source response.
	Test(ctx context.Context, s Resolved) TestReport
}

// Connector is a Definition with the leader's workers. Run holds them until
// ctx ends; the supervisor cancels and restarts it when the settings change.
type Connector interface {
	Definition
	Run(ctx context.Context, s Resolved) error
}

// FieldErrors maps field keys to messages for the form. It wraps
// ErrInvalid.
type FieldErrors map[string]string

func (e FieldErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, key := range slices.Sorted(maps.Keys(e)) {
		parts = append(parts, key+": "+e[key])
	}
	return ErrInvalid.Error() + ": " + strings.Join(parts, "; ")
}

func (e FieldErrors) Unwrap() error { return ErrInvalid }

// Values holds a connector's non-secret settings, one canonical Go value per
// field (see Field.Default).
type Values map[string]any

// Registry is every connector this binary knows.
type Registry struct {
	defs []Definition
}

var fieldKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// NewRegistry checks and registers definitions. A definition that breaks a
// rule panics: these are programming errors. In particular a secret field
// may never be a prompt variable.
func NewRegistry(defs ...Definition) *Registry {
	names := map[string]bool{OrgConnector: true}
	vars := map[string]bool{}
	for _, def := range defs {
		name := def.Name()
		if !fieldKey.MatchString(name) || names[name] {
			panic(fmt.Sprintf("settings: connector name %q is invalid or registered twice", name))
		}
		names[name] = true
		keys := map[string]bool{}
		for _, field := range def.Fields() {
			where := name + "." + field.Key
			if !fieldKey.MatchString(field.Key) || keys[field.Key] {
				panic(fmt.Sprintf("settings: field %s is invalid or repeated", where))
			}
			keys[field.Key] = true
			if field.Kind == KindSecret && field.PromptVar != "" {
				panic(fmt.Sprintf("settings: secret field %s may not be a prompt variable", where))
			}
			if field.Kind == KindSecret && (field.Discovered || field.Default != nil) {
				panic(fmt.Sprintf("settings: secret field %s can't be discovered or have a default", where))
			}
			if field.PromptVar != "" {
				if field.Kind != KindText || vars[field.PromptVar] ||
					!slices.Contains(promptcatalog.VariableNames(), field.PromptVar) {
					panic(fmt.Sprintf("settings: field %s: prompt variable %q must be a text field, unique, and listed in promptcatalog", where, field.PromptVar))
				}
				vars[field.PromptVar] = true
			}
			if len(field.OptionLabels) != 0 && len(field.OptionLabels) != len(field.Options) {
				panic(fmt.Sprintf("settings: field %s has %d option labels for %d options", where, len(field.OptionLabels), len(field.Options)))
			}
			if (field.Kind == KindEnum) != (len(field.Options) > 0) {
				panic(fmt.Sprintf("settings: field %s: only an enum has options, and it needs them", where))
			}
			if _, err := parseField(field, field.Default); field.Kind != KindSecret && err != nil {
				panic(fmt.Sprintf("settings: field %s: default: %v", where, err))
			}
		}
	}
	return &Registry{defs: defs}
}

// Definitions lists the registered connectors in registration order.
func (r *Registry) Definitions() []Definition {
	if r == nil {
		return nil
	}
	return slices.Clone(r.defs)
}

// Get returns the named connector.
func (r *Registry) Get(name string) (Definition, bool) {
	if r == nil {
		return nil, false
	}
	for _, def := range r.defs {
		if def.Name() == name {
			return def, true
		}
	}
	return nil, false
}

// Resolved is one connector's settings ready to use: every non-secret field
// with defaults filled in, and every secret's value. It prints and logs
// without its secrets.
type Resolved struct {
	Connector string
	Version   int64
	Enabled   bool
	values    Values
	secrets   map[string]string
	// envs names, by secret field, the deployment variable that sets it.
	envs map[string]string
}

// NewResolved builds settings directly, for tests and fakes.
func NewResolved(connector string, values Values, secrets map[string]string) Resolved {
	return Resolved{Connector: connector, values: maps.Clone(values), secrets: maps.Clone(secrets)}
}

// String returns a text field's value.
func (r Resolved) String(key string) string { s, _ := r.values[key].(string); return s }

// Bool returns a bool field's value.
func (r Resolved) Bool(key string) bool { b, _ := r.values[key].(bool); return b }

// List returns a list field's values.
func (r Resolved) List(key string) []string { l, _ := r.values[key].([]string); return slices.Clone(l) }

// Int returns an int field's value.
func (r Resolved) Int(key string) int64 { n, _ := r.values[key].(int64); return n }

// Duration returns a duration field's value.
func (r Resolved) Duration(key string) time.Duration {
	d, _ := time.ParseDuration(r.String(key))
	return d
}

// Secret returns a secret field's value, empty when unset.
func (r Resolved) Secret(key string) string { return r.secrets[key] }

// WithSecretEnv returns r with the secret field key set by the deployment's
// variable env, for tests and fakes.
func (r Resolved) WithSecretEnv(key, env string) Resolved {
	r.envs = maps.Clone(r.envs)
	if r.envs == nil {
		r.envs = map[string]string{}
	}
	r.envs[key] = env
	return r
}

// SupplySecret is a test remedy's instruction for giving the secret field key
// a working value, what: paste it on the connector's page, or, when the
// deployment's variable sets the field, change that variable. It has no
// final punctuation, so the remedy can go on.
func (r Resolved) SupplySecret(key, what string) string {
	if env := r.envs[key]; env != "" {
		return "Set " + env + " in the deployment to " + what
	}
	return "Paste " + what
}

// Values returns a copy of the non-secret values.
func (r Resolved) Values() Values { return maps.Clone(r.values) }

// Format keeps secrets out of every fmt verb.
func (r Resolved) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "settings.Resolved{%s v%d}", r.Connector, r.Version)
}

// LogValue keeps secrets out of logs.
func (r Resolved) LogValue() slog.Value {
	return slog.GroupValue(slog.String("connector", r.Connector), slog.Int64("version", r.Version))
}

// TestReport is what Test found: structured checks and discovered facts,
// never a raw source response.
type TestReport struct {
	Checks []Check `json:"checks"`
	Facts  []Fact  `json:"facts,omitempty"`
}

// Passed reports whether the report has checks and every one passed.
func (r TestReport) Passed() bool {
	if len(r.Checks) == 0 {
		return false
	}
	for _, check := range r.Checks {
		if !check.Passed {
			return false
		}
	}
	return true
}

// Fact returns the value of the fact key, if the report has it.
func (r TestReport) Fact(key string) (string, bool) {
	for _, fact := range r.Facts {
		if fact.Key == key {
			return fact.Value, true
		}
	}
	return "", false
}

// Check is one test step. Remedy says what to fix when it failed.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
	Remedy string `json:"remedy,omitempty"`
}

// Fact is something Test discovered. A Key that names a Discovered field
// fills that field on save.
type Fact struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Value string `json:"value"`
	// ID is the identifier of what Value names, when it has one.
	ID string `json:"id,omitempty"`
}

// TestRecord is a test's report, its outcome, and who ran it when.
type TestRecord struct {
	TestReport
	Passed bool      `json:"passed"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// parseField turns raw input (a form string or a JSON value) into the
// field's canonical value. Nil and "" are the kind's zero value.
func parseField(field Field, raw any) (any, error) {
	if s, ok := raw.(string); ok && field.Kind != KindText && field.Kind != KindList {
		raw = strings.TrimSpace(s)
		if raw == "" {
			raw = nil
		}
	}
	switch field.Kind {
	case KindText:
		s, ok := raw.(string)
		if raw != nil && !ok {
			return nil, errors.New("must be text")
		}
		return ValidateText(field.Key, s, MaxTextRunes)
	case KindBool:
		switch v := raw.(type) {
		case nil:
			return false, nil
		case bool:
			return v, nil
		case string:
			if v == "on" {
				return true, nil
			}
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, errors.New("must be true or false")
			}
			return b, nil
		}
		return nil, errors.New("must be true or false")
	case KindEnum:
		s, ok := raw.(string)
		if raw == nil {
			return "", nil
		}
		if !ok || !slices.Contains(field.Options, s) {
			return nil, fmt.Errorf("must be one of %s", strings.Join(field.Options, ", "))
		}
		return s, nil
	case KindList:
		return parseList(field, raw)
	case KindInt:
		switch v := raw.(type) {
		case nil:
			return int64(0), nil
		case int64:
			return v, nil
		case int:
			return int64(v), nil
		case float64:
			if v != float64(int64(v)) {
				return nil, errors.New("must be a whole number")
			}
			return int64(v), nil
		case string:
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, errors.New("must be a whole number")
			}
			return n, nil
		}
		return nil, errors.New("must be a whole number")
	case KindDuration:
		s, ok := raw.(string)
		if raw == nil {
			return "", nil
		}
		if !ok {
			return nil, errDuration
		}
		d, err := ParseDuration(s)
		if err != nil {
			return nil, err
		}
		return d.String(), nil
	case KindSecret:
		return nil, errors.New("is a secret")
	}
	return nil, fmt.Errorf("has unknown kind %q", field.Kind)
}

// parseList accepts a []string, a JSON array of strings, or text with one
// item per line or comma.
func parseList(field Field, raw any) ([]string, error) {
	var items []string
	switch v := raw.(type) {
	case nil:
	case []string:
		items = v
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, errors.New("must be a list of text")
			}
			items = append(items, s)
		}
	case string:
		items = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	default:
		return nil, errors.New("must be a list of text")
	}
	out := []string{}
	for _, item := range items {
		item, err := ValidateText(field.Key, item, MaxTextRunes)
		if err != nil {
			return nil, err
		}
		if item != "" && !slices.Contains(out, item) {
			out = append(out, item)
		}
	}
	return out, nil
}

// parseValues parses input over stored, the saved values with defaults
// filled in: a field left out of input keeps its stored value. Discovered
// fields are never input.
func parseValues(def Definition, input map[string]any, stored Values) (Values, error) {
	values := Values{}
	errs := FieldErrors{}
	for _, field := range def.Fields() {
		if field.Kind == KindSecret {
			continue
		}
		value := stored[field.Key]
		if raw, ok := input[field.Key]; ok && !field.Discovered {
			parsed, err := parseField(field, raw)
			if err != nil {
				errs[field.Key] = strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")
				continue
			}
			value = parsed
		}
		if field.Required && isZero(value) {
			errs[field.Key] = "is required"
			continue
		}
		values[field.Key] = value
	}
	if len(errs) > 0 {
		return nil, errs
	}
	if err := def.Validate(values); err != nil {
		return nil, err
	}
	return values, nil
}

// normalizeValues reads stored values: each field's value in canonical form,
// or its default when missing or unreadable.
func normalizeValues(def Definition, stored map[string]any) Values {
	values := Values{}
	for _, field := range def.Fields() {
		if field.Kind == KindSecret {
			continue
		}
		value, err := parseField(field, stored[field.Key])
		if _, ok := stored[field.Key]; !ok || err != nil {
			value = defaultValue(field)
		}
		values[field.Key] = value
	}
	return values
}

func defaultValue(field Field) any {
	value, _ := parseField(field, field.Default)
	return value
}

func isZero(value any) bool {
	switch v := value.(type) {
	case string:
		return v == ""
	case []string:
		return len(v) == 0
	}
	return false
}
