package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// HolderSigner signs a BRIDGE_LOCK on behalf of a customer whose sandbox
// wallet it custodies, after inspecting that the message is exactly the
// agreed lock bound to the plan hash. stableasset.CustodialWalletSigner
// implements it; the bridge never holds a customer key.
type HolderSigner interface {
	HolderAddress(account accounts.AccountID) devnet.PublicKey
	SignHolderLock(account accounts.AccountID, planHash [32]byte, lock devnet.BridgeLock, msg devnet.Message) (devnet.TxID, []byte, error)
}

// Params are the service's tunables; zero values take the defaults.
type Params struct {
	GasMinimum     uint64
	GasTopUp       uint64
	ReservationTTL time.Duration
}

func (p Params) withDefaults() Params {
	if p.GasMinimum == 0 {
		p.GasMinimum = 1_000_000
	}
	if p.GasTopUp < p.GasMinimum {
		p.GasTopUp = 100 * p.GasMinimum
	}
	if p.ReservationTTL <= 0 || p.ReservationTTL > capital.MaxReservationTTL {
		p.ReservationTTL = capital.MaxReservationTTL
	}
	return p
}

// Deps are the Service's collaborators; every one is required except
// Provider, which defaults to the DevnetProvider over Host and Attester.
type Deps struct {
	DB             *db.DB
	Clock          clock.Clock
	Host           *devnethost.Host
	Intents        *econ.IntentStore
	Effects        *econ.EffectStore
	Authorizer     *econ.Authorizer
	Facilities     *econ.Registry
	Ledger         *ledger.Service
	Capital        *capital.Service
	Risk           *risk.Store
	RiskPolicy     risk.AdvancedPolicy
	Router         *legalrouter.Router
	Kills          econ.KillSwitchReader
	Reconciliation *reconciliation.Repository
	Audit          audit.Writer
	Accounts       *accounts.Repository
	Assets         *assets.Repository
	Holder         HolderSigner
	Attester       *Attester
	Provider       BridgeProvider
	Params         Params
}

// Service is the cross-chain capital controller (goal §18): routes, legs
// through their bound state machine, the BRIDGE facility books, the BRIDGE
// reconciliation and the buying-power reader.
type Service struct {
	db         *db.DB
	clk        clock.Clock
	host       *devnethost.Host
	intents    *econ.IntentStore
	effects    *econ.EffectStore
	authorizer *econ.Authorizer
	facilities *econ.Registry
	ledger     *ledger.Service
	capital    *capital.Service
	risk       *risk.Store
	policy     risk.AdvancedPolicy
	router     *legalrouter.Router
	kills      econ.KillSwitchReader
	recon      *reconciliation.Repository
	audit      audit.Writer
	accounts   *accounts.Repository
	assets     *assets.Repository
	holder     HolderSigner
	attester   *Attester
	provider   BridgeProvider
	params     Params
}

// New wires a Service.
func New(d Deps) (*Service, error) {
	var missing []string
	check := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	check(d.DB != nil, "DB")
	check(d.Clock != nil, "Clock")
	check(d.Host != nil, "Host")
	check(d.Intents != nil, "Intents")
	check(d.Effects != nil, "Effects")
	check(d.Authorizer != nil, "Authorizer")
	check(d.Facilities != nil, "Facilities")
	check(d.Ledger != nil, "Ledger")
	check(d.Capital != nil, "Capital")
	check(d.Risk != nil, "Risk")
	check(!d.RiskPolicy.Missing(), "RiskPolicy")
	check(d.Router != nil, "Router")
	check(d.Kills != nil, "Kills")
	check(d.Reconciliation != nil, "Reconciliation")
	check(d.Audit != nil, "Audit")
	check(d.Accounts != nil, "Accounts")
	check(d.Assets != nil, "Assets")
	check(d.Holder != nil, "Holder")
	check(d.Attester != nil, "Attester")
	if len(missing) > 0 {
		return nil, errs.New(errs.CodeInternal, "bridge: missing dependencies").WithField("missing", missing)
	}
	provider := d.Provider
	if provider == nil {
		p, err := NewDevnetProvider(d.Host, d.Attester)
		if err != nil {
			return nil, err
		}
		provider = p
	}
	return &Service{
		db: d.DB, clk: d.Clock, host: d.Host, intents: d.Intents, effects: d.Effects, authorizer: d.Authorizer,
		facilities: d.Facilities, ledger: d.Ledger, capital: d.Capital, risk: d.Risk, policy: d.RiskPolicy,
		router: d.Router, kills: d.Kills, recon: d.Reconciliation, audit: d.Audit, accounts: d.Accounts,
		assets: d.Assets, holder: d.Holder, attester: d.Attester, provider: provider, params: d.Params.withDefaults(),
	}, nil
}

// txOptions is every transaction this package runs. No external side effect
// happens inside a transaction function (only reads of the devnet host
// through the transaction itself), so a retry is safe.
var txOptions = db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}

// systemActor is the actor of every step after admission.
var systemActor = econ.SystemActor("bridge")

// breakerActor pauses a route whose books disagree with the chains.
var breakerActor = econ.SystemActor("bridge-circuit-breaker")

func requireActor(a econ.Actor) error {
	if !a.Type.Valid() || strings.TrimSpace(a.ID) == "" {
		return errs.New(errs.CodeValidationFailed, "bridge: an actor type and id are required")
	}
	if a.Type == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "bridge: an agent never moves capital across chains")
	}
	return nil
}

// authenticate checks that the actor is the authenticated principal and owns
// the account; without a principal only SYSTEM and SERVICE actors proceed.
func (s *Service) authenticate(ctx context.Context, actor econ.Actor, account accounts.AccountID) error {
	if err := requireActor(actor); err != nil {
		return err
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		if actor.Type == security.ActorSystem || actor.Type == security.ActorService {
			return nil
		}
		return errs.New(errs.CodeUnauthenticated, "bridge: a request on behalf of a person needs an authenticated principal")
	}
	if p.IsAgent() {
		return errs.New(errs.CodeForbidden, "bridge: an agent never moves capital across chains")
	}
	if p.SubjectID != actor.ID || p.ActorType != actor.Type {
		return errs.New(errs.CodeForbidden, "bridge: the request's actor is not the authenticated principal")
	}
	if err := security.RequireAccountOwner(ctx, account.String()); err != nil {
		return errs.Wrap(err, errs.CodeForbidden, "bridge: the principal does not own the account")
	}
	return nil
}

func (s *Service) auditEvent(ctx context.Context, tx pgx.Tx, actor econ.Actor, action, resourceType, resourceID, account, reason string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "bridge: encode audit payload")
	}
	stream := audit.SystemStream
	if account != "" {
		stream = audit.AccountStream(account)
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: stream, ActorType: string(actor.Type), ActorID: actor.ID, Action: action,
		ResourceType: resourceType, ResourceID: resourceID, Reason: reason, Payload: body, OccurredAt: s.clk.Now().UTC(),
	})
	return err
}

func dbErr(op string, err error) error {
	if db.IsRetryable(err) {
		return err
	}
	if e, ok := errs.As(err); ok {
		return e
	}
	if mapped := ledger.MapError(err); mapped != nil {
		if e, ok := errs.As(mapped); ok && e.Code != errs.CodeInternal {
			return e
		}
	}
	return errs.Wrap(err, errs.CodeInternal, "bridge: "+op)
}

// immediateBindings makes the econ intent/effect transition bindings, and
// this package's own leg binding, check each status change at the end of its
// statement instead of at COMMIT: a leg's admission moves one intent through
// several statuses and one leg AVAILABLE -> RESERVED -> BRIDGE_REQUESTED in
// one transaction, and on this branch the deferred AU001 binding keeps a
// single edge per entity per transaction. Every transition row is written
// before the UPDATE it licenses, so every immediate check passes; nothing is
// weakened.
//
// INTEGRATOR: once migration 00733 (accumulated edges) is merged, delete this
// helper and its one call site (PrepareLeg): bridge_legs is bound by the same
// cp_flag_transition_edge / cp_require_transition_edge functions.
func immediateBindings(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS economic_intents_require_transition, economic_effects_require_transition,
		bridge_legs_require_transition IMMEDIATE`); err != nil {
		return dbErr("make transition bindings immediate", err)
	}
	return nil
}
