package settings

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

// fakeConnector is a registered connector with configurable fields, a Test
// that reports what it saw, and a Run that records its lifecycle.
type fakeConnector struct {
	name   string
	fields []Field
	test   func(Resolved) TestReport

	mu     sync.Mutex
	starts []Resolved
	stops  int
	fail   error
	panics bool
}

func (f *fakeConnector) Name() string    { return f.name }
func (f *fakeConnector) Fields() []Field { return f.fields }
func (f *fakeConnector) Validate(values Values) error {
	if values["mode"] == "forbidden" {
		return FieldErrors{"mode": "is forbidden"}
	}
	return nil
}

func (f *fakeConnector) Test(_ context.Context, r Resolved) TestReport {
	if f.test != nil {
		return f.test(r)
	}
	return TestReport{Checks: []Check{{Name: "ok", Passed: true}}}
}

func (f *fakeConnector) Run(ctx context.Context, r Resolved) error {
	f.mu.Lock()
	f.starts = append(f.starts, r)
	fail, panics := f.fail, f.panics
	f.mu.Unlock()
	if panics {
		panic("connector bug")
	}
	if fail != nil {
		return fail
	}
	<-ctx.Done()
	f.mu.Lock()
	f.stops++
	f.mu.Unlock()
	return nil
}

func (f *fakeConnector) lifecycle() (starts []Resolved, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Resolved(nil), f.starts...), f.stops
}

func exampleFields() []Field {
	return []Field{
		{Key: "token", Label: "Token", Kind: KindSecret, Required: true},
		{Key: "extra", Label: "Extra", Kind: KindSecret},
		{Key: "mode", Label: "Mode", Kind: KindEnum, Options: []string{"member", "all", "forbidden"}, Default: "member", Required: true},
		{Key: "ids", Label: "IDs", Kind: KindList},
		{Key: "deep", Label: "Deep", Kind: KindBool},
		{Key: "limit", Label: "Limit", Kind: KindInt, Default: int64(20)},
		{Key: "every", Label: "Every", Kind: KindDuration, Default: "5m"},
		{Key: "site", Label: "Site", Kind: KindText, Discovered: true, PromptVar: promptcatalog.VarSlackWorkspaceName},
	}
}

func TestRegistryRefusesBrokenDefinitions(t *testing.T) {
	require.NotPanics(t, func() { NewRegistry(&fakeConnector{name: "example", fields: exampleFields()}) })
	for name, fields := range map[string][]Field{
		"secret prompt variable": {{Key: "token", Kind: KindSecret, PromptVar: promptcatalog.VarSlackWorkspaceName}},
		"unlisted variable":      {{Key: "site", Kind: KindText, PromptVar: "NOT_A_VARIABLE"}},
		"non-text variable":      {{Key: "site", Kind: KindBool, PromptVar: promptcatalog.VarSlackWorkspaceName}},
		"enum without options":   {{Key: "mode", Kind: KindEnum}},
		"repeated key":           {{Key: "a", Kind: KindText}, {Key: "a", Kind: KindText}},
		"bad key":                {{Key: "Bad-Key", Kind: KindText}},
		"bad default":            {{Key: "n", Kind: KindInt, Default: "many"}},
		"discovered secret":      {{Key: "token", Kind: KindSecret, Discovered: true}},
	} {
		assert.Panics(t, func() { NewRegistry(&fakeConnector{name: "example", fields: fields}) }, name)
	}
	assert.Panics(t, func() { NewRegistry(&fakeConnector{name: OrgConnector}) }, "org is the organization's row")
	assert.Panics(t, func() { NewRegistry(&fakeConnector{name: "a"}, &fakeConnector{name: "a"}) })
}

func TestParseValues(t *testing.T) {
	def := &fakeConnector{name: "example", fields: exampleFields()}
	stored := normalizeValues(def, map[string]any{"ids": []any{"U1"}, "limit": float64(7), "site": "found.example"})
	assert.Equal(t, Values{
		"mode": "member", "ids": []string{"U1"}, "deep": false, "limit": int64(7), "every": "5m0s", "site": "found.example",
	}, stored, "stored JSON is read back canonically, with defaults")

	values, err := parseValues(def, map[string]any{
		"mode": "all", "ids": " U2,U3\nU2 \n", "deep": "true", "limit": "9", "every": "90s", "site": "forged",
	}, stored)
	require.NoError(t, err)
	assert.Equal(t, Values{
		"mode": "all", "ids": []string{"U2", "U3"}, "deep": true, "limit": int64(9), "every": "1m30s", "site": "found.example",
	}, values, "a discovered field is never input")

	values, err = parseValues(def, map[string]any{"deep": ""}, stored)
	require.NoError(t, err)
	assert.Equal(t, false, values["deep"])
	assert.Equal(t, []string{"U1"}, values["ids"], "fields left out keep their stored value")

	_, err = parseValues(def, map[string]any{"mode": "", "limit": "lots", "ids": "two\x00{{x}}"}, stored)
	var fieldErrs FieldErrors
	require.ErrorAs(t, err, &fieldErrs)
	assert.ErrorIs(t, err, ErrInvalid)
	assert.Equal(t, "is required", fieldErrs["mode"])
	assert.Contains(t, fieldErrs["limit"], "whole number")
	assert.Contains(t, fieldErrs["ids"], "{{")

	_, err = parseValues(def, map[string]any{"mode": "forbidden"}, stored)
	assert.ErrorContains(t, err, "mode: is forbidden", "Validate runs after the kinds are parsed")
}

// TestResolvedNeverPrintsSecrets: settings handed to workers keep their
// secrets out of every fmt verb and of slog.
func TestResolvedNeverPrintsSecrets(t *testing.T) {
	resolved := NewResolved("example", Values{"mode": "member"}, map[string]string{"token": "tok-SECRET-123"})
	assert.Equal(t, "tok-SECRET-123", resolved.Secret("token"))
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		assert.NotContains(t, fmt.Sprintf(verb, resolved), "tok-SECRET", verb)
		assert.NotContains(t, fmt.Sprintf(verb, []Resolved{resolved}), "tok-SECRET", verb)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("starting", "settings", resolved)
	assert.NotContains(t, logs.String(), "tok-SECRET")
	assert.Contains(t, logs.String(), `"connector":"example"`)
}

func TestScrubReport(t *testing.T) {
	report := scrubReport(TestReport{
		Checks: []Check{{Name: "Token", Detail: "rejected tok-SECRET-123", Remedy: "replace tok-SECRET-123"}},
		Facts:  []Fact{{Key: "who", Label: "Who", Value: "tok-SECRET-123"}},
	}, map[string]string{"token": "tok-SECRET-123", "extra": ""})
	assert.Equal(t, "rejected [secret]", report.Checks[0].Detail)
	assert.Equal(t, "replace [secret]", report.Checks[0].Remedy)
	assert.Equal(t, "[secret]", report.Facts[0].Value)
}

func TestTakeDiscovered(t *testing.T) {
	def := &fakeConnector{name: "example", fields: exampleFields()}
	values := normalizeValues(def, nil)
	test := TestRecord{Passed: true, TestReport: TestReport{Facts: []Fact{
		{Key: "site", Value: "found.example"}, {Key: "mode", Value: "all"},
	}}}
	require.NoError(t, takeDiscovered(def, values, test))
	assert.Equal(t, "found.example", values["site"])
	assert.Equal(t, "member", values["mode"], "a fact fills only a discovered field")
	values["site"] = "kept"
	test.Passed = false
	require.NoError(t, takeDiscovered(def, values, test))
	assert.Equal(t, "kept", values["site"], "a failed test discovers nothing")
	test.Passed, test.Facts = true, []Fact{{Key: "site", Value: "two\nlines"}}
	assert.ErrorIs(t, takeDiscovered(def, values, test), ErrInvalid)
}

// TestSupervisorRestartsOnVersionChangeAndStopsOnDisable drives the
// supervisor through the store's cache, as the poll would.
func TestSupervisorRestartsOnVersionChangeAndStopsOnDisable(t *testing.T) {
	fake := &fakeConnector{name: "example", fields: []Field{{Key: "mode", Kind: KindText}}}
	store := NewStore(nil, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))).WithConnectors(NewRegistry(fake), Secrets{})
	set := func(version int64, enabled bool, mode string) {
		store.storeConnector(fake, Row{Connector: "example", Version: version, Enabled: enabled, Settings: map[string]any{"mode": mode}})
	}
	set(1, false, "a")
	supervisor := NewSupervisor(store, slog.New(slog.NewTextHandler(io.Discard, nil)), fake)
	supervisor.Interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		supervisor.Run(ctx)
	}()
	waitFor := func(starts, stops int) []Resolved {
		t.Helper()
		var got []Resolved
		require.Eventually(t, func() bool {
			var stopped int
			got, stopped = fake.lifecycle()
			return len(got) == starts && stopped == stops
		}, 5*time.Second, 5*time.Millisecond)
		return got
	}

	waitFor(0, 0)
	set(2, true, "a")
	starts := waitFor(1, 0)
	assert.Equal(t, "a", starts[0].String("mode"))
	assert.Equal(t, int64(2), starts[0].Version)

	set(3, true, "b")
	starts = waitFor(2, 1)
	assert.Equal(t, "b", starts[1].String("mode"), "a version change restarts with the new settings")

	set(4, false, "b")
	waitFor(2, 2)
	time.Sleep(50 * time.Millisecond)
	waitFor(2, 2)

	set(5, true, "c")
	waitFor(3, 2)
	cancel()
	<-done
	waitFor(3, 3)
}

func TestSupervisorRetriesARunThatEnds(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		testSupervisorRetries(t, &fakeConnector{name: "example", fail: fmt.Errorf("boom")})
	})
	t.Run("panic", func(t *testing.T) { testSupervisorRetries(t, &fakeConnector{name: "example", panics: true}) })
}

func testSupervisorRetries(t *testing.T, fake *fakeConnector) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	store := NewStore(nil, logger).WithConnectors(NewRegistry(fake), Secrets{})
	store.storeConnector(fake, Row{Connector: "example", Version: 1, Enabled: true})
	supervisor := NewSupervisor(store, logger, fake)
	supervisor.Interval, supervisor.RetryAfter = 5*time.Millisecond, 40*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		supervisor.Run(ctx)
	}()
	require.Eventually(t, func() bool {
		starts, _ := fake.lifecycle()
		return len(starts) >= 3
	}, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done
	assert.True(t, strings.Contains(logs.String(), "connector stopped on its own"))
}
