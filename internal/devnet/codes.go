package devnet

import (
	"github.com/nodal/controlplane/internal/errs"
)

// ErrorCode is the stable, machine-readable reason a call was refused or a
// transaction FAILED. Codes never change meaning and are never removed.
//
// A refused call returns an *errs.Error whose Fields["reason"] carries the
// ErrorCode (ReasonOf extracts it); a FAILED transaction carries it in
// TxStatus.Err.
type ErrorCode string

// Submission, encoding and argument rejections. Nothing enters the mempool
// and nothing on the chain changes.
const (
	// CodeMalformedTransaction: the bytes are not a canonical version-1
	// transaction (bad magic or version, unknown program, wrong length,
	// trailing bytes, oversize input).
	CodeMalformedTransaction ErrorCode = "MALFORMED_TRANSACTION"
	// CodeInvalidMessage: the message is well-formed but breaks a rule
	// (zero key, zero amount, more than one BRIDGE_LOCK, a bridge to itself).
	CodeInvalidMessage ErrorCode = "INVALID_MESSAGE"
	// CodeInvalidChainID: a chain id is not 1..32 bytes of [a-z0-9-].
	CodeInvalidChainID ErrorCode = "INVALID_CHAIN_ID"
	// CodeInvalidBase58: text is not the canonical base58 form of a value of
	// the expected size.
	CodeInvalidBase58 ErrorCode = "INVALID_BASE58"
	// CodeInvalidArgument: an operator or helper call received an unusable
	// argument (zero key, zero amount, unknown reorg mode).
	CodeInvalidArgument ErrorCode = "INVALID_ARGUMENT"
	// CodeInvalidConfig: a ChainConfig failed validation.
	CodeInvalidConfig ErrorCode = "INVALID_CONFIG"
	// CodeWrongChain: the message names a different chain.
	CodeWrongChain ErrorCode = "WRONG_CHAIN"
	// CodeBlockhashNotFound: the recent blockhash is not among the
	// BlockhashWindow most recent canonical blocks (expired, orphaned by a
	// reorg, or never produced).
	CodeBlockhashNotFound ErrorCode = "BLOCKHASH_NOT_FOUND"
	// CodeSignatureMissing: fewer signatures than required signers.
	CodeSignatureMissing ErrorCode = "SIGNATURE_MISSING"
	// CodeSignatureExtra: more signatures than required signers, or a
	// keypair offered to Sign that the message does not require.
	CodeSignatureExtra ErrorCode = "SIGNATURE_EXTRA"
	// CodeSignatureInvalid: a signature does not verify against its signer
	// over the exact message bytes.
	CodeSignatureInvalid ErrorCode = "SIGNATURE_INVALID"
	// CodeKeypairUninitialized: Sign was called on the zero Keypair.
	CodeKeypairUninitialized ErrorCode = "KEYPAIR_UNINITIALIZED"
	// CodeDuplicateMessage: a different signature over a message the chain
	// already holds (pending or included). Ed25519 signatures are not unique
	// for adversarial keys, so replay protection is keyed by message too.
	CodeDuplicateMessage ErrorCode = "DUPLICATE_MESSAGE"
	// CodeTxIDConflict: a known TxID arrived with a different message.
	CodeTxIDConflict ErrorCode = "TXID_CONFLICT"
	// CodeSubmissionTimeout: an injected transport timeout. The transaction
	// may or may not have been accepted; observe Status before acting again.
	CodeSubmissionTimeout ErrorCode = "SUBMISSION_TIMEOUT"
	// CodeTransactionNotFound: no pending or canonical transaction has this
	// TxID.
	CodeTransactionNotFound ErrorCode = "TRANSACTION_NOT_FOUND"
	// CodeBlockNotFound: no canonical block at this height.
	CodeBlockNotFound ErrorCode = "BLOCK_NOT_FOUND"
	// CodeNotABridgeLock: the transaction carries no BRIDGE_LOCK.
	CodeNotABridgeLock ErrorCode = "NOT_A_BRIDGE_LOCK"
	// CodeLockNotFinalized: BuildRelease was given a lock that is not
	// FINALIZED; relaying it could mint against a lock a reorg removes.
	CodeLockNotFinalized ErrorCode = "LOCK_NOT_FINALIZED"
	// CodeChainNotFound: the Network has no chain with this id.
	CodeChainNotFound ErrorCode = "CHAIN_NOT_FOUND"
	// CodeChainExists: the Network already has a chain with this id.
	CodeChainExists ErrorCode = "CHAIN_EXISTS"
	// CodeMintExists: CreateMint named an address that is already a mint.
	CodeMintExists ErrorCode = "MINT_EXISTS"
	// CodeInvariantViolated: an internal consistency check failed and the
	// operation was refused without changing anything. Never expected.
	CodeInvariantViolated ErrorCode = "INVARIANT_VIOLATED"
)

// Execution failures. The transaction is in a block with status FAILED; its
// instructions changed nothing and only the fee moved.
const (
	// CodeInsufficientFundsForFee: the fee payer cannot pay the fee. Unlike
	// every other failure, no fee is charged.
	CodeInsufficientFundsForFee ErrorCode = "INSUFFICIENT_FUNDS_FOR_FEE"
	// CodeInsufficientFunds: a SYSTEM_TRANSFER exceeds the native balance.
	CodeInsufficientFunds ErrorCode = "INSUFFICIENT_FUNDS"
	// CodeInsufficientTokenBalance: a token transfer, burn or lock exceeds
	// the owner's balance of that mint.
	CodeInsufficientTokenBalance ErrorCode = "INSUFFICIENT_TOKEN_BALANCE"
	// CodeMintNotFound: the mint does not exist on this chain.
	CodeMintNotFound ErrorCode = "MINT_NOT_FOUND"
	// CodeMintFrozen: the mint is frozen; nothing of it moves.
	CodeMintFrozen ErrorCode = "MINT_FROZEN"
	// CodeAuthoritySignatureMissing: the instruction is signed by an
	// authority that is not the mint's (TOKEN_MINT_TO) or the chain's bridge
	// authority (BRIDGE_RELEASE), so the signature that would authorise it is
	// missing.
	CodeAuthoritySignatureMissing ErrorCode = "AUTHORITY_SIGNATURE_MISSING"
	// CodeArithmeticOverflow: a balance or supply would leave the uint64
	// range.
	CodeArithmeticOverflow ErrorCode = "ARITHMETIC_OVERFLOW"
	// CodeBridgeNotConfigured: the chain has no BridgeConfig.
	CodeBridgeNotConfigured ErrorCode = "BRIDGE_NOT_CONFIGURED"
	// CodeBridgeNonceUsed: the BRIDGE_LOCK nonce was already used on this
	// chain.
	CodeBridgeNonceUsed ErrorCode = "BRIDGE_NONCE_USED"
	// CodeBridgeAlreadyReleased: a release for this (source chain, source tx
	// id) or (source chain, nonce) already applied.
	CodeBridgeAlreadyReleased ErrorCode = "BRIDGE_ALREADY_RELEASED"
)

// reasonField is the errs.Error field that carries the ErrorCode.
const reasonField = "reason"

// newError returns an *errs.Error with the given boundary code whose
// Fields["reason"] is the devnet ErrorCode. Details are prefixed "devnet: ".
func newError(code errs.Code, reason ErrorCode, detail string) *errs.Error {
	return errs.New(code, "devnet: "+detail).WithField(reasonField, string(reason))
}

// invalid is newError(VALIDATION_FAILED, reason, detail).
func invalid(reason ErrorCode, detail string) *errs.Error {
	return newError(errs.CodeValidationFailed, reason, detail)
}

// malformed is the error Decode returns for every structural problem.
func malformed(detail string) *errs.Error {
	return invalid(CodeMalformedTransaction, "malformed transaction: "+detail)
}

// ReasonOf returns the devnet ErrorCode carried by err, or "" when err is nil
// or did not come from this package.
func ReasonOf(err error) ErrorCode {
	e, ok := errs.As(err)
	if !ok {
		return ""
	}
	r, ok := e.Fields[reasonField].(string)
	if !ok {
		return ""
	}
	return ErrorCode(r)
}
