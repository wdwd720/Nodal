package stableasset

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Refusal reason codes: the rejection_code written on the economic intent
// and the "reason" field of the returned error. Stable and machine-readable.
const (
	RefusalRouteDenied              = "STABLE_ROUTE_DENIED"
	RefusalAccountIneligible        = "STABLE_ACCOUNT_INELIGIBLE"
	RefusalAssetNotActive           = "STABLE_ASSET_NOT_ACTIVE"
	RefusalReserveAssetIneligible   = "STABLE_RESERVE_ASSET_INELIGIBLE"
	RefusalReservePriceUnavailable  = "STABLE_RESERVE_PRICE_UNAVAILABLE"
	RefusalCapExceeded              = reasonCapExceeded
	RefusalReserveShortfall         = reasonReserveShortfall
	RefusalRiskRejected             = "RISK_REJECTED"
	RefusalInsufficientFunds        = "STABLE_INSUFFICIENT_FUNDS"
	RefusalChainBalanceInsufficient = "STABLE_CHAIN_BALANCE_INSUFFICIENT"
	RefusalRedemptionBelowMinimum   = "STABLE_REDEMPTION_BELOW_MINIMUM"
	RefusalReserveInsufficient      = "STABLE_RESERVE_INSUFFICIENT_FOR_REDEMPTION"
)

// AccountFacts are the facts about the requesting account the HTTP layer
// resolved: the legal router keys on them and Risk V2 reads identity.
type AccountFacts struct {
	Jurisdiction     string
	Verification     valuedomain.VerificationLevel
	IdentityVerified bool
}

// refusal is a business refusal: recorded (intent REJECTED, decision
// persisted) and committed, then returned to the caller.
type refusal struct {
	code string
	err  error
}

func refuse(reason, detail string) refusal {
	return refusal{code: reason, err: errs.New(refusalCode(reason), "stableasset: "+detail).WithField("reason", reason)}
}

// refusalCode is the boundary code of each refusal reason, so a refusal and
// its idempotent replay answer with the same code.
func refusalCode(reason string) errs.Code {
	switch reason {
	case RefusalRouteDenied:
		return errs.CodeForbidden
	case RefusalAccountIneligible:
		return errs.CodeAccountFrozen
	case RefusalAssetNotActive, RefusalReserveShortfall, RefusalReserveInsufficient:
		return errs.CodeAssetRestricted
	case RefusalReserveAssetIneligible, RefusalRedemptionBelowMinimum:
		return errs.CodeValidationFailed
	case RefusalReservePriceUnavailable:
		return errs.CodeStaleMarketData
	case RefusalCapExceeded:
		return errs.CodeRiskMaxPosition
	case RefusalRiskRejected:
		return errs.CodeRiskRejected
	case RefusalInsufficientFunds, RefusalChainBalanceInsufficient:
		return errs.CodeInsufficientBuyingPower
	}
	return errs.CodeConflict
}

// authenticate checks that the actor is who the context says and owns the
// account. A request without a principal is accepted only from a SYSTEM or
// SERVICE actor (in-process background work); an agent is never accepted.
func (s *Service) authenticate(ctx context.Context, actor econ.Actor, account accounts.AccountID) error {
	if err := requireActor(actor); err != nil {
		return err
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		if actor.Type == security.ActorSystem || actor.Type == security.ActorService {
			return nil
		}
		return errs.New(errs.CodeUnauthenticated, "stableasset: a request on behalf of a person needs an authenticated principal")
	}
	if p.IsAgent() {
		return errs.New(errs.CodeForbidden, "stableasset: an agent never mints or burns")
	}
	if p.SubjectID != actor.ID || p.ActorType != actor.Type {
		return errs.New(errs.CodeForbidden, "stableasset: the request's actor is not the authenticated principal")
	}
	if err := security.RequireAccountOwner(ctx, account.String()); err != nil {
		return errs.Wrap(err, errs.CodeForbidden, "stableasset: the principal does not own the account")
	}
	return nil
}

// reject moves the intent to REJECTED with the refusal's code.
func (s *Service) reject(ctx context.Context, tx pgx.Tx, iid econ.IntentID, actor econ.Actor, r refusal) error {
	_, err := s.intents.Transition(ctx, tx, iid, econ.StatusRejected, actor, r.err.Error(), r.code)
	return err
}

// route compiles the intent through the settlement compiler (agent ladder,
// legal router, simulated domain). A refusal carries every reason.
func (s *Service) route(in econ.Intent, a PlatformStableAsset, facts AccountFacts) (refusal, bool, error) {
	rt, err := econ.Route(econ.RouteRequest{
		Intent:       in,
		Subject:      settlement.Subject{Type: settlement.SubjectFacility, ID: a.FacilityID.String(), AssetID: a.AssetID.String()},
		Jurisdiction: facts.Jurisdiction,
		Verification: facts.Verification,
	}, s.router, s.clk.Now().UTC())
	if err != nil {
		return refusal{}, false, err
	}
	if !rt.MayExecuteNow() {
		reasons := append([]string(nil), rt.Reasons...)
		if rt.Permitted {
			reasons = append(reasons, "CONFIRMATION_REQUIRED")
		}
		r := refuse(RefusalRouteDenied, "the settlement compiler refused the intent")
		if e, ok := errs.As(r.err); ok {
			r.err = e.WithField("route_reasons", reasons)
		}
		return r, false, nil
	}
	return refusal{}, true, nil
}

// riskSections are the Risk V2 inputs the caller computed.
type riskSections struct {
	notional       money.USD
	available      money.USD
	required       money.USD
	riskIncreasing bool
	// chainHealthy: the chain was readable through the caller's transaction.
	chainHealthy bool
}

// evaluateRisk builds the Risk V2 input with every section MINT_STABLE_ASSET
// and BURN_STABLE_ASSET require (account, identity, balance, chain,
// settlement), evaluates it, persists the decision and returns the refusal
// when the kernel rejects.
func (s *Service) evaluateRisk(ctx context.Context, tx pgx.Tx, in econ.Intent, a PlatformStableAsset, acct accounts.Account,
	facts AccountFacts, sec riskSections) (refusal, bool, error) {
	unknown, err := s.unknownEffects(ctx, tx, in.AccountID)
	if err != nil {
		return refusal{}, false, err
	}
	breakers, err := s.openBreakers(ctx, tx, a)
	if err != nil {
		return refusal{}, false, err
	}
	snap, err := s.kills.Snapshot(ctx, tx)
	if err != nil {
		return refusal{}, false, err
	}
	kills := make([]risk.KillSwitch, 0, len(snap.Switches))
	for _, sw := range snap.Switches {
		kills = append(kills, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
	}
	input := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{
			ID: in.ID.String(), Kind: string(in.Kind), AccountID: in.AccountID.String(), ActorType: string(in.ActorType),
			Capability: string(in.Capability), Mode: string(in.Mode), NotionalUSD: sec.notional, RiskIncreasing: sec.riskIncreasing,
		},
		Identity:        &risk.IdentitySnapshot{Verified: facts.IdentityVerified},
		Account:         &risk.AdvancedAccountSnapshot{Status: string(acct.Status)},
		Balance:         &risk.BalanceSnapshot{Available: sec.available, Required: sec.required},
		Chain:           &risk.ChainSnapshot{Healthy: sec.chainHealthy},
		Settlement:      &risk.SettlementSnapshot{UnknownEffects: unknown},
		CircuitBreakers: breakers,
		KillSwitches:    kills,
		Now:             s.clk.Now().UTC(),
	}
	decision := risk.EvaluateAdvanced(s.policy, input)
	if _, err := s.risk.RecordAdvancedDecision(ctx, tx, input, decision, in.CorrelationID); err != nil {
		return refusal{}, false, err
	}
	if rerr := decision.RejectionError(); rerr != nil {
		return refusal{code: RefusalRiskRejected, err: rerr}, false, nil
	}
	return refusal{}, true, nil
}

// unknownEffects counts the account's stable-asset effects whose outcome is
// UNKNOWN: an unknown settlement blocks new activity until it is reconciled.
func (s *Service) unknownEffects(ctx context.Context, tx pgx.Tx, account accounts.AccountID) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM economic_effects
		WHERE subsystem = $1 AND account_id = $2 AND status = 'UNKNOWN'`, econ.SubsystemStableAsset, account).Scan(&n); err != nil {
		return 0, dbErr("count unknown effects", err)
	}
	return n, nil
}

// openBreakers names the unresolved STABLE_RESERVE reconciliation records
// of the asset that block new risk: the reserve-mismatch circuit breakers.
func (s *Service) openBreakers(ctx context.Context, tx pgx.Tx, a PlatformStableAsset) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM reconciliation_records
		WHERE kind = $1 AND scope_type = $2 AND scope_id = $3 AND blocks_new_risk
		  AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED') ORDER BY id`,
		string(reconciliation.KindStableReserve), ScopeStableAsset, a.ID.String())
	if err != nil {
		return nil, dbErr("read circuit breakers", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var rid string
		if err := rows.Scan(&rid); err != nil {
			return nil, dbErr("scan circuit breaker", err)
		}
		out = append(out, "STABLE_RESERVE_MISMATCH:"+rid)
	}
	return out, rows.Err()
}

// isBusinessRefusal reports whether a capital error is a refusal about the
// request (the transaction is still usable) rather than a failure.
func isBusinessRefusal(err error) bool {
	switch errs.CodeOf(err) {
	case errs.CodeInsufficientBuyingPower, errs.CodeAccountFrozen, errs.CodeAssetRestricted:
		return true
	}
	return false
}

// ScopeStableAsset is the reconciliation scope type of a stable asset.
const ScopeStableAsset = "stable_asset"

func trimmed(s string) bool { return strings.TrimSpace(s) == s && s != "" }
