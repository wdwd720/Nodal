package delegation

import (
	"context"
	"sort"
	"strings"

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
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuation"
)

// AgentAdmission is the whole of delegated authority an agent tree may hold.
// It admits an agent's delegated swap request (RequestSwap), records an
// agent's withdrawal proposal (ProposeWithdrawal) and reads the capabilities
// the account's owner granted the calling agent (AgentCapabilities) -- and
// nothing else.
//
// It exists because an agent tree may propose, but never sign or decide. The
// agent runtime (internal/agent, cmd/agent-worker) runs agents in-process,
// and its import boundary -- test/security/authority_boundary_test.go and the
// depguard rule agent-authority -- keeps internal/gates, internal/killswitch
// and internal/capital out of its source, so it cannot build a Service; nor
// may it hold one, because a Service also records the owner's decisions,
// signs through the delegated signer, submits and settles. NewAgentAdmission
// therefore builds the platform readers those packages provide exactly as
// every other binary builds them, wires the Service with them, and keeps that
// Service unexported: there is no method here that signs, executes, decides,
// grants, revokes, registers a venue or sets a policy, and no accessor for
// the Service, its signer, its venues or its seeds. What this admits is then
// carried on by code the agent cannot reach: the owner's decision path, the
// operator's execute-pending route and the advanced worker's delegation
// upkeep.
type AgentAdmission struct {
	svc *Service
}

// AdmissionConfig is what the agent runtime's composition root supplies.
// Everything else the Service needs is built here with the platform defaults
// cmd/api and cmd/advanced-worker use: the kill-switch reader
// (killswitch.Policy{}), the capability-gate checker over the deployment's
// enabled-capability list, the capital service on the outbox, Risk Kernel V2
// with its default policy, the ledger, the price store, the identity rule of
// D-055, the compliance profiles, reconciliation, the account and asset
// repositories, and the default signing policy.
type AdmissionConfig struct {
	DB          *db.DB
	Clock       clock.Clock
	Environment config.Environment
	// Host is the sandbox chain the capabilities act on (sandboxchain.Chain).
	Host ChainHost
	// EnabledCapabilities is the deployment's enabled-capability list
	// (CP_API_ENABLED_CAPABILITIES): a gate that is not on it is inactive.
	// Names are matched case-insensitively and an unknown name enables
	// nothing, as cmd/api reads the same list.
	EnabledCapabilities []string
	// Router is the legal router; nil is the conservative policy.
	Router *legalrouter.Router
	// BuildVersion is recorded on audit events and journal transactions;
	// empty is config.BuildVersion.
	BuildVersion string
	// SignerSeed is the delegated signer's master seed
	// (sandboxchain.Seed.Derive(sandboxchain.PurposeDelegatedSigner)). The
	// admission path needs the delegated wallet's address, which only the
	// signer derives; the signer it builds is never reachable from here.
	SignerSeed [32]byte
	// VenueSeeds are the co-signer seeds of the venues this deployment
	// co-signs for, by venue name (sandboxchain.Seed.VenueSeed(name)). Risk
	// V2 counts a venue healthy only when it can co-sign, so a venue missing
	// here is refused, never admitted.
	VenueSeeds map[string][devnet.SeedSize]byte
}

// NewAgentAdmission builds the Service from c and returns the facade over it.
func NewAgentAdmission(c AdmissionConfig) (*AgentAdmission, error) {
	if c.DB == nil || c.Clock == nil || !c.Environment.IsValid() || c.Host == nil {
		return nil, errs.New(errs.CodeInternal, "delegation: the agent admission needs the database, a clock, a valid environment and the sandbox chain")
	}
	router := c.Router
	if router == nil {
		var err error
		if router, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "delegation: the conservative legal router")
		}
	}
	build := c.BuildVersion
	if build == "" {
		build = config.BuildVersion
	}
	enabled := map[gates.Capability]bool{}
	for _, raw := range c.EnabledCapabilities {
		if name := gates.Capability(strings.ToUpper(strings.TrimSpace(raw))); name.Valid() {
			enabled[name] = true
		}
	}
	checker, err := gates.NewChecker(string(c.Environment), func(cp gates.Capability) bool { return enabled[cp] }, c.Clock)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "delegation: the gate checker")
	}
	signer, err := NewDelegatedSigner(c.SignerSeed, SignerConfig{Environment: c.Environment, Clock: c.Clock,
		Policy: DefaultSigningPolicy(), Fees: HostFees(c.Host)})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(c.VenueSeeds))
	for name := range c.VenueSeeds {
		names = append(names, name)
	}
	sort.Strings(names)
	venues := make([]*VenueSigner, 0, len(names))
	for _, name := range names {
		venues = append(venues, NewVenueSigner(c.VenueSeeds[name]))
	}
	outbox := event.NewOutbox(c.Clock)
	writer := audit.NewWriterWithBuildVersion(build)
	kills := killswitch.NewChecker(killswitch.Policy{})
	svc, err := New(Deps{
		DB: c.DB, Clock: c.Clock, Environment: c.Environment, Host: c.Host,
		Intents: econ.NewIntentStore(c.Clock, outbox, writer), Effects: econ.NewEffectStore(c.Clock, outbox, writer),
		Authorizer: econ.NewAuthorizer(checker, kills), Kills: kills, Ledger: ledger.NewService(c.Clock, build),
		Capital: capital.NewService(c.Clock, capital.OutboxEmitter{Outbox: outbox, Clock: c.Clock}),
		Risk:    risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router,
		Prices: valuation.NewPriceStore(c.Clock), Identity: compliance.NewAccountVerification(c.Clock),
		Profiles: compliance.NewRepository(writer), Reconciliation: reconciliation.NewRepository(c.Clock, outbox, writer),
		Audit: writer, Accounts: accounts.NewRepository(), Assets: assets.NewRepository(), Signer: signer, Venues: venues,
	})
	if err != nil {
		return nil, err
	}
	return &AgentAdmission{svc: svc}, nil
}

// RequestSwap admits the calling agent's delegated swap: Service.RequestSwap,
// in the caller's transaction, under the AGENT principal in ctx. Nothing is
// signed; a refusal is recorded and returned in the ticket.
func (a *AgentAdmission) RequestSwap(ctx context.Context, tx pgx.Tx, req SwapRequest) (SwapTicket, error) {
	return a.svc.RequestSwap(ctx, tx, req)
}

// ProposeWithdrawal records the calling agent's withdrawal proposal:
// Service.ProposeWithdrawal, in the caller's transaction, under the AGENT
// principal in ctx. A person decides it unless the account's policy
// auto-approves it in a test environment; nothing is signed or submitted.
func (a *AgentAdmission) ProposeWithdrawal(ctx context.Context, tx pgx.Tx, req ProposalRequest) (WithdrawalTicket, error) {
	return a.svc.ProposeWithdrawal(ctx, tx, req)
}

// AgentCapabilities returns the capabilities the account's owner granted the
// calling agent on account, newest first, whatever their status: the AGENT
// principal in ctx must be bound to the account and be one of its agents,
// and another agent's grants are never returned. Whether one may be used now
// is Capability.UsableAt's to say, and every admission measures it again.
func (a *AgentAdmission) AgentCapabilities(ctx context.Context, q db.Querier, account accounts.AccountID) ([]Capability, error) {
	_, agent, err := a.svc.agentFor(ctx, q, account)
	if err != nil {
		return nil, err
	}
	all, err := a.svc.ListCapabilities(ctx, q, account)
	if err != nil {
		return nil, err
	}
	out := make([]Capability, 0, len(all))
	for _, c := range all {
		if strings.EqualFold(c.Terms.AgentID, agent.ID) {
			out = append(out, c)
		}
	}
	return out, nil
}
