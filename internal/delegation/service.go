package delegation

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
)

// IdentityReader answers whether an account's owner has a verified,
// unexpired identity. compliance.NewAccountVerification satisfies it.
type IdentityReader interface {
	IdentityVerified(ctx context.Context, q db.Querier, account accounts.AccountID) (bool, error)
}

// Params are the service's tunables; zero values take the defaults.
type Params struct {
	// GasMinimum and GasTopUp keep a delegated wallet able to pay network
	// fees: below the minimum it is topped up from the devnet faucet.
	GasMinimum uint64
	GasTopUp   uint64
	// ReservationTTL is the life of a reservation; it is locked to its swap
	// or withdrawal, so it never expires while one is pending.
	ReservationTTL time.Duration
	// QuoteTTL is how long a venue quote may be signed against.
	QuoteTTL time.Duration
	// PriceMaxAge is the oldest price a quote or a valuation accepts.
	PriceMaxAge time.Duration
	// StepUpMaxAge bounds the authentication age of a grant, a revocation,
	// a swap decision and a change to destinations or policy.
	StepUpMaxAge time.Duration
	// ApprovalTTL is how long a withdrawal waits for a human decision.
	ApprovalTTL time.Duration
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
	if p.QuoteTTL <= 0 {
		p.QuoteTTL = 2 * time.Minute
	}
	if p.PriceMaxAge <= 0 {
		p.PriceMaxAge = time.Minute
	}
	if p.StepUpMaxAge <= 0 {
		p.StepUpMaxAge = 15 * time.Minute
	}
	if p.ApprovalTTL <= 0 {
		p.ApprovalTTL = 24 * time.Hour
	}
	return p
}

// Deps are the Service's collaborators. Every one is required except Random
// (crypto/rand by default) and Venues (a deployment with no venue signs no
// swap).
type Deps struct {
	DB             *db.DB
	Clock          clock.Clock
	Environment    config.Environment
	Host           ChainHost
	Intents        *econ.IntentStore
	Effects        *econ.EffectStore
	Authorizer     *econ.Authorizer
	Kills          *killswitch.Checker
	Ledger         *ledger.Service
	Capital        *capital.Service
	Risk           *risk.Store
	RiskPolicy     risk.AdvancedPolicy
	Router         *legalrouter.Router
	Prices         *valuation.PriceStore
	Identity       IdentityReader
	Profiles       *compliance.Repository
	Reconciliation *reconciliation.Repository
	Audit          audit.Writer
	Accounts       *accounts.Repository
	Assets         *assets.Repository
	Signer         *DelegatedSigner
	Venues         []*VenueSigner
	Random         io.Reader
	Params         Params
}

// Service is delegated agent authority: capabilities, delegated signing of
// swaps and delegated withdrawals.
type Service struct {
	db         *db.DB
	clk        clock.Clock
	env        config.Environment
	host       ChainHost
	intents    *econ.IntentStore
	effects    *econ.EffectStore
	authorizer *econ.Authorizer
	kills      *killswitch.Checker
	ledger     *ledger.Service
	capital    *capital.Service
	risk       *risk.Store
	policy     risk.AdvancedPolicy
	router     *legalrouter.Router
	prices     *valuation.PriceStore
	identity   IdentityReader
	profiles   *compliance.Repository
	recon      *reconciliation.Repository
	audit      audit.Writer
	accounts   *accounts.Repository
	assets     *assets.Repository
	signer     *DelegatedSigner
	venues     map[devnet.PublicKey]*VenueSigner
	random     io.Reader
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
	check(d.Environment.IsValid(), "Environment")
	check(d.Host != nil, "Host")
	check(d.Intents != nil, "Intents")
	check(d.Effects != nil, "Effects")
	check(d.Authorizer != nil, "Authorizer")
	check(d.Kills != nil, "Kills")
	check(d.Ledger != nil, "Ledger")
	check(d.Capital != nil, "Capital")
	check(d.Risk != nil, "Risk")
	check(!d.RiskPolicy.Missing(), "RiskPolicy")
	check(d.Router != nil, "Router")
	check(d.Prices != nil, "Prices")
	check(d.Identity != nil, "Identity")
	check(d.Profiles != nil, "Profiles")
	check(d.Reconciliation != nil, "Reconciliation")
	check(d.Audit != nil, "Audit")
	check(d.Accounts != nil, "Accounts")
	check(d.Assets != nil, "Assets")
	check(d.Signer != nil, "Signer")
	if len(missing) > 0 {
		return nil, errs.New(errs.CodeInternal, "delegation: missing dependencies").WithField("missing", missing)
	}
	if d.Signer.env != d.Environment {
		return nil, errs.New(errs.CodeInternal, "delegation: the signer was configured for another environment")
	}
	venues := make(map[devnet.PublicKey]*VenueSigner, len(d.Venues))
	for _, v := range d.Venues {
		if v == nil {
			return nil, errs.New(errs.CodeInternal, "delegation: a nil venue signer")
		}
		venues[v.PublicKey()] = v
	}
	random := d.Random
	if random == nil {
		random = rand.Reader
	}
	return &Service{
		db: d.DB, clk: d.Clock, env: d.Environment, host: d.Host, intents: d.Intents, effects: d.Effects,
		authorizer: d.Authorizer, kills: d.Kills, ledger: d.Ledger, capital: d.Capital, risk: d.Risk, policy: d.RiskPolicy,
		router: d.Router, prices: d.Prices, identity: d.Identity, profiles: d.Profiles, recon: d.Reconciliation,
		audit: d.Audit, accounts: d.Accounts, assets: d.Assets, signer: d.Signer, venues: venues, random: random,
		params: d.Params.withDefaults(),
	}, nil
}

// txOptions is every transaction the orchestrators run. No external side
// effect happens inside a transaction function -- the chain is only read
// through the transaction and submitted to between transactions -- so a
// retry is safe.
var txOptions = db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}

func (s *Service) inTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return s.db.InTx(ctx, txOptions, fn)
}

// The service's own actors. Agents are never actors of a state change.
var (
	serviceActor  = Actor{Type: security.ActorService, ID: "delegation"}
	withdrawActor = Actor{Type: security.ActorService, ID: "delegation-withdrawals"}
	policyActor   = Actor{Type: security.ActorService, ID: "delegation-withdrawal-policy"}
)

// --- Authentication ---------------------------------------------------------------

// agentFor checks that the context's principal is an AGENT bound to the
// account, and that the agent exists, belongs to the account and may act.
func (s *Service) agentFor(ctx context.Context, q db.Querier, account accounts.AccountID) (security.Principal, agentRow, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return security.Principal{}, agentRow{}, errs.New(errs.CodeUnauthenticated, "delegation: an agent request needs an authenticated agent")
	}
	if !p.IsAgent() {
		return security.Principal{}, agentRow{}, errs.New(errs.CodeForbidden, "delegation: only an agent acts under a delegated capability")
	}
	if err := security.RequireAccount(ctx, account.String()); err != nil {
		return security.Principal{}, agentRow{}, errs.Wrap(err, errs.CodeForbidden, "delegation: the agent is not bound to this account")
	}
	a, found, err := getAgent(ctx, q, p.SubjectID)
	if err != nil {
		return security.Principal{}, agentRow{}, err
	}
	if !found || a.AccountID != account {
		return security.Principal{}, agentRow{}, errs.New(errs.CodeForbidden, "delegation: the agent is not one of this account's agents")
	}
	return p, a, nil
}

// ownerFor checks that the context's principal is the USER who owns the
// account (by the database, not by the principal's own claim) and, when
// stepUp is positive, that it authenticated strongly within stepUp.
func (s *Service) ownerFor(ctx context.Context, q db.Querier, account accounts.AccountID, stepUp time.Duration) (security.Principal, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return security.Principal{}, errs.New(errs.CodeUnauthenticated, "delegation: this is a customer action and needs an authenticated customer")
	}
	if p.IsAgent() || p.ActorType != security.ActorUser {
		return security.Principal{}, errs.New(errs.CodeForbidden, "delegation: only the account's owner may do this; never an agent")
	}
	if err := security.RequireAccountOwner(ctx, account.String()); err != nil {
		return security.Principal{}, errs.Wrap(err, errs.CodeForbidden, "delegation: the principal does not own the account")
	}
	acct, err := s.accounts.Get(ctx, q, account)
	if err != nil {
		return security.Principal{}, err
	}
	if acct.OwnerUserID.String() != p.SubjectID || acct.Kind != accounts.KindCustomer {
		return security.Principal{}, errs.New(errs.CodeForbidden, "delegation: the principal is not the owner of this customer account")
	}
	if stepUp > 0 {
		if err := security.RequireStepUp(ctx, stepUp, s.clk.Now); err != nil {
			return security.Principal{}, errs.Wrap(err, errs.CodeStepUpRequired, "delegation: a recent strong authentication is required")
		}
	}
	return p, nil
}

// requireExecutor refuses an agent principal on a service step: an agent
// never signs, submits, settles or posts.
func requireExecutor(ctx context.Context) error {
	if p, ok := security.PrincipalFrom(ctx); ok && p.IsAgent() {
		return errs.New(errs.CodeForbidden, "delegation: an agent never executes, signs or settles")
	}
	return nil
}

// serviceContext strips any principal from ctx: the ledger records the
// service, never the caller, as the poster of what the service settles.
func serviceContext(ctx context.Context) context.Context {
	return security.WithPrincipal(ctx, security.Principal{SubjectID: serviceActor.ID, ActorType: security.ActorService})
}

// --- Shared reads ------------------------------------------------------------------

func (s *Service) loadAccount(ctx context.Context, q db.Querier, account accounts.AccountID) (accounts.Account, error) {
	return s.accounts.Get(ctx, q, account)
}

func loadAsset(ctx context.Context, q db.Querier, aid assets.AssetID) (assets.Asset, error) {
	return assets.NewRepository().Get(ctx, q, aid)
}

func swapAssets(ctx context.Context, q db.Querier, sw Swap) (assets.Asset, assets.Asset, error) {
	pay, err := loadAsset(ctx, q, sw.PayAssetID)
	if err != nil {
		return assets.Asset{}, assets.Asset{}, err
	}
	recv, err := loadAsset(ctx, q, sw.ReceiveAssetID)
	if err != nil {
		return assets.Asset{}, assets.Asset{}, err
	}
	return pay, recv, nil
}

// swapClasses is the instrument classes a swap touches: both assets'.
func swapClasses(pay, recv assets.Asset) ([]InstrumentClass, bool) {
	a, ok1 := ClassifyAsset(pay)
	b, ok2 := ClassifyAsset(recv)
	if !ok1 || !ok2 {
		return nil, false
	}
	return sortedUnique([]InstrumentClass{a, b}), true
}

func capabilityStrategy(ctx context.Context, q db.Querier, cid CapabilityID) (string, error) {
	var strategy string
	if err := q.QueryRow(ctx, `SELECT strategy_id::text FROM delegated_capabilities WHERE id = $1`, cid).Scan(&strategy); err != nil {
		return "", dbErr("load capability strategy", err)
	}
	return strategy, nil
}

// usageExcluding is a capability's usage on the day of at and in all, not
// counting one usage row (the subject's own, when it is re-checked).
func usageExcluding(ctx context.Context, q db.Querier, cid CapabilityID, own UsageID, at time.Time) (money.USD, money.USD, error) {
	var dayMinor, allMinor int64
	err := q.QueryRow(ctx, `SELECT
		coalesce(sum(notional_minor) FILTER (WHERE usage_day = $3::date), 0)::bigint,
		coalesce(sum(notional_minor), 0)::bigint
		FROM delegated_capability_usage WHERE capability_id = $1 AND id <> $2`,
		cid, own, at.UTC().Format(time.DateOnly)).Scan(&dayMinor, &allMinor)
	if err != nil {
		return money.USD{}, money.USD{}, dbErr("read capability usage", err)
	}
	return money.USDFromMinor(dayMinor), money.USDFromMinor(allMinor), nil
}

// priceOf values one whole unit of a SIMULATED asset in the sandbox dollar:
// at par for a registry-declared USD-pegged unit, otherwise the freshest
// observation no older than PriceMaxAge. A missing or stale price is
// STALE_MARKET_DATA, never a guess.
func (s *Service) priceOf(ctx context.Context, q db.Querier, a assets.Asset, now time.Time) (money.Price, bool, error) {
	if a.IsStablecoin && a.PegCurrency == valuation.USDPeg {
		p, err := money.NewPrice(money.QuantityFromInt64(1), 0, econ.SandboxDollarID().String(), "registry-usd-peg", now)
		return p, true, err
	}
	p, err := s.prices.Latest(ctx, q, a.ID, econ.SandboxDollarID(), s.params.PriceMaxAge, now)
	return p, false, err
}

// killSwitches is the active switches as Risk V2 takes them.
func (s *Service) killSwitches(ctx context.Context, q db.Querier) ([]risk.KillSwitch, error) {
	snap, err := s.kills.Snapshot(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]risk.KillSwitch, 0, len(snap.Switches))
	for _, sw := range snap.Switches {
		out = append(out, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
	}
	return out, nil
}

// unknownEffects counts the account's delegated effects whose outcome is
// UNKNOWN: Risk V2's settlement section and the withdrawal anomaly.
func unknownEffects(ctx context.Context, q db.Querier, account accounts.AccountID) (int, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM economic_effects WHERE account_id = $1
		AND subsystem IN ('AGENT_AUTHORITY','WITHDRAWALS') AND status = 'UNKNOWN'`, account).Scan(&n); err != nil {
		return 0, dbErr("count unknown effects", err)
	}
	return n, nil
}

// --- Audit --------------------------------------------------------------------------

func (s *Service) auditEvent(ctx context.Context, tx pgx.Tx, actor Actor, action, resourceType, resourceID string,
	account accounts.AccountID, reason string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "delegation: encode audit payload")
	}
	stream := audit.SystemStream
	if !account.IsZero() {
		stream = audit.AccountStream(account.String())
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: stream, ActorType: string(actor.Type), ActorID: actor.ID, Action: action,
		ResourceType: resourceType, ResourceID: resourceID, Reason: reason, Payload: body, OccurredAt: s.clk.Now().UTC(),
	})
	return err
}

// --- Intents ------------------------------------------------------------------------

// moveIntent moves an intent to `to` if it is not already there or past it.
func (s *Service) moveIntent(ctx context.Context, tx pgx.Tx, iid econ.IntentID, to econ.IntentStatus, reason, code string) error {
	in, err := s.intents.Get(ctx, tx, iid)
	if err != nil {
		return err
	}
	if in.Status == to || in.Status.Terminal() || !econ.CanTransitionIntent(in.Status, to) {
		return nil
	}
	_, err = s.intents.Transition(ctx, tx, iid, to, serviceActor, reason, code)
	return err
}

// finishIntent ends an executing intent: COMPLETED (or RECONCILED from
// UNKNOWN) on success, FAILED otherwise.
func (s *Service) finishIntent(ctx context.Context, tx pgx.Tx, iid econ.IntentID, success bool, reason string) error {
	in, err := s.intents.Get(ctx, tx, iid)
	if err != nil {
		return err
	}
	to := econ.StatusFailed
	switch {
	case success && in.Status == econ.StatusUnknown:
		to = econ.StatusReconciled
	case success:
		to = econ.StatusCompleted
	}
	if in.Status == to || in.Status.Terminal() {
		return nil
	}
	_, err = s.intents.Transition(ctx, tx, iid, to, serviceActor, reason, "")
	return err
}

// refusal is a recorded refusal: what the caller commits and returns.
func refused(code errs.Code, reason, detail string) error {
	return errs.New(code, "delegation: "+detail).WithField("reason", reason)
}

// reasonOf returns the stable reason a refusal carries.
func reasonOf(err error) string {
	var e *errs.Error
	if !errors.As(err, &e) {
		return ""
	}
	if r, ok := e.Fields["reason"].(string); ok {
		return r
	}
	if codes, ok := e.Fields["reason_codes"].([]string); ok && len(codes) > 0 {
		return strings.Join(codes, ",")
	}
	return string(e.Code)
}
