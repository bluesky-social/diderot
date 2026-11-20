// Package markdown is the Markdown folder source: it indexes the Markdown
// files in the one folder the deployment mounts at DIDEROT_MARKDOWN_DIR.
package markdown

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/bluesky-social/diderot/internal/settings"
)

// ConnectorName is the source's connector_settings row and its events'
// source.
const ConnectorName = "markdown"

// DirVariable names the deployment variable that holds the folder's path.
// The folder is never a setting: a path typed into a form would let an
// admin index any directory the process can read.
const DirVariable = "DIDEROT_MARKDOWN_DIR"

// Markdown folder fields.
const (
	FieldRescanInterval = "rescan_interval"
	FieldBaseURL        = "base_url"
)

// minRescanInterval is the shortest rescan interval Validate accepts.
const minRescanInterval = time.Minute

// Connector is the Markdown folder source: its page's fields, their
// validation, and the pre-flight Test; in source.go, its facts, its leader
// worker, and its expansion; in status.go, its page's status.
type Connector struct{}

func (Connector) Name() string { return ConnectorName }

func (Connector) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldRescanInterval, Label: "Rescan every", Kind: settings.KindDuration, Required: true, Default: "5m",
			Help: "How often the folder is read again, such as 5 minutes or 1 hour. At least 1 minute."},
		{Key: FieldBaseURL, Label: "Link documents to", Kind: settings.KindText,
			Help: "A web address the folder is published at. Each document links to it plus the document's path in the folder. Leave empty for no links."},
	}
}

// Validate checks the rescan interval's minimum, and that a link base is an
// http or https URL.
func (Connector) Validate(values settings.Values) error {
	errs := settings.FieldErrors{}
	interval, _ := values[FieldRescanInterval].(string)
	if d, err := time.ParseDuration(interval); err == nil && d < minRescanInterval {
		errs[FieldRescanInterval] = "must be at least 1 minute"
	}
	if base, _ := values[FieldBaseURL].(string); base != "" {
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs[FieldBaseURL] = "must be an http or https URL"
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// Test opens the folder and lists its Markdown files, as a scan would.
func (Connector) Test(ctx context.Context, _ settings.Resolved) settings.TestReport {
	return test(ctx, os.Getenv(DirVariable), maxFiles)
}

func test(ctx context.Context, dir string, limit int) settings.TestReport {
	var report settings.TestReport
	add := func(check settings.Check) { report.Checks = append(report.Checks, check) }
	notChecked := settings.Check{Name: "Markdown files", Detail: "Not checked: the folder can't be read.", Remedy: "Fix the folder, then test again."}
	if dir == "" {
		add(settings.Check{Name: "Folder", Detail: string(errNoFolder),
			Remedy: "Mount a folder into the deployment and set " + DirVariable + " to its path."})
		add(notChecked)
		return report
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		add(settings.Check{Name: "Folder", Detail: "The folder can't be opened: " + err.Error() + ".",
			Remedy: "Check that the folder is mounted and readable by the server."})
		add(notChecked)
		return report
	}
	defer func() { _ = root.Close() }()
	add(settings.Check{Name: "Folder", Passed: true, Detail: "Opened " + dir + "."})

	found, err := list(ctx, root, limit)
	switch {
	case err != nil:
		add(settings.Check{Name: "Markdown files", Detail: describe(err), Remedy: "Fix the folder, then test again."})
	case len(found.paths) == 0:
		add(settings.Check{Name: "Markdown files", Detail: string(errNoFiles),
			Remedy: "Add .md or .markdown files to the folder, or check that the right folder is mounted."})
	case len(found.paths) == 1:
		add(settings.Check{Name: "Markdown files", Passed: true, Detail: "Found 1 Markdown file."})
	default:
		add(settings.Check{Name: "Markdown files", Passed: true, Detail: fmt.Sprintf("Found %d Markdown files.", len(found.paths))})
	}
	return report
}
