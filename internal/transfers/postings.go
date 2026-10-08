package transfers

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// Journal references. The database (PT005) and Reconcile both key on them.
const (
	refTypeTransfer = "p2p_transfer"
	refTypeReversal = "p2p_transfer_reversal"
)

// TransferEntries is the P2P_TRANSFER template of ADVANCED_FINANCIAL_MODEL §5:
//
//	sender:    Dr TRANSFER_OUT q / Cr WALLET q
//	recipient: Dr WALLET q       / Cr TRANSFER_IN q
//
// Each party balances on its own, and the two WALLET legs cancel: the sum of
// customer WALLET balances is the same before and after.
func TransferEntries(sender, recipient accounts.AccountID, asset assets.AssetID, q money.Quantity) []ledger.Entry {
	return []ledger.Entry{
		{Account: ledger.CustomerAccount(sender, ledger.CodeTransferOut, asset), Side: ledger.Debit, Quantity: q},
		{Account: ledger.CustomerAccount(sender, ledger.CodeWallet, asset), Side: ledger.Credit, Quantity: q},
		{Account: ledger.CustomerAccount(recipient, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: q},
		{Account: ledger.CustomerAccount(recipient, ledger.CodeTransferIn, asset), Side: ledger.Credit, Quantity: q},
	}
}

// ReversalEntries is the exact mirror of TransferEntries, posted as a
// COMPENSATION with reversal_of set:
//
//	sender:    Dr WALLET q      / Cr TRANSFER_OUT q
//	recipient: Dr TRANSFER_IN q / Cr WALLET q
func ReversalEntries(sender, recipient accounts.AccountID, asset assets.AssetID, q money.Quantity) []ledger.Entry {
	return []ledger.Entry{
		{Account: ledger.CustomerAccount(sender, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: q},
		{Account: ledger.CustomerAccount(sender, ledger.CodeTransferOut, asset), Side: ledger.Credit, Quantity: q},
		{Account: ledger.CustomerAccount(recipient, ledger.CodeTransferIn, asset), Side: ledger.Debit, Quantity: q},
		{Account: ledger.CustomerAccount(recipient, ledger.CodeWallet, asset), Side: ledger.Credit, Quantity: q},
	}
}

// transferPosting is the journal of one completed transfer. The memo is not
// in it: not in the description, not in the metadata, not in the content
// hash. The description is fixed text so nothing a customer types can reach
// the ledger.
func transferPosting(t Transfer, now time.Time) ledger.Posting {
	return ledger.Posting{
		Kind:           ledger.KindP2PTransfer,
		IdempotencyKey: "p2p:" + t.ID.String(),
		Reference:      ledger.FinancialEventReference{Type: refTypeTransfer, ID: t.ID.String()},
		EffectiveAt:    now,
		Description:    "simulated customer-to-customer transfer",
		CorrelationID:  t.CorrelationID,
		Entries:        TransferEntries(t.SenderAccountID, t.RecipientAccountID, t.AssetID, t.Quantity),
	}
}

// reversalPosting is the COMPENSATION that reverses t's journal.
func reversalPosting(t Transfer, reasonCode, correlationID string, now time.Time) ledger.Posting {
	original := *t.JournalTransactionID
	return ledger.Posting{
		Kind:           ledger.KindCompensation,
		IdempotencyKey: "p2p-reversal:" + t.ID.String(),
		Reference:      ledger.FinancialEventReference{Type: refTypeReversal, ID: t.ID.String()},
		EffectiveAt:    now,
		Description:    "reversal of a simulated customer-to-customer transfer",
		CorrelationID:  correlationID,
		ReversalOf:     &original,
		Entries:        ReversalEntries(t.SenderAccountID, t.RecipientAccountID, t.AssetID, t.Quantity),
		Metadata:       map[string]any{ledger.MetadataReasonCode: reasonCode},
	}
}
