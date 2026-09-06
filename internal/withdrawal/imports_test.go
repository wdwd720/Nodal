package withdrawal_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	modulePath     = "github.com/nodal/controlplane"
	withdrawalPath = modulePath + "/internal/withdrawal"
)

// agentFacingDirs are the packages that hold or serve agent authority
// (CONVENTIONS "Agent authority"). None of them may import the withdrawal
// boundary, directly or through a test.
var agentFacingDirs = []string{"internal/agent", "internal/strategy", "internal/model", "internal/prediction"}

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

// importsOf parses every Go file under dir (ImportsOnly) and returns
// file → imports.
func importsOf(t *testing.T, dir string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
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
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			out[path] = append(out[path], p)
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

// TestNoAgentPathImportsWithdrawal scans the agent-facing packages (when
// present) and every other package under internal/ and cmd/: no
// agent-facing file imports internal/withdrawal, and internal/withdrawal
// imports nothing agent-facing.
func TestNoAgentPathImportsWithdrawal(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, rel := range agentFacingDirs {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		for file, imports := range importsOf(t, dir) {
			for _, imp := range imports {
				require.NotEqual(t, withdrawalPath, imp, "%s imports the withdrawal boundary", file)
				require.False(t, strings.HasPrefix(imp, withdrawalPath+"/"), "%s imports the withdrawal boundary", file)
			}
		}
	}
	for file, imports := range importsOf(t, filepath.Join(root, "internal", "withdrawal")) {
		for _, imp := range imports {
			for _, rel := range agentFacingDirs {
				require.False(t, strings.HasPrefix(imp, modulePath+"/"+rel), "%s imports agent-facing package %s", file, imp)
			}
			for _, forbidden := range []string{"internal/execution", "internal/settlement", "internal/signing", "internal/wallet"} {
				require.False(t, strings.HasPrefix(imp, modulePath+"/"+forbidden), "%s imports %s", file, imp)
			}
		}
	}
	// Whole-tree sweep: any importer of the withdrawal boundary must live
	// outside the agent-facing directories.
	for _, top := range []string{"internal", "cmd"} {
		dir := filepath.Join(root, top)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		for file, imports := range importsOf(t, dir) {
			for _, imp := range imports {
				if imp != withdrawalPath && !strings.HasPrefix(imp, withdrawalPath+"/") {
					continue
				}
				relFile := filepath.ToSlash(strings.TrimPrefix(file, root+string(filepath.Separator)))
				for _, agentDir := range agentFacingDirs {
					require.False(t, strings.HasPrefix(relFile, agentDir+"/"), "%s imports the withdrawal boundary", relFile)
				}
			}
		}
	}
}
