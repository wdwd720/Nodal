package ledger

// holdingCodes are the account codes whose balance is value its owner holds:
// what would be found in that owner's custody if every unit of an asset were
// counted. Every other code is a flow (TRADING_OUTFLOW, TRANSFER_OUT, FEES_*,
// OPTION_PREMIUM_PAID, WITHDRAWN, PAYOUT_SETTLED), a source of value
// (CAPITAL, TRADING_INFLOW, ...), an accrual or a claim with no value behind
// it yet (LOAN_RECEIVABLE, DEFICIT_RECEIVABLE, PERFORMANCE_FEE_ACCRUED), or a
// mirror that another party's holding already counts (STABLE_ISSUED,
// BRIDGE_MINTED, PREDICTION_SHARES_ISSUED; BRIDGE_IN_FLIGHT, whose units the
// bridge facility holds as BRIDGE_LOCKED while they travel).
//
// The sum over every owner of an asset's holding balances is how much of the
// asset exists on the ledger. It is what a supply reconciliation compares
// with what was issued: a stable asset's units may sit in a customer's
// wallet, in margin or derivatives collateral, in a bridge escrow, in a house
// inventory or in collected fees, and every one of them is outstanding supply.
var holdingCodes = map[Code]bool{
	CodeWallet:                true,
	CodePlatformFeeReceivable: true,
	CodeCreditBalance:         true,
	CodeNativeAssetBalance:    true,
	CodePayoutReserved:        true,
	CodeMarketReserve:         true,
	CodeMarketInventory:       true,

	CodeMarginCollateral:    true,
	CodeDerivativesMargin:   true,
	CodeUnsettledReceivable: true,
	CodeHouseInventory:      true,
	CodeStableReserve:       true,
	CodePoolCash:            true,
	CodeInsuranceFundCash:   true,
	CodeClearingCash:        true,
	CodeBridgeLocked:        true,
	CodePredictionEscrow:    true,
}

// HoldsValue reports whether a balance under code c is value its owner holds
// (see holdingCodes). Unknown codes hold nothing.
func (c Code) HoldsValue() bool { return holdingCodes[c] }

// HoldingCodes returns every holding code in chart order.
func HoldingCodes() []Code {
	out := make([]Code, 0, len(holdingCodes))
	for _, c := range allCodes {
		if holdingCodes[c] {
			out = append(out, c)
		}
	}
	return out
}
