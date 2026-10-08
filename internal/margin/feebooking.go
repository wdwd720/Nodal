package margin

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// The liquidation fee is the one fee margin charges, and it is booked
// through the fee ledger (internal/feeledger) in the liquidation step's own
// transaction, once:
//
//   - The schedule. The policy's fee is the percentage fee of a fee-ledger
//     schedule quoted in the account's numeraire (LiquidationFeeSchedule),
//     published under a key that names the numeraire and the rate
//     (LiquidationFeeScheduleKey) and ensured in force before any fee is
//     disclosed: a liquidation fee is never charged under another version.
//   - The disclosure. The fee is a share of the sale's proceeds, rounded up
//     (PlanLiquidation: ceil(proceeds x fee bps)); the disclosure prices
//     exactly that -- the proceeds, at par in the numeraire, with the
//     percentage fee rounded up, a platform-favourable mode the schedule
//     states and every disclosure repeats -- and must charge exactly the fee
//     the plan computed, or the step is refused.
//   - The booking. The fee is posted inside the step's LIQUIDATION journal
//     (the explicit-fee pair) and the disclosure registered against that
//     journal and the effect that applied it (feeledger.RecordEmbedded).
//
// A liquidation reduces risk and a kill switch never stops it, so neither
// does a SPREAD_ENGINE kill switch or gate stop its fee from being booked
// (feeledger RiskReducing). A step that charges no fee books nothing. The
// database refuses a step whose fee has no booking (00761, FW001).

// LiquidationFeeScheduleKey is the fee-ledger key of the liquidation fee
// schedule of a rate in a numeraire, "margin:liquidation:<numeraire id>:<bps>bps".
// The key names the rate, so a policy that changes the rate publishes under
// a new key and one that changes it back finds its own version still in
// force.
func LiquidationFeeScheduleKey(numeraire assets.AssetID, feeBPS money.BPS) string {
	return fmt.Sprintf("margin:liquidation:%s:%dbps", numeraire, int64(feeBPS))
}

// LiquidationFeeSchedule is the fee-ledger schedule that discloses a policy's
// liquidation fee in a numeraire: the fee rate as the percentage fee, rounded
// up exactly as PlanLiquidation rounds it (RoundCeil, which can favour the
// platform, so the schedule adopts that explicitly and every disclosure says
// so), and nothing else.
func LiquidationFeeSchedule(p Policy, numeraire assets.AssetID, decimals uint8) fees.Schedule {
	r := fees.DefaultRounding()
	r.PercentageFee, r.AllowPlatformFavourable = money.RoundCeil, true
	return fees.Schedule{
		Version:          fmt.Sprintf("margin-liquidation-fee-%dbps", int64(p.LiquidationFeeBPS)),
		QuoteAsset:       numeraire,
		QuoteDecimals:    decimals,
		PercentageFeeBPS: p.LiquidationFeeBPS,
		Rounding:         r,
	}
}

// liquidationFeeInput is what the fee is a share of: the sale's proceeds in
// the numeraire, at par (one numeraire unit per unit), so the disclosed
// reference notional is exactly the proceeds the step credited.
func liquidationFeeInput(numeraire assets.AssetID, decimals uint8, proceeds money.Quantity, at time.Time) fees.QuoteInput {
	return fees.QuoteInput{
		Side: fees.SideSell, BaseQty: proceeds, BaseDecimals: decimals, QuoteDecimals: decimals,
		ReferencePrice: money.Price{Mantissa: money.QuantityFromInt64(1), QuoteAsset: numeraire.String(),
			Source: "margin-liquidation-proceeds", At: at},
	}
}

// discloseLiquidationFee ensures the policy's fee schedule is the version in
// force for the account's numeraire and records the disclosure of the step's
// fee for the customer, which must be exactly the fee the plan computed.
func (s *Service) discloseLiquidationFee(ctx context.Context, tx pgx.Tx, a Account, decimals uint8, key string, fee,
	proceeds money.Quantity, at time.Time) (feeledger.StoredQuote, error) {
	scheduleKey := LiquidationFeeScheduleKey(a.NumeraireAssetID, s.policy.LiquidationFeeBPS)
	pub, err := s.d.FeeLedger.EnsureSchedule(ctx, tx, feeledger.PublishRequest{
		Key: scheduleKey, Schedule: LiquidationFeeSchedule(s.policy, a.NumeraireAssetID, decimals), Actor: s.actor,
		Reason: "the liquidation fee of margin policy " + s.policy.Version,
	})
	if err != nil {
		return feeledger.StoredQuote{}, err
	}
	payer, numeraire := a.AccountID, a.NumeraireAssetID
	q, err := s.d.FeeLedger.RecordQuote(ctx, tx, feeledger.QuoteRequest{
		ScheduleKey: scheduleKey, At: feeledger.QuoteAt(pub, s.d.Clock.Now().UTC()),
		Input: liquidationFeeInput(numeraire, decimals, proceeds, at), Subsystem: econ.SubsystemMargin,
		IdempotencyKey: key, AccountID: &payer, BaseAsset: &numeraire, Mode: a.Mode, Actor: s.actor, RiskReducing: true,
	})
	if err != nil {
		return feeledger.StoredQuote{}, err
	}
	c := q.Charges
	if !c.Platform.Equal(fee) || !c.Strategy.IsZero() || !c.Spread.IsZero() || !c.Venue.IsZero() || !c.Network.IsZero() {
		return feeledger.StoredQuote{}, errs.New(errs.CodeInternal, "margin: the fee ledger's disclosure is not the liquidation fee the plan charges").
			WithField("disclosure_id", q.ID.String()).WithField("fee", fee.String())
	}
	return q, nil
}

// bookLiquidationFee registers a disclosed liquidation fee against the
// LIQUIDATION journal that charged it and the effect that applied it.
func (s *Service) bookLiquidationFee(ctx context.Context, tx pgx.Tx, a Account, q feeledger.StoredQuote, journal ledger.TransactionID,
	effect econ.EffectID) error {
	_, err := s.d.FeeLedger.RecordEmbedded(ctx, tx, feeledger.EmbeddedRequest{
		DisclosureID: q.ID, AccountID: a.AccountID, JournalTransactionID: journal, EffectID: effect, Mode: a.Mode, Actor: s.actor,
		RiskReducing: true,
	})
	return err
}
