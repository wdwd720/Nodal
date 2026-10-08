package derivatives

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuation"
)

var executionAt = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func reasonOf(t *testing.T, err error) any {
	t.Helper()
	e, ok := errs.As(err)
	require.True(t, ok, "%v", err)
	return e.Fields["reason"]
}

// TestNewExecutionAdapter_RefusesLiveEverywhereAndFakeWhereItIsNotAllowed is
// the constructor's whole table, every environment against every mode: live
// is refused in every environment (it does not exist), fake is refused where
// the environment refuses fakes and, where it allows them, because no fake
// venue exists; sandbox is the paper venue everywhere.
func TestNewExecutionAdapter_RefusesLiveEverywhereAndFakeWhereItIsNotAllowed(t *testing.T) {
	cfg := PaperVenueConfig{Name: "paper-test"}
	for _, env := range append(config.Environments(), config.Environment("NOWHERE")) {
		for _, mode := range []config.ProviderMode{config.ProviderModeFake, config.ProviderModeSandbox, config.ProviderModeLive, "carrier-pigeon"} {
			t.Run(string(env)+"/"+string(mode), func(t *testing.T) {
				a, err := NewExecutionAdapter(env, mode, cfg)
				switch {
				case mode == config.ProviderModeLive:
					assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err), "live, even where the environment is unknown")
					assert.Equal(t, ReasonLiveExecutionUnsupported, reasonOf(t, err))
				case !mode.IsValid(), !env.IsValid():
					assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
				case mode == config.ProviderModeFake && !env.AllowsFakeProviders():
					assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
					assert.Equal(t, ReasonProviderModeNotAllowed, reasonOf(t, err))
				case mode == config.ProviderModeFake:
					assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
					assert.Equal(t, ReasonProviderModeUnimplemented, reasonOf(t, err))
				default:
					require.NoError(t, err)
					assert.Equal(t, config.ProviderModeSandbox, a.Mode())
					assert.Equal(t, "paper-test", a.Name())
					assert.IsType(t, &PaperVenue{}, a)
				}
				if err != nil {
					assert.Nil(t, a, "a refusal builds nothing")
				}
			})
		}
		// What a binary asks for, where nothing configures a mode, is built.
		if env.IsValid() {
			_, err := NewExecutionAdapter(env, ExecutionModeFor(env), cfg)
			assert.NoError(t, err, "%s: the derived mode is one this environment runs", env)
		}
	}
	_, err := NewExecutionAdapter(config.EnvLocal, config.ProviderModeSandbox, PaperVenueConfig{Name: " padded "})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// genExecution draws a request on one of the three contract kinds.
func genExecution(t *rapid.T) ExecutionRequest {
	var s Spec
	switch rapid.IntRange(0, 2).Draw(t, "kind") {
	case 0:
		s = perpSpec(t)
	case 1:
		s = futureSpec(t)
	default:
		s = optionSpec(t, instruments.Put, 6_500_000)
	}
	side := Buy
	if rapid.Bool().Draw(t, "sell") {
		side = Sell
	}
	return ExecutionRequest{
		Spec: s, Side: side, Quantity: q(rapid.Int64Range(1, 10_000).Draw(t, "qty")),
		Mark: Mark{ID: newMarkID(), InstrumentID: s.InstrumentID, MarkScale: s.PriceScale,
			MarkMantissa: q(rapid.Int64Range(0, 2_000_000).Draw(t, "mark_ticks")).Mul(s.Tick)},
		MaxSlippageBPS: money.BPS(rapid.Int64Range(0, 9_999).Draw(t, "slip")),
		At:             executionAt,
	}
}

// TestContract_PaperVenueKeepsTheExecutionContract: for every contract kind,
// side, size, mark and disclosed slippage, the paper venue's answer keeps
// every promise of DerivativesExecutionAdapter -- a trade source, the
// disclosed slippage and no more, a price on the tick grid that is never
// better than the mark for the customer and never worse than the mark moved
// by the slippage -- and the same request always gets the same answer.
func TestContract_PaperVenueKeepsTheExecutionContract(t *testing.T) {
	venue, err := NewExecutionAdapter(config.EnvTest, config.ProviderModeSandbox, PaperVenueConfig{Name: "paper-contract"})
	require.NoError(t, err)
	rapid.Check(t, func(t *rapid.T) {
		req := genExecution(t)
		ex, err := venue.Execute(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if err := ex.Check(req); err != nil {
			t.Fatalf("the paper venue broke its own contract: %v", err)
		}
		if ex.Source != SourcePaper || ex.SlippageBPS != req.MaxSlippageBPS || ex.Venue != "paper-contract" {
			t.Fatalf("a paper fill at exactly the disclosed slippage, on its own venue: %+v", ex)
		}
		again, err := venue.Execute(context.Background(), req)
		if err != nil || again.Venue != ex.Venue || again.Source != ex.Source || !again.Price.Equal(ex.Price) ||
			again.SlippageBPS != ex.SlippageBPS {
			t.Fatalf("the same request answered %+v then %+v (%v)", ex, again, err)
		}
		// The price is the paper price the package always charged.
		want, err := paperPrice(req.Spec, req.Mark.MarkMantissa, req.Side, req.MaxSlippageBPS)
		if err != nil || !want.Equal(ex.Price) {
			t.Fatalf("price %s, the paper price is %s (%v)", ex.Price, want, err)
		}
	})
}

// TestPaperVenue_RefusesAMalformedRequest: an order the venue cannot price
// honestly is a VALIDATION_FAILED refusal, never an execution.
func TestPaperVenue_RefusesAMalformedRequest(t *testing.T) {
	venue, err := NewExecutionAdapter(config.EnvTest, config.ProviderModeSandbox, PaperVenueConfig{Name: "paper-refusals"})
	require.NoError(t, err)
	s := perpSpec(t)
	good := ExecutionRequest{Spec: s, Side: Buy, Quantity: q(3),
		Mark: Mark{ID: newMarkID(), MarkScale: s.PriceScale, MarkMantissa: q(6_500_000)}, MaxSlippageBPS: 10, At: executionAt}
	_, err = venue.Execute(context.Background(), good)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*ExecutionRequest){
		"no side":              func(r *ExecutionRequest) { r.Side = "HOLD" },
		"no contracts":         func(r *ExecutionRequest) { r.Quantity = q(0) },
		"negative contracts":   func(r *ExecutionRequest) { r.Quantity = q(-1) },
		"an unrecorded mark":   func(r *ExecutionRequest) { r.Mark.ID = MarkID{} },
		"a mark off the grid":  func(r *ExecutionRequest) { r.Mark.MarkMantissa = q(6_500_001) },
		"a mark at old terms":  func(r *ExecutionRequest) { r.Mark.MarkScale = s.PriceScale + 1 },
		"a negative mark":      func(r *ExecutionRequest) { r.Mark.MarkMantissa = q(-50) },
		"slippage of it all":   func(r *ExecutionRequest) { r.MaxSlippageBPS = money.OneHundredPercent },
		"negative slippage":    func(r *ExecutionRequest) { r.MaxSlippageBPS = -1 },
		"no time":              func(r *ExecutionRequest) { r.At = time.Time{} },
		"terms without a tick": func(r *ExecutionRequest) { r.Spec.Tick = q(0) },
	} {
		req := good
		mutate(&req)
		ex, err := venue.Execute(context.Background(), req)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
		assert.Equal(t, Execution{}, ex, name)
	}
}

// TestExecutionCheck_RefusesEveryBrokenPromise: Check -- which the service
// runs on every answer before anything is priced at it -- refuses each way
// an adapter could break the contract.
func TestExecutionCheck_RefusesEveryBrokenPromise(t *testing.T) {
	s := perpSpec(t)
	mark := q(6_500_000) // $65 000.00, on the 0.50 grid
	req := func(side FillSide) ExecutionRequest {
		return ExecutionRequest{Spec: s, Side: side, Quantity: q(1),
			Mark: Mark{ID: newMarkID(), MarkScale: s.PriceScale, MarkMantissa: mark}, MaxSlippageBPS: 10, At: executionAt}
	}
	// 10 bps of $65 000.00 is $65.00: a buy fills at $65 065.00 at most, a
	// sell at $64 935.00 at least.
	ok := map[FillSide]Execution{
		Buy:  {Venue: "v", Source: SourcePaper, Price: q(6_506_500), SlippageBPS: 10},
		Sell: {Venue: "v", Source: SourcePaper, Price: q(6_493_500), SlippageBPS: 10},
	}
	for side, ex := range ok {
		require.NoError(t, ex.Check(req(side)), side)
		atMark := ex
		atMark.Price = mark
		require.NoError(t, atMark.Check(req(side)), "%s at the mark itself is within any slippage", side)
		book := ex
		book.Source = SourceBook
		require.NoError(t, book.Check(req(side)), "a book fill is a trade too")
	}
	broken := map[string]struct {
		side FillSide
		ex   Execution
	}{
		"not a trade":                {Buy, Execution{Source: SourceExercise, Price: mark, SlippageBPS: 0}},
		"more slippage than told":    {Buy, Execution{Source: SourcePaper, Price: mark, SlippageBPS: 11}},
		"negative slippage":          {Sell, Execution{Source: SourcePaper, Price: mark, SlippageBPS: -1}},
		"off the tick grid":          {Buy, Execution{Source: SourcePaper, Price: q(6_500_001), SlippageBPS: 10}},
		"a buy below the mark":       {Buy, Execution{Source: SourcePaper, Price: q(6_499_950), SlippageBPS: 10}},
		"a buy beyond its slippage":  {Buy, Execution{Source: SourcePaper, Price: q(6_506_550), SlippageBPS: 10}},
		"a sell above the mark":      {Sell, Execution{Source: SourcePaper, Price: q(6_500_050), SlippageBPS: 10}},
		"a sell beyond its slippage": {Sell, Execution{Source: SourcePaper, Price: q(6_493_450), SlippageBPS: 10}},
		"slippage it did not take":   {Buy, Execution{Source: SourcePaper, Price: q(6_506_500), SlippageBPS: 5}},
	}
	for name, c := range broken {
		err := c.ex.Check(req(c.side))
		assert.Equal(t, errs.CodeInternal, errs.CodeOf(err), name)
	}
}

// liveAdapter claims a mode no adapter may have.
type liveAdapter struct{ *PaperVenue }

func (liveAdapter) Mode() config.ProviderMode { return config.ProviderModeLive }

func unitConfig(t *testing.T) Config {
	t.Helper()
	clk := clock.NewFake(executionAt)
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("unit")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	return Config{
		Clock: clk, Intents: econ.NewIntentStore(clk, out, aw), Effects: econ.NewEffectStore(clk, out, aw),
		Authorizer: econ.NewAuthorizer(checker, kills), Registry: econ.NewRegistry(), Ledger: ledger.NewService(clk, "unit"),
		Capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk}), RiskStore: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: DefaultPolicy(), Prices: valuation.NewPriceStore(clk),
		Instruments: instruments.NewRepository(), Router: router, KillSwitches: kills,
		Reconciliation: reconciliation.NewRepository(clk, out, aw), USDAssetID: assets.NewAssetID(),
	}
}

// TestNewService_ExecutesOnThePaperVenueByDefaultAndRefusesALiveAdapter: a
// service wired with no adapter executes on the paper venue in sandbox mode,
// one wired through NewExecutionAdapter executes on that adapter, and one
// handed an adapter that claims live mode does not start.
func TestNewService_ExecutesOnThePaperVenueByDefaultAndRefusesALiveAdapter(t *testing.T) {
	svc, err := NewService(unitConfig(t))
	require.NoError(t, err)
	assert.Equal(t, DefaultPaperVenueName, svc.Execution().Name())
	assert.Equal(t, config.ProviderModeSandbox, svc.Execution().Mode())

	venue, err := NewExecutionAdapter(config.EnvProd, ExecutionModeFor(config.EnvProd), PaperVenueConfig{Name: "paper-prod"})
	require.NoError(t, err)
	cfg := unitConfig(t)
	cfg.Execution = venue
	svc, err = NewService(cfg)
	require.NoError(t, err)
	assert.Same(t, venue, svc.Execution())

	cfg.Execution = liveAdapter{&PaperVenue{name: "claims-live"}}
	_, err = NewService(cfg)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	assert.Equal(t, ReasonLiveExecutionUnsupported, reasonOf(t, err))
}
