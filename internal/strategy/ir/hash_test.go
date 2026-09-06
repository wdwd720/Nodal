package ir_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/strategy/ir"
)

// goldenMomentumHash pins the semantic hash of the momentum fixture. It is
// the contract the TypeScript SDK must reproduce (testdata/parity) and the
// value strategy_versions.ir_hash dedupes on. A change here is a change to
// the hash algorithm or to the canonical document shape: both are breaking,
// both need a schema version bump, so this constant is deliberately hostile
// to casual edits.
const goldenMomentumHash = "8487051d87f34f11fc81bdab65b5baad3f6ec5cc77872331bfa3fa46f72513d5"

func semanticHashHex(t *testing.T, doc *ir.IR) string {
	t.Helper()
	h, err := ir.SemanticHash(doc)
	require.NoError(t, err)
	require.Len(t, h, 32, "semantic hash must be a sha256 digest")
	return hex.EncodeToString(h)
}

// TestSemanticHash_Golden is the stability anchor: the fixture's hash is a
// fixed constant, so any drift in field order, key names, number rendering
// or the exclusion list fails here rather than silently splitting one
// strategy into two versions.
func TestSemanticHash_Golden(t *testing.T) {
	t.Parallel()
	assert.Equal(t, goldenMomentumHash, semanticHashHex(t, momentum(t)))
}

// TestSemanticHash_DeterministicAcrossCalls: the same document hashes
// identically however many times it is asked, including concurrently. Map
// iteration order inside the document (dependency params) must not leak
// into the digest.
func TestSemanticHash_DeterministicAcrossCalls(t *testing.T) {
	t.Parallel()
	want := semanticHashHex(t, momentum(t))
	for i := 0; i < 1000; i++ {
		require.Equal(t, want, semanticHashHex(t, momentum(t)), "iteration %d", i)
	}

	var wg sync.WaitGroup
	got := make([]string, 64)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := ir.SemanticHash(momentum(t))
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = hex.EncodeToString(h)
		}(i)
	}
	wg.Wait()
	for i, g := range got {
		assert.Equal(t, want, g, "goroutine %d", i)
	}
}

// TestSemanticHash_StableAcrossProcesses re-runs the golden assertion in a
// fresh process. Go randomizes map iteration order per process, so a single
// process agreeing with itself proves less than it appears to.
func TestSemanticHash_StableAcrossProcesses(t *testing.T) {
	if os.Getenv("IR_HASH_CHILD") == "1" {
		// Child: print the hash and exit. Failures surface in the parent.
		h, err := ir.SemanticHash(momentum(t))
		require.NoError(t, err)
		_, _ = os.Stdout.WriteString("HASH=" + hex.EncodeToString(h) + "\n")
		return
	}
	want := semanticHashHex(t, momentum(t))
	for i := 0; i < 8; i++ {
		cmd := exec.Command(os.Args[0], "-test.run", "TestSemanticHash_StableAcrossProcesses", "-test.v")
		cmd.Env = append(os.Environ(), "IR_HASH_CHILD=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "child run %d: %s", i, out)
		var got string
		for _, line := range strings.Split(string(out), "\n") {
			if h, ok := strings.CutPrefix(strings.TrimSpace(line), "HASH="); ok {
				got = h
			}
		}
		require.NotEmpty(t, got, "child %d printed no hash: %s", i, out)
		assert.Equal(t, want, got, "child process %d disagrees", i)
	}
}

// TestSemanticHash_ExcludesProvenance: the hash covers meaning only. Two
// documents that differ solely in hash, version, build time or lineage are
// the same strategy, which is what makes an SDK document and a
// natural-language compilation dedupe (STRATEGY_IR.md §5).
func TestSemanticHash_ExcludesProvenance(t *testing.T) {
	t.Parallel()
	base := momentum(t)
	want := semanticHashHex(t, base)

	cases := map[string]func(*ir.IR){
		"hash":                    func(d *ir.IR) { d.Hash = ir.Hex{0xde, 0xad, 0xbe, 0xef} },
		"version":                 func(d *ir.IR) { d.Version = 99 },
		"built_at":                func(d *ir.IR) { d.BuiltAt = fxBuiltAt.Add(72 * time.Hour) },
		"lineage.source":          func(d *ir.IR) { d.Lineage.Source = ir.SourceNaturalLanguage },
		"lineage.source_hash":     func(d *ir.IR) { d.Lineage.SourceHash = ir.Hex{0x99} },
		"lineage.compile_attempt": func(d *ir.IR) { d.Lineage.CompileAttemptID = fxVersionID },
		"lineage.compiler":        func(d *ir.IR) { d.Lineage.CompilerVersion = "some-other-compiler" },
		"lineage.sdk":             func(d *ir.IR) { d.Lineage.SDKVersion = "9.9.9" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := momentum(t)
			mutate(doc)
			doc.Normalize()
			assert.Equal(t, want, semanticHashHex(t, doc), "%s must not change the semantic hash", name)
		})
	}
}

// TestSemanticHash_CoversEverySemanticField: the mirror of the exclusion
// test. Anything the evaluator reads must move the hash, or a version could
// change behavior while reusing an approved hash.
func TestSemanticHash_CoversEverySemanticField(t *testing.T) {
	t.Parallel()
	want := semanticHashHex(t, momentum(t))

	cases := map[string]func(*testing.T, *ir.IR){
		"schema_version":     func(_ *testing.T, d *ir.IR) { d.SchemaVersion = 2 },
		"strategy_id":        func(_ *testing.T, d *ir.IR) { d.StrategyID = fxVersionID },
		"owner.account":      func(_ *testing.T, d *ir.IR) { d.Owner.AccountID = fxVersionID },
		"owner.user":         func(_ *testing.T, d *ir.IR) { d.Owner.UserID = fxVersionID },
		"instrument binding": func(_ *testing.T, d *ir.IR) { d.Instruments[0].InstrumentID = fxVersionID },
		"trigger interval":   func(_ *testing.T, d *ir.IR) { d.Triggers[0].EveryMS = every(60_000) },
		"trigger dedup":      func(_ *testing.T, d *ir.IR) { d.Triggers[0].DedupWindowMS = 1 },
		"dependency max age": func(_ *testing.T, d *ir.IR) { d.Dependencies[0].MaxAgeMS = 5000 },
		"dependency version": func(_ *testing.T, d *ir.IR) { d.Dependencies[0].DependencyVersion = 2 },
		"dependency tool":    func(_ *testing.T, d *ir.IR) { d.Dependencies[0].ToolVersion = 2 },
		"dependency param":   func(_ *testing.T, d *ir.IR) { d.Dependencies[0].Params["window"] = "10m" },
		"signal scale":       func(_ *testing.T, d *ir.IR) { d.Signals[0].Scale = 6 },
		"signal rounding":    func(_ *testing.T, d *ir.IR) { d.Signals[0].Rounding = "down" },
		"signal lookback":    func(_ *testing.T, d *ir.IR) { d.Signals[0].Expr.Window.LookbackMS = 600_000 },
		"condition operator": func(_ *testing.T, d *ir.IR) { d.Conditions[0].Expr.Cmp.Op = ir.CmpLT },
		"condition constant": func(t *testing.T, d *ir.IR) { d.Conditions[0].Expr.Cmp.R = ptrExpr(constExpr(t, "0.0300")) },
		"action sizing":      func(_ *testing.T, d *ir.IR) { d.Actions[1].Intent.Sizing.NotionalUSD = usdPtr(75_00) },
		"action slippage":    func(_ *testing.T, d *ir.IR) { d.Actions[1].Intent.Constraints.MaxSlippageBPS = 100 },
		"action venues":      func(_ *testing.T, d *ir.IR) { d.Actions[1].Intent.Constraints.AllowedVenues = []string{"ORCA"} },
		"action deadline":    func(_ *testing.T, d *ir.IR) { d.Actions[1].Intent.DeadlineMS = 60_000 },
		"prediction horizon": func(_ *testing.T, d *ir.IR) { d.Actions[0].Prediction.HorizonMS = 60_000 },
		"risk policy":        func(_ *testing.T, d *ir.IR) { d.RiskPolicy.Version = "risk-v2" },
		"risk policy hash":   func(_ *testing.T, d *ir.IR) { d.RiskPolicy.Hash = ir.Hex{0x09} },
		"model budget":       func(_ *testing.T, d *ir.IR) { d.ModelBudget.MaxCallsPerDay = 10 },
		"data budget":        func(_ *testing.T, d *ir.IR) { d.DataBudget.MaxLookbackMS = 7_200_000 },
		"envelope trade cap": func(_ *testing.T, d *ir.IR) { d.Envelope.MaxSingleTrade = usd(999_00) },
		"envelope rate":      func(_ *testing.T, d *ir.IR) { d.Envelope.MaxIntentsPerHour = 60 },
		"effects":            func(_ *testing.T, d *ir.IR) { d.Effects = append(d.Effects, ir.EffectCallModel) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := momentum(t)
			mutate(t, doc)
			doc.Normalize()
			assert.NotEqual(t, want, semanticHashHex(t, doc), "%s must change the semantic hash", name)
		})
	}
}

// TestSemanticHash_IndependentOfParamInsertionOrder: params is a Go map, so
// its insertion order varies per process. Canonicalisation must erase that.
func TestSemanticHash_IndependentOfParamInsertionOrder(t *testing.T) {
	t.Parallel()
	forward := momentum(t)
	forward.Dependencies[0].Params = map[string]string{}
	for _, k := range []string{"a_alpha", "b_beta", "c_gamma", "d_delta", "e_epsilon"} {
		forward.Dependencies[0].Params[k] = k + "_value"
	}
	forward.Normalize()

	reverse := momentum(t)
	reverse.Dependencies[0].Params = map[string]string{}
	for _, k := range []string{"e_epsilon", "d_delta", "c_gamma", "b_beta", "a_alpha"} {
		reverse.Dependencies[0].Params[k] = k + "_value"
	}
	reverse.Normalize()

	assert.Equal(t, semanticHashHex(t, forward), semanticHashHex(t, reverse))
}

// TestSemanticHash_IndependentOfSetOrder: effects and the envelope
// allowlists are sets. Writing them in a different order is the same
// strategy.
func TestSemanticHash_IndependentOfSetOrder(t *testing.T) {
	t.Parallel()
	want := semanticHashHex(t, momentum(t))

	shuffled := momentum(t)
	shuffled.Effects = []ir.Effect{ir.EffectCreateTradeIntent, ir.EffectReadMarketData, ir.EffectCommitPrediction}
	shuffled.Envelope.Venues = []string{"JUPITER", "JUPITER"} // duplicate collapses
	shuffled.Normalize()
	assert.Equal(t, want, semanticHashHex(t, shuffled))
}

// TestSemanticHash_TimezoneIndependent: BuiltAt is excluded, but every other
// timestamp-free field still has to survive a document built in a non-UTC
// location identically.
func TestSemanticHash_TimezoneIndependent(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable:", err)
	}
	shifted := momentum(t)
	shifted.BuiltAt = fxBuiltAt.In(loc)
	shifted.Normalize()
	assert.Equal(t, semanticHashHex(t, momentum(t)), semanticHashHex(t, shifted))
}

// TestSemanticHashOfJSON_MatchesTypedHash: the raw-JSON entry point (used by
// the parity fixtures and the SDK check) and the typed entry point must
// agree, otherwise Go and TypeScript could pass their own tests and still
// disagree with each other.
func TestSemanticHashOfJSON_MatchesTypedHash(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]*ir.IR{"momentum": momentum(t), "wallet": walletTrigger(t)} {
		t.Run(name, func(t *testing.T) {
			raw := mustJSON(t, doc)
			fromJSON, err := ir.SemanticHashOfJSON(raw)
			require.NoError(t, err)
			assert.Equal(t, semanticHashHex(t, doc), hex.EncodeToString(fromJSON))
		})
	}
}

// TestSemanticHash_SurvivesRoundTrip: decoding a persisted document and
// re-hashing must reproduce the stored hash, or a stored version could stop
// matching itself after a restart.
func TestSemanticHash_SurvivesRoundTrip(t *testing.T) {
	t.Parallel()
	original := momentum(t)
	want := semanticHashHex(t, original)

	decoded, err := ir.ParseIR(mustJSON(t, original))
	require.NoError(t, err)
	assert.Equal(t, want, semanticHashHex(t, decoded))

	// And again, to catch a Normalize that is not idempotent.
	twice, err := ir.ParseIR(mustJSON(t, decoded))
	require.NoError(t, err)
	assert.Equal(t, want, semanticHashHex(t, twice))
}

// TestCanonicalJSON_IsCanonical: sorted keys, no whitespace, and no
// floating-point rendering anywhere in the document.
func TestCanonicalJSON_IsCanonical(t *testing.T) {
	t.Parallel()
	canon, err := ir.CanonicalJSON(momentum(t))
	require.NoError(t, err)

	assert.NotContains(t, string(canon), " \"", "canonical json carries no insignificant whitespace")
	assert.NotContains(t, string(canon), "\n")

	// Re-encoding the parsed form reproduces the same bytes.
	var generic map[string]any
	require.NoError(t, json.Unmarshal(canon, &generic))
	assert.Contains(t, generic, "built_at", "the full document keeps provenance; only the hash drops it")

	semantic, err := ir.SemanticDocument(momentum(t))
	require.NoError(t, err)
	var semanticDoc map[string]any
	require.NoError(t, json.Unmarshal(semantic, &semanticDoc))
	for _, excluded := range []string{"hash", "version", "built_at", "lineage"} {
		assert.NotContains(t, semanticDoc, excluded, "%s must not reach the digest", excluded)
	}
	for _, kept := range []string{"schema_version", "strategy_id", "owner", "triggers", "dependencies", "signals", "conditions", "actions", "effects", "envelope"} {
		assert.Contains(t, semanticDoc, kept)
	}
}

// TestSemanticHash_NilDocument: a nil document is an error, never a digest
// of nothing that could collide with a real strategy.
func TestSemanticHash_NilDocument(t *testing.T) {
	t.Parallel()
	_, err := ir.SemanticHash(nil)
	require.ErrorIs(t, err, ir.ErrNilIR)
	_, err = ir.CanonicalJSON(nil)
	require.ErrorIs(t, err, ir.ErrNilIR)
	_, err = ir.SemanticDocument(nil)
	require.ErrorIs(t, err, ir.ErrNilIR)
}

// TestSemanticHashOfJSON_RejectsGarbage: the raw entry point never panics
// and never returns a digest for input it could not parse.
func TestSemanticHashOfJSON_RejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "{", "null", "[]", "\"a string\"", "{\"a\":", "12"} {
		_, err := ir.SemanticHashOfJSON([]byte(bad))
		assert.Error(t, err, "input %q must not hash", bad)
	}
}
