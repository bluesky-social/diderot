// Package modelprovider defines the inference providers an administrator
// adds on the Models page. Each is a settings row of its own (a
// settings.Family), saved, tested, turned on and off, and audited like a
// tool connection. Its Test lists the provider's models, which the Models
// page offers each job; the leader lists them again every ten minutes and
// keeps the result under the row.
package modelprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
)

// Prefix starts every provider's settings row name.
const Prefix = "provider:"

// Row is the settings row of the provider name.
func Row(name string) string { return Prefix + name }

// Name is the provider a settings row holds.
func Name(row string) string { return strings.TrimPrefix(row, Prefix) }

// namePattern is a provider's name: it appears in model references, on
// pages and in logs, and in the name of the variable that may set its key.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// CheckName says what is wrong with a new provider's name, or nil.
func CheckName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("use 1 to 40 lowercase letters, digits or _, starting with a letter")
	}
	return nil
}

// Fields of a provider.
const (
	FieldKind    = "kind"
	FieldBaseURL = "base_url"
	FieldKey     = "key"
	FieldModels  = "models"
	// FieldNoKey marks a server that takes no key.
	FieldNoKey = "no_key"
	// FieldHeaders are extra headers every request carries, one
	// "Name: value" each; FieldSecretHeaders are more of them, kept as a
	// credential: one "Name: value" per line, stored encrypted and never
	// shown again.
	FieldHeaders       = "headers"
	FieldSecretHeaders = "secret_headers"
)

// ParseHeaders reads "Name: value" lines into headers. It reports the first
// line that is not one, or that names a header Diderot sets itself, by its
// number and never by its text, which may be a credential.
func ParseHeaders(lines []string) (http.Header, error) {
	headers := http.Header{}
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || value == "" || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("line %d is not a header as Name: value", index+1)
		}
		if err := llmclient.CheckHeaderName(name); err != nil {
			return nil, fmt.Errorf("line %d: %s", index+1, err.Error())
		}
		headers.Set(name, value)
	}
	return headers, nil
}

// FactModel is the key of each listed model's Test fact: its label is the
// model's ID and its value the modelregistry.Listed as JSON.
const FactModel = "model"

// FactNoListing marks a Test of a provider that has no model list.
const FactNoListing = "no_listing"

// CheckKey is the source_state key under a provider's row where the leader
// keeps its latest listing of the saved settings, as for tool connections.
const CheckKey = "check"

// Check is the leader's latest test of a provider's saved settings and the
// settings version it ran on.
type Check struct {
	Version int64               `json:"version"`
	Test    settings.TestRecord `json:"test"`
}

// Definitions makes each provider's settings.Definition.
type Definitions struct {
	// Standalone admits plain http to any host (DIDEROT_DEPLOYMENT=standalone):
	// a local server or another container.
	Standalone bool
	// HTTPClient lists models; nil is the default client.
	HTTPClient *http.Client
}

// Family is the settings family of every provider.
func (d Definitions) Family() settings.Family {
	return settings.Family{Prefix: Prefix, Define: d.Define}
}

// Define returns the definition of the provider whose settings row is row.
func (d Definitions) Define(row string) settings.Definition {
	return definition{row: row, defs: d}
}

type definition struct {
	row  string
	defs Definitions
}

func (d definition) Name() string { return d.row }

// SecretEnv is the variable that may set the provider's key in place of a
// pasted one: DIDEROT_PROVIDER_<NAME>_KEY.
func (d definition) SecretEnv(field string) string { return EnvName(Name(d.row), field) }

// EnvName is the variable that sets a provider's secret field.
func EnvName(name, field string) string {
	return "DIDEROT_PROVIDER_" + strings.ToUpper(name+"_"+field)
}

// ReadEnv reads every provider key variable that is set, from environ
// (os.Environ), for settings.Secrets.Env.
func ReadEnv(environ []string) map[string]string {
	env := map[string]string{}
	for _, entry := range environ {
		name, value, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, "DIDEROT_PROVIDER_") {
			continue
		}
		for _, field := range []string{FieldKey, FieldSecretHeaders} {
			if strings.HasSuffix(name, "_"+strings.ToUpper(field)) {
				if value = strings.TrimSpace(value); value != "" {
					env[name] = value
				}
			}
		}
	}
	return env
}

func kindNames() []string {
	names := make([]string, len(modelregistry.Kinds))
	for i, kind := range modelregistry.Kinds {
		names[i] = string(kind)
	}
	return names
}

func kindTitles() []string {
	titles := make([]string, len(modelregistry.Kinds))
	for i, kind := range modelregistry.Kinds {
		titles[i] = kind.Title()
	}
	return titles
}

func (definition) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldKind, Label: "Kind", Kind: settings.KindEnum, Options: kindNames(), OptionLabels: kindTitles(),
			Default: string(modelregistry.KindOpenAI)},
		{Key: FieldBaseURL, Label: "Base URL", Kind: settings.KindText, Required: true},
		{Key: FieldKey, Label: "API key", Kind: settings.KindSecret},
		{Key: FieldNoKey, Label: "It takes no API key", Kind: settings.KindBool},
		{Key: FieldModels, Label: "Models offered", Kind: settings.KindList},
		{Key: FieldHeaders, Label: "Extra request headers", Kind: settings.KindList},
		{Key: FieldSecretHeaders, Label: "Secret request headers", Kind: settings.KindSecret},
	}
}

// Validate checks the base URL and the offered model IDs.
func (d definition) Validate(values settings.Values) error {
	errs := settings.FieldErrors{}
	if raw, _ := values[FieldBaseURL].(string); raw != "" {
		raw = strings.TrimRight(raw, "/")
		values[FieldBaseURL] = raw
		if err := checkBaseURL(raw, d.defs.Standalone); err != nil {
			errs[FieldBaseURL] = err.Error()
		}
	}
	models, _ := values[FieldModels].([]string)
	for _, id := range models {
		if strings.TrimSpace(id) != id || id == "" {
			errs[FieldModels] = "enter each model ID without spaces around it"
		}
	}
	if headers, _ := values[FieldHeaders].([]string); len(headers) > 0 {
		if _, err := ParseHeaders(headers); err != nil {
			errs[FieldHeaders] = "Enter each header as Name: value: " + err.Error() + "."
		}
	}
	if keyless, _ := values[FieldNoKey].(bool); keyless {
		if kind, _ := values[FieldKind].(string); !modelregistry.Kind(kind).AllowsNoKey() {
			errs[FieldNoKey] = modelregistry.Kind(kind).Title() + " always takes a key."
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// checkBaseURL admits a provider's base URL. The key rides every request,
// so plain http is allowed only to an in-cluster Service, or in a
// standalone deployment, where the provider is often another container or
// a server on the same machine.
func checkBaseURL(raw string, standalone bool) error {
	if err := llmclient.ValidateBaseURL(raw); err != nil {
		return errors.New("enter an http or https address, such as https://api.openai.com/v1, without a user name, query or #")
	}
	u, _ := url.Parse(raw)
	if u.Scheme == "https" || standalone {
		return nil
	}
	host := u.Hostname()
	if addr, err := netip.ParseAddr(host); err == nil && addr.IsLoopback() {
		return errors.New("use https, or a Kubernetes Service address (*.svc.cluster.local)")
	}
	if !strings.HasSuffix(host, ".svc.cluster.local") {
		return errors.New("use https; plain http is allowed only for a Kubernetes Service (*.svc.cluster.local)")
	}
	return nil
}

// Settings is a provider's saved settings, ready to call. It prints and
// logs without its key.
type Settings struct {
	Provider modelregistry.Provider
	// Offered are the model IDs Models offered names; empty offers every
	// listed model.
	Offered []string
	// HeadersError is why the extra request headers can't be read; none are
	// sent then.
	HeadersError error
}

// FromResolved reads a provider's settings.
func FromResolved(r settings.Resolved) Settings {
	kind := modelregistry.Kind(r.String(FieldKind))
	if !slices.Contains(modelregistry.Kinds, kind) {
		kind = modelregistry.KindOpenAI
	}
	s := Settings{
		Provider: modelregistry.Provider{
			Name: Name(r.Connector), Kind: kind, Version: r.Version,
			Endpoint: llmclient.Endpoint{
				BaseURL: strings.TrimRight(strings.TrimSpace(r.String(FieldBaseURL)), "/"),
				APIKey:  r.Secret(FieldKey), Auth: kind.Auth(),
			},
			Keyless: r.Bool(FieldNoKey) && kind.AllowsNoKey(),
		},
		Offered: r.List(FieldModels),
	}
	// A server that takes no key is sent none, whatever is saved.
	if s.Provider.Keyless {
		s.Provider.Endpoint.APIKey = ""
	}
	// The secret headers win over the plain ones of the same name. Lines
	// that are not headers were refused when they were saved, or are
	// reported by Test; a request never carries half of one.
	if s.Provider.Endpoint.Headers, s.HeadersError = requestHeaders(r); s.HeadersError != nil {
		s.Provider.Fault = "has request headers that can't be read"
	}
	return s
}

// requestHeaders is the provider's extra request headers, plain and secret.
func requestHeaders(r settings.Resolved) (http.Header, error) {
	headers, err := ParseHeaders(r.List(FieldHeaders))
	if err != nil {
		return nil, fmt.Errorf("extra request headers: %w", err)
	}
	secret, err := ParseHeaders(strings.Split(r.Secret(FieldSecretHeaders), "\n"))
	if err != nil {
		return nil, fmt.Errorf("secret request headers: %w", err)
	}
	for name, values := range secret {
		headers[name] = values
	}
	if len(headers) == 0 {
		return nil, nil
	}
	return headers, nil
}

// Format keeps the key out of every fmt verb.
func (s Settings) Format(f fmt.State, verb rune) { s.Provider.Format(f, verb) }

// Test lists the provider's models with the settings, as the leader's check
// and the Models page do.
func (d definition) Test(ctx context.Context, r settings.Resolved) settings.TestReport {
	s := FromResolved(r)
	var report settings.TestReport
	if s.Provider.Endpoint.BaseURL == "" {
		report.Checks = append(report.Checks, settings.Check{
			Name: "List models", Detail: "There is nothing to connect to yet.", Remedy: "Enter the base URL, then test again.",
		})
		return report
	}
	if s.HeadersError != nil {
		report.Checks = append(report.Checks, settings.Check{
			Name: "Request headers", Detail: "Diderot can't read the " + s.HeadersError.Error() + ".",
			Remedy: "Enter each header on a line of its own, as Name: value.",
		})
		return report
	}
	if s.Provider.Endpoint.APIKey == "" && !s.Provider.Keyless {
		remedy := r.SupplySecret(FieldKey, "the provider's API key") + "."
		if s.Provider.Kind.AllowsNoKey() {
			remedy += " If the server takes none, tick It takes no API key."
		}
		report.Checks = append(report.Checks, settings.Check{Name: "List models", Detail: "There is no API key.", Remedy: remedy})
		return report
	}
	listed, err := modelregistry.List(ctx, d.defs.HTTPClient, s.Provider)
	switch {
	case errors.Is(err, modelregistry.ErrNoListing) && len(s.Offered) == 0:
		report.Checks = append(report.Checks, settings.Check{
			Name: "List models", Detail: "The provider has no model list, so Diderot can't find its models.",
			Remedy: "Enter the IDs of the models it offers under Models offered, then save.",
		})
		return report
	case errors.Is(err, modelregistry.ErrNoListing):
		report.Facts = append(report.Facts, settings.Fact{Key: FactNoListing, Label: "No model list", Value: "true"})
		report.Checks = append(report.Checks, settings.Check{
			Name: "List models", Passed: true,
			Detail: "The provider has no model list; it offers the " + count(len(s.Offered), "model") + " entered under Models offered. A job's test checks each one.",
		})
		return report
	case err != nil:
		report.Checks = append(report.Checks, settings.Check{
			Name: "List models", Detail: "Diderot could not list the provider's models: " + err.Error() + ".",
			Remedy: "Check the base URL (it includes any /v1), and " + lowerFirst(r.SupplySecret(FieldKey, "an API key the provider accepts")) + ".",
		})
		return report
	}
	for _, model := range listed {
		value, _ := json.Marshal(model)
		report.Facts = append(report.Facts, settings.Fact{Key: FactModel, Label: model.ID, Value: string(value), ID: model.ID})
	}
	detail := "The provider lists " + count(len(listed), "model") + "."
	if len(s.Offered) > 0 {
		var unlisted []string
		for _, id := range s.Offered {
			if !slices.ContainsFunc(listed, func(l modelregistry.Listed) bool { return l.ID == id }) {
				unlisted = append(unlisted, id)
			}
		}
		detail += " " + count(len(s.Offered), "model") + " offered"
		if len(unlisted) > 0 {
			detail += "; the list does not have " + strings.Join(unlisted, ", ") + ", which a job's test will check"
		}
		detail += "."
	}
	report.Checks = append(report.Checks, settings.Check{Name: "List models", Passed: true, Detail: detail})
	return report
}

// Menu is what a provider offers the Models page's jobs: its listing,
// narrowed or extended by Models offered.
type Menu struct {
	Name    string
	Kind    modelregistry.Kind
	Enabled bool
	// Models is the menu, in ID order. An offered ID the listing lacks has
	// only its ID.
	Models []modelregistry.Listed
	// Offered are the IDs Models offered names.
	Offered []string
	// Listing is the provider's whole listing, when it has one and the last
	// test read it.
	Listing []modelregistry.Listed
	// NoListing is set when the provider has no model list.
	NoListing bool
	// Error says why the latest listing failed; empty when it passed.
	Error string
	// At is when the listing the menu comes from was read; zero when it
	// never was.
	At time.Time
}

// Listed returns the menu's entry for id.
func (m Menu) Listed(id string) (modelregistry.Listed, bool) {
	index := slices.IndexFunc(m.Models, func(l modelregistry.Listed) bool { return l.ID == id })
	if index < 0 {
		return modelregistry.Listed{}, false
	}
	return m.Models[index], true
}

// InListing reports whether the latest listing had id; false without one.
func (m Menu) InListing(id string) bool {
	return slices.ContainsFunc(m.Listing, func(l modelregistry.Listed) bool { return l.ID == id })
}

// MenuOf is a provider's menu from its saved view and the leader's latest
// check, whichever of their tests is newer on the saved version.
func MenuOf(view settings.View, check *Check) Menu {
	menu := Menu{Name: Name(view.Name), Enabled: view.Enabled}
	kind, _ := view.Settings[FieldKind].(string)
	menu.Kind = modelregistry.Kind(kind)
	menu.Offered, _ = view.Settings[FieldModels].([]string)
	record := view.LastTest
	if check != nil && check.Version == view.Version && (record == nil || check.Test.At.After(record.At)) {
		record = &check.Test
	}
	if record != nil {
		menu.At = record.At
		for _, fact := range record.Facts {
			switch fact.Key {
			case FactModel:
				var listed modelregistry.Listed
				if json.Unmarshal([]byte(fact.Value), &listed) == nil && listed.ID != "" {
					menu.Listing = append(menu.Listing, listed)
				}
			case FactNoListing:
				menu.NoListing = true
			}
		}
		if !record.Passed {
			for _, c := range record.Checks {
				if !c.Passed {
					menu.Error = c.Detail
					break
				}
			}
		}
	}
	if len(menu.Offered) == 0 {
		menu.Models = menu.Listing
		return menu
	}
	for _, id := range menu.Offered {
		listed, ok := Menu{Models: menu.Listing}.Listed(id)
		if !ok {
			listed = modelregistry.Listed{ID: id}
		}
		menu.Models = append(menu.Models, listed)
	}
	slices.SortFunc(menu.Models, func(a, b modelregistry.Listed) int { return strings.Compare(a.ID, b.ID) })
	return menu
}

// Querier reads source_state (*pgxpool.Pool).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ReadChecks reads the leader's latest check of every provider, by row.
func ReadChecks(ctx context.Context, db Querier) (map[string]Check, error) {
	rows, err := db.Query(ctx, `
		SELECT source, value FROM source_state
		 WHERE starts_with(source, $1) AND key = $2`, Prefix, CheckKey)
	if err != nil {
		return nil, fmt.Errorf("provider checks: %w", err)
	}
	checks := map[string]Check{}
	var row string
	var raw []byte
	_, err = pgx.ForEachRow(rows, []any{&row, &raw}, func() error {
		var check Check
		if json.Unmarshal(raw, &check) == nil {
			checks[row] = check
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("provider checks: %w", err)
	}
	return checks, nil
}

// Lister lists the saved providers (*settings.Store).
type Lister interface {
	ListFamily(ctx context.Context, prefix string) ([]settings.View, error)
}

// ReadMenus reads every provider's menu, in name order.
func ReadMenus(ctx context.Context, store Lister, db Querier) ([]Menu, error) {
	views, err := store.ListFamily(ctx, Prefix)
	if err != nil {
		return nil, err
	}
	checks, err := ReadChecks(ctx, db)
	if err != nil {
		return nil, err
	}
	menus := make([]Menu, 0, len(views))
	for _, view := range views {
		var check *Check
		if c, ok := checks[view.Name]; ok {
			check = &c
		}
		menus = append(menus, MenuOf(view, check))
	}
	return menus, nil
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func lowerFirst(text string) string {
	if text == "" {
		return text
	}
	return strings.ToLower(text[:1]) + text[1:]
}
