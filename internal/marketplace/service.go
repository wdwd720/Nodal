package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
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

// Transactor opens the transactions a validation runs in: the claim and the
// outcome are recorded in two of them, and the build runs between them, in
// none. *db.DB is the production one.
type Transactor interface {
	InTx(ctx context.Context, opts db.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error
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
	Audit      audit.Writer
	// Limits bounds every sandbox run (sandboxrt.DefaultLimits when zero).
	Limits sandboxrt.Limits
	// ServiceID is the SERVICE actor id on the intents the marketplace
	// submits (default "marketplace").
	ServiceID string
	// RuntimeVersion names the sandbox runtime recorded with a validation
	// (default "wazero/v1.12.0").
	RuntimeVersion string
	// MaxSnapshotAge bounds how far before now a follow run's as-of time may
	// be (default five minutes): a scheduler that fell behind does not follow
	// the past.
	MaxSnapshotAge time.Duration
	// MaxPriceAge bounds how old a reference price may be at a run's as-of
	// time (default five minutes). The service reads reference prices from
	// the price store itself; an instrument without one is not in the run.
	MaxPriceAge time.Duration
	// MaxCloses bounds the price history a run's input carries per
	// instrument (default 64, at most nodalsdk.MaxClosesPerSeries).
	MaxCloses int
	// PaperCapital is what every new install's paper book starts with, in
	// simulated dollars (default 10 000.00). It is recorded on the install
	// and never changes.
	PaperCapital money.USD
	// ValidationLease bounds how long a claimed build keeps others from
	// building the same version (default 15 minutes; it should exceed the
	// builder's own timeout). A claim whose builder vanished lapses with it.
	ValidationLease time.Duration
}

// Service is the marketplace. It holds no state beyond its dependencies;
// every mutating method runs in the caller's transaction.
type Service struct {
	cfg Config
}

// NewService validates the wiring. Every dependency is required: a
// marketplace that could not see the risk kernel, the router or the kill
// switches would follow strategies past them.
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
	if cfg.MaxPriceAge <= 0 {
		cfg.MaxPriceAge = 5 * time.Minute
	}
	if cfg.MaxCloses <= 0 {
		cfg.MaxCloses = 64
	}
	if cfg.PaperCapital.IsZero() {
		cfg.PaperCapital = money.USDFromMinor(1_000_000)
	}
	if cfg.ValidationLease <= 0 {
		cfg.ValidationLease = 15 * time.Minute
	}
	switch {
	case cfg.MaxCloses > nodalsdk.MaxClosesPerSeries:
		return nil, errs.Newf(errs.CodeInternal, "marketplace: at most %d closes per instrument", nodalsdk.MaxClosesPerSeries)
	case !cfg.PaperCapital.IsPositive():
		return nil, errs.New(errs.CodeInternal, "marketplace: the paper capital must be positive")
	case cfg.MaxPriceAge > 24*time.Hour || cfg.MaxPriceAge%time.Millisecond != 0:
		return nil, errs.New(errs.CodeInternal, "marketplace: a reference price is at most a day old, in whole milliseconds")
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

// requireOwner refuses an actor who does not own the account: a person acts
// for the accounts they own and no other (the database holds the same rule,
// MK012). A SERVICE or SYSTEM id is never an owner.
func requireOwner(ctx context.Context, q db.Querier, a econ.Actor, account accounts.AccountID, what string) error {
	if account.IsZero() {
		return errs.New(errs.CodeValidationFailed, "marketplace: an account is required")
	}
	var owner string
	err := q.QueryRow(ctx, `SELECT owner_user_id::text FROM accounts WHERE id = $1`, account).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.New(errs.CodeNotFound, "marketplace: account not found").WithField("account_id", account.String())
	}
	if err != nil {
		return dbErr("load account owner", err)
	}
	if a.ID != owner {
		return errs.Newf(errs.CodeForbidden, "marketplace: only the account's owner can %s for it", what).
			WithField("account_id", account.String())
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
