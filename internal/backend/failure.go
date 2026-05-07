package backend

import (
	"errors"
	"fmt"
	"strings"
)

const (
	FailureReasonTimeout     = "timeout"
	FailureReasonTransport   = "transport-error"
	FailureReasonThrottled   = "throttled"
	FailureReasonServerError = "server-error"
	FailureReasonWaitTimeout = "wait-timeout"
	FailureReasonProbeFailed = "probe-failed"
)

type ClassifiedError struct {
	Reason string
	Err    error
}

func (e ClassifiedError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Err.Error()
}

func (e ClassifiedError) Unwrap() error {
	return e.Err
}

func NewClassifiedError(reason string, err error) error {
	reason = NormalizeFailureReason(reason)
	if reason == "" {
		return err
	}
	return ClassifiedError{Reason: reason, Err: err}
}

func FailureReason(err error) (string, bool) {
	var classified ClassifiedError
	if errors.As(err, &classified) {
		reason := NormalizeFailureReason(classified.Reason)
		return reason, reason != ""
	}
	return "", false
}

func NormalizeFailureReason(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case FailureReasonTimeout, FailureReasonTransport, FailureReasonThrottled, FailureReasonServerError, FailureReasonWaitTimeout, FailureReasonProbeFailed:
		return value
	case "deadline", "deadline-exceeded", "context-deadline-exceeded":
		return FailureReasonTimeout
	case "rate-limit", "rate-limited", "too-many-requests":
		return FailureReasonThrottled
	case "wait", "expired", "allocation-expired":
		return FailureReasonWaitTimeout
	default:
		return value
	}
}

func ClassifiedReasonError(reason string) error {
	return fmt.Errorf("backend failure classified as %s", NormalizeFailureReason(reason))
}
