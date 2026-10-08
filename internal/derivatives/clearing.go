package derivatives

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
)

// FundClearingHouse capitalizes the clearing house in a sandbox asset
// (FACILITY_FUNDING: Dr CLEARING_CASH / Cr CLEARING_CAPITAL). The clearing
// house is the counterparty of every paper trade, so a one-sided book is paid
// from its capital. Exactly once per key.
func (s *Service) FundClearingHouse(ctx context.Context, tx pgx.Tx, asset assets.AssetID, qty money.Quantity, key string) ([]ledger.TransactionID, error) {
	k := strings.TrimSpace(key)
	if k == "" || k != key || len(k) > 200 || strings.Contains(k, "..") {
		return nil, errs.New(errs.CodeValidationFailed, "derivatives: a funding key is 1..200 trimmed bytes")
	}
	if !qty.IsPositive() {
		return nil, errs.New(errs.CodeValidationFailed, "derivatives: clearing house funding must be positive")
	}
	if err := s.serializeClearing(ctx, tx); err != nil {
		return nil, err
	}
	ch, err := s.clearingHouse(ctx, tx)
	if err != nil {
		return nil, err
	}
	posted, err := s.executeLegs(ctx, tx, ch, []leg{{
		name: "clearing house capital", key: "deriv:clearing-funding:" + k, effectKind: EffectClearingFunding,
		kind: ledger.KindFacilityFunding, reference: ledger.FinancialEventReference{Type: "clearing_house_funding", ID: k},
		entries: clearingFundingEntries(ch, asset, qty),
	}}, "derivatives/clearing-funding/"+k, func(pl []postedLeg) any {
		return map[string]any{"clearing_house_id": ch.ID.String(), "asset_id": asset.String(), "quantity": qty.String(), "legs": pl}
	}, "")
	if err != nil {
		return nil, err
	}
	return journals(posted), nil
}

// ClearingReport compares the clearing house's books in one asset with the
// derivatives cash-flow register.
type ClearingReport struct {
	ClearingHouseID string `json:"clearing_house_id"`
	AssetID         string `json:"asset_id"`
	// The clearing house's ledger balances.
	ClearingCash    money.Quantity `json:"clearing_cash"`
	ClearingCapital money.Quantity `json:"clearing_capital"`
	ClearingResult  money.Quantity `json:"clearing_result"`
	// The register: Σ customer-side amounts (> 0 received from the clearing
	// house), Σ shortfalls and the number of flows.
	RegisterNet       money.Quantity `json:"register_net"`
	RegisterShortfall money.Quantity `json:"register_shortfall"`
	Flows             int64          `json:"flows"`
	// ExpectedResult is the clearing house's side of the register (−net);
	// Difference is ClearingResult − ExpectedResult.
	ExpectedResult money.Quantity `json:"expected_result"`
	Difference     money.Quantity `json:"difference"`
	// NetOpenInterest is Σ quantity of the open positions per instrument;
	// InternallyMatched is true when it is zero for every instrument, in
	// which case the clearing house carries no position of its own.
	NetOpenInterest   map[string]money.Quantity `json:"net_open_interest"`
	InternallyMatched bool                      `json:"internally_matched"`
	// Matched is true when the books agree with the register and the
	// clearing house's own books balance (cash = capital + result).
	Matched bool `json:"matched"`
	// RecordID is the CLEARING reconciliation record opened or refreshed for
	// a mismatch, or still unresolved from an earlier one.
	RecordID string `json:"record_id,omitempty"`
}

// ReconcileClearing is reconciliation kind CLEARING. The register is the
// clearing house's expected result: every flow it paid a customer debited
// CLEARING_RESULT and every payment it collected credited it, so CLEARING_RESULT
// must equal −Σ register amounts -- and for an internally matched book, whose
// flows cancel pairwise, both are zero. A difference means value moved through
// the clearing house without the subsystem that owns it knowing why: it opens
// a material MISMATCH record (or refreshes the open one), and while it is
// unresolved new derivative risk is refused (CLEARING_UNRECONCILED).
func (s *Service) ReconcileClearing(ctx context.Context, tx pgx.Tx, asset assets.AssetID) (ClearingReport, error) {
	// The clearing house's lock: no flow moves while the register and the
	// books are compared, and two passes that both found no open record
	// cannot both open one.
	if err := s.serializeClearing(ctx, tx); err != nil {
		return ClearingReport{}, err
	}
	ch, err := s.clearingHouse(ctx, tx)
	if err != nil {
		return ClearingReport{}, err
	}
	scope := clearingScopeID(ch.ID, asset)
	rep := ClearingReport{ClearingHouseID: ch.ID.String(), AssetID: asset.String()}
	for _, x := range []struct {
		code ledger.Code
		dst  *money.Quantity
	}{{ledger.CodeClearingCash, &rep.ClearingCash}, {ledger.CodeClearingCapital, &rep.ClearingCapital}, {ledger.CodeClearingResult, &rep.ClearingResult}} {
		if *x.dst, err = s.ledger.Balance(ctx, tx, ch.Account(x.code, asset)); err != nil {
			return ClearingReport{}, err
		}
	}
	if rep.RegisterNet, rep.RegisterShortfall, rep.Flows, err = registerTotals(ctx, tx, ch.ID, asset); err != nil {
		return ClearingReport{}, err
	}
	if rep.NetOpenInterest, err = netOpenInterest(ctx, tx, asset); err != nil {
		return ClearingReport{}, err
	}
	rep.InternallyMatched = true
	for _, q := range rep.NetOpenInterest {
		if !q.IsZero() {
			rep.InternallyMatched = false
		}
	}
	rep.ExpectedResult = rep.RegisterNet.Neg()
	rep.Difference = rep.ClearingResult.Sub(rep.ExpectedResult)
	booksBalance := rep.ClearingCash.Equal(rep.ClearingCapital.Add(rep.ClearingResult))
	rep.Matched = rep.Difference.IsZero() && booksBalance
	existing, open, err := s.recon.FindUnresolvedByScopeForUpdate(ctx, tx, reconciliation.KindClearing, ClearingScopeType, scope)
	if err != nil {
		return ClearingReport{}, err
	}
	if rep.Matched {
		if open {
			rep.RecordID = existing.ID.String()
		}
		return rep, nil
	}
	expected := map[string]string{"clearing_result": rep.ExpectedResult.String(), "register_net": rep.RegisterNet.String(),
		"register_shortfall": rep.RegisterShortfall.String()}
	observed := map[string]string{"clearing_result": rep.ClearingResult.String(), "clearing_cash": rep.ClearingCash.String(),
		"clearing_capital": rep.ClearingCapital.String()}
	difference := map[string]string{"clearing_result": rep.Difference.String(), "books_balance": strconv.FormatBool(booksBalance)}
	if open {
		rec, err := s.recon.UpdateObservation(ctx, tx, existing.ID, expected, observed, difference, true, false)
		if err != nil {
			return ClearingReport{}, err
		}
		rep.RecordID = rec.ID.String()
		return rep, nil
	}
	rec, err := s.recon.Open(ctx, tx, reconciliation.OpenRequest{
		Kind: reconciliation.KindClearing, Mode: reconciliation.ModePeriodic, ScopeType: ClearingScopeType, ScopeID: scope,
		AssetID: asset, Expected: expected, Observed: observed, Difference: difference,
		Status: reconciliation.StatusMismatch, Material: true,
		Actor:  reconciliation.Actor{Type: security.ActorSystem, ID: ActorName},
		Reason: "the clearing house's CLEARING_RESULT disagrees with the derivatives cash-flow register",
	})
	if err != nil {
		return ClearingReport{}, err
	}
	rep.RecordID = rec.ID.String()
	return rep, nil
}
