package stableasset

import (
	"time"

	"github.com/nodal/controlplane/internal/ledger"
)

// issuancePosting is STABLE_ISSUANCE exactly as model §5 states it:
//
//	C:          Cr WALLET(reserve)        / Dr TRADING_OUTFLOW(reserve)
//	            Dr WALLET(stable)         / Cr TRADING_INFLOW(stable)
//	F(issuer):  Dr STABLE_RESERVE         / Cr STABLE_RESERVE_FUNDING   (reserve asset)
//	            Dr STABLE_ISSUED          / Cr STABLE_OUTSTANDING       (stable asset)
//
// Every party balances per asset (SM002). The idempotency key is the
// request's, so a replay of the settlement posts nothing twice.
func issuancePosting(a PlatformStableAsset, r MintRecord, at time.Time) ledger.Posting {
	q, rq := r.Quantity, r.ReserveQuantity
	return ledger.Posting{
		Kind:           ledger.KindStableIssuance,
		IdempotencyKey: "stableasset:issuance:" + r.ID.String(),
		Reference:      ledger.FinancialEventReference{Type: "stable_mint_request", ID: r.ID.String()},
		EffectiveAt:    at,
		Description:    "sandbox stable asset issuance (simulated value)",
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(r.AccountID, ledger.CodeWallet, r.ReserveAssetID), Side: ledger.Credit, Quantity: rq},
			{Account: ledger.CustomerAccount(r.AccountID, ledger.CodeTradingOutflow, r.ReserveAssetID), Side: ledger.Debit, Quantity: rq},
			{Account: ledger.CustomerAccount(r.AccountID, ledger.CodeWallet, a.AssetID), Side: ledger.Debit, Quantity: q},
			{Account: ledger.CustomerAccount(r.AccountID, ledger.CodeTradingInflow, a.AssetID), Side: ledger.Credit, Quantity: q},
			{Account: a.account(ledger.CodeStableReserve, r.ReserveAssetID), Side: ledger.Debit, Quantity: rq},
			{Account: a.account(ledger.CodeStableReserveFunding, r.ReserveAssetID), Side: ledger.Credit, Quantity: rq},
			{Account: a.account(ledger.CodeStableIssued, a.AssetID), Side: ledger.Debit, Quantity: q},
			{Account: a.account(ledger.CodeStableOutstanding, a.AssetID), Side: ledger.Credit, Quantity: q},
		},
		Metadata: map[string]any{
			"stable_asset_id": a.ID.String(), "chain": string(r.Chain), "tx_id": string(r.TxID),
		},
	}
}

// redemptionPosting is STABLE_REDEMPTION, the inverse flow: the holder
// disposes of the stable asset and acquires the reserve asset through the
// trading pair codes, and the issuer's books are the exact side-swap of
// issuance -- the reserve leaves and the supply is retired.
//
//	C:          Cr WALLET(stable)         / Dr TRADING_OUTFLOW(stable)
//	            Dr WALLET(payout)         / Cr TRADING_INFLOW(payout)
//	F(issuer):  Cr STABLE_RESERVE         / Dr STABLE_RESERVE_FUNDING   (payout asset)
//	            Cr STABLE_ISSUED          / Dr STABLE_OUTSTANDING       (stable asset)
func redemptionPosting(a PlatformStableAsset, r BurnRecord, at time.Time) ledger.Posting {
	q, p := r.Quantity, r.PayoutQuantity
	return ledger.Posting{
		Kind:           ledger.KindStableRedemption,
		IdempotencyKey: "stableasset:redemption:" + r.ID.String(),
		Reference:      ledger.FinancialEventReference{Type: "stable_burn_request", ID: r.ID.String()},
		EffectiveAt:    at,
		Description:    "sandbox stable asset redemption (simulated value)",
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(r.AccountID, ledger.CodeWallet, a.AssetID), Side: ledger.Credit, Quantity: q},
			{Account: ledger.CustomerAccount(r.AccountID, ledger.CodeTradingOutflow, a.AssetID), Side: ledger.Debit, Quantity: q},
			{Account: ledger.CustomerAccount(r.AccountID, ledger.CodeWallet, r.PayoutAssetID), Side: ledger.Debit, Quantity: p},
			{Account: ledger.CustomerAccount(r.AccountID, ledger.CodeTradingInflow, r.PayoutAssetID), Side: ledger.Credit, Quantity: p},
			{Account: a.account(ledger.CodeStableReserve, r.PayoutAssetID), Side: ledger.Credit, Quantity: p},
			{Account: a.account(ledger.CodeStableReserveFunding, r.PayoutAssetID), Side: ledger.Debit, Quantity: p},
			{Account: a.account(ledger.CodeStableIssued, a.AssetID), Side: ledger.Credit, Quantity: q},
			{Account: a.account(ledger.CodeStableOutstanding, a.AssetID), Side: ledger.Debit, Quantity: q},
		},
		Metadata: map[string]any{
			"stable_asset_id": a.ID.String(), "chain": string(r.Chain), "tx_id": string(r.TxID),
		},
	}
}
