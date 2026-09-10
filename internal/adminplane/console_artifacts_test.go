package adminplane

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The operator console's generated documents, on both sides of its build.
//
// TestAuthorityGolden and TestDecisionVectorsGolden keep apps/admin/src/generated
// equal to the Go tables. That closes one gap and leaves another: the console
// does not compile those documents in, it fetches them at runtime from
// apps/admin/dist/generated, which `node build.mjs` copies from src. A dist/
// tree that predates a regeneration is a console rendering an authority model
// nobody holds — and it fails silently, because a stale document parses and
// validates perfectly. It has happened: a dist/generated/authority.json with
// 9 action kinds and 10 capabilities sat beside a src/ with 20 of each.
//
// build.mjs now verifies the copy it just made, which catches a partial build.
// This test catches the other case: a dist/ left behind by an older build that
// nobody has rerun. It is deliberately a skip rather than a failure when dist/
// does not exist — that tree is .gitignored, so CI and a fresh clone have none,
// and demanding one would make this test a build step.
const (
	consoleSrcGenerated  = "../../apps/admin/src/generated"
	consoleDistGenerated = "../../apps/admin/dist/generated"
)

func TestConsoleBuiltArtifactsMatchTheirSource(t *testing.T) {
	entries, err := os.ReadDir(consoleSrcGenerated)
	require.NoError(t, err, "the console's generated documents must exist; regenerate with -update-authority -update-vectors")
	require.NotEmpty(t, entries, "src/generated is empty")

	if _, err := os.Stat(consoleDistGenerated); os.IsNotExist(err) {
		t.Skip("apps/admin/dist does not exist here; nothing has been built to go stale")
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(consoleSrcGenerated, name))
			require.NoError(t, err)
			got, err := os.ReadFile(filepath.Join(consoleDistGenerated, name))
			if os.IsNotExist(err) {
				t.Fatalf("dist/generated/%s is missing from a built console; rebuild with `pnpm --filter @controlplane/admin build`", name)
			}
			require.NoError(t, err)
			if !bytes.Equal(want, got) {
				t.Fatalf("dist/generated/%s is stale (%d bytes built, %d bytes generated); rebuild with `pnpm --filter @controlplane/admin build`",
					name, len(got), len(want))
			}
		})
	}
}
