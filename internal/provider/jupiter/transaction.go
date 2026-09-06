package jupiter

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/mr-tron/base58"

	"github.com/nodal/controlplane/internal/errs"
)

// txSummary is what this package reads from transaction bytes: enough to
// cross-check the provider's claims (fee payer, blockhash, compute budget)
// and to know the signature of a signed transaction. It is deliberately
// not an inspection; internal/signing/inspect is the authority.
type txSummary struct {
	versioned       bool
	feePayer        string
	recentBlockhash string
	requiredSigners []string
	programIDs      []string

	computeUnitLimit uint32
	hasUnitLimit     bool
	computeUnitPrice uint64
	hasUnitPrice     bool

	numSignatureSlots int
	numSigned         int // non-zero signatures present
	firstSignature    string
}

// ComputeBudget instruction discriminators (first data byte), from the
// program's documented layout as mirrored by solana-go/programs/compute-budget.
const (
	computeBudgetSetUnitLimit = 2
	computeBudgetSetUnitPrice = 3
)

// summarizeTransaction decodes raw transaction bytes (legacy or v0). It
// never panics; malformed input is a VALIDATION_FAILED error.
func summarizeTransaction(raw []byte) (s txSummary, err error) {
	defer func() {
		// solana-go's decoder is defensive, but decoding attacker-controlled
		// bytes must never take the process down.
		if r := recover(); r != nil {
			s, err = txSummary{}, errs.Newf(errs.CodeValidationFailed, "jupiter: transaction decode panicked: %v", r)
		}
	}()
	if len(raw) == 0 {
		return txSummary{}, errs.New(errs.CodeValidationFailed, "jupiter: transaction is empty")
	}
	if len(raw) > MaxTransactionBytes {
		return txSummary{}, errs.New(errs.CodeValidationFailed, "jupiter: transaction exceeds the 1232-byte limit")
	}
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil {
		return txSummary{}, errs.Wrap(err, errs.CodeValidationFailed, "jupiter: transaction does not decode")
	}
	msg := &tx.Message
	if len(msg.AccountKeys) == 0 {
		return txSummary{}, errs.New(errs.CodeValidationFailed, "jupiter: transaction has no account keys")
	}
	s.versioned = msg.IsVersioned()
	s.feePayer = msg.AccountKeys[0].String()
	s.recentBlockhash = msg.RecentBlockhash.String()
	n := int(msg.Header.NumRequiredSignatures)
	if n > len(msg.AccountKeys) {
		return txSummary{}, errs.New(errs.CodeValidationFailed, "jupiter: transaction header requires more signers than keys")
	}
	for i := 0; i < n; i++ {
		s.requiredSigners = append(s.requiredSigners, msg.AccountKeys[i].String())
	}
	s.numSignatureSlots = len(tx.Signatures)
	for i, sig := range tx.Signatures {
		if sig == (solana.Signature{}) {
			continue
		}
		s.numSigned++
		if i == 0 {
			s.firstSignature = sig.String()
		}
	}
	seen := map[string]struct{}{}
	for _, ix := range msg.Instructions {
		if int(ix.ProgramIDIndex) >= len(msg.AccountKeys) {
			return txSummary{}, errs.New(errs.CodeValidationFailed, "jupiter: instruction program index out of range")
		}
		pid := msg.AccountKeys[ix.ProgramIDIndex]
		if _, dup := seen[pid.String()]; !dup {
			seen[pid.String()] = struct{}{}
			s.programIDs = append(s.programIDs, pid.String())
		}
		if pid.Equals(solana.ComputeBudget) {
			data := []byte(ix.Data)
			switch {
			case len(data) == 5 && data[0] == computeBudgetSetUnitLimit:
				s.computeUnitLimit, s.hasUnitLimit = binary.LittleEndian.Uint32(data[1:5]), true
			case len(data) == 9 && data[0] == computeBudgetSetUnitPrice:
				s.computeUnitPrice, s.hasUnitPrice = binary.LittleEndian.Uint64(data[1:9]), true
			}
		}
	}
	return s, nil
}

// signatureOfSigned returns the first (fee payer) signature of a signed
// transaction, which is the transaction id the chain de-duplicates by.
func signatureOfSigned(raw []byte) (string, error) {
	s, err := summarizeTransaction(raw)
	if err != nil {
		return "", err
	}
	if s.numSignatureSlots == 0 || s.firstSignature == "" {
		return "", errs.New(errs.CodeValidationFailed, "jupiter: transaction carries no fee-payer signature")
	}
	return s.firstSignature, nil
}

// base58Encode renders bytes in base58 (used for blockhashes from /build).
func base58Encode(b []byte) string { return base58.Encode(b) }

// FakeRouteParams describe the placeholder swap transaction built by
// BuildFakeRouteTransaction.
type FakeRouteParams struct {
	// ProgramID is the Jupiter program id the placeholder route instruction
	// is addressed to (must match the inspector's allowlist).
	ProgramID solana.PublicKey
	Taker     solana.PublicKey
	// InputMint / OutputMint are used to derive the taker's associated
	// token accounts.
	InputMint  solana.PublicKey
	OutputMint solana.PublicKey
	// TokenProgram defaults to the SPL Token program.
	TokenProgram solana.PublicKey
	// Amounts encoded in the route instruction data.
	InAmount        uint64
	QuotedOutAmount uint64
	SlippageBPS     uint16
	PlatformFeeBPS  uint8

	RecentBlockhash               solana.Hash
	ComputeUnitLimit              uint32
	ComputeUnitPriceMicroLamports uint64
}

// fakeRouteDiscriminator is the standard Anchor discriminator derivation
// (sha256("global:route")[:8]) applied to the name "route".
//
// UNVERIFIED (blocker SB-007): that Jupiter v6's real route instruction uses
// this discriminator has NOT been checked against the published IDL or a
// mainnet transaction. It is used only to give the fake instruction a
// plausible shape; nothing may treat it as evidence of the real layout.
func fakeRouteDiscriminator() []byte {
	sum := sha256.Sum256([]byte("global:route"))
	return sum[:8]
}

// BuildFakeRouteTransaction builds a real, serialized, unsigned Solana v0
// transaction with the shape of a Jupiter swap so that the signing
// inspector and the executor can consume Fake output:
//
//  1. ComputeBudget SetComputeUnitLimit(ComputeUnitLimit)
//  2. ComputeBudget SetComputeUnitPrice(ComputeUnitPriceMicroLamports)
//  3. AssociatedTokenAccount CreateIdempotent(payer=taker, wallet=taker,
//     mint=OutputMint) — the documented "rent for the wallet's own ATA"
//     allowance
//  4. A placeholder "route" instruction under ProgramID with accounts
//     [token program, taker (signer), taker input ATA (w), taker output ATA
//     (w), input mint, output mint] and data
//     discriminator(8) | route_plan len u32 = 0 | in_amount u64 LE |
//     quoted_out_amount u64 LE | slippage_bps u16 LE | platform_fee_bps u8.
//
// It is a fixture generator for LOCAL/TEST/DEV and fixtures only; it never
// talks to a network and is never wired in production (the Fake that uses
// it refuses STAGING/PROD).
//
// UNVERIFIED (blocker SB-007): the account order and data layout of
// instruction 4 were written from memory and have NOT been checked against
// the published Jupiter v6 IDL or a real mainnet swap. They are a
// placeholder chosen so the transaction is decodable and carries a quoted
// output and slippage; they are NOT a statement about the real instruction.
// In particular this function must never be cited as evidence that
// internal/signing/inspect's Jupiter layout is correct — both came from the
// same unverified source, so agreement between them proves nothing. The
// inspector's layout has to be validated against the IDL and recorded
// mainnet transactions before any canary trade.
func BuildFakeRouteTransaction(p FakeRouteParams) ([]byte, error) {
	if p.ProgramID.IsZero() || p.Taker.IsZero() || p.InputMint.IsZero() || p.OutputMint.IsZero() {
		return nil, errors.New("jupiter: fake route: program id, taker and mints are required")
	}
	if p.RecentBlockhash.IsZero() {
		return nil, errors.New("jupiter: fake route: recent blockhash is required")
	}
	tokenProgram := p.TokenProgram
	if tokenProgram.IsZero() {
		tokenProgram = solana.TokenProgramID
	}
	inATA, _, err := solana.FindAssociatedTokenAddress(p.Taker, p.InputMint)
	if err != nil {
		return nil, fmt.Errorf("jupiter: fake route: input ATA: %w", err)
	}
	outATA, _, err := solana.FindAssociatedTokenAddress(p.Taker, p.OutputMint)
	if err != nil {
		return nil, fmt.Errorf("jupiter: fake route: output ATA: %w", err)
	}

	data := make([]byte, 0, 8+4+8+8+2+1)
	data = append(data, fakeRouteDiscriminator()...)
	data = binary.LittleEndian.AppendUint32(data, 0) // empty route_plan vec
	data = binary.LittleEndian.AppendUint64(data, p.InAmount)
	data = binary.LittleEndian.AppendUint64(data, p.QuotedOutAmount)
	data = binary.LittleEndian.AppendUint16(data, p.SlippageBPS)
	data = append(data, p.PlatformFeeBPS)

	route := solana.NewInstruction(p.ProgramID, solana.AccountMetaSlice{
		solana.Meta(tokenProgram),
		solana.Meta(p.Taker).SIGNER(),
		solana.Meta(inATA).WRITE(),
		solana.Meta(outATA).WRITE(),
		solana.Meta(p.InputMint),
		solana.Meta(p.OutputMint),
	}, data)

	instructions := []solana.Instruction{
		computebudget.NewSetComputeUnitLimitInstruction(p.ComputeUnitLimit).Build(),
		computebudget.NewSetComputeUnitPriceInstruction(p.ComputeUnitPriceMicroLamports).Build(),
		associatedtokenaccount.NewCreateIdempotentInstructionWithTokenProgram(p.Taker, p.Taker, p.OutputMint, tokenProgram).Build(),
		route,
	}
	tx, err := solana.NewTransaction(instructions, p.RecentBlockhash,
		solana.TransactionPayer(p.Taker),
		solana.TransactionMessageVersion(solana.MessageVersionV0),
	)
	if err != nil {
		return nil, fmt.Errorf("jupiter: fake route: build: %w", err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("jupiter: fake route: marshal: %w", err)
	}
	if len(raw) > MaxTransactionBytes {
		return nil, errors.New("jupiter: fake route: transaction exceeds 1232 bytes")
	}
	return raw, nil
}
