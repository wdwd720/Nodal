package margin

import (
	"fmt"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// DerivativeExposure is the contract between the derivatives lane and the
// margin engine. The derivatives subsystem computes these per instrument and
// hands them over; this package never imports it.
//
//   - GrossNotional is the absolute notional of the position and its open
//     risk-increasing orders, at the derivatives lane's mark.
//   - InitialMargin and MaintenanceMargin are that lane's requirements; the
//     margin engine adds them to the account's own.
//   - UnrealizedPnL is the open profit (positive) or loss (negative). It is
//     the "unrealized pnl" term of the canonical equity.
//   - RiskIncreasingDirection reports whether the exposure adds to the
//     account's risk (true) or hedges it (false). Net exposure counts the
//     first positive and the second negative; gross exposure counts both.
//
// The contract carries no direction of the underlying, so the stress
// scenarios treat every derivative exposure as adverse to a gap in either
// direction: a gap of g costs g × GrossNotional of unrealized PnL.
type DerivativeExposure struct {
	InstrumentID            string    `json:"instrument_id"`
	GrossNotional           money.USD `json:"gross_notional"`
	InitialMargin           money.USD `json:"initial_margin"`
	MaintenanceMargin       money.USD `json:"maintenance_margin"`
	UnrealizedPnL           money.USD `json:"unrealized_pnl"`
	RiskIncreasingDirection bool      `json:"risk_increasing_direction"`
}

// Holding is one asset held as margin collateral (the MARGIN_COLLATERAL
// balance of that asset).
type Holding struct {
	AssetID  string         `json:"asset_id"`
	Decimals uint8          `json:"decimals"`
	Quantity money.Quantity `json:"quantity"`
	// Numeraire marks the account's quote asset: valued at par, never an
	// exposure, never haircut, and needing no oracle.
	Numeraire bool `json:"numeraire"`
	// Mark is the last known price of one whole unit in the numeraire. A
	// stale mark is still reported; its freshness is judged against the
	// policy. Nil means no price is known at all.
	Mark    *money.Price `json:"mark,omitempty"`
	Profile AssetProfile `json:"profile"`
	// SettlementDelayMS is how long until the holding settles (0 = settled).
	SettlementDelayMS int64 `json:"settlement_delay_ms"`
	// InFlight marks collateral moving between chains.
	InFlight      bool  `json:"in_flight"`
	InFlightMS    int64 `json:"in_flight_ms"`
	BridgeDelayed bool  `json:"bridge_delayed"`
}

// Loan is one open borrow lot, in numeraire base units.
type Loan struct {
	LotID string `json:"lot_id"`
	// Principal is the outstanding principal.
	Principal money.Quantity `json:"principal"`
	// Interest is capitalized (posted) interest not yet repaid.
	Interest money.Quantity `json:"interest"`
	RateBPS  money.BPS      `json:"rate_bps"`
	// AccruedThrough is the end of the last posted accrual period. Interest
	// from there to the evaluation time is the "accrued interest" term.
	AccruedThrough time.Time `json:"accrued_through"`
}

// Outstanding is principal plus capitalized interest.
func (l Loan) Outstanding() money.Quantity { return l.Principal.Add(l.Interest) }

// AccountState is everything the engine evaluates. It is built by the
// service from the ledger, the margin tables and the price store, or by a
// test from nothing; the engine does no I/O.
type AccountState struct {
	AccountID string     `json:"account_id"`
	Mode      MarginMode `json:"mode"`
	// IsolatedAssetID is the one risky asset an ISOLATED account margins.
	IsolatedAssetID   string               `json:"isolated_asset_id,omitempty"`
	NumeraireAssetID  string               `json:"numeraire_asset_id"`
	NumeraireDecimals uint8                `json:"numeraire_decimals"`
	Holdings          []Holding            `json:"holdings"`
	Loans             []Loan               `json:"loans"`
	Derivatives       []DerivativeExposure `json:"derivatives"`
	// RealizedPnL is profit or loss realized but not yet settled into
	// collateral (an integration's pending settlement). Spot sales settle
	// into collateral at once, so the margin service itself supplies zero.
	RealizedPnL money.USD `json:"realized_pnl"`
	// AccruedFees are fees owed and not yet posted. Every fee the margin
	// service charges is posted when charged, so it supplies zero.
	AccruedFees money.USD `json:"accrued_fees"`
	Now         time.Time `json:"now"`
}

// Clone returns a deep copy: changing the copy never changes s.
func (s AccountState) Clone() AccountState {
	out := s
	out.Holdings = make([]Holding, len(s.Holdings))
	for i, h := range s.Holdings {
		out.Holdings[i] = h
		if h.Mark != nil {
			m := *h.Mark
			out.Holdings[i].Mark = &m
		}
	}
	out.Loans = append([]Loan(nil), s.Loans...)
	out.Derivatives = append([]DerivativeExposure(nil), s.Derivatives...)
	return out
}

// holding returns the index of an asset's holding, or -1.
func (s AccountState) holding(assetID string) int {
	for i, h := range s.Holdings {
		if h.AssetID == assetID {
			return i
		}
	}
	return -1
}

// numeraireIndex returns the index of the numeraire holding, adding an
// empty one if there is none.
func (s *AccountState) numeraireIndex() int {
	if i := s.holding(s.NumeraireAssetID); i >= 0 {
		return i
	}
	s.Holdings = append(s.Holdings, Holding{
		AssetID: s.NumeraireAssetID, Decimals: s.NumeraireDecimals, Quantity: money.Quantity{}, Numeraire: true,
	})
	return len(s.Holdings) - 1
}

// LoanTotal is Σ outstanding over the loans.
func (s AccountState) LoanTotal() money.Quantity {
	total := money.Quantity{}
	for _, l := range s.Loans {
		total = total.Add(l.Outstanding())
	}
	return total
}

// Validate checks the state's structure. The engine refuses to evaluate a
// state it cannot vouch for rather than guessing.
func (s AccountState) Validate() error {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	if strings.TrimSpace(s.AccountID) == "" {
		add("account_id required")
	}
	if !s.Mode.Valid() {
		add("unknown margin mode %q", s.Mode)
	}
	if strings.TrimSpace(s.NumeraireAssetID) == "" {
		add("numeraire asset required")
	}
	if s.NumeraireDecimals < 2 || s.NumeraireDecimals > 18 {
		add("numeraire decimals must be within [2, 18] so cents convert exactly")
	}
	if s.Now.IsZero() {
		add("now required")
	}
	if s.Mode == ModeIsolated {
		if s.IsolatedAssetID == "" || s.IsolatedAssetID == s.NumeraireAssetID {
			add("an ISOLATED account names one risky asset")
		}
		if len(s.Derivatives) > 0 {
			add("an ISOLATED account margins one spot position and no derivative exposure")
		}
	} else if s.IsolatedAssetID != "" {
		add("only an ISOLATED account names an isolated asset")
	}
	seen := map[string]bool{}
	for _, h := range s.Holdings {
		if h.AssetID == "" || seen[h.AssetID] {
			add("holding asset %q missing or duplicated", h.AssetID)
		}
		seen[h.AssetID] = true
		if h.Quantity.IsNegative() {
			add("holding %s quantity is negative", h.AssetID)
		}
		if h.SettlementDelayMS < 0 || h.InFlightMS < 0 {
			add("holding %s delays must not be negative", h.AssetID)
		}
		isNum := h.AssetID == s.NumeraireAssetID
		if h.Numeraire != isNum {
			add("holding %s numeraire flag disagrees with the account's numeraire", h.AssetID)
		}
		if isNum {
			if h.Decimals != s.NumeraireDecimals {
				add("numeraire holding decimals disagree with the account")
			}
			if h.InFlight {
				add("numeraire collateral cannot be in flight")
			}
			continue
		}
		if h.Decimals > 38 {
			add("holding %s decimals out of range", h.AssetID)
		}
		if h.Profile.Volatility.Rank() < 0 || h.Profile.Liquidity.Rank() < 0 || h.Profile.Venue.Rank() < 0 {
			add("holding %s has an incomplete risk profile", h.AssetID)
		}
		if h.Profile.RiskWeightBPS < 1_000 || h.Profile.RiskWeightBPS > 50_000 {
			add("holding %s risk weight out of range", h.AssetID)
		}
		if h.Mark != nil {
			if err := h.Mark.Validate(); err != nil {
				add("holding %s mark is invalid: %v", h.AssetID, err)
			} else if h.Mark.QuoteAsset != s.NumeraireAssetID {
				add("holding %s is marked in %s, not in the numeraire", h.AssetID, h.Mark.QuoteAsset)
			}
		}
		if s.Mode == ModeIsolated && h.Quantity.IsPositive() && h.AssetID != s.IsolatedAssetID {
			add("ISOLATED account holds %s besides its isolated asset", h.AssetID)
		}
	}
	seenLots := map[string]bool{}
	for _, l := range s.Loans {
		if l.LotID == "" || seenLots[l.LotID] {
			add("loan lot %q missing or duplicated", l.LotID)
		}
		seenLots[l.LotID] = true
		if l.Principal.IsNegative() || l.Interest.IsNegative() {
			add("loan %s is negative", l.LotID)
		}
		if l.RateBPS < 0 || l.RateBPS > 100_000 {
			add("loan %s rate out of range", l.LotID)
		}
		if l.AccruedThrough.IsZero() {
			add("loan %s accrued_through required", l.LotID)
		}
		if s.Mode == ModeCash && l.Outstanding().IsPositive() {
			add("a CASH account cannot carry a loan")
		}
	}
	seenInst := map[string]bool{}
	for _, d := range s.Derivatives {
		if strings.TrimSpace(d.InstrumentID) == "" || seenInst[d.InstrumentID] {
			add("derivative exposure instrument %q missing or duplicated", d.InstrumentID)
		}
		seenInst[d.InstrumentID] = true
		if d.GrossNotional.IsNegative() || d.InitialMargin.IsNegative() || d.MaintenanceMargin.IsNegative() {
			add("derivative exposure %s has a negative notional or requirement", d.InstrumentID)
		}
		if d.MaintenanceMargin.Cmp(d.InitialMargin) > 0 {
			add("derivative exposure %s maintenance exceeds initial", d.InstrumentID)
		}
	}
	if s.AccruedFees.IsNegative() {
		add("accrued fees must not be negative")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid margin account state").WithField("problems", problems)
	}
	return nil
}
