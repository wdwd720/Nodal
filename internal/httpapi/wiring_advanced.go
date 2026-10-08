package httpapi

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuation"
)

// AdvancedDeps holds the services behind the advanced financial capabilities
// (ADR-0020). Authorizer, Intents, Effects and Risk are required together:
// leaving any of them nil leaves every advanced route UNSUPPORTED rather than
// half-answering.
type AdvancedDeps struct {
	Authorizer *econ.Authorizer
	Intents    *econ.IntentStore
	Effects    *econ.EffectStore
	Risk       *risk.Store
	// Prices marks holdings for buying power V2. Nil leaves buying power V2
	// UNSUPPORTED: a figure with no marks behind it would be invented.
	Prices valuation.PriceSource
	// MaxPriceAge bounds how old a mark buying power V2 may use.
	MaxPriceAge time.Duration
	// Margin reports margin state to buying power V2. Nil treats every owner
	// as a cash account.
	Margin buyingpower.MarginReader
	// Extra reports reservations the capital tables cannot classify.
	Extra buyingpower.ExtraReservationReader

	// >>> advanced:margin deps
	// MarginService runs margin accounts (internal/margin). Nil leaves the
	// margin routes UNSUPPORTED. It needs the ledger of WireDeps as well, to
	// report the facilities' balances after a seeding.
	MarginService *MarginService

	// >>> advanced:derivatives deps
	// Derivatives serves /advanced/derivatives (see DerivativesDeps); a nil
	// service leaves those routes UNSUPPORTED.
	Derivatives DerivativesDeps

	// >>> advanced:equities deps

	// >>> advanced:orders deps

	// >>> advanced:transfers deps
	// Transfers holds the transfer service and the sandbox faucet; each that is
	// nil leaves only its own routes UNSUPPORTED.
	Transfers TransfersDeps

	// >>> advanced:treasury deps

	// >>> advanced:stable deps

	// >>> advanced:bridge deps

	// >>> advanced:predmarket deps
	// PredictionMarkets runs prediction markets. Nil leaves their routes
	// UNSUPPORTED.
	PredictionMarkets *PredictionMarketService
	// PredictionOracle is the sandbox oracle the service resolves from, the
	// same instance its configuration holds. Nil (every deployment outside
	// LOCAL, TEST and DEV) answers PROVIDER_UNAVAILABLE wherever an oracle is
	// needed.
	PredictionOracle *PredictionSandboxOracle

	// >>> advanced:marketplace deps
	// Marketplace holds the strategy marketplace and the sandbox runtime
	// behind it; leaving either nil leaves only the /advanced/strategies
	// routes UNSUPPORTED.
	Marketplace MarketplaceDeps

	// >>> advanced:delegation deps

	// >>> advanced:operator deps
	// Operator serves the operator console (/advanced/ops): the overview, the
	// sandbox reference marks and the simulated chains. Incomplete leaves those
	// routes UNSUPPORTED.
	Operator OperatorDeps
}

func (d AdvancedDeps) complete() bool {
	return d.Authorizer != nil && d.Intents != nil && d.Effects != nil && d.Risk != nil
}

// wireAdvanced wires each advanced port whose services are present. Each
// subsystem decides for itself what "complete" means; a missing service
// leaves only that subsystem's routes UNSUPPORTED.
func wireAdvanced(p *Ports, d WireDeps) {
	if d.Advanced.complete() && d.Instruments != nil {
		p.Advanced = advancedCoreAdapter{deps: d.Advanced, instruments: d.Instruments, db: d.DB, clk: d.Clock}
	}

	// >>> advanced:margin wire
	if d.Advanced.MarginService != nil && d.Ledger != nil {
		p.Margin = marginAdapter{svc: d.Advanced.MarginService, ledger: d.Ledger, db: d.DB}
	}

	// >>> advanced:derivatives wire
	wireDerivatives(p, d)

	// >>> advanced:equities wire

	// >>> advanced:orders wire

	// >>> advanced:transfers wire
	wireTransfers(p, d)

	// >>> advanced:treasury wire

	// >>> advanced:stable wire

	// >>> advanced:bridge wire

	// >>> advanced:predmarket wire
	if d.Advanced.PredictionMarkets != nil {
		p.PredictionMarkets = newPredmarketAdapter(d)
	}

	// >>> advanced:marketplace wire
	wireMarketplace(p, d)

	// >>> advanced:delegation wire

	// >>> advanced:operator wire
	wireOperator(p, d)
}

type advancedCoreAdapter struct {
	deps        AdvancedDeps
	instruments *instruments.Repository
	db          *db.DB
	clk         clock.Clock
}

// snapshot runs a multi-statement read against one consistent snapshot, so a
// response never mixes two versions of the same state.
func (a advancedCoreAdapter) snapshot(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

func (a advancedCoreAdapter) Capabilities(ctx context.Context) (string, []econ.CapabilityState, error) {
	var env string
	var out []econ.CapabilityState
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var derr error
		env, out, derr = a.deps.Authorizer.Describe(ctx, tx)
		return derr
	})
	return env, out, err
}

func (a advancedCoreAdapter) Instruments(ctx context.Context, kind instruments.InstrumentType, limit int) ([]AdvancedInstrumentView, error) {
	var listed []instruments.Listed
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var lerr error
		listed, lerr = a.instruments.ListWithTerms(ctx, tx, kind, limit)
		return lerr
	})
	if err != nil {
		return nil, err
	}
	out := make([]AdvancedInstrumentView, 0, len(listed))
	for _, l := range listed {
		out = append(out, AdvancedInstrumentView{Instrument: l.Instrument, Current: l.Current})
	}
	return out, nil
}

func (a advancedCoreAdapter) Instrument(ctx context.Context, iid instruments.InstrumentID) (AdvancedInstrumentView, error) {
	var v AdvancedInstrumentView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if v.Instrument, err = a.instruments.Get(ctx, tx, iid); err != nil {
			return err
		}
		if v.Current, err = a.instruments.CurrentTerms(ctx, tx, iid); err != nil {
			return err
		}
		v.History, err = a.instruments.TermsHistory(ctx, tx, iid)
		return err
	})
	return v, err
}

func (a advancedCoreAdapter) Intents(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]econ.Intent, string, error) {
	return a.deps.Intents.ListForAccount(ctx, a.db, account, cursor, limit)
}

func (a advancedCoreAdapter) Intent(ctx context.Context, iid econ.IntentID) (econ.Intent, error) {
	return a.deps.Intents.Get(ctx, a.db, iid)
}

func (a advancedCoreAdapter) Effects(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]econ.Effect, string, error) {
	return a.deps.Effects.ListForAccount(ctx, a.db, account.String(), cursor, limit)
}

func (a advancedCoreAdapter) RiskDecisions(ctx context.Context, account accounts.AccountID, limit int) ([]risk.AdvancedDecisionRecord, error) {
	return a.deps.Risk.ListAdvancedDecisions(ctx, a.db, account.String(), limit)
}

func (a advancedCoreAdapter) BuyingPower(ctx context.Context, account accounts.AccountID) (AdvancedBuyingPower, error) {
	if a.deps.Prices == nil {
		return AdvancedBuyingPower{}, errNotWired("advanced buying power (no price source)")
	}
	var out AdvancedBuyingPower
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		usd, err := econ.SandboxDollar(ctx, tx)
		if errs.HasCode(err, errs.CodeNotFound) {
			// Not "no such account": this deployment has no simulated value
			// at all until the sandbox dollar exists.
			return errs.New(errs.CodeUnsupported, "the sandbox dollar is not provisioned in this deployment, so there is no simulated value to report")
		}
		if err != nil {
			return err
		}
		out.Quote = usd
		loader, err := buyingpower.NewV2Loader(buyingpower.V2LoaderConfig{
			Clock: a.clk, Prices: a.deps.Prices, QuoteAssetID: usd.ID, MaxPriceAge: a.deps.MaxPriceAge,
			Margin: a.deps.Margin, Extra: a.deps.Extra,
		})
		if err != nil {
			return err
		}
		out.V2, err = loader.Compute(ctx, tx, string(ledger.OwnerCustomer), account.String())
		return err
	})
	return out, err
}
