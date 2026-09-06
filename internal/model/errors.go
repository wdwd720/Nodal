package model

import (
	"errors"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// Sentinel conditions callers branch on. They map onto the API codes so a
// model failure is never reported as a generic internal error.
var (
	// ErrUnavailable: the provider could not be reached, timed out, was
	// overloaded, or returned a server error. Retryable at the caller's
	// discretion; every retry is a new billable request.
	ErrUnavailable = errors.New("model: provider unavailable")
	// ErrRateLimited: the provider refused for rate or spend reasons.
	ErrRateLimited = errors.New("model: provider rate limited")
	// ErrBadResponse: the provider answered, but the answer is unusable —
	// a truncated body, a refusal, or an unknown stop reason. A truncated
	// JSON document is never repaired (docs/api/providers/anthropic.md).
	ErrBadResponse = errors.New("model: unusable provider response")
	// ErrFakeNotAllowed: the test double was constructed outside LOCAL,
	// TEST or DEV.
	ErrFakeNotAllowed = errors.New("model: fake provider is not permitted in this environment")
)

// Unavailable wraps a transport or server failure.
func Unavailable(detail string, cause error) *errs.Error {
	if cause == nil {
		cause = ErrUnavailable
	}
	return errs.Wrap(cause, errs.CodeModelUnavailable, "model: "+detail)
}

// RateLimited wraps a provider rate-limit or spend-cap refusal. retryAfter
// is attached when the provider supplied one; a spend-cap refusal has none,
// and retrying it will not succeed.
func RateLimited(detail string, retryAfter *time.Duration) *errs.Error {
	e := errs.Wrap(ErrRateLimited, errs.CodeRateLimited, "model: "+detail)
	if retryAfter != nil {
		e = e.WithRetryAfter(*retryAfter)
	}
	return e
}

// BadResponse wraps an answer that cannot be used.
func BadResponse(detail string) *errs.Error {
	return errs.Wrap(ErrBadResponse, errs.CodeModelUnavailable, "model: "+detail)
}

// IsUnavailable reports whether err is a provider availability failure.
func IsUnavailable(err error) bool {
	return errors.Is(err, ErrUnavailable) || errs.HasCode(err, errs.CodeModelUnavailable)
}
