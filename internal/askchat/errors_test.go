package askchat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"
)

func TestShortAskErrHidesEinoTimeoutDetails(t *testing.T) {
	err := fmt.Errorf("ask service: Eino run: [NodeRunError] failed to create response: %w\nnode path: [node_1, ChatModel]", context.DeadlineExceeded)
	got := ErrorText(err)
	if got != "the model timed out before finishing; please try again" {
		t.Fatalf("ErrorText = %q", got)
	}
	for _, leaked := range []string{"Eino", "NodeRunError", "ChatModel"} {
		if strings.Contains(got, leaked) {
			t.Errorf("ErrorText leaked %q: %q", leaked, got)
		}
	}
}

func TestShortAskErrRecognizesStringOnlyDeadlineWrapper(t *testing.T) {
	err := errors.New("opaque framework wrapper: context deadline exceeded")
	if got := ErrorText(err); got != "the model timed out before finishing; please try again" {
		t.Fatalf("ErrorText = %q", got)
	}
}

func TestShortAskErrMapsGatewayFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		want   string
	}{
		{"context window", 400, "context_length_exceeded", "this conversation is too long for the model; please start a new thread or shorten your question"},
		{"encrypted history", 400, "invalid_encrypted_content", "I couldn't continue this conversation; please start a new thread"},
		{"other bad request", 400, "invalid_request_error", "I couldn't process that request; please try again in a new thread or let an operator know"},
		{"authentication", 401, "", "the model is unavailable right now; please let an operator know"},
		{"permission", 403, "", "the model is unavailable right now; please let an operator know"},
		{"request timeout", 408, "", "the model request was interrupted; please try again shortly"},
		{"conflict", 409, "", "the model request was interrupted; please try again shortly"},
		{"request too large", 413, "", "the request is too large for the model; please shorten your question"},
		{"rate limited", 429, "slow_down", "the model is busy right now; please try again shortly"},
		{"quota", 429, "credit_balance_exhausted", "the model is unavailable right now; please let an operator know"},
		{"server error", 500, "", "the model is temporarily unavailable; please try again shortly"},
		{"overloaded", 529, "", "the model is temporarily unavailable; please try again shortly"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, "http://gateway/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			apiErr := &responses.Error{
				StatusCode: test.status,
				Code:       test.code,
				Request:    request,
				Response:   &http.Response{StatusCode: test.status},
			}
			wrapped := fmt.Errorf("ask service: Eino run: [NodeRunError] failed to create response: %w\nnode path: [node_1, ChatModel]", apiErr)
			if got := ErrorText(wrapped); got != test.want {
				t.Errorf("ErrorText = %q, want %q", got, test.want)
			}
		})
	}
}

func TestShortAskErrHidesUnexpectedDetails(t *testing.T) {
	got := ErrorText(errors.New("Eino [NodeRunError] internal URL and credentials"))
	if got != "I hit an unexpected error; please try again. If it keeps happening, let an operator know" {
		t.Fatalf("ErrorText = %q", got)
	}
}
