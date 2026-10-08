package predmarket

import (
	"fmt"
	"sort"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// The arithmetic of complete sets, payouts and refunds. Everything here is
// pure and exact: quantities are base units in money.Quantity (big.Int), and
// every division names its rounding mode.

// ValidateSetQuantity checks a complete-set quantity for issuance or
// redemption: a positive whole number of sets, i.e. a positive multiple of
// one share (1,000,000 base units). A complete set is one share of every
// outcome and costs exactly one collateral unit.
func ValidateSetQuantity(q money.Quantity) error {
	if !q.IsPositive() {
		return errs.New(errs.CodeValidationFailed, "predmarket: a complete-set quantity must be positive").
			WithField("quantity", q.String())
	}
	if _, err := q.Div(ShareUnit(), money.RoundExact); err != nil {
		return errs.New(errs.CodeValidationFailed,
			"predmarket: complete sets are issued and redeemed whole: the quantity must be a multiple of 1000000 base units").
			WithField("quantity", q.String())
	}
	return nil
}

// PayoutFor returns the collateral, in base units, that a holding is paid at
// settlement: every share base unit of the winning outcome pays unit/ShareUnit
// collateral base units. A market's unit is one collateral unit of a
// six-decimal asset, so the ratio is exactly one and RoundExact never fails;
// if it ever could, the payout is refused rather than rounded.
func PayoutFor(shares []money.Quantity, winning int, unit money.Quantity) (money.Quantity, error) {
	if winning < 0 || winning >= len(shares) {
		return money.Quantity{}, errs.Newf(errs.CodeValidationFailed, "predmarket: winning outcome %d out of range", winning)
	}
	w := shares[winning]
	if w.IsNegative() {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "predmarket: a holding cannot be negative")
	}
	paid, err := w.MulDiv(unit, ShareUnit(), money.RoundExact)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodePrecisionLoss, "predmarket: a payout would need rounding")
	}
	return paid, nil
}

// RefundRatePerShare returns floor(unit / outcomes): the collateral base units
// one whole share of any outcome refunds when a market is voided. Each share
// is worth 1/N of a unit in a void; the rate rounds that down (RoundDown,
// toward zero), so a complete set refunds N × rate = unit − (unit mod N) and
// the unit mod N remainder stays with the escrow as a disclosed residual.
func RefundRatePerShare(unit money.Quantity, outcomes int) (money.Quantity, error) {
	if outcomes < MinOutcomes || outcomes > MaxOutcomes {
		return money.Quantity{}, errs.Newf(errs.CodeValidationFailed, "predmarket: %d outcomes is out of range", outcomes)
	}
	if !unit.IsPositive() {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "predmarket: the unit must be positive")
	}
	return unit.Div(money.QuantityFromInt64(int64(outcomes)), money.RoundDown)
}

// RefundFor returns the refund owed to one holder in a void:
// floor(H × rate / ShareUnit) collateral base units, where H is the holder's
// share base units summed across every outcome. The rounding happens once
// per holder, toward zero (RoundDown), always in favour of the escrow, so
// Σ refunds ≤ escrow for every distribution of holdings.
func RefundFor(shares []money.Quantity, rate money.Quantity) (money.Quantity, error) {
	if rate.IsNegative() {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "predmarket: the refund rate cannot be negative")
	}
	total := money.Quantity{}
	for _, s := range shares {
		if s.IsNegative() {
			return money.Quantity{}, errs.New(errs.CodeValidationFailed, "predmarket: a holding cannot be negative")
		}
		total = total.Add(s)
	}
	return total.MulDiv(rate, ShareUnit(), money.RoundDown)
}

// Holding is one holder's balance of every outcome share of a market, in base
// units, indexed by outcome.
type Holding struct {
	AccountID accounts.AccountID
	Shares    []money.Quantity
}

// PlanLine is what one holder is paid and which shares are retired.
type PlanLine struct {
	AccountID accounts.AccountID
	Shares    []money.Quantity
	Amount    money.Quantity
}

// SettlementPlan is the complete, deterministic account of a settlement: one
// line per holder in account order, and totals that conserve the escrow.
type SettlementPlan struct {
	Kind           SettlementKind
	OutcomeCount   int
	Unit           money.Quantity
	WinningOutcome int            // PAYOUT only; -1 for a refund
	RefundRate     money.Quantity // REFUND only
	Policy         RoundingPolicy
	EscrowBefore   money.Quantity
	Lines          []PlanLine
	TotalPaid      money.Quantity
	Residual       money.Quantity
}

// PlanPayout plans the settlement of a RESOLVED market. It refuses unless
// the holdings account for every outstanding share and the escrow backs
// exactly the outstanding complete sets; then every winning share pays one
// unit, every share is retired, and Σ payouts = escrow exactly.
func PlanPayout(escrow money.Quantity, outstanding []money.Quantity, holdings []Holding, winning int, unit money.Quantity) (SettlementPlan, error) {
	n := len(outstanding)
	if winning < 0 || winning >= n {
		return SettlementPlan{}, errs.Newf(errs.CodeValidationFailed, "predmarket: winning outcome %d out of range", winning)
	}
	lines, err := planLines(escrow, outstanding, holdings, money.Quantity{})
	if err != nil {
		return SettlementPlan{}, err
	}
	p := SettlementPlan{
		Kind: SettlementPayout, OutcomeCount: n, Unit: unit, WinningOutcome: winning,
		Policy: RoundingExact, EscrowBefore: escrow,
	}
	for i := range lines {
		amt, err := PayoutFor(lines[i].Shares, winning, unit)
		if err != nil {
			return SettlementPlan{}, err
		}
		lines[i].Amount = amt
		p.TotalPaid = p.TotalPaid.Add(amt)
	}
	p.Lines = lines
	p.Residual = escrow.Sub(p.TotalPaid)
	if !p.Residual.IsZero() {
		return SettlementPlan{}, errs.New(errs.CodeReconciliationRequired,
			"predmarket: the payouts do not exhaust the escrow; the books do not reconcile").
			WithField("escrow", escrow.String()).WithField("total_paid", p.TotalPaid.String())
	}
	return p, nil
}

// PlanRefund plans the refunds of a voided market under RoundingFloorPerHolder.
// The escrow must back exactly the outstanding complete sets beforehand; the
// residual it returns is escrow − Σ refunds, never negative.
func PlanRefund(escrow money.Quantity, outstanding []money.Quantity, holdings []Holding, unit money.Quantity) (SettlementPlan, error) {
	n := len(outstanding)
	rate, err := RefundRatePerShare(unit, n)
	if err != nil {
		return SettlementPlan{}, err
	}
	lines, err := planLines(escrow, outstanding, holdings, money.Quantity{})
	if err != nil {
		return SettlementPlan{}, err
	}
	p := SettlementPlan{
		Kind: SettlementRefund, OutcomeCount: n, Unit: unit, WinningOutcome: -1, RefundRate: rate,
		Policy: RoundingFloorPerHolder, EscrowBefore: escrow,
	}
	for i := range lines {
		amt, err := RefundFor(lines[i].Shares, rate)
		if err != nil {
			return SettlementPlan{}, err
		}
		lines[i].Amount = amt
		p.TotalPaid = p.TotalPaid.Add(amt)
	}
	p.Lines = lines
	p.Residual = escrow.Sub(p.TotalPaid)
	if p.Residual.IsNegative() {
		// Unreachable by the proof in RefundFor; checked because an escrow
		// that pays more than it holds is the one failure that must never
		// reach the ledger.
		return SettlementPlan{}, errs.New(errs.CodeInternal, "predmarket: refunds would exceed the escrow")
	}
	return p, nil
}

// planLines validates the holdings against the outstanding shares and the
// escrow, and returns one line per holder with any share, sorted by account.
func planLines(escrow money.Quantity, outstanding []money.Quantity, holdings []Holding, residual money.Quantity) ([]PlanLine, error) {
	n := len(outstanding)
	if n < MinOutcomes || n > MaxOutcomes {
		return nil, errs.Newf(errs.CodeValidationFailed, "predmarket: %d outcomes is out of range", n)
	}
	if escrow.IsNegative() {
		return nil, errs.New(errs.CodeReconciliationRequired, "predmarket: the escrow is negative")
	}
	held := make([]money.Quantity, n)
	seen := make(map[accounts.AccountID]bool, len(holdings))
	lines := make([]PlanLine, 0, len(holdings))
	for _, h := range holdings {
		if len(h.Shares) != n {
			return nil, errs.Newf(errs.CodeInternal, "predmarket: a holding names %d outcomes, the market has %d", len(h.Shares), n)
		}
		if h.AccountID.IsZero() || seen[h.AccountID] {
			return nil, errs.New(errs.CodeInternal, "predmarket: holdings must name each account once")
		}
		seen[h.AccountID] = true
		holdsShares := false
		shares := make([]money.Quantity, n)
		for i, s := range h.Shares {
			if s.IsNegative() {
				return nil, errs.New(errs.CodeReconciliationRequired, "predmarket: a share holding is negative")
			}
			shares[i] = s
			held[i] = held[i].Add(s)
			if s.IsPositive() {
				holdsShares = true
			}
		}
		if holdsShares {
			lines = append(lines, PlanLine{AccountID: h.AccountID, Shares: shares})
		}
	}
	for i := 0; i < n; i++ {
		if held[i].Cmp(outstanding[i]) != 0 {
			return nil, errs.Newf(errs.CodeReconciliationRequired,
				"predmarket: customers hold %s shares of outcome %d and the facility has %s outstanding",
				held[i], i, outstanding[i]).WithField("outcome_index", i)
		}
		if escrow.Cmp(outstanding[i].Add(residual)) != 0 {
			return nil, errs.Newf(errs.CodeReconciliationRequired,
				"predmarket: escrow %s does not back the %s outstanding complete sets of outcome %d",
				escrow, outstanding[i], i).WithField("outcome_index", i)
		}
	}
	sort.Slice(lines, func(a, b int) bool { return lines[a].AccountID.String() < lines[b].AccountID.String() })
	return lines, nil
}

// EscrowBooks is one consistent snapshot of a market's facility books, of the
// customer holdings of its shares and of the payouts its escrow holds for
// holders they could not be delivered to.
type EscrowBooks struct {
	Escrow            money.Quantity
	Collateral        money.Quantity
	SharesIssued      []money.Quantity
	SharesOutstanding []money.Quantity
	HeldByCustomers   []money.Quantity
	// Held is the sum of the market's held payouts not yet claimed: zero
	// before settlement, and what the escrow still owes named holders after.
	Held money.Quantity
}

// Violations returns every way the books break the escrow identity, given the
// residual a void refund disclosed (zero for any market that has not been
// refunded). An empty result means the books reconcile:
//
//	escrow = collateral = shares outstanding(i) + residual + held,
//	shares issued(i) = shares outstanding(i) = held by customers(i), for every i.
func (b EscrowBooks) Violations(residual money.Quantity) []string {
	var out []string
	if b.Escrow.IsNegative() || b.Collateral.IsNegative() || b.Held.IsNegative() {
		out = append(out, "escrow, collateral or held payouts are negative")
	}
	if b.Escrow.Cmp(b.Collateral) != 0 {
		out = append(out, fmt.Sprintf("escrow %s differs from collateral %s", b.Escrow, b.Collateral))
	}
	n := len(b.SharesOutstanding)
	if len(b.SharesIssued) != n || len(b.HeldByCustomers) != n || n == 0 {
		return append(out, "the outcome books are incomplete")
	}
	for i := 0; i < n; i++ {
		if b.SharesIssued[i].Cmp(b.SharesOutstanding[i]) != 0 {
			out = append(out, fmt.Sprintf("outcome %d: shares issued %s differ from shares outstanding %s",
				i, b.SharesIssued[i], b.SharesOutstanding[i]))
		}
		if b.HeldByCustomers[i].Cmp(b.SharesOutstanding[i]) != 0 {
			out = append(out, fmt.Sprintf("outcome %d: customers hold %s shares and %s are outstanding",
				i, b.HeldByCustomers[i], b.SharesOutstanding[i]))
		}
		if implied := b.SharesOutstanding[i].Add(residual).Add(b.Held); b.Escrow.Cmp(implied) != 0 {
			out = append(out, fmt.Sprintf("outcome %d: escrow %s differs from shares outstanding %s plus residual %s plus held payouts %s",
				i, b.Escrow, b.SharesOutstanding[i], residual, b.Held))
		}
	}
	return out
}
