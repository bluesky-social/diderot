package askchat

import (
	"context"
	"errors"
	"strings"

	"github.com/openai/openai-go/v3/responses"
)

// ErrorText turns a failed turn into guidance the asker can act on, without
// upstream response bodies or framework details. The full error stays in
// the log.
func ErrorText(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "the model timed out before finishing; please try again"
	}
	if errors.Is(err, context.Canceled) {
		return "the request was interrupted; please try again"
	}
	var apiErr *responses.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "context_length_exceeded":
			return "this conversation is too long for the model; please start a new thread or shorten your question"
		case "invalid_encrypted_content":
			return "I couldn't continue this conversation; please start a new thread"
		case "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded", "organization_usage_limit_exceeded":
			return "the model is unavailable right now; please let an operator know"
		}
		switch {
		case apiErr.StatusCode == 429:
			return "the model is busy right now; please try again shortly"
		case apiErr.StatusCode == 408 || apiErr.StatusCode == 409:
			return "the model request was interrupted; please try again shortly"
		case apiErr.StatusCode == 401 || apiErr.StatusCode == 403:
			return "the model is unavailable right now; please let an operator know"
		case apiErr.StatusCode == 413:
			return "the request is too large for the model; please shorten your question"
		case apiErr.StatusCode >= 500:
			return "the model is temporarily unavailable; please try again shortly"
		case apiErr.StatusCode >= 400:
			return "I couldn't process that request; please try again in a new thread or let an operator know"
		}
	}
	if strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		return "the model timed out before finishing; please try again"
	}
	return "I hit an unexpected error; please try again. If it keeps happening, let an operator know"
}
