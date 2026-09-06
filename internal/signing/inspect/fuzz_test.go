package inspect_test

import (
	"bytes"
	"testing"

	"github.com/gagliardetto/solana-go"

	"github.com/nodal/controlplane/internal/signing/inspect"
	"github.com/nodal/controlplane/internal/signing/signingtest"
)

// fuzzSeeds returns the golden transactions plus a set of mutated variants
// (unknown programs, extra transfers, malicious delegate, authority updates,
// unexpected token accounts, malformed instructions, Token-2022 extensions;
// PART 152) as seed corpus.
func fuzzSeeds() [][]byte {
	s := signingtest.NewSwap()
	n := signingtest.NewNativeInputSwap()
	acct := solana.NewAccountMeta
	seeds := [][]byte{
		s.Golden(inspect.VersionLegacy),
		s.Golden(inspect.VersionV0),
		n.Golden(inspect.VersionLegacy),
		n.Golden(inspect.VersionV0),
	}
	extra := [][]solana.Instruction{
		append(s.GoldenInstructions(), solana.NewInstruction(signingtest.PubkeyFromSeed("evil"), solana.AccountMetaSlice{acct(s.Wallet, true, true)}, []byte{1})),
		append(s.GoldenInstructions(), solana.NewInstruction(signingtest.SystemProgram, solana.AccountMetaSlice{acct(s.Wallet, true, true), acct(s.Foreign, true, false)}, signingtest.SystemTransferData(1))),
		append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram, solana.AccountMetaSlice{acct(s.InputATA, true, false), acct(s.Foreign, false, false), acct(s.Wallet, false, true)}, signingtest.TokenApproveData(1))),
		append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram, solana.AccountMetaSlice{acct(s.OutputATA, true, false), acct(s.Wallet, false, true)}, signingtest.TokenSetAuthorityData(2, s.Foreign))),
		append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram, solana.AccountMetaSlice{acct(s.InputATA, true, false), acct(s.Foreign, true, false), acct(s.Wallet, false, true)}, signingtest.TokenTransferData(5))),
		append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram, solana.AccountMetaSlice{acct(s.InputATA, true, false)}, []byte{3, 1})),
		append(s.GoldenInstructions(), solana.NewInstruction(signingtest.Token2022Program, solana.AccountMetaSlice{acct(s.OutputATA, true, false)}, []byte{36, 0})),
		append(s.GoldenInstructions(), s.CloseIx(s.OutputATA, s.Foreign)),
		append(s.GoldenInstructions(), s.RouteIx()),
		{s.RouteIx()},
	}
	for _, ixs := range extra {
		if raw, err := s.Build(inspect.VersionLegacy, ixs); err == nil {
			seeds = append(seeds, raw)
		}
		if raw, err := s.Build(inspect.VersionV0, ixs); err == nil {
			seeds = append(seeds, raw)
		}
	}
	return seeds
}

// FuzzDecode: Decode never panics and either fails or yields a structurally
// consistent transaction.
func FuzzDecode(f *testing.F) {
	for _, seed := range fuzzSeeds() {
		f.Add(seed)
	}
	f.Add([]byte{})
	f.Add([]byte{0x81})
	f.Add(bytes.Repeat([]byte{0xff}, 64))
	f.Fuzz(func(t *testing.T, raw []byte) {
		tx, err := inspect.Decode(raw)
		if err != nil {
			return
		}
		if tx.NumSignatures != int(tx.Header.NumRequiredSignatures) {
			t.Fatalf("signature count %d != header %d", tx.NumSignatures, tx.Header.NumRequiredSignatures)
		}
		if len(tx.StaticKeys) == 0 || len(tx.Instructions) == 0 {
			t.Fatal("decoded transaction without keys or instructions")
		}
		if tx.Version != inspect.VersionLegacy && tx.Version != inspect.VersionV0 {
			t.Fatalf("unexpected version %q", tx.Version)
		}
		if tx.Hash != inspect.TxHash(raw) {
			t.Fatal("hash mismatch")
		}
	})
}

// FuzzInspect: Decode+Inspect never panics; the untouched golden transactions
// are approved; anything approved satisfies the structural invariants the
// checks promise.
func FuzzInspect(f *testing.F) {
	for _, seed := range fuzzSeeds() {
		f.Add(seed)
	}
	// Fixtures and expectations are built once: they are immutable inputs of
	// every iteration (Inspect takes them by value and never mutates them).
	type target struct {
		wallet  solana.PublicKey
		version inspect.Version
		golden  []byte
		exp     inspect.Expectations
	}
	var targets []target
	for _, s := range []*signingtest.Swap{signingtest.NewSwap(), signingtest.NewNativeInputSwap()} {
		for _, version := range []inspect.Version{inspect.VersionLegacy, inspect.VersionV0} {
			golden := s.Golden(version)
			targets = append(targets, target{wallet: s.Wallet, version: version, golden: golden, exp: s.Expectations(golden)})
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		tx, err := inspect.Decode(raw)
		if err != nil {
			if inspect.RejectAll(inspect.ReasonDecodeError, err.Error()).Approved {
				t.Fatal("RejectAll approved")
			}
			return
		}
		for _, tg := range targets {
			res := inspect.Inspect(tx, tg.exp)
			if len(res.Checks) != len(inspect.CheckNames) {
				t.Fatalf("%d checks", len(res.Checks))
			}
			if bytes.Equal(raw, tg.golden) && !res.Approved {
				t.Fatalf("golden %s rejected: %v", tg.version, res.ReasonCodes)
			}
			if res.Approved {
				if len(res.ReasonCodes) != 0 {
					t.Fatal("approved with reason codes")
				}
				if !bytes.Equal(raw, tg.golden) {
					// The simulation hash binds the exact bytes and every account
					// and instruction is checked, so nothing but the golden bytes
					// can be approved under the golden expectations.
					t.Fatalf("mutated bytes approved for %s", tg.version)
				}
				if !tx.StaticKeys[0].Equals(tg.wallet) {
					t.Fatal("approved with a foreign fee payer")
				}
			}
		}
	})
}
