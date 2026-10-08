package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/sandboxchain"
)

// envLegalPolicy is the API's legal policy variable, which the worker reads
// too: an agent's delegated request is routed against the same determination
// as the owner's decision on it.
const envLegalPolicy = "CP_API_LEGAL_POLICY"

// delegatedNotWired is what every run is told, and what the worker logs,
// when no sandbox chain is configured.
const delegatedNotWired = "no sandbox chain is configured (CP_SANDBOX_DEVNET_NETWORK is empty): agents are offered no delegated tool"

// registeredVenuesSQL lists the delegation venues by name. Read-only;
// delegation exports no venue lister (cmd/advanced-worker reads the same).
const registeredVenuesSQL = `SELECT name FROM delegation_venues ORDER BY name`

// wireDelegation builds the agent's side of delegated authority when the
// sandbox chain is configured, and only then: the admission facade
// (delegation.NewAgentAdmission), the one piece of delegated authority an
// agent tree may hold -- it admits an agent's swap request, records its
// withdrawal proposal and reads its capabilities, and can do nothing else.
//
// The keys derive from the sandbox seed exactly as cmd/api derives them: the
// delegated wallet signer from PurposeDelegatedSigner, one co-signer per
// registered venue from the venue's name. Their seeds go straight into the
// facade, which keeps them out of reach: this binary never holds a signer,
// and nothing it holds can sign, submit, settle or decide. Unconfigured, it
// returns no facade and the reason, and the runtime tells every run so.
func wireDelegation(ctx context.Context, cfg *config.Config, resolver config.Resolver, database *db.DB, clk clock.Clock) (*delegation.AgentAdmission, string, error) {
	chain, seed, err := openSandboxChain(ctx, cfg, resolver, database, clk)
	if err != nil {
		return nil, "", err
	}
	if chain == nil {
		return nil, delegatedNotWired, nil
	}
	venues, err := registeredVenueNames(ctx, database)
	if err != nil {
		return nil, "", err
	}
	seeds := make(map[string][32]byte, len(venues))
	for _, name := range venues {
		seeds[name] = seed.VenueSeed(name)
	}
	router, err := legalRouterFor(cfg.Env, cfg.API.LegalPolicy)
	if err != nil {
		return nil, "", err
	}
	admission, err := delegation.NewAgentAdmission(delegation.AdmissionConfig{
		DB: database, Clock: clk, Environment: cfg.Env, Host: chain.Host,
		EnabledCapabilities: capabilityNames(cfg.API.EnabledCapabilities), Router: router, BuildVersion: cfg.BuildVersion,
		SignerSeed: seed.Derive(sandboxchain.PurposeDelegatedSigner), VenueSeeds: seeds,
	})
	if err != nil {
		return nil, "", fmt.Errorf("delegated authority: %w", err)
	}
	return admission, "", nil
}

// delegatedFor is the runner's delegated-emitter factory over the facade, or
// nil when delegated authority is not wired.
func delegatedFor(admission *delegation.AgentAdmission, clk clock.Clock, pauses agent.PauseChecker) func(agent.Authority) (agent.DelegatedProposer, error) {
	if admission == nil {
		return nil
	}
	return func(a agent.Authority) (agent.DelegatedProposer, error) {
		return agent.NewDelegatedEmitter(agent.DelegatedEmitterDeps{
			Clock: clk, Authority: admission, Pauses: pauses, Budgets: agent.NewBudgetReader(),
		}, a)
	}
}

// openSandboxChain is cmd/api's: the persistent simulated chain host with
// every platform key derived from the configured seed, or nil when no
// sandbox chain is configured. The seed is resolved through the binary's own
// secret resolver and never logged.
func openSandboxChain(ctx context.Context, cfg *config.Config, resolver config.Resolver, database *db.DB, clk clock.Clock) (*sandboxchain.Chain, sandboxchain.Seed, error) {
	network := cfg.Sandbox.DevnetNetwork
	if network == "" {
		return nil, sandboxchain.Seed{}, nil
	}
	text, err := resolver.Resolve(ctx, cfg.Sandbox.DevnetSeedRef)
	if err != nil {
		return nil, sandboxchain.Seed{}, fmt.Errorf("sandbox chain seed: %w", err)
	}
	seed, err := sandboxchain.ParseSeed(text)
	if err != nil {
		return nil, sandboxchain.Seed{}, fmt.Errorf("sandbox chain seed: %w", err)
	}
	chain, err := sandboxchain.Open(ctx, database, clk, cfg.Env, network, seed)
	if err != nil {
		return nil, sandboxchain.Seed{}, fmt.Errorf("sandbox chain: %w", err)
	}
	return &chain, seed, nil
}

// registeredVenueNames reads the names of the registered delegation venues.
// A venue registered after the worker started is co-signable here only after
// a restart; until then Risk V2 counts it unhealthy and a swap at it is
// refused, never admitted.
func registeredVenueNames(ctx context.Context, q db.Querier) ([]string, error) {
	rows, err := q.Query(ctx, registeredVenuesSQL)
	if err != nil {
		return nil, fmt.Errorf("delegation venues: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("delegation venues: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("delegation venues: %w", err)
	}
	return out, nil
}

// capabilityNames splits the deployment's enabled-capability list; the
// facade reads it as cmd/api does (case-insensitive, unknown names enable
// nothing).
func capabilityNames(list string) []string {
	var out []string
	for _, raw := range strings.Split(list, ",") {
		if name := strings.TrimSpace(raw); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// legalRouterFor is cmd/api's: nil for the conservative policy, the
// development policy only outside production-like environments.
func legalRouterFor(env config.Environment, policy string) (*legalrouter.Router, error) {
	name := strings.ToUpper(strings.TrimSpace(policy))
	switch name {
	case "", "CONSERVATIVE":
		return nil, nil
	case "DEVELOPMENT":
		if env.IsProductionLike() {
			return nil, fmt.Errorf("%s=DEVELOPMENT: a development legal policy may not be loaded in %s", envLegalPolicy, env)
		}
		r, err := legalrouter.New(legalrouter.DevelopmentPolicy())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envLegalPolicy, err)
		}
		return r, nil
	default:
		return nil, fmt.Errorf("%s=%q: expected CONSERVATIVE or DEVELOPMENT", envLegalPolicy, name)
	}
}
