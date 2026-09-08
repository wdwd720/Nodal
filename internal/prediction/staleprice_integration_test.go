//go:build integration

package prediction

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

// A dead feed does not score predictions (F-59).
//
// `PriceAsOf` bounds its read from above -- `received_at <= asOf` -- so a
// resolution can never see the future. That property was tested thoroughly and
// holds. Nothing bounded it from below, and the resolver compared neither
// price's age to anything.
//
// So when the feed for an instrument stopped, both reads returned the same row:
// the newest price that existed, however old. `returnInBPS` on one observation
// against itself is exactly zero, so the realized direction was FLAT, the
// drawdown zero, and `Validate` passed because both price refs were non-empty.
// Every open prediction on that instrument was written off as a miss against a
// market nobody had observed -- and `prediction_outcomes` carries
// `forbid_mutation`, so the score could never be corrected.
//
// The existing coverage could not see this. `TestResolverRefusesWhenThereIsNoPrice`
// covers *zero* rows; `TestResolveScoresFromPointInTimePrices` writes its prices
// one second before the commitment and exactly at the cut-off, and its one
// "must not be used" case is a price from the future. The look-ahead direction
// was tested from every angle and the look-behind direction not at all.

func TestIntegration_ResolverRefusesADeadFeed(t *testing.T) {
	f := newFixture(t)
	p := f.commit(t, f.draft(t, "enter", time.Hour, "0.8"))

	// One price, from well before the prediction was even made, and nothing
	// afterwards. This is what an instrument whose feed died looks like.
	f.writePrice(t, "10000", 2, p.CommittedAt.Add(-6*time.Hour))
	f.clk.Set(p.HorizonEnd().Add(time.Minute))

	r, err := NewResolver(f.clk, NewPriceReader(), 15*time.Minute)
	require.NoError(t, err)

	_, err = r.Resolve(context.Background(), testDB, p)
	require.Error(t, err, "the resolver scored a prediction against a feed that stopped before it was made")
	assert.ErrorIs(t, err, ErrStalePrice)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))

	// Nothing was written. An outcome here would be permanent: the table is
	// append-only and a wrong score cannot be taken back.
	var outcomes int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM prediction_outcomes WHERE prediction_id = $1`, p.ID).Scan(&outcomes))
	assert.Zero(t, outcomes, "a refusal must leave the prediction unresolved, not half-resolved")
}

// TestIntegration_ResolverRefusesOneObservationSpanningTheWindow is the same
// defect in its subtler form: prices exist on both sides of the read, but
// PriceAsOf returns the *same row* for both cut-offs because none arrived
// during the window. The return is then zero by construction rather than by
// measurement.
//
// This case needs no configured age at all to be wrong, which is why the
// resolver refuses it unconditionally: a generous max age would let it through
// while the outcome remained a statement about our data rather than the market.
func TestIntegration_ResolverRefusesOneObservationSpanningTheWindow(t *testing.T) {
	f := newFixture(t)
	p := f.commit(t, f.draft(t, "enter", time.Hour, "0.8"))

	// A single price, recent enough to satisfy any age bound, and no second one.
	f.writePrice(t, "10000", 2, p.CommittedAt.Add(-time.Second))
	f.clk.Set(p.HorizonEnd().Add(time.Minute))

	r, err := NewResolver(f.clk, NewPriceReader(), 24*time.Hour)
	require.NoError(t, err)

	_, err = r.Resolve(context.Background(), testDB, p)
	require.Error(t, err, "one observation was used as both endpoints and scored as FLAT")
	assert.ErrorIs(t, err, ErrStalePrice)
	assert.Contains(t, err.Error(), "no price movement to score")

	// The positive control, and the reason this test is not simply "refuse more
	// often": one further price inside the window makes it resolvable, and it
	// resolves to the direction that price actually moved.
	f.writePrice(t, "10100", 2, p.CommittedAt.Add(30*time.Minute))
	o, err := r.Resolve(context.Background(), testDB, p)
	require.NoError(t, err, "a window with a real second observation must still resolve")
	assert.Equal(t, DirectionUp, o.RealizedDirection)
}

// TestIntegration_ResolverRefusesAStaleStart covers the other endpoint. A fresh
// closing price against a baseline from days ago measures a real movement from
// an unreal starting point, which is the harder case to notice because the
// outcome looks plausible.
func TestIntegration_ResolverRefusesAStaleStart(t *testing.T) {
	f := newFixture(t)
	p := f.commit(t, f.draft(t, "enter", time.Hour, "0.8"))

	f.writePrice(t, "10000", 2, p.CommittedAt.Add(-48*time.Hour))
	f.writePrice(t, "12000", 2, p.HorizonEnd())
	f.clk.Set(p.HorizonEnd().Add(time.Minute))

	r, err := NewResolver(f.clk, NewPriceReader(), 15*time.Minute)
	require.NoError(t, err)

	_, err = r.Resolve(context.Background(), testDB, p)
	require.Error(t, err, "a two-day-old baseline was scored as this window's opening price")
	assert.ErrorIs(t, err, ErrStalePrice)
	assert.Contains(t, err.Error(), "start price")
}

// TestResolverRequiresAMaximumPriceAge: the constructor fails closed rather
// than choosing a number for the caller. A default buried here is how the
// original defect would come back -- silently, in whichever deployment forgot
// to set it.
func TestResolverRequiresAMaximumPriceAge(t *testing.T) {
	for _, age := range []time.Duration{0, -time.Second} {
		_, err := NewResolver(clock.NewFake(time.Now().UTC()), NewPriceReader(), age)
		require.Error(t, err, "a resolver with a %s maximum price age was built", age)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		assert.Contains(t, err.Error(), "maximum price age")
	}
	// The positive control: a real value is accepted.
	r, err := NewResolver(clock.NewFake(time.Now().UTC()), NewPriceReader(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, r)
}
