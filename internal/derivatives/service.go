package derivatives

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
)

// ActorName is the actor id of every effect this subsystem applies.
const ActorName = "derivatives"

// DefaultClearingHouseName names the CLEARING_HOUSE facility every derivative
// cash flow goes through.
const DefaultClearingHouseName = "derivatives clearing house"

// DefaultReservationTTL bounds the reservation of a trade's margin and
// premium. The reservation is created and consumed in one transaction; the TTL
// only matters if that transaction outlives it.
const DefaultReservationTTL = 5 * time.Minute

// Config wires a Service. Every dependency is required -- Execution aside,
// whose default is the only adapter there is: a service that could not see
// the gates, the kill switches, the risk kernel or the reconciliation
// register would act without them.
type Config struct {
	Clock          clock.Clock
	Intents        *econ.IntentStore
	Effects        *econ.EffectStore
	Authorizer     *econ.Authorizer
	Registry       *econ.Registry
	Ledger         *ledger.Service
	Capital        *capital.Service
	RiskStore      *risk.Store
	RiskPolicy     risk.AdvancedPolicy
	Policy         Policy
	Prices         valuation.PriceSource
	Instruments    *instruments.Repository
	Router         *legalrouter.Router
	KillSwitches   econ.KillSwitchReader
	Reconciliation *reconciliation.Repository
	// USDAssetID is the sandbox dollar margin snapshots and buying power are
	// expressed in. Positions settling in it are valued at face; any other
	// settlement asset needs a fresh price against it.
	USDAssetID assets.AssetID
	// ClearingHouseName names the CLEARING_HOUSE facility (default
	// DefaultClearingHouseName).
	ClearingHouseName string
	// ReservationTTL bounds a trade's capital reservation (default
	// DefaultReservationTTL).
	ReservationTTL time.Duration
	// Execution is where orders execute (DerivativesExecutionAdapter). The
	// binaries build it with NewExecutionAdapter. It is the one dependency
	// with a default: nil is the paper venue under DefaultPaperVenueName --
	// the only adapter this build has, and exactly what NewExecutionAdapter
	// builds in every environment -- so a caller that wires none executes
	// where every deployment does. An adapter in mode live is refused.
	Execution DerivativesExecutionAdapter
}

// Service is the derivatives subsystem: futures, perpetuals and options in
// PAPER and SANDBOX mode, cleared through one clearing house. It holds no
// state beyond its dependencies; every mutating method runs in the caller's
// transaction.
type Service struct {
	clk            clock.Clock
	intents        *econ.IntentStore
	effects        *econ.EffectStore
	authorizer     *econ.Authorizer
	registry       *econ.Registry
	ledger         *ledger.Service
	capital        *capital.Service
	riskStore      *risk.Store
	riskPolicy     risk.AdvancedPolicy
	policy         Policy
	prices         valuation.PriceSource
	instruments    *instruments.Repository
	router         *legalrouter.Router
	kills          econ.KillSwitchReader
	recon          *reconciliation.Repository
	usdAsset       assets.AssetID
	clearingName   string
	reservationTTL time.Duration
	actor          econ.Actor
	execution      DerivativesExecutionAdapter
}

// NewService validates the wiring.
func NewService(cfg Config) (*Service, error) {
	var missing []string
	for name, ok := range map[string]bool{
		"clock": cfg.Clock != nil, "intents": cfg.Intents != nil, "effects": cfg.Effects != nil,
		"authorizer": cfg.Authorizer != nil, "registry": cfg.Registry != nil, "ledger": cfg.Ledger != nil,
		"capital": cfg.Capital != nil, "risk store": cfg.RiskStore != nil, "prices": cfg.Prices != nil,
		"instruments": cfg.Instruments != nil, "router": cfg.Router != nil, "kill switches": cfg.KillSwitches != nil,
		"reconciliation": cfg.Reconciliation != nil, "usd asset": !cfg.USDAssetID.IsZero(),
	} {
		if !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, errs.New(errs.CodeInternal, "derivatives: service wiring is incomplete").WithField("missing", missing)
	}
	if cfg.RiskPolicy.Missing() {
		return nil, errs.New(errs.CodeInternal, "derivatives: a usable risk policy is required")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(cfg.ClearingHouseName)
	if name == "" {
		name = DefaultClearingHouseName
	}
	ttl := cfg.ReservationTTL
	if ttl <= 0 {
		ttl = DefaultReservationTTL
	}
	if ttl > capital.MaxReservationTTL {
		return nil, errs.New(errs.CodeInternal, "derivatives: reservation TTL exceeds the capital maximum")
	}
	execution := cfg.Execution
	if execution == nil {
		venue, err := newPaperVenue(PaperVenueConfig{Name: DefaultPaperVenueName})
		if err != nil {
			return nil, err
		}
		execution = venue
	}
	if err := checkAdapter(execution); err != nil {
		return nil, err
	}
	return &Service{
		clk: cfg.Clock, intents: cfg.Intents, effects: cfg.Effects, authorizer: cfg.Authorizer, registry: cfg.Registry,
		ledger: cfg.Ledger, capital: cfg.Capital, riskStore: cfg.RiskStore, riskPolicy: cfg.RiskPolicy, policy: cfg.Policy,
		prices: cfg.Prices, instruments: cfg.Instruments, router: cfg.Router, kills: cfg.KillSwitches,
		recon: cfg.Reconciliation, usdAsset: cfg.USDAssetID, clearingName: name, reservationTTL: ttl,
		actor: econ.Actor{Type: security.ActorSystem, ID: ActorName}, execution: execution,
	}, nil
}

// Policy returns the service's margin, funding and paper-fill policy.
func (s *Service) Policy() Policy { return s.policy }

// Execution returns the adapter orders execute on.
func (s *Service) Execution() DerivativesExecutionAdapter { return s.execution }

// clearingHouse returns the CLEARING_HOUSE facility, creating it ACTIVE on
// first use, and refuses one that is not ACTIVE. Only this package's
// operations reach it; nothing outside moves value through the clearing
// house.
func (s *Service) clearingHouse(ctx context.Context, tx pgx.Tx) (econ.Facility, error) {
	f, err := s.registry.EnsureFacility(ctx, tx, ledger.FacilityClearingHouse, s.clearingName, s.actor)
	if err != nil {
		return econ.Facility{}, err
	}
	if err := f.RequireActive(); err != nil {
		return econ.Facility{}, err
	}
	return f, nil
}

// Refusal reports an intent the subsystem decided not to execute: the route,
// the capability gate, the risk kernel or buying power said no.
//
// A refusal is a recorded fact, not a failure: by the time it is returned the
// intent is REJECTED with a stable code and, for a risk refusal, the Risk V2
// decision is persisted -- inside the caller's transaction. The caller should
// commit that transaction to keep the record (AsRefusal tells a refusal from
// an error that must roll back), and return the refusal to its own caller.
type Refusal struct {
	IntentID   econ.IntentID
	Code       errs.Code
	Reasons    []string
	DecisionID *risk.DecisionID
	// Replay is true when the refusal was recorded by an earlier attempt with
	// the same idempotency key.
	Replay bool
	err    *errs.Error
}

func (r *Refusal) Error() string { return r.err.Error() }

// Unwrap exposes the API error so errs.CodeOf sees RISK_REJECTED,
// CAPABILITY_NOT_APPROVED, INSUFFICIENT_BUYING_POWER, ... and its fields.
func (r *Refusal) Unwrap() error { return r.err }

// AsRefusal reports whether err is a Refusal.
func AsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// Outcome is the result of an intent-driven operation.
type Outcome struct {
	Intent econ.Intent
	// Replay is true when the idempotency key had already been executed;
	// nothing was written this time.
	Replay     bool
	DecisionID *risk.DecisionID
	Decision   *risk.AdvancedDecision
	// Fill is the customer's fill (the opening or closing trade, or the
	// exercise).
	Fill       *FillRecord
	Position   *Position
	ExerciseID *ExerciseID
	Journals   []ledger.TransactionID
}

// refuse rejects the intent with a stable code and returns the refusal.
func (s *Service) refuse(ctx context.Context, tx pgx.Tx, in econ.Intent, code errs.Code, detail string, reasons []string, decision *risk.DecisionID) (Outcome, error) {
	rejected, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, s.actor, detail, string(code))
	if err != nil {
		return Outcome{}, err
	}
	if reasons == nil {
		reasons = []string{}
	}
	e := errs.New(code, "derivatives: "+detail).WithField("intent_id", in.ID.String()).WithField("reasons", reasons)
	if decision != nil {
		e = e.WithField("risk_decision_id", decision.String())
	}
	return Outcome{Intent: rejected, DecisionID: decision}, &Refusal{IntentID: in.ID, Code: code, Reasons: reasons, DecisionID: decision, err: e}
}

// serializeClearing takes the clearing house's transaction-scoped lock. Every
// operation that can move value through the clearing house takes it first,
// before any other lock: a run (which takes each account's audit-stream lock
// while already holding the clearing house's balance rows) and a trade (which
// holds its account's stream lock before it reaches those rows) would
// otherwise be able to wait on each other in opposite orders. Reconciliation
// takes it too, so it compares the register and the books at one quiescent
// moment rather than across a concurrent flow. Released at COMMIT or
// ROLLBACK; re-entrant within a transaction.
func (s *Service) serializeClearing(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "derivatives-clearing-house:"+s.clearingName); err != nil {
		return dbErr("serialize on the clearing house", err)
	}
	return nil
}

// advance moves an intent one step along its lifecycle.
func (s *Service) advance(ctx context.Context, tx pgx.Tx, in econ.Intent, to econ.IntentStatus, reason string) (econ.Intent, error) {
	return s.intents.Transition(ctx, tx, in.ID, to, s.actor, reason, "")
}

// replayOutcome answers a repeated intent from what the first attempt left.
func (s *Service) replayOutcome(ctx context.Context, tx pgx.Tx, in econ.Intent) (Outcome, error) {
	switch in.Status {
	case econ.StatusCompleted:
		fills, err := FillsForIntent(ctx, tx, in.ID)
		if err != nil {
			return Outcome{}, err
		}
		out := Outcome{Intent: in, Replay: true}
		for i := range fills {
			if fills[i].AccountID == in.AccountID {
				f := fills[i]
				out.Fill = &f
				p, err := GetPosition(ctx, tx, f.PositionID)
				if err != nil {
					return Outcome{}, err
				}
				out.Position = &p
				break
			}
		}
		return out, nil
	case econ.StatusRejected:
		e := errs.New(errs.Code(in.RejectionCode), "derivatives: this intent was refused").
			WithField("intent_id", in.ID.String()).WithField("replay", true)
		return Outcome{Intent: in, Replay: true}, &Refusal{IntentID: in.ID, Code: errs.Code(in.RejectionCode), Replay: true, err: e}
	}
	return Outcome{}, errs.Newf(errs.CodeReconciliationRequired,
		"derivatives: intent %s is %s; it is not retried, it is reconciled", in.ID, in.Status).
		WithField("intent_id", in.ID.String()).WithField("status", string(in.Status))
}
