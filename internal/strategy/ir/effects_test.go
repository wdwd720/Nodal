package ir_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// TestEffectTables_MatchTheGoal pins the two tables verbatim against goal
// PART 63. The allowed list is also the strategy_versions.effect_set CHECK
// in migration 00500: if these drift, a document the compiler accepts is
// rejected by the database (or, worse, the reverse).
func TestEffectTables_MatchTheGoal(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []ir.Effect{
		ir.EffectCallModel,
		ir.EffectCommitPrediction,
		ir.EffectCreateTradeIntent,
		ir.EffectReadApprovedSocialData,
		ir.EffectReadMarketData,
		ir.EffectReadOnchainData,
		ir.EffectReadWalletIntelligence,
	}, ir.AllowedEffects(), "allowed effects are sorted and exactly the seven of PART 63")

	assert.Equal(t, []ir.Effect{
		ir.EffectAccessAdminAPI,
		ir.EffectArbitraryContractCall,
		ir.EffectArbitraryNetwork,
		ir.EffectChangeCapital,
		ir.EffectChangeRisk,
		ir.EffectExportSecret,
		ir.EffectModifyCapabilityGate,
		ir.EffectRawSign,
		ir.EffectTransferValue,
		ir.EffectWithdraw,
	}, ir.ForbiddenEffects(), "forbidden effects are sorted and exactly the ten of PART 63")
}

// TestEffectTables_Disjoint: nothing may be both grantable and reserved.
func TestEffectTables_Disjoint(t *testing.T) {
	t.Parallel()
	for _, a := range ir.AllowedEffects() {
		assert.True(t, a.Allowed(), "%s reports itself allowed", a)
		assert.False(t, a.Forbidden(), "%s must not also be forbidden", a)
	}
	for _, f := range ir.ForbiddenEffects() {
		assert.True(t, f.Forbidden(), "%s reports itself forbidden", f)
		assert.False(t, f.Allowed(), "%s must never be allowed", f)
	}
}

// TestEffect_UnknownNameIsNotAllowed: the classifier fails closed. A name
// from a future version, or one a model invented, is not grantable.
func TestEffect_UnknownNameIsNotAllowed(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "read_market_data", "READ_MARKET_DATA ", "SUDO", "READ_EVERYTHING", "TRANSFER_VALUE_V2"} {
		e := ir.Effect(name)
		assert.False(t, e.Allowed(), "%q must not be allowed", name)
	}
}

// TestDeriveEffects_IsClosed is the core closure property: whatever a
// document contains, the derived set is a subset of the allowed table. A
// strategy cannot derive its way to a capability that does not exist.
func TestDeriveEffects_IsClosed(t *testing.T) {
	t.Parallel()
	allowed := map[ir.Effect]struct{}{}
	for _, a := range ir.AllowedEffects() {
		allowed[a] = struct{}{}
	}
	rapid.Check(t, func(rt *rapid.T) {
		doc := &ir.IR{}
		kinds := []ir.DependencyKind{
			ir.DepPrice, ir.DepOnchain, ir.DepWalletEvent, ir.DepSocial, ir.DepWalletIntelligence, ir.DepModel, ir.DepFeature,
			// Names outside the enum, including forbidden effect names used
			// where a dependency kind belongs.
			ir.DependencyKind("TRANSFER_VALUE"), ir.DependencyKind("EXPORT_SECRET"), ir.DependencyKind(""), ir.DependencyKind("WHATEVER"),
		}
		actionKinds := []ir.ActionKind{
			ir.ActionCallModel, ir.ActionCommitPrediction, ir.ActionCreateTradeIntent,
			ir.ActionKind("RAW_SIGN"), ir.ActionKind("WITHDRAW"), ir.ActionKind(""), ir.ActionKind("EXEC"),
		}
		for i := 0; i < rapid.IntRange(0, 12).Draw(rt, "deps"); i++ {
			doc.Dependencies = append(doc.Dependencies, ir.Dependency{
				Kind: kinds[rapid.IntRange(0, len(kinds)-1).Draw(rt, "kind")],
			})
		}
		for i := 0; i < rapid.IntRange(0, 12).Draw(rt, "actions"); i++ {
			doc.Actions = append(doc.Actions, ir.Action{
				Kind: actionKinds[rapid.IntRange(0, len(actionKinds)-1).Draw(rt, "akind")],
			})
		}

		derived := ir.DeriveEffects(doc)
		for _, e := range derived {
			_, ok := allowed[e]
			if !ok {
				rt.Fatalf("derived effect %q is outside the allowed table", e)
			}
			if e.Forbidden() {
				rt.Fatalf("derived effect %q is forbidden", e)
			}
		}
		// Sorted, unique, and idempotent.
		for i := 1; i < len(derived); i++ {
			if derived[i-1] >= derived[i] {
				rt.Fatalf("derived effects not strictly sorted: %v", derived)
			}
		}
		doc.Effects = derived
		if !ir.EffectsEqual(derived, ir.DeriveEffects(doc)) {
			rt.Fatalf("DeriveEffects is not idempotent")
		}
	})
}

// TestDeriveEffects_PerKind pins the mapping table of STRATEGY_IR.md §3.
func TestDeriveEffects_PerKind(t *testing.T) {
	t.Parallel()
	depCases := map[ir.DependencyKind]ir.Effect{
		ir.DepPrice:              ir.EffectReadMarketData,
		ir.DepFeature:            ir.EffectReadMarketData,
		ir.DepOnchain:            ir.EffectReadOnchainData,
		ir.DepWalletEvent:        ir.EffectReadOnchainData,
		ir.DepSocial:             ir.EffectReadApprovedSocialData,
		ir.DepWalletIntelligence: ir.EffectReadWalletIntelligence,
		ir.DepModel:              ir.EffectCallModel,
	}
	for kind, want := range depCases {
		assert.Equal(t, want, ir.EffectOfDependency(kind), "dependency kind %s", kind)
		doc := &ir.IR{Dependencies: []ir.Dependency{{Kind: kind}}}
		assert.Equal(t, []ir.Effect{want}, ir.DeriveEffects(doc))
	}
	assert.Equal(t, ir.Effect(""), ir.EffectOfDependency("NOT_A_KIND"), "unknown kinds grant nothing")

	actionCases := map[ir.ActionKind]ir.Effect{
		ir.ActionCallModel:         ir.EffectCallModel,
		ir.ActionCommitPrediction:  ir.EffectCommitPrediction,
		ir.ActionCreateTradeIntent: ir.EffectCreateTradeIntent,
	}
	for kind, want := range actionCases {
		assert.Equal(t, want, ir.EffectOfAction(kind), "action kind %s", kind)
	}
	assert.Equal(t, ir.Effect(""), ir.EffectOfAction("NOT_A_KIND"), "unknown action kinds grant nothing")
	assert.Empty(t, ir.DeriveEffects(nil), "a nil document derives nothing")
}

// TestCheck_RejectsEveryForbiddenEffect walks the whole forbidden table:
// every reserved name must be rejected by name, so the golden corpus can
// point at a specific constant rather than a generic failure (PART 170).
func TestCheck_RejectsEveryForbiddenEffect(t *testing.T) {
	t.Parallel()
	for _, forbidden := range ir.ForbiddenEffects() {
		t.Run(string(forbidden), func(t *testing.T) {
			doc := momentum(t)
			doc.Effects = append(doc.Effects, forbidden)
			doc.Normalize()

			err := ir.Check(doc)
			require.Error(t, err, "%s must be rejected", forbidden)

			var ve *ir.ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Equal(t, "EFFECT", ve.Stage, "a forbidden effect fails at the EFFECT stage")
			assert.Contains(t, ve.Codes(), ir.CodeEffectForbidden)

			// The rejection names the offending effect, and maps to the
			// non-retryable API code.
			assert.Contains(t, err.Error(), ir.CodeEffectForbidden)
			assert.Equal(t, errs.CodeEffectForbidden, errs.CodeOf(ve.ToErr()))

			var named bool
			for _, is := range ve.Issues {
				if is.Detail == string(forbidden) {
					named = true
				}
			}
			assert.True(t, named, "the finding must name %s", forbidden)
		})
	}
}

// TestCheck_RejectsForbiddenEffectFromRawJSON: the same rejection must
// happen on the wire path, where a model or an SDK supplies the document as
// bytes. This is the "invalid transfer request" / "invalid secret export"
// corpus behavior.
func TestCheck_RejectsForbiddenEffectFromRawJSON(t *testing.T) {
	t.Parallel()
	for _, forbidden := range []ir.Effect{ir.EffectTransferValue, ir.EffectWithdraw, ir.EffectExportSecret, ir.EffectRawSign, ir.EffectArbitraryContractCall} {
		t.Run(string(forbidden), func(t *testing.T) {
			doc := momentum(t)
			doc.Effects = append(doc.Effects, forbidden)
			doc.Normalize()

			_, err := ir.ParseIR(mustJSON(t, doc))
			require.Error(t, err)
			var ve *ir.ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Equal(t, "EFFECT", ve.Stage)
			assert.Contains(t, ve.Codes(), ir.CodeEffectForbidden)
		})
	}
}

// TestCheck_DeclaredMustEqualDerived: a document cannot under-declare (grant
// itself a capability the runtime would not expect) or over-declare (hold a
// capability nothing in it uses). Both are EFFECT_MISMATCH.
func TestCheck_DeclaredMustEqualDerived(t *testing.T) {
	t.Parallel()

	t.Run("over-declared", func(t *testing.T) {
		doc := momentum(t)
		doc.Effects = append(doc.Effects, ir.EffectReadApprovedSocialData) // nothing reads social data
		doc.Normalize()
		var ve *ir.ValidationError
		require.ErrorAs(t, ir.Check(doc), &ve)
		assert.Equal(t, "EFFECT", ve.Stage)
		assert.Contains(t, ve.Codes(), ir.CodeEffectMismatch)
	})

	t.Run("under-declared", func(t *testing.T) {
		doc := momentum(t)
		// Add an on-chain read but leave the declared set alone.
		doc.Dependencies = append(doc.Dependencies, ir.Dependency{
			Name: "chain", Kind: ir.DepOnchain, ToolCode: "chain_reader", ToolVersion: 1, DependencyVersion: 1,
			Params: map[string]string{}, MaxAgeMS: 2000, Required: true,
		})
		doc.Normalize()
		var ve *ir.ValidationError
		require.ErrorAs(t, ir.Check(doc), &ve)
		assert.Contains(t, ve.Codes(), ir.CodeEffectMismatch)
	})

	t.Run("empty declaration", func(t *testing.T) {
		doc := momentum(t)
		doc.Effects = nil
		doc.Normalize()
		var ve *ir.ValidationError
		require.ErrorAs(t, ir.Check(doc), &ve)
		assert.Contains(t, ve.Codes(), ir.CodeEffectMismatch)
	})

	t.Run("correctly declared", func(t *testing.T) {
		doc := momentum(t)
		doc.Effects = ir.DeriveEffects(doc)
		doc.Normalize()
		require.NoError(t, ir.Check(doc))
	})
}

// TestCheck_AddingAnActionRequiresRedeclaration: the mismatch rule is what
// makes the effect set a real declaration rather than a comment. Adding a
// model call to an accepted strategy cannot silently inherit its approval.
func TestCheck_AddingAnActionRequiresRedeclaration(t *testing.T) {
	t.Parallel()
	doc := momentum(t)
	require.NoError(t, ir.Check(doc))

	doc.Dependencies = append(doc.Dependencies, ir.Dependency{
		Name: "judge", Kind: ir.DepModel, ToolCode: "llm_judge", ToolVersion: 1, DependencyVersion: 1,
		Params: map[string]string{}, MaxAgeMS: 60_000, Required: false,
	})
	doc.ModelBudget.MaxCallsPerRun = 1
	doc.ModelBudget.MaxCallsPerDay = 10
	doc.ModelBudget.MaxOutputTokens = 4096
	doc.Actions = append([]ir.Action{{
		Name: "ask_model", Kind: ir.ActionCallModel,
		Model: &ir.ModelCall{TemplateVersion: "v1", OutputSchema: "judge", Inputs: []ir.Ref{"ret_5m"}, MaxOutputTokens: 2048, Required: false},
	}}, doc.Actions...)
	doc.Normalize()

	var ve *ir.ValidationError
	require.ErrorAs(t, ir.Check(doc), &ve, "the stale effect set must be rejected")
	assert.Contains(t, ve.Codes(), ir.CodeEffectMismatch)

	doc.Effects = ir.DeriveEffects(doc)
	doc.Normalize()
	require.NoError(t, ir.Check(doc), "redeclaring the derived set makes it valid")
	assert.Contains(t, ir.EffectStrings(doc.Effects), string(ir.EffectCallModel))
}

// TestEffects_PersistenceRoundTrip: the strings written to
// strategy_versions.effect_set come back as the same set.
func TestEffects_PersistenceRoundTrip(t *testing.T) {
	t.Parallel()
	doc := momentum(t)
	stored := ir.EffectStrings(doc.Effects)
	assert.Equal(t, []string{"COMMIT_PREDICTION", "CREATE_TRADE_INTENT", "READ_MARKET_DATA"}, stored, "sorted for a stable column value")
	assert.True(t, ir.EffectsEqual(doc.Effects, ir.EffectsFromStrings(stored)))

	for _, s := range stored {
		assert.Equal(t, strings.ToUpper(s), s, "effect names are upper snake case")
		assert.True(t, ir.Effect(s).Allowed(), "%s is within the migration CHECK", s)
	}
}

// TestSortEffects_Normalises: sorting deduplicates and copies, so a caller
// cannot mutate the tables through a returned slice.
func TestSortEffects_Normalises(t *testing.T) {
	t.Parallel()
	got := ir.SortEffects([]ir.Effect{ir.EffectCreateTradeIntent, ir.EffectReadMarketData, ir.EffectReadMarketData, ir.EffectCallModel})
	assert.Equal(t, []ir.Effect{ir.EffectCallModel, ir.EffectCreateTradeIntent, ir.EffectReadMarketData}, got)

	table := ir.AllowedEffects()
	table[0] = "MUTATED"
	assert.NotEqual(t, ir.Effect("MUTATED"), ir.AllowedEffects()[0], "AllowedEffects returns a copy")

	forbidden := ir.ForbiddenEffects()
	forbidden[0] = "MUTATED"
	assert.NotEqual(t, ir.Effect("MUTATED"), ir.ForbiddenEffects()[0], "ForbiddenEffects returns a copy")

	assert.True(t, ir.ContainsEffect(table[1:], ir.AllowedEffects()[1]))
	assert.False(t, ir.ContainsEffect(nil, ir.EffectCallModel))
}

// TestCheck_ForbiddenEffectBeatsOtherFailures: a document that is both
// structurally broken and asks for a forbidden capability is reported as an
// effect violation, because that outcome is terminal (never retried) while
// structural failures are retryable (STRATEGY_IR.md §4).
func TestCheck_ForbiddenEffectBeatsOtherFailures(t *testing.T) {
	t.Parallel()
	doc := momentum(t)
	doc.Effects = append(doc.Effects, ir.EffectWithdraw)
	doc.Triggers = nil // also structurally invalid
	doc.SchemaVersion = 99
	doc.Normalize()

	var ve *ir.ValidationError
	require.ErrorAs(t, ir.Check(doc), &ve)
	assert.Equal(t, "EFFECT", ve.Stage, "the terminal failure is reported, not the retryable one")
	assert.Contains(t, ve.Codes(), ir.CodeEffectForbidden)
}

// TestForbiddenEffect_CannotBeSmuggledThroughUnknownJSONFields: the strict
// decoder rejects a document carrying an undeclared capability field, so
// there is no side channel around the effect set.
func TestForbiddenEffect_CannotBeSmuggledThroughUnknownJSONFields(t *testing.T) {
	t.Parallel()
	var generic map[string]any
	require.NoError(t, json.Unmarshal(mustJSON(t, momentum(t)), &generic))
	generic["capabilities"] = []string{"TRANSFER_VALUE"}
	raw, err := json.Marshal(generic)
	require.NoError(t, err)

	_, err = ir.ParseIR(raw)
	require.Error(t, err)
	var ve *ir.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "PARSE", ve.Stage)
	assert.Contains(t, ve.Codes(), ir.CodeParseFailed)
}
