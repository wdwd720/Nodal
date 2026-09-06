// Package errs is the error model shared by every layer of the control
// plane: stable machine-readable codes, a single error type that carries
// them, and the RFC 9457 application/problem+json projection used at the
// HTTP boundary (goal PART 37).
//
// # Responsibility
//
//   - Code is the closed set of business and infrastructure rejection codes.
//     Every code has a deterministic HTTP status (HTTPStatus) and a human
//     title; both are table-driven and covered by tests so no code can fall
//     through to a generic 500 by accident.
//   - Error carries a Code, a client-safe Detail, optional structured Fields
//     (for VALIDATION_FAILED and similar), an optional RetryAfter hint and a
//     private cause. It supports errors.Is/As/Unwrap; errors.Is matches by
//     code so `errors.Is(err, errs.New(errs.CodeNotFound, ""))` is the
//     idiom for "is this a not-found error anywhere in the chain".
//   - ToProblem and WriteProblem project any error to a Problem document.
//     For INTERNAL (and any error that is not an *Error) the detail is the
//     constant "internal error" and fields are dropped: the cause text,
//     stack traces and internal identifiers are never sent to clients.
//
// # Conventions
//
// Return *Error (or Wrap) for anything that can reach an API boundary.
// Detail is written for the client: it must not contain secrets, SQL,
// provider payloads or hostnames; put those in the cause, which only logs
// see. Prefer one of the existing codes; adding a code means adding it to
// the registry in codes.go so the exhaustiveness tests cover it.
//
// # Never
//
// This package must never log, never read configuration, never import
// domain packages, and never include a cause or stack trace in a Problem.
package errs
