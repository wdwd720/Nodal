package bridge

import (
	"time"

	"github.com/nodal/controlplane/internal/ledger"
)

func legReference(l Leg) ledger.FinancialEventReference {
	return ledger.FinancialEventReference{Type: "bridge_leg", ID: l.ID.String()}
}

// sourceFinalizedPosting is BRIDGE_SOURCE_FINALIZED (model §5):
//
//	C:          Dr BRIDGE_IN_FLIGHT(src) / Cr WALLET(src)
//	F(bridge):  Dr BRIDGE_LOCKED(src)    / Cr BRIDGE_LOCK_FUNDING(src)
func sourceFinalizedPosting(r Route, l Leg, at time.Time) ledger.Posting {
	q := l.Quantity
	return ledger.Posting{
		Kind: ledger.KindBridgeSourceFinalized, IdempotencyKey: "bridge:source:" + l.ID.String(), Reference: legReference(l),
		EffectiveAt: at, Description: "sandbox bridge: source lock finalized (simulated value)",
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(l.AccountID, ledger.CodeBridgeInFlight, r.SourceAssetID), Side: ledger.Debit, Quantity: q},
			{Account: ledger.CustomerAccount(l.AccountID, ledger.CodeWallet, r.SourceAssetID), Side: ledger.Credit, Quantity: q},
			{Account: r.account(ledger.CodeBridgeLocked, r.SourceAssetID), Side: ledger.Debit, Quantity: q},
			{Account: r.account(ledger.CodeBridgeLockFunding, r.SourceAssetID), Side: ledger.Credit, Quantity: q},
		},
		Metadata: map[string]any{"route_id": r.ID.String(), "lock_tx_id": string(l.LockTxID)},
	}
}

// destinationFinalizedPosting is BRIDGE_DESTINATION_FINALIZED (model §5):
//
//	C:          Dr TRADING_OUTFLOW(src) / Cr BRIDGE_IN_FLIGHT(src)
//	            Dr WALLET(dst)          / Cr TRADING_INFLOW(dst)
//	F(bridge):  Dr BRIDGE_MINTED(dst)   / Cr BRIDGE_OUTSTANDING(dst)
func destinationFinalizedPosting(r Route, l Leg, releaseTx string, at time.Time) ledger.Posting {
	q := l.Quantity
	return ledger.Posting{
		Kind: ledger.KindBridgeDestinationFinalized, IdempotencyKey: "bridge:destination:" + l.ID.String(), Reference: legReference(l),
		EffectiveAt: at, Description: "sandbox bridge: destination release finalized (simulated value)",
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(l.AccountID, ledger.CodeTradingOutflow, r.SourceAssetID), Side: ledger.Debit, Quantity: q},
			{Account: ledger.CustomerAccount(l.AccountID, ledger.CodeBridgeInFlight, r.SourceAssetID), Side: ledger.Credit, Quantity: q},
			{Account: ledger.CustomerAccount(l.AccountID, ledger.CodeWallet, r.DestinationAssetID), Side: ledger.Debit, Quantity: q},
			{Account: ledger.CustomerAccount(l.AccountID, ledger.CodeTradingInflow, r.DestinationAssetID), Side: ledger.Credit, Quantity: q},
			{Account: r.account(ledger.CodeBridgeMinted, r.DestinationAssetID), Side: ledger.Debit, Quantity: q},
			{Account: r.account(ledger.CodeBridgeOutstanding, r.DestinationAssetID), Side: ledger.Credit, Quantity: q},
		},
		Metadata: map[string]any{"route_id": r.ID.String(), "release_tx_id": releaseTx},
	}
}

// refundPosting is BRIDGE_REFUND (model §5):
//
//	C:          Dr WALLET(src)             / Cr BRIDGE_IN_FLIGHT(src)
//	F(bridge):  Dr BRIDGE_LOCK_FUNDING(src) / Cr BRIDGE_LOCKED(src)
func refundPosting(r Route, l Leg, refundTx string, at time.Time) ledger.Posting {
	q := l.Quantity
	return ledger.Posting{
		Kind: ledger.KindBridgeRefund, IdempotencyKey: "bridge:refund:" + l.ID.String(), Reference: legReference(l),
		EffectiveAt: at, Description: "sandbox bridge: refund after a destination that never released (simulated value)",
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(l.AccountID, ledger.CodeWallet, r.SourceAssetID), Side: ledger.Debit, Quantity: q},
			{Account: ledger.CustomerAccount(l.AccountID, ledger.CodeBridgeInFlight, r.SourceAssetID), Side: ledger.Credit, Quantity: q},
			{Account: r.account(ledger.CodeBridgeLockFunding, r.SourceAssetID), Side: ledger.Debit, Quantity: q},
			{Account: r.account(ledger.CodeBridgeLocked, r.SourceAssetID), Side: ledger.Credit, Quantity: q},
		},
		Metadata: map[string]any{"route_id": r.ID.String(), "refund_tx_id": refundTx},
	}
}
