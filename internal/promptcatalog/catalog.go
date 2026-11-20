// Package promptcatalog loads, validates, and stores Diderot's model prompts.
// The database holds the active release; the example catalog in prompts/
// seeds a fresh install. Go owns only stable keys and composition.
package promptcatalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/bluesky-social/diderot/prompts"
	"gopkg.in/yaml.v3"
)

const SchemaVersion = 1

const (
	AskBase                           = "ask.base"
	AskAnswerDiscipline               = "ask.answer_discipline"
	AskTerminalOutput                 = "ask.terminal_output"
	AskSurfaceHTTP                    = "ask.surface.http"
	AskSurfaceSlack                   = "ask.surface.slack"
	AskObservabilityDelegation        = "ask.observability.delegation"
	AskObservabilityRole              = "ask.observability.role"
	AskObservabilityDiscipline        = "ask.observability.discipline"
	AskObservabilityReport            = "ask.observability.report"
	AskObservabilitySentryDiscipline  = "ask.observability.sentry_discipline"
	AskObservabilityGrafanaDiscipline = "ask.observability.grafana_discipline"
	AskObservabilityGrafanaReport     = "ask.observability.grafana_report"
	AskObservabilityAlert             = "ask.observability.workflow.alert"
	AskObservabilitySLO               = "ask.observability.workflow.slo"
	AskObservabilityExplore           = "ask.observability.workflow.exploration"
	AskObservabilityIncident          = "ask.observability.workflow.incident"
	AskObservabilitySentry            = "ask.observability.workflow.sentry"
	AskObservabilityPromQL            = "ask.observability.syntax.promql"
	AskObservabilityLogQL             = "ask.observability.syntax.logql"
	AskObservabilityTraceQL           = "ask.observability.syntax.traceql"
	SessionTitle                      = "session.title"
	AskTitle                          = "ask.title"
	ConnectorDocumentExtract          = "connector.document_extract"
	ConnectorImageCaption             = "connector.image_caption"
	ScheduledIndividualDaily          = "scheduled_report.individual_daily"
	ScheduledTeamWeekly               = "scheduled_report.team_weekly"
	SummarySessionMap                 = "summary.session_map"
	SummarySessionReduce              = "summary.session_reduce"

	// AskOrganization is the organization note: what Ask needs to know about
	// one install and no shipped prompt can, such as its internal sites.
	AskOrganization = "ask.organization"
)

// notes are the optional prompts no example ships, so a release without one
// has an empty note, and Ask appends nothing in its place.
var notes = []Prompt{
	{Key: AskOrganization, Path: "notes/ask-organization.md",
		Description: "What Ask should know about your organization that the shipped prompts cannot: internal sites and tools, and where to look for what. Added after Role and research. Empty unless you write it."},
}

// Notes lists the optional prompts no example ships.
func Notes() []Prompt { return append([]Prompt(nil), notes...) }

// IsNote reports whether key is an organization note.
func IsNote(key string) bool {
	for _, note := range notes {
		if note.Key == key {
			return true
		}
	}
	return false
}

var activeBundle atomic.Pointer[Bundle]

func init() {
	bundle := MustEmbedded()
	activeBundle.Store(&bundle)
}

type manifest struct {
	SchemaVersion int          `yaml:"schema_version"`
	Prompts       []definition `yaml:"prompts"`
}

type definition struct {
	Key         string `yaml:"key"`
	Path        string `yaml:"path"`
	Description string `yaml:"description"`
}

// Prompt is one immutable, named model instruction in a release bundle.
type Prompt struct {
	Key         string `json:"key"`
	Path        string `json:"path"`
	Description string `json:"description"`
	Body        string `json:"body"`
	SHA256      string `json:"sha256"`
}

// Bundle is the complete atomic prompt unit promoted through environments.
// Prompts are sorted by key, making hashes and wire payloads deterministic.
type Bundle struct {
	SchemaVersion int      `json:"schema_version"`
	SHA256        string   `json:"sha256"`
	Prompts       []Prompt `json:"prompts"`
}

// EffectiveSHA256 identifies the model-visible contents of a bundle. Source
// paths and descriptions are release metadata: changing either must not create
// a new active prompt interval or invalidate inference caches.
func (b Bundle) EffectiveSHA256() string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "schema:%d\n", b.SchemaVersion)
	for _, prompt := range b.Prompts {
		_, _ = fmt.Fprintf(h, "%s\x00%s\n", prompt.Key, prompt.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// BodySHA256 returns the canonical digest used for prompt bodies. Prompt files
// ignore surrounding whitespace.
func BodySHA256(body string) string {
	return hash(strings.TrimSpace(body))
}

// Embedded loads the example catalog compiled from prompts/.
func Embedded() (Bundle, error) {
	return Load(prompts.FS)
}

// MustEmbedded is for package defaults whose failure means the binary was
// built from an invalid repository. CI tests Embedded directly for diagnostics.
func MustEmbedded() Bundle {
	bundle, err := Embedded()
	if err != nil {
		panic(err)
	}
	return bundle
}

// Load reads manifest.yaml and every referenced prompt from root, and
// validates the result as the release API would.
func Load(root fs.FS) (Bundle, error) {
	raw, err := fs.ReadFile(root, "manifest.yaml")
	if err != nil {
		return Bundle{}, fmt.Errorf("prompt catalog: read manifest: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var spec manifest
	if err := decoder.Decode(&spec); err != nil {
		return Bundle{}, fmt.Errorf("prompt catalog: parse manifest: %w", err)
	}
	if spec.SchemaVersion != SchemaVersion {
		return Bundle{}, fmt.Errorf("prompt catalog: schema version %d is not supported (want %d)", spec.SchemaVersion, SchemaVersion)
	}
	if len(spec.Prompts) == 0 {
		return Bundle{}, errors.New("prompt catalog: manifest has no prompts")
	}

	bundle := Bundle{SchemaVersion: spec.SchemaVersion, Prompts: make([]Prompt, 0, len(spec.Prompts))}
	seenKeys := make(map[string]struct{}, len(spec.Prompts))
	seenPaths := make(map[string]struct{}, len(spec.Prompts))
	for _, item := range spec.Prompts {
		item.Key = strings.TrimSpace(item.Key)
		item.Path = strings.TrimSpace(item.Path)
		item.Description = strings.TrimSpace(item.Description)
		if item.Key == "" || strings.ContainsAny(item.Key, " /\\") {
			return Bundle{}, fmt.Errorf("prompt catalog: invalid key %q", item.Key)
		}
		if _, exists := seenKeys[item.Key]; exists {
			return Bundle{}, fmt.Errorf("prompt catalog: duplicate key %q", item.Key)
		}
		seenKeys[item.Key] = struct{}{}
		clean := path.Clean(item.Path)
		if clean != item.Path || clean == "." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
			return Bundle{}, fmt.Errorf("prompt catalog: invalid path %q for %s", item.Path, item.Key)
		}
		if path.Ext(clean) != ".md" {
			return Bundle{}, fmt.Errorf("prompt catalog: %s must reference a markdown file", item.Key)
		}
		if _, exists := seenPaths[clean]; exists {
			return Bundle{}, fmt.Errorf("prompt catalog: duplicate path %q", clean)
		}
		seenPaths[clean] = struct{}{}
		body, err := fs.ReadFile(root, clean)
		if err != nil {
			return Bundle{}, fmt.Errorf("prompt catalog: read %s: %w", item.Key, err)
		}
		text := strings.TrimSpace(string(body))
		if text == "" {
			return Bundle{}, fmt.Errorf("prompt catalog: %s is empty", item.Key)
		}
		bundle.Prompts = append(bundle.Prompts, Prompt{
			Key: item.Key, Path: clean, Description: item.Description,
			Body: text, SHA256: hash(text),
		})
	}
	if err := fs.WalkDir(root, ".", func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || path.Ext(filePath) != ".md" {
			return nil
		}
		if _, ok := seenPaths[filePath]; !ok {
			return fmt.Errorf("prompt catalog: markdown file %q is not listed in manifest", filePath)
		}
		return nil
	}); err != nil {
		return Bundle{}, err
	}
	sort.Slice(bundle.Prompts, func(i, j int) bool { return bundle.Prompts[i].Key < bundle.Prompts[j].Key })
	bundle.SHA256 = bundleHash(bundle)
	if err := ValidateWire(bundle); err != nil {
		return Bundle{}, err
	}
	if err := ValidatePlaceholders(bundle); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

// ValidateWire verifies an untrusted bundle received by the release API.
func ValidateWire(bundle Bundle) error {
	if bundle.SchemaVersion != SchemaVersion {
		return fmt.Errorf("prompt catalog: schema version %d is not supported", bundle.SchemaVersion)
	}
	if len(bundle.Prompts) == 0 {
		return errors.New("prompt catalog: bundle has no prompts")
	}
	seen := make(map[string]struct{}, len(bundle.Prompts))
	last := ""
	for i, prompt := range bundle.Prompts {
		if strings.TrimSpace(prompt.Key) != prompt.Key || prompt.Key == "" || strings.ContainsAny(prompt.Key, " /\\") {
			return fmt.Errorf("prompt catalog: invalid key %q", prompt.Key)
		}
		if _, ok := seen[prompt.Key]; ok {
			return fmt.Errorf("prompt catalog: duplicate key %q", prompt.Key)
		}
		seen[prompt.Key] = struct{}{}
		if i > 0 && prompt.Key <= last {
			return errors.New("prompt catalog: prompts must be sorted by key")
		}
		last = prompt.Key
		if strings.TrimSpace(prompt.Body) == "" {
			return fmt.Errorf("prompt catalog: %s is empty", prompt.Key)
		}
		if prompt.Body != strings.TrimSpace(prompt.Body) {
			return fmt.Errorf("prompt catalog: %s has outer whitespace", prompt.Key)
		}
		if prompt.SHA256 != hash(prompt.Body) {
			return fmt.Errorf("prompt catalog: %s content hash mismatch", prompt.Key)
		}
	}
	for _, key := range requiredKeys() {
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("prompt catalog: required prompt %s is missing", key)
		}
	}
	if bundle.SHA256 != bundleHash(bundle) {
		return errors.New("prompt catalog: bundle hash mismatch")
	}
	return nil
}

func requiredKeys() []string {
	return []string{
		AskBase, AskAnswerDiscipline, AskTerminalOutput, AskSurfaceHTTP, AskSurfaceSlack,
		AskObservabilityDelegation, AskObservabilityRole,
		AskObservabilityDiscipline, AskObservabilityReport,
		AskObservabilitySentryDiscipline, AskObservabilityGrafanaDiscipline,
		AskObservabilityGrafanaReport,
		AskObservabilityAlert, AskObservabilitySLO, AskObservabilityExplore,
		AskObservabilityIncident, AskObservabilitySentry,
		AskObservabilityPromQL, AskObservabilityLogQL,
		AskObservabilityTraceQL,
		SessionTitle,
		ConnectorDocumentExtract, ConnectorImageCaption,
		ScheduledIndividualDaily, ScheduledTeamWeekly,
		SummarySessionMap, SummarySessionReduce,
	}
}

// Text returns a prompt body by stable key.
func (b Bundle) Text(key string) (string, error) {
	i := sort.Search(len(b.Prompts), func(i int) bool { return b.Prompts[i].Key >= key })
	if i == len(b.Prompts) || b.Prompts[i].Key != key {
		return "", fmt.Errorf("prompt catalog: prompt %s not found", key)
	}
	return b.Prompts[i].Body, nil
}

// WithBody returns a copy of b with key's body replaced (outer whitespace
// trimmed) and its hashes recomputed.
func (b Bundle) WithBody(key, body string) (Bundle, error) {
	i := sort.Search(len(b.Prompts), func(i int) bool { return b.Prompts[i].Key >= key })
	if i == len(b.Prompts) || b.Prompts[i].Key != key {
		return Bundle{}, fmt.Errorf("prompt catalog: prompt %s not found", key)
	}
	out := b
	out.Prompts = append([]Prompt(nil), b.Prompts...)
	out.Prompts[i].Body = strings.TrimSpace(body)
	out.Prompts[i].SHA256 = hash(out.Prompts[i].Body)
	out.SHA256 = bundleHash(out)
	return out, nil
}

// NoteText returns an organization note's text, empty when the release has
// none.
func (b Bundle) NoteText(key string) string {
	text, _ := b.Text(key)
	return text
}

func (b Bundle) MustText(key string) string {
	text, err := b.Text(key)
	if err != nil {
		panic(err)
	}
	return text
}

// SetActive updates the process-wide projection used by non-session model
// calls. Ask sessions use explicit release snapshots instead.
func SetActive(bundle Bundle) error {
	if err := ValidateWire(bundle); err != nil {
		return err
	}
	copyBundle := bundle
	copyBundle.Prompts = append([]Prompt(nil), bundle.Prompts...)
	activeBundle.Store(&copyBundle)
	return nil
}

// Active returns the active release with its variables filled for one
// model request.
func Active() Bundle { return activeBundle.Load().Render(Values()) }

// ActiveText returns one prompt of the active release, rendered for one
// model request.
func ActiveText(key string) string { return Render(activeBundle.Load().MustText(key), Values()) }

func hash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func bundleHash(bundle Bundle) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "schema:%d\n", bundle.SchemaVersion)
	for _, prompt := range bundle.Prompts {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\n", prompt.Key, prompt.Path, prompt.Description, prompt.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}
