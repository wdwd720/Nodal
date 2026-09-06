package model

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// BudgetKind names which budget refused a call, for the error field and the
// agent_runs skip reason.
type BudgetKind string

// Budget kinds.
const (
	BudgetModel BudgetKind = "MODEL"
	BudgetData  BudgetKind = "DATA"
)

// Budget caps what one compile request or one agent run may spend. It is a
// value, not a service: the caller loads the remaining allowance from
// persisted counters (model_calls sums, compile_attempts costs) and passes
// it in, so an in-memory counter can never be the thing that authorizes
// spend.
type Budget struct {
	Kind BudgetKind
	// MaxCalls is the number of provider dials still permitted. Zero means
	// no call may be made.
	MaxCalls int
	// MaxSpend is the remaining allowance. Zero means no spend is permitted.
	MaxSpend money.USD
	// CallsUsed and SpendUsed are what has already been consumed within the
	// window this Budget describes.
	CallsUsed int
	SpendUsed money.USD
}

// NewBudget returns a budget with nothing consumed yet.
func NewBudget(kind BudgetKind, maxCalls int, maxSpend money.USD) Budget {
	return Budget{Kind: kind, MaxCalls: maxCalls, MaxSpend: maxSpend}
}

// RemainingCalls is how many dials are still allowed.
func (b Budget) RemainingCalls() int {
	if b.CallsUsed >= b.MaxCalls {
		return 0
	}
	return b.MaxCalls - b.CallsUsed
}

// RemainingSpend is the allowance left. It never goes below zero.
func (b Budget) RemainingSpend() money.USD {
	rem, err := b.MaxSpend.Sub(b.SpendUsed)
	if err != nil || rem.IsNegative() {
		return money.USDFromMinor(0)
	}
	return rem
}

// Exhausted reports whether no further call may be made.
func (b Budget) Exhausted() bool {
	return b.RemainingCalls() <= 0 || !b.RemainingSpend().IsPositive()
}

// CheckBeforeCall is called before every dial. It refuses when the call
// count or the spend allowance is used up, and when the estimated cost of
// the next call would exceed what is left — the check happens before the
// provider is contacted, so an exhausted budget costs nothing.
func (b Budget) CheckBeforeCall(estimated money.USD) error {
	if b.RemainingCalls() <= 0 {
		return errs.Newf(errs.CodeBudgetExhausted, "model: %s budget exhausted: %d of %d calls used", b.Kind, b.CallsUsed, b.MaxCalls).
			WithField("budget", string(b.Kind)).
			WithField("calls_used", b.CallsUsed).
			WithField("max_calls", b.MaxCalls)
	}
	remaining := b.RemainingSpend()
	if !remaining.IsPositive() {
		return errs.Newf(errs.CodeBudgetExhausted, "model: %s budget exhausted: %s of %s spent", b.Kind, b.SpendUsed, b.MaxSpend).
			WithField("budget", string(b.Kind)).
			WithField("spend_used", b.SpendUsed.String()).
			WithField("max_spend", b.MaxSpend.String())
	}
	if estimated.IsPositive() && estimated.Cmp(remaining) > 0 {
		return errs.Newf(errs.CodeBudgetExhausted, "model: %s budget would be exceeded: estimated %s, remaining %s", b.Kind, estimated, remaining).
			WithField("budget", string(b.Kind)).
			WithField("estimated", estimated.String()).
			WithField("remaining", remaining.String())
	}
	return nil
}

// Consume records one completed call. It returns the updated budget; the
// caller persists the real counters, this is only the in-request view.
func (b Budget) Consume(cost money.USD) (Budget, error) {
	spent, err := b.SpendUsed.Add(cost)
	if err != nil {
		return b, errs.Wrap(err, errs.CodeOverflow, "model: budget spend overflow")
	}
	b.CallsUsed++
	b.SpendUsed = spent
	return b, nil
}
