package security

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// F-65 left a bridge unbuilt, on purpose, and this is the tripwire on that
// decision.
//
// Two kill switches reach nothing. `MODEL_DISABLE` can never match, because no
// production code sets `ModelID` on a `killswitch.Action` and the model-call
// path is architecturally forbidden from consulting a kill switch at all.
// `AGENT_PAUSE` writes `kill_switches` while every agent-runtime pause check
// reads `agent_pauses`. `agent.KillSwitchMirror` exists to join them and is
// implemented by nothing.
//
// The finding records why the bridge was NOT built: the agent runtime is inert.
// `agent.NewLifecycle` and `agent.NewEmitter` have no production callers and
// `cmd/agent-worker`'s `EmitterFor` returns UNSUPPORTED, so there is no running
// agent for either switch to fail to stop. Wiring a control into a subsystem
// that does not run would produce the thing this register keeps removing: a
// control only a test can reach.
//
// That reasoning is sound and it rests on a fact about the code, which nothing
// was watching. **The day the agent runtime becomes reachable is exactly the day
// nobody re-reads a four-month-old finding.** So this test watches the fact:
// when the runtime acquires a production caller, this fails and says what is now
// owed.
//
// It is deliberately not a test of the bridge. There is nothing to test yet.
// It is a test of the premise the deferral rests on.
func TestDeferredBridge_TheAgentRuntimeIsStillInert(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	// The two constructors whose absence from production is F-65's premise.
	callers := productionCallersOf(t, root, regexp.MustCompile(`\bagent\.(NewLifecycle|NewEmitter)\b`))
	assert.Emptyf(t, callers, `the agent runtime has production callers now: %v

F-65 deferred the AGENT_PAUSE / MODEL_DISABLE bridge because nothing runs an
agent, so neither switch had anything to fail to stop. That is no longer true,
and two things are now owed:

  1. agent.KillSwitchMirror, or an operator path that writes agent_pauses, so
     that activating AGENT_PAUSE actually pauses an agent.
  2. MODEL_DISABLE needs a decision of its own: it cannot be fixed at the call
     site, because internal/killswitch is in forbiddenForAgents and the model
     path may not consult it. Either the switch stops claiming to gate model
     calls, or the gate moves somewhere the agent tree may reach.

Until then the runbooks describe a control that does not work, on the day an
operator most needs it to.`, callers)

	// The second half of the premise, checked separately so that one changing
	// without the other is still visible.
	worker := filepath.Join(root, "cmd", "agent-worker")
	if _, err := os.Stat(worker); err == nil {
		body := readTree(t, worker)
		assert.Containsf(t, body, "CodeUnsupported",
			"cmd/agent-worker no longer returns UNSUPPORTED from EmitterFor; see the message above -- F-65's bridge is owed")
	}
}

// productionCallersOf returns the non-test files under internal/ and cmd/ whose
// text matches re. Text rather than a type-checked search, deliberately: this
// has to keep working when the thing it looks for does not exist yet.
func productionCallersOf(t *testing.T, root string, re *regexp.Regexp) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				// The package itself is where the constructors live, and its own
				// tests are not production callers.
				if name == "agent" && filepath.Base(filepath.Dir(path)) == "internal" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if re.Match(b) {
				rel, _ := filepath.Rel(root, path)
				out = append(out, filepath.ToSlash(rel))
			}
			return nil
		})
		require.NoError(t, err)
	}
	return out
}

func readTree(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		sb.Write(b)
		return nil
	})
	require.NoError(t, err)
	return sb.String()
}
