package risk

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Golden corpus layout:
//
//	testdata/policies/<name>.json  {"version": "...", "rules": {...}}
//	testdata/inputs/<name>.json    a complete Input document
//	testdata/cases/<name>.json     {"policy", "input_base", "input" (patch), "input_after" (patch), "expect"}
//
// Patches deep-merge into the base input; an object carrying "$replace": true
// replaces the base object instead of merging. The policy names MISSING and
// DEFAULT select the zero policy and DefaultGlobalPolicyJSON respectively.

type policyFixture struct {
	Version string          `json:"version"`
	Rules   json.RawMessage `json:"rules"`
}

type caseExpect struct {
	Decision             Verdict     `json:"decision"`
	ReasonCodes          []string    `json:"reason_codes"`
	ActionClass          ActionClass `json:"action_class"`
	EffectiveNotionalUSD string      `json:"effective_notional_usd"`
	MaxNotionalUSD       string      `json:"max_notional_usd"`
	MaxSlippageBPS       *int64      `json:"max_slippage_bps"`
}

type caseFixture struct {
	Name       string          `json:"-"`
	Policy     string          `json:"policy"`
	InputBase  string          `json:"input_base"`
	Input      json.RawMessage `json:"input"`
	InputAfter json.RawMessage `json:"input_after"`
	Expect     caseExpect      `json:"expect"`
}

func loadPolicyFixture(t *testing.T, name string) Policy {
	t.Helper()
	switch name {
	case "MISSING":
		return Policy{}
	case "DEFAULT":
		p := MustParsePolicy([]byte(DefaultGlobalPolicyJSON))
		p.Version = "default-v0"
		return p
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "policies", name+".json"))
	require.NoError(t, err)
	var pf policyFixture
	require.NoError(t, json.Unmarshal(raw, &pf))
	require.NotEmpty(t, pf.Version)
	p, err := ParsePolicy(pf.Rules)
	require.NoError(t, err)
	p.Version = pf.Version
	return p
}

func loadCases(t *testing.T) []caseFixture {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "cases", "*.json"))
	require.NoError(t, err)
	sort.Strings(files)
	out := make([]caseFixture, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		var c caseFixture
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		require.NoError(t, dec.Decode(&c), f)
		c.Name = strings.TrimSuffix(filepath.Base(f), ".json")
		out = append(out, c)
	}
	return out
}

func decodeGeneric(t *testing.T, raw []byte) any {
	t.Helper()
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v))
	return v
}

func caseInput(t *testing.T, c caseFixture) Input {
	t.Helper()
	var base any = map[string]any{}
	if c.InputBase != "" {
		raw, err := os.ReadFile(filepath.Join("testdata", "inputs", c.InputBase+".json"))
		require.NoError(t, err)
		base = decodeGeneric(t, raw)
	}
	merged := base
	for _, patch := range [][]byte{c.Input, c.InputAfter} {
		if p := decodeGeneric(t, patch); p != nil {
			merged = deepMerge(merged, p)
		}
	}
	b, err := json.Marshal(merged)
	require.NoError(t, err)
	var in Input
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&in), c.Name)
	return in
}

func deepMerge(base, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	if r, ok := pm["$replace"].(bool); ok && r {
		out := make(map[string]any, len(pm))
		for k, v := range pm {
			if k != "$replace" {
				out[k] = v
			}
		}
		return out
	}
	bm, _ := base.(map[string]any)
	out := make(map[string]any, len(bm)+len(pm))
	for k, v := range bm {
		out[k] = v
	}
	for k, v := range pm {
		out[k] = deepMerge(out[k], v)
	}
	return out
}

func baseInput(t *testing.T) Input {
	t.Helper()
	return caseInput(t, caseFixture{Name: "base", InputBase: "new_risk"})
}
