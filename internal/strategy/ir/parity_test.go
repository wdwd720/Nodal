package ir_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/strategy/ir"
)

// parityDir holds the fixtures shared with packages/strategy-sdk. Both
// languages read these exact files and must agree on expected_hash: if Go
// and TypeScript ever disagree, one of them would silently create a
// duplicate strategy version for a document the other considers identical,
// so the fixture is shared data rather than two hand-copied literals.
const parityDir = "testdata/parity"

// ParityFixture is the on-disk shape. Keep it in sync with
// packages/strategy-sdk/src/parity.test.ts, which decodes the same files.
type ParityFixture struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	ExpectedHash string          `json:"expected_hash"`
	IR           json.RawMessage `json:"ir"`
}

// regenerateParity rewrites the fixtures from the Go implementation. Run
// with -run TestParity_Regenerate -parity-write when the IR shape changes
// on purpose; the hash constants in hash_test.go must move with it.
var regenerateParity = os.Getenv("IR_PARITY_WRITE") == "1"

func parityDocuments(t *testing.T) map[string]*ir.IR {
	t.Helper()
	docs := map[string]*ir.IR{
		"momentum": momentum(t),
		"wallet":   walletTrigger(t),
	}

	// A document exercising the escaping and ordering rules that are easiest
	// to get wrong across languages: unicode, a quote, a backslash, keys that
	// sort differently by insertion order, and a deep expression tree.
	tricky := momentum(t)
	tricky.Dependencies[0].Params = map[string]string{
		"zeta":       "last by key",
		"alpha":      "first by key",
		"unicode":    "café — naïve ☕",
		"quote":      `he said "hi"`,
		"backslash":  `a\b\c`,
		"slash":      "a/b/c",
		"tab_free":   "no control characters allowed",
		"emoji":      "🚀📈",
		"empty":      "",
		"digits_key": "12345",
	}
	tricky.Signals = append(tricky.Signals, ir.Signal{
		Name: "nested", Scale: 4, Rounding: "half_even",
		Expr: ir.Expr{Bin: &ir.BinOp{
			Op: ir.OpAdd, Scale: 4, Rounding: "half_even",
			L: ptrExpr(ir.Expr{Bin: &ir.BinOp{
				Op: ir.OpMul, Scale: 4, Rounding: "half_even",
				L: &ir.Expr{Signal: ref("ret_5m")},
				R: ptrExpr(constExpr(t, "-2.5000")),
			}}),
			R: ptrExpr(constExpr(t, "0.0001")),
		}},
	})
	tricky.Normalize()
	docs["tricky"] = tricky

	// A minimal document: every optional list empty, so both languages must
	// agree on which keys are present when nothing is set.
	minimal := &ir.IR{SchemaVersion: ir.SchemaVersion, StrategyID: fxStrategyID, Owner: ir.Owner{AccountID: fxAccountID, UserID: fxUserID}}
	minimal.Normalize()
	docs["minimal"] = minimal

	return docs
}

// TestParity_FixturesMatchGo is the Go half of the parity contract: each
// fixture's expected_hash must be what this implementation computes, both
// from the typed value and from the raw JSON in the file.
func TestParity_FixturesMatchGo(t *testing.T) {
	if regenerateParity {
		writeParityFixtures(t)
	}
	entries, err := os.ReadDir(parityDir)
	require.NoError(t, err, "parity fixtures are missing; regenerate with IR_PARITY_WRITE=1")
	require.NotEmpty(t, entries)

	seen := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		seen++
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(parityDir, e.Name()))
			require.NoError(t, err)

			var fx ParityFixture
			require.NoError(t, json.Unmarshal(raw, &fx))
			require.NotEmpty(t, fx.ExpectedHash, "fixture must state the expected hash")
			require.Len(t, fx.ExpectedHash, 64, "expected_hash is a hex sha256")

			// From the raw document bytes, which is what TypeScript sees.
			fromJSON, err := ir.SemanticHashOfJSON(fx.IR)
			require.NoError(t, err)
			assert.Equal(t, fx.ExpectedHash, hex.EncodeToString(fromJSON), "raw-JSON hash")

			// And from the decoded typed value, so a decode that loses a
			// field is caught too.
			doc, err := ir.Decode(fx.IR)
			require.NoError(t, err)
			fromTyped, err := ir.SemanticHash(doc)
			require.NoError(t, err)
			assert.Equal(t, fx.ExpectedHash, hex.EncodeToString(fromTyped), "typed hash")
		})
	}
	require.GreaterOrEqual(t, seen, 4, "expected the full parity set")
}

// TestParity_FixturesCoverTheGoldenHash ties the shared fixtures to the
// constant asserted in hash_test.go, so the two cannot drift apart.
func TestParity_FixturesCoverTheGoldenHash(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(parityDir, "momentum.json"))
	require.NoError(t, err)
	var fx ParityFixture
	require.NoError(t, json.Unmarshal(raw, &fx))
	assert.Equal(t, goldenMomentumHash, fx.ExpectedHash,
		"the momentum fixture is the golden hash the TypeScript SDK must reproduce")
}

// writeParityFixtures regenerates the shared files.
func writeParityFixtures(t *testing.T) {
	t.Helper()
	require.NoError(t, os.MkdirAll(parityDir, 0o755))
	docs := parityDocuments(t)

	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	sort.Strings(names)

	descriptions := map[string]string{
		"momentum": "Valid simple momentum strategy: the PART 170 reference case and the golden hash.",
		"wallet":   "Wallet-triggered mirror strategy: adds an ON_EVENT trigger and READ_ONCHAIN_DATA.",
		"tricky":   "Escaping and ordering torture case: unicode, quotes, backslashes, unsorted keys, nested arithmetic.",
		"minimal":  "Minimal normalized document: proves both languages agree on which keys exist when nothing is set.",
	}

	for _, name := range names {
		doc := docs[name]
		hash, err := ir.SemanticHash(doc)
		require.NoError(t, err)
		body, err := json.Marshal(doc)
		require.NoError(t, err)

		fx := ParityFixture{
			Name:         name,
			Description:  descriptions[name],
			ExpectedHash: hex.EncodeToString(hash),
			IR:           body,
		}
		out, err := json.MarshalIndent(fx, "", "  ")
		require.NoError(t, err)
		out = append(out, '\n')
		require.NoError(t, os.WriteFile(filepath.Join(parityDir, name+".json"), out, 0o644))
		t.Logf("wrote %s/%s.json hash=%s", parityDir, name, fx.ExpectedHash)
	}
}
