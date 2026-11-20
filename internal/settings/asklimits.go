package settings

import "time"

// Ask's limits beside its reasoning effort and tool turns: the value each
// has until an administrator saves one, and the range the Models page
// accepts.
const (
	DefaultAskMaxOutputTokens = 32768
	MinAskMaxOutputTokens     = 256
	MaxAskMaxOutputTokens     = 1_000_000

	DefaultAskModelTimeout         = 5 * time.Minute
	DefaultAskTurnDeadline         = 10 * time.Minute
	DefaultAskInvestigatorDeadline = 4 * time.Minute
	DefaultAskInvestigatorGrace    = 2 * time.Minute
	MinAskDuration                 = 10 * time.Second
	MaxAskDuration                 = 24 * time.Hour
)

// AskLimits are the limits a replica builds Ask with: two builds with equal
// AskLimits behave the same.
type AskLimits struct {
	// ServiceTier is the provider's latency tier, empty for its default.
	ServiceTier string
	// MaxOutputTokens bounds one model reply.
	MaxOutputTokens int
	// ModelTimeout bounds one request to the model, and TurnDeadline the
	// whole answer to one question.
	ModelTimeout, TurnDeadline time.Duration
	// InvestigatorDeadline is how long the observability investigator
	// researches before it is told to report, and InvestigatorGrace how long
	// it then has to write the report. The grace must cover a report (about
	// 40 seconds) and whatever remains of a tool call running as the
	// deadline passes, which is only checked between model calls.
	InvestigatorDeadline, InvestigatorGrace time.Duration
}

// AskLimits is Ask's limits in effect: each saved value while it is in
// range, otherwise its default.
func (m Models) AskLimits() AskLimits {
	limits := AskLimits{
		ServiceTier:          m.AskServiceTier,
		MaxOutputTokens:      DefaultAskMaxOutputTokens,
		ModelTimeout:         askDuration(m.AskModelTimeout, DefaultAskModelTimeout),
		TurnDeadline:         askDuration(m.AskTurnDeadline, DefaultAskTurnDeadline),
		InvestigatorDeadline: askDuration(m.AskInvestigatorDeadline, DefaultAskInvestigatorDeadline),
		InvestigatorGrace:    askDuration(m.AskInvestigatorGrace, DefaultAskInvestigatorGrace),
	}
	if AskMaxOutputTokensInRange(m.AskMaxOutputTokens) {
		limits.MaxOutputTokens = m.AskMaxOutputTokens
	}
	return limits
}

// AskMaxOutputTokensInRange reports whether the Models page accepts tokens.
func AskMaxOutputTokensInRange(tokens int) bool {
	return tokens >= MinAskMaxOutputTokens && tokens <= MaxAskMaxOutputTokens
}

// AskDurationInRange reports whether the Models page accepts d for one of
// Ask's time limits.
func AskDurationInRange(d time.Duration) bool {
	return d >= MinAskDuration && d <= MaxAskDuration
}

func askDuration(saved Duration, def time.Duration) time.Duration {
	if d := time.Duration(saved); AskDurationInRange(d) {
		return d
	}
	return def
}
