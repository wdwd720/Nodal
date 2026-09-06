package ir_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// requireCodes asserts that checking doc fails at the given stage with the
// expected code present, and returns the findings for further inspection.
func requireCodes(t *testing.T, doc *ir.IR, stage, code string) []ir.Issue {
	t.Helper()
	err := ir.Check(doc)
	require.Error(t, err, "expected %s/%s", stage, code)
	var ve *ir.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, stage, ve.Stage)
	assert.Contains(t, ve.Codes(), code)
	return ve.Issues
}

func TestParseIR_AcceptsValidDocument(t *testing.T) {
	t.Parallel()
	doc, err := ir.ParseIR(mustJSON(t, momentum(t)))
	require.NoError(t, err)
	assert.Equal(t, ir.SchemaVersion, doc.SchemaVersion)
	assert.Equal(t, fxStrategyID, doc.StrategyID)
	assert.Len(t, doc.Actions, 2)
	assert.Equal(t, []ir.Effect{ir.EffectCommitPrediction, ir.EffectCreateTradeIntent, ir.EffectReadMarketData}, doc.Effects)
}

// TestDecode_RejectsUnknownFields is the model-output contract: a document
// carrying anything the IR does not define is refused outright rather than
// silently ignored, so a field cannot smuggle behavior past validation.
func TestDecode_RejectsUnknownFields(t *testing.T) {
	t.Parallel()
	var generic map[string]any
	require.NoError(t, json.Unmarshal(mustJSON(t, momentum(t)), &generic))

	for _, field := range []string{"exec", "shell", "eval", "callback_url", "unknown"} {
		t.Run(field, func(t *testing.T) {
			mutated := map[string]any{}
			for k, v := range generic {
				mutated[k] = v
			}
			mutated[field] = "anything"
			raw, err := json.Marshal(mutated)
			require.NoError(t, err)

			_, err = ir.Decode(raw)
			require.Error(t, err)
			var ve *ir.ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Equal(t, "PARSE", ve.Stage)
			assert.Contains(t, ve.Codes(), ir.CodeParseFailed)
		})
	}
}

func TestDecode_RejectsMalformedInput(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":         "",
		"not json":      "this is not json",
		"truncated":     `{"schema_version": 1`,
		"trailing data": `{"schema_version":1} {"schema_version":1}`,
		"array":         `[]`,
		"string":        `"a strategy"`,
		"number":        `42`,
		"wrong type":    `{"schema_version": "one"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ir.Decode([]byte(raw))
			require.Error(t, err)
			var ve *ir.ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Equal(t, "PARSE", ve.Stage)
		})
	}
}

// TestDecode_RejectsOversizeAndDeepDocuments: the size and nesting caps are
// checked before the reflective decoder runs, so a bomb cannot exhaust
// memory during a compile.
func TestDecode_RejectsOversizeAndDeepDocuments(t *testing.T) {
	t.Parallel()

	t.Run("oversize", func(t *testing.T) {
		raw := append([]byte(`{"strategy_id":"`), []byte(strings.Repeat("a", ir.MaxDocumentBytes))...)
		raw = append(raw, []byte(`"}`)...)
		_, err := ir.Decode(raw)
		require.Error(t, err)
		assert.Contains(t, err.Error(), ir.CodeParseFailed)
	})

	t.Run("deeply nested", func(t *testing.T) {
		depth := ir.MaxJSONDepth + 10
		raw := strings.Repeat(`{"a":`, depth) + "1" + strings.Repeat("}", depth)
		_, err := ir.Decode([]byte(raw))
		require.Error(t, err, "a nesting bomb must be rejected, not recursed into")
	})

	t.Run("deep array", func(t *testing.T) {
		depth := ir.MaxJSONDepth + 10
		raw := strings.Repeat("[", depth) + strings.Repeat("]", depth)
		_, err := ir.Decode([]byte(raw))
		require.Error(t, err)
	})

	t.Run("invalid utf8", func(t *testing.T) {
		_, err := ir.Decode([]byte{'{', '"', 0xff, 0xfe, '"', ':', '1', '}'})
		require.Error(t, err)
	})
}

// TestStructural_RejectsUnboundedLoop is the "invalid unbounded loop" case
// of the golden corpus (PART 170, PART 198): a sub-second interval is
// refused, so no strategy can spin.
func TestStructural_RejectsUnboundedLoop(t *testing.T) {
	t.Parallel()

	t.Run("sub-second interval", func(t *testing.T) {
		doc := momentum(t)
		doc.Triggers[0].EveryMS = every(100)
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralIntervalTooShort)
	})

	t.Run("zero interval", func(t *testing.T) {
		doc := momentum(t)
		doc.Triggers[0].EveryMS = every(0)
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralIntervalTooShort)
	})

	t.Run("interval beyond a week", func(t *testing.T) {
		doc := momentum(t)
		doc.Triggers[0].EveryMS = every(ir.MaxIntervalMS + 1)
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidTrigger)
	})

	t.Run("runs per minute beyond the cap", func(t *testing.T) {
		doc := momentum(t)
		doc.Envelope.MaxRunsPerMinute = ir.MaxRunsPerMinute + 1
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralRateLimit)
	})

	t.Run("no rate limit at all", func(t *testing.T) {
		doc := momentum(t)
		doc.Envelope.MaxRunsPerMinute = 0
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralRateLimit)
	})
}

// TestStructural_RejectsSelfAndForwardSignalReferences: the grammar has no
// recursion, and this is what enforces it. A signal may only read signals
// declared before it, so evaluation terminates by construction.
func TestStructural_RejectsSelfAndForwardSignalReferences(t *testing.T) {
	t.Parallel()

	t.Run("self reference", func(t *testing.T) {
		doc := momentum(t)
		doc.Signals[0].Expr = ir.Expr{Signal: ref("ret_5m")}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralSignalCycle)
	})

	t.Run("forward reference", func(t *testing.T) {
		doc := momentum(t)
		doc.Signals = []ir.Signal{
			{Name: "first", Scale: 4, Rounding: "half_even", Expr: ir.Expr{Signal: ref("second")}},
			{Name: "second", Scale: 4, Rounding: "half_even", Expr: constExpr(t, "1.0000")},
		}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralSignalCycle)
	})

	t.Run("mutual cycle", func(t *testing.T) {
		doc := momentum(t)
		doc.Signals = []ir.Signal{
			{Name: "a", Scale: 4, Rounding: "half_even", Expr: ir.Expr{Signal: ref("b")}},
			{Name: "b", Scale: 4, Rounding: "half_even", Expr: ir.Expr{Signal: ref("a")}},
		}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralSignalCycle)
	})

	t.Run("backward reference is fine", func(t *testing.T) {
		doc := momentum(t)
		doc.Signals = append(doc.Signals, ir.Signal{
			Name: "doubled", Scale: 4, Rounding: "half_even",
			Expr: ir.Expr{Bin: &ir.BinOp{
				Op: ir.OpMul, Scale: 4, Rounding: "half_even",
				L: &ir.Expr{Signal: ref("ret_5m")},
				R: ptrExpr(constExpr(t, "2.0000")),
			}},
		})
		doc.Normalize()
		require.NoError(t, ir.Check(doc))
	})

	t.Run("undeclared signal", func(t *testing.T) {
		doc := momentum(t)
		doc.Conditions[0].Expr.Cmp.L = &ir.Expr{Signal: ref("nonexistent")}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralUnresolvedRef)
	})
}

// TestStructural_BoundsExpressions: depth and node caps make evaluation
// cost a property of the document, not of the input data.
func TestStructural_BoundsExpressions(t *testing.T) {
	t.Parallel()

	t.Run("too deep", func(t *testing.T) {
		doc := momentum(t)
		e := constExpr(t, "1.0000")
		for i := 0; i < ir.MaxExprDepth+2; i++ {
			e = ir.Expr{Bin: &ir.BinOp{Op: ir.OpAdd, Scale: 4, Rounding: "half_even", L: ptrExpr(e), R: ptrExpr(constExpr(t, "1.0000"))}}
		}
		doc.Signals[0].Expr = e
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralExprDepth)
	})

	t.Run("too many nodes", func(t *testing.T) {
		doc := momentum(t)
		// Many shallow signals, each a couple of nodes, past the total cap.
		doc.Signals = nil
		for i := 0; i < ir.MaxSignals; i++ {
			doc.Signals = append(doc.Signals, ir.Signal{
				Name: ir.Ref(fmt.Sprintf("s_%d", i)), Scale: 4, Rounding: "half_even",
				Expr: ir.Expr{And: []*ir.Expr{
					ptrExpr(constExpr(t, "1.0000")), ptrExpr(constExpr(t, "1.0000")),
					ptrExpr(constExpr(t, "1.0000")), ptrExpr(constExpr(t, "1.0000")),
				}},
			})
		}
		doc.Conditions[0].Expr.Cmp.L = ptrExpr(constExpr(t, "1.0000"))
		issues := requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralExprNodes)
		assert.Contains(t, ir.Codes(issues), ir.CodeStructuralExprNodes)
	})

	t.Run("malformed expression", func(t *testing.T) {
		doc := momentum(t)
		doc.Signals[0].Expr = ir.Expr{} // no member set
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralExprMalformed)
	})

	t.Run("two members set", func(t *testing.T) {
		doc := momentum(t)
		d := dec(t, "1.0000")
		doc.Signals[0].Expr = ir.Expr{Const: &d, Signal: ref("ret_5m")}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralExprMalformed)
	})

	t.Run("unknown operator", func(t *testing.T) {
		doc := momentum(t)
		doc.Conditions[0].Expr.Cmp.Op = "APPROXIMATELY"
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidOperator)
	})
}

// TestStructural_EnforcesStalenessDeclaration (PART 174): every dependency
// must carry a positive max age, so no read is implicitly timeless.
func TestStructural_EnforcesStalenessDeclaration(t *testing.T) {
	t.Parallel()
	for _, age := range []int64{0, -1, -5000} {
		doc := momentum(t)
		doc.Dependencies[0].MaxAgeMS = age
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralMaxAge)
	}
}

// TestStructural_BoundsWindowLookback: a window may not reach further back
// than the declared data budget.
func TestStructural_BoundsWindowLookback(t *testing.T) {
	t.Parallel()

	doc := momentum(t)
	doc.Signals[0].Expr.Window.LookbackMS = doc.DataBudget.MaxLookbackMS + 1
	requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralLookbackBudget)

	zero := momentum(t)
	zero.Signals[0].Expr.Window.LookbackMS = 0
	requireCodes(t, zero, "STRUCTURAL", ir.CodeStructuralLookbackBudget)
}

// TestStructural_IntentRequiresPrediction (PART 72): an intent must name an
// earlier COMMIT_PREDICTION action, so prediction always predates execution.
func TestStructural_IntentRequiresPrediction(t *testing.T) {
	t.Parallel()

	t.Run("missing prediction action", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions = doc.Actions[1:] // drop the prediction, keep the intent
		doc.Effects = ir.DeriveEffects(doc)
		doc.Normalize()
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralIntentNoPrediction)
	})

	t.Run("names a non-prediction action", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[1].Intent.Prediction = "buy" // itself
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralIntentNoPrediction)
	})

	t.Run("names an undeclared action", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[1].Intent.Prediction = "ghost"
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralIntentNoPrediction)
	})
}

func TestStructural_RejectsInvalidReferences(t *testing.T) {
	t.Parallel()

	t.Run("bad ref grammar", func(t *testing.T) {
		for _, bad := range []ir.Ref{"", "1abc", "Abc", "with-dash", "with space", "with.dot", ir.Ref(strings.Repeat("a", 65))} {
			doc := momentum(t)
			doc.Signals[0].Name = bad
			err := ir.Check(doc)
			require.Error(t, err, "ref %q must be rejected", bad)
		}
	})

	t.Run("duplicate names", func(t *testing.T) {
		doc := momentum(t)
		doc.Dependencies = append(doc.Dependencies, doc.Dependencies[0])
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralDuplicateRef)
	})

	t.Run("undeclared dependency in a window", func(t *testing.T) {
		doc := momentum(t)
		doc.Signals[0].Expr.Window.Dependency = "no_such_dependency"
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralUnresolvedRef)
	})

	t.Run("undeclared instrument", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[1].Intent.Instrument = "no_such_instrument"
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralUnresolvedRef)
	})

	t.Run("undeclared condition", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[0].When = ref("no_such_condition")
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralUnresolvedRef)
	})

	t.Run("instrument outside the envelope", func(t *testing.T) {
		doc := momentum(t)
		doc.Envelope.Instruments = []string{fxVersionID}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralEnvelopeInstruments)
	})
}

func TestStructural_RejectsBadIdentityAndCounts(t *testing.T) {
	t.Parallel()

	t.Run("non-uuid identifiers", func(t *testing.T) {
		for _, field := range []string{"strategy", "account", "user"} {
			doc := momentum(t)
			switch field {
			case "strategy":
				doc.StrategyID = "not-a-uuid"
			case "account":
				doc.Owner.AccountID = ""
			case "user":
				doc.Owner.UserID = "12345"
			}
			requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidID)
		}
	})

	t.Run("wrong schema version", func(t *testing.T) {
		for _, v := range []int{0, 2, -1, 999} {
			doc := momentum(t)
			doc.SchemaVersion = v
			requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralSchemaVersion)
		}
	})

	t.Run("no trigger", func(t *testing.T) {
		doc := momentum(t)
		doc.Triggers = nil
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralNoTrigger)
	})

	t.Run("no action", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions = nil
		doc.Effects = ir.DeriveEffects(doc)
		doc.Normalize()
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralNoAction)
	})

	t.Run("no dependency", func(t *testing.T) {
		doc := momentum(t)
		doc.Dependencies = nil
		doc.Effects = ir.DeriveEffects(doc)
		doc.Normalize()
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralNoDependency)
	})

	t.Run("too many triggers", func(t *testing.T) {
		doc := momentum(t)
		for i := 0; i < ir.MaxTriggers+2; i++ {
			doc.Triggers = append(doc.Triggers, ir.Trigger{
				Name: ir.Ref(fmt.Sprintf("t_%d", i)), Kind: ir.TriggerOnInterval, EveryMS: every(60_000), DedupWindowMS: 0,
			})
		}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralTooMany)
	})
}

func TestStructural_TriggerKindRules(t *testing.T) {
	t.Parallel()

	t.Run("unknown kind", func(t *testing.T) {
		doc := momentum(t)
		doc.Triggers[0].Kind = "ON_WHATEVER"
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidTrigger)
	})

	t.Run("interval trigger carrying event fields", func(t *testing.T) {
		doc := momentum(t)
		doc.Triggers[0].EventType = "market.price"
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidTrigger)
	})

	t.Run("event trigger carrying an interval", func(t *testing.T) {
		doc := walletTrigger(t)
		doc.Triggers[0].EveryMS = every(5000)
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidTrigger)
	})

	t.Run("negative dedup window", func(t *testing.T) {
		doc := momentum(t)
		doc.Triggers[0].DedupWindowMS = -1
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralDedupWindow)
	})

	t.Run("window over the triggering event", func(t *testing.T) {
		doc := walletTrigger(t)
		doc.Triggers[0].Filter = &ir.Expr{Cmp: &ir.Cmp{
			Op: ir.CmpGT,
			L: &ir.Expr{Window: &ir.WindowOp{
				Fn: ir.WinSum, Dependency: "on_transfer", Path: "amount", LookbackMS: 1000, Scale: 2, Rounding: "half_even",
			}},
			R: ptrExpr(constExpr(t, "1.00")),
		}}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidTrigger)
	})
}

func TestStructural_ActionShapeRules(t *testing.T) {
	t.Parallel()

	t.Run("kind without its spec", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[0].Prediction = nil
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidAction)
	})

	t.Run("two specs at once", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[0].Intent = doc.Actions[1].Intent
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidAction)
	})

	t.Run("unknown action kind", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[0].Kind = "TRANSFER"
		doc.Effects = ir.DeriveEffects(doc)
		doc.Normalize()
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidAction)
	})

	t.Run("unknown intent action", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[1].Intent.Action = "SEND_TO_ADDRESS"
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidAction)
	})

	t.Run("unknown sizing kind", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[1].Intent.Sizing.Kind = "ALL_AVAILABLE_FUNDS"
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidAction)
	})

	t.Run("non-positive deadline", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[1].Intent.DeadlineMS = 0
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidAction)
	})

	t.Run("unknown prediction direction", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[0].Prediction.Direction = "SIDEWAYS"
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidAction)
	})

	t.Run("non-positive horizon", func(t *testing.T) {
		doc := momentum(t)
		doc.Actions[0].Prediction.HorizonMS = 0
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidAction)
	})
}

func TestStructural_BudgetConsistency(t *testing.T) {
	t.Parallel()

	t.Run("tool calls per run below the read count", func(t *testing.T) {
		doc := momentum(t)
		doc.DataBudget.MaxToolCallsPerRun = 0
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralDataBudget)
	})

	t.Run("daily below per-run", func(t *testing.T) {
		doc := momentum(t)
		doc.DataBudget.MaxToolCallsPerDay = 1
		doc.DataBudget.MaxToolCallsPerRun = 4
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralDataBudget)
	})

	t.Run("model daily below per-run", func(t *testing.T) {
		doc := momentum(t)
		doc.ModelBudget.MaxCallsPerRun = 5
		doc.ModelBudget.MaxCallsPerDay = 1
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralModelBudget)
	})

	t.Run("negative budgets", func(t *testing.T) {
		doc := momentum(t)
		doc.DataBudget.MaxLookbackMS = -1
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralDataBudget)
	})

	t.Run("intents without an hourly cap", func(t *testing.T) {
		doc := momentum(t)
		doc.Envelope.MaxIntentsPerHour = 0
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralRateLimit)
	})
}

func TestStructural_DependencyRules(t *testing.T) {
	t.Parallel()

	t.Run("unknown kind", func(t *testing.T) {
		doc := momentum(t)
		doc.Dependencies[0].Kind = "READ_EVERYTHING"
		doc.Effects = ir.DeriveEffects(doc)
		doc.Normalize()
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidDependency)
	})

	t.Run("zero tool version", func(t *testing.T) {
		doc := momentum(t)
		doc.Dependencies[0].ToolVersion = 0
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidDependency)
	})

	t.Run("zero dependency version", func(t *testing.T) {
		doc := momentum(t)
		doc.Dependencies[0].DependencyVersion = 0
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralInvalidDependency)
	})

	t.Run("bad param key", func(t *testing.T) {
		doc := momentum(t)
		doc.Dependencies[0].Params = map[string]string{"Bad Key": "v"}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralParams)
	})

	t.Run("oversize param value", func(t *testing.T) {
		doc := momentum(t)
		doc.Dependencies[0].Params = map[string]string{"k": strings.Repeat("v", ir.MaxParamLength+1)}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralParams)
	})

	t.Run("control characters in a param", func(t *testing.T) {
		doc := momentum(t)
		doc.Dependencies[0].Params = map[string]string{"k": "line\x00break"}
		requireCodes(t, doc, "STRUCTURAL", ir.CodeStructuralParams)
	})
}

// TestValidationError_Reporting: findings are sorted, deduplicated and
// carry a field path, because these strings are persisted in
// compile_attempts.failure_codes and fed back to a retry as tool results.
func TestValidationError_Reporting(t *testing.T) {
	t.Parallel()
	doc := momentum(t)
	doc.SchemaVersion = 7
	doc.Triggers = nil
	doc.Dependencies[0].MaxAgeMS = 0

	var ve *ir.ValidationError
	require.ErrorAs(t, ir.Check(doc), &ve)

	codes := ve.Codes()
	assert.Equal(t, "STRUCTURAL", ve.Stage)
	require.NotEmpty(t, codes)
	for i := 1; i < len(codes); i++ {
		assert.Less(t, codes[i-1], codes[i], "codes are sorted and unique")
	}
	assert.Contains(t, codes, ir.CodeStructuralSchemaVersion)
	assert.Contains(t, codes, ir.CodeStructuralNoTrigger)
	assert.Contains(t, codes, ir.CodeStructuralMaxAge)

	fields := ir.Fields(ve.Issues)
	assert.Contains(t, fields, "schema_version")
	assert.Contains(t, fields, "triggers")

	// Structural failures are retryable, so they map to the generic
	// rejection code rather than the terminal effect one.
	assert.Equal(t, errs.CodeStrategyRejected, errs.CodeOf(ve.ToErr()))
	assert.Contains(t, ve.ToErr().Fields, "stage")
	assert.Contains(t, ve.ToErr().Fields, "codes")
}

func TestCheck_NilDocument(t *testing.T) {
	t.Parallel()
	require.ErrorIs(t, ir.Check(nil), ir.ErrNilIR)
	assert.NotEmpty(t, ir.Structural(nil), "a nil document is a finding, not a pass")
}

// TestNormalize_IsIdempotent: normalising twice equals normalising once, so
// a document cannot change hash by being handled an extra time.
func TestNormalize_IsIdempotent(t *testing.T) {
	t.Parallel()
	doc := momentum(t)
	first := mustJSON(t, doc)
	doc.Normalize()
	doc.Normalize()
	assert.JSONEq(t, string(first), string(mustJSON(t, doc)))

	// Normalize also fills nil collections so the JSON shape is stable.
	sparse := &ir.IR{SchemaVersion: ir.SchemaVersion}
	sparse.Normalize()
	assert.NotNil(t, sparse.Triggers)
	assert.NotNil(t, sparse.Dependencies)
	assert.NotNil(t, sparse.Signals)
	assert.NotNil(t, sparse.Conditions)
	assert.NotNil(t, sparse.Actions)
	assert.NotNil(t, sparse.Effects)
	assert.NotNil(t, sparse.Instruments)
}

func TestClone_IsDeep(t *testing.T) {
	t.Parallel()
	original := momentum(t)
	clone, err := original.Clone()
	require.NoError(t, err)

	clone.Dependencies[0].Params["instrument_id"] = "mutated"
	clone.Effects[0] = ir.EffectCallModel
	clone.Actions[1].Intent.Constraints.AllowedVenues[0] = "ORCA"

	assert.Equal(t, "sol_usdc", original.Dependencies[0].Params["instrument_id"])
	assert.Equal(t, ir.EffectCommitPrediction, original.Effects[0])
	assert.Equal(t, "JUPITER", original.Actions[1].Intent.Constraints.AllowedVenues[0])

	_, err = (*ir.IR)(nil).Clone()
	require.ErrorIs(t, err, ir.ErrNilIR)
}

func TestLookupHelpers(t *testing.T) {
	t.Parallel()
	doc := momentum(t)

	dep, ok := doc.Dependency("price")
	require.True(t, ok)
	assert.Equal(t, ir.DepPrice, dep.Kind)
	_, ok = doc.Dependency("absent")
	assert.False(t, ok)

	inst, ok := doc.Instrument("sol_usdc")
	require.True(t, ok)
	assert.Equal(t, fxInstrumentID, inst.InstrumentID)
	_, ok = doc.Instrument("absent")
	assert.False(t, ok)

	trig, ok := doc.Trigger("tick")
	require.True(t, ok)
	assert.Equal(t, ir.TriggerOnInterval, trig.Kind)
	_, ok = doc.Trigger("absent")
	assert.False(t, ok)

	act, ok := doc.Action("buy")
	require.True(t, ok)
	assert.Equal(t, ir.ActionCreateTradeIntent, act.Kind)
	_, ok = doc.Action("absent")
	assert.False(t, ok)
}

func TestRef_Grammar(t *testing.T) {
	t.Parallel()
	valid := []ir.Ref{"a", "abc", "a1", "a_b", "price_5m", ir.Ref("a" + strings.Repeat("b", 63))}
	for _, r := range valid {
		assert.True(t, r.Valid(), "%q should be valid", r)
		assert.Equal(t, string(r), r.String())
	}
	invalid := []ir.Ref{"", "1a", "_a", "A", "aB", "a-b", "a.b", "a b", "a\n", ir.Ref("a" + strings.Repeat("b", 64))}
	for _, r := range invalid {
		assert.False(t, r.Valid(), "%q should be invalid", r)
	}
}

func TestHex_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	h := ir.Hex{0xde, 0xad, 0xbe, 0xef}
	raw, err := json.Marshal(h)
	require.NoError(t, err)
	assert.JSONEq(t, `"deadbeef"`, string(raw), "hex renders lowercase so Go and TypeScript agree")

	var back ir.Hex
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, h, back)
	assert.Equal(t, "deadbeef", h.String())

	var empty ir.Hex
	require.NoError(t, json.Unmarshal([]byte(`""`), &empty))
	assert.Empty(t, empty)

	require.Error(t, json.Unmarshal([]byte(`"zz"`), &back), "non-hex is rejected")
	require.Error(t, json.Unmarshal([]byte(`123`), &back), "a number is not a hash")
}

// TestResponseSchema_IsStructuredOutputSafe: the schema handed to the model
// must satisfy the provider's structured-output subset — no recursion (the
// expression grammar is unrolled), additionalProperties false on every
// object, and a required list on each.
func TestResponseSchema_IsStructuredOutputSafe(t *testing.T) {
	t.Parallel()
	raw := ir.ResponseSchemaJSON()
	require.NotEmpty(t, raw)

	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))

	// The envelope carries the IR plus the clarification channel.
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	for _, key := range []string{"ir", "clarifications_needed", "rationale", "evidence_refs"} {
		assert.Contains(t, props, key)
	}

	// Every object node closes itself.
	var objects int
	var walk func(any)
	walk = func(n any) {
		m, ok := n.(map[string]any)
		if !ok {
			if arr, isArr := n.([]any); isArr {
				for _, e := range arr {
					walk(e)
				}
			}
			return
		}
		if m["type"] == "object" {
			objects++
			assert.Equal(t, false, m["additionalProperties"], "every object must close additionalProperties")
		}
		for _, v := range m {
			walk(v)
		}
	}
	walk(schema)
	assert.Greater(t, objects, 10, "the schema should describe the whole document")

	// The expression grammar is unrolled to a finite depth, never self-referential.
	assert.NotContains(t, string(raw), `"$ref":"#"`, "no self reference")
	assert.Contains(t, string(raw), "$defs", "expression levels are named definitions")
	assert.Contains(t, string(raw), "expr0")
	assert.Contains(t, string(raw), fmt.Sprintf("expr%d", ir.MaxExprDepth-1))
	assert.NotContains(t, string(raw), fmt.Sprintf("expr%d", ir.MaxExprDepth), "unrolling stops at the depth cap")
}

// TestParamPairs_FoldDeterministically: params cross the model boundary as
// a pair array because the structured-output subset cannot express an open
// map. The fold must be order-independent and must refuse a duplicate key
// rather than let the last one silently win.
func TestParamPairs_FoldDeterministically(t *testing.T) {
	t.Parallel()
	params := map[string]string{"instrument_id": "sol_usdc", "window": "5m", "source": "primary"}

	pairs := ir.PairsFromParams(params)
	assert.Equal(t, []ir.ParamPair{
		{Key: "instrument_id", Value: "sol_usdc"},
		{Key: "source", Value: "primary"},
		{Key: "window", Value: "5m"},
	}, pairs, "rendered in sorted key order")

	back, err := ir.ParamsFromPairs(pairs)
	require.NoError(t, err)
	assert.Equal(t, params, back)

	// Reversed input folds to the same map.
	reversed := []ir.ParamPair{pairs[2], pairs[1], pairs[0]}
	fromReversed, err := ir.ParamsFromPairs(reversed)
	require.NoError(t, err)
	assert.Equal(t, params, fromReversed)

	_, err = ir.ParamsFromPairs([]ir.ParamPair{{Key: "k", Value: "a"}, {Key: "k", Value: "b"}})
	require.Error(t, err, "a duplicate key is ambiguous and must be refused")
	assert.Contains(t, err.Error(), "duplicate")

	empty, err := ir.ParamsFromPairs(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
	assert.Empty(t, ir.PairsFromParams(nil))
}
