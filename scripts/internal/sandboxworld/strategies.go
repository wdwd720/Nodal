package sandboxworld

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/httpapi"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
)

// Strategies is the strategy marketplace and the sandbox it runs strategies
// in, wired over the world exactly as cmd/api wires them (the
// "advanced:marketplace services" block of cmd/api/wire.go): the builder is
// the go command found on PATH, made absolute, building under
// sandboxrt.Builder's default bounds with the shared strategy build cache in
// this user's cache directory; the runner is wazero with WASI preview1 only;
// the sandbox charges every run to an account under sandboxrt.DefaultBudget;
// the risk sections are read from the database; and a risk-approved paper
// intent fills on the order book's paper venue under the one published
// schedule (httpapi.MarketplacePaperSchedule), booked through the world's fee
// ledger.
//
// The one difference from cmd/api: where no Go toolchain is found, cmd/api
// wires a builder that answers UNSUPPORTED; a demonstration of a build cannot
// demonstrate anything without one, so OpenStrategies refuses instead.
type Strategies struct {
	Builder  *sandboxrt.Builder
	Runner   *sandboxrt.Runner
	Sandbox  *sandboxrt.Service
	Executor *httpapi.MarketplacePaperExecutor
	Market   *marketplace.Service
}

// StrategyMaxPriceAge is the bound cmd/api gives the marketplace's reference
// prices, its risk sections and the paper venue's mark.
const StrategyMaxPriceAge = 5 * time.Minute

// OpenStrategies wires the marketplace over the world. Close releases the
// runner.
func (w *World) OpenStrategies(ctx context.Context) (*Strategies, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: strategies are built with the local Go toolchain and no go command is on PATH: %w", err)
	}
	if goBin, err = filepath.Abs(goBin); err != nil {
		return nil, fmt.Errorf("sandboxworld: the go command's path: %w", err)
	}
	cache := ""
	if base, cacheErr := os.UserCacheDir(); cacheErr == nil {
		cache = filepath.Join(base, "nodal", "strategy-gocache")
		if mkErr := os.MkdirAll(cache, 0o700); mkErr != nil {
			cache = ""
		}
	}
	builder, err := sandboxrt.NewBuilder(sandboxrt.BuilderConfig{GoBinary: goBin, CacheDir: cache, Clock: w.Clock})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: strategy builder: %w", err)
	}
	runner, err := sandboxrt.NewRunner(ctx, sandboxrt.RunnerConfig{})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: strategy sandbox runner: %w", err)
	}
	s, err := w.strategies(builder, runner)
	if err != nil {
		return nil, errors.Join(err, runner.Close(context.WithoutCancel(ctx)))
	}
	return s, nil
}

func (w *World) strategies(builder *sandboxrt.Builder, runner *sandboxrt.Runner) (*Strategies, error) {
	auth := econ.NewAuthorizer(w.Gates, w.Kills)
	sandbox := sandboxrt.NewService(w.Clock, auth, runner, w.Audit)
	verification, err := identity.NewVerificationResolver(w.DB)
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: verification resolver: %w", err)
	}
	sections, err := httpapi.NewMarketplaceRiskSections(httpapi.MarketplaceSectionsConfig{
		Clock: w.Clock, Accounts: accounts.NewRepository(), Instruments: w.Instruments, Identity: compliance.NewAccountVerification(w.Clock),
		Prices: w.Prices, KillSwitches: w.Kills, MaxPriceAge: StrategyMaxPriceAge, Verification: verification,
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: marketplace risk sections: %w", err)
	}
	intents := econ.NewIntentStore(w.Clock, w.Outbox, w.Audit)
	executor, err := httpapi.NewMarketplacePaperExecutor(httpapi.MarketplacePaperExecutorConfig{
		Venue: orders.PaperConfig{
			Clock: w.Clock, Ledger: w.Ledger, Capital: w.Capital, Intents: intents,
			Effects: econ.NewEffectStore(w.Clock, w.Outbox, w.Audit), Authorizer: auth, Risk: risk.NewStore(),
			RiskPolicy: risk.DefaultAdvancedPolicy(), Router: w.Router, Instruments: w.Instruments, Assets: w.Assets,
			Prices: w.Prices, KillSwitches: w.Kills, MaxMarkAge: StrategyMaxPriceAge,
			Verification: httpapi.AdvancedVerificationLevels{Rule: compliance.NewAccountVerification(w.Clock)},
			FeeLedger:    w.FeeLedger,
		},
		Schedule: httpapi.MarketplacePaperSchedule(),
		Intents:  intents,
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: marketplace paper executor: %w", err)
	}
	market, err := marketplace.NewService(marketplace.Config{
		Clock: w.Clock, Intents: intents, Effects: econ.NewEffectStore(w.Clock, w.Outbox, w.Audit), Auth: auth,
		Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: w.Router, Sandbox: sandbox, Builder: builder,
		Sections: sections, Executor: executor, Audit: w.Audit, MaxPriceAge: StrategyMaxPriceAge,
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxworld: strategy marketplace: %w", err)
	}
	return &Strategies{Builder: builder, Runner: runner, Sandbox: sandbox, Executor: executor, Market: market}, nil
}

// Close releases the runner and every module it compiled.
func (s *Strategies) Close(ctx context.Context) error { return s.Runner.Close(ctx) }
