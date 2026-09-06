package security

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sec "github.com/nodal/controlplane/internal/security"
)

const module = "github.com/nodal/controlplane/"

// forbiddenForAgents are the packages an untrusted proposal generator must
// never reach (PART 9, PART 63): signing, wallet access, admin control,
// production gates, withdrawal, and policy/authority mutation.
var forbiddenForAgents = []string{
	"internal/signing", "internal/wallet", "internal/admin", "internal/gates",
	"internal/withdrawal", "internal/killswitch",
}

// agentTrees are the source trees that run agent/strategy logic.
var agentTrees = []string{"internal/agent", "internal/strategy", "internal/model", "internal/prediction", "cmd/agent-worker"}

// signingImporters is the closed set of trees allowed to import the signing
// service (PART 95: isolated from everything but the execution worker).
var signingImporters = map[string]bool{"internal/signing": true, "internal/settlement": true, "cmd/execution-worker": true, "test/": true}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		require.NotEqual(t, filepath.Dir(dir), dir, "go.mod not found above %s", wd)
	}
}

// importsOf returns module-internal imports per Go file (tests included) under dir.
func importsOf(t *testing.T, root, dir string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	base := filepath.Join(root, filepath.FromSlash(dir))
	if _, err := os.Stat(base); err != nil {
		return out // tree does not exist yet: the rule holds vacuously and is re-checked when it appears
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(p, module) {
				rel, _ := filepath.Rel(root, path)
				out[filepath.ToSlash(rel)] = append(out[filepath.ToSlash(rel)], strings.TrimPrefix(p, module))
			}
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

// TestAgentTreesNeverImportAuthority: PART 9 enforced structurally, not by convention.
func TestAgentTreesNeverImportAuthority(t *testing.T) {
	root := repoRoot(t)
	for _, tree := range agentTrees {
		for file, imports := range importsOf(t, root, tree) {
			for _, imp := range imports {
				for _, bad := range forbiddenForAgents {
					assert.False(t, imp == bad || strings.HasPrefix(imp, bad+"/"), "%s imports %s (forbidden for agent trees)", file, imp)
				}
			}
		}
	}
}

// TestSigningImportedOnlyByExecutionBoundary: the signing *service*
// (internal/signing) is reachable from the execution worker and the settlement
// executor only. internal/signing/inspect is a pure, I/O-free library (the
// transaction inspector and its program allow-lists) and may be shared, e.g.
// by the wallet-provider adapter that configures signing policies.
func TestSigningImportedOnlyByExecutionBoundary(t *testing.T) {
	root := repoRoot(t)
	for _, tree := range []string{"internal", "cmd"} {
		for file, imports := range importsOf(t, root, tree) {
			for _, imp := range imports {
				if imp != "internal/signing" {
					continue
				}
				allowed := false
				for prefix := range signingImporters {
					if strings.HasPrefix(file, prefix) {
						allowed = true
					}
				}
				assert.True(t, allowed, "%s imports the signing service; only the execution boundary may", file)
			}
		}
	}
}

// TestAgentPrincipalPermissionSetIsClosed: whatever the matrix grows into, an
// AGENT principal can never satisfy a dual-control, kill, gate, withdrawal,
// ledger-correction, or risk-policy permission (PART 21, PART 60).
func TestAgentPrincipalPermissionSetIsClosed(t *testing.T) {
	agent := sec.AgentPrincipal("agent-1", "acct-1")
	now := agent.AuthTime
	never := []sec.Permission{
		sec.PermKillActivate, sec.PermKillRelease, sec.PermGatePropose, sec.PermGateApprove,
		sec.PermWithdrawalCreate, sec.PermWithdrawalApprove, sec.PermLedgerPostCorrection, sec.PermLedgerApproveCorrection,
		sec.PermRiskPolicyWrite, sec.PermEnvelopeAuthorityWrite, sec.PermEnvelopeApprove, sec.PermAccountFreeze,
		sec.PermProviderDisable, sec.PermProviderEnable, sec.PermInstrumentStatusWrite, sec.PermSessionRevokeAny,
		sec.PermBreakGlassRequest, sec.PermBreakGlassApprove, sec.PermAgentPromote, sec.PermAgentPromoteApprove, sec.PermTradeCreate,
	}
	for _, p := range never {
		assert.False(t, agent.Has(p, now), "agent must never hold %s", p)
	}
	for _, p := range sec.DualControlPermissions() {
		assert.False(t, agent.Has(p, now), "agent must never hold dual-control %s", p)
		for _, r := range sec.AllRoles() {
			if r != sec.RoleBreakGlass {
				assert.False(t, sec.RoleGrants(r, p), "standing role %s must not hold dual-control %s", r, p)
			}
		}
	}
	assert.ElementsMatch(t, sec.AgentPermissions(), []sec.Permission{
		sec.PermAgentRun, sec.PermPredictionCommit, sec.PermIntentCreateAgent, sec.PermAccountRead, sec.PermTradeRead, sec.PermStrategyRead,
	}, "the agent permission set is closed; extending it is a security decision")
}
