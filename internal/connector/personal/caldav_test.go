package personal

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emersion/go-ical"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/settings"
)

func TestCalDAVDiscoverCalendarsFromServerURL(t *testing.T) {
	t.Parallel()
	const multistatus = `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">%s</D:multistatus>`
	prop := func(path, properties string) string {
		return fmt.Sprintf(`<D:response><D:href>%s</D:href><D:propstat><D:prop>%s</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`,
			path, properties)
	}
	calendar := func(path, component string) string {
		return prop(path, `<D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`+
			fmt.Sprintf(`<C:supported-calendar-component-set><C:comp name="%s"/></C:supported-calendar-component-set>`, component))
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		assert.Equal(t, "alice@example.com", user)
		assert.Equal(t, "fixture-password", pass)
		data, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		if r.URL.Path == "/.well-known/caldav" {
			w.Header().Set("Location", "/dav/calendars")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		switch r.URL.Path {
		case "/dav/calendars":
			assert.Equal(t, "PROPFIND", r.Method)
			assert.Contains(t, string(data), "current-user-principal")
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprintf(w, multistatus, prop("/dav/calendars", `<D:current-user-principal><D:href>/dav/principals/alice/</D:href></D:current-user-principal>`))
		case "/dav/principals/alice/":
			assert.Equal(t, "PROPFIND", r.Method)
			assert.Contains(t, string(data), "calendar-home-set")
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprintf(w, multistatus, prop("/dav/principals/alice/", `<C:calendar-home-set><D:href>/dav/calendars/alice/</D:href></C:calendar-home-set>`))
		case "/dav/calendars/alice/":
			assert.Equal(t, "PROPFIND", r.Method)
			assert.Equal(t, "1", r.Header.Get("Depth"))
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprintf(w, multistatus, prop("/dav/calendars/alice/", `<D:resourcetype><D:collection/></D:resourcetype>`)+
				calendar("/dav/calendars/alice/work/", "VEVENT")+calendar("/dav/calendars/alice/home/", "VEVENT")+
				calendar("/dav/calendars/alice/journal/", "VJOURNAL"))
		case "/dav/calendars/alice/work/", "/dav/calendars/alice/home/":
			assert.Equal(t, "REPORT", r.Method)
			assert.True(t, strings.Contains(string(data), `name="VEVENT"`))
			writeDAVReport(w, r.URL.Path, "BEGIN:VEVENT\r\nUID:fixture\r\nDTSTART:20261125T140000Z\r\nEND:VEVENT\r\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cfg := settings.NewResolved(caldavName, settings.Values{fieldUsername: "alice@example.com"}, map[string]string{fieldPassword: "fixture-password"})
	dav := newDAVClient(cfg)
	dav.http.Transport = server.Client().Transport

	events := dav.testCollection(t.Context(), collection{server.URL, ical.CompEvent, "Calendar events"})
	require.True(t, events.Passed, events.Detail)
	require.Equal(t, "Read 2 objects across 2 collections.", events.Detail)
	journals, err := dav.resolve(t.Context(), collection{server.URL + "/", ical.CompJournal, "Journal notes"})
	require.NoError(t, err)
	require.Equal(t, []collection{{server.URL + "/dav/calendars/alice/journal/", ical.CompJournal, "Journal notes"}}, journals)
}

func TestCalDAVRejectsCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("CalDAV contacted a different origin")
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(target.Close)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/dav/calendars")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	t.Cleanup(server.Close)
	cfg := settings.NewResolved(caldavName, settings.Values{fieldUsername: "alice@example.com"}, map[string]string{fieldPassword: "fixture-password"})
	dav := newDAVClient(cfg)
	dav.http.Transport = server.Client().Transport
	_, err := dav.resolve(t.Context(), collection{server.URL, ical.CompEvent, "Calendar events"})
	require.ErrorContains(t, err, "CalDAV refused the redirect")
}
