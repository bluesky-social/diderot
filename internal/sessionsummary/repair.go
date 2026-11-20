package sessionsummary

import (
	"encoding/json"
	"regexp"
	"strings"
)

// The reduce model sometimes writes the rest of its tool call inside a string
// field: narrative ends with "</narrative>" followed by
// `<parameter name="embedding_text">…`, and the field the text belongs to is
// left empty. No prompt wording stopped it. The swallowed text is well formed,
// so it is split back into its fields here, without another model call.

const summaryFieldNames = `narrative|evidence|embedding_text|context_hint|tags`

var (
	// fieldStart opens a swallowed field, in either shape the model writes.
	fieldStart = regexp.MustCompile(`<parameter name="(` + summaryFieldNames + `)">|<(` + summaryFieldNames + `)>`)
	// fieldEnd is the closing markup left at the end of a field's text.
	fieldEnd = regexp.MustCompile(`(?:\s*</(?:` + summaryFieldNames + `|parameter|summary)>)+\s*$`)
	cdata    = regexp.MustCompile(`(?s)^<!\[CDATA\[(.*)\]\]>$`)
	tagList  = regexp.MustCompile(`(?s)<(projects|languages|activities|freeform)>(.*?)</(?:projects|languages|activities|freeform)>`)
)

// SplitSwallowedFields separates text that swallowed later fields of a summary
// tool call. It returns the text that belongs to the field itself and the
// swallowed fields by name ("narrative" when the field's own text was written
// inside it, as in `…</evidence><narrative>…`). Text without such markup comes
// back unchanged with no fields.
func SplitSwallowedFields(text string) (string, map[string]string) {
	starts := fieldStart.FindAllStringSubmatchIndex(text, -1)
	// Either form can be ordinary text in a summary of work on such markup,
	// so the first swallowed field counts only right after closing markup
	// ("</narrative>", "</parameter>"), which is how the model always
	// writes it.
	for len(starts) > 0 && !fieldEnd.MatchString(text[:starts[0][0]]) {
		starts = starts[1:]
	}
	if len(starts) == 0 {
		return fieldEnd.ReplaceAllString(text, ""), nil
	}
	fields := make(map[string]string, len(starts))
	for index, start := range starts {
		end := len(text)
		if index+1 < len(starts) {
			end = starts[index+1][0]
		}
		var name string
		if start[2] >= 0 {
			name = text[start[2]:start[3]]
		} else {
			name = text[start[4]:start[5]]
		}
		value := strings.TrimSpace(fieldEnd.ReplaceAllString(text[start[1]:end], ""))
		if match := cdata.FindStringSubmatch(value); match != nil {
			value = strings.TrimSpace(match[1])
		}
		if _, seen := fields[name]; !seen {
			fields[name] = value
		}
	}
	own := strings.TrimSpace(fieldEnd.ReplaceAllString(text[:starts[0][0]], ""))
	return own, fields
}

// repairSwallowedFields moves fields the model wrote inside other string
// fields back to their own. A field the model filled itself keeps its value.
func repairSwallowedFields(summary *FinalSummary) {
	recovered := map[string]string{}
	split := func(text string) string {
		own, fields := SplitSwallowedFields(text)
		for name, value := range fields {
			if _, seen := recovered[name]; !seen {
				recovered[name] = value
			}
		}
		return own
	}
	narrative := split(summary.Narrative)
	if inner := recovered["narrative"]; inner != "" {
		narrative = inner
	}
	summary.Narrative = narrative
	summary.EmbeddingText = split(summary.EmbeddingText)
	summary.ContextHint = split(summary.ContextHint)
	if strings.TrimSpace(summary.EmbeddingText) == "" {
		summary.EmbeddingText = recovered["embedding_text"]
	}
	if strings.TrimSpace(summary.ContextHint) == "" {
		summary.ContextHint = recovered["context_hint"]
	}
	if summary.Tags.empty() {
		if tags, ok := ParseSwallowedTags(recovered["tags"]); ok {
			summary.Tags = tags
		}
	}
	if len(summary.Evidence) == 0 {
		summary.Evidence = parseSwallowedEvidence(recovered["evidence"])
	}
	summary.Tags = summary.Tags.withoutPlaceholders()
}

// StoredSummary is the text of a published session summary, its tags in
// their stored, flattened form (TagSet.Flatten).
type StoredSummary struct {
	Narrative     string
	EmbeddingText string
	ContextHint   string
	Tags          []string
}

// RepairStoredSummary applies the same repair to a summary stored before
// ReduceCall repaired them. changed reports whether any text moved; complete
// whether the repaired summary has a narrative, embedding text and context
// hint free of tool-call markup, so it can be published as it is.
func RepairStoredSummary(stored StoredSummary) (repaired StoredSummary, changed, complete bool) {
	summary := FinalSummary{Narrative: stored.Narrative, EmbeddingText: stored.EmbeddingText, ContextHint: stored.ContextHint}
	repairSwallowedFields(&summary)
	repaired = StoredSummary{Narrative: summary.Narrative, EmbeddingText: summary.EmbeddingText, ContextHint: summary.ContextHint}
	for _, tag := range stored.Tags {
		value := tag[strings.Index(tag, ":")+1:]
		if strings.HasPrefix(value, "<") && strings.HasSuffix(value, ">") {
			continue
		}
		repaired.Tags = append(repaired.Tags, tag)
	}
	if len(repaired.Tags) == 0 {
		repaired.Tags = summary.Tags.Flatten()
	}
	changed = repaired.Narrative != stored.Narrative || repaired.EmbeddingText != stored.EmbeddingText ||
		repaired.ContextHint != stored.ContextHint || strings.Join(repaired.Tags, "\x00") != strings.Join(stored.Tags, "\x00")
	complete = strings.TrimSpace(repaired.Narrative) != "" && strings.TrimSpace(repaired.EmbeddingText) != "" &&
		strings.TrimSpace(repaired.ContextHint) != "" && !hasSwallowedFields(repaired.Narrative) &&
		!hasSwallowedFields(repaired.EmbeddingText) && !hasSwallowedFields(repaired.ContextHint)
	return repaired, changed, complete
}

func hasSwallowedFields(text string) bool {
	own, fields := SplitSwallowedFields(text)
	return len(fields) > 0 || strings.TrimSpace(own) != strings.TrimSpace(text)
}

// ParseSwallowedTags reads tags written as text: the JSON object the tool
// schema asks for, or one element per tag list holding a JSON array.
func ParseSwallowedTags(text string) (TagSet, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return TagSet{}, false
	}
	var tags TagSet
	if json.Unmarshal([]byte(text), &tags) == nil {
		return tags, !tags.empty()
	}
	for _, match := range tagList.FindAllStringSubmatch(text, -1) {
		var values []string
		if json.Unmarshal([]byte(strings.TrimSpace(match[2])), &values) != nil {
			continue
		}
		switch match[1] {
		case "projects":
			tags.Projects = values
		case "languages":
			tags.Languages = values
		case "activities":
			tags.Activities = values
		case "freeform":
			tags.Freeform = values
		}
	}
	return tags, !tags.empty()
}

func parseSwallowedEvidence(text string) StringList {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var list StringList
	if json.Unmarshal([]byte(text), &list) == nil {
		return list
	}
	return StringList{text}
}

func (t TagSet) empty() bool {
	return len(t.Projects)+len(t.Languages)+len(t.Activities)+len(t.Freeform) == 0
}

// withoutPlaceholders drops values such as "<UNKNOWN>" that the model writes
// when it has nothing for a list.
func (t TagSet) withoutPlaceholders() TagSet {
	keep := func(values []string) []string {
		var out []string
		for _, value := range values {
			trimmed := strings.TrimSpace(value)
			if trimmed == "" || (strings.HasPrefix(trimmed, "<") && strings.HasSuffix(trimmed, ">")) {
				continue
			}
			out = append(out, value)
		}
		return out
	}
	return TagSet{
		Projects: keep(t.Projects), Languages: keep(t.Languages),
		Activities: keep(t.Activities), Freeform: keep(t.Freeform),
	}
}
