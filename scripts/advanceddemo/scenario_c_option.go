package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

func init() {
	register("C", "Option: buy a call, the price moves, reference Greeks, expiry, exact cash settlement", scenarioOption)
}

func scenarioOption(ctx context.Context, w *sandboxworld.World, rec *recorder) error {
	usd := func(n int64) money.Quantity { return sandboxworld.Units(n, w.USD.Decimals) }
	cents := func(n int64) money.Quantity { return money.QuantityFromInt64(n * 100) } // whole dollars at price scale 2
	show := func(q money.Quantity) string { return q.ToDecimalString(w.USD.Decimals) }

	btc, err := w.SimAsset(ctx, "SBX-BTC", "Sandbox bitcoin (simulated)", 8, assets.RiskMajor, false)
	if err != nil {
		return err
	}
	var spot, call instruments.InstrumentID
	expiry := w.Clock.Now().Add(48 * time.Hour)
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		s, err := w.Instruments.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: btc.ID, Quote: w.USD.ID, Settlement: w.USD.ID, CanonicalName: "SBX-BTC/SBX-USD",
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: w.Clock.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		spot = s.ID
		strike := cents(65_000)
		terms := instruments.Terms{
			Kind: instruments.TypeOption, Symbol: "SBX-BTC-C-65000", VenueCode: "NODAL-SANDBOX",
			SettlementAssetID: w.USD.ID, UnderlyingInstrumentID: &spot,
			ContractMultiplier: money.QuantityFromInt64(1_000_000), // 0.01 SBX-BTC per contract
			PriceScale:         2, TickSize: money.QuantityFromInt64(50), LotSize: money.QuantityFromInt64(1),
			MinQuantity: money.QuantityFromInt64(1), SettlementStyle: instruments.SettleCash,
			TradingCalendar: derivatives.Calendar24x7, OracleMaxAgeMS: 60_000, OracleSources: []string{"demo-oracle"},
			MarginModel: instruments.MarginOption, CorporateActionBehavior: instruments.CorporateActionsNone,
			Transferability: instruments.NonTransferable,
			Expiry:          &expiry, StrikeMantissa: &strike, OptionRight: instruments.Call, ExerciseStyle: instruments.European,
		}
		ins, _, err := w.Instruments.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
			Terms: terms, CanonicalName: "SBX-BTC call 65,000 (sandbox)", RiskClass: assets.RiskSpeculative,
			Status: assets.StatusActive, ActiveFrom: w.Clock.Now().Add(-time.Hour), ActorType: "OPERATOR", ActorID: "demo-operator",
		})
		call = ins.ID
		return err
	}); err != nil {
		return fmt.Errorf("list the option: %w", err)
	}
	rec.step("an operator lists a European call on sandbox bitcoin: strike 65,000, 0.01 SBX-BTC a contract, cash-settled in SBX-USD, expiring in 48 hours",
		"instrument", call.String(), "underlying", spot.String())

	mark := func(premium, index int64, at time.Time) error {
		return w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			mp := money.Price{Mantissa: cents(premium), Scale: 2, QuoteAsset: w.USD.ID.String(), Source: "demo-oracle", At: at}
			ip := money.Price{Mantissa: cents(index), Scale: 2, QuoteAsset: w.USD.ID.String(), Source: "demo-index", At: at}
			_, err := w.Derivatives.RecordMark(ctx, tx, derivatives.RecordMarkRequest{InstrumentID: call, Mark: mp,
				Source: "demo-oracle", ObservedAt: at, Index: &ip})
			return err
		})
	}
	if err := mark(1_000, 65_000, w.Clock.Now()); err != nil {
		return fmt.Errorf("mark: %w", err)
	}

	holder, err := w.Customer(ctx, "dana-holder")
	if err != nil {
		return err
	}
	writer, err := w.Customer(ctx, "eli-writer")
	if err != nil {
		return err
	}
	for i, c := range []sandboxworld.Customer{holder, writer} {
		if _, err := w.Fund(ctx, c, w.USD.ID, usd(100_000), fmt.Sprintf("demo-c-fund-%d", i)); err != nil {
			return err
		}
	}
	open := func(c sandboxworld.Customer, side derivatives.Side, key string) (derivatives.Outcome, error) {
		var out derivatives.Outcome
		err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = w.Derivatives.OpenPosition(ctx, tx, derivatives.OpenRequest{
				IntentRequest: derivatives.IntentRequest{AccountID: c.Account, InstrumentID: call, Mode: gates.ModePaper,
					IdempotencyKey: key, ActorType: security.ActorUser, ActorID: c.User.String(), Jurisdiction: "US-CA",
					Verification: valuedomain.VerificationNodalIdentity, CorrelationID: "demo-c"},
				Side: side, Quantity: money.QuantityFromInt64(2),
			})
			return err
		})
		return out, err
	}
	buy, err := open(holder, derivatives.Long, "demo-c-buy")
	if err != nil {
		return fmt.Errorf("buy the call: %w", err)
	}
	sell, err := open(writer, derivatives.Short, "demo-c-write")
	if err != nil {
		return fmt.Errorf("write the call: %w", err)
	}
	premiumPaid, err := w.Balance(ctx, ledger.CustomerAccount(holder.Account, ledger.CodeOptionPremiumPaid, w.USD.ID))
	if err != nil {
		return err
	}
	premiumRecv, err := w.Balance(ctx, ledger.CustomerAccount(writer.Account, ledger.CodeOptionPremiumReceived, w.USD.ID))
	if err != nil {
		return err
	}
	rec.step("Dana buys 2 calls and Eli writes 2 at the 1,000 mark plus the disclosed paper slippage; Eli posts the scenario margin",
		"holder_intent", buy.Intent.ID.String(), "writer_intent", sell.Intent.ID.String(),
		"premium_paid", show(premiumPaid)+" SBX-USD", "premium_received", show(premiumRecv)+" SBX-USD")
	rec.check("both positions opened", buy.Position != nil && sell.Position != nil, "holder %v, writer %v", buy.Position != nil, sell.Position != nil)

	// A day later the underlying is up and the premium with it.
	w.Clock.Advance(24 * time.Hour)
	if err := mark(2_200, 67_000, w.Clock.Now()); err != nil {
		return fmt.Errorf("mark: %w", err)
	}
	var an derivatives.Analytics
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		an, err = w.Derivatives.Analytics(ctx, tx, derivatives.AnalyticsRequest{InstrumentID: call, VolBPS: 6_500, RateBPS: 400})
		return err
	}); err != nil {
		return fmt.Errorf("analytics: %w", err)
	}
	implied := an.ImpliedVol
	if implied == "" {
		implied = "not solvable: " + an.ImpliedVolReason
	}
	rec.step("a day later the index is 67,000 and the premium 2,200; reference analytics price the call at a 65% model volatility and solve the implied volatility from the mark (labelled reference values, never a quote, never posted)",
		"label", an.Label, "model", an.Model, "model_vol", an.Vol, "model_price", an.Price, "delta", an.Delta, "gamma", an.Gamma,
		"theta_per_day", an.ThetaPerDay, "vega_per_vol_point", an.VegaPerPoint, "implied_vol_from_mark", implied,
		"intrinsic", an.Intrinsic, "time_value", an.TimeValue)
	rec.check("the analytics are labelled reference values", an.Label != "", "label %q", an.Label)

	// Expiry: the final mark settles the call in the money.
	w.Clock.Set(expiry)
	if err := mark(3_000, 68_000, expiry); err != nil {
		return fmt.Errorf("expiry mark: %w", err)
	}
	var st, again derivatives.SettlementResult
	for _, out := range []*derivatives.SettlementResult{&st, &again} {
		if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			*out, err = w.Derivatives.SettleExpiry(ctx, tx, derivatives.SettleRequest{InstrumentID: call, CorrelationID: "demo-c-expiry"})
			return err
		}); err != nil {
			return fmt.Errorf("settle: %w", err)
		}
	}
	rec.step("at expiry the underlying settles at 68,000: the call is 3,000 in the money per SBX-BTC, 30 a contract, and is exercised automatically",
		"settlement_price", st.Price.String()+" (price scale 2)", "intrinsic_per_contract", show(st.IntrinsicPerContract),
		"holder_received", show(st.Received), "writer_paid", show(st.Paid))
	rec.check("what the holder receives the writer pays", st.Received.Equal(st.Paid) && st.Received.Equal(usd(60)),
		"received %s, paid %s", show(st.Received), show(st.Paid))
	rec.check("an expiry settles exactly once", again.Replay, "second settlement replayed=%t", again.Replay)

	pnlHolder, err := w.Balance(ctx, ledger.CustomerAccount(holder.Account, ledger.CodeRealizedPnL, w.USD.ID))
	if err != nil {
		return err
	}
	pnlWriter, err := w.Balance(ctx, ledger.CustomerAccount(writer.Account, ledger.CodeRealizedPnL, w.USD.ID))
	if err != nil {
		return err
	}
	rec.check("realized PnL is the payoff less the premium, exactly",
		pnlHolder.Equal(usd(60).Sub(premiumPaid)) && pnlWriter.Equal(premiumRecv.Sub(usd(60))),
		"holder %s, writer %s", show(pnlHolder), show(pnlWriter))

	hw, err := w.Wallet(ctx, holder, w.USD.ID)
	if err != nil {
		return err
	}
	ww, err := w.Wallet(ctx, writer, w.USD.ID)
	if err != nil {
		return err
	}
	slippage := premiumPaid.Sub(premiumRecv)
	rec.check("value is conserved: the two wallets plus the clearing house's disclosed slippage equal what was funded",
		hw.Add(ww).Add(slippage).Equal(usd(200_000)),
		"holder %s + writer %s + slippage %s", show(hw), show(ww), show(slippage))
	return nil
}
