package inspect

import "github.com/gagliardetto/solana-go"

// Well-known Solana program and mint addresses (docs/api/providers/solana-rpc.md
// and jupiter.md, verified 2026-09-05).
const (
	// SystemProgram is the native System program.
	SystemProgram = "11111111111111111111111111111111"
	// ComputeBudgetProgram sets compute-unit limits and priority fees.
	ComputeBudgetProgram = "ComputeBudget111111111111111111111111111111"
	// TokenProgram is SPL Token.
	TokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA" // #nosec G101 -- public SPL program address published by Solana, not a credential
	// Token2022Program is SPL Token-2022 (token extensions).
	Token2022Program = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb" // #nosec G101 -- public SPL program address published by Solana, not a credential
	// AssociatedTokenProgram is the Associated Token Account program.
	AssociatedTokenProgram = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL" // #nosec G101 -- public SPL program address published by Solana, not a credential
	// AddressLookupTableProgram is the ALT program (never allowlisted for
	// swaps: a swap has no business mutating lookup tables).
	AddressLookupTableProgram = "AddressLookupTab1e1111111111111111111111111"
	// JupiterV6Program is the Jupiter aggregator v6 program id
	// (jup-ag/instruction-parser; see jupiter.md).
	JupiterV6Program = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
	// WrappedSOLMint is the native mint used for wrapped SOL.
	WrappedSOLMint = "So11111111111111111111111111111111111111112"
)

// Program keys as PublicKey values. These are immutable constants in
// PublicKey form, not mutable state.
var (
	systemProgramKey          = solana.MustPublicKeyFromBase58(SystemProgram)
	computeBudgetProgramKey   = solana.MustPublicKeyFromBase58(ComputeBudgetProgram)
	tokenProgramKey           = solana.MustPublicKeyFromBase58(TokenProgram)
	token2022ProgramKey       = solana.MustPublicKeyFromBase58(Token2022Program)
	associatedTokenProgramKey = solana.MustPublicKeyFromBase58(AssociatedTokenProgram)
	wrappedSOLMintKey         = solana.MustPublicKeyFromBase58(WrappedSOLMint)
)

// DefaultAllowedPrograms is the base allowlist for a V1 Jupiter swap: System,
// Compute Budget, SPL Token, Associated Token Account and Jupiter v6.
// Token-2022 is deliberately absent; the signing service adds it only when
// the asset is explicitly enabled and carries no extensions (PART 34).
func DefaultAllowedPrograms() []string {
	return []string{SystemProgram, ComputeBudgetProgram, TokenProgram, AssociatedTokenProgram, JupiterV6Program}
}

// Solana runtime constants used for bounds.
const (
	// MaxTransactionSize is the packet limit for a serialized transaction.
	MaxTransactionSize = 1232
	// MaxComputeUnitLimit is the runtime cap on compute units per transaction.
	MaxComputeUnitLimit = 1_400_000
	// DefaultComputeUnitsPerInstruction is the runtime default when no
	// SetComputeUnitLimit instruction is present.
	DefaultComputeUnitsPerInstruction = 200_000
	// LamportsPerSignature is the base fee per signature.
	LamportsPerSignature = 5_000
	// TokenAccountRentLamports is the rent-exempt minimum for a 165-byte SPL
	// token account (what creating an ATA costs the payer).
	TokenAccountRentLamports = 2_039_280
	// MicroLamportsPerLamport converts compute-unit prices to lamports.
	MicroLamportsPerLamport = 1_000_000
)
