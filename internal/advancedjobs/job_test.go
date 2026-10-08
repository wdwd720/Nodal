package advancedjobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// TestRedact_KeepsRefusalsAndHidesInternals: a subsystem's refusal is
// written for an API boundary and keeps its code and detail; anything
// internal is reduced to its class.
func TestRedact_KeepsRefusalsAndHidesInternals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		err        error
		wantCode   string
		wantReason string
		hidden     string
	}{
		{"refusal", errs.New(errs.CodeStaleMarketData, "no fresh mark at expiry"), "STALE_MARKET_DATA", "no fresh mark at expiry", ""},
		{"wrapped refusal", fmt.Errorf("context: %w", errs.New(errs.CodeConflict, "orders still live")), "CONFLICT", "orders still live", ""},
		{"internal", errs.New(errs.CodeInternal, "row 42 of table secrets was odd"), "INTERNAL", "", "secrets"},
		{"wrapped internal", errs.Wrap(errors.New("dial tcp 10.0.0.7:5432"), errs.CodeInternal, "orders: read book"), "INTERNAL", "", "10.0.0.7"},
		{"plain error", errors.New("password=hunter2"), "INTERNAL", "", "hunter2"},
		{"driver error", &pgconn.PgError{Code: "23505", Message: "duplicate key value (id)=(abc)"}, "INTERNAL", "", "abc"},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), CodeTimeout, "", ""},
		{"cancelled", context.Canceled, CodeCancelled, "", ""},
		{"serialization", &pgconn.PgError{Code: "40001"}, CodeRetriesExhausted, "", ""},
		{"retries exhausted", fmt.Errorf("%w: boom", db.ErrRetriesExhausted), CodeRetriesExhausted, "", ""},
	} {
		code, reason := Redact(tc.err)
		assert.Equal(t, tc.wantCode, code, tc.name)
		if tc.wantReason != "" {
			assert.Equal(t, tc.wantReason, reason, tc.name)
		}
		assert.NotEmpty(t, reason, tc.name)
		if tc.hidden != "" {
			assert.NotContains(t, reason, tc.hidden, tc.name)
		}
	}
	code, reason := Redact(nil)
	assert.Empty(t, code)
	assert.Empty(t, reason)
}

// TestRedact_BoundsAndFlattensAReason: a reason is one printable line of
// bounded length, whatever the detail carried.
func TestRedact_BoundsAndFlattensAReason(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1000)
	_, reason := Redact(errs.New(errs.CodeConflict, "line one\nline two\t"+long))
	assert.NotContains(t, reason, "\n")
	assert.NotContains(t, reason, "\t")
	assert.LessOrEqual(t, len([]rune(reason)), maxReasonRunes+3)
	assert.True(t, strings.HasPrefix(reason, "line one line two"))
}

// TestCatalog_EveryJobIsDescribedOnceWithASaneDefault: the catalog is the
// one place the jobs' names, defaults and bounds live; every name is unique,
// valid as a configuration key, and resolves to a schedule the scheduler
// accepts.
func TestCatalog_EveryJobIsDescribedOnceWithASaneDefault(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, d := range Catalog() {
		assert.False(t, seen[d.Name], "%s is described twice", d.Name)
		seen[d.Name] = true
		assert.Regexp(t, `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`, d.Name, "a job name is a valid override key")
		assert.Positive(t, d.Default.Interval, d.Name)
		assert.Positive(t, d.Default.Batch, d.Name)
		assert.NotEmpty(t, d.Calls, d.Name)
		assert.NotEmpty(t, d.KillSwitch, d.Name)
		if d.MaxBatch > 0 {
			assert.LessOrEqual(t, d.Default.Batch, d.MaxBatch, d.Name)
		}
		s := spec(d.Name, Settings{})
		assert.Equal(t, d.Default, s.Settings, "no override means the default")
	}
	assert.Len(t, seen, 24)
}

// TestCatalog_OverridesApplyAndAreClamped: an override replaces a default,
// a zero override keeps it, and a batch beyond the subsystem's bound is
// clamped to the bound.
func TestCatalog_OverridesApplyAndAreClamped(t *testing.T) {
	t.Parallel()
	d, ok := Describe(NameMarginLiquidation)
	require.True(t, ok)
	got := d.Resolve(Settings{Interval: 10 * time.Second, Batch: 5000})
	assert.Equal(t, 10*time.Second, got.Interval)
	assert.Equal(t, 100, got.Batch, "ProcessLiquidationQueue takes at most 100")
	assert.Equal(t, d.Default, d.Resolve(Settings{}))

	d, _ = Describe(NameMarginInterest)
	assert.Equal(t, maxBatch, d.Resolve(Settings{Batch: maxBatch * 2}).Batch)

	assert.Equal(t, Spec{Name: "nope.never"}, spec("nope.never", Settings{}), "an unknown name gets no schedule")
	_, err := NewScheduler([]Job{&fakeJob{spec: spec("nope.never", Settings{})}}, Options{Clock: nil, Logger: quietLogger()})
	require.Error(t, err)
}

// TestCatalog_ValidateOverridesRefusesUnknownJobs: a typo in a schedule is a
// failed start.
func TestCatalog_ValidateOverridesRefusesUnknownJobs(t *testing.T) {
	t.Parallel()
	require.NoError(t, ValidateOverrides(map[string]time.Duration{NameMarginInterest: time.Hour},
		map[string]int{NameOrdersBookProof: 10}))
	err := ValidateOverrides(map[string]time.Duration{"margin.intrest": time.Hour}, nil)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	require.Error(t, ValidateOverrides(nil, map[string]int{"orders.book_prof": 1}))
}
