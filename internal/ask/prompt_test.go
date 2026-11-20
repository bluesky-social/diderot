package ask

import (
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

// These tests pin the facts the example prompts must keep telling the model,
// each by a phrase of the sentence that states it. A rewrite may reword a
// sentence and update its phrase here; it may not drop the fact.

func requirePhrases(t *testing.T, name, text string, phrases ...string) {
	t.Helper()
	for _, want := range phrases {
		if !strings.Contains(text, want) {
			t.Errorf("%s is missing %q", name, want)
		}
	}
}

func TestDefaultPromptDocumentsDeliveryAndExactReferenceSemantics(t *testing.T) {
	prompt := DefaultPrompt()
	requirePhrases(t, "default prompt", prompt,
		"`<exact_references>`",
		"answer from it directly instead of searching for it",
		"When the block names no asker, do not assume who is asking",
		"Earlier turns of the same conversation",
	)
	requirePhrases(t, "terminal output prompt", TerminalOutputInstructions(),
		"your final message is the answer",
		"`answer`: the complete reply shown to the asker",
		"Use an empty array when there are none",
		"never invent an ID",
	)
	if strings.Contains(prompt, "call `respond`") {
		t.Error("default prompt still instructs the retired respond tool")
	}
}

func TestDefaultPromptDocumentsZendeskRoutingAndBoundaries(t *testing.T) {
	requirePhrases(t, "default prompt", DefaultPrompt(),
		"Zendesk tickets and help articles are searched like any other source",
		"There is no live lookup",
		"not finding a ticket does not mean none was filed",
		"A ticket is evidence that a customer experienced something",
		"any engineering work that followed, is in the other sources",
		"A ticket's first message is not its conversation",
		"A `retrieve` result is a sample, not a count",
		"similar subjects do not show that tickets share a cause",
		"count tickets and not rows",
		"note whether it is published and when it was last updated",
		"never repeat credentials, verification codes or private links",
		"You see only what the asker is allowed to read",
	)
}

func TestDefaultPromptDocumentsSlackDMCoverageBoundary(t *testing.T) {
	requirePhrases(t, "default prompt", DefaultPrompt(),
		"Direct messages between people are never indexed",
		"cannot be answered either way",
		"do not offer channel messages in its place",
		"do not treat silence as a sign the conversation never happened",
	)
}

func TestDefaultPromptDocumentsChangeHistoryAndOwnership(t *testing.T) {
	requirePhrases(t, "default prompt", DefaultPrompt(),
		"When a deployment-history tool is in your tool list",
		"a discussion or a commit date is not a rollout time",
		"Cite the event links it returns",
		"no record of a change does not prove there was none",
		"a failed lookup is not an empty result",
		"Without such a tool, do not say you checked change history",
	)
	requirePhrases(t, "observability delegation prompt", ObservabilityDelegationInstructions(),
		"that comparison is the investigator's job",
	)
}

func TestDefaultPromptDocumentsCanonicalAgentSessionSource(t *testing.T) {
	prompt := promptcatalog.Render(DefaultPrompt(), map[string]string{promptcatalog.VarSources: testSources.PromptList()})
	requirePhrases(t, "default prompt", prompt,
		"- `agent_session` — canonical summaries of redacted coding-agent session transcripts.",
		"call an `agent_session` result an agent session",
	)
	for _, stale := range []string{
		"Claude Code sessions and other Claude-driven work",
		"Claude sessions",
		"Claude Code sessions",
		"across clients, harnesses, origin apps, and inference providers",
	} {
		if strings.Contains(prompt, stale) {
			t.Errorf("default prompt still contains stale agent-session language %q", stale)
		}
	}
}

func TestDefaultPromptUsesModelFacingSourceContracts(t *testing.T) {
	prompt := DefaultPrompt()
	requirePhrases(t, "default prompt", prompt,
		"a hit is one message without its conversation",
		"a model's description of an image they shared",
		"a whole-session hit by the session's last activity",
		"A Notion actor last edited the page and may not have written it",
		"read the organization's repositories as of the last index",
	)
	for _, implementationDetail := range []string{
		"labeled sections: `CAPTION`",
		"`UNREAD`",
		"`notion.so/<id>`",
		"already widened by ±20 messages",
		"Zoekt query over ALL",
		"on Slack each becomes its own noise message",
	} {
		if strings.Contains(prompt, implementationDetail) {
			t.Errorf("default prompt contains implementation detail %q", implementationDetail)
		}
	}
}

// TestDefaultPromptsStateTheEvidenceRules pins the evidence rules the default
// prompts state, on points where a prompt that is silent or contradicts
// another produces wrong answers.
func TestDefaultPromptsStateTheEvidenceRules(t *testing.T) {
	requirePhrases(t, "default prompt", DefaultPrompt(),
		// Currency: a plan or a ticket status is a claim as of its date.
		"are claims as of their date",
		// The asking thread comes back as a search hit.
		"A question is not evidence for its own answer",
		// What was said in the thread is evidence of what was said.
		"It is good evidence of what people in the thread said and decided",
		// A stopping rule in place of a per-tool call budget.
		"Stop when the question is answered and every claim you will make rests on a source you have read",
		// Code is not instructions for people.
		"Code is not instructions for people",
		// A date range the question did not ask for hides older material.
		"a date range you do not need hides older material that still holds",
	)
	requirePhrases(t, "answer rules", AnswerDisciplineInstructions(),
		"A yes-or-no question gets",
		"Match the length to the request",
		"named as the nearest thing and not passed off as the answer",
		"Never assemble one from fragments of different results",
		"Do not agree to something you have no evidence for",
	)
}

// TestDefaultPromptsDoNotShout keeps the example free of the emphasis the
// rewrite removed: models weigh a capitalized rule over its neighbors, which
// is how one instruction came to override another.
//
// The session map prompt is left out. Its text is part of every chunk
// summary's cache key, so rewording it makes each session that is still
// growing map all of its chunks again. It changes together with the map
// model, when that work is paid for anyway.
func TestDefaultPromptsDoNotShout(t *testing.T) {
	bundle := promptcatalog.MustEmbedded()
	for _, prompt := range bundle.Prompts {
		if prompt.Key == promptcatalog.SummarySessionMap {
			continue
		}
		for _, shout := range []string{"IMPORTANT", "MUST", "NEVER", "ALWAYS", "DO NOT", "CRITICAL"} {
			if strings.Contains(prompt.Body, shout) {
				t.Errorf("%s contains %q", prompt.Key, shout)
			}
		}
	}
}

// TestInvestigatorRulesLeaveConnectionFactsToTheConnection: which Grafana
// datasources to query is set on the Grafana connection and reaches the
// investigator through one variable, and a Sentry connection's URL limits it
// to an organization and project. The shipped rules carry neither, so an
// install never has to edit them to say where its telemetry lives.
func TestInvestigatorRulesLeaveConnectionFactsToTheConnection(t *testing.T) {
	bodies := map[string]string{}
	for _, prompt := range promptcatalog.MustEmbedded().Prompts {
		bodies[prompt.Key] = prompt.Body
	}
	grafana := bodies[promptcatalog.AskObservabilityGrafanaDiscipline]
	if !strings.Contains(grafana, "\n{{"+promptcatalog.VarGrafanaDatasources+"}}\n") {
		t.Errorf("the Grafana rules must give the pinned datasources a line of their own:\n%s", grafana)
	}
	sentry := bodies[promptcatalog.AskObservabilitySentryDiscipline]
	for _, text := range []string{grafana, sentry} {
		for _, fact := range []string{"organization and project", "project filter", "Use only these datasource"} {
			if strings.Contains(text, fact) {
				t.Errorf("a shipped investigator rule states a connection's fact (%q):\n%s", fact, text)
			}
		}
	}
}
