package files

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/aws/smithy-go"
)

// TransientError wraps an underlying failure that's likely to clear
// up on its own — service 5xx, network blip, AWS throttling, Slack
// rate-limit. The FileWorker's loops bypass failure_count for these
// (they're not poison; the file content is fine, the surrounding
// service is just having a moment) and sleep + retry.
//
// Mirrors internal/embedder.TransientError. We don't import that
// type because pulling embedder into files would be a layer
// inversion (embedder is downstream of the events sink, files is
// upstream); the two packages share the pattern by convention
// rather than by code.
type TransientError struct {
	Underlying error
}

func (e *TransientError) Error() string { return "transient: " + e.Underlying.Error() }
func (e *TransientError) Unwrap() error { return e.Underlying }

// IsTransient reports whether err (or anything it wraps) is a
// TransientError.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var te *TransientError
	return errors.As(err, &te)
}

// PermanentError wraps a failure that will never clear up on
// retry — the row should jump straight to the poison cap instead
// of burning claim cycles. Mirrors TransientError's shape.
// isPermanentTerminal checks errors.As for this type first, with
// the legacy string-match fallback retained for pre-existing
// classification (see worker.go).
type PermanentError struct {
	Underlying error
}

func (e *PermanentError) Error() string { return "permanent: " + e.Underlying.Error() }
func (e *PermanentError) Unwrap() error { return e.Underlying }

// classifyHTTPStatus wraps a status-code-bearing error as transient
// for 5xx and 429. Used where we synthesize errors from
// resp.StatusCode rather than getting a typed error back.
func classifyHTTPStatus(status int, err error) error {
	if err == nil {
		return nil
	}
	if status == http.StatusTooManyRequests || (status >= 500 && status < 600) {
		return &TransientError{Underlying: err}
	}
	return err
}

// classifyAWSError wraps S3 errors as transient when they look like
// service-side hiccups: throttling, 5xx, network errors. NoSuchKey
// and other client-side 4xx are not transient (they indicate the
// object genuinely isn't there or the request is wrong).
func classifyAWSError(err error) error {
	if err == nil {
		return nil
	}
	if isNetworkError(err) {
		return &TransientError{Underlying: err}
	}
	// smithy.APIError is the AWS SDK v2 typed error interface. The
	// fault classification (Server vs Client) tells us if it's
	// retryable on the service side.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "Throttling", "ThrottlingException", "RequestLimitExceeded",
			"SlowDown", "TooManyRequestsException", "ProvisionedThroughputExceededException":
			return &TransientError{Underlying: err}
		}
		if apiErr.ErrorFault() == smithy.FaultServer {
			return &TransientError{Underlying: err}
		}
	}
	return err
}

// isNetworkError detects "the connection didn't make it / didn't
// answer in time" failures. Errors with this shape are transient
// regardless of which service was on the other end. Mirrors the
// net.Error.Timeout() check pattern from net/http callers.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A naked DeadlineExceeded with no parent context cancel
		// means the request itself timed out, not that the worker
		// was shutting down. Treat as transient.
		return true
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	// String-matching fallback for the cases the typed error
	// doesn't surface cleanly: dial-tcp connect-refused, broken
	// pipe, etc. These are well-known transient shapes and the
	// underlying types differ across stdlib versions.
	//
	// Deliberately NOT matching "EOF" — the substring shows up in
	// permanent JSON-parse errors ("unexpected EOF", "EOF at
	// offset N") which would cause the worker to infinitely retry
	// unparseable payloads as if they were network blips. Genuine
	// mid-stream connection drops surface via the net.Error
	// timeout path above, or as one of the explicit substrings
	// here. If we ever need typed io.ErrUnexpectedEOF detection
	// for a specific transient case, errors.Is is the right
	// pattern.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "no such host"):
		return true
	}
	return false
}
