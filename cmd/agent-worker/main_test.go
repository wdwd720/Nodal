package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
)

// exampleEnv is the documented example environment with overrides; a value
// of "-" removes the variable.
func exampleEnv(t *testing.T, overrides map[string]string) func(string) (string, bool) {
	t.Helper()
	env, err := config.ParseDotEnv(strings.NewReader(config.ExampleEnv()))
	require.NoError(t, err)
	for k, v := range overrides {
		if v == "-" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	return config.LookupFromMap(env)
}

func TestRun_UsageErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"everything"}, {"run", "extra"}, {"calibrate", "only-one"}} {
		var out, errOut bytes.Buffer
		assert.Equal(t, exitUsage, run(args, exampleEnv(t, nil), &out, &errOut), "%v", args)
	}
	var out, errOut bytes.Buffer
	require.Equal(t, exitOK, run([]string{"help"}, exampleEnv(t, nil), &out, &errOut))
	assert.Contains(t, out.String(), "CP_SANDBOX_DEVNET_NETWORK", "the usage says what wires delegated authority")
}

// TestWireDelegationOffersNothingWithoutASandboxChain: unconfigured, the
// worker builds no facade, touches no database, and says why -- and the
// runner gets no delegated factory.
func TestWireDelegationOffersNothingWithoutASandboxChain(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: config.EnvTest}
	admission, reason, err := wireDelegation(context.Background(), cfg, "", nil, clock.NewFake(time.Now()))
	require.NoError(t, err)
	assert.Nil(t, admission)
	assert.Equal(t, delegatedNotWired, reason)
	assert.Contains(t, reason, "CP_SANDBOX_DEVNET_NETWORK")
	assert.Nil(t, delegatedFor(nil, clock.NewFake(time.Now()), nil), "no facade, no delegated tool")
}

// TestTheWorkersConfigurationHoldsNoSeed: the worker's configuration takes
// the sandbox chain's network and never its seed. With the published example
// environment, which carries both, the configuration loader is never shown
// the seed's reference, nothing the worker loads holds it, and the worker
// knows only that it was offered one (to warn). With the network alone the
// worker's configuration loads -- where the API's, which signs on the chain,
// is refused without the seed.
func TestTheWorkersConfigurationHoldsNoSeed(t *testing.T) {
	t.Parallel()
	example, err := config.ParseDotEnv(strings.NewReader(config.ExampleEnv()))
	require.NoError(t, err)
	seedRef := example[envSandboxSeedRef]
	require.NotEmpty(t, seedRef, "the example environment offers the seed's reference")
	require.NotEmpty(t, example[envSandboxNetwork])

	var asked []string
	recorded := func(lookup func(string) (string, bool)) func(string) (string, bool) {
		return func(k string) (string, bool) {
			asked = append(asked, k)
			return lookup(k)
		}
	}
	_, _, masked := sandboxNetworkOnly(exampleEnv(t, nil))
	cfg, err := config.Load(context.Background(), config.ServiceAgentWorker, recorded(masked))
	require.NoError(t, err)
	assert.Contains(t, asked, envSandboxSeedRef, "the loader asks for the seed's reference")
	got, ok := masked(envSandboxSeedRef)
	assert.False(t, ok, "and is never given it")
	assert.Empty(t, got)
	assert.True(t, cfg.Sandbox.DevnetSeedRef.IsZero())
	assert.NotContains(t, fmt.Sprintf("%#v", *cfg), seedRef, "no part of the worker's configuration holds the seed")
	// The control: loaded the way every other binary loads it, the same
	// environment's configuration does hold it, where the check can see it.
	unmasked, err := config.Load(context.Background(), config.ServiceAgentWorker, exampleEnv(t, nil))
	require.NoError(t, err)
	require.Contains(t, fmt.Sprintf("%#v", *unmasked), seedRef)

	cfg, network, offered, err := loadWorkerConfig(context.Background(), exampleEnv(t, nil))
	require.NoError(t, err)
	assert.Equal(t, example[envSandboxNetwork], network, "the network is read beside the configuration")
	assert.True(t, offered, "the worker knows the environment offered a seed, to warn")
	assert.True(t, cfg.Sandbox.DevnetSeedRef.IsZero())

	noSeed := exampleEnv(t, map[string]string{envSandboxSeedRef: "-"})
	_, network, offered, err = loadWorkerConfig(context.Background(), noSeed)
	require.NoError(t, err, "the worker needs no seed")
	assert.Equal(t, example[envSandboxNetwork], network)
	assert.False(t, offered)
	_, err = config.Load(context.Background(), config.ServiceAPI, noSeed)
	require.Error(t, err, "the API, which signs on the chain, cannot open it without the seed")
	assert.Contains(t, err.Error(), "DevnetSeedRef")
}

// TestTheWorkerTakesNoSeed proves at the source level what the worker's
// wiring holds of the sandbox chain: never the package of its seeds and
// platform keys (internal/sandboxchain), never the seed's reference, a
// seed, a key derivation, a key pair or a signer constructor -- and the one
// secret its secret resolver resolves is its database URL: the resolver is
// used once, for that, and handed to nothing.
func TestTheWorkerTakesNoSeed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	forbidden := map[string]bool{
		"DevnetSeedRef": true, "Seed": true, "ParseSeed": true, "Derive": true, "DeriveKeys": true, "VenueSeed": true,
		"Keys": true, "Keypair": true, "NewKeypairFromSeed": true, "SeedSize": true, "PurposeDelegatedSigner": true,
		"NewDelegatedSigner": true, "NewVenueSigner": true, "DelegatedSigner": true, "VenueSigner": true,
	}
	fset := token.NewFileSet()
	var files []*ast.File
	resolvers := map[string]bool{} // the variables config.NewResolver's result is bound to
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files = append(files, f)
		for _, imp := range f.Imports {
			assert.NotEqual(t, `"github.com/nodal/controlplane/internal/sandboxchain"`, imp.Path.Value,
				"%s imports the package of the sandbox chain's seed and platform keys", name)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				assert.False(t, forbidden[x.Sel.Name], "%s names %s", fset.Position(x.Pos()), x.Sel.Name)
			case *ast.AssignStmt:
				for i, rhs := range x.Rhs {
					call, ok := rhs.(*ast.CallExpr)
					if !ok {
						continue
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "NewResolver" || i >= len(x.Lhs) {
						continue
					}
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "config" {
						if id, ok := x.Lhs[i].(*ast.Ident); ok {
							resolvers[id.Name] = true
						}
					}
				}
			}
			return true
		})
	}
	require.Len(t, resolvers, 1, "the worker builds one secret resolver")
	uses, resolved := 0, []string{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Resolve" {
					if id, ok := sel.X.(*ast.Ident); ok && resolvers[id.Name] && len(x.Args) == 2 {
						var arg strings.Builder
						require.NoError(t, format.Node(&arg, fset, x.Args[1]))
						resolved = append(resolved, arg.String())
					}
				}
			case *ast.Ident:
				if resolvers[x.Name] {
					uses++
				}
			}
			return true
		})
	}
	assert.Equal(t, []string{"cfg.Database.AppURL"}, resolved, "the one secret the worker resolves is its database URL")
	assert.Equal(t, 2, uses, "the resolver is bound once and used once, to resolve that URL, and handed to nothing")
}

// TestTheWorkerHoldsOnlyTheAdmissionFacade proves at the source level what
// the import boundary alone cannot: of delegated authority this binary names
// the admission facade and its configuration, and nothing else -- never the
// Service, a signer, a venue signer or their constructors.
func TestTheWorkerHoldsOnlyTheAdmissionFacade(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	used := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		local := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "github.com/nodal/controlplane/internal/delegation" {
				local = "delegation"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == local {
					used[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	var names []string
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"AdmissionConfig", "AgentAdmission", "NewAgentAdmission"}, names)
}

func TestLegalRouterForFollowsTheAPI(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"", "conservative", " CONSERVATIVE "} {
		r, err := legalRouterFor(config.EnvProd, policy)
		require.NoError(t, err)
		assert.Nil(t, r, "the conservative policy is the facade's default")
	}
	r, err := legalRouterFor(config.EnvLocal, "development")
	require.NoError(t, err)
	assert.NotNil(t, r)
	_, err = legalRouterFor(config.EnvProd, "DEVELOPMENT")
	require.Error(t, err, "a development legal policy is never loaded where it is production-like")
	_, err = legalRouterFor(config.EnvLocal, "lenient")
	require.Error(t, err)
}

func TestCapabilityNames(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"AGENT_SIGNING", "agent_withdrawals"}, capabilityNames(" AGENT_SIGNING ,, agent_withdrawals,"))
	assert.Empty(t, capabilityNames(""))
}
