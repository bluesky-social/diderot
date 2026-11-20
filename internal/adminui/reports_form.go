package adminui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/scheduledreport"
)

// Limits the store enforces, checked here so the form can say so.
const (
	reportNameMaxBytes   = 200
	reportPromptMaxBytes = 32 << 10
	reportLateMaxHours   = 720
)

// Where a report goes, as the form names it.
const (
	reportDeliveryChannel = "channel"
	reportDeliveryDM      = "dm"
)

// reportFormData is the page that creates a report or changes one.
type reportFormData struct {
	pageData
	// Report is the saved report being changed, nil for a new one.
	Report    *reportView
	Form      reportForm
	SlackOn   bool
	CSRFToken string
	Error     string
}

// ReportTab is the report page tab this is.
func (reportFormData) ReportTab() string { return "edit" }

// reportForm is the report form as the operator filled it in.
type reportForm struct {
	Name    string
	Prompt  string
	Cadence string
	// Weekday is the ISO weekday a weekly report runs, "1" (Monday) to "7".
	Weekday  string
	Time     string
	TimeZone string
	// Delivery is reportDeliveryChannel or reportDeliveryDM.
	Delivery string
	Channel  string
	Owner    string
	// LateHours is how many hours late a run may still be sent.
	LateHours string
	// Enabled is whether a new report runs on its schedule. Changing a
	// report keeps its state.
	Enabled bool
	// Errors say what is wrong with a field, by its name.
	Errors map[string]string
}

func (f *reportForm) fail(field, message string) {
	if f.Errors == nil {
		f.Errors = map[string]string{}
	}
	if _, ok := f.Errors[field]; !ok {
		f.Errors[field] = message
	}
}

func (s *Server) renderReportForm(w http.ResponseWriter, r *http.Request, saved *reportView, form reportForm, problem string, status int) {
	csrfToken, err := issueCSRFToken(w, r)
	if err != nil {
		s.reportPageError(w, "CSRF token", err)
		return
	}
	title := "New report"
	if saved != nil {
		title = saved.Name
	}
	data := reportFormData{
		pageData: s.page(r, title, navReports), Report: saved, Form: form,
		SlackOn: s.slackOn(r.Context()), CSRFToken: csrfToken, Error: problem,
	}
	data.Stylesheet, data.Script = "reports.css", "reports.js"
	s.renderStatus(w, reportFormPage, data, status)
}

// newReportForm is the form for a new report: weekdays at 9:00 in the
// operator's Slack time zone, owned by the operator.
func (s *Server) newReportForm(ctx context.Context, operator string) reportForm {
	form := reportForm{
		Cadence: scheduledreport.CadenceWeekdays, Weekday: "1", Time: "09:00", TimeZone: "UTC",
		Delivery: reportDeliveryChannel, LateHours: "6", Enabled: true,
	}
	if !strings.EqualFold(operator, auth.BootstrapEmail) {
		form.Owner = operator
	}
	var zone string
	err := s.pool.QueryRow(ctx, `
		SELECT tz FROM slack_users
		 WHERE lower(email) = lower($1) AND NOT is_bot AND COALESCE(tz, '') <> ''
		 LIMIT 1`, operator).Scan(&zone)
	if err == nil && validReportZone(zone) {
		form.TimeZone = zone
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.logger.Warn("admin reports: operator time zone unavailable", "err", err)
	}
	return form
}

// reportFormFromReport is the form for changing report.
func reportFormFromReport(report scheduledreport.Report, names map[string]string) reportForm {
	form := reportForm{
		Name: report.Name, Prompt: report.Prompt, Cadence: report.Cadence, Weekday: "1",
		Time:     fmt.Sprintf("%02d:%02d", report.ScheduleHour, report.ScheduleMinute),
		TimeZone: report.Timezone, Delivery: reportDeliveryChannel, Owner: report.OwnerEmail,
		LateHours: formatLateHours(report.CatchupWindow),
		Enabled:   report.Enabled,
	}
	if report.Weekday != nil {
		form.Weekday = strconv.Itoa(*report.Weekday)
	}
	if report.DeliveryKind == scheduledreport.DeliverySlackDM {
		form.Delivery = reportDeliveryDM
	} else if name := names[report.SlackTarget]; name != "" {
		form.Channel = name
	} else {
		form.Channel = report.SlackTarget
	}
	return form
}

func reportFormFromRequest(r *http.Request) reportForm {
	field := func(name string) string { return strings.TrimSpace(r.Form.Get(name)) }
	return reportForm{
		Name: field("name"), Prompt: field("prompt"), Cadence: field("cadence"), Weekday: field("weekday"),
		Time: field("time"), TimeZone: field("timezone"), Delivery: field("delivery"),
		Channel: field("channel"), Owner: field("owner"), LateHours: field("late_hours"),
		Enabled: r.Form.Get("enabled") == "true",
	}
}

// resolveReportForm checks the form and turns it into a report, resolving
// the channel and the owner through the Slack directory. What is wrong is
// in form.Errors; err is only a failed lookup.
//
// saved is the report being changed, nil for a new one, and shown the form
// its edit page drew. A delivery target the form leaves as saved is kept
// without a lookup: a report whose channel was archived, or whose owner
// left Slack, can still have its schedule or instructions changed.
func (s *Server) resolveReportForm(ctx context.Context, form *reportForm, saved *scheduledreport.Report, shown reportForm) (scheduledreport.CreateParams, error) {
	params := scheduledreport.CreateParams{Name: form.Name, Prompt: form.Prompt, Cadence: form.Cadence}
	switch {
	case form.Name == "":
		form.fail("name", "Enter a name.")
	case len(form.Name) > reportNameMaxBytes:
		form.fail("name", fmt.Sprintf("Use at most %d characters.", reportNameMaxBytes))
	}
	switch {
	case form.Prompt == "":
		form.fail("prompt", "Say what the report should cover, or start from an example.")
	case len(form.Prompt) > reportPromptMaxBytes:
		form.fail("prompt", "This is too long. Keep the instructions under 32 KB.")
	}

	switch form.Cadence {
	case scheduledreport.CadenceDaily, scheduledreport.CadenceWeekdays:
	case scheduledreport.CadenceWeekly:
		day, err := strconv.Atoi(form.Weekday)
		if err != nil || day < 1 || day > 7 {
			form.fail("schedule", "Choose the day of the week.")
		} else {
			params.Weekday = &day
		}
	default:
		form.fail("schedule", "Choose how often it runs.")
	}
	if hour, minute, ok := parseReportTime(form.Time); ok {
		params.ScheduleHour, params.ScheduleMinute = hour, minute
	} else {
		form.fail("time", "Enter a time such as 09:30.")
	}
	if zone, ok := resolveReportZone(form.TimeZone); ok {
		params.Timezone = zone
	} else {
		form.fail("timezone", "Enter a time zone such as America/New_York.")
	}

	if seconds, ok := parseLateHours(form.LateHours); ok {
		params.CatchupSeconds = seconds
	} else {
		form.fail("late_hours", fmt.Sprintf("Enter a number of hours above zero, at most %d.", reportLateMaxHours))
	}

	sameDM := saved != nil && saved.DeliveryKind == scheduledreport.DeliverySlackDM &&
		form.Delivery == reportDeliveryDM && strings.EqualFold(form.Owner, saved.OwnerEmail)
	email, userID, problem, err := s.resolveReportOwner(ctx, form.Owner, form.Delivery == reportDeliveryDM && !sameDM)
	if err != nil {
		return params, err
	}
	if problem != "" {
		form.fail("owner", problem)
	}
	params.OwnerEmail = email
	switch form.Delivery {
	case reportDeliveryChannel:
		params.DeliveryKind = scheduledreport.DeliverySlackChannel
		if saved != nil && saved.DeliveryKind == scheduledreport.DeliverySlackChannel && sameReportChannel(form.Channel, shown.Channel, saved.SlackTarget) {
			params.SlackTarget = saved.SlackTarget
			break
		}
		channelID, name, problem, err := s.resolveReportChannel(ctx, form.Channel)
		if err != nil {
			return params, err
		}
		if problem != "" {
			form.fail("channel", problem)
		} else {
			form.Channel = name
		}
		params.SlackTarget = channelID
	case reportDeliveryDM:
		params.DeliveryKind, params.SlackTarget = scheduledreport.DeliverySlackDM, userID
		if sameDM {
			params.SlackTarget = saved.SlackTarget
		}
	default:
		form.fail("delivery", "Choose where the report goes.")
	}
	return params, nil
}

// sameReportChannel reports whether the channel typed is the saved one:
// what the edit form showed for it, or its ID.
func sameReportChannel(typed, shown, savedID string) bool {
	key := func(value string) string { return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "#")) }
	return key(typed) != "" && (key(typed) == key(shown) || strings.TrimSpace(typed) == savedID)
}

const reportNameTaken = "Another report already has this name. Choose another."

// duplicateReportName reports whether err is the store refusing a name
// another report has.
func duplicateReportName(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "scheduled_reports_name_key"
}

var (
	slackChannelIDPattern = regexp.MustCompile(`^[CG][A-Z0-9]{6,}$`)
	slackUserIDPattern    = regexp.MustCompile(`^[UW][A-Z0-9]{6,}$`)
)

// resolveReportChannel finds the channel input names: "#name", "name", an
// ID, or a link to the channel. problem says why none matches.
func (s *Server) resolveReportChannel(ctx context.Context, input string) (id, name, problem string, err error) {
	key := slackChannelKey(input)
	if key == "" {
		return "", "", "Enter the channel's name, such as #general.", nil
	}
	var archived bool
	err = s.pool.QueryRow(ctx, `
		SELECT channel_id, COALESCE(name, ''), is_archived FROM slack_channel_info
		 WHERE channel_id = $1 OR lower(name) = lower($1)
		 ORDER BY channel_id = $1 DESC, is_archived
		 LIMIT 1`, key).Scan(&id, &name, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		if slackChannelIDPattern.MatchString(key) {
			return "", "", "Diderot does not know the channel " + key + ". Check the ID, or add Diderot to the channel in Slack.", nil
		}
		return "", "", "Diderot does not know a channel named #" + key + ". Check the name, or add Diderot to the channel in Slack.", nil
	}
	if err != nil {
		return "", "", "", fmt.Errorf("resolve report channel: %w", err)
	}
	display := "#" + name
	if name == "" {
		display = id
	}
	if archived {
		return "", "", display + " is archived. Choose a channel people still use.", nil
	}
	return id, display, "", nil
}

// resolveReportOwner turns the owner input, an email or a Slack member ID,
// into an email. For a direct message it also finds the one Slack account
// with that email, which is where the message goes.
func (s *Server) resolveReportOwner(ctx context.Context, input string, direct bool) (email, userID, problem string, err error) {
	key := strings.TrimSpace(input)
	switch {
	case key == "":
		return "", "", "Enter the owner's email.", nil
	case slackUserIDPattern.MatchString(key):
		err = s.pool.QueryRow(ctx, `
			SELECT lower(COALESCE(email, '')) FROM slack_users WHERE user_id = $1 AND NOT is_bot`, key).Scan(&email)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", "Diderot does not know the Slack member " + key + ". Enter their email instead.", nil
		}
		if err != nil {
			return "", "", "", fmt.Errorf("resolve report owner: %w", err)
		}
		if email == "" {
			return "", "", "Slack has no email for this member. Enter their email instead.", nil
		}
	case strings.Contains(key, "@") && !strings.ContainsAny(key, " \t<>"):
		email = strings.ToLower(key)
	default:
		return "", "", "Enter an email address, such as ana@example.com.", nil
	}
	if !direct {
		return email, "", "", nil
	}
	rows, err := s.pool.Query(ctx, `SELECT user_id FROM slack_users WHERE lower(email) = $1 AND NOT is_bot`, email)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve report recipient: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", "", "", fmt.Errorf("resolve report recipient: %w", err)
	}
	switch len(ids) {
	case 0:
		return email, "", "No one in Slack has this email, so a direct message cannot reach them. Use the email of their Slack account, or post in a channel.", nil
	case 1:
		return email, ids[0], "", nil
	}
	return email, "", "More than one Slack account has this email, so a direct message cannot go to it. Post in a channel instead.", nil
}

// parseLateHours reads how many hours late a report may be sent, as whole
// seconds from a minute to 30 days, which the store takes.
func parseLateHours(raw string) (int, bool) {
	hours, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(hours) || hours <= 0 || hours > reportLateMaxHours {
		return 0, false
	}
	seconds := int(math.Round(hours * 3600))
	return seconds, seconds >= 60
}

// formatLateHours writes limit as hours with the fewest decimals that read
// back as the same seconds, so saving a report unchanged keeps it.
func formatLateHours(limit time.Duration) string {
	seconds := int(limit / time.Second)
	for decimals := 0; decimals < 6; decimals++ {
		text := strconv.FormatFloat(limit.Hours(), 'f', decimals, 64)
		if parsed, ok := parseLateHours(text); ok && parsed == seconds {
			return text
		}
	}
	return strconv.FormatFloat(limit.Hours(), 'f', -1, 64)
}

// parseReportTime reads a time of day: "09:30", "9:30", "9:30 am".
func parseReportTime(raw string) (hour, minute int, ok bool) {
	compact := strings.ToUpper(strings.Join(strings.Fields(raw), ""))
	for _, layout := range []string{"15:04", "15:04:05", "3:04PM", "3PM"} {
		if parsed, err := time.Parse(layout, compact); err == nil {
			return parsed.Hour(), parsed.Minute(), true
		}
	}
	return 0, 0, false
}

// reportZone is a time zone the form suggests, and how a schedule names it.
type reportZone struct {
	Name, Label string
}

// reportZones are the time zones the form suggests. Any IANA zone works.
var reportZones = []reportZone{
	{"America/Los_Angeles", "Pacific time"},
	{"America/Denver", "Mountain time"},
	{"America/Phoenix", "Arizona time"},
	{"America/Chicago", "Central time"},
	{"America/New_York", "Eastern time"},
	{"America/Anchorage", "Alaska time"},
	{"Pacific/Honolulu", "Hawaii time"},
	{"America/Toronto", "Toronto time"},
	{"America/Mexico_City", "Mexico City time"},
	{"America/Sao_Paulo", "São Paulo time"},
	{"Europe/London", "London time"},
	{"Europe/Dublin", "Dublin time"},
	{"Europe/Lisbon", "Lisbon time"},
	{"Europe/Paris", "Paris time"},
	{"Europe/Berlin", "Berlin time"},
	{"Europe/Amsterdam", "Amsterdam time"},
	{"Europe/Madrid", "Madrid time"},
	{"Europe/Stockholm", "Stockholm time"},
	{"Europe/Warsaw", "Warsaw time"},
	{"Europe/Kyiv", "Kyiv time"},
	{"Africa/Lagos", "Lagos time"},
	{"Africa/Johannesburg", "Johannesburg time"},
	{"Asia/Dubai", "Dubai time"},
	{"Asia/Kolkata", "India time"},
	{"Asia/Singapore", "Singapore time"},
	{"Asia/Shanghai", "China time"},
	{"Asia/Tokyo", "Tokyo time"},
	{"Asia/Seoul", "Seoul time"},
	{"Australia/Sydney", "Sydney time"},
	{"Pacific/Auckland", "Auckland time"},
	{"UTC", "UTC"},
}

// TimeZones are the time zones the form suggests.
func (reportFormData) TimeZones() []reportZone { return reportZones }

// zoneLabel names a time zone in a schedule: "Pacific time", "Lisbon time",
// "UTC".
func zoneLabel(zone string) string {
	for _, known := range reportZones {
		if known.Name == zone {
			return known.Label
		}
	}
	if zone == "" || zone == "Etc/UTC" {
		return "UTC"
	}
	city := zone[strings.LastIndex(zone, "/")+1:]
	return strings.ReplaceAll(city, "_", " ") + " time"
}

// resolveReportZone reads a time zone: an IANA name, or a label the form
// suggests ("Pacific time", "pacific").
func resolveReportZone(raw string) (string, bool) {
	key := strings.TrimSuffix(strings.ToLower(raw), " time")
	for _, known := range reportZones {
		if strings.EqualFold(known.Name, raw) || strings.TrimSuffix(strings.ToLower(known.Label), " time") == key {
			return known.Name, true
		}
	}
	return raw, validReportZone(raw)
}

// validReportZone reports whether zone is an IANA time zone. "Local", the
// server's own zone, is not one an operator can see.
func validReportZone(zone string) bool {
	if zone == "" || zone == "Local" {
		return false
	}
	_, err := time.LoadLocation(zone)
	return err == nil
}

var weekdayNames = []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// reportWeekday is a day a weekly report can run on.
type reportWeekday struct {
	Value, Name string
}

// Weekdays are the days a weekly report can run on.
func (reportFormData) Weekdays() []reportWeekday {
	days := make([]reportWeekday, len(weekdayNames))
	for index, name := range weekdayNames {
		days[index] = reportWeekday{Value: strconv.Itoa(index + 1), Name: name}
	}
	return days
}

// scheduleWords says a schedule: "Weekdays at 9:30 AM Pacific time".
func scheduleWords(cadence string, weekday *int, hour, minute int, zone string) string {
	var when string
	switch cadence {
	case scheduledreport.CadenceDaily:
		when = "Every day"
	case scheduledreport.CadenceWeekdays:
		when = "Weekdays"
	case scheduledreport.CadenceWeekly:
		when = "Every week"
		if weekday != nil && *weekday >= 1 && *weekday <= 7 {
			when = "Every " + weekdayNames[*weekday-1]
		}
	default:
		when = "Unscheduled"
	}
	clock := time.Date(2000, 1, 1, hour, minute, 0, 0, time.UTC).Format("3:04 PM")
	return when + " at " + clock + " " + zoneLabel(zone)
}

// lateWords says how late a report may be sent: "6 hours", "90 minutes".
func lateWords(limit time.Duration) string {
	switch {
	case limit >= time.Hour && limit%time.Hour == 0:
		return plural(int(limit/time.Hour), "hour")
	case limit%time.Minute == 0:
		return plural(int(limit/time.Minute), "minute")
	}
	return formatDuration(limit)
}

// reportExample is a built-in report to start from.
type reportExample struct {
	Key     string
	Title   string
	Summary string
	Prompt  string
}

// reportExampleWords title and describe the built-in templates by name.
// One missing here still lists, under its name.
var reportExampleWords = map[string][2]string{
	"individual-daily": {"Daily standup", "one person's work on the previous working day, best sent to them as a direct message"},
	"team-weekly":      {"Weekly team update", "what a team shipped, decided, and is working on over a week, best posted in its channel"},
}

// Examples are the built-in reports a new one can start from.
func (reportFormData) Examples() []reportExample {
	var examples []reportExample
	for _, name := range scheduledreport.TemplateNames() {
		if example, ok := reportExampleNamed(name); ok {
			examples = append(examples, example)
		}
	}
	return examples
}

func reportExampleNamed(name string) (reportExample, bool) {
	prompt, err := scheduledreport.Template(name)
	if err != nil {
		return reportExample{}, false
	}
	example := reportExample{Key: name, Title: name, Prompt: prompt}
	if words, ok := reportExampleWords[name]; ok {
		example.Title, example.Summary = words[0], words[1]
	}
	return example, true
}
