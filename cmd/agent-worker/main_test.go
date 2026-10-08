package main

import (
	"bytes"
	"context"
	"go/ast"
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
// worker builds no facade, touches neither the secret resolver nor the
// database, and says why -- and the runner gets no delegated factory.
func TestWireDelegationOffersNothingWithoutASandboxChain(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: config.EnvTest}
	admission, reason, err := wireDelegation(context.Background(), cfg, nil, nil, clock.NewFake(time.Now()))
	require.NoError(t, err)
	assert.Nil(t, admission)
	assert.Equal(t, delegatedNotWired, reason)
	assert.Contains(t, reason, "CP_SANDBOX_DEVNET_NETWORK")
	assert.Nil(t, delegatedFor(nil, clock.NewFake(time.Now()), nil), "no facade, no delegated tool")
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
