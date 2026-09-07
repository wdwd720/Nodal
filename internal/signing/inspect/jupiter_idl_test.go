package inspect

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file closes SB-007.
//
// The Jupiter v6 layout in jupiter.go was reproduced from memory of the
// published IDL and was, by its own comment, unverified. Worse, the test that
// existed to catch an error in it — TestLayout_JupiterDiscriminators — asserted
// the same values the code assumes, and the fake in internal/provider/jupiter
// mirrored them too. Three artifacts agreeing because they share one unverified
// source is not corroboration, and a guard that encodes the assumption it
// guards cannot detect the thing it exists for.
//
// The expectations below come from testdata/jupiter_v6_idl.json, which is the
// published Anchor IDL fetched from the jup-ag/jupiter-cpi repository at commit
// 12bc5f67b94a2c3edc74d6e721a19442124a0bad. That repository's src/lib.rs
// carries `anchor_lang::declare_id!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4")`,
// which is exactly the program id this inspector accepts — that is what ties
// the document to the program rather than to a name.
//
// WHAT THIS DOES NOT PROVE, stated because the distinction is the whole point
// of SB-007:
//
//   - It does not prove the deployed program still matches this IDL. An IDL is
//     a published artifact, not the chain. Confirming that needs either the
//     on-chain IDL account or a decoded mainnet transaction, neither of which
//     this build fetches.
//   - Swap variants 39..47 (OpenBookV2 through RaydiumCP) are NOT in this IDL,
//     which predates them. Their payload sizes remain from memory. They are
//     asserted here only to be absent from the IDL, so that a future IDL
//     containing them fails this test and forces a real check.
//   - exact_out_route and the *_v2 instructions are likewise absent. The
//     inspector derives discriminators for them only so a rejection can name
//     them; it never accepts them, so an extra discriminator that matches
//     nothing on chain is inert.
//
// So: the layout the inspector relies on for the two instructions it actually
// accepts is now verified against the published IDL for the right program id.
// Whether the chain agrees with that IDL is a separate question and is recorded
// as such in BLOCKERS.md.

type idlAccount struct {
	Name       string `json:"name"`
	IsMut      bool   `json:"isMut"`
	IsSigner   bool   `json:"isSigner"`
	IsOptional bool   `json:"isOptional"`
}

type idlField struct {
	Name string          `json:"name"`
	Type json.RawMessage `json:"type"`
}

type idlVariant struct {
	Name   string     `json:"name"`
	Fields []idlField `json:"fields"`
}

type jupiterIDL struct {
	Instructions []struct {
		Name     string       `json:"name"`
		Accounts []idlAccount `json:"accounts"`
		Args     []idlField   `json:"args"`
	} `json:"instructions"`
	Types []struct {
		Name string `json:"name"`
		Type struct {
			Kind     string       `json:"kind"`
			Fields   []idlField   `json:"fields"`
			Variants []idlVariant `json:"variants"`
		} `json:"type"`
	} `json:"types"`
}

func loadJupiterIDL(t *testing.T) jupiterIDL {
	t.Helper()
	raw, err := os.ReadFile("testdata/jupiter_v6_idl.json")
	require.NoError(t, err, "the published IDL fixture must be present; it is the only non-circular source in this file")
	var idl jupiterIDL
	require.NoError(t, json.Unmarshal(raw, &idl))
	require.NotEmpty(t, idl.Instructions, "IDL parsed to zero instructions — the fixture or the shape changed, and every assertion below would be vacuous")
	return idl
}

func (i jupiterIDL) accounts(t *testing.T, name string) []idlAccount {
	t.Helper()
	for _, ix := range i.Instructions {
		if ix.Name == name {
			return ix.Accounts
		}
	}
	t.Fatalf("instruction %q is not in the published IDL", name)
	return nil
}

// TestIDL_AccountLayoutsMatchTheInspector pins the account order and
// optionality the inspector decodes against the IDL's own list. The inspector
// documents these in prose; here they are compared to the document.
func TestIDL_AccountLayoutsMatchTheInspector(t *testing.T) {
	t.Parallel()
	idl := loadJupiterIDL(t)

	// The IDL names accounts in camelCase; the inspector's prose uses the
	// on-chain snake_case. Only order and optionality are compared, because
	// those are what a decoder can get wrong.
	route := []struct {
		name     string
		optional bool
	}{
		{"tokenProgram", false},
		{"userTransferAuthority", false},
		{"userSourceTokenAccount", false},
		{"userDestinationTokenAccount", false},
		{"destinationTokenAccount", true},
		{"destinationMint", false},
		{"platformFeeAccount", true},
		{"eventAuthority", false},
		{"program", false},
	}
	shared := []struct {
		name     string
		optional bool
	}{
		{"tokenProgram", false},
		{"programAuthority", false},
		{"userTransferAuthority", false},
		{"sourceTokenAccount", false},
		{"programSourceTokenAccount", false},
		{"programDestinationTokenAccount", false},
		{"destinationTokenAccount", false},
		{"sourceMint", false},
		{"destinationMint", false},
		{"platformFeeAccount", true},
		{"token2022Program", true},
		{"eventAuthority", false},
		{"program", false},
	}

	for _, tc := range []struct {
		ix   string
		want []struct {
			name     string
			optional bool
		}
	}{
		{"route", route},
		{"sharedAccountsRoute", shared},
	} {
		t.Run(tc.ix, func(t *testing.T) {
			got := idl.accounts(t, tc.ix)
			require.Len(t, got, len(tc.want), "account count changed for %s", tc.ix)
			for i, w := range tc.want {
				assert.Equalf(t, w.name, got[i].Name, "%s account %d", tc.ix, i)
				assert.Equalf(t, w.optional, got[i].IsOptional, "%s account %d (%s) optionality", tc.ix, i, w.name)
			}
		})
	}

	// The signer is the transfer authority, and it is the only signer. The
	// inspector's authority check depends on this.
	for _, ix := range []string{"route", "sharedAccountsRoute"} {
		var signers []string
		for _, a := range idl.accounts(t, ix) {
			if a.IsSigner {
				signers = append(signers, a.Name)
			}
		}
		assert.Equalf(t, []string{"userTransferAuthority"}, signers, "%s signers", ix)
	}
}

// TestIDL_RouteArgsMatchTheInspector pins the argument list the inspector
// decodes — in particular that slippage is u16 and platform fee is u8, which
// is what makes the "arguments must end exactly at the end of the data" check
// meaningful.
func TestIDL_RouteArgsMatchTheInspector(t *testing.T) {
	t.Parallel()
	idl := loadJupiterIDL(t)
	for _, ix := range idl.Instructions {
		if ix.Name != "route" {
			continue
		}
		var names, types []string
		for _, a := range ix.Args {
			names = append(names, a.Name)
			types = append(types, string(a.Type))
		}
		assert.Equal(t, []string{"routePlan", "inAmount", "quotedOutAmount", "slippageBps", "platformFeeBps"}, names)
		assert.Equal(t, `"u64"`, types[1], "in_amount")
		assert.Equal(t, `"u64"`, types[2], "quoted_out_amount")
		assert.Equal(t, `"u16"`, types[3], "slippage_bps")
		assert.Equal(t, `"u8"`, types[4], "platform_fee_bps")
		return
	}
	t.Fatal("route is not in the published IDL")
}

// TestIDL_SwapVariantPayloadsMatchTheTable is the assertion that matters most.
//
// swapPayloadSize maps a Swap enum ordinal to its fixed payload size, and a
// wrong size shifts every byte after it — which is how a decoder ends up
// reading a different in_amount or minimum_out than the one that will execute.
// The sizes are derived here from the IDL's own field types rather than
// restated.
func TestIDL_SwapVariantPayloadsMatchTheTable(t *testing.T) {
	t.Parallel()
	idl := loadJupiterIDL(t)

	sizeOf := map[string]int{`"bool"`: 1, `"u8"`: 1, `"u16"`: 2, `"u32"`: 4, `"u64"`: 8}

	var variants []idlVariant
	for _, ty := range idl.Types {
		if ty.Name == "Swap" {
			variants = ty.Type.Variants
		}
	}
	require.NotEmpty(t, variants, "the Swap enum is missing from the IDL")

	for ordinal, v := range variants {
		t.Run(v.Name, func(t *testing.T) {
			want := 0
			for _, f := range v.Fields {
				n, ok := sizeOf[string(f.Type)]
				if !ok {
					// Side is a fieldless enum: one byte on the wire.
					require.Contains(t, string(f.Type), "Side",
						"variant %s field %s has an unmodelled type %s", v.Name, f.Name, f.Type)
					n = 1
				}
				want += n
			}
			got, ok := swapPayloadSize[uint8(ordinal)]
			require.Truef(t, ok, "variant %d (%s) is in the IDL but absent from swapPayloadSize, so it would be rejected", ordinal, v.Name)
			assert.Equalf(t, want, got,
				"variant %d (%s): the IDL says %d payload bytes, swapPayloadSize says %d — a wrong size shifts every byte after it",
				ordinal, v.Name, want, got)
		})
	}
}

// TestIDL_VariantsBeyondThePublishedIDLStayUnverified records the boundary
// rather than hiding it. Ordinals 39 and above are in swapPayloadSize from
// memory and are NOT in this IDL. If a future IDL adds them, this test fails
// and forces someone to check them against it instead of inheriting the guess.
func TestIDL_VariantsBeyondThePublishedIDLStayUnverified(t *testing.T) {
	t.Parallel()
	idl := loadJupiterIDL(t)
	var count int
	for _, ty := range idl.Types {
		if ty.Name == "Swap" {
			count = len(ty.Type.Variants)
		}
	}
	require.Equal(t, 39, count,
		"the published IDL's Swap enum changed size; ordinals >= %d in swapPayloadSize are unverified and must be re-checked against it", count)

	for ordinal := range swapPayloadSize {
		if int(ordinal) >= count {
			t.Logf("ordinal %d is unverified: beyond the published IDL (SB-007 residue)", ordinal)
		}
	}
}
