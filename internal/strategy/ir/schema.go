package ir

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// ResponseSchema is the JSON schema handed to the model as the constrained
// output format for a compile request (STRATEGY_IR.md §4). It is the
// structured-output subset the Anthropic API accepts: no recursion (the
// expression grammar is unrolled to MaxExprDepth levels), no numeric
// ranges (enforced after parsing by Structural and the TYPE stage), and
// additionalProperties:false everywhere.
//
// The document the model returns is only a candidate: Hash, Version,
// BuiltAt and Lineage are discarded and set by the compiler.
func ResponseSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ir": IRSchema(),
			"clarifications_needed": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Non-empty when the request is ambiguous; no version is created.",
			},
			"rationale": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary":     map[string]any{"type": "string"},
					"assumptions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required":             []string{"summary", "assumptions"},
				"additionalProperties": false,
			},
			"evidence_refs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []string{"ir", "clarifications_needed", "rationale", "evidence_refs"},
		"additionalProperties": false,
	}
}

// ResponseSchemaJSON renders ResponseSchema as JSON bytes.
func ResponseSchemaJSON() json.RawMessage {
	b, err := json.Marshal(ResponseSchema())
	if err != nil {
		// A constant schema cannot fail to encode; returning nil would only
		// hide a programming error until a model call.
		panic("ir: response schema: " + err.Error())
	}
	return b
}

// ParamPair is one dependency parameter as the model-facing schema requests
// it. The provider's structured-output subset cannot express an open map
// (additionalProperties must be false), so params cross the model boundary
// as a pair array and are folded into Dependency.Params here.
type ParamPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ParamsFromPairs folds a candidate's pair array into the IR's params map.
// A duplicate key is an error rather than a last-one-wins overwrite: which
// value survived would otherwise decide what the strategy reads.
func ParamsFromPairs(pairs []ParamPair) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if _, dup := out[p.Key]; dup {
			return nil, fmt.Errorf("ir: duplicate dependency param %q", p.Key)
		}
		out[p.Key] = p.Value
	}
	return out, nil
}

// PairsFromParams renders a params map as a sorted pair array, so a
// document round-trips through the model-facing shape deterministically.
func PairsFromParams(params map[string]string) []ParamPair {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ParamPair, 0, len(keys))
	for _, k := range keys {
		out = append(out, ParamPair{Key: k, Value: params[k]})
	}
	return out
}

// IRSchema is the JSON schema of one schema-1 IR document (the candidate
// shape). Money is a string with two decimals, decimals are {m, s}, hashes
// are hex strings.
func IRSchema() map[string]any {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	boolean := map[string]any{"type": "boolean"}
	strArray := map[string]any{"type": "array", "items": str}
	refArray := map[string]any{"type": "array", "items": str}
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	enum := func(vals ...string) map[string]any { return map[string]any{"type": "string", "enum": vals} }
	decimal := obj(map[string]any{"m": str, "s": integer}, "m", "s")
	expr := map[string]any{"$ref": "#/$defs/expr0"}

	sizing := obj(map[string]any{
		"kind":         enum(string(SizingNone), string(SizingFixedNotional), string(SizingEnvelopeFractionBPS), string(SizingTargetExposure)),
		"notional_usd": str, "fraction_bps": integer, "target_usd": str,
	}, "kind")
	constraints := obj(map[string]any{
		"max_slippage_bps": integer, "max_fee_bps": integer, "max_price_impact_bps": integer,
		"quote_freshness_ms": integer, "allowed_venues": strArray,
	}, "max_slippage_bps", "max_fee_bps", "max_price_impact_bps", "quote_freshness_ms", "allowed_venues")

	schema := obj(map[string]any{
		"schema_version": integer,
		"strategy_id":    str,
		"version":        integer,
		"hash":           str,
		"owner":          obj(map[string]any{"account_id": str, "user_id": str}, "account_id", "user_id"),
		"instruments":    map[string]any{"type": "array", "items": obj(map[string]any{"name": str, "instrument_id": str}, "name", "instrument_id")},
		"triggers": map[string]any{"type": "array", "items": obj(map[string]any{
			"name": str, "kind": enum(string(TriggerOnEvent), string(TriggerOnInterval)), "event_type": str,
			"filter": expr, "every_ms": integer, "dedup_window_ms": integer,
		}, "name", "kind", "dedup_window_ms")},
		"dependencies": map[string]any{"type": "array", "items": obj(map[string]any{
			"name": str, "kind": enum(string(DepPrice), string(DepOnchain), string(DepWalletEvent), string(DepSocial), string(DepWalletIntelligence), string(DepModel), string(DepFeature)),
			"tool_code": str, "tool_version": integer, "dependency_version": integer,
			// The provider's structured-output subset requires
			// additionalProperties to be literally false, so an open
			// string→string map is not expressible. Params are requested as a
			// key/value pair array and folded into Dependency.Params by the
			// compiler, which rejects duplicate keys.
			"params":     map[string]any{"type": "array", "items": obj(map[string]any{"key": str, "value": str}, "key", "value")},
			"max_age_ms": integer, "required": boolean,
		}, "name", "kind", "tool_code", "tool_version", "dependency_version", "params", "max_age_ms", "required")},
		"signals": map[string]any{"type": "array", "items": obj(map[string]any{
			"name": str, "expr": expr, "scale": integer, "rounding": str,
		}, "name", "expr", "scale", "rounding")},
		"conditions": map[string]any{"type": "array", "items": obj(map[string]any{"name": str, "expr": expr}, "name", "expr")},
		"actions": map[string]any{"type": "array", "items": obj(map[string]any{
			"name": str, "kind": enum(string(ActionCallModel), string(ActionCommitPrediction), string(ActionCreateTradeIntent)), "when": str,
			"model": obj(map[string]any{
				"template_version": str, "output_schema": str, "inputs": refArray, "max_output_tokens": integer, "required": boolean,
			}, "template_version", "output_schema", "inputs", "max_output_tokens", "required"),
			"prediction": obj(map[string]any{
				"instrument": str, "horizon_ms": integer, "direction": enum(string(DirectionUp), string(DirectionDown), string(DirectionFlat)),
				"probability": expr, "expected_return_bps": expr, "downside_probability": expr, "max_downside_bps": expr, "confidence": expr,
			}, "instrument", "horizon_ms", "direction", "probability", "expected_return_bps", "downside_probability", "max_downside_bps", "confidence"),
			"intent": obj(map[string]any{
				"action": enum("ACQUIRE_NOTIONAL", "REDUCE_NOTIONAL", "CLOSE_POSITION", "TARGET_EXPOSURE"), "instrument": str,
				"sizing": sizing, "constraints": constraints, "deadline_ms": integer, "prediction": str,
			}, "action", "instrument", "sizing", "constraints", "deadline_ms", "prediction"),
		}, "name", "kind")},
		"risk_policy": obj(map[string]any{"version": str, "hash": str}, "version", "hash"),
		"model_budget": obj(map[string]any{
			"required": boolean, "providers": strArray, "max_calls_per_run": integer, "max_calls_per_day": integer,
			"max_input_tokens": integer, "max_output_tokens": integer, "max_spend_per_day": str,
		}, "required", "providers", "max_calls_per_run", "max_calls_per_day", "max_input_tokens", "max_output_tokens", "max_spend_per_day"),
		"data_budget": obj(map[string]any{
			"max_tool_calls_per_run": integer, "max_tool_calls_per_day": integer, "max_spend_per_day": str, "max_lookback_ms": integer,
		}, "max_tool_calls_per_run", "max_tool_calls_per_day", "max_spend_per_day", "max_lookback_ms"),
		"envelope": obj(map[string]any{
			"min_allocation": str, "max_single_trade": str, "max_position": str, "max_daily_loss": str,
			"instruments": strArray, "asset_classes": strArray, "venues": strArray,
			"max_intents_per_hour": integer, "max_runs_per_minute": integer,
		}, "min_allocation", "max_single_trade", "max_position", "max_daily_loss", "instruments", "asset_classes", "venues", "max_intents_per_hour", "max_runs_per_minute"),
		// Effects are free strings so the model can name a capability the user
		// asked for even when it is forbidden; the EFFECT stage then rejects it
		// with a precise code instead of a vague structural failure.
		"effects": strArray,
		"lineage": obj(map[string]any{
			"source": str, "source_hash": str, "compile_attempt_id": str, "parent_version_id": str, "compiler_version": str, "sdk_version": str,
		}),
		"built_at": str,
	}, "schema_version", "strategy_id", "owner", "instruments", "triggers", "dependencies", "signals", "conditions", "actions",
		"risk_policy", "model_budget", "data_budget", "envelope", "effects")

	defs := map[string]any{}
	for level := 0; level < MaxExprDepth; level++ {
		defs[exprDefName(level)] = exprLevelSchema(level, decimal, str, integer, enum, obj)
	}
	schema["$defs"] = defs
	return schema
}

func exprDefName(level int) string { return "expr" + strconv.Itoa(level) }

// exprLevelSchema unrolls the recursive expression grammar: level k may
// contain level k+1 operands; the deepest level is leaves only.
func exprLevelSchema(level int, decimal, str, integer map[string]any, enum func(...string) map[string]any, obj func(map[string]any, ...string) map[string]any) map[string]any {
	props := map[string]any{
		"const":  decimal,
		"field":  obj(map[string]any{"dependency": str, "path": str, "scale": integer}, "dependency", "path", "scale"),
		"signal": str,
	}
	if level < MaxExprDepth-1 {
		next := map[string]any{"$ref": "#/$defs/" + exprDefName(level+1)}
		props["bin"] = obj(map[string]any{
			"op": enum(string(OpAdd), string(OpSub), string(OpMul), string(OpDiv), string(OpMin), string(OpMax)),
			"l":  next, "r": next, "scale": integer, "rounding": str,
		}, "op", "l", "r", "scale", "rounding")
		props["window"] = obj(map[string]any{
			"fn":         enum(string(WinSMA), string(WinEMA), string(WinMax), string(WinMin), string(WinSum), string(WinCount), string(WinReturn), string(WinStddev)),
			"dependency": str, "path": str, "lookback_ms": integer, "scale": integer, "rounding": str,
		}, "fn", "dependency", "path", "lookback_ms", "scale", "rounding")
		props["cmp"] = obj(map[string]any{
			"op": enum(string(CmpLT), string(CmpLE), string(CmpGT), string(CmpGE), string(CmpEQ), string(CmpNE)),
			"l":  next, "r": next,
		}, "op", "l", "r")
		props["and"] = map[string]any{"type": "array", "items": next}
		props["or"] = map[string]any{"type": "array", "items": next}
		props["not"] = next
	}
	return obj(props)
}
