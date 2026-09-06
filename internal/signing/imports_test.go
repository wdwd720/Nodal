package signing_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAgentBoundaryNeverImportsSigning scans the agent-facing trees
// (cmd/agent-worker, internal/agent, internal/strategy — any that exist) and
// fails if any Go file imports internal/signing or internal/wallet
// (CONVENTIONS non-negotiable 9; AGENT_RUNTIME.md). Trees that do not exist
// yet are skipped by name so the test starts guarding them the moment they
// appear.
func TestAgentBoundaryNeverImportsSigning(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	forbidden := []string{
		"github.com/nodal/controlplane/internal/signing",
		"github.com/nodal/controlplane/internal/wallet",
	}
	trees := []string{"cmd/agent-worker", "internal/agent", "internal/strategy"}
	scanned := 0
	for _, tree := range trees {
		dir := filepath.Join(root, tree)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			t.Logf("%s does not exist yet; nothing to scan", tree)
			continue
		}
		fset := token.NewFileSet()
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			scanned++
			for _, imp := range f.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					continue
				}
				for _, bad := range forbidden {
					if p == bad || strings.HasPrefix(p, bad+"/") {
						t.Errorf("%s imports %s: agent code must never reach the signing boundary", path, p)
					}
				}
			}
			return nil
		})
		require.NoError(t, err)
	}
	t.Logf("scanned %d files", scanned)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "go.mod not found above %s", dir)
		dir = parent
	}
}
