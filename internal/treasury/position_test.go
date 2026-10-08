package treasury

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func TestPosition_AverageCostIsReleasedRoundedUpAndFullyOnClose(t *testing.T) {
	var p Position
	m, err := p.Increase(q(3), q(100), q(0))
	require.NoError(t, err)
	p = m.After
	// One of three units releases ceil(100 / 3) = 34.
	m, err = p.Decrease(q(1), q(40), q(1))
	require.NoError(t, err)
	assert.Equal(t, "34", m.CostRemoved.String())
	assert.Equal(t, "5", m.Realized.String(), "40 - 34 - 1")
	p = m.After
	assert.Equal(t, "66", p.Cost.String())
	// Closing releases exactly what is left, whatever the rounding before.
	m, err = p.Decrease(q(2), q(60), q(0))
	require.NoError(t, err)
	assert.Equal(t, "66", m.CostRemoved.String())
	assert.True(t, m.After.Quantity.IsZero() && m.After.Cost.IsZero())

	_, err = Position{}.Decrease(q(1), q(1), q(0))
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "nothing can be sold from nothing")
	_, err = Position{Quantity: q(0), Cost: q(5)}.Increase(q(1), q(1), q(0))
	assert.Error(t, err, "a zero position with cost is impossible")
	_, err = p.Increase(q(0), q(1), q(0))
	assert.Error(t, err)
	_, err = p.Increase(q(1), q(-1), q(0))
	assert.Error(t, err)
}

// TestProp_PositionAccountingConservesCost: over any sequence of buys and
// sells, every unit of cost put into a position is either still in it or was
// released exactly once; realized PnL is exactly proceeds less released cost
// less fees; each release is the rounded-up average cost; a closed position
// holds nothing. Nothing leaks in either direction.
func TestProp_PositionAccountingConservesCost(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var p Position
		var costIn, released, proceeds, fees, realized money.Quantity
		steps := rapid.IntRange(1, 40).Draw(t, "steps")
		for i := 0; i < steps; i++ {
			fee := q(rapid.Int64Range(0, 1_000).Draw(t, "fee"))
			if p.Quantity.IsZero() || rapid.Bool().Draw(t, "buy") {
				qty := q(rapid.Int64Range(1, 1_000_000).Draw(t, "buyQty"))
				value := q(rapid.Int64Range(0, 1_000_000_000).Draw(t, "cost"))
				m, err := p.Increase(qty, value, fee)
				if err != nil {
					t.Fatal(err)
				}
				costIn, fees, realized = costIn.Add(value), fees.Add(fee), realized.Add(m.Realized)
				p = m.After
				continue
			}
			max, _ := p.Quantity.Int64()
			qty := q(rapid.Int64Range(1, max).Draw(t, "sellQty"))
			value := q(rapid.Int64Range(0, 1_000_000_000).Draw(t, "proceeds"))
			m, err := p.Decrease(qty, value, fee)
			if err != nil {
				t.Fatal(err)
			}
			// The release is ceil(cost x qty / quantity), exactly.
			if qty.Cmp(p.Quantity) < 0 {
				lhs := m.CostRemoved.Mul(p.Quantity)
				rhs := p.Cost.Mul(qty)
				if lhs.Cmp(rhs) < 0 || m.CostRemoved.Sub(q(1)).Mul(p.Quantity).Cmp(rhs) >= 0 {
					t.Fatalf("released %s of %s for %s of %s: not the rounded-up average", m.CostRemoved, p.Cost, qty, p.Quantity)
				}
			} else if !m.CostRemoved.Equal(p.Cost) {
				t.Fatalf("closing released %s of %s", m.CostRemoved, p.Cost)
			}
			released, proceeds, fees, realized = released.Add(m.CostRemoved), proceeds.Add(value), fees.Add(fee), realized.Add(m.Realized)
			p = m.After
		}
		if !costIn.Equal(released.Add(p.Cost)) {
			t.Fatalf("cost in %s != released %s + remaining %s", costIn, released, p.Cost)
		}
		if !realized.Equal(proceeds.Sub(released).Sub(fees)) {
			t.Fatalf("realized %s != proceeds %s - released %s - fees %s", realized, proceeds, released, fees)
		}
		if p.Quantity.IsZero() && !p.Cost.IsZero() {
			t.Fatalf("a closed position kept cost %s", p.Cost)
		}
		if p.Quantity.IsNegative() || p.Cost.IsNegative() {
			t.Fatalf("an impossible position %s / %s", p.Quantity, p.Cost)
		}
	})
}
