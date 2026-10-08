package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/legalrouter"
)

// envLegalPolicy is the API's legal policy variable, which the worker reads
// too: an agent's delegated request is routed against the same determination
// as the owner's decision on it.
const envLegalPolicy = "CP_API_LEGAL_POLICY"

// The sandbox chain's variables. The worker reads the network and nothing
// else: the seed's reference is for the binaries that sign on the chain
// (cmd/api, cmd/advanced-worker), and this one never reads it.
const (
	envSandboxNetwork = "CP_SANDBOX_DEVNET_NETWORK"
	envSandboxSeedRef = "CP_SANDBOX_DEVNET_SEED_REF"
)

// delegatedNotWired is what every run is told, and what the worker logs,
// when no sandbox chain is configured.
const delegatedNotWired = "no sandbox chain is configured (CP_SANDBOX_DEVNET_NETWORK is empty): agents are offered no delegated tool"

// sandboxNetworkOnly reads the sandbox chain's network from the environment
// and returns it with a lookup that shows the configuration loader neither
// sandbox variable. The loader ties the network to the seed's reference --
// every other binary that opens the chain signs on it -- and this one must
// hold no seed: so the loader never sees the reference, and the network,
// all the worker needs to read the chain, is read here. seedOffered reports
// that the environment carries the reference anyway; its value is never
// kept.
func sandboxNetworkOnly(lookup func(string) (string, bool)) (network string, seedOffered bool, masked func(string) (string, bool)) {
	if v, ok := lookup(envSandboxNetwork); ok {
		network = strings.TrimSpace(v)
	}
	if v, ok := lookup(envSandboxSeedRef); ok && strings.TrimSpace(v) != "" {
		seedOffered = true
	}
	return network, seedOffered, func(k string) (string, bool) {
		if k == envSandboxNetwork || k == envSandboxSeedRef {
			return "", false
		}
		return lookup(k)
	}
}

// loadWorkerConfig is the worker's configuration: the CP_* configuration
// loaded for this binary without the sandbox variables, the sandbox chain's
// network, and whether the environment offered the seed's reference.
func loadWorkerConfig(ctx context.Context, lookup func(string) (string, bool)) (*config.Config, string, bool, error) {
	network, seedOffered, masked := sandboxNetworkOnly(lookup)
	cfg, err := config.Load(ctx, config.ServiceAgentWorker, masked)
	if err != nil {
		return nil, "", false, err
	}
	return cfg, network, seedOffered, nil
}

// wireDelegation builds the agent's side of delegated authority when the
// sandbox chain is configured, and only then: the admission facade
// (delegation.NewAgentAdmission), the one piece of delegated authority an
// agent tree may hold -- it admits an agent's swap request, records its
// withdrawal proposal and reads its capabilities, and can do nothing else.
//
// What it is given is all this binary holds of delegated authority: the
// deployment's settings and the sandbox chain opened for reading, without
// its seed (openChainReader). No seed is resolved, no key is derived, and no
// signer or venue co-signer exists in this process: the facade plans from
// the delegated wallet key the owner's grant registered in cmd/api, and
// counts a venue able to co-sign from its registration, and the binaries
// that sign check the real keys again before anything is signed.
// Unconfigured, it returns no facade and the reason, and the runtime tells
// every run so.
func wireDelegation(ctx context.Context, cfg *config.Config, network string, database *db.DB, clk clock.Clock) (*delegation.AgentAdmission, string, error) {
	if network == "" {
		return nil, delegatedNotWired, nil
	}
	host, err := openChainReader(ctx, database, clk, cfg.Env, network)
	if err != nil {
		return nil, "", err
	}
	router, err := legalRouterFor(cfg.Env, cfg.API.LegalPolicy)
	if err != nil {
		return nil, "", err
	}
	admission, err := delegation.NewAgentAdmission(delegation.AdmissionConfig{
		Clock: clk, Environment: cfg.Env, Host: host,
		EnabledCapabilities: capabilityNames(cfg.API.EnabledCapabilities), Router: router, BuildVersion: cfg.BuildVersion,
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

// openChainReader opens the sandbox chain's network for reading, without its
// seed. The host's configuration is the standard one (devnethost's default
// parameters, as sandboxchain.Open builds it) naming the bridge attester's
// public key that the network's GENESIS recorded -- the one key the
// configuration names, and only its public half. The host replays and
// verifies the whole recorded history, exactly as every other binary's host
// does, over a history that refuses every append (readOnlyHistory): it
// answers reads and can write nothing -- no genesis, no submission, no
// block, no faucet top-up, no fault. A network with no recorded history is
// refused: its genesis is written by a binary that holds the seed (cmd/api,
// cmd/advanced-worker).
func openChainReader(ctx context.Context, database *db.DB, clk clock.Clock, env config.Environment, network string) (*devnethost.Host, error) {
	if err := devnethost.DefaultConfig(network, env, devnet.PublicKey{}).Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", envSandboxNetwork, err)
	}
	attester, err := recordedAttester(ctx, database, network)
	if err != nil {
		return nil, err
	}
	host, err := devnethost.Open(ctx, readOnlyHistory{devnethost.NewPgStore(database)}, devnethost.DefaultConfig(network, env, attester), clk)
	if err != nil {
		return nil, fmt.Errorf("sandbox chain %s: %w", network, err)
	}
	return host, nil
}

// readOnlyHistory is the worker's view of the sandbox chain's recorded
// history: it reads every event and appends none.
type readOnlyHistory struct{ devnethost.Store }

// Exclusive refuses: the history is written only by the binaries that hold
// the sandbox chain's seed.
func (readOnlyHistory) Exclusive(context.Context, string, uint64, func([]devnethost.Event) (*devnethost.Event, error)) error {
	return errs.New(errs.CodeForbidden, "agent-worker: the sandbox chain is read-only here; its history is written by the binaries that hold its seed")
}

// genesisSQL reads a network's GENESIS document: the chain configuration its
// history was produced under. Read-only.
const genesisSQL = `SELECT args::text FROM devnet_events WHERE network = $1 AND seq = 1 AND kind = 'GENESIS'`

// recordedAttester is the bridge attester's public key the network's
// GENESIS recorded, which every chain of a standard host names alike. The
// host that opens with it compares its whole configuration with the recorded
// genesis, byte for byte, and refuses any difference -- so a genesis read
// wrongly here fails closed.
func recordedAttester(ctx context.Context, q db.Querier, network string) (devnet.PublicKey, error) {
	var doc string
	if err := q.QueryRow(ctx, genesisSQL, network).Scan(&doc); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return devnet.PublicKey{}, fmt.Errorf("sandbox chain %s has no recorded history: a binary that holds its seed "+
				"(cmd/api, cmd/advanced-worker) opens it, and this one only reads it", network)
		}
		return devnet.PublicKey{}, fmt.Errorf("sandbox chain %s: read its genesis: %w", network, err)
	}
	var genesis struct {
		Chains []struct {
			BridgeAuthority string `json:"bridge_authority"`
		} `json:"chains"`
	}
	if err := json.Unmarshal([]byte(doc), &genesis); err != nil || len(genesis.Chains) == 0 {
		return devnet.PublicKey{}, fmt.Errorf("sandbox chain %s: its genesis does not describe its chains", network)
	}
	authority := genesis.Chains[0].BridgeAuthority
	for _, c := range genesis.Chains[1:] {
		if c.BridgeAuthority != authority {
			return devnet.PublicKey{}, fmt.Errorf("sandbox chain %s: its chains name different bridge attesters, which no standard host does", network)
		}
	}
	if authority == "" {
		return devnet.PublicKey{}, nil
	}
	key, err := devnet.ParsePublicKey(authority)
	if err != nil {
		return devnet.PublicKey{}, fmt.Errorf("sandbox chain %s: its genesis names no valid bridge attester", network)
	}
	return key, nil
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
