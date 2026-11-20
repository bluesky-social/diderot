package scheduledreport

import (
	"fmt"
	"sort"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

var templates = map[string]string{
	"individual-daily": promptcatalog.ScheduledIndividualDaily,
	"team-weekly":      promptcatalog.ScheduledTeamWeekly,
}

// Template returns a built-in prompt by name.
func Template(name string) (string, error) {
	key, ok := templates[name]
	if !ok {
		return "", fmt.Errorf("unknown report template %q", name)
	}
	return promptcatalog.ActiveText(key), nil
}

// TemplateNames returns the templates' stable names.
func TemplateNames() []string {
	out := make([]string, 0, len(templates))
	for name := range templates {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
