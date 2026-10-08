package sandboxworld

import (
	"context"
	"fmt"

	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/httpapi"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

// wireEquities builds paper equities over the world exactly as cmd/api builds
// them (the "advanced:equities services" block of cmd/api/wire.go): the
// deterministic paper venue equities.NewExecutionAdapter gives this
// environment with the fake execution provider, on the XNYS calendar with the
// closures recorded in the database when the world opens, the sandbox
// schedule in the sandbox dollar, the advanced capabilities' one identity
// rule, and the world's fee ledger, through which every fill's spread and
// fees are disclosed and booked in the transaction that applies the fill.
func (w *World) wireEquities(ctx context.Context) error {
	holidays, err := equities.Holidays(ctx, w.DB, equities.CalendarXNYS)
	if err != nil {
		return fmt.Errorf("sandboxworld: equities holidays: %w", err)
	}
	cal, err := equities.NewCalendar(equities.CalendarXNYS, holidays)
	if err != nil {
		return fmt.Errorf("sandboxworld: equities calendar: %w", err)
	}
	venue, err := equities.NewExecutionAdapter(w.Env, config.ProviderModeFake, equities.PaperConfig{
		Name: "nodal-paper-equities", Calendar: cal, SlippageBPS: 10,
	})
	if err != nil {
		return fmt.Errorf("sandboxworld: equities execution adapter: %w", err)
	}
	w.Equities, err = equities.NewService(equities.Config{
		Clock: w.Clock, Intents: econ.NewIntentStore(w.Clock, w.Outbox, w.Audit),
		Effects: econ.NewEffectStore(w.Clock, w.Outbox, w.Audit), Authorizer: w.Authorizer, KillSwitches: w.Kills,
		Router: w.Router, RiskPolicy: risk.DefaultAdvancedPolicy(), Capital: w.Capital, Ledger: w.Ledger, Prices: w.Prices,
		Audit: w.Audit, Adapter: venue,
		Schedules: []fees.Schedule{{
			Version: "equities-sandbox-v1", QuoteAsset: econ.SandboxDollarID(), QuoteDecimals: econ.SandboxDollarDecimals,
			SpreadBPS: 10, PercentageFeeBPS: 10, StrategyFeeBPS: 5, SlippageReserveBPS: 50, Rounding: fees.DefaultRounding(),
		}},
		FeeLedger:       w.FeeLedger,
		MarketCollarBPS: 100, MinOrderNotional: money.USDFromMinor(1_00),
		Verification: httpapi.AdvancedVerificationLevels{Rule: compliance.NewAccountVerification(w.Clock)},
	})
	if err != nil {
		return fmt.Errorf("sandboxworld: equities: %w", err)
	}
	return nil
}
