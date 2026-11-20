package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/adminaction"
	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/adminjob"
	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/reembed"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

const jobsPageSize = 20

var (
	operationsPage = mustPage("operations.html")
	jobsPage       = mustPage("operations_jobs.html")
)

// statusData is Operations > Status.
type statusData struct {
	pageData
	Work []workRow
	// Sources are the sources that are on; Off counts the others.
	Sources []statusSource
	Off     int
}

type statusSource struct {
	sourceHealth
	Newest *time.Time
}

// Cells are the source's State and Newest item cells, as the Connectors
// list shows them.
func (s statusSource) Cells() sourceCells {
	return sourceCells{State: s.state(), Newest: s.Newest}
}

// sourceCells is what the source-state and source-newest partials draw.
type sourceCells struct {
	State      connectorState
	NeverSetUp bool
	Newest     *time.Time
}

// jobsData is Operations > Jobs and maintenance.
type jobsData struct {
	pageData
	CSRFToken string
	Notice    string
	Error     string
	// Summaries is the session summaries control, shown while agent
	// sessions reach this install.
	Summaries summaryState
	// Schedule is when sessions are summarized, shown with Summaries.
	Schedule *summaryScheduleForm
	// Reembed is the re-embed waiting or running, nil when none is.
	Reembed *jobRow
	// ReembedSources are the sources a re-embed can rebuild.
	ReembedSources []reembedSource
	// Documents is the rebuild of what Diderot read from Slack documents,
	// nil where Slack is not a source.
	Documents *documentRebuild
	// Images is the control that describes images again, nil while image
	// captions are off.
	Images *imageCaptions
	// SummaryRepair is the control that repairs session summaries, nil while
	// none needs it.
	SummaryRepair *summaryRepair
	Jobs          []jobRow
	Pages         pagination
	// Active says a job is waiting or running, so the page refreshes itself.
	Active bool
}

// reembedSource is a source a re-embed can rebuild. It can only while the
// source is on: rebuilding an item asks the source about it.
type reembedSource struct {
	// Name is the source's event source, which the form posts.
	Name  string
	Title string
	On    bool
}

// ReembedOffered reports whether a re-embed can be requested: some source
// it rebuilds is on.
func (d jobsData) ReembedOffered() bool {
	return slices.ContainsFunc(d.ReembedSources, func(src reembedSource) bool { return src.On })
}

// documentRebuild is the control that reads every channel's documents
// again.
type documentRebuild struct {
	SlackOn bool
	// Channels counts the channels with document indexing on.
	Channels int64
	// Running is the rebuild waiting or running, nil when none is.
	Running *jobRow
}

// imageCaptions is the control that has the current model describe the
// images an earlier model described.
type imageCaptions struct {
	// Model is the model that describes images now.
	Model string
	// Earlier counts the images another model described.
	Earlier int64
	// Waiting counts the images waiting to be described.
	Waiting int64
}

// summaryRepair is the control that repairs session summaries the summary
// model wrote with part of its answer in the wrong place.
type summaryRepair struct {
	// FromText counts the summaries restored from their own text.
	FromText int64
	// Resummarize counts the summaries written again by the reduce model.
	Resummarize int64
	// Tokens estimates what those calls read.
	Tokens int64
	// Running is the repair waiting or running, nil when none is.
	Running *jobRow
}

// Total counts the summaries a repair would change.
func (r summaryRepair) Total() int64 { return r.FromText + r.Resummarize }

// jobRow is one long-running administrator job.
type jobRow struct {
	ID          int64
	Kind        string
	Name        string
	Status      string
	Tone        string
	Active      bool
	RequestedBy string
	RequestedAt time.Time
	// Took is how long it ran, or has run so far; zero before it starts.
	Took     time.Duration
	Progress string
	Error    string

	progress  []byte
	request   []byte
	channelID string
}

func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	health, err := s.loadHealth(r.Context())
	if err != nil {
		s.logger.Error("admin operations status query failed", "err", err)
		http.Error(w, "operations query failed", http.StatusInternalServerError)
		return
	}
	data := statusData{Work: health.Work}
	var on []string
	for _, src := range health.Sources {
		if !src.Enabled {
			data.Off++
			continue
		}
		on = append(on, src.EventSource)
		data.Sources = append(data.Sources, statusSource{sourceHealth: src})
	}
	newest, err := s.newestItems(r.Context(), on)
	if err != nil {
		s.logger.Error("admin newest items failed", "err", err)
		http.Error(w, "operations query failed", http.StatusInternalServerError)
		return
	}
	for i, src := range data.Sources {
		data.Sources[i].Newest = newest[src.EventSource]
	}
	data.pageData = s.page(r, "Status", navOperations)
	data.CurrentTab = tabOperationsStatus
	data.Stylesheet = "operations.css"
	s.render(w, operationsPage, data)
}

func (s *Server) handleOperationsJobs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	data := jobsData{Notice: cleanNotice(query.Get("notice")), Error: cleanNotice(query.Get("error"))}
	var cursor *auditCursor
	if raw := strings.TrimSpace(query.Get("before")); raw != "" {
		if parsed, ok := parseAuditCursor(raw); ok {
			cursor = &parsed
		}
	}
	var err error
	if data.Summaries, err = s.loadSummaryState(r.Context()); err != nil {
		s.logger.Error("session summaries query failed", "err", err)
		http.Error(w, "operations query failed", http.StatusInternalServerError)
		return
	}
	if data.Summaries.Relevant() {
		if data.Schedule, err = s.loadSummarySchedule(r.Context()); err != nil {
			s.logger.Error("session summary schedule query failed", "err", err)
			http.Error(w, "operations query failed", http.StatusInternalServerError)
			return
		}
	}
	var more bool
	if data.Jobs, more, err = s.loadJobs(r.Context(), cursor); err != nil {
		s.logger.Error("admin jobs query failed", "err", err)
		http.Error(w, "operations query failed", http.StatusInternalServerError)
		return
	}
	if data.Reembed, err = s.activeJob(r.Context(), reembed.JobKind); err == nil {
		data.ReembedSources, err = s.loadReembedSources(r.Context())
	}
	if err == nil {
		data.Documents, err = s.loadDocumentRebuild(r.Context())
	}
	if err == nil {
		data.Images, err = s.loadImageCaptions(r.Context())
	}
	if err == nil {
		data.SummaryRepair, err = s.loadSummaryRepair(r.Context())
	}
	if err != nil {
		s.logger.Error("admin jobs query failed", "err", err)
		http.Error(w, "operations query failed", http.StatusInternalServerError)
		return
	}
	data.Active = data.Reembed != nil || (data.Documents != nil && data.Documents.Running != nil) ||
		(data.SummaryRepair != nil && data.SummaryRepair.Running != nil)
	for _, job := range data.Jobs {
		data.Active = data.Active || job.Active
	}
	data.Pages = pagination{Summary: plural(len(data.Jobs), "job") + ", newest first"}
	if cursor != nil {
		data.Pages.BackURL = jobsPath
	}
	if more {
		last := data.Jobs[len(data.Jobs)-1]
		data.Pages.ForwardURL = "?" + url.Values{"before": {formatAuditCursor(last.RequestedAt, last.ID)}}.Encode()
	}
	if data.CSRFToken, err = issueCSRFToken(w, r); err != nil {
		s.logger.Error("admin jobs CSRF token failed", "err", err)
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	data.pageData = s.page(r, "Jobs and maintenance", navOperations)
	data.CurrentTab = tabOperationsJobs
	data.Script = "operations.js"
	s.render(w, jobsPage, data)
}

// summaryScheduleForm is when sessions are summarized, as the Jobs and
// maintenance page shows it: each wait in words.
type summaryScheduleForm struct {
	// Version is the row's version, which a save posts back.
	Version                 int64
	QuietFor, MaxPendingFor string
	MinAddedUserTurns       int
	MinAddedRecords         int
	MinAddedBytes           int64
	UpdatedBy               string
	UpdatedAt               time.Time
}

// loadSummarySchedule reads when sessions are summarized, nil on a server
// without settings.
func (s *Server) loadSummarySchedule(ctx context.Context) (*summaryScheduleForm, error) {
	if s.setup == nil {
		return nil, nil
	}
	row, err := s.setup.SessionSummaries(ctx)
	if err != nil {
		return nil, err
	}
	schedule := row.EffectiveSchedule()
	return &summaryScheduleForm{
		Version:  row.Version,
		QuietFor: settings.FormatDuration(time.Duration(schedule.QuietFor)), MaxPendingFor: settings.FormatDuration(time.Duration(schedule.MaxPendingFor)),
		MinAddedUserTurns: schedule.MinAddedUserTurns, MinAddedRecords: schedule.MinAddedRecords, MinAddedBytes: schedule.MinAddedBytes,
		UpdatedBy: row.UpdatedBy, UpdatedAt: row.UpdatedAt,
	}, nil
}

// parseSummarySchedule reads the schedule form. Its messages name the field
// as the page labels it.
func parseSummarySchedule(form url.Values) (settings.SummarySchedule, error) {
	var schedule settings.SummarySchedule
	for _, wait := range []struct {
		field, label string
		into         *settings.Duration
	}{
		{"quiet_for", "the quiet time", &schedule.QuietFor},
		{"max_pending_for", "the longest wait", &schedule.MaxPendingFor},
	} {
		d, err := settings.ParseDuration(form.Get(wait.field))
		if err != nil {
			return schedule, fmt.Errorf("%w: %s must be a length of time, such as 10 minutes or 6 hours", settings.ErrInvalid, wait.label)
		}
		*wait.into = settings.Duration(d)
	}
	amount := func(field, label string) (int64, error) {
		n, err := strconv.ParseInt(strings.TrimSpace(form.Get(field)), 10, 32)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%w: %s must be a whole number, 0 or more", settings.ErrInvalid, label)
		}
		return n, nil
	}
	turns, err := amount("min_added_user_turns", "new messages from the person")
	if err != nil {
		return schedule, err
	}
	records, err := amount("min_added_records", "new entries")
	if err != nil {
		return schedule, err
	}
	if schedule.MinAddedBytes, err = amount("min_added_bytes", "new bytes"); err != nil {
		return schedule, err
	}
	schedule.MinAddedUserTurns, schedule.MinAddedRecords = int(turns), int(records)
	return schedule, nil
}

func (s *Server) handleOperationsSummarySchedule(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetup(w) || !s.requireMutation(w, r) {
		return
	}
	version, err := strconv.ParseInt(r.Form.Get("version"), 10, 64)
	if err != nil {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	schedule, err := parseSummarySchedule(r.Form)
	if err == nil {
		_, err = s.setup.SaveSummarySchedule(r.Context(), s.setupActor(r), version, schedule)
	}
	notice, ok := setupNotice(err)
	if !ok {
		s.logger.Error("session summary schedule save failed", "err", err)
		http.Error(w, "session summary schedule save failed", http.StatusInternalServerError)
		return
	}
	if err != nil {
		redirectJobs(w, r, "error", "When sessions are summarized was not changed: "+notice)
		return
	}
	redirectJobs(w, r, "notice", "When sessions are summarized was saved. It applies within a minute.")
}

// sessionSummaries reads whether agent-session summaries run, and if not,
// why.
func (s *Server) sessionSummaries(ctx context.Context) (sessioningest.SummaryControlState, error) {
	control := s.summaryControl
	if control == nil {
		control = sessioningest.NewSummaryControl(sessioningest.NewStore(s.pool), nil)
	}
	return control.State(ctx)
}

func (s *Server) handleOperationsSessionSummarization(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	if s.summaryControl == nil {
		http.Error(w, "session summarization control unavailable", http.StatusServiceUnavailable)
		return
	}
	enabled := r.Form.Get("enabled") == "true"
	if r.Form.Get("enabled") != "true" && r.Form.Get("enabled") != "false" {
		http.Error(w, "enabled must be true or false", http.StatusBadRequest)
		return
	}
	err := adminaction.SetSessionSummarization(r.Context(), s.audit(r), s.summaryControl, enabled)
	if s.auditFailed(w, err) {
		return
	}
	if err != nil {
		s.logger.Warn("session summarization control update failed", "err", err)
		redirectJobs(w, r, "error", "Session summaries were not changed: "+err.Error())
		return
	}
	notice := "Session summaries paused."
	if enabled {
		notice = "Session summaries resumed."
	}
	redirectJobs(w, r, "notice", notice)
}

func (s *Server) handleOperationsReembed(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	if r.Form.Get("confirm") != "reembed" {
		redirectJobs(w, r, "error", "Nothing was started: tick the box to confirm the re-embed.")
		return
	}
	offered, err := s.loadReembedSources(r.Context())
	if err != nil {
		s.logger.Error("admin jobs query failed", "err", err)
		http.Error(w, "operations query failed", http.StatusInternalServerError)
		return
	}
	sources, problem := chosenReembedSources(offered, r.Form["source"])
	if problem != "" {
		redirectJobs(w, r, "error", "Nothing was started: "+problem)
		return
	}
	_, err = adminaction.StartReembed(r.Context(), s.audit(r), s.reembed, sources)
	if s.auditFailed(w, err) {
		return
	}
	if errors.Is(err, adminjob.ErrActive) {
		redirectJobs(w, r, "notice", "A re-embed is already waiting or running.")
		return
	}
	if err != nil {
		s.logger.Warn("admin UI re-embed start failed", "err", err)
		redirectJobs(w, r, "error", "The re-embed was not requested: "+err.Error())
		return
	}
	redirectJobs(w, r, "notice", "Re-embed requested. It starts within a few seconds.")
}

// loadReembedSources are the sources a re-embed can rebuild, and whether
// each is on.
func (s *Server) loadReembedSources(ctx context.Context) ([]reembedSource, error) {
	var sources []reembedSource
	for _, name := range s.reembedSources {
		src, ok := s.registries.Sources.Lookup(name)
		if !ok {
			continue
		}
		on, err := s.sourceOn(ctx, name)
		if err != nil {
			return nil, err
		}
		sources = append(sources, reembedSource{Name: name, Title: src.Info().Title, On: on})
	}
	return sources, nil
}

// chosenReembedSources checks the sources the form chose against those
// offered, and returns them as the job takes them: nil for every source a
// re-embed rebuilds. problem says why nothing can start.
func chosenReembedSources(offered []reembedSource, chosen []string) (sources []string, problem string) {
	if len(chosen) == 0 {
		return nil, "choose at least one source to re-embed."
	}
	for _, name := range chosen {
		index := slices.IndexFunc(offered, func(src reembedSource) bool { return src.Name == name })
		switch {
		case index < 0:
			return nil, "a re-embed cannot rebuild " + cleanNotice(name) + "."
		case !offered[index].On:
			return nil, offered[index].Title + " is off, and a source can be re-embedded only while it is on."
		case !slices.Contains(sources, name):
			sources = append(sources, name)
		}
	}
	if len(sources) == len(offered) {
		return nil, ""
	}
	return sources, ""
}

// loadDocumentRebuild is the Slack document rebuild's state, nil where
// Slack is not a source.
func (s *Server) loadDocumentRebuild(ctx context.Context) (*documentRebuild, error) {
	if _, registered := s.registries.Sources.Lookup(slackEventSource); !registered || s.sourceControl == nil {
		return nil, nil
	}
	rebuild := &documentRebuild{SlackOn: s.sourceControl.SlackOn()}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM slack_channel_config WHERE doc_ingest_enabled`).Scan(&rebuild.Channels); err != nil {
		return nil, fmt.Errorf("document channels: %w", err)
	}
	var err error
	rebuild.Running, err = s.activeJob(ctx, adminapi.SlackDocumentRebuildJobKind)
	return rebuild, err
}

func (s *Server) handleOperationsSlackDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	if r.Form.Get("confirm") != "documents" {
		redirectJobs(w, r, "error", "Nothing was started: tick the box to confirm reading the documents again.")
		return
	}
	_, err := adminaction.RebuildSlackDocuments(r.Context(), s.audit(r), s.sourceControl)
	if s.auditFailed(w, err) {
		return
	}
	switch {
	case errors.Is(err, adminjob.ErrActive):
		redirectJobs(w, r, "notice", "Slack documents are already being read again.")
	case errors.Is(err, adminapi.ErrSlackOff):
		redirectJobs(w, r, "error", "Nothing was started: Slack is off.")
	case errors.Is(err, adminapi.ErrNoDocumentChannels):
		redirectJobs(w, r, "error", "Nothing was started: no channel has document indexing on.")
	case err != nil:
		s.logger.Warn("admin UI Slack document rebuild failed", "err", err)
		redirectJobs(w, r, "error", "The documents are not being read again: something went wrong; the audit log has the details.")
	default:
		redirectJobs(w, r, "notice", "Reading every channel's documents again. Follow it under Jobs below.")
	}
}

// currentCaptionModel is the model that describes images now, empty while
// image captions are off.
func (s *Server) currentCaptionModel() string {
	if s.captionModel == nil {
		return ""
	}
	return s.captionModel()
}

// loadImageCaptions is the state of the control that describes images
// again, nil while image captions are off.
func (s *Server) loadImageCaptions(ctx context.Context) (*imageCaptions, error) {
	model := s.currentCaptionModel()
	if model == "" {
		return nil, nil
	}
	state := &imageCaptions{Model: model}
	var err error
	if state.Earlier, err = files.CountOtherModels(ctx, s.pool, model); err != nil {
		return nil, err
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM event_images i JOIN events e ON e.id = i.event_id
		 WHERE i.captioned_at IS NULL AND i.failure_count < $1 AND NOT e.deleted`,
		events.MaxFailures).Scan(&state.Waiting); err != nil {
		return nil, fmt.Errorf("images waiting for a caption: %w", err)
	}
	return state, nil
}

// imageRequeuer queues images for the caption worker (adminaction.ImageRequeuer).
type imageRequeuer struct{ pool *pgxpool.Pool }

func (q imageRequeuer) RequeueOtherModels(ctx context.Context, model string) (int64, error) {
	return files.RequeueOtherModels(ctx, q.pool, model)
}

func (s *Server) handleOperationsImages(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	if r.Form.Get("confirm") != "images" {
		redirectJobs(w, r, "error", "Nothing was started: tick the box to confirm describing the images again.")
		return
	}
	model := s.currentCaptionModel()
	if model == "" {
		redirectJobs(w, r, "error", "Nothing was started: image captions are off.")
		return
	}
	queued, err := adminaction.DescribeImagesAgain(r.Context(), s.audit(r), imageRequeuer{pool: s.pool}, model)
	if s.auditFailed(w, err) {
		return
	}
	switch {
	case err != nil:
		s.logger.Warn("admin UI describe images again failed", "err", err)
		redirectJobs(w, r, "error", "The images are not being described again: something went wrong; the audit log has the details.")
	case queued == 0:
		redirectJobs(w, r, "notice", "Nothing to do: "+model+" has described every image.")
	default:
		redirectJobs(w, r, "notice", countOf(queued, "image", "images")+" will be described again by "+model+
			". Image captions on the Overview shows how many are left.")
	}
}

// summaryRepairBytesPerToken converts stored chunk-summary text to the tokens
// the reduce model reads, and summaryRepairPromptTokens is what each call
// reads besides it.
const (
	summaryRepairBytesPerToken = 3
	summaryRepairPromptTokens  = 1500
)

// loadSummaryRepair is the state of the control that repairs session
// summaries, nil when there is nothing to repair and no repair is running.
func (s *Server) loadSummaryRepair(ctx context.Context) (*summaryRepair, error) {
	running, err := s.activeJob(ctx, sessioningest.SummaryRepairJobKind)
	if err != nil || running != nil {
		return &summaryRepair{Running: running}, err
	}
	plan, err := sessioningest.NewStore(s.pool).PlanSummaryRepair(ctx)
	if err != nil {
		return nil, err
	}
	if plan.FromText+plan.Resummarize == 0 {
		return nil, nil
	}
	return &summaryRepair{
		FromText: plan.FromText, Resummarize: plan.Resummarize,
		Tokens: plan.ResummarizeBytes/summaryRepairBytesPerToken + plan.Resummarize*summaryRepairPromptTokens,
	}, nil
}

// summaryRepairRequester records repairs for the leader (adminaction.SummaryRepairRequester).
type summaryRepairRequester struct{ jobs *adminjob.Store }

func (q summaryRepairRequester) RequestSummaryRepair(ctx context.Context, requestedBy string) (int64, error) {
	return sessioningest.SummaryRepairRequest(ctx, q.jobs, requestedBy)
}

func (s *Server) handleOperationsSummaryRepair(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	if r.Form.Get("confirm") != "repair" {
		redirectJobs(w, r, "error", "Nothing was started: tick the box to confirm the repair.")
		return
	}
	_, err := adminaction.RepairSessionSummaries(r.Context(), s.audit(r), summaryRepairRequester{jobs: adminjob.NewStore(s.pool)})
	if s.auditFailed(w, err) {
		return
	}
	switch {
	case errors.Is(err, adminjob.ErrActive):
		redirectJobs(w, r, "notice", "A repair of session summaries is already waiting or running.")
	case err != nil:
		s.logger.Warn("admin UI session summary repair failed", "err", err)
		redirectJobs(w, r, "error", "The repair was not requested: something went wrong; the audit log has the details.")
	default:
		redirectJobs(w, r, "notice", "Session summaries are being repaired. Jobs below shows how far it has got.")
	}
}

// redirectJobs returns to Jobs and maintenance with a notice or an error.
func redirectJobs(w http.ResponseWriter, r *http.Request, kind, message string) {
	http.Redirect(w, r, jobsPath+"?"+url.Values{kind: {message}}.Encode(), http.StatusSeeOther)
}

// activeJob is the job of kind waiting or running, nil when none is. One
// that stalled is not: requesting another replaces it.
func (s *Server) activeJob(ctx context.Context, kind string) (*jobRow, error) {
	jobs, err := s.queryJobs(ctx, `WHERE kind = $1 AND status IN ('queued', 'running')`, 1, kind)
	if err != nil || len(jobs) == 0 || !jobs[0].Active {
		return nil, err
	}
	return &jobs[0], nil
}

// loadJobs reads a page of jobs, newest first, before cursor when it is
// set, and reports whether older ones remain.
func (s *Server) loadJobs(ctx context.Context, cursor *auditCursor) ([]jobRow, bool, error) {
	where, args := "", []any{}
	if cursor != nil {
		where, args = `WHERE (requested_at, id) < ($1, $2)`, []any{cursor.Created, cursor.ID}
	}
	jobs, err := s.queryJobs(ctx, where, jobsPageSize+1, args...)
	if err != nil {
		return nil, false, err
	}
	if len(jobs) > jobsPageSize {
		return jobs[:jobsPageSize], true, nil
	}
	return jobs, false, nil
}

// queryJobs reads the newest limit jobs a WHERE clause selects, and words
// their progress.
func (s *Server) queryJobs(ctx context.Context, where string, limit int, args ...any) ([]jobRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, status, requested_by, progress, request, requested_at, started_at,
		       heartbeat_at, stale_after_seconds, finished_at, COALESCE(error, '')
		  FROM admin_jobs `+where+`
		 ORDER BY requested_at DESC, id DESC
		 LIMIT `+fmt.Sprint(limit), args...)
	if err != nil {
		return nil, fmt.Errorf("admin jobs: %w", err)
	}
	defer rows.Close()
	now := time.Now()
	var jobs []jobRow
	channels := map[string]string{}
	for rows.Next() {
		var job jobRow
		var status string
		var heartbeat time.Time
		var started, finished *time.Time
		var staleSeconds int64
		if err := rows.Scan(&job.ID, &job.Kind, &status, &job.RequestedBy, &job.progress, &job.request, &job.RequestedAt,
			&started, &heartbeat, &staleSeconds, &finished, &job.Error); err != nil {
			return nil, fmt.Errorf("admin job scan: %w", err)
		}
		job.Name = jobName(job.Kind)
		stale := status == "running" && now.Sub(heartbeat) > time.Duration(staleSeconds)*time.Second
		job.Status, job.Tone = jobState(status, stale)
		job.Active = (status == "queued" || status == "running") && !stale
		if started != nil {
			end := now
			if finished != nil {
				end = *finished
			}
			job.Took = max(end.Sub(*started), 0)
		}
		job.Error = compactText(job.Error, 300)
		if job.Kind == slackImageReplayKind {
			var progress struct {
				ChannelID string `json:"channel_id"`
			}
			if json.Unmarshal(job.progress, &progress) == nil && progress.ChannelID != "" {
				job.channelID = progress.ChannelID
				channels[progress.ChannelID] = ""
			}
		}
		jobs = append(jobs, job)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("admin job rows: %w", err)
	}
	if err := s.nameChannels(ctx, channels); err != nil {
		return nil, err
	}
	for i := range jobs {
		jobs[i].Progress = jobProgress(jobs[i], channels[jobs[i].channelID], s.registries.Sources.Infos())
	}
	return jobs, nil
}

// nameChannels fills in the names of the Slack channels keyed by ID.
func (s *Server) nameChannels(ctx context.Context, channels map[string]string) error {
	if len(channels) == 0 {
		return nil
	}
	ids := make([]string, 0, len(channels))
	for id := range channels {
		ids = append(ids, id)
	}
	rows, err := s.pool.Query(ctx, `SELECT channel_id, name FROM slack_channel_info WHERE channel_id = ANY($1) AND name <> ''`, ids)
	if err != nil {
		return fmt.Errorf("slack channel names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return fmt.Errorf("slack channel names scan: %w", err)
		}
		channels[id] = name
	}
	return rows.Err()
}

const (
	slackImageReplayKind = "slack_image_replay"
	slackEventSource     = "slack"
)

func jobName(kind string) string {
	switch kind {
	case reembed.JobKind:
		return "Re-embed"
	case slackImageReplayKind:
		return "Re-read Slack images"
	case adminapi.SlackDocumentRebuildJobKind:
		return "Re-read Slack documents"
	case sessioningest.SummaryRepairJobKind:
		return "Repair session summaries"
	}
	name := []rune(strings.ReplaceAll(kind, "_", " "))
	if len(name) > 0 {
		name[0] = unicode.ToUpper(name[0])
	}
	return string(name)
}

// jobState is a job's badge. A running job whose heartbeat has gone stale
// has stalled: whatever ran it stopped.
func jobState(status string, stale bool) (word, tone string) {
	switch status {
	case "queued":
		return "waiting to start", toneAttention
	case "running":
		if stale {
			return "stalled", toneBad
		}
		return "running", toneAttention
	case "succeeded":
		return "done", toneGood
	case "failed":
		return "failed", toneBad
	}
	return status, toneNeutral
}

// reembedPercent is how far a re-embed has got, when it knows its total.
func reembedPercent(raw []byte) (int, bool) {
	var progress reembed.JobProgress
	if json.Unmarshal(raw, &progress) != nil || progress.Total <= 0 {
		return 0, false
	}
	return int(min(100*progress.Processed/progress.Total, 100)), true
}

// reembedScope names the sources a re-embed was asked to rebuild: "All
// sources" for a request that names none.
func reembedScope(raw []byte, sources source.Infos) string {
	var request reembed.JobRequest
	if json.Unmarshal(raw, &request) != nil || len(request.Sources) == 0 {
		return "All sources"
	}
	titles := make([]string, 0, len(request.Sources))
	for _, name := range request.Sources {
		title := sources.Of(name).Title
		if title == "" {
			title = name
		}
		titles = append(titles, title)
	}
	return strings.Join(titles, ", ")
}

// jobProgress says how far a job got, in words; channel names the Slack
// channel of an image replay, and sources the sources of a re-embed.
func jobProgress(job jobRow, channel string, sources source.Infos) string {
	switch job.Kind {
	case reembed.JobKind:
		scope := reembedScope(job.request, sources)
		var progress reembed.JobProgress
		if json.Unmarshal(job.progress, &progress) != nil || progress.Total == 0 {
			return scope
		}
		text := fmt.Sprintf("%s · %s of %s", scope, formatCount(progress.Processed), countOf(progress.Total, "item", "items"))
		if percent, ok := reembedPercent(job.progress); ok {
			text += fmt.Sprintf(" (%d%%)", percent)
		}
		if progress.Errors > 0 {
			text += fmt.Sprintf(" · %s could not be rebuilt", formatCount(progress.Errors))
		}
		return text
	case slackImageReplayKind:
		var progress struct {
			Scanned    int64 `json:"scanned"`
			Reconciled int64 `json:"reconciled"`
			Errors     int64 `json:"errors"`
		}
		if json.Unmarshal(job.progress, &progress) != nil {
			return ""
		}
		where := "All channels"
		switch {
		case channel != "":
			where = "#" + channel
		case job.channelID != "":
			where = job.channelID
		}
		text := fmt.Sprintf("%s · %s read · %s rebuilt", where, countOf(progress.Scanned, "message", "messages"), formatCount(progress.Reconciled))
		if progress.Errors > 0 {
			text += fmt.Sprintf(" · %s", countOf(progress.Errors, "error", "errors"))
		}
		return text
	case adminapi.SlackDocumentRebuildJobKind:
		var progress adminapi.SlackDocumentRebuildProgress
		if json.Unmarshal(job.progress, &progress) != nil || progress.Scanned == 0 {
			return ""
		}
		text := fmt.Sprintf("%s read · %s to read again",
			countOf(int64(progress.Scanned), "message with files", "messages with files"),
			countOf(int64(progress.Documents), "document", "documents"))
		if progress.Errors > 0 {
			text += fmt.Sprintf(" · %s", countOf(int64(progress.Errors), "error", "errors"))
		}
		return text
	case sessioningest.SummaryRepairJobKind:
		var progress sessioningest.SummaryRepairProgress
		if json.Unmarshal(job.progress, &progress) != nil || progress.Total == 0 {
			return ""
		}
		text := fmt.Sprintf("%s of %s · %s from their text · %s written again",
			formatCount(progress.Processed), countOf(progress.Total, "summary", "summaries"),
			formatCount(progress.FromText), formatCount(progress.Resummarize))
		if progress.Skipped > 0 {
			text += fmt.Sprintf(" · %s left to their next summary", formatCount(progress.Skipped))
		}
		if progress.Errors > 0 {
			text += fmt.Sprintf(" · %s", countOf(progress.Errors, "error", "errors"))
		}
		return text
	}
	return ""
}

func compactText(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return strings.TrimSpace(string(runes[:maxRunes])) + "…"
}
