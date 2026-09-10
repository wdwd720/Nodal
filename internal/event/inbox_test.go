package event

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

func TestInbox_Process_ValidatesArguments(t *testing.T) {
	t.Parallel()
	in := NewInbox(clock.NewFake(fixedTime))
	ctx := context.Background()
	fn := func(context.Context, pgx.Tx) error { return nil }

	tests := []struct {
		name            string
		source, message string
		version         int
		field           string
	}{
		{"empty source", "", "m", 1, "source"},
		{"empty message id", "s", "", 1, "message_id"},
		{"control char", "s", "m\n", 1, "message_id"},
		{"too long", strings.Repeat("s", MaxInboxKeyLength+1), "m", 1, "source"},
		{"version zero", "s", "m", 0, "schema_version"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := in.Process(ctx, nil, tc.source, tc.message, tc.version, fn)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			ee, _ := errs.As(err)
			assert.Contains(t, ee.Fields, tc.field)

			err = in.MarkFailed(ctx, nil, tc.source, tc.message, tc.version, "", errors.New("x"))
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}

	_, err := in.Process(ctx, nil, "s", "m", 1, nil)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err), "nil fn")
	_, err = in.Process(ctx, nil, "s", "m", 1, fn)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err), "nil tx")
	err = in.MarkFailed(ctx, nil, "s", "m", 1, "", errors.New("x"))
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err), "nil querier")
}

func TestInbox_WithObserver(t *testing.T) {
	t.Parallel()
	in := NewInbox(nil)
	assert.NotNil(t, in.clk)
	assert.NotNil(t, in.obs)
	in2 := in.WithObserver(nil)
	assert.NotNil(t, in2.obs)
	assert.NotSame(t, in, in2)
}

func TestOutcome_String(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "PROCESSED", Processed.String())
	assert.Equal(t, "DUPLICATE", Duplicate.String())
	assert.Equal(t, "Outcome(9)", Outcome(9).String())
}

func TestHashPayload(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", HashPayload(nil))
	assert.Len(t, HashPayload([]byte(`{"a":1}`)), 64)
	assert.NotEqual(t, HashPayload([]byte("a")), HashPayload([]byte("b")))
}

func TestSafeErrorText(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "unknown error", safeErrorText(nil))
	assert.Equal(t, "boom", safeErrorText(errors.New("boom")))
	wrapped := errs.Wrap(errors.New("secret sql detail"), errs.CodeInsufficientBuyingPower, "not enough buying power")
	assert.Equal(t, "INSUFFICIENT_BUYING_POWER: not enough buying power", safeErrorText(wrapped), "cause is never stored")
	long := errors.New(strings.Repeat("x", maxLastError*2))
	assert.Len(t, safeErrorText(long), maxLastError)
}

func TestInProgressCarriesRetryAfter(t *testing.T) {
	t.Parallel()
	err := inProgress(nil)
	assert.Equal(t, errs.CodeIdempotencyInProgress, errs.CodeOf(err))
	ee, _ := errs.As(err)
	require.NotNil(t, ee.RetryAfter)
	assert.Equal(t, InProgressRetryAfter, *ee.RetryAfter)
}

func TestCheckHash(t *testing.T) {
	t.Parallel()
	a, empty := "a", ""
	assert.NoError(t, checkHash("s", "m", nil, "a"))
	assert.NoError(t, checkHash("s", "m", &empty, "a"))
	assert.NoError(t, checkHash("s", "m", &a, ""))
	assert.NoError(t, checkHash("s", "m", &a, "a"))
	err := checkHash("s", "m", &a, "b")
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
}
