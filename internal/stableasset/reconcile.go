package stableasset

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// SupplyReconciliation compares the books with the external truth (goal §5):
// the ledger's outstanding supply, the sum of customer wallets of the stable
// asset, the devnet supply of the mint, and the verified reserve.
type SupplyReconciliation struct {
	StableAssetID     StableAssetID  `json:"stable_asset_id"`
	LedgerOutstanding money.Quantity `json:"ledger_outstanding"`
	CustomerWalletSum money.Quantity `json:"customer_wallet_sum"`
	ChainSupply       money.Quantity `json:"chain_supply"`
	// LandedUnpostedMints and LandedUnpostedBurns are requests whose
	// transaction is already in the chain state (CONFIRMED or FINALIZED, or a
	// queued redemption) but not yet posted: the honest difference between
	// the two at this instant.
	LandedUnpostedMints money.Quantity `json:"landed_unposted_mints"`
	LandedUnpostedBurns money.Quantity `json:"landed_unposted_burns"`
	ExpectedChainSupply money.Quantity `json:"expected_chain_supply"`
	SupplyValue         Value          `json:"supply_value"`
	EligibleReserve     Value          `json:"eligible_reserve"`
	CoverageBPS         money.BPS      `json:"coverage_bps"`
	SupplyMatches       bool           `json:"supply_matches"`
	WalletsMatch        bool           `json:"wallets_match"`
	Covered             bool           `json:"covered"`
	RecordID            string         `json:"record_id,omitempty"`
	BreakerTripped      bool           `json:"breaker_tripped"`
}

// Matched reports that books, wallets, chain and reserve all agree.
func (r SupplyReconciliation) Matched() bool { return r.SupplyMatches && r.WalletsMatch && r.Covered }

// ReconcileSupply runs the STABLE_RESERVE reconciliation of one stable asset
// in the caller's transaction. A mismatch opens (or refreshes) a material
// STABLE_RESERVE record that blocks new risk, writes an alert and trips the
// circuit breaker. A periodic or full run that matches records the match.
func (s *Service) ReconcileSupply(ctx context.Context, tx pgx.Tx, sid StableAssetID, mode reconciliation.Mode, actor econ.Actor) (SupplyReconciliation, error) {
	if err := requireActor(actor); err != nil {
		return SupplyReconciliation{}, err
	}
	a, err := s.get(ctx, tx, sid, true)
	if err != nil {
		return SupplyReconciliation{}, err
	}
	return s.reconcileLocked(ctx, tx, &a, mode, actor)
}

func (s *Service) reconcileLocked(ctx context.Context, tx pgx.Tx, a *PlatformStableAsset, mode reconciliation.Mode, actor econ.Actor) (SupplyReconciliation, error) {
	now := s.clk.Now().UTC()
	rl, err := s.reserveLedger(ctx, tx, *a, now)
	if err != nil {
		return SupplyReconciliation{}, err
	}
	out := SupplyReconciliation{StableAssetID: a.ID, LedgerOutstanding: rl.IssuedSupply, SupplyValue: rl.SupplyValue,
		EligibleReserve: rl.EligibleReserve, CoverageBPS: rl.CoverageBPS, Covered: rl.Covered()}
	var walletSum string
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(b.balance), 0)::text FROM ledger_accounts la
		JOIN ledger_balances b ON b.ledger_account_id = la.id
		WHERE la.owner_type = 'CUSTOMER' AND la.code = 'WALLET' AND la.asset_id = $1`, a.AssetID).Scan(&walletSum); err != nil {
		return SupplyReconciliation{}, dbErr("sum customer wallets", err)
	}
	if out.CustomerWalletSum, err = money.ScanQuantity(walletSum); err != nil {
		return SupplyReconciliation{}, dbErr("sum customer wallets", err)
	}
	mints, err := openMints(ctx, tx, a.ID)
	if err != nil {
		return SupplyReconciliation{}, err
	}
	burns, err := openBurns(ctx, tx, a.ID)
	if err != nil {
		return SupplyReconciliation{}, err
	}
	params, err := s.window(a.Chain)
	if err != nil {
		return SupplyReconciliation{}, err
	}
	// One View, read through this transaction: the supply and the statuses
	// that explain it, at one instant.
	err = s.host.ViewIn(ctx, tx, func(v devnethost.View) error {
		supply, err := v.Supply(a.Chain, a.Mint)
		if err != nil {
			return err
		}
		out.ChainSupply = devnet.QuantityOf(supply)
		for _, m := range mints {
			obs, err := observeIn(v, m.Chain, m.TxID, m.RecentBlockhash, parseHeight(m.Plan.BlockhashHeight), params.BlockhashWindow)
			if err != nil {
				return err
			}
			if obs.Landed() {
				out.LandedUnpostedMints = out.LandedUnpostedMints.Add(m.Quantity)
			}
		}
		for _, b := range burns {
			if b.Status == BurnRedemptionQueued {
				out.LandedUnpostedBurns = out.LandedUnpostedBurns.Add(b.Quantity)
				continue
			}
			obs, err := observeIn(v, b.Chain, b.TxID, b.RecentBlockhash, parseHeight(b.Plan.BlockhashHeight), params.BlockhashWindow)
			if err != nil {
				return err
			}
			if obs.Landed() {
				out.LandedUnpostedBurns = out.LandedUnpostedBurns.Add(b.Quantity)
			}
		}
		return nil
	})
	if err != nil {
		return SupplyReconciliation{}, err
	}
	out.ExpectedChainSupply = out.LedgerOutstanding.Add(out.LandedUnpostedMints).Sub(out.LandedUnpostedBurns)
	out.SupplyMatches = out.ExpectedChainSupply.Cmp(out.ChainSupply) == 0
	out.WalletsMatch = out.CustomerWalletSum.Cmp(out.LedgerOutstanding) == 0

	if out.Matched() {
		if mode == reconciliation.ModeEventDriven {
			return out, nil
		}
		if _, open, err := s.recon.FindUnresolvedByScope(ctx, tx, reconciliation.KindStableReserve, ScopeStableAsset, a.ID.String()); err != nil || open {
			return out, err
		}
		rec, err := s.recon.Open(ctx, tx, s.reconRequest(*a, out, mode, actor, reconciliation.StatusMatched))
		if err != nil {
			return SupplyReconciliation{}, err
		}
		out.RecordID = rec.ID.String()
		return out, nil
	}

	expected, observed, difference := reconDocuments(out)
	if existing, found, err := s.recon.FindUnresolvedByScopeForUpdate(ctx, tx, reconciliation.KindStableReserve, ScopeStableAsset, a.ID.String()); err != nil {
		return SupplyReconciliation{}, err
	} else if found {
		if _, err := s.recon.UpdateObservation(ctx, tx, existing.ID, expected, observed, difference, true, true); err != nil {
			return SupplyReconciliation{}, err
		}
		out.RecordID = existing.ID.String()
	} else {
		rec, err := s.recon.Open(ctx, tx, s.reconRequest(*a, out, mode, actor, reconciliation.StatusMismatch))
		if err != nil {
			return SupplyReconciliation{}, err
		}
		out.RecordID = rec.ID.String()
	}
	wasActive := a.Status == StatusActive
	if !out.SupplyMatches || !out.WalletsMatch {
		if err := s.trip(ctx, tx, a, AlertSupplyMismatch, SeverityCritical, out); err != nil {
			return SupplyReconciliation{}, err
		}
	}
	if !out.Covered {
		if err := s.trip(ctx, tx, a, AlertReserveShortfall, SeverityCritical, out); err != nil {
			return SupplyReconciliation{}, err
		}
	}
	out.BreakerTripped = wasActive && a.Status == StatusPaused
	return out, nil
}

func reconDocuments(r SupplyReconciliation) (expected, observed, difference map[string]string) {
	expected = map[string]string{
		"ledger_outstanding": r.LedgerOutstanding.String(), "expected_chain_supply": r.ExpectedChainSupply.String(),
		"landed_unposted_mints": r.LandedUnpostedMints.String(), "landed_unposted_burns": r.LandedUnpostedBurns.String(),
		"supply_value": r.SupplyValue.String(),
	}
	observed = map[string]string{
		"chain_supply": r.ChainSupply.String(), "customer_wallet_sum": r.CustomerWalletSum.String(),
		"eligible_reserve": r.EligibleReserve.String(), "coverage_bps": money.QuantityFromInt64(int64(r.CoverageBPS)).String(),
	}
	difference = map[string]string{
		"chain_minus_expected":       r.ChainSupply.Sub(r.ExpectedChainSupply).String(),
		"wallets_minus_outstanding":  r.CustomerWalletSum.Sub(r.LedgerOutstanding).String(),
		"supply_value_minus_reserve": r.SupplyValue.Sub(r.EligibleReserve).String(),
	}
	return expected, observed, difference
}

func (s *Service) reconRequest(a PlatformStableAsset, r SupplyReconciliation, mode reconciliation.Mode, actor econ.Actor, status reconciliation.Status) reconciliation.OpenRequest {
	expected, observed, difference := reconDocuments(r)
	reason := "stable asset books, wallets, chain supply and verified reserve agree"
	if status == reconciliation.StatusMismatch {
		reason = "stable asset books disagree with the chain, the wallets or the verified reserve"
	}
	return reconciliation.OpenRequest{
		Kind: reconciliation.KindStableReserve, Mode: mode, ScopeType: ScopeStableAsset, ScopeID: a.ID.String(),
		AssetID: a.AssetID, Expected: expected, Observed: observed, Difference: difference, Status: status,
		Material: status == reconciliation.StatusMismatch, BlocksNewRisk: status == reconciliation.StatusMismatch,
		Actor: reconciliation.Actor{Type: actor.Type, ID: actor.ID}, Reason: reason,
	}
}

// breakerActor is the actor of an automatic pause.
var breakerActor = econ.SystemActor("stableasset-circuit-breaker")

// trip records an alert and, if the asset is ACTIVE, pauses it with a bound
// transition naming the reason. The breaker only ever pauses; resuming is an
// operator's decision (Transition).
func (s *Service) trip(ctx context.Context, tx pgx.Tx, a *PlatformStableAsset, kind AlertKind, sev Severity, observed any) error {
	tripping := a.Status == StatusActive
	if err := s.alert(ctx, tx, *a, kind, sev, observed, tripping); err != nil {
		return err
	}
	if !tripping {
		return nil
	}
	paused, err := s.transitionLocked(ctx, tx, *a, StatusPaused, breakerActor, "circuit breaker: "+string(kind))
	if err != nil {
		return err
	}
	*a = paused
	return nil
}

// alert appends a peg-monitor alert.
func (s *Service) alert(ctx context.Context, tx pgx.Tx, a PlatformStableAsset, kind AlertKind, sev Severity, observed any, tripped bool) error {
	body, err := json.Marshal(observed)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "stableasset: encode alert")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stable_peg_alerts (id, stable_asset_id, kind, severity, observed, breaker_tripped, observed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, id.New[id.Any](), a.ID, kind, sev, body, tripped, s.clk.Now().UTC()); err != nil {
		return dbErr("record alert", err)
	}
	return nil
}
