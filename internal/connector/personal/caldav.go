package personal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
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
			Help: "An HTTPS calendar collection URL, or a server URL such as https://caldav.fastmail.com/ to discover all event calendars. Leave empty for notes only."},
		settings.Field{Key: fieldJournalURL, Label: "Notes collection URL", Kind: settings.KindText,
			Help: "An HTTPS collection URL for CalDAV journal entries (VJOURNAL), or a server URL to discover journal collections."})
}

func (CalDAV) Validate(values settings.Values) error {
	errs := accountErrors(values)
	calendar, journal := settingsString(values, fieldCalendarURL), settingsString(values, fieldJournalURL)
	if calendar == "" && journal == "" {
		errs[fieldCalendarURL] = "enter at least one CalDAV URL"
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
		u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("enter an HTTPS server or collection URL without credentials, a query, or a fragment")
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
	dav := newDAVClient(cfg)
	for _, col := range collections(cfg) {
		report.Checks = append(report.Checks, dav.testCollection(ctx, col))
	}
	return report
}

func (d davClient) testCollection(ctx context.Context, col collection) settings.Check {
	sources, err := d.resolve(ctx, col)
	if err != nil {
		return caldavCheckError(col.label, err)
	}
	total := 0
	for _, src := range sources {
		objects, err := d.query(ctx, src)
		if err != nil {
			return caldavCheckError(col.label, err)
		}
		total += len(objects)
	}
	return settings.Check{Name: col.label, Passed: true,
		Detail: fmt.Sprintf("Read %d objects across %d collections.", total, len(sources))}
}

func caldavCheckError(label string, err error) settings.Check {
	check := settings.Check{Name: label, Detail: "Could not read the collection over HTTPS.",
		Remedy: "Check the CalDAV URL and this server's connection, or try a full collection URL."}
	var status davStatusError
	switch {
	case errors.Is(err, errNoCalDAVCollections):
		check.Detail = "The server advertised no collections for this entry type."
		check.Remedy = "Enter a collection URL for this entry type or clear this field."
	case errors.As(err, &status):
		check.Detail = fmt.Sprintf("The CalDAV server responded with HTTP %d.", status)
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			check.Remedy = "Check the full account username and use an app password with CalDAV access."
		}
	}
	return check
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
		http: safehttp.NewClient(safehttp.Options{Timeout: 30 * time.Second, CheckRedirect: safehttp.NoFollow}),
		user: cfg.String(fieldUsername), password: cfg.Secret(fieldPassword),
	}
}

type davStatusError int

func (e davStatusError) Error() string { return fmt.Sprintf("CalDAV HTTP %d", e) }

func (d davClient) Do(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(d.user, d.password)
	for redirects := 0; ; redirects++ {
		resp, err := d.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 == 2 {
			if resp.ContentLength > maxCalendarResponse {
				_ = resp.Body.Close()
				return nil, errors.New("CalDAV response exceeds the size limit")
			}
			resp.Body = &boundedDAVBody{ReadCloser: resp.Body, remaining: maxCalendarResponse}
			return resp, nil
		}
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		default:
			return nil, davStatusError(resp.StatusCode)
		}
		target, err := resp.Location()
		if err != nil || redirects >= 3 || target.Scheme != req.URL.Scheme ||
			!strings.EqualFold(target.Host, req.URL.Host) || target.User != nil ||
			target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
			return nil, errors.New("CalDAV refused the redirect")
		}
		// Preserve the DAV method and XML body across a service or slash redirect.
		req = req.Clone(req.Context())
		req.URL = target
		if req.GetBody != nil {
			replayed, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = replayed
		} else if req.Body != nil && req.Body != http.NoBody {
			return nil, errors.New("CalDAV cannot replay the redirected request")
		}
	}
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

var errNoCalDAVCollections = errors.New("CalDAV discovery found no matching collections")

func (d davClient) resolve(ctx context.Context, col collection) ([]collection, error) {
	endpoint, err := collectionURL(col.endpoint)
	if err != nil {
		return nil, err
	}
	if endpoint.Path != "" && endpoint.Path != "/" {
		return []collection{col}, nil
	}

	discovery := *endpoint
	discovery.Path, discovery.RawPath = "/.well-known/caldav", ""
	dav, err := caldav.NewClient(d, discovery.String())
	if err != nil {
		return nil, err
	}
	principal, err := dav.FindCurrentUserPrincipal(ctx)
	if errors.Is(err, davStatusError(http.StatusNotFound)) {
		dav, err = caldav.NewClient(d, endpoint.String())
		if err != nil {
			return nil, err
		}
		principal, err = dav.FindCurrentUserPrincipal(ctx)
	}
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(principal, "/") {
		return nil, errors.New("CalDAV returned an invalid principal")
	}
	home, err := dav.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(home, "/") {
		return nil, errors.New("CalDAV returned an invalid calendar home")
	}
	calendars, err := dav.FindCalendars(ctx, home)
	if err != nil {
		return nil, err
	}

	var result []collection
	seen := map[string]bool{}
	for _, calendar := range calendars {
		if !strings.HasPrefix(calendar.Path, "/") ||
			(len(calendar.SupportedComponentSet) != 0 && !slices.Contains(calendar.SupportedComponentSet, col.component)) {
			continue
		}
		collectionURL := *endpoint
		collectionURL.Path, collectionURL.RawPath = calendar.Path, ""
		discovered := col
		discovered.endpoint = collectionURL.String()
		if !seen[discovered.endpoint] {
			result = append(result, discovered)
			seen[discovered.endpoint] = true
		}
	}
	if len(result) == 0 {
		return nil, errNoCalDAVCollections
	}
	return result, nil
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
	var resolved []collection
	for _, col := range cols {
		sources, err := dav.resolve(ctx, col)
		if err != nil {
			return fmt.Errorf("CalDAV could not discover %s", strings.ToLower(col.label))
		}
		resolved = append(resolved, sources...)
	}
	for _, col := range resolved {
		objects, err := dav.query(ctx, col)
		if err != nil {
			// A server's error can contain an href or part of an entry.
			return fmt.Errorf("CalDAV could not query %s", strings.ToLower(col.label))
		}
		prefix := namespace(owner, cfg.String(fieldUsername), strings.TrimSuffix(col.endpoint, "/")+"/", col.component)
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
