package delegationtest

import (
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
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

// Platform is one test deployment of delegated authority as the platform
// holds it: the deployment's settings, the database and the chain it
// writes, and the keys only the platform holds -- the delegated signer's
// master seed and the venues' co-signer seeds. The agent's side of the same
// deployment is Admission(): the settings and a read of the chain, without
// the database handle and without a key.
type Platform struct {
	DB                  *db.DB
	Clock               clock.Clock
	Environment         config.Environment
	Host                delegation.ChainHost
	EnabledCapabilities []string
	Router              *legalrouter.Router
	BuildVersion        string
	// SignerSeed is the delegated signer's master seed.
	SignerSeed [32]byte
	// VenueSeeds are the co-signer seeds of the venues the platform
	// co-signs for, by venue name.
	VenueSeeds map[string][devnet.SeedSize]byte
}

// Admission is the agent's side of the deployment: what
// delegation.NewAgentAdmission takes. It carries no seed and no key.
func (p Platform) Admission() delegation.AdmissionConfig {
	return delegation.AdmissionConfig{Clock: p.Clock, Environment: p.Environment, Host: p.Host,
		EnabledCapabilities: p.EnabledCapabilities, Router: p.Router, BuildVersion: p.BuildVersion}
}

// NewService builds the whole Service the platform runs: the same platform
// readers delegation.NewAgentAdmission builds (killswitch.Policy{}, a gate
// checker over the enabled list, the capital service on the outbox, Risk V2
// with its default policy, the default signing policy), with the delegated
// signer and the venue co-signers derived from p's seeds, so the platform
// side of a test grants, decides and signs exactly what the agent side
// admitted. Its grants register the delegated wallet keys the agent's side
// reads.
func NewService(p Platform) (*delegation.Service, error) {
	if p.DB == nil || p.Clock == nil || !p.Environment.IsValid() || p.Host == nil {
		return nil, errs.New(errs.CodeInternal, "delegationtest: the service needs the database, a clock, a valid environment and the chain")
	}
	router := p.Router
	if router == nil {
		var err error
		if router, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
			return nil, err
		}
	}
	build := p.BuildVersion
	if build == "" {
		build = config.BuildVersion
	}
	enabled := map[gates.Capability]bool{}
	for _, raw := range p.EnabledCapabilities {
		if name := gates.Capability(strings.ToUpper(strings.TrimSpace(raw))); name.Valid() {
			enabled[name] = true
		}
	}
	checker, err := gates.NewChecker(string(p.Environment), func(cp gates.Capability) bool { return enabled[cp] }, p.Clock)
	if err != nil {
		return nil, err
	}
	signer, err := delegation.NewDelegatedSigner(p.SignerSeed, delegation.SignerConfig{Environment: p.Environment,
		Clock: p.Clock, Policy: delegation.DefaultSigningPolicy(), Fees: delegation.HostFees(p.Host)})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(p.VenueSeeds))
	for name := range p.VenueSeeds {
		names = append(names, name)
	}
	sort.Strings(names)
	venues := make([]*delegation.VenueSigner, 0, len(names))
	for _, name := range names {
		venues = append(venues, delegation.NewVenueSigner(p.VenueSeeds[name]))
	}
	outbox := event.NewOutbox(p.Clock)
	writer := audit.NewWriterWithBuildVersion(build)
	kills := killswitch.NewChecker(killswitch.Policy{})
	return delegation.New(delegation.Deps{
		DB: p.DB, Clock: p.Clock, Environment: p.Environment, Host: p.Host,
		Intents: econ.NewIntentStore(p.Clock, outbox, writer), Effects: econ.NewEffectStore(p.Clock, outbox, writer),
		Authorizer: econ.NewAuthorizer(checker, kills), Kills: kills, Ledger: ledger.NewService(p.Clock, build),
		Capital: capital.NewService(p.Clock, capital.OutboxEmitter{Outbox: outbox, Clock: p.Clock}),
		Risk:    risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router,
		Prices: valuation.NewPriceStore(p.Clock), Identity: compliance.NewAccountVerification(p.Clock),
		Profiles: compliance.NewRepository(writer), Reconciliation: reconciliation.NewRepository(p.Clock, outbox, writer),
		Audit: writer, Accounts: accounts.NewRepository(), Assets: assets.NewRepository(), Signer: signer, Venues: venues,
	})
}
