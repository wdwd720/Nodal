package ir_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/strategy/ir"
)

// FuzzParseIR: arbitrary bytes never panic, and anything ParseIR accepts is
// a document the rest of the platform can trust — it validates, its effect
// set is closed and equals the derived set, and it hashes.
//
// The compiler feeds this function raw model output, so "never panics" is a
// availability property of the compile path, not a nicety.
func FuzzParseIR(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"schema_version":1}`))
	f.Add([]byte(`{"schema_version":1,"effects":["TRANSFER_VALUE"]}`))
	f.Add([]byte(`{"schema_version":1,"signals":[{"name":"a","expr":{"signal":"a"},"scale":4,"rounding":"half_even"}]}`))
	f.Add([]byte(strings.Repeat(`{"a":`, 200) + "1" + strings.Repeat("}", 200)))
	f.Add([]byte(`{"schema_version":1,"dependencies":[{"name":"d","kind":"PRICE","max_age_ms":-1}]}`))

	// A real document, and the shapes either side of it.
	seed := func(mutate func(*ir.IR)) []byte {
		doc := &ir.IR{
			SchemaVersion: ir.SchemaVersion,
			StrategyID:    fxStrategyID,
			Owner:         ir.Owner{AccountID: fxAccountID, UserID: fxUserID},
		}
		if mutate != nil {
			mutate(doc)
		}
		doc.Normalize()
		b, err := json.Marshal(doc)
		if err != nil {
			f.Fatal(err)
		}
		return b
	}
	f.Add(seed(nil))
	f.Add(seed(func(d *ir.IR) { d.Effects = []ir.Effect{ir.EffectExportSecret} }))

	f.Fuzz(func(t *testing.T, raw []byte) {
		doc, err := ir.ParseIR(raw)
		if err != nil {
			// Every rejection is a typed validation failure carrying a stage,
			// never a bare panic or an untyped error.
			var ve *ir.ValidationError
			if require.ErrorAs(t, err, &ve); ve != nil {
				require.Contains(t, []string{"PARSE", "STRUCTURAL", "EFFECT"}, ve.Stage)
				require.NotEmpty(t, ve.Codes(), "a rejection must name at least one code")
			}
			require.Nil(t, doc, "a rejected document is never returned")
			return
		}

		require.NotNil(t, doc)

		// An accepted document satisfies every invariant the runtime relies on.
		require.Equal(t, ir.SchemaVersion, doc.SchemaVersion)
		require.NoError(t, ir.Check(doc), "ParseIR accepted a document that does not re-validate")

		derived := ir.DeriveEffects(doc)
		require.True(t, ir.EffectsEqual(doc.Effects, derived), "accepted document has an effect set that is not the derived set")
		for _, e := range doc.Effects {
			require.True(t, e.Allowed(), "accepted document declares the non-allowed effect %q", e)
			require.False(t, e.Forbidden(), "accepted document declares the forbidden effect %q", e)
		}

		// It hashes, and re-parsing its own encoding is stable.
		h1, err := ir.SemanticHash(doc)
		require.NoError(t, err)
		require.Len(t, h1, 32)

		encoded, err := json.Marshal(doc)
		require.NoError(t, err)
		again, err := ir.ParseIR(encoded)
		require.NoError(t, err, "an accepted document did not survive a round trip")
		h2, err := ir.SemanticHash(again)
		require.NoError(t, err)
		require.Equal(t, h1, h2, "round trip changed the semantic hash")
	})
}

// FuzzDecode covers the parser alone, without the structural stage, so
// crashes in the strict decoder are found even when a document would be
// rejected later anyway.
func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"schema_version":1}`))
	f.Add([]byte(`{"built_at":"not-a-time"}`))
	f.Add([]byte(`{"hash":"zzzz"}`))
	f.Add([]byte(`{"triggers":[{"every_ms":9223372036854775807}]}`))
	f.Add([]byte(`{"envelope":{"max_single_trade":"not-money"}}`))
	f.Add([]byte(`{"signals":[{"expr":{"const":{"m":"1","s":255}}}]}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		doc, err := ir.Decode(raw)
		if err != nil {
			var ve *ir.ValidationError
			if require.ErrorAs(t, err, &ve); ve != nil {
				require.Equal(t, "PARSE", ve.Stage)
			}
			return
		}
		require.NotNil(t, doc)
		// Whatever decoded, normalising it twice is stable and it re-encodes.
		first, err := json.Marshal(doc)
		require.NoError(t, err)
		doc.Normalize()
		second, err := json.Marshal(doc)
		require.NoError(t, err)
		require.JSONEq(t, string(first), string(second), "Decode did not leave the document normalised")
	})
}
