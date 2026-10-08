package margin

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/settlement"
)

// reservationTTL bounds the capital reservations this service takes; each is
// consumed inside the transaction that took it.
const reservationTTL = time.Minute

// velocityWindow is the window of the release velocity check.
const velocityWindow = 24 * time.Hour

// CollateralRequest moves collateral into or out of a margin account.
type CollateralRequest struct {
	AccountID      accounts.AccountID
	AssetID        assets.AssetID
	Quantity       money.Quantity
	IdempotencyKey string
	Requester      Requester
	CorrelationID  string
}

func (r CollateralRequest) validate() error {
	if r.AccountID.IsZero() || r.AssetID.IsZero() {
		return errs.New(errs.CodeValidationFailed, "margin: account and asset are required")
	}
	if !r.Quantity.IsPositive() {
		return errs.New(errs.CodeValidationFailed, "margin: a collateral quantity must be positive")
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return err
	}
	return r.Requester.validate()
}

// CollateralResult is the outcome of a collateral move.
type CollateralResult struct {
	MarginAccountID      AccountID
	MoveID               MoveID
	IntentID             *econ.IntentID
	JournalTransactionID ledger.TransactionID
	EffectID             econ.EffectID
	ReservationID        string
	Replayed             bool
	Status               AccountStatus
	Evaluation           Evaluation
}

// cents values qty of an asset of the state at its mark, in cents, rounded
// in mode; an unpriced asset is worth nothing.
func cents(st AccountState, h Holding, qty money.Quantity, mode money.RoundingMode) (money.USD, error) {
	if h.Numeraire {
		return unitsToUSD(qty, st.NumeraireDecimals, mode)
	}
	if h.Mark == nil {
		return money.USD{}, nil
	}
	v, err := notional(qty, h.Decimals, *h.Mark, st.NumeraireDecimals, mode)
	if err != nil {
		return money.USD{}, err
	}
	return unitsToUSD(v, st.NumeraireDecimals, mode)
}

// replayedEffect reports whether an effect key was already applied with the
// same request. A different request under the key is refused; an effect
// that is not APPLIED is never retried.
func (s *Service) replayedEffect(ctx context.Context, tx pgx.Tx, key, kind string, acct accounts.AccountID, request any) (econ.Effect, bool, error) {
	if _, found, err := s.d.Effects.GetByKey(ctx, tx, key); err != nil || !found {
		return econ.Effect{}, false, err
	}
	e, _, err := s.d.Effects.Begin(ctx, tx, econ.BeginRequest{Key: key, Subsystem: econ.SubsystemMargin, Kind: kind,
		AccountID: acct.String(), Request: request, PostsLedger: true, Actor: s.actor})
	if err != nil {
		return econ.Effect{}, false, err
	}
	if e.Status != econ.EffectApplied {
		return econ.Effect{}, false, errs.Newf(errs.CodeSubmissionStateUnknown, "margin: effect %s is %s and is not retried", e.ID, e.Status)
	}
	return e, true, nil
}

func (s *Service) checkCollateralAsset(ctx context.Context, tx pgx.Tx, a Account, aid assets.AssetID) error {
	info, err := s.assetInfo(ctx, tx, aid)
	if err != nil {
		return err
	}
	if !simulatedAsset(info.asset) {
		return errs.New(errs.CodeValidationFailed, "margin: margin collateral is SIMULATED value only").WithField("asset_id", aid.String())
	}
	if aid == a.NumeraireAssetID {
		return nil
	}
	if !info.eligible {
		return errs.New(errs.CodeAssetRestricted, "margin: this asset is not eligible as margin collateral under the policy").
			WithField("asset_id", aid.String())
	}
	if a.MarginMode == ModeIsolated && *a.IsolatedAssetID != aid {
		return errs.New(errs.CodeValidationFailed, "margin: an ISOLATED account holds only its numeraire and its isolated asset").
			WithField("asset_id", aid.String())
	}
	return nil
}

// PostCollateral moves collateral from the customer's wallet into the margin
// account (MARGIN_COLLATERAL_MOVE: Dr MARGIN_COLLATERAL / Cr WALLET). The
// wallet quantity is reserved through capital first, so it cannot be spent
// twice; the move is exactly once per idempotency key. Posting collateral
// reduces risk, so it is allowed while a margin call or a liquidation is
// open and is never stopped by a kill switch.
func (s *Service) PostCollateral(ctx context.Context, tx pgx.Tx, r CollateralRequest) (CollateralResult, error) {
	if err := r.validate(); err != nil {
		return CollateralResult{}, err
	}
	a, err := s.lockAccount(ctx, tx, r.AccountID)
	if err != nil {
		return CollateralResult{}, err
	}
	key := "margin:collateral-post:" + a.ID.String() + ":" + keyHash(r.IdempotencyKey)
	req := map[string]string{"margin_account_id": a.ID.String(), "asset_id": r.AssetID.String(), "quantity": r.Quantity.String(),
		"direction": string(MovePost), "idempotency_key": r.IdempotencyKey}
	if e, done, err := s.replayedEffect(ctx, tx, key, "COLLATERAL_POST", a.AccountID, req); err != nil {
		return CollateralResult{}, err
	} else if done {
		mv, err := s.st.moveByEffect(ctx, tx, e.ID)
		if err != nil {
			return CollateralResult{}, err
		}
		return CollateralResult{MarginAccountID: a.ID, MoveID: mv, JournalTransactionID: *e.JournalTransactionID, EffectID: e.ID,
			ReservationID: e.ReservationID, Replayed: true, Status: a.Status}, nil
	}
	if !a.Status.AllowsRiskReduction() {
		return CollateralResult{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: a %s account takes no collateral", a.Status)
	}
	if err := s.authorize(ctx, tx, a.Mode, true, gates.Margin); err != nil {
		return CollateralResult{}, err
	}
	if err := s.checkCollateralAsset(ctx, tx, a, r.AssetID); err != nil {
		return CollateralResult{}, err
	}
	now := s.now()
	res, err := s.d.Capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: a.AccountID.String(), AssetID: r.AssetID, Quantity: r.Quantity, ActorType: r.Requester.Type,
		ActorID: r.Requester.ID, IdempotencyKey: key + ":reserve", TTL: reservationTTL, Reason: "margin collateral post",
	})
	if err != nil {
		return CollateralResult{}, err
	}
	m := s.meta("margin_collateral_move", a.ID.String(), "margin collateral posted")
	p, err := CollateralMovePosting(m, a.AccountID, r.AssetID, r.Quantity, MovePost)
	if err != nil {
		return CollateralResult{}, err
	}
	out, err := s.postOnce(ctx, tx, postSpec{key: key, kind: "COLLATERAL_POST", account: a.AccountID, request: req, posting: p,
		evidence: req, reserve: res.ID.String(), corr: r.CorrelationID})
	if err != nil {
		return CollateralResult{}, err
	}
	if _, err := s.d.Capital.ConsumeFinal(ctx, tx, res.ID, r.Quantity, 0, ""); err != nil {
		return CollateralResult{}, err
	}
	state, _, err := s.loadState(ctx, tx, a, now)
	if err != nil {
		return CollateralResult{}, err
	}
	value := money.USD{}
	if i := state.holding(r.AssetID.String()); i >= 0 {
		if value, err = cents(state, state.Holdings[i], r.Quantity, money.RoundFloor); err != nil {
			return CollateralResult{}, err
		}
	}
	mv, err := s.st.insertMove(ctx, tx, a.ID, r.AssetID, MovePost, r.Quantity, value.Minor(), out.Journal, out.Effect.ID, nil, now)
	if err != nil {
		return CollateralResult{}, err
	}
	ev, _, err := s.refreshHealth(ctx, tx, &a, "collateral posted")
	if err != nil {
		return CollateralResult{}, err
	}
	return CollateralResult{MarginAccountID: a.ID, MoveID: mv, JournalTransactionID: out.Journal, EffectID: out.Effect.ID,
		ReservationID: res.ID.String(), Status: a.Status, Evaluation: ev}, nil
}

// ReleaseCollateral moves collateral out of the margin account to the
// customer's wallet (MARGIN_COLLATERAL_MOVE reversed). It is the economic
// intent TRANSFER_VALUE to the owner's own account, so an agent can never
// request it, and Risk Kernel V2 refuses it with
// RISK_WITHDRAW_BREACHES_MARGIN when equity after the release would be below
// maintenance -- and, while the account owes anything, with
// RISK_MARGIN_INITIAL, RISK_LEVERAGE_CAP or a stale-oracle code when the
// release would leave it below initial margin, above its leverage cap, or
// valued on a mark nobody can vouch for.
func (s *Service) ReleaseCollateral(ctx context.Context, tx pgx.Tx, r CollateralRequest) (CollateralResult, error) {
	if err := r.validate(); err != nil {
		return CollateralResult{}, err
	}
	a, err := s.lockAccount(ctx, tx, r.AccountID)
	if err != nil {
		return CollateralResult{}, err
	}
	sp := intentSpec{
		kind: econ.KindTransferValue, capability: gates.Margin, account: a, requester: r.Requester, key: r.IdempotencyKey,
		asset: r.AssetID, quantity: r.Quantity, corr: r.CorrelationID,
		terms:   map[string]string{"margin_account_id": a.ID.String(), "direction": string(MoveRelease), "destination": "OWN_WALLET"},
		subject: settlement.Subject{Type: settlement.SubjectAccount, ID: a.AccountID.String(), AssetID: r.AssetID.String()},
	}
	in, existing, err := s.submitIntent(ctx, tx, sp)
	if err != nil {
		return CollateralResult{}, err
	}
	key := "margin:release:" + in.ID.String()
	if existing {
		if err := replayIntent(in); err != nil {
			return CollateralResult{}, err
		}
		e, found, err := s.d.Effects.GetByKey(ctx, tx, key)
		if err != nil || !found {
			return CollateralResult{}, errs.New(errs.CodeInternal, "margin: a completed release has no effect")
		}
		mv, err := s.st.moveByEffect(ctx, tx, e.ID)
		if err != nil {
			return CollateralResult{}, err
		}
		iid := in.ID
		return CollateralResult{MarginAccountID: a.ID, MoveID: mv, IntentID: &iid, JournalTransactionID: *e.JournalTransactionID,
			EffectID: e.ID, Replayed: true, Status: a.Status}, nil
	}
	res, err := s.release(ctx, tx, a, &in, sp, key, r)
	if err != nil {
		return CollateralResult{}, s.abandon(ctx, tx, &in, err)
	}
	return res, nil
}

func (s *Service) release(ctx context.Context, tx pgx.Tx, a Account, in *econ.Intent, sp intentSpec, key string, r CollateralRequest) (CollateralResult, error) {
	if err := s.route(ctx, tx, in, sp); err != nil {
		return CollateralResult{}, err
	}
	if !a.Status.AllowsNewRisk() {
		return CollateralResult{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: a %s account releases no collateral", a.Status)
	}
	if err := s.authorize(ctx, tx, a.Mode, false, gates.Margin); err != nil {
		return CollateralResult{}, err
	}
	now := s.now()
	state, _, err := s.loadState(ctx, tx, a, now)
	if err != nil {
		return CollateralResult{}, err
	}
	i := state.holding(r.AssetID.String())
	if i < 0 {
		return CollateralResult{}, errs.New(errs.CodeInsufficientBuyingPower, "margin: the account holds none of this asset")
	}
	held := state.Holdings[i]
	after, err := WithRelease(state, r.AssetID.String(), r.Quantity)
	if err != nil {
		return CollateralResult{}, err
	}
	evAfter, err := Evaluate(s.policy, after)
	if err != nil {
		return CollateralResult{}, err
	}
	sections, err := RiskSectionsFor(evAfter)
	if err != nil {
		return CollateralResult{}, err
	}
	value, err := cents(state, held, r.Quantity, money.RoundCeil)
	if err != nil {
		return CollateralResult{}, err
	}
	available, err := cents(state, held, held.Quantity, money.RoundFloor)
	if err != nil {
		return CollateralResult{}, err
	}
	count, window, err := s.st.releaseVelocity(ctx, tx, a.ID, now.Add(-velocityWindow))
	if err != nil {
		return CollateralResult{}, err
	}
	ai := risk.AdvancedIntent{Kind: string(econ.KindTransferValue), RiskIncreasing: evAfter.Levered, NotionalUSD: value}
	if _, err := s.decide(ctx, tx, in, a, ai, func(input *risk.AdvancedInput) {
		input.Balance = &risk.BalanceSnapshot{Available: available, Required: value}
		input.Velocity = &risk.VelocitySnapshot{CountInWindow: count, NotionalInWindow: window}
		input.Destination = &risk.DestinationSnapshot{Allowlisted: true, AddedAt: a.OpenedAt}
		input.Margin, input.Oracle, input.Concentration = sections.Margin, sections.Oracle, sections.Concentration
	}); err != nil {
		return CollateralResult{}, err
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusExecuting, "releasing collateral", ""); err != nil {
		return CollateralResult{}, err
	}
	m := s.meta("margin_collateral_move", a.ID.String(), "margin collateral released")
	p, err := CollateralMovePosting(m, a.AccountID, r.AssetID, r.Quantity, MoveRelease)
	if err != nil {
		return CollateralResult{}, err
	}
	req := map[string]string{"margin_account_id": a.ID.String(), "intent_id": in.ID.String(), "asset_id": r.AssetID.String(),
		"quantity": r.Quantity.String(), "direction": string(MoveRelease)}
	iid := in.ID
	out, err := s.postOnce(ctx, tx, postSpec{key: key, kind: "COLLATERAL_RELEASE", intent: &iid, account: a.AccountID, request: req,
		posting: p, evidence: map[string]any{"request": req, "evaluation_after": evAfter}, corr: r.CorrelationID})
	if err != nil {
		return CollateralResult{}, err
	}
	releaseValue, err := cents(state, held, r.Quantity, money.RoundFloor)
	if err != nil {
		return CollateralResult{}, err
	}
	mv, err := s.st.insertMove(ctx, tx, a.ID, r.AssetID, MoveRelease, r.Quantity, releaseValue.Minor(), out.Journal, out.Effect.ID, &iid, now)
	if err != nil {
		return CollateralResult{}, err
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusCompleted, "collateral released", ""); err != nil {
		return CollateralResult{}, err
	}
	ev, _, err := s.refreshHealth(ctx, tx, &a, "collateral released")
	if err != nil {
		return CollateralResult{}, err
	}
	return CollateralResult{MarginAccountID: a.ID, MoveID: mv, IntentID: &iid, JournalTransactionID: out.Journal,
		EffectID: out.Effect.ID, Status: a.Status, Evaluation: ev}, nil
}
