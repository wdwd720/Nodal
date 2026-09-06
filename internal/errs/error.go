package errs

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

// Error is the error type for anything that can reach an API boundary.
//
// Detail is client-facing and must be safe to return verbatim. The wrapped
// cause is for logs only and is never projected into a Problem. Error
// values are immutable once created: WithField and WithRetryAfter return
// modified copies so package-level sentinels can be shared safely.
type Error struct {
	Code       Code
	Detail     string
	Fields     map[string]any
	RetryAfter *time.Duration
	cause      error
}

// New returns an Error with the given code and client-safe detail.
func New(code Code, detail string) *Error {
	return &Error{Code: code, Detail: detail}
}

// Newf is New with a formatted detail. Use Wrap, not a %w verb, to attach a
// cause.
func Newf(code Code, format string, a ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, a...)}
}

// Wrap returns an Error with the given code and detail whose cause is err.
// The cause is reachable through errors.Is, errors.As and Unwrap, and is
// included in Error() for logs, but never in a Problem.
//
// Wrap(nil, code, detail) is equivalent to New(code, detail): it never
// returns a nil *Error, so `return errs.Wrap(err, ...)` is safe only inside
// an `if err != nil` block, as with any error constructor.
func Wrap(err error, code Code, detail string) *Error {
	return &Error{Code: code, Detail: detail, cause: err}
}

// Wrapf is Wrap with a formatted detail.
func Wrapf(err error, code Code, format string, a ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, a...), cause: err}
}

// Error renders "CODE: detail: cause" for logs, omitting empty parts.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var sb strings.Builder
	if e.Code == "" {
		sb.WriteString(string(CodeInternal))
	} else {
		sb.WriteString(string(e.Code))
	}
	if e.Detail != "" {
		sb.WriteString(": ")
		sb.WriteString(e.Detail)
	}
	if e.cause != nil {
		sb.WriteString(": ")
		sb.WriteString(e.cause.Error())
	}
	return sb.String()
}

// Unwrap returns the wrapped cause, if any.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Is implements the errors.Is protocol by code: e matches target when
// target is an *Error with the same Code and either an empty Detail or the
// identical Detail. Because errors.Is walks the whole chain, the idiom
//
//	errors.Is(err, errs.New(errs.CodeNotFound, ""))
//
// is true if any error in err's chain carries CodeNotFound, regardless of
// how it was wrapped afterwards. Use CodeOf for the outermost code only.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || t == nil || e == nil {
		return false
	}
	if t.Code != e.Code {
		return false
	}
	return t.Detail == "" || t.Detail == e.Detail
}

// WithField returns a copy of e with k set to v in Fields. The receiver is
// not modified.
func (e *Error) WithField(k string, v any) *Error {
	c := e.clone()
	if c.Fields == nil {
		c.Fields = make(map[string]any, 1)
	}
	c.Fields[k] = v
	return c
}

// WithFields returns a copy of e with every entry of fields merged into
// Fields. The receiver is not modified.
func (e *Error) WithFields(fields map[string]any) *Error {
	c := e.clone()
	if len(fields) == 0 {
		return c
	}
	if c.Fields == nil {
		c.Fields = make(map[string]any, len(fields))
	}
	maps.Copy(c.Fields, fields)
	return c
}

// WithRetryAfter returns a copy of e advising clients to wait d before
// retrying. WriteProblem emits it as the Retry-After header.
func (e *Error) WithRetryAfter(d time.Duration) *Error {
	c := e.clone()
	c.RetryAfter = &d
	return c
}

// clone returns a shallow copy with its own Fields map.
func (e *Error) clone() *Error {
	if e == nil {
		return &Error{Code: CodeInternal}
	}
	c := *e
	c.Fields = maps.Clone(e.Fields)
	if e.RetryAfter != nil {
		d := *e.RetryAfter
		c.RetryAfter = &d
	}
	return &c
}

// CodeOf returns the code of the outermost *Error in err's chain, INTERNAL
// if err is not an *Error (or is an *Error without a code), and the empty
// Code if err is nil.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	e, ok := As(err)
	if !ok || e.Code == "" {
		return CodeInternal
	}
	return e.Code
}

// As returns the outermost *Error in err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e, true
	}
	return nil, false
}

// HasCode reports whether any *Error in err's chain carries code. It is
// errors.Is(err, New(code, "")) without the allocation.
func HasCode(err error, code Code) bool {
	for err != nil {
		if e, ok := err.(*Error); ok && e != nil && e.Code == code {
			return true
		}
		switch x := err.(type) {
		case interface{ Unwrap() error }:
			err = x.Unwrap()
		case interface{ Unwrap() []error }:
			for _, inner := range x.Unwrap() {
				if HasCode(inner, code) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}
