package infra

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
)

// internal/config's requirements table is the single description of what a
// deployment needs. Three things depend on it and only on it: .env.example,
// scripts/configcheck, and the configuration hash /v1/version reports so an
// operator can prove which configuration a running binary loaded.
//
// A variable read straight from the environment by a binary is therefore
// invisible three times over. It is undocumented, a configuration check cannot
// see that it is missing or malformed, and -- the one that matters most -- it
// can be changed in a platform dashboard without changing the hash that exists
// to detect exactly that.
//
// This has happened six times. Two settlement-asset variables, discovered when
// a deployment passed every configuration check and then refused to start. The
// four transport rate limits, discovered when a test tried to compare the
// enforced limit against the declared one and found the deployment declared
// none. Then the enabled-capability list, which is condition 1 of the policy
// authority, plus the two funding-quote variables and the legal policy.
//
// Each was found by accident. This test is the one that does not need an
// accident: every CP_* name a binary mentions must be in the table.

var cpVarPattern = regexp.MustCompile(`"(CP_[A-Z0-9_]+)"`)

// exemptFromTheTable names variables that are deliberately not service
// configuration. Each is here with a reason, and the list is meant to stay
// short: an exemption is how the next one of these hides.
var exemptFromTheTable = map[string]string{
	"CP_DATABASE_APP_URL_PLAIN": "operator tooling reads it; no service does",
	"CP_BOOTSTRAP_ADMIN_URL":    "operator tooling reads it; no service does",
}

// undeclaredBacklog is every CP_* name a worker binary still reads directly.
//
// It is a frozen list, not an exemption. These are the same defect as the six
// above, in binaries this tier does not deploy -- the launch blueprint runs one
// web service -- and fixing them means touching seven binaries no running
// deployment can verify the change against. So they are written down instead,
// which makes the debt countable and stops it growing: a new one fails this
// test, and a name that leaves the tree fails it too.
var undeclaredBacklog = []string{
	"CP_AGENT_BATCH",
	"CP_AGENT_RESOLVE_INTERVAL",
	"CP_AGENT_TICK_INTERVAL",
	"CP_AUDIT_ARCHIVE_DIR",
	"CP_AUDIT_CHECKPOINT_INTERVAL",
	"CP_AUDIT_LOCAL_RETIRED_KEY_REFS",
	"CP_AUDIT_LOCAL_SIGNING_KEY_REF",
	"CP_AUDIT_MAX_LEAVES",
	"CP_AUDIT_RETIRED_SIGNING_KEY_IDS",
	"CP_AUDIT_REVOKED_SIGNING_KEY_IDS",
	"CP_AUDIT_SWEEP_EVERY",
	"CP_AUDIT_VERIFY_INTERVAL",
	"CP_CREDIT_SETTLEMENT_WINDOW",
	"CP_CREDIT_SWEEP_INTERVAL",
	"CP_EXECUTION_WORKER_CONCURRENCY",
	"CP_EXECUTION_WORKER_DRAIN_TIMEOUT",
	"CP_EXECUTION_WORKER_LEASE_TTL",
	"CP_EXECUTION_WORKER_OWNER",
	"CP_EXECUTION_WORKER_POLL_INTERVAL",
	"CP_INGEST_CONSUMER",
	"CP_INGEST_DATA_SOURCE",
	"CP_INGEST_FEATURE_LATENCY",
	"CP_INGEST_HEALTH_INTERVAL",
	"CP_INGEST_HEARTBEAT",
	"CP_INGEST_PIPELINE_LATENCY",
	"CP_INGEST_SILENCE_INTERVAL",
	"CP_INGEST_TOPIC",
	"CP_INGEST_VERIFY_INTERVAL",
	"CP_INGEST_VERIFY_LIMIT",
	"CP_INGEST_WALLETS",
	"CP_INGEST_WINDOW",
	"CP_RECONCILIATION_BATCH",
	"CP_RECONCILIATION_FULL_INTERVAL",
	"CP_RECONCILIATION_PERIODIC_INTERVAL",
	"CP_RECONCILIATION_VERIFY_INTERVAL",
	"CP_RELAY_WORKER_ALLOW_LOOPBACK_BUS",
	"CP_RELAY_WORKER_BATCH_SIZE",
	"CP_RELAY_WORKER_DRAIN_TIMEOUT",
	"CP_RELAY_WORKER_EXCLUSIVE",
	"CP_RELAY_WORKER_MAX_INTERVAL",
	"CP_RELAY_WORKER_POLL_INTERVAL",
	"CP_RELAY_WORKER_RETRY_BACKOFF_BASE",
	"CP_RELAY_WORKER_RETRY_BACKOFF_MAX",
	"CP_RELAY_WORKER_RUN_TIMEOUT",
	"CP_RELAY_WORKER_SAMPLE_INTERVAL",
	"CP_WORKFLOW_WORKER_DRAIN_TIMEOUT",
	"CP_WORKFLOW_WORKER_MAX_ACTIVITIES",
	"CP_WORKFLOW_WORKER_MAX_WORKFLOWS",
}

// TestConfigTable_EveryVariableABinaryReadsIsDeclared.
//
// cmd/api is what the launch tier runs, so its table is complete and stays
// complete. Everything else is the backlog above.
func TestConfigTable_EveryVariableABinaryReadsIsDeclared(t *testing.T) {
	t.Parallel()

	declared := map[string]bool{}
	for _, v := range config.Vars() {
		declared[v.Name] = true
	}
	require.NotEmpty(t, declared, "the configuration table is empty")

	found := map[string][]string{}
	roots := []string{"../../cmd"}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path) //nolint:gosec // G304: walking a fixed directory in the repository
			if rerr != nil {
				return rerr
			}
			for _, m := range cpVarPattern.FindAllStringSubmatch(string(b), -1) {
				found[m[1]] = append(found[m[1]], path)
			}
			return nil
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, found, "no CP_* names were found in cmd/, which means this test is not looking where it thinks")

	backlog := map[string]bool{}
	for _, n := range undeclaredBacklog {
		backlog[n] = true
	}

	var inAPI, newlyUndeclared []string
	mentioned := map[string]bool{}
	for name, files := range found {
		if declared[name] {
			continue
		}
		if _, ok := exemptFromTheTable[name]; ok {
			continue
		}
		mentioned[name] = true
		where := dedupe(files)
		if slices.ContainsFunc(where, func(p string) bool {
			return strings.Contains(filepath.ToSlash(p), "cmd/api/")
		}) {
			inAPI = append(inAPI, name)
			continue
		}
		if !backlog[name] {
			newlyUndeclared = append(newlyUndeclared, name+" ("+strings.Join(where, ", ")+")")
		}
	}
	sort.Strings(inAPI)
	sort.Strings(newlyUndeclared)

	assert.Empty(t, inAPI,
		"cmd/api is the binary this tier deploys, and these variables it reads are in no requirements "+
			"table -- so they are undocumented, invisible to scripts/configcheck, and absent from the "+
			"configuration hash that is supposed to prove which configuration is running: %v", inAPI)

	assert.Empty(t, newlyUndeclared,
		"these are new: a binary reads them and no requirements table declares them. Add them to "+
			"internal/config rather than to the backlog: %v", newlyUndeclared)

	// And the backlog never keeps names that are gone, because a stale entry
	// would silently excuse a future variable that happened to take the same
	// name.
	var stale []string
	for _, n := range undeclaredBacklog {
		if !mentioned[n] {
			stale = append(stale, n)
		}
	}
	sort.Strings(stale)
	assert.Empty(t, stale, "the backlog lists names no binary reads any more; delete them: %v", stale)
}

// TestConfigTable_NoExemptionIsStale keeps the escape hatch honest.
//
// An exemption outlives the thing it excused: the variable is removed, the
// entry stays, and years later a new variable takes the same name and is
// exempt for a reason nobody wrote about it. So every exemption has to still
// name something, and nothing that is in the table may also be exempt from it.
func TestConfigTable_NoExemptionIsStale(t *testing.T) {
	t.Parallel()

	declared := map[string]bool{}
	for _, v := range config.Vars() {
		declared[v.Name] = true
	}

	for name, reason := range exemptFromTheTable {
		assert.NotEmpty(t, reason, "%s is exempt for no stated reason", name)
		assert.False(t, declared[name],
			"%s is exempt AND declared; one of the two is wrong", name)
		assert.True(t, mentionedInRepo(t, name),
			"%s is exempt from the configuration table and nothing mentions it any more", name)
	}
}

// mentionedInRepo reports whether the name appears anywhere under scripts/ or
// cmd/, which is where a variable an exemption is protecting would live.
func mentionedInRepo(t *testing.T, name string) bool {
	t.Helper()
	found := false
	for _, root := range []string{"../../cmd", "../../scripts"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || found {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			b, rerr := os.ReadFile(path) //nolint:gosec // G304: walking fixed directories in the repository
			if rerr != nil {
				return rerr
			}
			if strings.Contains(string(b), name) {
				found = true
			}
			return nil
		})
		require.NoError(t, err)
	}
	return found
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
