package inspect_test

import (
	"crypto/sha256"
	"sort"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/signing/inspect"
	"github.com/nodal/controlplane/internal/signing/signingtest"
)

func requireApproved(t *testing.T, res inspect.Result) {
	t.Helper()
	if !res.Approved {
		var failed []string
		for _, c := range res.Checks {
			if !c.Passed {
				failed = append(failed, c.Name+" -> "+c.Detail)
			}
		}
		t.Fatalf("expected approval, got %v:\n%s", res.ReasonCodes, strings.Join(failed, "\n"))
	}
	require.Empty(t, res.ReasonCodes)
	require.Len(t, res.Checks, len(inspect.CheckNames))
}

func checkByName(res inspect.Result, name string) inspect.Check {
	for _, c := range res.Checks {
		if c.Name == name {
			return c
		}
	}
	return inspect.Check{}
}

func TestGolden_Approved(t *testing.T) {
	t.Parallel()
	for _, version := range []inspect.Version{inspect.VersionLegacy, inspect.VersionV0} {
		t.Run(string(version), func(t *testing.T) {
			t.Parallel()
			s := signingtest.NewSwap()
			raw := s.Golden(version)
			tx, err := inspect.Decode(raw)
			require.NoError(t, err)
			assert.Equal(t, version, tx.Version)
			if version == inspect.VersionV0 {
				require.Len(t, tx.LookupTables, 1, "pool accounts must be loaded from the lookup table")
			}
			res := inspect.Inspect(tx, s.Expectations(raw))
			requireApproved(t, res)
			assert.Contains(t, checkByName(res, inspect.CheckMinOutput).Detail, "route minimum out")
			assert.Contains(t, checkByName(res, inspect.CheckComputeBudget).Detail, "priority fee 400 lamports")
		})
	}
	t.Run("native input wrap/sync/close", func(t *testing.T) {
		t.Parallel()
		for _, version := range []inspect.Version{inspect.VersionLegacy, inspect.VersionV0} {
			s := signingtest.NewNativeInputSwap()
			raw := s.Golden(version)
			tx, err := inspect.Decode(raw)
			require.NoError(t, err)
			requireApproved(t, inspect.Inspect(tx, s.Expectations(raw)))
		}
	})
	t.Run("shared accounts route", func(t *testing.T) {
		t.Parallel()
		s := signingtest.NewSwap()
		ixs := append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.SharedAccountsRouteIx())
		raw := s.MustBuild(inspect.VersionV0, ixs)
		tx, err := inspect.Decode(raw)
		require.NoError(t, err)
		requireApproved(t, inspect.Inspect(tx, s.Expectations(raw)))
	})
	t.Run("token-2022 output explicitly enabled", func(t *testing.T) {
		t.Parallel()
		s := signingtest.NewSwap()
		s.OutputProgram = signingtest.Token2022Program
		s.Derive()
		raw := s.Golden(inspect.VersionLegacy)
		tx, err := inspect.Decode(raw)
		require.NoError(t, err)
		exp := s.Expectations(raw)
		exp.Token2022Allowed = true
		exp.AllowedPrograms = append(exp.AllowedPrograms, inspect.Token2022Program)
		requireApproved(t, inspect.Inspect(tx, exp))
		// The same transaction with Token-2022 disabled rejects.
		exp.Token2022Allowed = false
		exp.AllowedPrograms = s.Expectations(raw).AllowedPrograms
		res := inspect.Inspect(tx, exp)
		require.False(t, res.Approved)
		assert.Contains(t, res.ReasonCodes, inspect.ReasonToken2022Disabled)
	})
	t.Run("simulation not required when policy says so", func(t *testing.T) {
		t.Parallel()
		s := signingtest.NewSwap()
		raw := s.Golden(inspect.VersionLegacy)
		tx, err := inspect.Decode(raw)
		require.NoError(t, err)
		exp := s.Expectations(raw)
		exp.Simulation, exp.RequireSimulation = nil, false
		requireApproved(t, inspect.Inspect(tx, exp))
	})
}

// mutation builds a rejected variant from the golden fixture.
type mutation struct {
	name    string
	version inspect.Version
	native  bool
	// build returns the instruction list (nil = golden) and may tweak the
	// fixture before expectations are derived.
	build func(s *signingtest.Swap) []solana.Instruction
	// tweak adjusts the expectations after they were derived from the fixture.
	tweak func(s *signingtest.Swap, raw []byte, exp *inspect.Expectations)
	// payer overrides the fee payer key (signs the message too).
	payer *solana.PrivateKey
	// want lists (check, reason) pairs that must fail.
	want [][2]string
}

func TestRejections(t *testing.T) {
	t.Parallel()
	other := signingtest.KeyFromSeed("other-signer")
	otherPub := other.PublicKey()
	sysAcct := solana.NewAccountMeta

	muts := []mutation{
		{
			name: "extra SystemProgram transfer from wallet",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.SystemProgram,
					solana.AccountMetaSlice{sysAcct(s.Wallet, true, true), sysAcct(s.Foreign, true, false)}, signingtest.SystemTransferData(1_000)))
			},
			want: [][2]string{{inspect.CheckNoSystemTransfer, inspect.ReasonSystemTransfer}, {inspect.CheckNoUnexpectedDestination, inspect.ReasonUnexpectedDestination}},
		},
		{
			name: "token transfer to foreign destination",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram, solana.AccountMetaSlice{
					sysAcct(s.InputATA, true, false), sysAcct(s.InputMint, false, false), sysAcct(s.Foreign, true, false), sysAcct(s.Wallet, false, true),
				}, signingtest.TokenTransferCheckedData(1, 6)))
			},
			want: [][2]string{{inspect.CheckNoUnexpectedDestination, inspect.ReasonUnexpectedDestination}, {inspect.CheckInputDebitBound, inspect.ReasonInputDebitExceeded}},
		},
		{
			name: "approve delegate",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram, solana.AccountMetaSlice{
					sysAcct(s.InputATA, true, false), sysAcct(s.Foreign, false, false), sysAcct(s.Wallet, false, true),
				}, signingtest.TokenApproveData(1)))
			},
			want: [][2]string{{inspect.CheckNoAuthorityChange, inspect.ReasonDelegation}},
		},
		{
			name: "set authority",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram, solana.AccountMetaSlice{
					sysAcct(s.OutputATA, true, false), sysAcct(s.Wallet, false, true),
				}, signingtest.TokenSetAuthorityData(2, s.Foreign)))
			},
			want: [][2]string{{inspect.CheckNoAuthorityChange, inspect.ReasonAuthorityChange}},
		},
		{
			name: "close output account to foreign destination",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), s.CloseIx(s.OutputATA, s.Foreign))
			},
			want: [][2]string{{inspect.CheckNoAuthorityChange, inspect.ReasonUnexpectedClose}, {inspect.CheckNoUnexpectedDestination, inspect.ReasonUnexpectedDestination}},
		},
		{
			name: "close non-native account to wallet",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), s.CloseIx(s.OutputATA, s.Wallet))
			},
			want: [][2]string{{inspect.CheckNoAuthorityChange, inspect.ReasonUnexpectedClose}},
		},
		{
			name: "freeze account",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram, solana.AccountMetaSlice{
					sysAcct(s.OutputATA, true, false), sysAcct(s.OutputMint, false, false), sysAcct(s.Wallet, false, true),
				}, signingtest.TokenSingleByteData(10)))
			},
			want: [][2]string{{inspect.CheckNoAuthorityChange, inspect.ReasonAuthorityChange}},
		},
		{
			name: "unknown program",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.PubkeyFromSeed("evil-program"),
					solana.AccountMetaSlice{sysAcct(s.Wallet, true, true)}, []byte{1, 2, 3}))
			},
			want: [][2]string{{inspect.CheckProgramAllowlist, inspect.ReasonProgramNotAllowed}, {inspect.CheckNoArbitraryCPI, inspect.ReasonProgramNotAllowed}},
		},
		{
			name: "top-level instruction to a per-plan DEX program",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(s.DEXProgram, solana.AccountMetaSlice{sysAcct(s.Wallet, true, true)}, []byte{9}))
			},
			want: [][2]string{{inspect.CheckNoArbitraryCPI, inspect.ReasonUnknownInstruction}},
		},
		{
			name: "Token-2022 instruction while not allowlisted",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.Token2022Program,
					solana.AccountMetaSlice{sysAcct(s.OutputATA, true, false)}, signingtest.TokenSingleByteData(17)))
			},
			want: [][2]string{{inspect.CheckProgramAllowlist, inspect.ReasonProgramNotAllowed}},
		},
		{
			name: "Token-2022 output asset when disabled",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.OutputTokenProgram = inspect.Token2022Program
			},
			want: [][2]string{{inspect.CheckTokenProgram, inspect.ReasonToken2022Disabled}},
		},
		{
			name: "ATA created under Token-2022 while disabled",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), s.ATACreateIx(s.Wallet, s.OutputMint, signingtest.Token2022Program))
			},
			want: [][2]string{{inspect.CheckTokenProgram, inspect.ReasonToken2022Disabled}},
		},
		{
			name:  "wrong fee payer",
			payer: &other,
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.SystemProgram,
					solana.AccountMetaSlice{sysAcct(otherPub, true, true), sysAcct(s.Wallet, true, false)}, signingtest.SystemTransferData(1)))
			},
			want: [][2]string{{inspect.CheckFeePayer, inspect.ReasonFeePayerMismatch}, {inspect.CheckSigners, inspect.ReasonExtraSigner}},
		},
		{
			name: "extra signer",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.SystemProgram,
					solana.AccountMetaSlice{sysAcct(otherPub, true, true), sysAcct(s.Wallet, true, false)}, signingtest.SystemTransferData(1)))
			},
			want: [][2]string{{inspect.CheckSigners, inspect.ReasonExtraSigner}, {inspect.CheckNoSystemTransfer, inspect.ReasonSystemTransfer}},
		},
		{
			name: "route authority is not the wallet",
			build: func(s *signingtest.Swap) []solana.Instruction {
				acc := s.DefaultRouteAccounts()
				acc.Authority = otherPub
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIxWith(acc, s.DefaultRouteArgs()))
			},
			want: [][2]string{{inspect.CheckSigners, inspect.ReasonSignerMismatch}, {inspect.CheckSigners, inspect.ReasonExtraSigner}},
		},
		{
			name: "min output below plan",
			build: func(s *signingtest.Swap) []solana.Instruction {
				s.QuotedOut = s.MinOutput // 0.5% slippage puts the guaranteed minimum below the plan
				return nil
			},
			want: [][2]string{{inspect.CheckMinOutput, inspect.ReasonMinOutputBelowPlan}},
		},
		{
			name: "expired blockhash",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.CurrentBlockHeight = exp.LastValidBlockHeight - exp.BlockHeightMargin + 1
			},
			want: [][2]string{{inspect.CheckBlockhash, inspect.ReasonBlockhashExpired}},
		},
		{
			name: "blockhash mismatch",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.RecentBlockhash = signingtest.PubkeyFromSeed("another-blockhash").String()
			},
			want: [][2]string{{inspect.CheckBlockhash, inspect.ReasonBlockhashMismatch}},
		},
		{
			name: "oversized priority fee",
			build: func(s *signingtest.Swap) []solana.Instruction {
				s.CUPrice = 1_000_000 // 400_000 lamports
				return nil
			},
			want: [][2]string{{inspect.CheckComputeBudget, inspect.ReasonPriorityFeeExceeded}},
		},
		{
			name: "compute units above bound",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.MaxComputeUnits = 300_000
			},
			want: [][2]string{{inspect.CheckComputeBudget, inspect.ReasonComputeUnitsExceeded}},
		},
		{
			name: "duplicate compute budget instructions",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.ComputeBudgetProgram, nil, signingtest.ComputeUnitPriceData(1)))
			},
			want: [][2]string{{inspect.CheckComputeBudget, inspect.ReasonComputeBudgetDuplicate}},
		},
		{
			name: "plan hash mismatch",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				h := sha256.Sum256([]byte("another-plan"))
				exp.Claimed.PlanHash = h[:]
			},
			want: [][2]string{{inspect.CheckPlanIdentity, inspect.ReasonPlanHashMismatch}},
		},
		{
			name: "quote id mismatch",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.Claimed.QuoteID = "0192f7a0-1b2c-7d3e-8f4a-5b6c7d8e9f99"
			},
			want: [][2]string{{inspect.CheckPlanIdentity, inspect.ReasonQuoteIDMismatch}},
		},
		{
			name: "slippage above bound",
			build: func(s *signingtest.Swap) []solana.Instruction {
				s.SlippageBPS = 500
				return nil
			},
			want: [][2]string{{inspect.CheckSlippage, inspect.ReasonSlippageExceeded}},
		},
		{
			name: "input amount above reservation",
			build: func(s *signingtest.Swap) []solana.Instruction {
				s.InAmount = s.MaxInputDebit + 1
				return nil
			},
			want: [][2]string{{inspect.CheckInputDebitBound, inspect.ReasonInputDebitExceeded}},
		},
		{
			name: "zero input amount",
			build: func(s *signingtest.Swap) []solana.Instruction {
				s.InAmount = 0
				return nil
			},
			want: [][2]string{{inspect.CheckInputDebitBound, inspect.ReasonInputAmountZero}},
		},
		{
			name: "simulation output delta below minimum",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.Simulation.TokenDeltas[1].Delta = money.QuantityFromInt64(1)
			},
			want: [][2]string{{inspect.CheckSimulation, inspect.ReasonSimulationDelta}},
		},
		{
			name: "simulation input debit above bound",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.Simulation.TokenDeltas[0].Delta = exp.MaxInputDebit.Add(money.QuantityFromInt64(1)).Neg()
			},
			want: [][2]string{{inspect.CheckSimulation, inspect.ReasonSimulationDelta}},
		},
		{
			name: "simulation drains an unexpected mint",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.Simulation.TokenDeltas = append(exp.Simulation.TokenDeltas, inspect.TokenDelta{Owner: s.Wallet.String(), Mint: signingtest.PubkeyFromSeed("mint-usdt").String(), Delta: money.QuantityFromInt64(-1)})
			},
			want: [][2]string{{inspect.CheckSimulation, inspect.ReasonSimulationDelta}},
		},
		{
			name: "simulation lamport debit above fees",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.Simulation.NativeDeltas[0].Lamports = -50_000_000
			},
			want: [][2]string{{inspect.CheckSimulation, inspect.ReasonSimulationDelta}},
		},
		{
			name: "simulation reveals a non-allowlisted inner program",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.Simulation.InnerProgramIDs = append(exp.Simulation.InnerProgramIDs, signingtest.PubkeyFromSeed("hidden-cpi").String())
			},
			want: [][2]string{{inspect.CheckNoArbitraryCPI, inspect.ReasonProgramNotAllowed}, {inspect.CheckProgramAllowlist, inspect.ReasonProgramNotAllowed}},
		},
		{
			name: "simulation failed",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.Simulation.Succeeded, exp.Simulation.Error = false, "InstructionError [3, Custom(6001)]"
			},
			want: [][2]string{{inspect.CheckSimulation, inspect.ReasonSimulationFailed}},
		},
		{
			name: "simulation missing",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.Simulation = nil
			},
			want: [][2]string{{inspect.CheckSimulation, inspect.ReasonSimulationMissing}},
		},
		{
			name: "simulation of different bytes",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				h := sha256.Sum256([]byte("other bytes"))
				exp.Simulation.TxHash = h[:]
			},
			want: [][2]string{{inspect.CheckSimulation, inspect.ReasonSimulationMismatch}},
		},
		{
			name:    "lookup table not resolvable",
			version: inspect.VersionV0,
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.LookupTables = nil
			},
			want: [][2]string{{inspect.CheckFeePayer, inspect.ReasonLookupTableUnresolved}, {inspect.CheckSimulation, inspect.ReasonLookupTableUnresolved}},
		},
		{
			name:    "lookup table too short",
			version: inspect.VersionV0,
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.LookupTables[s.LookupTable.String()] = exp.LookupTables[s.LookupTable.String()][:2]
			},
			want: [][2]string{{inspect.CheckMinOutput, inspect.ReasonLookupTableUnresolved}},
		},
		{
			name: "exact-out route",
			build: func(s *signingtest.Swap) []solana.Instruction {
				args := s.DefaultRouteArgs()
				args.Name = "exact_out_route"
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIxWith(s.DefaultRouteAccounts(), args))
			},
			want: [][2]string{{inspect.CheckNoArbitraryCPI, inspect.ReasonUnknownInstruction}, {inspect.CheckMinOutput, inspect.ReasonRouteCount}},
		},
		{
			name: "route with trailing bytes",
			build: func(s *signingtest.Swap) []solana.Instruction {
				args := s.DefaultRouteArgs()
				args.Trailing = []byte{0}
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIxWith(s.DefaultRouteAccounts(), args))
			},
			want: [][2]string{{inspect.CheckNoArbitraryCPI, inspect.ReasonUnknownInstruction}},
		},
		{
			name: "route with unknown swap variant",
			build: func(s *signingtest.Swap) []solana.Instruction {
				args := s.DefaultRouteArgs()
				args.Steps = []signingtest.RouteStep{{SwapTag: 200, Percent: 100, OutputIndex: 1}}
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIxWith(s.DefaultRouteAccounts(), args))
			},
			want: [][2]string{{inspect.CheckNoArbitraryCPI, inspect.ReasonUnknownInstruction}},
		},
		{
			name: "route destination is a foreign account",
			build: func(s *signingtest.Swap) []solana.Instruction {
				acc := s.DefaultRouteAccounts()
				acc.UserDestination = s.Foreign
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIxWith(acc, s.DefaultRouteArgs()))
			},
			want: [][2]string{{inspect.CheckOutputToken, inspect.ReasonOutputAccountMismatch}},
		},
		{
			name: "route destination mint is not the output",
			build: func(s *signingtest.Swap) []solana.Instruction {
				acc := s.DefaultRouteAccounts()
				acc.DestinationMint = signingtest.PubkeyFromSeed("mint-usdt")
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIxWith(acc, s.DefaultRouteArgs()))
			},
			want: [][2]string{{inspect.CheckOutputToken, inspect.ReasonOutputMintMismatch}},
		},
		{
			name: "route source is not the wallet's input ATA",
			build: func(s *signingtest.Swap) []solana.Instruction {
				acc := s.DefaultRouteAccounts()
				acc.UserSource = s.Foreign
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIxWith(acc, s.DefaultRouteArgs()))
			},
			want: [][2]string{{inspect.CheckTokenProgram, inspect.ReasonTokenAccountUnknown}},
		},
		{
			name: "route platform fee account",
			build: func(s *signingtest.Swap) []solana.Instruction {
				acc := s.DefaultRouteAccounts()
				acc.PlatformFeeAccount = s.Foreign
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIxWith(acc, s.DefaultRouteArgs()))
			},
			want: [][2]string{{inspect.CheckNoUnexpectedDestination, inspect.ReasonUnexpectedDestination}},
		},
		{
			name: "route optional destination token account elsewhere",
			build: func(s *signingtest.Swap) []solana.Instruction {
				acc := s.DefaultRouteAccounts()
				acc.DestinationTokenAccount = s.Foreign
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIxWith(acc, s.DefaultRouteArgs()))
			},
			want: [][2]string{{inspect.CheckOutputToken, inspect.ReasonOutputAccountMismatch}, {inspect.CheckNoUnexpectedDestination, inspect.ReasonUnexpectedDestination}},
		},
		{
			name: "route passes the wallet as a writable remaining account",
			build: func(s *signingtest.Swap) []solana.Instruction {
				acc := s.DefaultRouteAccounts()
				acc.Remaining = append(acc.Remaining, s.Wallet)
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIxWith(acc, s.DefaultRouteArgs()))
			},
			want: [][2]string{{inspect.CheckNoUnexpectedDestination, inspect.ReasonUnexpectedDestination}},
		},
		{
			name: "two route instructions",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), s.RouteIx())
			},
			want: [][2]string{{inspect.CheckNoArbitraryCPI, inspect.ReasonRouteCount}, {inspect.CheckInputDebitBound, inspect.ReasonRouteCount}},
		},
		{
			name: "no route instruction",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.ComputeBudgetIxs(), s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram))
			},
			want: [][2]string{{inspect.CheckMinOutput, inspect.ReasonRouteCount}, {inspect.CheckOutputToken, inspect.ReasonRouteCount}, {inspect.CheckSlippage, inspect.ReasonRouteCount}},
		},
		{
			name: "System.Assign of the wallet",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.SystemProgram,
					solana.AccountMetaSlice{sysAcct(s.Wallet, true, true)}, signingtest.SystemAssignData(s.Foreign)))
			},
			want: [][2]string{{inspect.CheckNoAuthorityChange, inspect.ReasonAuthorityChange}},
		},
		{
			name: "token transfer with multisig signers",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram, solana.AccountMetaSlice{
					sysAcct(s.InputATA, true, false), sysAcct(s.OutputATA, true, false), sysAcct(s.Wallet, false, true), sysAcct(s.Foreign, false, false),
				}, signingtest.TokenTransferData(1)))
			},
			want: [][2]string{{inspect.CheckNoArbitraryCPI, inspect.ReasonUnknownInstruction}},
		},
		{
			name: "malformed token instruction data",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram, solana.AccountMetaSlice{
					sysAcct(s.InputATA, true, false), sysAcct(s.OutputATA, true, false), sysAcct(s.Wallet, false, true),
				}, []byte{3, 1, 2}))
			},
			want: [][2]string{{inspect.CheckNoArbitraryCPI, inspect.ReasonUnknownInstruction}},
		},
		{
			name: "ATA created for a foreign owner",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), s.ATACreateIx(s.Foreign, s.OutputMint, s.OutputProgram))
			},
			want: [][2]string{{inspect.CheckNoUnexpectedDestination, inspect.ReasonUnexpectedDestination}},
		},
		{
			name: "ATA created for an unexpected mint",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), s.ATACreateIx(s.Wallet, signingtest.PubkeyFromSeed("mint-usdt"), signingtest.TokenProgram))
			},
			want: [][2]string{{inspect.CheckTokenProgram, inspect.ReasonUnexpectedMint}},
		},
		{
			name: "SyncNative on a non-native leg",
			build: func(s *signingtest.Swap) []solana.Instruction {
				return append(s.GoldenInstructions(), solana.NewInstruction(signingtest.TokenProgram,
					solana.AccountMetaSlice{sysAcct(s.WSOLATA, true, false)}, signingtest.TokenSingleByteData(17)))
			},
			want: [][2]string{{inspect.CheckTokenProgram, inspect.ReasonTokenAccountUnknown}},
		},
		{
			name:   "native input: wrap above the reservation",
			native: true,
			build: func(s *signingtest.Swap) []solana.Instruction {
				ixs := s.ComputeBudgetIxs()
				ixs = append(ixs, s.WrapIxs(s.MaxInputDebit+1)...)
				return append(ixs, s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIx(), s.CloseIx(s.WSOLATA, s.Wallet))
			},
			want: [][2]string{{inspect.CheckNoSystemTransfer, inspect.ReasonWrapExceeded}, {inspect.CheckInputDebitBound, inspect.ReasonWrapExceeded}},
		},
		{
			name:   "native input: close wSOL to a foreign destination",
			native: true,
			build: func(s *signingtest.Swap) []solana.Instruction {
				ixs := s.ComputeBudgetIxs()
				ixs = append(ixs, s.WrapIxs(s.InAmount)...)
				return append(ixs, s.ATACreateIx(s.Wallet, s.OutputMint, s.OutputProgram), s.RouteIx(), s.CloseIx(s.WSOLATA, s.Foreign))
			},
			want: [][2]string{{inspect.CheckNoAuthorityChange, inspect.ReasonUnexpectedClose}},
		},
		{
			name:   "native input: missing lamport prediction",
			native: true,
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.Simulation.NativeDeltas = nil
			},
			want: [][2]string{{inspect.CheckSimulation, inspect.ReasonSimulationDelta}},
		},
		{
			name: "invalid expectations reject everything",
			tweak: func(s *signingtest.Swap, _ []byte, exp *inspect.Expectations) {
				exp.MinOutput = money.QuantityFromInt64(0)
			},
			want: [][2]string{{inspect.CheckFeePayer, inspect.ReasonExpectationsInvalid}, {inspect.CheckSimulation, inspect.ReasonExpectationsInvalid}},
		},
	}

	for _, m := range muts {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			var s *signingtest.Swap
			if m.native {
				s = signingtest.NewNativeInputSwap()
			} else {
				s = signingtest.NewSwap()
			}
			version := m.version
			if version == "" {
				version = inspect.VersionLegacy
			}
			var ixs []solana.Instruction
			if m.build != nil {
				ixs = m.build(s)
			}
			if ixs == nil {
				ixs = s.GoldenInstructions()
			}
			var raw []byte
			if m.payer != nil {
				tx, err := solana.NewTransaction(ixs, s.Blockhash, solana.TransactionPayer(m.payer.PublicKey()))
				require.NoError(t, err)
				raw, err = tx.MarshalBinary()
				require.NoError(t, err)
			} else {
				raw = s.MustBuild(version, ixs)
			}
			tx, err := inspect.Decode(raw)
			require.NoError(t, err)
			exp := s.Expectations(raw)
			if m.tweak != nil {
				m.tweak(s, raw, &exp)
			}
			res := inspect.Inspect(tx, exp)
			require.False(t, res.Approved, "must reject: %v", res.ReasonCodes)
			require.NotEmpty(t, res.ReasonCodes)
			assert.True(t, sort.StringsAreSorted(res.ReasonCodes), "reason codes sorted")
			for _, want := range m.want {
				chk := checkByName(res, want[0])
				assert.False(t, chk.Passed, "check %s should fail: %+v", want[0], res.Checks)
				assert.Contains(t, chk.Detail, want[1], "check %s detail", want[0])
				assert.Contains(t, res.ReasonCodes, want[1])
			}
		})
	}
}

func TestInspect_Deterministic(t *testing.T) {
	t.Parallel()
	s := signingtest.NewSwap()
	ixs := append(s.GoldenInstructions(), s.RouteIx(), s.CloseIx(s.OutputATA, s.Foreign))
	raw := s.MustBuild(inspect.VersionV0, ixs)
	tx, err := inspect.Decode(raw)
	require.NoError(t, err)
	exp := s.Expectations(raw)
	first := inspect.Inspect(tx, exp)
	for i := 0; i < 5; i++ {
		assert.Equal(t, first, inspect.Inspect(tx, exp))
	}
	names := make([]string, 0, len(first.Checks))
	for _, c := range first.Checks {
		names = append(names, c.Name)
	}
	assert.Equal(t, inspect.CheckNames, names)
	assert.True(t, sort.StringsAreSorted(first.ReasonCodes))
}

func TestCheckNames_MatchExecutionSpec(t *testing.T) {
	t.Parallel()
	want := []string{
		"BLOCKHASH", "COMPUTE_BUDGET", "FEE_PAYER", "INPUT_DEBIT_BOUND", "MIN_OUTPUT", "NO_ARBITRARY_CPI",
		"NO_AUTHORITY_CHANGE", "NO_SYSTEM_TRANSFER", "NO_UNEXPECTED_DESTINATION", "OUTPUT_TOKEN",
		"PLAN_IDENTITY", "PROGRAM_ALLOWLIST", "SIGNERS", "SIMULATION", "SLIPPAGE", "TOKEN_PROGRAM",
	}
	assert.Equal(t, want, inspect.CheckNames)
	res := inspect.RejectAll(inspect.ReasonDecodeError, "boom")
	assert.False(t, res.Approved)
	assert.Equal(t, []string{inspect.ReasonDecodeError}, res.ReasonCodes)
	require.Len(t, res.Checks, 16)
	for _, c := range res.Checks {
		assert.False(t, c.Passed)
		assert.Contains(t, c.Detail, "not evaluated")
	}
}

func TestExpectations_Validate(t *testing.T) {
	t.Parallel()
	s := signingtest.NewSwap()
	raw := s.Golden(inspect.VersionLegacy)
	good := s.Expectations(raw)
	require.NoError(t, good.Validate())

	cases := map[string]func(e *inspect.Expectations){
		"empty wallet":                    func(e *inspect.Expectations) { e.Wallet = "" },
		"bad wallet":                      func(e *inspect.Expectations) { e.Wallet = "not-base58!" },
		"same mints":                      func(e *inspect.Expectations) { e.OutputMint = e.InputMint },
		"non-token program":               func(e *inspect.Expectations) { e.InputTokenProgram = inspect.SystemProgram },
		"zero max input":                  func(e *inspect.Expectations) { e.MaxInputDebit = money.QuantityFromInt64(0) },
		"empty allowlist":                 func(e *inspect.Expectations) { e.AllowedPrograms = nil },
		"token-2022 allowlisted but off":  func(e *inspect.Expectations) { e.AllowedPrograms = append(e.AllowedPrograms, inspect.Token2022Program) },
		"token-2022 on but not allowed":   func(e *inspect.Expectations) { e.Token2022Allowed = true },
		"route program not allowlisted":   func(e *inspect.Expectations) { e.RoutePrograms = []string{signingtest.PubkeyFromSeed("x").String()} },
		"route program is core":           func(e *inspect.Expectations) { e.RoutePrograms = []string{inspect.TokenProgram} },
		"zero compute units":              func(e *inspect.Expectations) { e.MaxComputeUnits = 0 },
		"compute units above runtime cap": func(e *inspect.Expectations) { e.MaxComputeUnits = 2_000_000 },
		"blockhash empty":                 func(e *inspect.Expectations) { e.RecentBlockhash = "" },
		"last valid zero":                 func(e *inspect.Expectations) { e.LastValidBlockHeight = 0 },
		"current height zero":             func(e *inspect.Expectations) { e.CurrentBlockHeight = 0 },
		"plan hash short":                 func(e *inspect.Expectations) { e.PlanHash = []byte{1} },
		"quote id empty":                  func(e *inspect.Expectations) { e.QuoteID = "" },
		"slippage above 100%":             func(e *inspect.Expectations) { e.MaxSlippageBPS = 10_001 },
		"native input on wrong mint":      func(e *inspect.Expectations) { e.InputIsNative = true },
		"lookup entry invalid":            func(e *inspect.Expectations) { e.LookupTables = map[string][]string{s.LookupTable.String(): {"nope"}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := s.Expectations(raw)
			mutate(&e)
			err := e.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, inspect.ErrExpectations)
		})
	}
}
