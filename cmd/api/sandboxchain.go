package main

import (
	"context"
	"fmt"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/sandboxchain"
)

// openSandboxChain opens the persistent simulated chain host the sandbox
// capabilities run on, with every platform key derived from the configured
// seed (internal/sandboxchain). It returns (nil, zero seed, nil) when no
// sandbox chain is configured: there is no default seed, and the capabilities
// that need the chain stay unwired. The seed is resolved through the API's own
// secret resolver and is never logged; a seed that does not match the
// network's recorded history fails here, because the host refuses a genesis
// written under another attester.
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
