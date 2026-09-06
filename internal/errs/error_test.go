package errs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

var _ error = (*errs.Error)(nil)

func TestNew(t *testing.T) {
	t.Parallel()
	e := errs.New(errs.CodeNotFound, "order not found")
	require.Equal(t, errs.CodeNotFound, e.Code)
	require.Equal(t, "order not found", e.Detail)
	require.Nil(t, e.Fields)
	require.Nil(t, e.RetryAfter)
	require.NoError(t, e.Unwrap())
	require.Equal(t, "NOT_FOUND: order not found", e.Error())

	require.Equal(t, "NOT_FOUND", errs.New(errs.CodeNotFound, "").Error())
}

func TestNewf(t *testing.T) {
	t.Parallel()
	e := errs.Newf(errs.CodeInsufficientBuyingPower, "need %s, have %s", "100.00", "42.00")
	require.Equal(t, "INSUFFICIENT_BUYING_POWER: need 100.00, have 42.00", e.Error())
}

func TestWrap_PreservesCause(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("dial tcp: %w", io.ErrUnexpectedEOF)
	e := errs.Wrap(cause, errs.CodeProviderUnavailable, "alpaca unreachable")

	require.Equal(t, "PROVIDER_UNAVAILABLE: alpaca unreachable: dial tcp: unexpected EOF", e.Error())
	require.Same(t, cause, e.Unwrap())
	require.Same(t, cause, errors.Unwrap(e))
	require.ErrorIs(t, e, io.ErrUnexpectedEOF, "errors.Is reaches through to the cause")

	ef := errs.Wrapf(cause, errs.CodeProviderUnavailable, "%s unreachable", "alpaca")
	require.Equal(t, e.Error(), ef.Error())
	require.ErrorIs(t, ef, cause)
}

func TestWrap_NilCauseBehavesLikeNew(t *testing.T) {
	t.Parallel()
	e := errs.Wrap(nil, errs.CodeConflict, "version mismatch")
	require.NotNil(t, e)
	require.NoError(t, e.Unwrap())
	require.Equal(t, "CONFLICT: version mismatch", e.Error())
}

func TestWrap_ThroughFmtAndNested(t *testing.T) {
	t.Parallel()
	inner := errs.New(errs.CodeAccountFrozen, "frozen by compliance")
	mid := fmt.Errorf("posting journal: %w", inner)
	outer := errs.Wrap(mid, errs.CodeInvalidStateTransition, "cannot post")

	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(outer), "outermost code wins")
	require.True(t, errs.HasCode(outer, errs.CodeAccountFrozen), "inner code still findable")
	require.True(t, errors.Is(outer, errs.New(errs.CodeAccountFrozen, "")))
	require.True(t, errors.Is(outer, errs.New(errs.CodeInvalidStateTransition, "")))
	require.False(t, errors.Is(outer, errs.New(errs.CodeNotFound, "")))

	var got *errs.Error
	require.True(t, errors.As(outer, &got))
	require.Same(t, outer, got)

	require.True(t, errors.Is(outer, inner), "pointer identity through the chain")
}

func TestIs_Semantics(t *testing.T) {
	t.Parallel()
	err := errs.New(errs.CodeNotFound, "order 42 not found")

	cases := []struct {
		name   string
		target error
		want   bool
	}{
		{"same code, empty detail", errs.New(errs.CodeNotFound, ""), true},
		{"same code, same detail", errs.New(errs.CodeNotFound, "order 42 not found"), true},
		{"same code, different detail", errs.New(errs.CodeNotFound, "other"), false},
		{"different code, empty detail", errs.New(errs.CodeConflict, ""), false},
		{"different code, same detail", errs.New(errs.CodeConflict, "order 42 not found"), false},
		{"not an *Error", errors.New("NOT_FOUND"), false},
		{"typed nil *Error", (*errs.Error)(nil), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, errors.Is(err, tc.target))
		})
	}

	// A sentinel with an empty detail does not match a differently coded
	// error just because both have empty details.
	require.False(t, errors.Is(errs.New(errs.CodeConflict, ""), errs.New(errs.CodeNotFound, "")))
	// Is never matches a plain error target.
	require.False(t, errors.Is(err, context.Canceled))
}

func TestIs_WithJoin(t *testing.T) {
	t.Parallel()
	joined := errors.Join(errors.New("x"), errs.New(errs.CodeRateLimited, "slow down"))
	require.True(t, errors.Is(joined, errs.New(errs.CodeRateLimited, "")))
	require.True(t, errs.HasCode(joined, errs.CodeRateLimited))
	require.False(t, errs.HasCode(joined, errs.CodeNotFound))
	require.Equal(t, errs.CodeRateLimited, errs.CodeOf(joined))
}

func TestCodeOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want errs.Code
	}{
		{"nil", nil, ""},
		{"plain error", errors.New("boom"), errs.CodeInternal},
		{"context canceled", context.Canceled, errs.CodeInternal},
		{"direct", errs.New(errs.CodeForbidden, ""), errs.CodeForbidden},
		{"wrapped by fmt", fmt.Errorf("handler: %w", errs.New(errs.CodeForbidden, "")), errs.CodeForbidden},
		{"wrapped twice", fmt.Errorf("a: %w", fmt.Errorf("b: %w", errs.New(errs.CodeQuoteExpired, ""))), errs.CodeQuoteExpired},
		{"errs.Wrap of plain", errs.Wrap(errors.New("boom"), errs.CodeProviderUnavailable, ""), errs.CodeProviderUnavailable},
		{"nested errs", errs.Wrap(errs.New(errs.CodeNotFound, ""), errs.CodeConflict, ""), errs.CodeConflict},
		{"empty code struct", &errs.Error{Detail: "no code"}, errs.CodeInternal},
		{"typed nil *Error", (*errs.Error)(nil), errs.CodeInternal},
		{"unregistered code", errs.New("TYPO", ""), "TYPO"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, errs.CodeOf(tc.err))
		})
	}
}

func TestAs(t *testing.T) {
	t.Parallel()
	e := errs.New(errs.CodeStepUpRequired, "mfa")
	got, ok := errs.As(fmt.Errorf("wrap: %w", e))
	require.True(t, ok)
	require.Same(t, e, got)

	got, ok = errs.As(errors.New("plain"))
	require.False(t, ok)
	require.Nil(t, got)

	got, ok = errs.As(nil)
	require.False(t, ok)
	require.Nil(t, got)

	got, ok = errs.As((*errs.Error)(nil))
	require.False(t, ok)
	require.Nil(t, got)
}

func TestWithField_CopyOnWrite(t *testing.T) {
	t.Parallel()
	sentinel := errs.New(errs.CodeValidationFailed, "invalid request")
	a := sentinel.WithField("amount", "must be positive")
	b := a.WithField("asset", "unknown").WithField("amount", "too large")

	require.Nil(t, sentinel.Fields, "sentinel untouched")
	require.Equal(t, map[string]any{"amount": "must be positive"}, a.Fields)
	require.Equal(t, map[string]any{"amount": "too large", "asset": "unknown"}, b.Fields)
	require.NotSame(t, sentinel, a)
	require.NotSame(t, a, b)
	require.Equal(t, sentinel.Code, b.Code)
	require.Equal(t, sentinel.Detail, b.Detail)

	// The cause survives copying.
	cause := errors.New("cause")
	w := errs.Wrap(cause, errs.CodeConflict, "c").WithField("k", 1)
	require.ErrorIs(t, w, cause)

	// A nil receiver yields a usable INTERNAL error rather than a panic.
	var nilErr *errs.Error
	require.Equal(t, errs.CodeInternal, nilErr.WithField("k", "v").Code)
}

func TestWithFields(t *testing.T) {
	t.Parallel()
	base := errs.New(errs.CodeValidationFailed, "").WithField("a", 1)
	m := map[string]any{"b": 2, "a": 3}
	got := base.WithFields(m)
	require.Equal(t, map[string]any{"a": 3, "b": 2}, got.Fields)
	require.Equal(t, map[string]any{"a": 1}, base.Fields)

	m["c"] = 4
	require.NotContains(t, got.Fields, "c", "input map is copied, not aliased")

	require.Equal(t, base.Fields, base.WithFields(nil).Fields)
}

func TestWithRetryAfter(t *testing.T) {
	t.Parallel()
	base := errs.New(errs.CodeRateLimited, "")
	got := base.WithRetryAfter(3 * time.Second)
	require.Nil(t, base.RetryAfter)
	require.NotNil(t, got.RetryAfter)
	require.Equal(t, 3*time.Second, *got.RetryAfter)

	// Copies do not share the RetryAfter pointer.
	again := got.WithField("k", "v")
	*again.RetryAfter = time.Minute
	require.Equal(t, 3*time.Second, *got.RetryAfter)
}

func TestError_NilReceiver(t *testing.T) {
	t.Parallel()
	var e *errs.Error
	require.Equal(t, "<nil>", e.Error())
	require.NoError(t, e.Unwrap())
	require.False(t, e.Is(errs.New(errs.CodeInternal, "")))
}

func TestError_EmptyCodeRendersInternal(t *testing.T) {
	t.Parallel()
	e := &errs.Error{Detail: "oops"}
	require.Equal(t, "INTERNAL: oops", e.Error())
}

func TestHasCode(t *testing.T) {
	t.Parallel()
	require.False(t, errs.HasCode(nil, errs.CodeNotFound))
	require.False(t, errs.HasCode(errors.New("x"), errs.CodeNotFound))
	require.True(t, errs.HasCode(errs.New(errs.CodeNotFound, ""), errs.CodeNotFound))
	require.True(t, errs.HasCode(fmt.Errorf("w: %w", errs.New(errs.CodeNotFound, "")), errs.CodeNotFound))
	require.False(t, errs.HasCode(errs.New(errs.CodeNotFound, ""), errs.CodeConflict))
}
