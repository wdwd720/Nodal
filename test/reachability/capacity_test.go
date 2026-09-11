package reachability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every capacity ceiling has a caller a deployment can reach (F-91).
//
// This is the same question the rest of this package asks, aimed at a control
// rather than at a method that moves money -- and it found the same answer.
// `CP_CAPACITY_MAX_ACCOUNTS` was read from the environment, validated, logged
// at startup as "launch-tier capacity ceilings in force", and enforced nowhere:
// `capacity.ActionOpenAccount` had no reference outside internal/capacity and
// its own tests, while accounts are auto-provisioned on first OIDC login. The
// 51st authenticated user got one, and so would the five-thousandth.
//
// A ceiling nobody asks about is not a ceiling. The number was in the
// configuration hash, in the blueprint and in the startup log, which is exactly
// what made it look enforced.
func TestEveryCapacityActionIsAskedAboutSomewhereReachable(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	// The actions capacity declares. Adding one to internal/capacity without a
	// caller fails here rather than shipping as a number in a log line.
	actions := declaredCapacityActions(t, root)
	require.NotEmpty(t, actions, "no capacity actions found; the scan is broken, not the code")

	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			callers := referencesOutsideCapacity(t, root, action)
			assert.NotEmptyf(t, callers,
				"capacity.%s is declared and nothing outside internal/capacity ever asks for it, "+
					"so the ceiling it names is configured, logged and unenforced", action)
		})
	}
}

// declaredCapacityActions reads the Action constants from the package source.
func declaredCapacityActions(t *testing.T, root string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash("internal/capacity/capacity.go")))
	require.NoError(t, err)
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Action") || !strings.Contains(line, "Action = ") {
			continue
		}
		out = append(out, strings.TrimSpace(strings.SplitN(line, " ", 2)[0]))
	}
	return out
}

// referencesOutsideCapacity returns the files naming capacity.<action> outside
// the declaring package and outside tests. A test caller is exactly what this
// check must not accept: it is what made the ceiling look reachable.
func referencesOutsideCapacity(t *testing.T, root, action string) []string {
	t.Helper()
	needle := "capacity." + action
	var found []string
	for _, tree := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".go") ||
				strings.HasSuffix(d.Name(), "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if strings.HasPrefix(filepath.ToSlash(rel), "internal/capacity/") {
				return nil
			}
			body, rerr := os.ReadFile(path) // #nosec G304 -- walking the repository
			if rerr != nil {
				return rerr
			}
			if strings.Contains(string(body), needle) {
				found = append(found, filepath.ToSlash(rel))
			}
			return nil
		})
		require.NoError(t, err)
	}
	return found
}
