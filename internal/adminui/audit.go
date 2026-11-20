package adminui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const auditPageSize = 50

var auditPage = mustPage("audit.html")

type auditData struct {
	pageData
	Records []auditRecord
	// Actions are the actions the log holds, for the filter.
	Actions     []auditAction
	Actor       string
	Action      string
	Target      string
	Status      string
	RequestID   string
	Range       string
	NextURL     string
	Truncated   bool
	FilterError string
}

// Filtered reports whether any filter narrows the log.
func (d auditData) Filtered() bool {
	return d.Actor != "" || d.Action != "" || d.Target != "" || d.Status != "" || d.RequestID != "" || d.Range != "all"
}

// Pages is the pagination row under the records.
func (d auditData) Pages() pagination {
	pages := pagination{Summary: fmt.Sprintf("%s shown, newest first", plural(len(d.Records), "change"))}
	if d.Truncated {
		pages.ForwardURL = d.NextURL
	}
	return pages
}

// auditAction is an action key and how the page words it.
type auditAction struct {
	Key   string
	Label string
}

type auditRecord struct {
	ID         int64
	CreatedAt  time.Time
	FinishedAt *time.Time
	// Duration is how long the action took, empty while it has not finished.
	Duration string
	Actor    string
	// Action is the stored key; ActionLabel says it in words.
	Action      string
	ActionLabel string
	// Target is the stored target, usually an ID; TargetName the name it
	// resolves to, where one is known.
	Target     string
	TargetName string
	RequestID  string
	// Status is the stored status; Result and Tone are its badge.
	Status  string
	Result  string
	Tone    string
	Details string
	Error   string
}

type auditCursor struct {
	Created time.Time
	ID      int64
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadAudit(r.Context(), r)
	if err != nil {
		s.logger.Error("admin audit query failed", "err", err)
		http.Error(w, "audit query failed", http.StatusInternalServerError)
		return
	}
	data.pageData = s.page(r, "Audit log", navOperations)
	data.CurrentTab = tabOperationsAudit
	s.render(w, auditPage, data)
}

func (s *Server) loadAudit(ctx context.Context, r *http.Request) (auditData, error) {
	query := r.URL.Query()
	data := auditData{
		Actor: strings.ToLower(strings.TrimSpace(query.Get("actor"))), Action: strings.TrimSpace(query.Get("action")),
		Target: strings.TrimSpace(query.Get("target")), Status: strings.TrimSpace(query.Get("status")),
		RequestID: strings.TrimSpace(query.Get("request_id")), Range: strings.TrimSpace(query.Get("range")),
	}
	if data.Range != "24h" && data.Range != "7d" && data.Range != "30d" {
		data.Range = "all"
	}
	if data.Status != "" && data.Status != "started" && data.Status != "succeeded" && data.Status != "failed" {
		data.FilterError = "Result must be started, succeeded, or failed."
		data.Status = ""
	}

	var createdAfter *time.Time
	now := time.Now()
	switch data.Range {
	case "24h":
		value := now.Add(-24 * time.Hour)
		createdAfter = &value
	case "7d":
		value := now.Add(-7 * 24 * time.Hour)
		createdAfter = &value
	case "30d":
		value := now.Add(-30 * 24 * time.Hour)
		createdAfter = &value
	}

	var cursor *auditCursor
	if raw := strings.TrimSpace(query.Get("before")); raw != "" {
		if parsed, ok := parseAuditCursor(raw); ok {
			cursor = &parsed
		} else {
			if data.FilterError != "" {
				data.FilterError += " "
			}
			data.FilterError += "That page link is not valid, so this is the newest matching page."
		}
	}

	statement, args := buildAuditQuery(data, createdAfter, cursor)
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return data, fmt.Errorf("audit log: %w", err)
	}
	defer rows.Close()

	var lastCursor string
	for rows.Next() {
		var record auditRecord
		var details []byte
		if err := rows.Scan(&record.ID, &record.CreatedAt, &record.FinishedAt, &record.Actor, &record.Action,
			&record.Target, &record.RequestID, &record.Status, &details, &record.Error); err != nil {
			return data, fmt.Errorf("audit log scan: %w", err)
		}
		if len(data.Records) == auditPageSize {
			data.Truncated = true
			break
		}
		record.ActionLabel = s.auditActionLabel(record.Action)
		record.Result, record.Tone = auditResult(record.Status)
		record.Details = prettyAuditDetails(details)
		record.Error = compactText(record.Error, 2000)
		if record.FinishedAt != nil {
			record.Duration = formatDuration(record.FinishedAt.Sub(record.CreatedAt))
		}
		data.Records = append(data.Records, record)
		lastCursor = formatAuditCursor(record.CreatedAt, record.ID)
	}
	// A full page stops reading one row early, which leaves the connection
	// with rows; give it back before the queries below ask for another.
	rows.Close()
	if err := rows.Err(); err != nil {
		return data, fmt.Errorf("audit log rows: %w", err)
	}
	if data.Truncated {
		next := cloneURLValues(query)
		next.Set("before", lastCursor)
		data.NextURL = "?" + next.Encode()
	}
	if err := s.nameAuditTargets(ctx, data.Records); err != nil {
		return data, err
	}
	data.Actions, err = s.auditActions(ctx, data.Action)
	return data, err
}

func buildAuditQuery(data auditData, createdAfter *time.Time, cursor *auditCursor) (string, []any) {
	statement := `
		SELECT id, created_at, finished_at, actor_email, action, target,
		       COALESCE(request_id, ''), status, details, COALESCE(error, '')
		  FROM admin_audit_log`
	conditions := make([]string, 0, 7)
	args := make([]any, 0, 9)
	addExact := func(column, value string) {
		if value == "" {
			return
		}
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	addExact("actor_email", data.Actor)
	addExact("action", data.Action)
	addExact("target", data.Target)
	addExact("status", data.Status)
	addExact("request_id", data.RequestID)
	if createdAfter != nil {
		args = append(args, *createdAfter)
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if cursor != nil {
		args = append(args, cursor.Created, cursor.ID)
		conditions = append(conditions, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	if len(conditions) > 0 {
		statement += "\n WHERE " + strings.Join(conditions, "\n   AND ")
	}
	args = append(args, auditPageSize+1)
	statement += fmt.Sprintf("\n ORDER BY created_at DESC, id DESC\n LIMIT $%d", len(args))
	return statement, args
}

// auditActions lists the actions the log holds, by label, for the filter.
// selected stays in the list when the log no longer holds it.
func (s *Server) auditActions(ctx context.Context, selected string) ([]auditAction, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT action FROM admin_audit_log`)
	if err != nil {
		return nil, fmt.Errorf("audit log actions: %w", err)
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("audit log actions scan: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit log actions rows: %w", err)
	}
	if selected != "" && !slices.Contains(keys, selected) {
		keys = append(keys, selected)
	}
	actions := make([]auditAction, 0, len(keys))
	for _, key := range keys {
		actions = append(actions, auditAction{Key: key, Label: s.auditActionLabel(key)})
	}
	slices.SortFunc(actions, func(a, b auditAction) int {
		return strings.Compare(a.Label+a.Key, b.Label+b.Key)
	})
	return actions, nil
}

// nameAuditTargets fills TargetName for the targets that are IDs of things
// with names: Slack channels and reports.
func (s *Server) nameAuditTargets(ctx context.Context, records []auditRecord) error {
	const channel, report = "sources.slack.", "reports."
	ids := map[string][]string{}
	for _, record := range records {
		for _, kind := range []string{channel, report} {
			if strings.HasPrefix(record.Action, kind) {
				ids[kind] = append(ids[kind], record.Target)
			}
		}
	}
	names := map[string]map[string]string{channel: {}, report: {}}
	collect := func(kind, statement, prefix string) error {
		if len(ids[kind]) == 0 {
			return nil
		}
		rows, err := s.pool.Query(ctx, statement, ids[kind])
		if err != nil {
			return fmt.Errorf("audit log target names: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				return fmt.Errorf("audit log target names scan: %w", err)
			}
			names[kind][id] = prefix + name
		}
		return rows.Err()
	}
	if err := collect(channel, `SELECT channel_id, name FROM slack_channel_info WHERE channel_id = ANY($1) AND name <> ''`, "#"); err != nil {
		return err
	}
	if err := collect(report, `SELECT id::text, name FROM scheduled_reports WHERE id::text = ANY($1)`, ""); err != nil {
		return err
	}
	for index := range records {
		for kind, byID := range names {
			if strings.HasPrefix(records[index].Action, kind) {
				records[index].TargetName = byID[records[index].Target]
			}
		}
	}
	return nil
}

// auditResult is the badge for a stored status.
func auditResult(status string) (label, tone string) {
	switch status {
	case "succeeded":
		return "ok", toneGood
	case "failed":
		return "failed", toneBad
	default:
		return "started", toneAttention
	}
}

// auditActionLabels says each recorded action in words. A key missing here
// still reads, less well, through auditActionLabel's fallback.
var auditActionLabels = map[string]string{
	"setup.organization.save":              "Saved the organization",
	"setup.models.save":                    "Saved model choices",
	"setup.test_models":                    "Tested the models",
	"setup.acknowledge_access":             "Marked access as reviewed",
	"setup.accept_example_prompts":         "Kept the example prompts",
	"setup.disable_bootstrap_admin":        "Turned off the bootstrap admin",
	"setup.bootstrap_admin.reset":          "Turned the bootstrap admin back on",
	"setup.go_live":                        "Went live",
	"setup.back_to_setup":                  "Went back to setup",
	"sources.slack.config":                 "Changed a Slack channel's settings",
	"sources.slack.config.ask":             "Turned Ask on or off in a Slack channel",
	"sources.slack.config.documents":       "Turned document indexing on or off in a Slack channel",
	"sources.slack.config.external":        "Changed who may ask in a Slack channel",
	"sources.slack.backfill":               "Started re-reading a Slack channel's history or documents",
	"sources.slack.blacklist":              "Blocked a Slack channel",
	"sources.slack.unblacklist":            "Unblocked a Slack channel",
	"sources.slack.invite":                 "Added the bot to a Slack channel",
	"sources.slack.leave":                  "Removed the bot from a Slack channel",
	"sources.slack.images.enqueue":         "Started rebuilding a Slack channel's image descriptions",
	"sources.slack.documents.rebuild":      "Started reading every Slack channel's documents again",
	"sources.notion.root.add":              "Added a Notion page to index",
	"sources.notion.root.remove":           "Removed a Notion page from the index",
	"reports.create":                       "Created a report",
	"reports.update":                       "Changed a report",
	"reports.delete":                       "Deleted a report",
	"reports.enable":                       "Turned a report on",
	"reports.disable":                      "Turned a report off",
	"reports.run":                          "Ran a report now",
	"prompts.create":                       "Pushed a version of the prompts",
	"prompts.save":                         "Saved a prompt and made it live",
	"prompts.revert":                       "Restored an earlier version of a prompt",
	"prompts.activate":                     "Made a version of the prompts live",
	"prompts.load-example":                 "Loaded the example prompts",
	"prompts.draft.save":                   "Saved a prompt to the draft",
	"prompts.draft.discard":                "Discarded a prompt's change from the draft",
	"prompts.draft.restore":                "Put an earlier version of a prompt in the draft",
	"prompts.draft.take_shipped":           "Put the shipped text of prompts in the draft",
	"prompts.draft.discard_all":            "Discarded the prompt draft",
	"prompts.draft.publish":                "Made the prompt draft live",
	"operations.reembed.start":             "Started a re-embed",
	"operations.failures.retry":            "Retried failed items",
	"operations.images.describe_again":     "Started describing images again",
	"operations.session_summaries.repair":  "Started repairing session summaries",
	"operations.session_summarization.set": "Paused or resumed session summaries",
	"sessions.privacy.set":                 "Changed who can find agent sessions",
}

// auditActionLabel says an action key in words: from the table, a connector
// or source key by its title, and anything else by spacing out the key.
func (s *Server) auditActionLabel(key string) string {
	if label, ok := auditActionLabels[key]; ok {
		return label
	}
	if rest, ok := strings.CutPrefix(key, "sources."); ok {
		if name, ok := strings.CutSuffix(rest, ".backfill"); ok && !strings.Contains(name, ".") {
			return "Started re-reading " + s.registries.Title(name)
		}
	}
	if rest, ok := strings.CutPrefix(key, "connectors."); ok {
		if name, verb, ok := strings.Cut(rest, "."); ok {
			title := s.registries.Title(name)
			switch verb {
			case "save":
				return "Saved " + title + " settings"
			case "enable":
				return "Turned on " + title
			case "disable":
				return "Turned off " + title
			}
		}
	}
	words := strings.Fields(strings.NewReplacer(".", " ", "_", " ").Replace(key))
	if len(words) == 0 {
		return key
	}
	label := strings.Join(words, " ")
	first := []rune(label)
	first[0] = unicode.ToUpper(first[0])
	return string(first)
}

func prettyAuditDetails(raw []byte) string {
	if len(raw) == 0 || string(raw) == "{}" {
		return ""
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, raw, "", "  "); err != nil {
		return compactText(string(raw), 4000)
	}
	return formatted.String()
}

func formatAuditCursor(created time.Time, id int64) string {
	return created.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(id, 10)
}

func parseAuditCursor(raw string) (auditCursor, bool) {
	createdRaw, idRaw, ok := strings.Cut(raw, "|")
	if !ok {
		return auditCursor{}, false
	}
	created, err := time.Parse(time.RFC3339Nano, createdRaw)
	if err != nil {
		return auditCursor{}, false
	}
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil || id <= 0 {
		return auditCursor{}, false
	}
	return auditCursor{Created: created, ID: id}, true
}
