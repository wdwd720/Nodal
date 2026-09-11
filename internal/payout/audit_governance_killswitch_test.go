package payout

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Adversarial audit (goal §54), area governance-sandbox-tier.
//
// F-gov-5. POLICY_AUTHORITY §2 declares the blocking matrix as a property of
// every guarded operation: "Every guarded operation declares an ActionClass",
// and the `WITHDRAW` class is blocked by `WITHDRAWALS_DISABLE`,
// `ACCOUNT_FREEZE` and `GLOBAL_NEW_RISK_KILL`.
//
// The conversion request is the withdrawal surface this productization built,
// and on the deployed tier it is the ONLY one that can be reached: the
// conservative and development legal policies both DENY `PAYOUT`, and
// `legalrouter.SandboxPolicy` is the one policy that permits it. So every
// exercise of the withdrawal boundary this system can actually perform happens
// on a sandbox tier, through `POST /v1/payouts` ->
// `httpapi.payoutsAdapter.Create` -> `compileRoute` -> `payout.Service.Create`.
//
// Nothing on that path consults a kill switch. `internal/payout` does not
// import `internal/killswitch` at all, so no switch an operator activates can
// stop a conversion request, a reservation or a submission to the provider.
// `internal/withdrawal` -- the older crypto-address path, which refuses
// everything today -- does consult one, which is the negative control below.
func TestAUDIT_NoKillSwitchCanReachTheConversionRequestPath(t *testing.T) {
	t.Parallel()
	const killswitchPkg = "github.com/nodal/controlplane/internal/killswitch"

	// Negative control: the older withdrawal domain does consult one, so an
	// absence below is a fact about this path and not about how the test looks.
	require.True(t, imports(packageImports(t, filepath.Join("..", "withdrawal")), killswitchPkg),
		"precondition: internal/withdrawal consults internal/killswitch")

	assert.True(t, imports(packageImports(t, "."), killswitchPkg),
		"internal/payout creates, reserves, submits and settles every conversion request and imports no "+
			"kill switch, so WITHDRAWALS_DISABLE, GLOBAL_NEW_RISK_KILL and ACCOUNT_FREEZE stop none of it; "+
			"POLICY_AUTHORITY §2 says the WITHDRAW class is blocked by all three")

	// And the composition above it does not supply one either: the three
	// httpapi files on the path name no switch.
	for _, f := range []string{
		filepath.Join("..", "httpapi", "wiring_native.go"),   // payoutsAdapter.Create
		filepath.Join("..", "httpapi", "wiring_compiler.go"), // compileRoute
		filepath.Join("..", "httpapi", "handlers_payout_cancel.go"),
	} {
		src, err := os.ReadFile(f) // #nosec G304 -- a fixed list of repository paths
		require.NoError(t, err)
		assert.True(t, strings.Contains(string(src), "killswitch"),
			"%s is on the conversion-request path and names no kill switch", filepath.Base(f))
	}
}

// packageImports returns every import path of the non-test Go files in dir.
func packageImports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		require.NoError(t, perr)
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			require.NoError(t, uerr)
			out = append(out, p)
		}
	}
	return out
}

// imports reports whether path is in the list.
func imports(list []string, path string) bool {
	for _, p := range list {
		if p == path {
			return true
		}
	}
	return false
}
