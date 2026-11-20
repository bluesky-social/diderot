package promptcatalog

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// Prompt variables are {{NAME}} placeholders filled on every model request.
// The list is fixed here; a release that uses any other name is rejected.
// Connector variables are connector fields (settings.Field.PromptVar): the
// registry refuses a PromptVar missing here, and while the connector is
// disabled its variable renders the field's default.
const (
	VarOrgName        = "ORG_NAME"
	VarOrgDescription = "ORG_DESCRIPTION"
	VarMaxTitleRunes  = "MAX_TITLE_RUNES"
	// VarSources lists the sources a build indexes, one line each, from
	// the source registry.
	VarSources = "SOURCES"

	VarSlackWorkspaceName = "SLACK_WORKSPACE_NAME"
	// VarGrafanaDatasources says which Grafana datasources the observability
	// tools are pinned to, one line, empty when none is.
	VarGrafanaDatasources = "GRAFANA_DATASOURCES"
)

var variableNames = []string{VarOrgName, VarOrgDescription, VarMaxTitleRunes, VarSources, VarSlackWorkspaceName, VarGrafanaDatasources}

// VariableNames lists every variable a prompt may use.
func VariableNames() []string { return slices.Clone(variableNames) }

// MaxTitleRunes is the title length {{MAX_TITLE_RUNES}} asks for and the
// title tools enforce.
const MaxTitleRunes = 72

// DefaultOrgName fills {{ORG_NAME}} while the organization has no name, so
// prompts written as "what {{ORG_NAME}} knows" still read as a sentence.
const DefaultOrgName = "the organization"

// placeholderPattern also matches near-misses such as {{ ORG_NAME }} or
// {{org_name}}, so validation rejects them instead of sending them verbatim.
var placeholderPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_-]+)\s*\}\}`)

var variableSource atomic.Pointer[func() map[string]string]

// SetVariables installs the source of the organization's and connectors'
// variable values. It is called on every model request, so it must be cheap.
// Unset or nil, every such value is empty.
func SetVariables(source func() map[string]string) {
	variableSource.Store(&source)
}

// Values returns every variable's value for one model request.
func Values() map[string]string {
	var provided map[string]string
	if source := variableSource.Load(); source != nil && *source != nil {
		provided = (*source)()
	}
	orgName := strings.TrimSpace(provided[VarOrgName])
	if orgName == "" {
		orgName = DefaultOrgName
	}
	return map[string]string{
		VarOrgName:            orgName,
		VarOrgDescription:     strings.TrimSpace(provided[VarOrgDescription]),
		VarMaxTitleRunes:      strconv.Itoa(MaxTitleRunes),
		VarSources:            strings.TrimSpace(provided[VarSources]),
		VarSlackWorkspaceName: strings.TrimSpace(provided[VarSlackWorkspaceName]),
		VarGrafanaDatasources: strings.TrimSpace(provided[VarGrafanaDatasources]),
	}
}

// Render replaces each {{NAME}} whose NAME is in values, in one pass: a
// substituted value is never scanned again, and other text is left as is.
func Render(text string, values map[string]string) string {
	pairs := make([]string, 0, 2*len(values))
	for name, value := range values {
		pairs = append(pairs, "{{"+name+"}}", value)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// Render returns a copy of b with every prompt rendered and its hashes
// recomputed, so SHA256 identifies the text models receive.
func (b Bundle) Render(values map[string]string) Bundle {
	out := b
	out.Prompts = make([]Prompt, len(b.Prompts))
	for i, prompt := range b.Prompts {
		prompt.Body = Render(prompt.Body, values)
		prompt.SHA256 = hash(prompt.Body)
		out.Prompts[i] = prompt
	}
	out.SHA256 = bundleHash(out)
	return out
}

// ValidatePlaceholders rejects any placeholder that is not exactly {{NAME}}
// for a known variable. New releases must pass it; stored ones are not
// rechecked, so history written before a rename stays readable.
func ValidatePlaceholders(bundle Bundle) error {
	for _, prompt := range bundle.Prompts {
		if err := ValidateText(prompt.Key, prompt.Body); err != nil {
			return err
		}
	}
	return nil
}

// ValidateText applies ValidatePlaceholders to one prompt body.
func ValidateText(key, body string) error {
	for _, match := range placeholderPattern.FindAllStringSubmatch(body, -1) {
		if match[0] != "{{"+match[1]+"}}" || !slices.Contains(variableNames, match[1]) {
			return fmt.Errorf("prompt catalog: %s uses unknown variable %s (known: {{%s}})",
				key, match[0], strings.Join(variableNames, "}}, {{"))
		}
	}
	return nil
}
