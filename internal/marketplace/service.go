package marketplace

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// ArtifactBuilder scans and builds a version's source. *sandboxrt.Builder is
// the production builder; its Build always scans first.
type ArtifactBuilder interface {
	Build(ctx context.Context, src sandboxrt.Source) (sandboxrt.Build, error)
}

// RiskContext is what the marketplace does not know about an intent: the
// account, identity, instrument, price and every other section Risk V2 may
// require, plus the account facts the legal router keys on.
//
// The marketplace OVERWRITES Input.Stage, Input.Now, Input.Intent,
// Input.Strategy and Input.Sandbox with what it knows itself, so a provider
// can never vouch for a strategy or a sandbox run. A section the provider
// leaves out that the intent requires is a rejection, never a pass.
type RiskContext struct {
	Input        risk.AdvancedInput
	Jurisdiction string
	Verification valuedomain.VerificationLevel
}

// RiskSections supplies the RiskContext of an intent.
type RiskSections interface {
	RiskContext(ctx context.Context, q db.Querier, in econ.Intent, p sandboxrt.AcceptedProposal) (RiskContext, error)
}

// PaperExecution is what reaches execution: a PAPER intent the risk kernel
// allowed, the decision that allowed it, the proposal it came from and the
// marketplace's dispatch effect.
type PaperExecution struct {
	Intent     econ.Intent
	Decision   risk.AdvancedDecision
	DecisionID risk.DecisionID
	Proposal   sandboxrt.AcceptedProposal
	EffectID   econ.EffectID
}

// ExecutionReceipt is what execution reports back.
type ExecutionReceipt struct {
	// ExternalRef names what execution did (an order, a paper fill).
	ExternalRef string
}

// IntentExecutor executes risk-approved paper intents. The orders subsystem
// implements it. It runs inside a savepoint of the caller's transaction: an
// error rolls its writes back, except SUBMISSION_STATE_UNKNOWN, whose writes
// are kept and whose effect becomes UNKNOWN (reconciled, never retried).
type IntentExecutor interface {
	ExecutePaper(ctx context.Context, tx pgx.Tx, ex PaperExecution) (ExecutionReceipt, error)
}

// SnapshotSource builds the input document of a scheduled follow run.
type SnapshotSource interface {
	Snapshot(ctx context.Context, q db.Querier, install Install, v Version, asOf time.Time) (sandboxrt.Input, error)
}

// Config wires a Service.
type Config struct {
	Clock      clock.Clock
	Intents    *econ.IntentStore
	Effects    *econ.EffectStore
	Auth       *econ.Authorizer
	Risk       *risk.Store
	RiskPolicy risk.AdvancedPolicy
	Router     *legalrouter.Router
	Sandbox    *sandboxrt.Service
	Builder    ArtifactBuilder
	Sections   RiskSections
	Executor   IntentExecutor
	// Snapshots is needed only for scheduled follow runs.
	Snapshots SnapshotSource
	Audit     audit.Writer
	// Limits bounds every sandbox run (sandboxrt.DefaultLimits when zero).
	Limits sandboxrt.Limits
	// ServiceID is the SERVICE actor id on the intents the marketplace
	// submits (default "marketplace").
	ServiceID string
	// RuntimeVersion names the sandbox runtime recorded with a validation
	// (default "wazero/v1.12.0").
	RuntimeVersion string
	// MaxSnapshotAge bounds how old a follow run's input may be (default
	// five minutes).
	MaxSnapshotAge time.Duration
}

// Service is the marketplace. It holds no state beyond its dependencies;
// every mutating method runs in the caller's transaction.
type Service struct {
	cfg Config
}

// NewService validates the wiring. Every dependency except Snapshots is
// required: a marketplace that could not see the risk kernel, the router or
// the kill switches would follow strategies past them.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.Clock == nil, cfg.Intents == nil, cfg.Effects == nil, cfg.Auth == nil, cfg.Risk == nil, cfg.Router == nil,
		cfg.Sandbox == nil, cfg.Builder == nil, cfg.Sections == nil, cfg.Executor == nil, cfg.Audit == nil:
		return nil, errs.New(errs.CodeInternal, "marketplace: every dependency is required")
	case cfg.RiskPolicy.Missing():
		return nil, errs.New(errs.CodeInternal, "marketplace: a risk policy is required")
	}
	if cfg.Limits == (sandboxrt.Limits{}) {
		cfg.Limits = sandboxrt.DefaultLimits()
	}
	if err := cfg.Limits.Validate(); err != nil {
		return nil, err
	}
	if cfg.ServiceID == "" {
		cfg.ServiceID = "marketplace"
	}
	if cfg.RuntimeVersion == "" {
		cfg.RuntimeVersion = "wazero/v1.12.0"
	}
	if cfg.MaxSnapshotAge <= 0 {
		cfg.MaxSnapshotAge = 5 * time.Minute
	}
	return &Service{cfg: cfg}, nil
}

func (s *Service) now() time.Time { return s.cfg.Clock.Now().UTC() }

func (s *Service) serviceActor() econ.Actor {
	return econ.Actor{Type: security.ActorService, ID: s.cfg.ServiceID}
}

func requireActor(a econ.Actor, allowed []security.ActorType, what string) error {
	if !a.Type.Valid() || strings.TrimSpace(a.ID) == "" {
		return errs.New(errs.CodeValidationFailed, "marketplace: an actor type and id are required")
	}
	if a.Type == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "marketplace: an agent cannot "+what)
	}
	if !actorIn(a.Type, allowed) {
		return errs.Newf(errs.CodeForbidden, "marketplace: a %s cannot %s", a.Type, what)
	}
	return nil
}

func (s *Service) authorize(ctx context.Context, tx pgx.Tx, m gates.Mode) error {
	return s.cfg.Auth.Require(ctx, tx, gates.StrategyMarketplace, m)
}

func (s *Service) auditEvent(ctx context.Context, tx pgx.Tx, stream string, actor econ.Actor, action, resourceType, resourceID, reason string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "marketplace: encode audit payload")
	}
	_, err = s.cfg.Audit.Append(ctx, tx, audit.Event{
		Stream: stream, ActorType: string(actor.Type), ActorID: actor.ID, Action: action,
		ResourceType: resourceType, ResourceID: resourceID, Reason: reason, Payload: body, OccurredAt: s.now(),
	})
	return err
}

func dbErr(op string, err error) error {
	if db.IsRetryable(err) {
		return err
	}
	if ee, ok := errs.As(err); ok {
		return ee
	}
	if db.IsUniqueViolation(err) {
		return errs.Wrap(err, errs.CodeConflict, "marketplace: "+op+" conflicts with an existing row")
	}
	return errs.Wrap(err, errs.CodeInternal, "marketplace: "+op)
}
