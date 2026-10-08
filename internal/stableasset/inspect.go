package stableasset

import (
	"bytes"
	"encoding/hex"
	"sort"

	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

// Transaction inspection reason codes. Stable and machine-readable; a
// refusal carries every code that applies, sorted.
const (
	InspectMalformed         = "INSPECT_MALFORMED"
	InspectInvalidPlan       = "INSPECT_INVALID_PLAN"
	InspectChainMismatch     = "INSPECT_CHAIN_MISMATCH"
	InspectBlockhashMismatch = "INSPECT_BLOCKHASH_MISMATCH"
	InspectFeePayerMismatch  = "INSPECT_FEE_PAYER_MISMATCH"
	InspectInstructionCount  = "INSPECT_INSTRUCTION_COUNT"
	InspectProgramMismatch   = "INSPECT_PROGRAM_MISMATCH"
	InspectMintMismatch      = "INSPECT_MINT_MISMATCH"
	InspectOwnerMismatch     = "INSPECT_OWNER_MISMATCH"
	InspectAmountMismatch    = "INSPECT_AMOUNT_MISMATCH"
	InspectAuthorityMismatch = "INSPECT_AUTHORITY_MISMATCH"
	InspectMemoMismatch      = "INSPECT_MEMO_MISMATCH"
	InspectSignerSetMismatch = "INSPECT_SIGNER_SET_MISMATCH"
	InspectSignerNotHeld     = "INSPECT_SIGNER_NOT_HELD"
	InspectSignatureInvalid  = "INSPECT_SIGNATURE_INVALID"
	InspectLockMismatch      = "INSPECT_LOCK_MISMATCH"
)

// rejected is the error every inspection refusal returns.
func rejected(codes []string) error {
	sort.Strings(codes)
	out := codes[:0]
	for i, c := range codes {
		if i == 0 || c != codes[i-1] {
			out = append(out, c)
		}
	}
	return errs.New(errs.CodeSigningRejected, "stableasset: transaction inspection refused the transaction").
		WithField("reason_codes", out)
}

// canonicalMessage round-trips msg through the wire format, so what is
// inspected is exactly what the chain will decode and what will be signed.
func canonicalMessage(msg devnet.Message) (devnet.Message, error) {
	raw, err := devnet.Transaction{Message: msg}.Encode()
	if err != nil {
		return devnet.Message{}, rejected([]string{InspectMalformed})
	}
	tx, err := devnet.Decode(raw)
	if err != nil {
		return devnet.Message{}, rejected([]string{InspectMalformed})
	}
	return tx.Message, nil
}

func sameSigners(got, want []devnet.PublicKey) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// inspectMint checks a decoded message against a mint plan: the program set
// is exactly [TOKEN_MINT_TO, MEMO], every field matches the plan, the MEMO is
// the plan hash, and the only signer is the mint authority, who also pays the
// fee.
func inspectMint(plan MintPlan, m devnet.Message) []string {
	parts, err := plan.parts()
	if err != nil {
		return []string{InspectInvalidPlan}
	}
	h, _, err := plan.Hash()
	if err != nil {
		return []string{InspectInvalidPlan}
	}
	var codes []string
	if m.ChainID != parts.chain {
		codes = append(codes, InspectChainMismatch)
	}
	if m.RecentBlockhash != parts.blockhash {
		codes = append(codes, InspectBlockhashMismatch)
	}
	if m.FeePayer != parts.feePayer || parts.feePayer != parts.authority {
		codes = append(codes, InspectFeePayerMismatch)
	}
	if len(m.Instructions) != 2 {
		return append(codes, InspectInstructionCount)
	}
	if mt, ok := m.Instructions[0].(devnet.TokenMintTo); !ok {
		codes = append(codes, InspectProgramMismatch)
	} else {
		if mt.Mint != parts.mint {
			codes = append(codes, InspectMintMismatch)
		}
		if mt.ToOwner != parts.recipient {
			codes = append(codes, InspectOwnerMismatch)
		}
		if mt.Amount != parts.amount {
			codes = append(codes, InspectAmountMismatch)
		}
		if mt.Authority != parts.authority {
			codes = append(codes, InspectAuthorityMismatch)
		}
	}
	codes = append(codes, inspectMemo(m.Instructions[1], h)...)
	if !sameSigners(m.RequiredSigners(), []devnet.PublicKey{parts.authority}) {
		codes = append(codes, InspectSignerSetMismatch)
	}
	return codes
}

// inspectBurn checks a decoded message against a burn plan: exactly
// [TOKEN_BURN, MEMO], the holder burns its own units and pays the fee, and
// signs alone.
func inspectBurn(plan BurnPlan, m devnet.Message) []string {
	parts, err := plan.parts()
	if err != nil {
		return []string{InspectInvalidPlan}
	}
	h, _, err := plan.Hash()
	if err != nil {
		return []string{InspectInvalidPlan}
	}
	var codes []string
	if m.ChainID != parts.chain {
		codes = append(codes, InspectChainMismatch)
	}
	if m.RecentBlockhash != parts.blockhash {
		codes = append(codes, InspectBlockhashMismatch)
	}
	if m.FeePayer != parts.feePayer || parts.feePayer != parts.holder {
		codes = append(codes, InspectFeePayerMismatch)
	}
	if len(m.Instructions) != 2 {
		return append(codes, InspectInstructionCount)
	}
	if b, ok := m.Instructions[0].(devnet.TokenBurn); !ok {
		codes = append(codes, InspectProgramMismatch)
	} else {
		if b.Mint != parts.mint {
			codes = append(codes, InspectMintMismatch)
		}
		if b.Owner != parts.holder {
			codes = append(codes, InspectOwnerMismatch)
		}
		if b.Amount != parts.amount {
			codes = append(codes, InspectAmountMismatch)
		}
	}
	codes = append(codes, inspectMemo(m.Instructions[1], h)...)
	if !sameSigners(m.RequiredSigners(), []devnet.PublicKey{parts.holder}) {
		codes = append(codes, InspectSignerSetMismatch)
	}
	return codes
}

// inspectLock checks a decoded message against the BRIDGE_LOCK a holder
// agreed to: exactly [BRIDGE_LOCK, MEMO], the lock is the agreed one and is
// the holder's own, and the holder alone signs and pays.
func inspectLock(holder devnet.PublicKey, planHash [32]byte, lock devnet.BridgeLock, m devnet.Message) []string {
	var codes []string
	if m.FeePayer != holder {
		codes = append(codes, InspectFeePayerMismatch)
	}
	if lock.Owner != holder {
		codes = append(codes, InspectOwnerMismatch)
	}
	if len(m.Instructions) != 2 {
		return append(codes, InspectInstructionCount)
	}
	if got, ok := m.Instructions[0].(devnet.BridgeLock); !ok {
		codes = append(codes, InspectProgramMismatch)
	} else if got != lock {
		codes = append(codes, InspectLockMismatch)
	}
	codes = append(codes, inspectMemo(m.Instructions[1], planHash)...)
	if !sameSigners(m.RequiredSigners(), []devnet.PublicKey{holder}) {
		codes = append(codes, InspectSignerSetMismatch)
	}
	return codes
}

func inspectMemo(ins devnet.Instruction, planHash [32]byte) []string {
	memo, ok := ins.(devnet.Memo)
	if !ok {
		return []string{InspectProgramMismatch}
	}
	if !bytes.Equal(memo.Data, []byte(hex.EncodeToString(planHash[:]))) {
		return []string{InspectMemoMismatch}
	}
	return nil
}

// inspectSigned decodes a signed transaction, applies the message inspection
// and verifies every signature over the exact message bytes.
func inspectSigned(raw []byte, inspect func(devnet.Message) []string) (devnet.TxID, error) {
	tx, err := devnet.Decode(raw)
	if err != nil {
		return "", rejected([]string{InspectMalformed})
	}
	if codes := inspect(tx.Message); len(codes) > 0 {
		return "", rejected(codes)
	}
	if err := tx.VerifySignatures(); err != nil {
		return "", rejected([]string{InspectSignatureInvalid})
	}
	id, err := tx.ID()
	if err != nil {
		return "", rejected([]string{InspectSignatureInvalid})
	}
	return id, nil
}

// InspectMintTransaction verifies a signed issuance transaction against its
// plan: program TOKEN_MINT_TO, mint, recipient, amount, MEMO = plan hash,
// signer set = mint authority only, fee payer = mint authority, valid
// signatures. It returns the TxID, or SIGNING_REJECTED with reason codes.
func InspectMintTransaction(plan MintPlan, raw []byte) (devnet.TxID, error) {
	return inspectSigned(raw, func(m devnet.Message) []string { return inspectMint(plan, m) })
}

// InspectBurnTransaction verifies a signed burn transaction against its plan.
func InspectBurnTransaction(plan BurnPlan, raw []byte) (devnet.TxID, error) {
	return inspectSigned(raw, func(m devnet.Message) []string { return inspectBurn(plan, m) })
}
