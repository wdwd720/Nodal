package delegation

import (
	"encoding/hex"
	"time"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
)

// Reference types of the journals this package posts. The database binds a
// settled swap or withdrawal to exactly its journal through them (DG006,
// DW007).
const (
	ReferenceSwap       = "delegated_swap"
	ReferenceWithdrawal = "delegated_withdrawal"
)

// SwapPosting is the SIM_TRADE of a settled delegated swap (model §5): the
// customer pays AmountIn of the pay asset and receives AmountOut of the
// receive asset; the venue's house book is the counterparty. Every party
// balances per asset.
//
//	customer  Dr TRADING_OUTFLOW(pay)        / Cr WALLET(pay)               amount in
//	          Dr WALLET(receive)             / Cr TRADING_INFLOW(receive)   amount out
//	house     Dr HOUSE_INVENTORY(pay)        / Cr HOUSE_TRADING_INFLOW(pay) amount in
//	          Dr HOUSE_TRADING_OUTFLOW(recv) / Cr HOUSE_INVENTORY(recv)     amount out
func SwapPosting(sw Swap, house econ.HouseAccountID, effectiveAt time.Time) ledger.Posting {
	h := econ.HouseAccount{ID: house}
	return ledger.Posting{
		Kind: ledger.KindSimTrade, IdempotencyKey: "delegation:swap:" + sw.ID.String(),
		Reference:   ledger.FinancialEventReference{Type: ReferenceSwap, ID: sw.ID.String()},
		EffectiveAt: effectiveAt, Description: "delegated swap (simulated value, devnet)",
		Metadata: map[string]any{"swap_id": sw.ID.String(), "tx_id": string(sw.TxID),
			"plan_hash": hex.EncodeToString(sw.PlanHash[:])},
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(sw.AccountID, ledger.CodeTradingOutflow, sw.PayAssetID), Side: ledger.Debit, Quantity: sw.AmountIn},
			{Account: ledger.CustomerAccount(sw.AccountID, ledger.CodeWallet, sw.PayAssetID), Side: ledger.Credit, Quantity: sw.AmountIn},
			{Account: ledger.CustomerAccount(sw.AccountID, ledger.CodeWallet, sw.ReceiveAssetID), Side: ledger.Debit, Quantity: sw.AmountOut},
			{Account: ledger.CustomerAccount(sw.AccountID, ledger.CodeTradingInflow, sw.ReceiveAssetID), Side: ledger.Credit, Quantity: sw.AmountOut},
			{Account: h.Account(ledger.CodeHouseInventory, sw.PayAssetID), Side: ledger.Debit, Quantity: sw.AmountIn},
			{Account: h.Account(ledger.CodeHouseTradingInflow, sw.PayAssetID), Side: ledger.Credit, Quantity: sw.AmountIn},
			{Account: h.Account(ledger.CodeHouseTradingOutflow, sw.ReceiveAssetID), Side: ledger.Debit, Quantity: sw.AmountOut},
			{Account: h.Account(ledger.CodeHouseInventory, sw.ReceiveAssetID), Side: ledger.Credit, Quantity: sw.AmountOut},
		},
	}
}

// WithdrawalPosting is the SANDBOX_WITHDRAWAL of a settled delegated
// withdrawal (model §5): C: Dr WITHDRAWN / Cr WALLET.
func WithdrawalPosting(w Withdrawal, effectiveAt time.Time) ledger.Posting {
	return ledger.Posting{
		Kind: ledger.KindSandboxWithdrawal, IdempotencyKey: "delegation:withdrawal:" + w.ID.String(),
		Reference:   ledger.FinancialEventReference{Type: ReferenceWithdrawal, ID: w.ID.String()},
		EffectiveAt: effectiveAt, Description: "delegated withdrawal (simulated value, devnet)",
		Metadata: map[string]any{"withdrawal_id": w.ID.String(), "tx_id": string(w.TxID),
			"plan_hash": hex.EncodeToString(w.PlanHash[:])},
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(w.AccountID, ledger.CodeWithdrawn, w.AssetID), Side: ledger.Debit, Quantity: w.Amount},
			{Account: ledger.CustomerAccount(w.AccountID, ledger.CodeWallet, w.AssetID), Side: ledger.Credit, Quantity: w.Amount},
		},
	}
}

const evidenceDisclaimer = "simulated sandbox value on a simulated chain; not money"

// settlementEvidence is the evidence document of a settled swap or
// withdrawal: what was planned, decided, signed, observed, reconciled and
// posted.
type settlementEvidence struct {
	Disclaimer           string          `json:"disclaimer"`
	Kind                 string          `json:"kind"`
	SubjectID            string          `json:"subject_id"`
	CapabilityID         string          `json:"capability_id"`
	IntentID             string          `json:"intent_id"`
	Plan                 any             `json:"plan"`
	PlanHash             string          `json:"plan_hash"`
	Approval             *approvalRecord `json:"approval,omitempty"`
	Chain                string          `json:"chain"`
	TxID                 string          `json:"tx_id"`
	SignedTxSHA256       string          `json:"signed_tx_sha256"`
	ObservedState        string          `json:"observed_state"`
	ObservedSlot         string          `json:"observed_slot"`
	FinalizedHeight      string          `json:"finalized_height"`
	Reconciled           bool            `json:"reconciled"`
	JournalTransactionID string          `json:"journal_transaction_id"`
}

type approvalRecord struct {
	Decision     string    `json:"decision"`
	ApproverType string    `json:"approver_type"`
	ApproverID   string    `json:"approver_id"`
	DecidedAt    time.Time `json:"decided_at"`
}

func approvalEvidence(a *approvalRow) *approvalRecord {
	if a == nil {
		return nil
	}
	return &approvalRecord{Decision: string(a.Decision), ApproverType: string(a.ApproverType), ApproverID: a.ApproverID,
		DecidedAt: a.DecidedAt.UTC()}
}
