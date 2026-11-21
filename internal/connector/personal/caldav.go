package personal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/safehttp"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

const (
	caldavName          = "caldav"
	fieldCalendarURL    = "calendar_url"
	fieldJournalURL     = "journal_url"
	maxCalendarResponse = 32 << 20
)

// CalDAV indexes events and journal entries from explicitly chosen collections.
type CalDAV struct{}

func (CalDAV) Name() string { return caldavName }

func (CalDAV) Info() source.Info {
	return source.Info{
		EventSource: caldavName, Title: "CalDAV calendar and notes", ResultNoun: "Calendar or journal entry",
		Summary:         "Calendar events and journal notes from private CalDAV collections.",
		ToolDescription: "Calendar events and journal notes from an owner's CalDAV collections.",
	}
}

func (CalDAV) Fields() []settings.Field {
	return append(accountFields(),
		settings.Field{Key: fieldCalendarURL, Label: "Calendar collection URL", Kind: settings.KindText,
			Help: "The full HTTPS collection URL for events. Leave empty for notes only."},
		settings.Field{Key: fieldJournalURL, Label: "Notes collection URL", Kind: settings.KindText,
			Help: "The full HTTPS collection URL for CalDAV journal notes (VJOURNAL). It can be the same URL as the calendar."})
}

func (CalDAV) Validate(values settings.Values) error {
	errs := accountErrors(values)
	calendar, journal := settingsString(values, fieldCalendarURL), settingsString(values, fieldJournalURL)
	if calendar == "" && journal == "" {
		errs[fieldCalendarURL] = "enter at least one collection URL"
	}
	for field, raw := range map[string]string{fieldCalendarURL: calendar, fieldJournalURL: journal} {
		if raw != "" {
			if _, err := collectionURL(raw); err != nil {
				errs[field] = err.Error()
			}
		}
	}
	if len(errs) != 0 {
		return errs
	}
	return nil
}

func collectionURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || !strings.HasSuffix(u.Path, "/") {
		return nil, errors.New("enter an HTTPS collection URL ending in / without credentials, a query, or a fragment")
	}
	return u, nil
}

type collection struct {
	endpoint, component, label string
}

func collections(cfg settings.Resolved) []collection {
	var result []collection
	for _, col := range []collection{
		{cfg.String(fieldCalendarURL), ical.CompEvent, "Calendar events"},
		{cfg.String(fieldJournalURL), ical.CompJournal, "Journal notes"},
	} {
		if col.endpoint != "" {
			result = append(result, col)
		}
	}
	return result
}

func (CalDAV) Test(ctx context.Context, cfg settings.Resolved) settings.TestReport {
	var report settings.TestReport
	for _, col := range collections(cfg) {
		check := settings.Check{Name: col.label, Detail: "Could not read the collection over HTTPS.",
			Remedy: "Check the collection URL, credentials, TLS certificate, and supported entry types."}
		objects, err := newDAVClient(cfg).query(ctx, col)
		if err == nil {
			check = settings.Check{Name: col.label, Passed: true,
				Detail: fmt.Sprintf("Read %d objects from the collection.", len(objects))}
		}
		report.Checks = append(report.Checks, check)
	}
	return report
}

func (CalDAV) Run(ctx context.Context, deps source.Deps, cfg settings.Resolved) error {
	dav := newDAVClient(cfg)
	return run(ctx, deps, caldavName, cfg, func(ctx context.Context) error {
		return syncCollections(ctx, deps.Sink, dav, cfg)
	})
}

func (CalDAV) Expand(ctx context.Context, deps source.Deps, id int64, _ []string) (source.Expanded, error) {
	return expand(ctx, deps, id, caldavName)
}

type davClient struct {
	http     *http.Client
	user     string
	password string
}

func newDAVClient(cfg settings.Resolved) davClient {
	return davClient{
		http: safehttp.NewClient(safehttp.Options{Timeout: 30 * time.Second, FailOnRedirect: true}),
		user: cfg.String(fieldUsername), password: cfg.Secret(fieldPassword),
	}
}

func (d davClient) Do(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(d.user, d.password)
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > maxCalendarResponse {
		_ = resp.Body.Close()
		return nil, errors.New("CalDAV response exceeds the size limit")
	}
	resp.Body = &boundedDAVBody{ReadCloser: resp.Body, remaining: maxCalendarResponse}
	return resp, nil
}

type boundedDAVBody struct {
	io.ReadCloser
	remaining int
}

func (b *boundedDAVBody) Read(buf []byte) (int, error) {
	if b.remaining == 0 {
		return 0, errors.New("CalDAV response exceeds the size limit")
	}
	n, err := b.ReadCloser.Read(buf[:min(len(buf), b.remaining)])
	b.remaining -= n
	return n, err
}

func (d davClient) query(ctx context.Context, col collection) ([]caldav.CalendarObject, error) {
	endpoint, err := collectionURL(col.endpoint)
	if err != nil {
		return nil, err
	}
	dav, err := caldav.NewClient(d, endpoint.String())
	if err != nil {
		return nil, err
	}
	return dav.QueryCalendar(ctx, endpoint.Path, &caldav.CalendarQuery{
		CompFilter: caldav.CompFilter{Name: ical.CompCalendar, Comps: []caldav.CompFilter{{Name: col.component}}},
	})
}

func syncCollections(ctx context.Context, sink *events.DBSink, dav davClient, cfg settings.Resolved) error {
	existing, err := sink.ExternalIDs(ctx, caldavName)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(existing))
	owner := ownerEmail(cfg.String(fieldOwner))
	cols := collections(cfg)
	if len(cols) == 0 {
		return errors.New("CalDAV has no configured collections")
	}
	for _, col := range cols {
		objects, err := dav.query(ctx, col)
		if err != nil {
			// A server's error can contain an href or part of an entry.
			return fmt.Errorf("CalDAV could not query %s", strings.ToLower(col.label))
		}
		prefix := namespace(owner, cfg.String(fieldUsername), col.endpoint, col.component)
		for _, obj := range objects {
			for _, comp := range obj.Data.Children {
				if comp.Name != col.component {
					continue
				}
				entry, err := calendarEvent(comp, obj, prefix, events.ScopeOwner(owner))
				if err != nil {
					return err
				}
				if _, err := sink.Upsert(ctx, entry); err != nil {
					return err
				}
				seen[entry.ExternalID] = true
			}
		}
	}
	return removeMissing(ctx, sink, caldavName, existing, seen)
}
