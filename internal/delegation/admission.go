package delegation

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
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
// signs through the delegated signer, co-signs as the venues, submits and
// settles. NewAgentAdmission therefore builds the platform readers those
// packages provide exactly as every other binary builds them and wires them
// into an admitter -- the half of a Service that admits, and a different
// type: there is no method here that signs, executes, decides, grants,
// revokes, registers a venue or sets a policy, and no accessor for anything
// it holds.
//
// It holds no private key material, and needs none. The delegated wallet it
// plans from is the public key registered when the account's owner granted a
// capability on the chain (Service.GrantCapability, in the process that
// holds the signer); an account with no key there is refused
// (ReasonSignerKeyNotRegistered). A venue counts as able to co-sign when its
// house book is ACTIVE and its co-signer's public key -- the address it was
// registered at -- is registered. The chain is only read. What this admits
// is carried on by code the agent cannot reach, in the processes that hold
// the delegated signer and the real venue co-signers and check them again
// before anything is signed: the owner's decision path and the operator's
// execute-pending route (cmd/api) and the advanced worker's delegation
// upkeep (cmd/advanced-worker).
type AgentAdmission struct {
	adm *admitter
}

// AdmissionConfig is what the agent runtime's composition root supplies: the
// deployment's settings and a view of the sandbox chain, and no key, signer
// or seed. Everything else the admitter needs is built here with the
// platform defaults cmd/api and cmd/advanced-worker use: the kill-switch
// reader (killswitch.Policy{}), the capability-gate checker over the
// deployment's enabled-capability list, the capital service on the outbox,
// Risk Kernel V2 with its default policy, the ledger, the price store, the
// identity rule of D-055, the compliance profiles, the account repository
// and the default signing policy (a policy, held to the slippage bound;
// never a key). The facade holds no database handle of its own: each call
// runs in the transaction, or on the querier, its caller hands it.
type AdmissionConfig struct {
	Clock       clock.Clock
	Environment config.Environment
	// Host is the sandbox chain the capabilities act on, read and never
	// written: its configuration (the network, each chain's fee) and each
	// chain's health. A *devnethost.Host satisfies it, including one opened
	// without the seed over a history that refuses every append
	// (cmd/agent-worker).
	Host ChainReader
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
}

// NewAgentAdmission builds the admitter from c and returns the facade over it.
func NewAgentAdmission(c AdmissionConfig) (*AgentAdmission, error) {
	if c.Clock == nil || !c.Environment.IsValid() || c.Host == nil {
		return nil, errs.New(errs.CodeInternal, "delegation: the agent admission needs a clock, a valid environment and the sandbox chain")
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
	signing := DefaultSigningPolicy()
	if err := signing.Validate(); err != nil {
		return nil, err
	}
	outbox := event.NewOutbox(c.Clock)
	writer := audit.NewWriterWithBuildVersion(build)
	kills := killswitch.NewChecker(killswitch.Policy{})
	return &AgentAdmission{adm: &admitter{
		clk: c.Clock, env: c.Environment, chain: c.Host, intents: econ.NewIntentStore(c.Clock, outbox, writer),
		authorizer: econ.NewAuthorizer(checker, kills), kills: kills, ledger: ledger.NewService(c.Clock, build),
		capital: capital.NewService(c.Clock, capital.OutboxEmitter{Outbox: outbox, Clock: c.Clock}),
		risk:    risk.NewStore(), policy: risk.DefaultAdvancedPolicy(), router: router,
		prices: valuation.NewPriceStore(c.Clock), identity: compliance.NewAccountVerification(c.Clock),
		profiles: compliance.NewRepository(writer), audit: writer, accounts: accounts.NewRepository(), signing: signing,
		params: Params{}.withDefaults(),
	}}, nil
}

// RequestSwap admits the calling agent's delegated swap (Service.RequestSwap
// is the same method), in the caller's transaction, under the AGENT
// principal in ctx. Nothing is signed; a refusal is recorded and returned in
// the ticket. The request's Action must be what the swap does (SwapAction):
// REDUCE_EXPOSURE only for paying a token for a USD-pegged stable unit,
// ACQUIRE_EXPOSURE for anything else. A label that is not is refused,
// recorded (ReasonActionMismatch), and the capability, the account's status
// and Risk V2 are only ever measured against what the swap does. An account
// with no delegated wallet key registered on the venue's chain is refused
// before anything is recorded (ReasonSignerKeyNotRegistered).
func (a *AgentAdmission) RequestSwap(ctx context.Context, tx pgx.Tx, req SwapRequest) (SwapTicket, error) {
	return a.adm.RequestSwap(ctx, tx, req)
}

// ProposeWithdrawal records the calling agent's withdrawal proposal
// (Service.ProposeWithdrawal is the same method), in the caller's
// transaction, under the AGENT principal in ctx. A person decides it unless
// the account's policy auto-approves it in a test environment; nothing is
// signed or submitted. An account with no delegated wallet key registered on
// the destination's chain is refused before anything is recorded
// (ReasonSignerKeyNotRegistered).
func (a *AgentAdmission) ProposeWithdrawal(ctx context.Context, tx pgx.Tx, req ProposalRequest) (WithdrawalTicket, error) {
	return a.adm.ProposeWithdrawal(ctx, tx, req)
}

// AgentCapabilities returns the capabilities the account's owner granted the
// calling agent on account, newest first, whatever their status: the AGENT
// principal in ctx must be bound to the account and be one of its agents,
// and another agent's grants are never returned. Whether one may be used now
// is Capability.UsableAt's to say, and every admission measures it again.
func (a *AgentAdmission) AgentCapabilities(ctx context.Context, q db.Querier, account accounts.AccountID) ([]Capability, error) {
	_, agent, err := a.adm.agentFor(ctx, q, account)
	if err != nil {
		return nil, err
	}
	all, err := a.adm.ListCapabilities(ctx, q, account)
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

// admittedWallet is the account's delegated wallet on chain as an admission
// plans from it: the public key registered when the account's owner granted
// a capability on that chain. Nothing is derived and nothing is registered
// here. An account with no key on chain is refused
// (ReasonSignerKeyNotRegistered): no grant has given its agents that chain.
// That the registered key is the one the delegated signer derives is the
// signer's to check, and it checks it again before it signs.
func admittedWallet(ctx context.Context, q db.Querier, account accounts.AccountID, chain devnet.ChainID) (devnet.PublicKey, error) {
	key, ok, err := registeredKey(ctx, q, account, chain)
	if err != nil {
		return devnet.PublicKey{}, err
	}
	if !ok {
		return devnet.PublicKey{}, errs.New(errs.CodeForbidden, "delegation: the account has no delegated wallet key on "+string(chain)+
			"; the key is registered when the account's owner grants a capability on that chain").
			WithField("reason", ReasonSignerKeyNotRegistered)
	}
	return key, nil
}

// venueCanCoSign is whether v counts as able to co-sign at admission, judged
// from what is registered and nothing else: its house book is ACTIVE, and
// its co-signer's public key -- the address it was registered at, which a
// venue keeps for ever -- is registered. A switch that disables the venue is
// the kill switches' to apply, and they are. Whether a process holds the
// co-signer for that key is checked where the co-signers are: a signing
// process refuses a swap at a venue it cannot co-sign for, before anything
// is signed (ReasonVenueSignerUnavailable).
func venueCanCoSign(ctx context.Context, q db.Querier, v Venue) (bool, error) {
	if v.Address.IsZero() {
		return false, nil
	}
	house, err := econ.NewRegistry().HouseAccount(ctx, q, v.HouseAccountID)
	if err != nil {
		return false, err
	}
	return house.Status == econ.HouseActive, nil
}
