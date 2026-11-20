package adminui

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/settings"
)

// templateFuncs is what every page template can call. The style guide page
// (templates/styleguide.html) shows each one's output.
var templateFuncs = template.FuncMap{
	"time":      func(value any) template.HTML { return timeTag(value, false) },
	"logtime":   func(value any) template.HTML { return timeTag(value, true) },
	"duration":  formatDuration,
	"count":     func(n any) string { return formatCount(templateInt(n)) },
	"bytes":     func(n any) string { return formatBytes(templateInt(n)) },
	"badge":     badge,
	"mark":      func() template.HTML { return markSVG },
	"favicon":   func() template.URL { return faviconURL },
	"actor":     actorName,
	"changedBy": func(updatedBy string) string { return actorName(settings.ChangedBy(updatedBy)) },
	"header":    newPageHeader,
	"crumb":     func(label, href string) crumb { return crumb{Label: label, Href: href} },
	"tabs":      func(label string, tabs ...navTab) tabSet { return tabSet{Label: label, Tabs: tabs} },
	"tab": func(label, href string, current bool) navTab {
		return navTab{Label: label, Href: href, Current: current}
	},
}

// templateInt is an integer of any width a template passes.
func templateInt(value any) int64 {
	switch n := value.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	}
	panic(fmt.Sprintf("adminui: unsupported number %T", value))
}

// pageHeader is the "page-header" partial's input. A template builds one
// with the header func and adds to it with the With methods.
type pageHeader struct {
	Title  string
	Lead   string
	Crumbs []crumb
	// Badge is the state of the thing the page shows, beside its title.
	Badge *headerBadge
	// HasMeta and HasActions say the page defines the "header-meta" or
	// "header-actions" template; each is rendered with the data given.
	HasMeta     bool
	MetaData    any
	HasActions  bool
	ActionsData any
}

type headerBadge struct {
	Tone  string
	Label string
}

type crumb struct {
	Label string
	Href  string
}

func newPageHeader(title, lead string, crumbs ...crumb) pageHeader {
	return pageHeader{Title: title, Lead: lead, Crumbs: crumbs}
}

// WithBadge puts a status badge beside the title.
func (h pageHeader) WithBadge(tone, label string) pageHeader {
	h.Badge = &headerBadge{Tone: tone, Label: label}
	return h
}

// WithMeta adds the line of facts under the title: the page's own
// "header-meta" template, rendered with data.
func (h pageHeader) WithMeta(data any) pageHeader {
	h.HasMeta, h.MetaData = true, data
	return h
}

// WithActions adds the page-level actions: the page's own "header-actions"
// template, rendered with data.
func (h pageHeader) WithActions(data any) pageHeader {
	h.HasActions, h.ActionsData = true, data
	return h
}

// pagination is the "pagination" partial's input: what is shown, and the
// links back toward the start of the list and forward along it, each empty
// when there is none. The links read "Newer" and "Older" unless a list in
// another order names them.
type pagination struct {
	Summary      string
	BackURL      string
	ForwardURL   string
	BackLabel    string
	ForwardLabel string
}

// Back and Forward are the two links' words.
func (p pagination) Back() string {
	if p.BackLabel != "" {
		return p.BackLabel
	}
	return "Newer"
}

func (p pagination) Forward() string {
	if p.ForwardLabel != "" {
		return p.ForwardLabel
	}
	return "Older"
}

// More says the list has another page either way.
func (p pagination) More() bool { return p.BackURL != "" || p.ForwardURL != "" }

// barChart is the "chart" partial's input: counts over time, drawn as bars
// in an SVG whose viewBox is one unit wide per bar and 100 units tall.
type barChart struct {
	// Caption names what is counted: "Questions per hour".
	Caption string
	Bars    []chartBar
	// Summary says what the chart shows, for a screen reader.
	Summary string
	// From and To label the two ends of the time axis.
	From, To string
	// BadLabel names the part of each bar drawn in the bad tone, and
	// RestLabel the rest of it; with them the chart shows a key. Empty
	// for a chart of one kind of thing.
	BadLabel, RestLabel string
	// Note is a sentence under the chart about how far to trust it.
	Note string
}

type chartBar struct {
	Start time.Time
	Count int64
	// Bad is how much of Count is the bad part (BadLabel).
	Bad int64
	// Label is what pointing at the bar says, when the start time and
	// count alone would not: "Week of Nov 10: 3 helpful, 1 not helpful".
	Label string
}

// drawnBar is a bar placed in the chart's viewBox, with its bad part on
// top.
type drawnBar struct {
	chartBar
	X, Y, Height float64
	BadHeight    float64
}

// Peak is the largest bar.
func (c barChart) Peak() int64 {
	var peak int64
	for _, bar := range c.Bars {
		peak = max(peak, bar.Count)
	}
	return peak
}

// Top is the value of the top gridline: the smallest of 2, 4, 6, 8, or 10
// times a power of ten at or above the peak, so it and Middle are whole.
func (c barChart) Top() int64 {
	peak := c.Peak()
	top := int64(2)
	for scale := int64(1); top < peak; scale *= 10 {
		for _, step := range []int64{2, 4, 6, 8, 10} {
			if top = step * scale; top >= peak {
				break
			}
		}
	}
	return top
}

// Middle is the value of the middle gridline.
func (c barChart) Middle() int64 { return c.Top() / 2 }

// Width is the viewBox's width: one unit a bar.
func (c barChart) Width() int { return len(c.Bars) }

// Drawn places the bars, each 0.7 units wide and centred in its unit.
func (c barChart) Drawn() []drawnBar {
	top := float64(c.Top())
	drawn := make([]drawnBar, len(c.Bars))
	for i, bar := range c.Bars {
		height := 100 * float64(bar.Count) / top
		bad := 100 * float64(min(bar.Bad, bar.Count)) / top
		drawn[i] = drawnBar{chartBar: bar, X: float64(i) + 0.15, Y: 100 - height, Height: height, BadHeight: bad}
	}
	return drawn
}

// relativeLimit is how far from now an instant still reads as relative.
const relativeLimit = 30 * 24 * time.Hour

// timeTag renders an instant (a time.Time or *time.Time) as a <time>
// element: relative text within 30 days of now, a date beyond, and the exact
// UTC value in the title. admin.js rewrites the title and a date in the
// viewer's zone. A nil or zero instant renders an em dash.
//
// withClock is for lists that read as a log (the audit log, job and run
// history, failures, conversations), where several rows share a day: a date
// then carries its time of day, "Aug 28, 2:05 PM".
func timeTag(value any, withClock bool) template.HTML {
	var at time.Time
	switch v := value.(type) {
	case time.Time:
		at = v
	case *time.Time:
		if v != nil {
			at = *v
		}
	default:
		panic(fmt.Sprintf("adminui: time: unsupported %T", value))
	}
	if at.IsZero() {
		return `<span class="muted">—</span>`
	}
	now := time.Now()
	text, relative := relativeTime(now, at)
	utc := at.UTC()
	mode := "date"
	switch {
	case relative:
		mode = "relative"
	case withClock:
		mode = "datetime"
		text += utc.Format(", 3:04 PM") + " UTC"
	}
	return template.HTML(fmt.Sprintf(`<time datetime="%s" title="%s" data-time="%s">%s</time>`,
		utc.Format(time.RFC3339), utc.Format("2006-01-02 15:04:05 UTC"), mode, template.HTMLEscapeString(text)))
}

// relativeTime says when at is, as seen from now: "just now", "3 min ago",
// "2 hours ago", "5 days ago", "in 4 hours", or beyond 30 days a UTC date.
// relative is false for the date.
func relativeTime(now, at time.Time) (text string, relative bool) {
	distance := now.Sub(at)
	future := distance < 0
	if future {
		distance = -distance
	}
	if distance >= relativeLimit {
		utc := at.UTC()
		if utc.Year() == now.UTC().Year() {
			return utc.Format("Jan 2"), false
		}
		return utc.Format("Jan 2, 2006"), false
	}
	var amount string
	switch {
	case distance < time.Minute:
		return "just now", true
	case distance < time.Hour:
		amount = fmt.Sprintf("%d min", int(distance/time.Minute))
	case distance < 24*time.Hour:
		amount = plural(int(distance/time.Hour), "hour")
	default:
		amount = plural(int(distance/(24*time.Hour)), "day")
	}
	if future {
		return "in " + amount, true
	}
	return amount + " ago", true
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// formatAge is relativeTime as plain text, for a view model that carries a
// string; a nil instant is an em dash. Templates use the time func instead.
func formatAge(at *time.Time) string {
	if at == nil {
		return "—"
	}
	text, _ := relativeTime(time.Now(), *at)
	return text
}

// formatDuration reads "83ms", "1.2s", "31s", "8m 20s", "2h 5m", "3d 4h": the
// two largest units, the smaller left out when it is zero. Under ten seconds
// it keeps a tenth, since that is where a tool call or a quick answer lands.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	two := func(big int64, bigUnit string, small int64, smallUnit string) string {
		if small == 0 {
			return fmt.Sprintf("%d%s", big, bigUnit)
		}
		return fmt.Sprintf("%d%s %d%s", big, bigUnit, small, smallUnit)
	}
	switch {
	case d == 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", float64(d/(100*time.Millisecond))/10)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	case d < time.Hour:
		return two(int64(d/time.Minute), "m", int64(d%time.Minute/time.Second), "s")
	case d < 24*time.Hour:
		return two(int64(d/time.Hour), "h", int64(d%time.Hour/time.Minute), "m")
	}
	return two(int64(d/(24*time.Hour)), "d", int64(d%(24*time.Hour)/time.Hour), "h")
}

// formatCount abbreviates a count from a thousand up: "950", "12.3K", "4.1M".
func formatCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// formatBytes reads a size in binary units: "512 B", "3.4 MiB".
func formatBytes(n int64) string {
	const (
		ki = 1024
		mi = 1024 * ki
		gi = 1024 * mi
	)
	switch {
	case n >= gi:
		return fmt.Sprintf("%.1f GiB", float64(n)/gi)
	case n >= mi:
		return fmt.Sprintf("%.1f MiB", float64(n)/mi)
	case n >= ki:
		return fmt.Sprintf("%.1f KiB", float64(n)/ki)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// Badge tones. A state keeps one word and one tone on every page.
const (
	toneGood      = "good"
	toneAttention = "attention"
	toneBad       = "bad"
	toneNeutral   = "neutral"
)

// badge renders the status badge: one lowercase word in one of the four
// tones.
func badge(tone, label string) template.HTML {
	switch tone {
	case toneGood, toneAttention, toneBad, toneNeutral:
	default:
		panic("adminui: badge: unknown tone " + tone)
	}
	return template.HTML(fmt.Sprintf(`<span class="badge badge-%s">%s</span>`,
		tone, template.HTMLEscapeString(strings.ToLower(label))))
}

// systemActorPrefix marks a change Diderot made itself (settings and
// setup write "diderot:migration", "diderot:registry", "diderot:seed").
const systemActorPrefix = "diderot:"

// actorName is how pages show who did something: the stored address, except
// the bootstrap admin's placeholder and Diderot's own writes.
func actorName(email string) string {
	switch {
	case strings.EqualFold(email, auth.BootstrapEmail):
		return "admin (bootstrap)"
	case strings.HasPrefix(email, systemActorPrefix):
		return "Diderot"
	}
	return email
}
