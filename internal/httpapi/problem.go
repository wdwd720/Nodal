package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// errNotWired is the answer for an operation whose port is not configured in
// this deployment. It is a 422 with a stable code, never a 500 and never a
// fabricated success.
func errNotWired(what string) *errs.Error {
	return errs.Newf(errs.CodeUnsupported, "%s is not enabled in this deployment", what)
}

// classify maps any error reaching the HTTP boundary onto the stable code set
// of internal/errs. Errors that already carry a code keep it; the sentinels of
// internal/{security,auth,idempotency,db} are translated; everything else is
// INTERNAL, whose detail errs.ToProblem replaces with a constant so a driver
// message, SQL string, provider URL or secret can never leak.
//
// A cancelled or timed-out request is never reported as a business failure: a
// transport timeout says nothing about whether the work happened, so it maps
// to PROVIDER_UNAVAILABLE (503, retryable) rather than to a rejection.
func classify(ctx context.Context, err error) *errs.Error {
	if err == nil {
		return nil
	}
	if e, ok := errs.As(err); ok {
		return e
	}

	switch {
	case errors.Is(err, security.ErrStepUpRequired), errors.Is(err, auth.ErrStepUpNotSatisfied):
		return errs.Wrap(err, errs.CodeStepUpRequired, "recent strong authentication is required")
	case errors.Is(err, security.ErrSelfApproval):
		return errs.Wrap(err, errs.CodeForbidden, "an action cannot be approved by its proposer")
	case errors.Is(err, security.ErrCrossTenant):
		// Tenant scoping is FORBIDDEN by the errs contract, and the spec
		// declares 403 on every account-scoped route.
		return errs.Wrap(err, errs.CodeForbidden, "the principal does not own this account")
	case errors.Is(err, security.ErrForbidden):
		return errs.Wrap(err, errs.CodeForbidden, "the principal is not permitted to perform this operation")
	case errors.Is(err, security.ErrUnauthenticated), errors.Is(err, auth.ErrInvalidSession):
		return errs.Wrap(err, errs.CodeUnauthenticated, "authentication is required")

	case errors.Is(err, idempotency.ErrKeyReuseConflict):
		return errs.Wrap(err, errs.CodeInvalidIdempotencyReuse,
			"this Idempotency-Key was already used with a different request body")
	case errors.Is(err, idempotency.ErrInvalidArgument):
		return errs.Wrap(err, errs.CodeValidationFailed, "invalid Idempotency-Key")

	case errors.Is(err, db.ErrRetriesExhausted), db.IsSerializationFailure(err), db.IsDeadlock(err):
		return errs.Wrap(err, errs.CodeConflict, "concurrent update; retry the request")
	case db.IsLockTimeout(err), db.IsStatementTimeout(err):
		return errs.Wrap(err, errs.CodeProviderUnavailable, "the request could not be completed in time")

	// A cancelled or deadline-exceeded context is a transport fact, not a
	// financial one. Never let it become a definitive rejection.
	case errors.Is(err, context.Canceled):
		if ctx != nil && ctx.Err() != nil {
			return errs.Wrap(err, errs.CodeProviderUnavailable, "the request was cancelled before it completed")
		}
		return errs.Wrap(err, errs.CodeProviderUnavailable, "the request was cancelled before it completed")
	case errors.Is(err, context.DeadlineExceeded):
		return errs.Wrap(err, errs.CodeProviderUnavailable, "the request exceeded its deadline before it completed")
	}

	return errs.Wrap(err, errs.CodeInternal, "internal error")
}

// writeProblem renders err as application/problem+json for r.
func writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	e := classify(r.Context(), err)
	p := errs.ToProblem(e, r.URL.Path, observability.RequestID(r.Context()))
	errs.WriteProblem(w, p)
}

// requestBindingError turns the generated server's parameter and body binding
// failures into a VALIDATION_FAILED problem. The generated error text names
// the offending parameter and never contains request content, so it is safe to
// return; a body decode failure is reported without echoing the body.
func requestBindingError(err error) *errs.Error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "error binding") || strings.Contains(msg, "decode") ||
		strings.Contains(msg, "unmarshal") || strings.Contains(msg, "JSON") {
		return errs.New(errs.CodeValidationFailed, "the request body could not be decoded")
	}
	return errs.New(errs.CodeValidationFailed, msg)
}

// validationError builds a VALIDATION_FAILED error naming one field.
func validationError(field, detail string) *errs.Error {
	return errs.New(errs.CodeValidationFailed, detail).WithField("field", field)
}
