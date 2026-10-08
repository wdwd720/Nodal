package delegation

import (
	"bytes"
	"sort"

	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

// InspectorVersion is recorded with every signing decision.
const InspectorVersion = "delegation-inspector/1"

// Inspection reason codes. Stable and machine-readable; a refusal carries
// every code that applies, sorted.
const (
	InspectMalformed            = "INSPECT_MALFORMED"
	InspectInvalidPlan          = "INSPECT_INVALID_PLAN"
	InspectPlanHashMismatch     = "INSPECT_PLAN_HASH_MISMATCH"
	InspectChainMismatch        = "INSPECT_CHAIN_MISMATCH"
	InspectBlockhashMismatch    = "INSPECT_BLOCKHASH_MISMATCH"
	InspectFeePayerMismatch     = "INSPECT_FEE_PAYER_MISMATCH"
	InspectFeeMismatch          = "INSPECT_FEE_MISMATCH"
	InspectInstructionCount     = "INSPECT_INSTRUCTION_COUNT"
	InspectProgramMismatch      = "INSPECT_PROGRAM_MISMATCH"
	InspectDestinationMismatch  = "INSPECT_DESTINATION_MISMATCH"
	InspectTokenAccountMismatch = "INSPECT_TOKEN_ACCOUNT_MISMATCH"
	InspectAmountMismatch       = "INSPECT_AMOUNT_MISMATCH"
	InspectSlippageExceeded     = "INSPECT_SLIPPAGE_EXCEEDED"
	InspectMemoMismatch         = "INSPECT_MEMO_MISMATCH"
	InspectSignerSetMismatch    = "INSPECT_SIGNER_SET_MISMATCH"
	InspectSignatureInvalid     = "INSPECT_SIGNATURE_INVALID"
	InspectNotThePlanned        = "INSPECT_NOT_THE_PLANNED_TRANSACTION"
)

// SwapExpectation is everything the inspector compares a swap transaction
// against. Every field comes from persisted rows: the plan and its stored
// hash, the blockhash recorded when the transaction was compiled, the signer
// key registered for the account and chain, the capability's allowlists, the
// chain's configured fee and the signing policy's fee bound. Nothing comes
// from the caller.
type SwapExpectation struct {
	Plan                SwapPlan
	PlanHash            [32]byte
	Blockhash           devnet.Hash
	Wallet              devnet.PublicKey
	AllowedChains       []devnet.ChainID
	AllowedDestinations []devnet.PublicKey
	ChainFee            uint64
	MaxFee              uint64
}

// WithdrawalExpectation is the same for a withdrawal; the destination must be
// on both the customer's allowlist (the persisted destination row) and the
// agent's capability.
type WithdrawalExpectation struct {
	Plan                WithdrawalPlan
	PlanHash            [32]byte
	Blockhash           devnet.Hash
	Wallet              devnet.PublicKey
	AllowedChains       []devnet.ChainID
	AllowedDestinations []devnet.PublicKey
	CustomerDestination devnet.PublicKey
	ChainFee            uint64
	MaxFee              uint64
}

// InspectSwapMessage checks a decoded message against the expectation and
// returns every violated reason code, sorted (none means it is exactly the
// planned transaction).
func InspectSwapMessage(e SwapExpectation, m devnet.Message) []string {
	var codes []string
	add := func(c string) { codes = append(codes, c) }
	h, herr := e.Plan.Hash()
	if herr != nil || h != e.PlanHash {
		add(InspectPlanHashMismatch)
	}
	p, err := e.Plan.parts()
	if err != nil {
		return sortedCodes(append(codes, InspectInvalidPlan))
	}
	if m.ChainID != p.chain || !contains(e.AllowedChains, p.chain) {
		add(InspectChainMismatch)
	}
	if m.RecentBlockhash != e.Blockhash {
		add(InspectBlockhashMismatch)
	}
	if m.FeePayer != e.Wallet || p.wallet != e.Wallet {
		add(InspectFeePayerMismatch)
	}
	if p.fee != e.ChainFee || p.fee > e.MaxFee {
		add(InspectFeeMismatch)
	}
	if !contains(e.AllowedDestinations, p.venue) {
		add(InspectDestinationMismatch)
	}
	if len(m.Instructions) != 3 {
		return sortedCodes(append(codes, InspectInstructionCount))
	}
	pay, ok0 := m.Instructions[0].(devnet.TokenTransfer)
	recv, ok1 := m.Instructions[1].(devnet.TokenTransfer)
	memo, ok2 := m.Instructions[2].(devnet.Memo)
	if !ok0 || !ok1 || !ok2 {
		return sortedCodes(append(codes, InspectProgramMismatch))
	}
	if pay.Mint != p.payMint || pay.FromOwner != e.Wallet {
		add(InspectTokenAccountMismatch)
	}
	if pay.ToOwner != p.venue {
		add(InspectDestinationMismatch)
	}
	if pay.Amount != p.amountIn {
		add(InspectAmountMismatch)
	}
	if recv.Mint != p.receiveMint || recv.ToOwner != e.Wallet {
		add(InspectTokenAccountMismatch)
	}
	if recv.FromOwner != p.venue {
		add(InspectDestinationMismatch)
	}
	if recv.Amount < p.minAmountOut {
		add(InspectSlippageExceeded)
	}
	if recv.Amount != p.amountOut {
		add(InspectAmountMismatch)
	}
	if herr == nil && !bytes.Equal(memo.Data, memoOf(e.PlanHash)) {
		add(InspectMemoMismatch)
	}
	if !sameKeys(m.RequiredSigners(), []devnet.PublicKey{e.Wallet, p.venue}) {
		add(InspectSignerSetMismatch)
	}
	if len(codes) == 0 {
		// Every field matched; the bytes must too, so nothing the field checks
		// do not look at can ride along.
		want, err := e.Plan.Message(e.Blockhash)
		if err != nil || !sameMessage(want, m) {
			add(InspectNotThePlanned)
		}
	}
	return sortedCodes(codes)
}

// InspectWithdrawalMessage checks a decoded withdrawal message.
func InspectWithdrawalMessage(e WithdrawalExpectation, m devnet.Message) []string {
	var codes []string
	add := func(c string) { codes = append(codes, c) }
	h, herr := e.Plan.Hash()
	if herr != nil || h != e.PlanHash {
		add(InspectPlanHashMismatch)
	}
	p, err := e.Plan.parts()
	if err != nil {
		return sortedCodes(append(codes, InspectInvalidPlan))
	}
	if m.ChainID != p.chain || !contains(e.AllowedChains, p.chain) {
		add(InspectChainMismatch)
	}
	if m.RecentBlockhash != e.Blockhash {
		add(InspectBlockhashMismatch)
	}
	if m.FeePayer != e.Wallet || p.wallet != e.Wallet {
		add(InspectFeePayerMismatch)
	}
	if p.fee != e.ChainFee || p.fee > e.MaxFee {
		add(InspectFeeMismatch)
	}
	if p.destination != e.CustomerDestination || !contains(e.AllowedDestinations, p.destination) {
		add(InspectDestinationMismatch)
	}
	if len(m.Instructions) != 2 {
		return sortedCodes(append(codes, InspectInstructionCount))
	}
	pay, ok0 := m.Instructions[0].(devnet.TokenTransfer)
	memo, ok1 := m.Instructions[1].(devnet.Memo)
	if !ok0 || !ok1 {
		return sortedCodes(append(codes, InspectProgramMismatch))
	}
	if pay.Mint != p.mint || pay.FromOwner != e.Wallet {
		add(InspectTokenAccountMismatch)
	}
	if pay.ToOwner != p.destination {
		add(InspectDestinationMismatch)
	}
	if pay.Amount != p.amount {
		add(InspectAmountMismatch)
	}
	if herr == nil && !bytes.Equal(memo.Data, memoOf(e.PlanHash)) {
		add(InspectMemoMismatch)
	}
	if !sameKeys(m.RequiredSigners(), []devnet.PublicKey{e.Wallet}) {
		add(InspectSignerSetMismatch)
	}
	if len(codes) == 0 {
		want, err := e.Plan.Message(e.Blockhash)
		if err != nil || !sameMessage(want, m) {
			add(InspectNotThePlanned)
		}
	}
	return sortedCodes(codes)
}

// InspectSwapTransaction checks a signed swap transaction: the message
// inspection, then every signature over the exact message bytes. It returns
// the TxID, or SIGNING_REJECTED with the reason codes.
func InspectSwapTransaction(e SwapExpectation, raw []byte) (devnet.TxID, error) {
	return inspectSigned(raw, func(m devnet.Message) []string { return InspectSwapMessage(e, m) })
}

// InspectWithdrawalTransaction checks a signed withdrawal transaction.
func InspectWithdrawalTransaction(e WithdrawalExpectation, raw []byte) (devnet.TxID, error) {
	return inspectSigned(raw, func(m devnet.Message) []string { return InspectWithdrawalMessage(e, m) })
}

func inspectSigned(raw []byte, inspect func(devnet.Message) []string) (devnet.TxID, error) {
	tx, err := devnet.Decode(raw)
	if err != nil {
		return "", inspectionRefused([]string{InspectMalformed})
	}
	if codes := inspect(tx.Message); len(codes) > 0 {
		return "", inspectionRefused(codes)
	}
	if err := tx.VerifySignatures(); err != nil {
		return "", inspectionRefused([]string{InspectSignatureInvalid})
	}
	txID, err := tx.ID()
	if err != nil {
		return "", inspectionRefused([]string{InspectSignatureInvalid})
	}
	return txID, nil
}

// decodeMessage parses canonical unsigned message bytes as a transaction
// with no signatures would carry them: what is inspected is exactly what the
// chain will decode and what will be signed.
func decodeMessage(msg []byte) (devnet.Message, error) {
	if len(msg) == 0 || len(msg) > devnet.MaxTransactionSize {
		return devnet.Message{}, inspectionRefused([]string{InspectMalformed})
	}
	var tmp devnet.Transaction
	raw, err := wrapMessage(msg)
	if err != nil {
		return devnet.Message{}, inspectionRefused([]string{InspectMalformed})
	}
	if tmp, err = devnet.Decode(raw); err != nil {
		return devnet.Message{}, inspectionRefused([]string{InspectMalformed})
	}
	again, err := tmp.Message.Bytes()
	if err != nil || !bytes.Equal(again, msg) {
		return devnet.Message{}, inspectionRefused([]string{InspectMalformed})
	}
	return tmp.Message, nil
}

// wrapMessage frames message bytes as an unsigned version-1 transaction:
// "ndvt" | u8 version | u16 length | message | u8 0 (no signatures).
func wrapMessage(msg []byte) ([]byte, error) {
	if len(msg) > 0xFFFF {
		return nil, errs.New(errs.CodeValidationFailed, "delegation: message too large")
	}
	out := make([]byte, 0, len(msg)+8)
	out = append(out, 'n', 'd', 'v', 't', devnet.WireVersion, byte(len(msg)), byte(len(msg)>>8))
	out = append(out, msg...)
	return append(out, 0), nil
}

func inspectionRefused(codes []string) error {
	return errs.New(errs.CodeSigningRejected, "delegation: transaction inspection refused the transaction").
		WithField("reason_codes", sortedCodes(codes))
}

func sameMessage(a, b devnet.Message) bool {
	x, err1 := a.Bytes()
	y, err2 := b.Bytes()
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func sameKeys(got, want []devnet.PublicKey) bool {
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

func sortedCodes(codes []string) []string {
	out := append([]string(nil), codes...)
	sort.Strings(out)
	n := 0
	for i, c := range out {
		if i == 0 || c != out[n-1] {
			out[n] = c
			n++
		}
	}
	return out[:n]
}
