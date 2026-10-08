package derivatives

import (
	"context"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// DerivativesExecutionAdapter is where a derivative order executes (advanced
// goal §27): given the contract's terms, the customer's side, the number of
// contracts and the mark the order is priced against, it answers how the
// order executes -- the trade price and the slippage taken -- or refuses it.
//
// It is the seam OpenPosition and ClosePosition always priced through. The
// service asks once per order, inside the order's database transaction and
// under the clearing house's lock, before the risk kernel decides: the kernel
// judges the order at exactly the price the adapter answered and the fill
// applies exactly that price. So an adapter answers from the request alone --
// no I/O, no clock, no state of its own -- and a refusal changes nothing. An
// adapter that had to call out of process could not keep that promise inside
// a transaction; it would need the two-phase seam equities has (record the
// order, call outside any transaction, apply the answer), which is why there
// is no such adapter here and LIVE is refused (gates.AdvancedSpecs:
// LiveImplemented is false for DERIVATIVES).
//
// Whatever the adapter, the counterparty of every fill is the SIMULATED
// CLEARING_HOUSE facility. An Execution keeps these promises, which Check
// states and the service enforces on every answer before anything is priced
// at it (TestContract_PaperVenueKeepsTheExecutionContract):
//
//   - Source is a trade source (PAPER or BOOK).
//   - SlippageBPS is in [0, req.MaxSlippageBPS].
//   - Price is on the contract's tick grid at its price scale, and never
//     better for the customer than the mark: a BUY at or above it, a SELL at
//     or below it.
//   - Price is never worse for the customer than the mark moved against the
//     customer by SlippageBPS and rounded onto the tick grid against the
//     customer.
//   - The same request always gets the same answer; a malformed request is
//     refused with VALIDATION_FAILED.
type DerivativesExecutionAdapter interface {
	// Name identifies the venue.
	Name() string
	// Mode is the provider mode the adapter was built for.
	Mode() config.ProviderMode
	// Execute answers how the order executes. Nothing is executed until the
	// fill is applied, which the service does only after the risk kernel
	// allowed exactly this execution.
	Execute(ctx context.Context, req ExecutionRequest) (Execution, error)
}

// ExecutionRequest is one order as the execution venue sees it.
type ExecutionRequest struct {
	// Spec is the contract's current terms (share-locked by the caller).
	Spec Spec
	// Side is the customer's side of the trade.
	Side FillSide
	// Quantity is the number of contracts, positive.
	Quantity money.Quantity
	// Mark is the mark the order is priced against, at the contract's price
	// scale and on its tick grid. Its freshness is the service's decision: a
	// close is priced only at a fresh mark, an opening order's freshness is
	// judged by the risk kernel.
	Mark Mark
	// MaxSlippageBPS is the slippage against the mark the customer was told
	// an execution may take (Policy.PaperSlippageBPS).
	MaxSlippageBPS money.BPS
	// At is when the order executes.
	At time.Time
}

func (r ExecutionRequest) validate() error {
	var problems []string
	if !r.Side.Valid() {
		problems = append(problems, "side must be BUY or SELL")
	}
	if !r.Quantity.IsPositive() {
		problems = append(problems, "quantity must be positive")
	}
	switch {
	case !r.Spec.Tick.IsPositive():
		problems = append(problems, "the contract's terms need a positive tick")
	case r.Mark.ID.IsZero() || !markPriceable(r.Spec, r.Mark):
		problems = append(problems, "a recorded mark at the contract's price scale, on its tick grid, is required")
	}
	if r.MaxSlippageBPS < 0 || r.MaxSlippageBPS >= money.OneHundredPercent {
		problems = append(problems, "the disclosed slippage must be within 0..9999 bps")
	}
	if r.At.IsZero() {
		problems = append(problems, "the execution time is required")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "derivatives: invalid execution request").WithField("problems", problems)
	}
	return nil
}

// Execution is the venue's answer for one order.
type Execution struct {
	// Venue names the venue that answered.
	Venue string
	// Source is what produced the fill: PAPER for the paper venue.
	Source FillSource
	// Price is the trade price at the contract's price scale.
	Price money.Quantity
	// SlippageBPS is the slippage the execution took against the mark.
	SlippageBPS money.BPS
}

// fillReason is the intent transition's reason for the fill: "paper fill"
// for the paper venue, as it always was.
func (e Execution) fillReason() string { return strings.ToLower(string(e.Source)) + " fill" }

// Check reports whether e keeps the execution contract for req (see
// DerivativesExecutionAdapter). It is the contract, executable: the service
// runs it on every answer, and the contract tests run it on the paper venue.
func (e Execution) Check(req ExecutionRequest) error {
	var problems []string
	if !e.Source.isTrade() {
		problems = append(problems, "the source of an execution is PAPER or BOOK")
	}
	if e.SlippageBPS < 0 || e.SlippageBPS > req.MaxSlippageBPS {
		problems = append(problems, "the slippage is outside what the customer was told")
	}
	if !req.Spec.OnTick(e.Price) {
		problems = append(problems, "the price is not on the contract's tick grid")
	}
	if len(problems) == 0 {
		mark := req.Mark.MarkMantissa
		bound, err := paperPrice(req.Spec, mark, req.Side, e.SlippageBPS)
		if err != nil {
			return err
		}
		switch req.Side {
		case Buy:
			if e.Price.Cmp(mark) < 0 || e.Price.Cmp(bound) > 0 {
				problems = append(problems, "a buy executes at or above the mark and at most the mark moved up by the slippage")
			}
		case Sell:
			if e.Price.Cmp(mark) > 0 || e.Price.Cmp(bound) < 0 {
				problems = append(problems, "a sell executes at or below the mark and at least the mark moved down by the slippage")
			}
		}
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeInternal, "derivatives: the execution venue broke the execution contract").
			WithField("venue", e.Venue).WithField("problems", problems)
	}
	return nil
}

// DefaultPaperVenueName names the paper venue a Config without an adapter
// executes on.
const DefaultPaperVenueName = "nodal-paper-derivatives"

// PaperVenueConfig configures the paper venue.
type PaperVenueConfig struct {
	// Name identifies the venue (1..100 characters).
	Name string
}

// PaperVenue is the sandbox's execution venue: an order fills against the
// clearing house at the mark, moved against the customer by the disclosed
// slippage and rounded onto the tick grid against the customer as well
// (paperPrice), all of it recorded on the fill. It is the only
// DerivativesExecutionAdapter in this build, and its mode is sandbox: it is
// not a stand-in for a venue somewhere else -- derivatives here are novated to
// Nodal's own SIMULATED clearing house, and the paper venue is that clearing
// house's execution (doc.go, "Execution"). It holds nothing but its name.
type PaperVenue struct {
	name string
}

func newPaperVenue(cfg PaperVenueConfig) (*PaperVenue, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" || name != cfg.Name || len(name) > 100 {
		return nil, errs.New(errs.CodeValidationFailed, "derivatives: the paper venue needs a name of 1..100 characters")
	}
	return &PaperVenue{name: name}, nil
}

// Name implements DerivativesExecutionAdapter.
func (p *PaperVenue) Name() string { return p.name }

// Mode implements DerivativesExecutionAdapter: the paper venue is the
// sandbox's own venue.
func (p *PaperVenue) Mode() config.ProviderMode { return config.ProviderModeSandbox }

// Execute implements DerivativesExecutionAdapter: the paper price, at exactly
// the disclosed slippage.
func (p *PaperVenue) Execute(_ context.Context, req ExecutionRequest) (Execution, error) {
	if err := req.validate(); err != nil {
		return Execution{}, err
	}
	price, err := paperPrice(req.Spec, req.Mark.MarkMantissa, req.Side, req.MaxSlippageBPS)
	if err != nil {
		return Execution{}, err
	}
	return Execution{Venue: p.name, Source: SourcePaper, Price: price, SlippageBPS: req.MaxSlippageBPS}, nil
}

// Stable reasons for refusing an execution adapter.
const (
	// ReasonLiveExecutionUnsupported: live derivatives execution does not
	// exist in this build. No environment, flag or configuration changes that.
	ReasonLiveExecutionUnsupported = "DERIVATIVES_LIVE_EXECUTION_UNSUPPORTED"
	// ReasonProviderModeNotAllowed: a fake provider never runs where the
	// environment refuses fakes (STAGING and PROD).
	ReasonProviderModeNotAllowed = "DERIVATIVES_PROVIDER_MODE_NOT_ALLOWED"
	// ReasonProviderModeUnimplemented: this build has no fake execution venue;
	// the paper venue is the sandbox.
	ReasonProviderModeUnimplemented = "DERIVATIVES_PROVIDER_MODE_UNIMPLEMENTED"
)

// NewExecutionAdapter returns the execution adapter for a provider mode in an
// environment. Live is refused unconditionally, before the environment is
// even looked at; fake is refused where the environment refuses fakes, and
// everywhere else because no fake venue exists; sandbox is the paper venue,
// in every environment -- in STAGING and PROD the DERIVATIVES gate still has
// to be ACTIVE before an order reaches it (gates.DecideMode).
func NewExecutionAdapter(env config.Environment, mode config.ProviderMode, cfg PaperVenueConfig) (DerivativesExecutionAdapter, error) {
	if mode == config.ProviderModeLive {
		return nil, errs.New(errs.CodeUnsupported, "derivatives: live derivatives execution does not exist in this build").
			WithField("reason", ReasonLiveExecutionUnsupported)
	}
	if !mode.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "derivatives: unknown provider mode %q", mode)
	}
	if !env.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "derivatives: unknown environment %q", env)
	}
	if mode == config.ProviderModeFake {
		if !env.AllowsFakeProviders() {
			return nil, errs.Newf(errs.CodeForbidden, "derivatives: provider mode fake is not allowed in environment %s", env).
				WithField("reason", ReasonProviderModeNotAllowed)
		}
		return nil, errs.New(errs.CodeUnsupported, "derivatives: there is no fake execution venue; the paper venue is the sandbox").
			WithField("reason", ReasonProviderModeUnimplemented)
	}
	return newPaperVenue(cfg)
}

// ExecutionModeFor is the provider mode a binary builds the execution adapter
// in when no configuration names one: sandbox, in every environment. The
// paper venue is the only implementation, and it is the sandbox; there is no
// fake to choose where fakes are allowed, and live is refused everywhere.
// (Providers.Execution names the spot execution router and the equities
// broker boundary, never the derivatives venue.)
func ExecutionModeFor(config.Environment) config.ProviderMode { return config.ProviderModeSandbox }

// checkAdapter refuses an adapter a Service must not execute on.
func checkAdapter(a DerivativesExecutionAdapter) error {
	switch m := a.Mode(); {
	case m == config.ProviderModeLive:
		return errs.New(errs.CodeUnsupported, "derivatives: a live execution adapter is refused").
			WithField("reason", ReasonLiveExecutionUnsupported)
	case !m.IsValid():
		return errs.Newf(errs.CodeInternal, "derivatives: the execution adapter reports an unknown mode %q", m)
	case strings.TrimSpace(a.Name()) == "":
		return errs.New(errs.CodeInternal, "derivatives: the execution adapter has no name")
	}
	return nil
}

// executeOrder asks the execution adapter how an order executes and refuses
// an answer that breaks the execution contract, before anything is priced at
// it.
func (s *Service) executeOrder(ctx context.Context, spec Spec, side FillSide, qty money.Quantity, mark Mark, at time.Time) (Execution, error) {
	req := ExecutionRequest{Spec: spec, Side: side, Quantity: qty, Mark: mark, MaxSlippageBPS: s.policy.PaperSlippageBPS, At: at}
	ex, err := s.execution.Execute(ctx, req)
	if err != nil {
		return Execution{}, err
	}
	if err := ex.Check(req); err != nil {
		return Execution{}, err
	}
	return ex, nil
}
