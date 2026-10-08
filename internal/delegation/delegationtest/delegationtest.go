package delegationtest

import (
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/delegation"
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

// NewService builds the whole Service from the admission facade's
// configuration: the same platform readers (killswitch.Policy{}, a gate
// checker over the enabled list, the capital service on the outbox, Risk V2
// with its default policy, the default signing policy) and the same signer
// and venue co-signers, so the platform side of a test decides and signs
// exactly what the agent side admitted.
func NewService(c delegation.AdmissionConfig) (*delegation.Service, error) {
	if c.DB == nil || c.Clock == nil || !c.Environment.IsValid() || c.Host == nil {
		return nil, errs.New(errs.CodeInternal, "delegationtest: the service needs the database, a clock, a valid environment and the chain")
	}
	router := c.Router
	if router == nil {
		var err error
		if router, err = legalrouter.New(legalrouter.ConservativePolicy()); err != nil {
			return nil, err
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
		return nil, err
	}
	signer, err := delegation.NewDelegatedSigner(c.SignerSeed, delegation.SignerConfig{Environment: c.Environment,
		Clock: c.Clock, Policy: delegation.DefaultSigningPolicy(), Fees: delegation.HostFees(c.Host)})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(c.VenueSeeds))
	for name := range c.VenueSeeds {
		names = append(names, name)
	}
	sort.Strings(names)
	venues := make([]*delegation.VenueSigner, 0, len(names))
	for _, name := range names {
		venues = append(venues, delegation.NewVenueSigner(c.VenueSeeds[name]))
	}
	outbox := event.NewOutbox(c.Clock)
	writer := audit.NewWriterWithBuildVersion(build)
	kills := killswitch.NewChecker(killswitch.Policy{})
	return delegation.New(delegation.Deps{
		DB: c.DB, Clock: c.Clock, Environment: c.Environment, Host: c.Host,
		Intents: econ.NewIntentStore(c.Clock, outbox, writer), Effects: econ.NewEffectStore(c.Clock, outbox, writer),
		Authorizer: econ.NewAuthorizer(checker, kills), Kills: kills, Ledger: ledger.NewService(c.Clock, build),
		Capital: capital.NewService(c.Clock, capital.OutboxEmitter{Outbox: outbox, Clock: c.Clock}),
		Risk:    risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router,
		Prices: valuation.NewPriceStore(c.Clock), Identity: compliance.NewAccountVerification(c.Clock),
		Profiles: compliance.NewRepository(writer), Reconciliation: reconciliation.NewRepository(c.Clock, outbox, writer),
		Audit: writer, Accounts: accounts.NewRepository(), Assets: assets.NewRepository(), Signer: signer, Venues: venues,
	})
}
