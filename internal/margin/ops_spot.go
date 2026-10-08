package margin

import (
	"context"

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

// SpotRequest buys Quantity base units of an asset on margin.
type SpotRequest struct {
	AccountID      accounts.AccountID
	AssetID        assets.AssetID
	Quantity       money.Quantity
	IdempotencyKey string
	Requester      Requester
	CorrelationID  string
}

// SpotResult is the outcome of a leveraged buy.
type SpotResult struct {
	MarginAccountID AccountID
	IntentID        econ.IntentID
	LotID           *LotID
	// Mark is the price the buy was valued at; ExecutionPrice is mark × (1 +
	// SlippageBPS), the disclosed paper slippage; Cost is quantity ×
	// execution price, rounded up, and is what was borrowed.
	Mark           money.Price
	ExecutionPrice money.Price
	SlippageBPS    money.BPS
	Cost           money.Quantity
	BorrowJournal  ledger.TransactionID
	TradeJournal   ledger.TransactionID
	CollateralJrnl ledger.TransactionID
	ReservationID  string
	Replayed       bool
	Status         AccountStatus
	Evaluation     Evaluation
}

// OpenLeveragedSpot buys an asset entirely on margin, in one transaction:
//
//  1. MARGIN_BORROW of the cost from the lending pool into the wallet (a new
//     lot),
//  2. a capital reservation of that cost on the wallet, so nothing else can
//     spend it,
//  3. a paper SIM_TRADE at mark × (1 + the policy's disclosed slippage),
//     consuming the reservation,
//  4. MARGIN_COLLATERAL_MOVE of the bought quantity into the margin account.
//
// It is the economic intent ACQUIRE_EXPOSURE, risk-increasing: Risk Kernel V2
// refuses it unless equity after the buy covers initial margin, leverage
// stays within the mode's cap, the asset is tradable, the venue healthy and
// every mark fresh. Each posting is its own exactly-once effect keyed on the
// intent, so a replay posts nothing.
func (s *Service) OpenLeveragedSpot(ctx context.Context, tx pgx.Tx, r SpotRequest) (SpotResult, error) {
	if r.AccountID.IsZero() || r.AssetID.IsZero() || !r.Quantity.IsPositive() {
		return SpotResult{}, errs.New(errs.CodeValidationFailed, "margin: account, asset and a positive quantity are required")
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return SpotResult{}, err
	}
	if err := r.Requester.validate(); err != nil {
		return SpotResult{}, err
	}
	a, err := s.lockAccount(ctx, tx, r.AccountID)
	if err != nil {
		return SpotResult{}, err
	}
	if a.MarginMode == ModeCash {
		return SpotResult{}, errs.New(errs.CodeUnsupported, "margin: a CASH margin account takes no leverage")
	}
	if r.AssetID == a.NumeraireAssetID {
		return SpotResult{}, errs.New(errs.CodeValidationFailed, "margin: the numeraire is not a position")
	}
	sp := intentSpec{
		kind: econ.KindAcquireExposure, capability: gates.Leverage, account: a, requester: r.Requester, key: r.IdempotencyKey,
		asset: r.AssetID, quantity: r.Quantity, corr: r.CorrelationID,
		terms: map[string]string{"margin_account_id": a.ID.String(), "side": "BUY", "financing": "MARGIN_BORROW",
			"quote_asset_id": a.NumeraireAssetID.String()},
		subject: settlement.Subject{Type: settlement.SubjectInstrument, ID: "spot:" + r.AssetID.String() + "/" + a.NumeraireAssetID.String(),
			AssetID: r.AssetID.String()},
	}
	in, existing, err := s.submitIntent(ctx, tx, sp)
	if err != nil {
		return SpotResult{}, err
	}
	prefix := "margin:spot:" + in.ID.String()
	if existing {
		if err := replayIntent(in); err != nil {
			return SpotResult{}, err
		}
		out := SpotResult{MarginAccountID: a.ID, IntentID: in.ID, Replayed: true, Status: a.Status}
		for _, k := range []struct {
			key string
			dst *ledger.TransactionID
		}{{prefix + ":borrow", &out.BorrowJournal}, {prefix + ":trade", &out.TradeJournal}, {prefix + ":collateral", &out.CollateralJrnl}} {
			e, found, err := s.d.Effects.GetByKey(ctx, tx, k.key)
			if err != nil || !found {
				return SpotResult{}, errs.New(errs.CodeInternal, "margin: a completed leveraged buy is missing an effect")
			}
			*k.dst = *e.JournalTransactionID
			if k.key == prefix+":borrow" {
				lot, err := s.st.lotByEffect(ctx, tx, e.ID)
				if err != nil {
					return SpotResult{}, err
				}
				out.LotID, out.Cost = &lot.ID, lot.Principal
			}
		}
		return out, nil
	}
	res, err := s.openSpot(ctx, tx, a, &in, sp, prefix, r)
	if err != nil {
		return SpotResult{}, s.abandon(ctx, tx, &in, err)
	}
	return res, nil
}

func (s *Service) openSpot(ctx context.Context, tx pgx.Tx, a Account, in *econ.Intent, sp intentSpec, prefix string, r SpotRequest) (SpotResult, error) {
	if err := s.route(ctx, tx, in, sp); err != nil {
		return SpotResult{}, err
	}
	if !a.Status.AllowsNewRisk() {
		return SpotResult{}, errs.Newf(errs.CodeInvalidStateTransition, "margin: a %s account takes no new leverage", a.Status)
	}
	if err := s.authorize(ctx, tx, a.Mode, false, gates.Leverage, gates.Margin); err != nil {
		return SpotResult{}, err
	}
	if err := s.checkCollateralAsset(ctx, tx, a, r.AssetID); err != nil {
		return SpotResult{}, err
	}
	pool, _, err := s.accountFacilities(ctx, tx, a)
	if err != nil {
		return SpotResult{}, err
	}
	if err := pool.RequireActive(); err != nil {
		return SpotResult{}, err
	}
	now := s.now()
	state, _, err := s.loadState(ctx, tx, a, now)
	if err != nil {
		return SpotResult{}, err
	}
	template, info, err := s.holdingFor(ctx, tx, a, r.AssetID, money.Quantity{}, now)
	if err != nil {
		return SpotResult{}, err
	}
	venueUp, err := s.d.Venue.Available(ctx, r.AssetID)
	if err != nil {
		return SpotResult{}, err
	}
	instrument := &risk.InstrumentSnapshot{Status: string(info.asset.Status), SessionOpen: true, Expired: false}
	if template.Mark == nil {
		// Nothing to price the buy at: the decision records the missing oracle
		// and refuses; no state is invented to evaluate.
		ai := risk.AdvancedIntent{Kind: string(econ.KindAcquireExposure), InstrumentKind: "SPOT_PAIR", RiskIncreasing: true}
		_, err := s.decide(ctx, tx, in, a, ai, func(input *risk.AdvancedInput) {
			input.Instrument = instrument
			input.Oracle = &risk.OracleSnapshot{Available: false, ObservedAt: s.d.Clock.Now().UTC()}
			input.Venue = &risk.VenueSnapshot{Healthy: venueUp}
		})
		if err == nil {
			err = errs.New(errs.CodeStaleMarketData, "margin: no mark for the asset")
		}
		return SpotResult{}, err
	}
	exec, err := scalePrice(*template.Mark, int64(10_000+s.policy.OpenSlippageBPS))
	if err != nil {
		return SpotResult{}, err
	}
	lotID := newLotID()
	after, cost, err := WithLeveragedBuy(state, template, r.Quantity, exec, lotID.String(), s.policy.BorrowRateBPS)
	if err != nil {
		return SpotResult{}, err
	}
	evAfter, err := Evaluate(s.policy, after)
	if err != nil {
		return SpotResult{}, err
	}
	sections, err := RiskSectionsFor(evAfter)
	if err != nil {
		return SpotResult{}, err
	}
	poolCash, err := s.d.Ledger.Balance(ctx, tx, pool.Account(ledger.CodePoolCash, a.NumeraireAssetID))
	if err != nil {
		return SpotResult{}, err
	}
	costUSD, err := unitsToUSD(cost, state.NumeraireDecimals, money.RoundCeil)
	if err != nil {
		return SpotResult{}, err
	}
	poolUSD, err := unitsToUSD(poolCash, state.NumeraireDecimals, money.RoundFloor)
	if err != nil {
		return SpotResult{}, err
	}
	ai := risk.AdvancedIntent{Kind: string(econ.KindAcquireExposure), InstrumentKind: "SPOT_PAIR", RiskIncreasing: true, NotionalUSD: costUSD}
	if _, err := s.decide(ctx, tx, in, a, ai, func(input *risk.AdvancedInput) {
		input.Instrument = instrument
		input.Venue = &risk.VenueSnapshot{Healthy: venueUp}
		input.Margin, input.Oracle, input.Concentration = sections.Margin, sections.Oracle, sections.Concentration
		input.Liquidity = &risk.LiquiditySnapshot{AvailableUSD: poolUSD}
	}); err != nil {
		return SpotResult{}, err
	}
	if poolCash.Cmp(cost) < 0 {
		return SpotResult{}, errs.New(errs.CodeInsufficientBuyingPower, "margin: the lending pool does not hold that much cash")
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusExecuting, "buying on margin", ""); err != nil {
		return SpotResult{}, err
	}
	iid := in.ID
	reqBase := map[string]string{"margin_account_id": a.ID.String(), "intent_id": in.ID.String(), "asset_id": r.AssetID.String(),
		"quantity": r.Quantity.String(), "cost": cost.String(), "mark": template.Mark.String(), "execution_price": exec.String(),
		"slippage_bps": s.policy.OpenSlippageBPS.String()}
	disclosure := map[string]any{"request": reqBase, "evaluation_after": evAfter}

	// 1. Borrow the cost into the wallet.
	bm := s.meta("margin_borrow_lot", lotID.String(), "margin borrow for a leveraged buy")
	bp, err := BorrowPosting(bm, a.AccountID, pool, a.NumeraireAssetID, cost)
	if err != nil {
		return SpotResult{}, err
	}
	borrowed, err := s.postOnce(ctx, tx, postSpec{key: prefix + ":borrow", kind: "SPOT_BORROW", intent: &iid, account: a.AccountID,
		request: reqBase, posting: bp, evidence: disclosure, corr: r.CorrelationID})
	if err != nil {
		return SpotResult{}, err
	}
	if err := s.st.insertLot(ctx, tx, Lot{ID: lotID, MarginAccountID: a.ID, AssetID: a.NumeraireAssetID, Principal: cost,
		RateBPS: s.policy.BorrowRateBPS, OpenedAt: now, BorrowJournalID: borrowed.Journal, EffectID: borrowed.Effect.ID}); err != nil {
		return SpotResult{}, err
	}
	// 2. Reserve the cost on the wallet.
	res, err := s.d.Capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: a.AccountID.String(), AssetID: a.NumeraireAssetID, Quantity: cost,
		ActorType: r.Requester.Type, ActorID: r.Requester.ID, IdempotencyKey: prefix + ":reserve", TTL: reservationTTL,
		Reason: "margin leveraged buy",
	})
	if err != nil {
		return SpotResult{}, err
	}
	// 3. The paper trade at the disclosed slippage.
	tm := s.meta("margin_spot_trade", in.ID.String(), "paper buy at mark plus disclosed slippage")
	tp, err := SpotBuyPosting(tm, a.AccountID, a.NumeraireAssetID, cost, r.AssetID, r.Quantity)
	if err != nil {
		return SpotResult{}, err
	}
	traded, err := s.postOnce(ctx, tx, postSpec{key: prefix + ":trade", kind: "SPOT_TRADE", intent: &iid, account: a.AccountID,
		request: reqBase, posting: tp, evidence: disclosure, reserve: res.ID.String(), corr: r.CorrelationID})
	if err != nil {
		return SpotResult{}, err
	}
	if _, err := s.d.Capital.ConsumeFinal(ctx, tx, res.ID, cost, 0, ""); err != nil {
		return SpotResult{}, err
	}
	// 4. The bought quantity becomes collateral.
	cm := s.meta("margin_collateral_move", in.ID.String(), "leveraged position into margin collateral")
	cp, err := CollateralMovePosting(cm, a.AccountID, r.AssetID, r.Quantity, MovePost)
	if err != nil {
		return SpotResult{}, err
	}
	moved, err := s.postOnce(ctx, tx, postSpec{key: prefix + ":collateral", kind: "SPOT_COLLATERAL", intent: &iid, account: a.AccountID,
		request: reqBase, posting: cp, evidence: disclosure, corr: r.CorrelationID})
	if err != nil {
		return SpotResult{}, err
	}
	value, err := cents(after, template, r.Quantity, money.RoundFloor)
	if err != nil {
		return SpotResult{}, err
	}
	if _, err := s.st.insertMove(ctx, tx, a.ID, r.AssetID, MovePost, r.Quantity, value.Minor(), moved.Journal, moved.Effect.ID, &iid, now); err != nil {
		return SpotResult{}, err
	}
	if err := s.moveIntent(ctx, tx, in, econ.StatusCompleted, "bought on margin", ""); err != nil {
		return SpotResult{}, err
	}
	ev, _, err := s.refreshHealth(ctx, tx, &a, "leveraged buy")
	if err != nil {
		return SpotResult{}, err
	}
	return SpotResult{MarginAccountID: a.ID, IntentID: in.ID, LotID: &lotID, Mark: *template.Mark, ExecutionPrice: exec,
		SlippageBPS: s.policy.OpenSlippageBPS, Cost: cost, BorrowJournal: borrowed.Journal, TradeJournal: traded.Journal,
		CollateralJrnl: moved.Journal, ReservationID: res.ID.String(), Status: a.Status, Evaluation: ev}, nil
}
