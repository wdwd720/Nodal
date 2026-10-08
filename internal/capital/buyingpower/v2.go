package buyingpower

import (
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Universal Buying Power V2 (advanced goal §20; ADR-0020).
//
// V1 (Engine, above) answers for real value and never extends credit. V2
// answers for the SIMULATED domain of the advanced capabilities, where margin,
// derivatives, unsettled equity proceeds, bridge legs in flight, pending
// transfers and withdrawals, open-order and strategy reservations and house
// books all exist and must be unified into one explainable answer.
//
// EvaluateV2 is pure: every input is in V2Input, every output number is
// explained by V2Line entries that sum to it, and the same input always gives
// the same output. It never reads the database; LoadV2 (v2_load.go) builds the
// input from the ledger and from reader interfaces the subsystems implement.
//
// The rules, stated once:
//
//	gross_equity         = Σ marked holdings (wallet + margin collateral +
//	                       derivatives margin + unsettled receivable +
//	                       bridge in flight) + unrealized PnL
//	                       − borrow liabilities − accrued fees − accrued interest
//	eligible_collateral  = Σ marked holdings × collateral factor (haircuts applied)
//	haircuts             = Σ marked holdings − eligible_collateral
//	eligible_equity      = eligible_collateral + unrealized PnL − borrow liabilities
//	                       − accrued fees − accrued interest
//	reservations         = open orders + pending transfers + pending withdrawals
//	                       + strategy reservations
//	trade_buying_power   = max(0, settled spendable cash − reservations)
//	margin_available     = max(0, eligible_equity − initial margin used − reservations)
//	risk_increasing_bp   = trade_buying_power for a CASH account, otherwise
//	                       margin_available × 10000 / initial margin rate
//	withdrawable         = max(0, min(settled spendable cash − reservations,
//	                       eligible_equity − maintenance margin − withdrawal buffer))
//
// so that no withdrawal can leave equity below maintenance, bridge value in
// flight counts only at the policy's haircut (zero by default: in-flight
// capital is not final), and unsettled equity proceeds can fund trading at a
// policy factor but are never withdrawable.

// V2EngineVersion is part of the policy version of every V2 answer.
const V2EngineVersion = "buying-power/2"

// V2Component names what a holding line is.
type V2Component string

// Components.
const (
	ComponentWallet              V2Component = "WALLET"
	ComponentMarginCollateral    V2Component = "MARGIN_COLLATERAL"
	ComponentDerivativesMargin   V2Component = "DERIVATIVES_MARGIN"
	ComponentUnsettledReceivable V2Component = "UNSETTLED_RECEIVABLE"
	ComponentBridgeInFlight      V2Component = "BRIDGE_IN_FLIGHT"
)

// V2Holding is one marked balance.
type V2Holding struct {
	Component V2Component    `json:"component"`
	AssetID   string         `json:"asset_id"`
	Symbol    string         `json:"symbol"`
	Quantity  money.Quantity `json:"quantity"`
	// MarkedUSD is quantity × mark, rounded down (computed by the loader).
	MarkedUSD money.USD `json:"marked_usd"`
	// FactorBPS is the collateral factor after every haircut (10000 = full
	// value, 0 = excluded). The loader composes it; Reason says why it is
	// below 10000.
	FactorBPS money.BPS `json:"factor_bps"`
	Reason    string    `json:"reason"`
	// SpendableCash marks a settled, cash-like WALLET line that can pay for
	// a purchase without conversion.
	SpendableCash bool `json:"spendable_cash"`
}

// V2MarginState is what the margin and derivatives subsystems report.
type V2MarginState struct {
	// Mode is CASH, ISOLATED or CROSS.
	Mode              string    `json:"mode"`
	BorrowLiabilities money.USD `json:"borrow_liabilities"`
	AccruedInterest   money.USD `json:"accrued_interest"`
	AccruedFees       money.USD `json:"accrued_fees"`
	UnrealizedPnL     money.USD `json:"unrealized_pnl"`
	InitialMarginUsed money.USD `json:"initial_margin_used"`
	MaintenanceMargin money.USD `json:"maintenance_margin"`
	// InitialMarginRateBPS is the policy's initial margin rate used to turn
	// available margin into risk-increasing buying power (2000 = 20% = 5x).
	InitialMarginRateBPS money.BPS `json:"initial_margin_rate_bps"`
}

// V2Reservations are capital spoken for by things not yet final.
type V2Reservations struct {
	OpenOrders         money.USD `json:"open_orders"`
	PendingTransfers   money.USD `json:"pending_transfers"`
	PendingWithdrawals money.USD `json:"pending_withdrawals"`
	Strategies         money.USD `json:"strategies"`
}

// V2Policy is the versioned V2 policy.
type V2Policy struct {
	Version string `json:"version"`
	// WithdrawalBufferUSD is kept above maintenance when computing withdrawable.
	WithdrawalBufferUSD money.USD `json:"withdrawal_buffer_usd"`
	// UnsettledTradeFactorBPS is how much of unsettled proceeds may fund
	// trading (never withdrawals). 0 by default.
	UnsettledTradeFactorBPS money.BPS `json:"unsettled_trade_factor_bps"`
}

// DefaultV2Policy counts nothing that is not final.
func DefaultV2Policy() V2Policy {
	return V2Policy{Version: "bp2-sandbox-v1", WithdrawalBufferUSD: money.USDFromMinor(0), UnsettledTradeFactorBPS: 0}
}

// V2Input is everything EvaluateV2 needs.
type V2Input struct {
	OwnerType    string         `json:"owner_type"` // CUSTOMER or HOUSE
	OwnerID      string         `json:"owner_id"`
	Holdings     []V2Holding    `json:"holdings"`
	Margin       V2MarginState  `json:"margin"`
	Reservations V2Reservations `json:"reservations"`
	Policy       V2Policy       `json:"policy"`
	// Blocked is true when an account-scoped restriction (frozen account,
	// kill switch, open reconciliation block) zeroes new risk and withdrawals.
	Blocked      bool      `json:"blocked"`
	BlockReasons []string  `json:"block_reasons"`
	AsOf         time.Time `json:"as_of"`
}

// V2Line explains one number.
type V2Line struct {
	Field     string    `json:"field"`
	Component string    `json:"component"`
	AssetID   string    `json:"asset_id,omitempty"`
	AmountUSD money.USD `json:"amount_usd"`
	Detail    string    `json:"detail"`
}

// V2Reason is a machine-readable statement about the answer.
type V2Reason struct {
	Code   string    `json:"code"`
	Detail string    `json:"detail"`
	Amount money.USD `json:"amount"`
}

// V2 is the typed explanation of goal §20.
type V2 struct {
	GrossEquity                   money.USD  `json:"gross_equity"`
	EligibleCollateral            money.USD  `json:"eligible_collateral"`
	Haircuts                      money.USD  `json:"haircuts"`
	BorrowLiabilities             money.USD  `json:"borrow_liabilities"`
	InitialMarginUsed             money.USD  `json:"initial_margin_used"`
	MaintenanceMargin             money.USD  `json:"maintenance_margin"`
	OpenOrderReservations         money.USD  `json:"open_order_reservations"`
	PendingTransferReservations   money.USD  `json:"pending_transfer_reservations"`
	PendingWithdrawalReservations money.USD  `json:"pending_withdrawal_reservations"`
	StrategyReservations          money.USD  `json:"strategy_reservations"`
	PendingBridgeHaircut          money.USD  `json:"pending_bridge_haircut"`
	EligibleEquity                money.USD  `json:"eligible_equity"`
	MarginAvailable               money.USD  `json:"margin_available"`
	Withdrawable                  money.USD  `json:"withdrawable"`
	TradeBuyingPower              money.USD  `json:"trade_buying_power"`
	RiskIncreasingBuyingPower     money.USD  `json:"risk_increasing_buying_power"`
	Reasons                       []V2Reason `json:"reasons"`
	Lines                         []V2Line   `json:"lines"`
	PolicyVersion                 string     `json:"policy_version"`
	AsOf                          time.Time  `json:"as_of"`
}

// Reason codes.
const (
	V2ReasonHaircut            = "HAIRCUT_APPLIED"
	V2ReasonBridgeInFlight     = "BRIDGE_IN_FLIGHT_NOT_FINAL"
	V2ReasonUnsettled          = "UNSETTLED_NOT_WITHDRAWABLE"
	V2ReasonMaintenanceBinding = "MAINTENANCE_LIMITS_WITHDRAWAL"
	V2ReasonNegativeEquity     = "NEGATIVE_EQUITY"
	V2ReasonBlocked            = "ACCOUNT_BLOCKED"
	V2ReasonReserved           = "CAPITAL_RESERVED"
)

// EvaluateV2 computes the V2 answer. It returns an error only on arithmetic
// overflow or malformed input; every policy outcome is a number with a reason.
func EvaluateV2(in V2Input) (V2, error) {
	if in.OwnerType != "CUSTOMER" && in.OwnerType != "HOUSE" {
		return V2{}, errs.Newf(errs.CodeValidationFailed, "buying power v2: owner type %q is neither CUSTOMER nor HOUSE", in.OwnerType)
	}
	if in.Policy.Version == "" {
		return V2{}, errs.New(errs.CodeValidationFailed, "buying power v2: a policy version is required")
	}
	holdings := append([]V2Holding(nil), in.Holdings...)
	sort.SliceStable(holdings, func(i, j int) bool {
		if holdings[i].Component != holdings[j].Component {
			return holdings[i].Component < holdings[j].Component
		}
		return holdings[i].AssetID < holdings[j].AssetID
	})
	acc := &v2acc{}
	out := V2{PolicyVersion: V2EngineVersion + "|" + in.Policy.Version, AsOf: in.AsOf.UTC()}

	var marked, eligible, spendable, unsettledEligible, bridgeHaircut money.USD
	for _, h := range holdings {
		if h.MarkedUSD.IsNegative() || h.FactorBPS < 0 || h.FactorBPS > money.OneHundredPercent {
			return V2{}, errs.Newf(errs.CodeValidationFailed, "buying power v2: holding %s/%s is malformed", h.Component, h.AssetID)
		}
		marked = acc.add(marked, h.MarkedUSD)
		factor := h.FactorBPS
		if h.Component == ComponentBridgeInFlight {
			// In flight is not final. The loader passes the policy factor
			// (zero by default); the difference is reported on its own line.
			hc := acc.sub(h.MarkedUSD, acc.bps(h.MarkedUSD, factor))
			bridgeHaircut = acc.add(bridgeHaircut, hc)
		}
		e := acc.bps(h.MarkedUSD, factor)
		eligible = acc.add(eligible, e)
		out.Lines = append(out.Lines, V2Line{Field: "eligible_collateral", Component: string(h.Component), AssetID: h.AssetID,
			AmountUSD: e, Detail: h.Symbol + " " + h.Quantity.String() + " marked " + h.MarkedUSD.String() + " at factor " + h.FactorBPS.String()})
		if h.FactorBPS < money.OneHundredPercent && !h.MarkedUSD.IsZero() {
			out.Reasons = append(out.Reasons, V2Reason{Code: V2ReasonHaircut, Detail: string(h.Component) + " " + h.Symbol + ": " + h.Reason,
				Amount: acc.sub(h.MarkedUSD, e)})
		}
		switch {
		case h.Component == ComponentWallet && h.SpendableCash:
			spendable = acc.add(spendable, h.MarkedUSD)
		case h.Component == ComponentUnsettledReceivable:
			unsettledEligible = acc.add(unsettledEligible, acc.bps(h.MarkedUSD, in.Policy.UnsettledTradeFactorBPS))
			if !h.MarkedUSD.IsZero() {
				out.Reasons = append(out.Reasons, V2Reason{Code: V2ReasonUnsettled, Detail: "unsettled proceeds are never withdrawable", Amount: h.MarkedUSD})
			}
		}
	}
	if !bridgeHaircut.IsZero() {
		out.Reasons = append(out.Reasons, V2Reason{Code: V2ReasonBridgeInFlight, Detail: "bridge value in flight counts only at the policy haircut", Amount: bridgeHaircut})
	}

	m := in.Margin
	debts := acc.add(acc.add(m.BorrowLiabilities, m.AccruedInterest), m.AccruedFees)
	out.GrossEquity = acc.sub(acc.add(marked, m.UnrealizedPnL), debts)
	out.EligibleCollateral = eligible
	out.Haircuts = acc.sub(marked, eligible)
	out.PendingBridgeHaircut = bridgeHaircut
	out.BorrowLiabilities = m.BorrowLiabilities
	out.InitialMarginUsed = m.InitialMarginUsed
	out.MaintenanceMargin = m.MaintenanceMargin
	out.EligibleEquity = acc.sub(acc.add(eligible, m.UnrealizedPnL), debts)
	out.Lines = append(out.Lines,
		V2Line{Field: "eligible_equity", Component: "UNREALIZED_PNL", AmountUSD: m.UnrealizedPnL, Detail: "derivative and leveraged positions marked to market"},
		V2Line{Field: "eligible_equity", Component: "BORROW_LIABILITIES", AmountUSD: m.BorrowLiabilities.Neg(), Detail: "margin loan principal and capitalized interest"},
		V2Line{Field: "eligible_equity", Component: "ACCRUED_INTEREST", AmountUSD: m.AccruedInterest.Neg(), Detail: "interest accrued and not yet capitalized"},
		V2Line{Field: "eligible_equity", Component: "ACCRUED_FEES", AmountUSD: m.AccruedFees.Neg(), Detail: "fees accrued and not yet charged"},
	)
	if out.GrossEquity.IsNegative() {
		out.Reasons = append(out.Reasons, V2Reason{Code: V2ReasonNegativeEquity, Detail: "liabilities exceed marked holdings", Amount: out.GrossEquity})
	}

	r := in.Reservations
	out.OpenOrderReservations = r.OpenOrders
	out.PendingTransferReservations = r.PendingTransfers
	out.PendingWithdrawalReservations = r.PendingWithdrawals
	out.StrategyReservations = r.Strategies
	reserved := acc.add(acc.add(r.OpenOrders, r.PendingTransfers), acc.add(r.PendingWithdrawals, r.Strategies))
	if !reserved.IsZero() {
		out.Reasons = append(out.Reasons, V2Reason{Code: V2ReasonReserved, Detail: "open orders, pending transfers and withdrawals, strategy reservations", Amount: reserved})
	}

	cashAfterReservations := floor0(acc.sub(spendable, reserved))
	out.TradeBuyingPower = floor0(acc.add(cashAfterReservations, unsettledEligible))
	out.MarginAvailable = floor0(acc.sub(acc.sub(out.EligibleEquity, m.InitialMarginUsed), reserved))
	if m.Mode == "" || m.Mode == "CASH" || m.InitialMarginRateBPS <= 0 {
		out.RiskIncreasingBuyingPower = out.TradeBuyingPower
	} else {
		lev, err := out.MarginAvailable.MulRatio(int64(money.OneHundredPercent), int64(m.InitialMarginRateBPS), money.RoundDown)
		if err != nil {
			acc.err = err
		}
		out.RiskIncreasingBuyingPower = floor0(lev)
	}
	headroom := floor0(acc.sub(acc.sub(out.EligibleEquity, m.MaintenanceMargin), in.Policy.WithdrawalBufferUSD))
	out.Withdrawable = minUSD(cashAfterReservations, headroom)
	if headroom.Cmp(cashAfterReservations) < 0 {
		out.Reasons = append(out.Reasons, V2Reason{Code: V2ReasonMaintenanceBinding,
			Detail: "withdrawing more would leave equity below maintenance margin plus buffer", Amount: headroom})
	}
	if in.Blocked {
		out.TradeBuyingPower, out.RiskIncreasingBuyingPower, out.Withdrawable, out.MarginAvailable = money.USD{}, money.USD{}, money.USD{}, money.USD{}
		for _, b := range in.BlockReasons {
			out.Reasons = append(out.Reasons, V2Reason{Code: V2ReasonBlocked, Detail: b})
		}
		if len(in.BlockReasons) == 0 {
			out.Reasons = append(out.Reasons, V2Reason{Code: V2ReasonBlocked, Detail: "account-scoped restriction"})
		}
	}
	if acc.err != nil {
		return V2{}, errs.Wrap(acc.err, errs.CodeOverflow, "buying power v2: arithmetic overflow")
	}
	sort.SliceStable(out.Reasons, func(i, j int) bool {
		if out.Reasons[i].Code != out.Reasons[j].Code {
			return out.Reasons[i].Code < out.Reasons[j].Code
		}
		return out.Reasons[i].Detail < out.Reasons[j].Detail
	})
	if out.Reasons == nil {
		out.Reasons = []V2Reason{}
	}
	return out, nil
}

// v2acc carries the first overflow so the arithmetic above reads as algebra.
type v2acc struct{ err error }

func (a *v2acc) add(x, y money.USD) money.USD {
	r, err := x.Add(y)
	if err != nil && a.err == nil {
		a.err = err
	}
	return r
}

func (a *v2acc) sub(x, y money.USD) money.USD {
	r, err := x.Sub(y)
	if err != nil && a.err == nil {
		a.err = err
	}
	return r
}

// bps applies a factor, rounding toward zero (never in the account's favour
// for an asset: a haircut rounds the contribution down).
func (a *v2acc) bps(x money.USD, f money.BPS) money.USD {
	r, err := x.MulBPS(f, money.RoundDown)
	if err != nil && a.err == nil {
		a.err = err
	}
	return r
}

func floor0(u money.USD) money.USD {
	if u.IsNegative() {
		return money.USD{}
	}
	return u
}

func minUSD(a, b money.USD) money.USD {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}
