package stableasset

import (
	"context"
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// ReserveHolding is one reserve asset's amount in one line of the reserve
// ledger, with the price and factor it is counted at.
type ReserveHolding struct {
	AssetID   assets.AssetID `json:"asset_id"`
	Kind      ReserveKind    `json:"kind"`
	Quantity  money.Quantity `json:"quantity"`
	Decimals  uint8          `json:"decimals"`
	FactorBPS money.BPS      `json:"factor_bps"`
	// Price is the verified price used (quote units per whole unit; cash
	// equivalents capped at par). Nil means no fresh price: the holding
	// counts zero towards the eligible reserve.
	Price *PriceDoc `json:"price,omitempty"`
	// Eligible is Quantity × Price × FactorBPS, exact.
	Eligible Value `json:"eligible"`
}

// ReserveLedger is the issuer's reserve position for one stable asset (goal
// model §5). Every figure says where it comes from.
//
// Ledger balances (the books of the STABLE_ISSUER facility, internal/ledger):
//
//   - IssuedSupply: facility STABLE_OUTSTANDING of the stable asset (equal to
//     STABLE_ISSUED by construction of every posting).
//   - CashEquivalentReserve / TokenizedReserve: facility STABLE_RESERVE of
//     each reserve asset of that kind (STABLE_RESERVE_FUNDING mirrors it).
//
// Derived from request states (stable_mint_requests, stable_burn_requests),
// never from a ledger balance:
//
//   - PendingMint: Σ quantity of mints SUBMITTING, SUBMITTED or
//     SUBMISSION_UNKNOWN. It is also ReservedNotFinalSupply: supply reserved
//     against the cap and the reserve but not final on chain.
//   - Receivable: Σ reserve_quantity of those mints per reserve asset: the
//     customers' reserve funds held by capital reservations, due to the
//     facility when the mint finalizes.
//   - PendingBurn: Σ quantity of burns SUBMITTING, SUBMITTED or
//     SUBMISSION_UNKNOWN.
//   - PendingRedemption: Σ payout of burns SUBMITTING, SUBMITTED,
//     SUBMISSION_UNKNOWN or REDEMPTION_QUEUED per payout asset: reserve that
//     will leave the facility.
type ReserveLedger struct {
	StableAssetID StableAssetID `json:"stable_asset_id"`
	AsOf          time.Time     `json:"as_of"`

	IssuedSupply          money.Quantity   `json:"issued_supply"`
	CashEquivalentReserve []ReserveHolding `json:"cash_equivalent_reserve"`
	TokenizedReserve      []ReserveHolding `json:"tokenized_reserve"`

	PendingMint            money.Quantity   `json:"pending_mint"`
	ReservedNotFinalSupply money.Quantity   `json:"reserved_not_final_supply"`
	Receivable             []ReserveHolding `json:"receivable"`
	PendingBurn            money.Quantity   `json:"pending_burn"`
	PendingRedemption      []ReserveHolding `json:"pending_redemption"`

	// SupplyValue is IssuedSupply at the target peg; EligibleReserve is the
	// sum of the eligible values of both reserve lines. CoverageBPS is
	// floor(EligibleReserve / SupplyValue × 10000).
	SupplyValue     Value     `json:"supply_value"`
	EligibleReserve Value     `json:"eligible_reserve"`
	CoverageBPS     money.BPS `json:"coverage_bps"`
	// Unverified lists reserve assets holding a balance with no fresh price.
	Unverified []assets.AssetID `json:"unverified"`
}

// Covered reports the fully reserved invariant: issued supply at the peg is
// covered by the verified eligible reserve.
func (rl ReserveLedger) Covered() bool { return rl.EligibleReserve.Cmp(rl.SupplyValue) >= 0 }

// cashHolding returns the cash-equivalent reserve held in asset.
func (rl ReserveLedger) cashHolding(asset assets.AssetID) money.Quantity {
	for _, h := range rl.CashEquivalentReserve {
		if h.AssetID == asset {
			return h.Quantity
		}
	}
	return money.Quantity{}
}

func sumEligible(hs []ReserveHolding) Value {
	total := Value{Coef: money.Quantity{}, Exp: 0}
	for _, h := range hs {
		total = total.Add(h.Eligible)
	}
	return total
}

func sumQuantity(hs []ReserveHolding, asset assets.AssetID) money.Quantity {
	var q money.Quantity
	for _, h := range hs {
		if h.AssetID == asset {
			q = q.Add(h.Quantity)
		}
	}
	return q
}

// pricing values reserve assets. It is built once per reserve-ledger read so
// every line uses the same prices.
type pricing struct {
	prices map[assets.AssetID]*money.Price
}

func (p pricing) holding(r EligibleReserve, qty money.Quantity) ReserveHolding {
	h := ReserveHolding{AssetID: r.AssetID, Kind: r.Kind, Quantity: qty, Decimals: r.Decimals, FactorBPS: r.FactorBPS,
		Eligible: Value{Coef: money.Quantity{}}}
	if price := p.prices[r.AssetID]; price != nil {
		doc := priceDoc(*price)
		h.Price = &doc
		h.Eligible = unitsValue(qty, r.Decimals).times(*price).timesBPS(r.FactorBPS)
	}
	return h
}

// reservePrice returns the price a reserve asset is counted at: par for the
// peg's own quote asset, otherwise the freshest observation of the asset's
// oracle source within MaxPriceAge, capped at par for a cash equivalent. ok
// is false when no such price exists: the asset cannot be verified.
func (s *Service) reservePrice(ctx context.Context, q db.Querier, a PlatformStableAsset, r EligibleReserve, now time.Time) (money.Price, bool, error) {
	if r.AssetID == a.PegQuoteAssetID {
		return money.Price{Mantissa: money.QuantityFromInt64(1), Scale: 0, QuoteAsset: a.PegQuoteAssetID.String(), Source: "PAR", At: now}, true, nil
	}
	p, err := s.prices.Latest(ctx, q, r.AssetID, a.PegQuoteAssetID, a.MaxPriceAge, now)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeStaleMarketData {
			return money.Price{}, false, nil
		}
		return money.Price{}, false, err
	}
	if p.Source != a.OracleSource {
		return money.Price{}, false, nil
	}
	if r.Kind == ReserveCashEquivalent {
		p = capAtPar(p)
	}
	return p, true, nil
}

// marketPrice returns the stable asset's own market price from its oracle
// source, or ok=false when none is fresh.
func (s *Service) marketPrice(ctx context.Context, q db.Querier, a PlatformStableAsset, now time.Time) (money.Price, bool, error) {
	p, err := s.prices.Latest(ctx, q, a.AssetID, a.PegQuoteAssetID, a.MaxPriceAge, now)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeStaleMarketData {
			return money.Price{}, false, nil
		}
		return money.Price{}, false, err
	}
	if p.Source != a.OracleSource {
		return money.Price{}, false, nil
	}
	return p, true, nil
}

func (s *Service) pricing(ctx context.Context, q db.Querier, a PlatformStableAsset, now time.Time) (pricing, error) {
	p := pricing{prices: map[assets.AssetID]*money.Price{}}
	for _, r := range a.Reserves {
		price, ok, err := s.reservePrice(ctx, q, a, r, now)
		if err != nil {
			return pricing{}, err
		}
		if ok {
			p.prices[r.AssetID] = &price
		}
	}
	return p, nil
}

// ReserveLedger reads the reserve position of a stable asset.
func (s *Service) ReserveLedger(ctx context.Context, q db.Querier, sid StableAssetID) (ReserveLedger, error) {
	a, err := s.get(ctx, q, sid, false)
	if err != nil {
		return ReserveLedger{}, err
	}
	return s.reserveLedger(ctx, q, a, s.clk.Now().UTC())
}

func (s *Service) reserveLedger(ctx context.Context, q db.Querier, a PlatformStableAsset, now time.Time) (ReserveLedger, error) {
	prices, err := s.pricing(ctx, q, a, now)
	if err != nil {
		return ReserveLedger{}, err
	}
	return s.reserveLedgerAt(ctx, q, a, prices, now)
}

func (s *Service) reserveLedgerAt(ctx context.Context, q db.Querier, a PlatformStableAsset, prices pricing, now time.Time) (ReserveLedger, error) {
	rl := ReserveLedger{StableAssetID: a.ID, AsOf: now}
	balances, err := s.ledger.BalancesForOwner(ctx, q, ledger.OwnerFacility, a.FacilityID.String())
	if err != nil {
		return ReserveLedger{}, err
	}
	reserveBal := map[assets.AssetID]money.Quantity{}
	for _, b := range balances {
		switch {
		case b.Account.Code == ledger.CodeStableOutstanding && b.Account.AssetID == a.AssetID:
			rl.IssuedSupply = b.Balance
		case b.Account.Code == ledger.CodeStableReserve:
			reserveBal[b.Account.AssetID] = b.Balance
		}
	}
	for _, r := range a.Reserves {
		h := prices.holding(r, reserveBal[r.AssetID])
		if h.Price == nil && h.Quantity.IsPositive() {
			rl.Unverified = append(rl.Unverified, r.AssetID)
		}
		if r.Kind == ReserveCashEquivalent {
			rl.CashEquivalentReserve = append(rl.CashEquivalentReserve, h)
		} else {
			rl.TokenizedReserve = append(rl.TokenizedReserve, h)
		}
	}

	mints, err := openMints(ctx, q, a.ID)
	if err != nil {
		return ReserveLedger{}, err
	}
	receivable := map[assets.AssetID]money.Quantity{}
	for _, m := range mints {
		rl.PendingMint = rl.PendingMint.Add(m.Quantity)
		receivable[m.ReserveAssetID] = receivable[m.ReserveAssetID].Add(m.ReserveQuantity)
	}
	rl.ReservedNotFinalSupply = rl.PendingMint
	burns, err := openBurns(ctx, q, a.ID)
	if err != nil {
		return ReserveLedger{}, err
	}
	payouts := map[assets.AssetID]money.Quantity{}
	for _, b := range burns {
		if b.Status != BurnRedemptionQueued {
			rl.PendingBurn = rl.PendingBurn.Add(b.Quantity)
		}
		payouts[b.PayoutAssetID] = payouts[b.PayoutAssetID].Add(b.PayoutQuantity)
	}
	rl.Receivable = holdings(a, prices, receivable)
	rl.PendingRedemption = holdings(a, prices, payouts)

	rl.SupplyValue = unitsValue(rl.IssuedSupply, a.Decimals).times(a.Peg)
	rl.EligibleReserve = sumEligible(rl.CashEquivalentReserve).Add(sumEligible(rl.TokenizedReserve))
	rl.CoverageBPS = ratioBPS(rl.EligibleReserve, rl.SupplyValue)
	return rl, nil
}

func holdings(a PlatformStableAsset, prices pricing, by map[assets.AssetID]money.Quantity) []ReserveHolding {
	out := []ReserveHolding{}
	for asset, qty := range by {
		r, ok := a.Reserve(asset)
		if !ok {
			r = EligibleReserve{AssetID: asset, Kind: ReserveCashEquivalent}
		}
		out = append(out, prices.holding(r, qty))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssetID.String() < out[j].AssetID.String() })
	return out
}

// Admission refusal reasons, decided by admitMint.
const (
	reasonCapExceeded      = "STABLE_ISSUANCE_CAP_EXCEEDED"
	reasonReserveShortfall = "STABLE_RESERVE_SHORTFALL"
)

// admitMint is the fully reserved admission rule, pure so it can be
// property-tested. A mint of q units, paying reserve units of an eligible
// reserve asset counted at its verified price and factor, is admitted only
// if, counting every mint not yet final as if it were issued (with the
// reserve it brings) and every redemption not yet posted as if its payout
// had left:
//
//	issued + pending + q                 <= issuance cap
//	value(issued + pending + q at peg)   <= eligible(reserve) + eligible(receivable)
//	                                        + eligible(this payment) − eligible(pending payouts)
//
// Pending burns' supply reductions are not credited until they post, which
// only ever makes the rule stricter.
func admitMint(rl ReserveLedger, a PlatformStableAsset, q money.Quantity, payment ReserveHolding) string {
	supply := rl.IssuedSupply.Add(rl.PendingMint).Add(q)
	if supply.Cmp(a.IssuanceCap) > 0 {
		return reasonCapExceeded
	}
	need := unitsValue(supply, a.Decimals).times(a.Peg)
	have := rl.EligibleReserve.Add(sumEligible(rl.Receivable)).Add(payment.Eligible).Sub(sumEligible(rl.PendingRedemption))
	if need.Cmp(have) > 0 {
		return reasonReserveShortfall
	}
	return ""
}
