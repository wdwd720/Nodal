package nativemarket

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
)

// PriceSource names this venue in asset_prices.source.
//
// It is a real venue name rather than a provider name, and that distinction is
// the whole point of this file: for a Nodal-native market there is no external
// provider, no provider clock and no delivery lag. Nodal observes the trade by
// executing it.
const PriceSource = "nodal-native-market"

// Reality and proof integration for Domain A (STAGE 15; PARTS XLII, XLIII,
// 87–88).
//
// Two things happen inside the same transaction as every trade, and being in
// the same transaction is the substance of both claims:
//
//   - the market's post-trade price is recorded in asset_prices, which is what
//     the prediction ledger's resolver reads. A price that could be written
//     after the trade committed would be a price whose knowledge time we made
//     up, and every "no lookahead" guarantee downstream would rest on it.
//   - the fill is appended to the account's audit stream, which is what
//     internal/proof builds Merkle checkpoints and PART 88 bundles from. An
//     audit row written outside the trade's transaction is an audit row that
//     can disagree with the trade.
//
// # Temporal provenance for a venue that is us
//
// PART XLII asks every relevant event to preserve source_event_at,
// provider_published_at, nodal_received_at and the availability instants. For
// an external feed those are six different numbers. For this market they are
// not, and pretending otherwise would be the lie:
//
//	source_event_at      = the instant the trade committed
//	provider_published_at = none: there is no provider
//	nodal_received_at    = the same instant; Nodal observed it by doing it
//
// So observed_at and received_at are deliberately equal. The alternative —
// inventing a lag so the columns look like a provider feed's — would let a
// backtest believe a price was knowable before it existed.
//
// The availability instants (feature_available_at, decision_available_at) are
// NOT set here. They belong to whoever consumes the datum and are computed by
// reality.AvailabilityPolicy from its own latencies: a decision process still
// needs time to react even to our own data, and how much is a property of that
// process, not of this trade.

// publishPrice records the market's post-trade price.
//
// The published number is SpotAfter, the marginal price the next trader faces,
// not EffectivePrice, which depends on the size of THIS trade. A price series
// built from effective prices would move with order size rather than with the
// market, and calibration would be scoring the wrong thing.
//
// raw_ref carries the fill id, so every price can be traced back to the exact
// trade that set it, and a price with no fill behind it is visible as such.
// It returns the instant it stamped, because the trade's public print carries
// the same one: they are the same observation, and two instants for it would
// let a chart and a price series disagree about when the market moved.
func (s *Service) publishPrice(ctx context.Context, tx pgx.Tx, m Market, spot money.Quantity, at time.Time, ref string) (time.Time, error) {
	at, err := s.priceInstant(ctx, tx, m, at)
	if err != nil {
		return time.Time{}, err
	}
	_, err = s.prices.RecordPrice(ctx, tx, valuation.PriceObservation{
		AssetID:      m.AssetID,
		QuoteAssetID: m.CreditAssetID,
		Mantissa:     spot,
		Scale:        PriceScale,
		Source:       PriceSource,
		ObservedAt:   at,
		ReceivedAt:   at,
		RawRef:       ref,
	})
	if err != nil {
		return time.Time{}, errs.Wrap(err, errs.CodeOf(err),
			"nativemarket: the market's new price could not be recorded")
	}
	return at, nil
}

// priceInstant returns the instant to stamp this market's next price with.
//
// asset_prices is append-only — cp_app holds no UPDATE grant on it, which is
// deliberate: a price that could be rewritten is a history that can be
// rewritten. Its observation identity is (asset, quote, source, observed_at),
// so two prices from this market cannot share an instant.
//
// They can try to. Trades on one market serialise behind the market's row
// lock, but the platform clock is not guaranteed to tick between two of them,
// and a fake clock in a test does not tick at all. So the stamp is nudged to
// the smallest instant strictly after the last one this market recorded.
//
// The step is one MICROsecond because that is timestamptz's resolution: a
// nanosecond nudge is stored as the same instant and collides again.
//
// The nudge can only move a price LATER, never earlier, and only by
// microseconds. That direction is the safe one: PART XLII forbids a datum
// appearing to have been available before it existed, and a price stamped a
// microsecond late can only make a backtest more conservative, never let it
// see the future.
func (s *Service) priceInstant(ctx context.Context, tx pgx.Tx, m Market, at time.Time) (time.Time, error) {
	at = at.UTC()
	var last time.Time
	// received_at, not max(observed_at): this venue writes them equal, and the
	// (asset, quote, received_at DESC) index makes this a single row read
	// rather than a scan of every price the market has ever had.
	err := tx.QueryRow(ctx,
		`SELECT observed_at FROM asset_prices
		  WHERE asset_id = $1 AND quote_asset_id = $2 AND source = $3
		  ORDER BY received_at DESC LIMIT 1`,
		m.AssetID, m.CreditAssetID, PriceSource).Scan(&last)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return at, nil
	case err != nil:
		return time.Time{}, errs.Wrap(err, errs.CodeInternal, "nativemarket: read the market's last price instant")
	}
	if next := last.UTC().Add(time.Microsecond); next.After(at) {
		return next, nil
	}
	return at, nil
}

// recordFill appends the fill to the trader's audit stream.
//
// The stream is the trader's, not the market's, because that is the stream a
// customer's proof bundle is built from: PART 88 asks what happened to one
// person's money, and a market-wide stream would answer a different question
// and leak other people's trades into the bundle.
func (s *Service) recordFill(ctx context.Context, tx pgx.Tx, m Market, r ExecuteRequest, fill Fill, fillID FillID, journalTx string, at time.Time) error {
	actorType, actorID := actorFrom(ctx)
	payload, err := json.Marshal(map[string]any{
		"market_id":                 m.ID.String(),
		"asset_id":                  m.AssetID.String(),
		"credit_asset_id":           m.CreditAssetID.String(),
		"side":                      string(r.Side),
		"credits_in":                fill.CreditsIn.String(),
		"credits_out":               fill.CreditsOut.String(),
		"assets_in":                 fill.AssetsIn.String(),
		"assets_out":                fill.AssetsOut.String(),
		"platform_fee":              fill.PlatformFee.String(),
		"creator_fee":               fill.CreatorFee.String(),
		"spot_before":               fill.SpotBefore.String(),
		"spot_after":                fill.SpotAfter.String(),
		"effective_price":           fill.EffectivePrice.String(),
		"price_scale":               PriceScale,
		"state_version_before":      fill.StateBefore.Version,
		"real_credit_reserve_after": fill.StateAfter.RealCreditReserve.String(),
		"asset_reserve_after":       fill.StateAfter.AssetReserve.String(),
		"journal_transaction_id":    journalTx,
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "nativemarket: encode fill audit payload")
	}
	_, err = s.auditor.Append(ctx, tx, audit.Event{
		Stream:       audit.AccountStream(r.AccountID.String()),
		ActorType:    actorType,
		ActorID:      actorID,
		Action:       "native_market.fill",
		ResourceType: "native_market_fill",
		ResourceID:   fillID.String(),
		Reason:       "native market trade executed",
		Payload:      payload,
		OccurredAt:   at.UTC(),
	})
	return err
}

// registerInstrument gives the market a row in the platform's instrument
// registry, as a SPOT_PAIR of (native asset / Credit).
//
// This is what lets anything outside this package NAME a Domain A market.
// Predictions are keyed by instrument id; without this row the prediction
// ledger could not reference a native market at all, and PARTS XLII–XLIII
// would apply to Domain B and C only.
//
// # It opens HALTED
//
// A market is created PENDING and becomes tradable later, so ACTIVE would be
// false at this moment. Every later change of the market's status is mirrored
// onto the instrument by mirrorInstrumentStatus, so the registry and the venue
// do not drift apart -- and PENDING maps to HALTED for the same reason ACTIVE
// would have been wrong: nothing may trade yet.
func (s *Service) registerInstrument(ctx context.Context, tx pgx.Tx, m Market, r CreateRequest) error {
	var (
		symbol    string
		creditSym string
		assetRisk assets.RiskClass
	)
	if err := tx.QueryRow(ctx,
		`SELECT a.symbol, a.risk_class, c.symbol
		   FROM assets a JOIN assets c ON c.id = $2
		  WHERE a.id = $1`, m.AssetID, m.CreditAssetID).Scan(&symbol, &assetRisk, &creditSym); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "nativemarket: read the market's assets for its instrument")
	}
	_, err := s.instruments.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
		Base: m.AssetID, Quote: m.CreditAssetID, Settlement: m.CreditAssetID,
		CanonicalName: symbol + "/" + creditSym,
		// The asset's own risk class, read rather than assumed: a native asset
		// is speculative, but which speculative class it is was decided when
		// the asset was created and is not this package's to restate.
		RiskClass:  assetRisk,
		Status:     assets.StatusHalted,
		ActiveFrom: r.EffectiveAt.UTC(),
		PolicyRef:  "native-market:" + m.ID.String(),
	})
	if err != nil && errs.CodeOf(err) == errs.CodeConflict {
		// The asset already has an instrument. A market is unique per asset,
		// so this is a replay of the same creation, not a second market.
		return nil
	}
	return err
}

// instrumentStatusFor maps a market's status onto the registry's vocabulary.
//
// The two tables are not the same and the differences are the interesting part:
//
//   - PENDING has no registry equivalent, because "created but not trading" is
//     what HALTED means there.
//   - FROZEN maps to HALTED too. The registry has no word for "holders cannot
//     exit either", and inventing one would be claiming the registry knows
//     something it does not. Both say the same operative thing: nothing may
//     trade.
//   - DELISTED is terminal on both sides.
//
// Where the market's vocabulary is finer than the registry's, the mapping
// loses detail in the direction of caution: every market state that is not
// fully open maps to a registry state that permits no new exposure.
func instrumentStatusFor(s Status) (assets.Status, bool) {
	switch s {
	case StatusActive:
		return assets.StatusActive, true
	case StatusCloseOnly:
		return assets.StatusCloseOnly, true
	case StatusPending, StatusHalted, StatusFrozen:
		return assets.StatusHalted, true
	case StatusDelisted:
		return assets.StatusDelisted, true
	}
	return "", false
}

// mirrorInstrumentStatus moves the market's instrument to match the market.
//
// The registry is the platform's answer to "what may be traded", and a venue
// that halted its own market while the registry still said ACTIVE would be two
// sources disagreeing about the same fact. This package already refuses that
// shape for capabilities -- "a second source would eventually disagree with the
// first, and the disagreement would be discovered by something moving that
// should not have" -- and the registry deserves the same treatment.
//
// A market with no instrument is not an error. Markets created before the
// registry row existed are real, and refusing to halt one because its
// bookkeeping is incomplete would make the registry a reason not to stop a
// market. Stopping must never be the harder path.
func (s *Service) mirrorInstrumentStatus(ctx context.Context, tx pgx.Tx, m Market, to Status, reason string) error {
	want, ok := instrumentStatusFor(to)
	if !ok {
		return nil
	}
	ins, err := s.instruments.GetBySpotPair(ctx, tx, m.AssetID, m.CreditAssetID)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return nil
		}
		return err
	}
	if ins.Status == want {
		return nil
	}
	if !assets.CanTransition(ins.Status, want) {
		// The registry's own table refuses this step. That is information, not
		// an obstacle to route around: the market has already moved, and the
		// registry says its path there was not one it recognises. Reported, so
		// somebody can reconcile the two rather than discovering the drift
		// later.
		return errs.Newf(errs.CodeInvalidStateTransition,
			"the market moved to %s but its instrument cannot go %s -> %s", to, ins.Status, want).
			WithField("market_id", m.ID.String()).
			WithField("instrument_id", ins.ID.String())
	}
	actorType, actorID := actorFrom(ctx)
	_, err = s.instruments.TransitionStatus(ctx, tx, ins.ID, instruments.StatusChange{
		To: want, ActorType: actorType, ActorID: actorID,
		Reason: "native market " + string(to) + ": " + reason,
	})
	return err
}
