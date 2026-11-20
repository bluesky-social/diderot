package adminui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/adminaction"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/scheduledreport"
	"github.com/bluesky-social/diderot/internal/settings"
)

var (
	reportsPage    = mustPage("reports.html", "report_parts.html")
	reportPage     = mustPage("report.html", "report_parts.html")
	reportFormPage = mustPage("report_form.html", "report_parts.html")
)

// reportRunLimit is how many of a report's runs its page lists.
const reportRunLimit = 50

// reportRefreshSeconds is how often a report's page reloads while a run is
// in progress.
const reportRefreshSeconds = 15

type ScheduledReportsStore interface {
	Create(context.Context, scheduledreport.CreateParams) (*scheduledreport.Report, error)
	List(context.Context) ([]scheduledreport.Report, error)
	Get(context.Context, uuid.UUID) (*scheduledreport.Report, error)
	Update(context.Context, uuid.UUID, scheduledreport.CreateParams) (*scheduledreport.Report, error)
	SetEnabled(context.Context, uuid.UUID, bool) (*scheduledreport.Report, error)
	Delete(context.Context, uuid.UUID) (bool, error)
	EnqueueNow(context.Context, uuid.UUID) (*scheduledreport.Run, error)
	ListRuns(context.Context, uuid.UUID, int) ([]scheduledreport.Run, error)
	LatestRuns(context.Context) (map[uuid.UUID]scheduledreport.Run, error)
}

// reportsData is the list of reports.
type reportsData struct {
	pageData
	Reports []reportRow
	// SlackOn is whether the Slack connector is on. Reports post there, so
	// none can be made or delivered without it.
	SlackOn bool
	Notice  string
	Error   string
}

type reportRow struct {
	ID       string
	Name     string
	Schedule string
	Target   reportTarget
	Enabled  bool
	NextRun  time.Time
	// NextNote says why NextRun is not a time to show, when it is not.
	NextNote string
	// Last is the newest run, nil before the first.
	Last *reportRunView
}

// reportTarget is where a report goes.
type reportTarget struct {
	// DM is a direct message to the owner; otherwise a channel post.
	DM bool
	// Name is "#channel" or the person's name, empty when the Slack
	// directory does not know ID.
	Name string
	ID   string
}

// reportData is one report and its runs.
type reportData struct {
	pageData
	Report reportView
	Runs   []reportRunView
	// MoreRuns is set when the report has older runs than the page lists.
	MoreRuns  bool
	SlackOn   bool
	CSRFToken string
	Notice    string
	Error     string
}

// ReportTab is the report page tab this is.
func (reportData) ReportTab() string { return "runs" }

// Active reports whether a run is waiting or in progress and can move on,
// so the page is worth reloading. Nothing moves while Slack is off or
// Diderot is still being set up.
func (d reportData) Active() bool {
	if !d.SlackOn || d.InSetup {
		return false
	}
	for _, run := range d.Runs {
		if run.Tone == toneAttention {
			return true
		}
	}
	return false
}

// RefreshSeconds is how often the page reloads while a run is active.
func (reportData) RefreshSeconds() int { return reportRefreshSeconds }

type reportView struct {
	ID       string
	Name     string
	Prompt   string
	Schedule string
	Target   reportTarget
	Owner    string
	Enabled  bool
	NextRun  time.Time
	// NextNote says why NextRun is not a time to show, when it is not.
	NextNote string
	// LateLimit is how late a run may still be sent after Diderot was down.
	LateLimit     time.Duration
	LastDelivered *time.Time
	CreatedBy     string
	CreatedAt     time.Time
}

// LateWords says LateLimit in words: "6 hours", "90 minutes".
func (v reportView) LateWords() string { return lateWords(v.LateLimit) }

// reportRunView is one run as the history lists it.
type reportRunView struct {
	At       time.Time
	Manual   bool
	Attempts int
	// Label and Tone are the run's badge.
	Label string
	Tone  string
	// Reason says why a run failed or was skipped, in words. Error is
	// what was recorded, when Reason does not repeat it.
	Reason     string
	Error      string
	Took       time.Duration
	SessionURL string
	SlackURL   string
}

func (s *Server) handleReports(w http.ResponseWriter, r *http.Request) {
	if !s.requireReports(w) {
		return
	}
	ctx := r.Context()
	reports, err := s.reports.List(ctx)
	if err != nil {
		s.reportPageError(w, "report list", err)
		return
	}
	latest, err := s.reports.LatestRuns(ctx)
	if err != nil {
		s.reportPageError(w, "latest report runs", err)
		return
	}
	names, err := s.reportTargetNames(ctx, reports...)
	if err != nil {
		s.reportPageError(w, "report target names", err)
		return
	}
	data := reportsData{
		pageData: s.page(r, "Reports", navReports), SlackOn: s.slackOn(ctx),
		Notice: cleanNotice(r.URL.Query().Get("notice")), Error: cleanNotice(r.URL.Query().Get("error")),
	}
	data.Stylesheet = "reports.css"
	host := s.slackWorkspaceHost()
	for _, report := range reports {
		view := newReportView(report, names)
		row := reportRow{
			ID: view.ID, Name: view.Name, Schedule: view.Schedule, Target: view.Target,
			Enabled: view.Enabled, NextRun: view.NextRun,
			NextNote: reportNextNote(view.NextRun, data.SlackOn, data.InSetup),
		}
		if run, ok := latest[report.ID]; ok {
			last := newReportRunView(run, report, host)
			row.Last = &last
		}
		data.Reports = append(data.Reports, row)
	}
	s.render(w, reportsPage, data)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	report, ok := s.loadReport(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	runs, err := s.reports.ListRuns(ctx, report.ID, reportRunLimit+1)
	if err != nil {
		s.reportPageError(w, "report runs", err)
		return
	}
	names, err := s.reportTargetNames(ctx, *report)
	if err != nil {
		s.reportPageError(w, "report target names", err)
		return
	}
	csrfToken, err := issueCSRFToken(w, r)
	if err != nil {
		s.reportPageError(w, "CSRF token", err)
		return
	}
	data := reportData{
		pageData: s.page(r, report.Name, navReports), Report: newReportView(*report, names),
		SlackOn: s.slackOn(ctx), CSRFToken: csrfToken,
		Notice: cleanNotice(r.URL.Query().Get("notice")), Error: cleanNotice(r.URL.Query().Get("error")),
	}
	data.Stylesheet, data.Script = "reports.css", "reports.js"
	data.Report.NextNote = reportNextNote(data.Report.NextRun, data.SlackOn, data.InSetup)
	if len(runs) > reportRunLimit {
		runs, data.MoreRuns = runs[:reportRunLimit], true
	}
	host := s.slackWorkspaceHost()
	for _, run := range runs {
		data.Runs = append(data.Runs, newReportRunView(run, *report, host))
	}
	s.render(w, reportPage, data)
}

func (s *Server) handleReportNew(w http.ResponseWriter, r *http.Request) {
	if !s.requireReports(w) {
		return
	}
	principal, _ := PrincipalFromContext(r.Context())
	form := s.newReportForm(r.Context(), principal.Email)
	if name := strings.TrimSpace(r.URL.Query().Get("template")); name != "" {
		if example, ok := reportExampleNamed(name); ok {
			form.Prompt = example.Prompt
			if form.Name == "" {
				form.Name = example.Title
			}
		}
	}
	s.renderReportForm(w, r, nil, form, "", http.StatusOK)
}

func (s *Server) handleReportCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireReports(w) || !s.requireMutation(w, r) {
		return
	}
	form := reportFormFromRequest(r)
	// (b) Reports are delivered in Slack: the page offers no form while it
	// is off, and a form posted anyway is refused.
	if !s.slackOn(r.Context()) {
		reportRedirect(w, r, "/admin/reports", "", "The report was not created: reports are delivered in Slack, and the Slack connector is off. Turn it on first.")
		return
	}
	params, err := s.resolveReportForm(r.Context(), &form, nil, reportForm{})
	if err != nil {
		s.reportPageError(w, "report form", err)
		return
	}
	if len(form.Errors) > 0 {
		s.renderReportForm(w, r, nil, form, "The report was not created. Fix what is marked below.", http.StatusUnprocessableEntity)
		return
	}
	params.Enabled = &form.Enabled
	report, err := adminaction.CreateReport(r.Context(), s.audit(r), s.reports, params)
	if s.auditFailed(w, err) {
		return
	}
	if duplicateReportName(err) {
		form.fail("name", reportNameTaken)
		s.renderReportForm(w, r, nil, form, "The report was not created. Fix what is marked below.", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.logger.Warn("admin report create failed", "name", params.Name, "err", err)
		s.renderReportForm(w, r, nil, form, reportActionProblem(err, "The report was not created."), http.StatusUnprocessableEntity)
		return
	}
	reportRedirect(w, r, reportPath(report.ID), "Report created. Run it now to see what it sends.", "")
}

func (s *Server) handleReportEdit(w http.ResponseWriter, r *http.Request) {
	report, ok := s.loadReport(w, r)
	if !ok {
		return
	}
	names, err := s.reportTargetNames(r.Context(), *report)
	if err != nil {
		s.reportPageError(w, "report target names", err)
		return
	}
	saved := newReportView(*report, names)
	s.renderReportForm(w, r, &saved, reportFormFromReport(*report, names), cleanNotice(r.URL.Query().Get("error")), http.StatusOK)
}

func (s *Server) handleReportUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireReports(w) || !s.requireMutation(w, r) {
		return
	}
	report, ok := s.loadReport(w, r)
	if !ok {
		return
	}
	names, err := s.reportTargetNames(r.Context(), *report)
	if err != nil {
		s.reportPageError(w, "report target names", err)
		return
	}
	saved := newReportView(*report, names)
	form := reportFormFromRequest(r)
	params, err := s.resolveReportForm(r.Context(), &form, report, reportFormFromReport(*report, names))
	if err != nil {
		s.reportPageError(w, "report form", err)
		return
	}
	if len(form.Errors) > 0 {
		s.renderReportForm(w, r, &saved, form, "The changes were not saved. Fix what is marked below.", http.StatusUnprocessableEntity)
		return
	}
	_, err = adminaction.UpdateReport(r.Context(), s.audit(r), s.reports, report.ID, params)
	if s.auditFailed(w, err) {
		return
	}
	if duplicateReportName(err) {
		form.fail("name", reportNameTaken)
		s.renderReportForm(w, r, &saved, form, "The changes were not saved. Fix what is marked below.", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.logger.Warn("admin report update failed", "report_id", report.ID, "err", err)
		s.renderReportForm(w, r, &saved, form, reportActionProblem(err, "The changes were not saved."), http.StatusUnprocessableEntity)
		return
	}
	reportRedirect(w, r, reportPath(report.ID), "Changes saved.", "")
}

func (s *Server) handleReportEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := s.reportMutationID(w, r)
	if !ok {
		return
	}
	enabled, err := strconv.ParseBool(r.Form.Get("enabled"))
	if err != nil {
		http.Error(w, "invalid enabled state", http.StatusBadRequest)
		return
	}
	_, err = adminaction.SetReportEnabled(r.Context(), s.audit(r), s.reports, id, enabled)
	if s.auditFailed(w, err) {
		return
	}
	if err != nil {
		s.logger.Warn("admin report enable failed", "report_id", id, "enabled", enabled, "err", err)
		reportRedirect(w, r, reportPath(id), "", reportActionProblem(err, "The report was not turned "+onOff(enabled)+"."))
		return
	}
	notice := "Report turned off. It will not run on its schedule, and any run waiting to start was skipped. Run now still works."
	if enabled {
		notice = "Report turned on. It runs on its schedule again."
	}
	reportRedirect(w, r, reportPath(id), notice, "")
}

func (s *Server) handleReportRun(w http.ResponseWriter, r *http.Request) {
	id, ok := s.reportMutationID(w, r)
	if !ok {
		return
	}
	_, err := adminaction.RunReport(r.Context(), s.audit(r), s.reports, id)
	if s.auditFailed(w, err) {
		return
	}
	if err != nil {
		s.logger.Warn("admin report run failed", "report_id", id, "err", err)
		reportRedirect(w, r, reportPath(id), "", reportActionProblem(err, "The run was not queued."))
		return
	}
	notice := "Run queued. It is listed below and usually delivered within a few minutes."
	switch {
	case s.setup != nil && !s.setup.Org().Live():
		notice = "Run queued. It waits until Diderot goes live."
	case !s.slackOn(r.Context()):
		notice = "Run queued. It waits until the Slack connector is on."
	}
	reportRedirect(w, r, reportPath(id), notice, "")
}

func (s *Server) handleReportDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.reportMutationID(w, r)
	if !ok {
		return
	}
	if r.Form.Get("confirm") != "delete" {
		reportRedirect(w, r, reportPath(id)+"/edit", "", "The report was not deleted: the request did not confirm it.")
		return
	}
	name := "The report"
	if report, err := s.reports.Get(r.Context(), id); err == nil {
		name = `"` + report.Name + `"`
	}
	err := adminaction.DeleteReport(r.Context(), s.audit(r), s.reports, id)
	if s.auditFailed(w, err) {
		return
	}
	if err != nil {
		s.logger.Warn("admin report delete failed", "report_id", id, "err", err)
		reportRedirect(w, r, reportPath(id)+"/edit", "", reportActionProblem(err, "The report was not deleted."))
		return
	}
	reportRedirect(w, r, "/admin/reports", name+" and its run history were deleted.", "")
}

// loadReport reads the report the path names. For one that does not exist,
// a page load gets the not-found page, and a form sends the browser back to
// the list.
func (s *Server) loadReport(w http.ResponseWriter, r *http.Request) (*scheduledreport.Report, bool) {
	if !s.requireReports(w) {
		return nil, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.renderNotFound(w, r, missingReport)
		return nil, false
	}
	report, err := s.reports.Get(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		if browsing(r) {
			s.renderNotFound(w, r, missingReport)
		} else {
			reportRedirect(w, r, "/admin/reports", "", "That report no longer exists.")
		}
		return nil, false
	}
	if err != nil {
		s.reportPageError(w, "report", err)
		return nil, false
	}
	return report, true
}

func (s *Server) reportMutationID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	if !s.requireReports(w) || !s.requireMutation(w, r) {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) requireReports(w http.ResponseWriter) bool {
	if s.reports == nil {
		http.Error(w, "scheduled reports are unavailable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// slackOn reports whether the Slack connector is on. Without connector
// settings it assumes so.
func (s *Server) slackOn(ctx context.Context) bool {
	if s.connectors == nil {
		return true
	}
	view, err := s.connectors.View(ctx, slackcx.ConnectorName)
	if errors.Is(err, settings.ErrUnknownConnector) {
		return false
	}
	if err != nil {
		s.logger.Warn("admin reports: Slack connector state unavailable", "err", err)
		return true
	}
	return view.Enabled
}

// reportNextNote says what stands in for a report's next run when its time
// is not one to show: runs wait while Slack is off and during setup, and a
// time already past is a run about to start.
func reportNextNote(next time.Time, slackOn, inSetup bool) string {
	switch {
	case !slackOn:
		return "Waiting for Slack"
	case inSetup:
		return "Waiting for setup"
	case !next.After(time.Now()):
		return "Due now"
	}
	return ""
}

func newReportView(report scheduledreport.Report, names map[string]string) reportView {
	view := reportView{
		ID: report.ID.String(), Name: report.Name, Prompt: report.Prompt,
		Schedule: scheduleWords(report.Cadence, report.Weekday, report.ScheduleHour, report.ScheduleMinute, report.Timezone),
		Target: reportTarget{
			DM: report.DeliveryKind == scheduledreport.DeliverySlackDM,
			ID: report.SlackTarget, Name: names[report.SlackTarget],
		},
		Owner: report.OwnerEmail, Enabled: report.Enabled, NextRun: report.NextRunAt,
		LateLimit: report.CatchupWindow, LastDelivered: report.LastSuccessAt,
		CreatedBy: report.CreatedBy, CreatedAt: report.CreatedAt,
	}
	return view
}

func newReportRunView(run scheduledreport.Run, report scheduledreport.Report, slackHost string) reportRunView {
	view := reportRunView{At: run.ScheduledFor, Manual: run.Trigger == "manual", Attempts: run.Attempts}
	view.Label, view.Tone, view.Reason, view.Error = reportRunState(run)
	if run.FinishedAt != nil && run.Status != "skipped" {
		view.Took = run.FinishedAt.Sub(run.CreatedAt)
	}
	if run.AskSessionID != nil {
		view.SessionURL = "/admin/ask/sessions/" + run.AskSessionID.String()
	}
	kind, channel := report.DeliveryKind, report.SlackTarget
	if run.DeliveryKind != nil && run.SlackTarget != nil {
		kind, channel = *run.DeliveryKind, *run.SlackTarget
	}
	if run.DeliveryTS != nil && *run.DeliveryTS != "" && slackHost != "" && kind == scheduledreport.DeliverySlackChannel {
		view.SlackURL = (&url.URL{
			Scheme: "https", Host: slackHost,
			Path: "/archives/" + channel + "/p" + strings.Replace(*run.DeliveryTS, ".", "", 1),
		}).String()
	}
	return view
}

// reportRunState is a run's badge, and why it failed or was skipped.
func reportRunState(run scheduledreport.Run) (label, tone, reason, recorded string) {
	if run.Error != nil {
		recorded = strings.TrimSpace(*run.Error)
	}
	switch run.Status {
	case "succeeded":
		return "delivered", toneGood, "", ""
	case "running":
		return "running", toneAttention, "", ""
	case "pending":
		if recorded == "" {
			return "queued", toneAttention, "", ""
		}
		label, tone = "retrying", toneAttention
	case "generated":
		// Written, and waiting to be sent or sent again.
		label, tone = "sending", toneAttention
	case "failed":
		label, tone = "failed", toneBad
	case "skipped":
		reason, _ = reportFailureWords(recorded)
		return "skipped", toneNeutral, reason, ""
	default:
		label, tone = run.Status, toneNeutral
	}
	if recorded == "" {
		return label, tone, "", ""
	}
	reason, paraphrased := reportFailureWords(recorded)
	if !paraphrased {
		recorded = ""
	}
	return label, tone, reason, recorded
}

// reportFailures say in words the failures a run records. The first whose
// text the record contains wins.
var reportFailures = []struct{ contains, words string }{
	{"report disabled", "The report was turned off before this run started."},
	{"outside catch-up window", "Diderot was down for longer than the report may be sent late, so this run was skipped."},
	{"not_in_channel", "Diderot is not in the channel. Add it to the channel in Slack."},
	{"channel_not_found", "Slack could not find the channel. It may have been deleted, or made private without Diderot in it."},
	{"is_archived", "The channel is archived."},
	{"does not match owner", "The owner's email no longer matches the Slack account the report was addressed to. Edit the report and save it again."},
	{"no authorized corpus access", "The channel may not see anything Diderot has read, so there was nothing to report from."},
	{"empty report", "Ask returned an empty answer."},
	{"Slack connector is off", "The Slack connector was off."},
	{"resolver is not configured", "The Slack connector was not running."},
	{"no saved delivery audience", "This run cannot be sent. Run the report again."},
	{"context canceled", "Diderot stopped while working on the run."},
	{"deadline exceeded", "The run took too long."},
}

// reportFailureWords says a recorded failure in words. paraphrased is false
// when it has no words for it and returns the record itself, shortened.
func reportFailureWords(recorded string) (words string, paraphrased bool) {
	if recorded == "" {
		return "", false
	}
	for _, failure := range reportFailures {
		if strings.Contains(recorded, failure.contains) {
			return failure.words, true
		}
	}
	first, _, _ := strings.Cut(recorded, "\n")
	return compactText(first, 240), false
}

// reportTargetNames names the reports' Slack targets from the Slack
// directory: "#name" for a channel, a person's name for a user.
func (s *Server) reportTargetNames(ctx context.Context, reports ...scheduledreport.Report) (map[string]string, error) {
	var channels, users []string
	for _, report := range reports {
		if report.DeliveryKind == scheduledreport.DeliverySlackDM {
			users = append(users, report.SlackTarget)
		} else {
			channels = append(channels, report.SlackTarget)
		}
	}
	names := map[string]string{}
	if len(channels) > 0 {
		if err := s.collectNames(ctx, names, "#", `
			SELECT channel_id, name FROM slack_channel_info
			 WHERE channel_id = ANY($1) AND COALESCE(name, '') <> ''`, channels); err != nil {
			return nil, err
		}
	}
	if len(users) > 0 {
		if err := s.collectNames(ctx, names, "", `
			SELECT user_id, `+slackPersonName+` FROM slack_users
			 WHERE user_id = ANY($1) AND `+slackPersonName+` <> ''`, users); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// slackPersonName is a Slack user's name as people know them.
const slackPersonName = `COALESCE(NULLIF(real_name, ''), NULLIF(display_name, ''), NULLIF(name, ''), email, '')`

func (s *Server) collectNames(ctx context.Context, names map[string]string, prefix, statement string, ids []string) error {
	rows, err := s.pool.Query(ctx, statement, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		names[id] = prefix + name
	}
	return rows.Err()
}

func reportPath(id uuid.UUID) string { return "/admin/reports/" + id.String() }

// reportActionProblem says why an action on a report did not happen.
func reportActionProblem(err error, what string) string {
	if errors.Is(err, pgx.ErrNoRows) {
		return what + " That report no longer exists."
	}
	return what + " Try again; if it fails again, the audit log has the error."
}

// reportRedirect sends the browser to path with the result of what it did.
func reportRedirect(w http.ResponseWriter, r *http.Request, path, notice, problem string) {
	query := url.Values{}
	if notice != "" {
		query.Set("notice", cleanNotice(notice))
	}
	if problem != "" {
		query.Set("error", cleanNotice(problem))
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (s *Server) reportPageError(w http.ResponseWriter, operation string, err error) {
	s.logger.Error("admin reports failed", "operation", operation, "err", err)
	http.Error(w, operation+" failed", http.StatusInternalServerError)
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
