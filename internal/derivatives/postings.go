package derivatives

import (
	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// The posting templates of ADVANCED_FINANCIAL_MODEL §5 this package uses.
// Every builder returns entries that balance per party -- each (owner, asset)
// debits exactly what it credits -- so value only ever moves between the
// customer and the clearing house as an explicit pair. A builder never emits
// a zero entry; a template with nothing to move returns no entries.
//
//	DERIVATIVE_MARGIN_MOVE  post:     C: Dr DERIVATIVES_MARGIN / Cr WALLET
//	                        release:  C: Dr WALLET / Cr DERIVATIVES_MARGIN
//	VARIATION_MARGIN, FUNDING_PAYMENT, FUTURE_SETTLEMENT, and the cash of an
//	OPTION_SETTLEMENT or of a closing OPTION_PREMIUM
//	                        payer:    C: Dr REALIZED_PNL / Cr WALLET, Cr DERIVATIVES_MARGIN
//	                                  F: Dr CLEARING_CASH / Cr CLEARING_RESULT
//	                        receiver: C: Dr WALLET / Cr REALIZED_PNL
//	                                  F: Dr CLEARING_RESULT / Cr CLEARING_CASH
//	OPTION_PREMIUM          buyer:    C: Dr OPTION_PREMIUM_PAID / Cr WALLET
//	                                  F: Dr CLEARING_CASH / Cr CLEARING_RESULT
//	                        writer:   C: Dr WALLET / Cr OPTION_PREMIUM_RECEIVED
//	                                  F: Dr CLEARING_RESULT / Cr CLEARING_CASH
//	premium recognition when an option closes, is exercised or settles
//	(part of the OPTION_PREMIUM or OPTION_SETTLEMENT posting):
//	                        holder:   C: Dr REALIZED_PNL / Cr OPTION_PREMIUM_PAID
//	                        writer:   C: Dr OPTION_PREMIUM_RECEIVED / Cr REALIZED_PNL
//	FACILITY_FUNDING        F: Dr CLEARING_CASH / Cr CLEARING_CAPITAL
//
// An internally matched book moves CLEARING_CASH and CLEARING_RESULT by equal
// and opposite amounts, so both net to zero.

// payment is how an obligation of the customer is met: from the wallet
// first, then from the posted margin of the position; whatever neither covers
// is a shortfall the clearing house absorbs (recorded on the cash flow and the
// position, never posted as a negative balance).
type payment struct {
	FromWallet money.Quantity
	FromMargin money.Quantity
	Shortfall  money.Quantity
}

// collected is what actually moves.
func (p payment) collected() money.Quantity { return p.FromWallet.Add(p.FromMargin) }

// splitPayment allocates amount (>= 0) across the wallet balance and the
// position's margin.
func splitPayment(amount, wallet, margin money.Quantity) payment {
	w := amount.Min(wallet.Max(money.Quantity{}))
	rest := amount.Sub(w)
	m := rest.Min(margin.Max(money.Quantity{}))
	return payment{FromWallet: w, FromMargin: m, Shortfall: rest.Sub(m)}
}

func entry(ref ledger.AccountRef, side ledger.Side, q money.Quantity) ledger.Entry {
	return ledger.Entry{Account: ref, Side: side, Quantity: q}
}

func cust(a accounts.AccountID, code ledger.Code, asset assets.AssetID) ledger.AccountRef {
	return ledger.CustomerAccount(a, code, asset)
}

// marginMoveEntries posts (post=true) or releases initial margin.
func marginMoveEntries(a accounts.AccountID, asset assets.AssetID, q money.Quantity, post bool) []ledger.Entry {
	if !q.IsPositive() {
		return nil
	}
	if post {
		return []ledger.Entry{
			entry(cust(a, ledger.CodeDerivativesMargin, asset), ledger.Debit, q),
			entry(cust(a, ledger.CodeWallet, asset), ledger.Credit, q),
		}
	}
	return []ledger.Entry{
		entry(cust(a, ledger.CodeWallet, asset), ledger.Debit, q),
		entry(cust(a, ledger.CodeDerivativesMargin, asset), ledger.Credit, q),
	}
}

// receiveEntries pays amount from the clearing house to the customer.
func receiveEntries(a accounts.AccountID, ch econ.Facility, asset assets.AssetID, amount money.Quantity) []ledger.Entry {
	if !amount.IsPositive() {
		return nil
	}
	return []ledger.Entry{
		entry(cust(a, ledger.CodeWallet, asset), ledger.Debit, amount),
		entry(cust(a, ledger.CodeRealizedPnL, asset), ledger.Credit, amount),
		entry(ch.Account(ledger.CodeClearingResult, asset), ledger.Debit, amount),
		entry(ch.Account(ledger.CodeClearingCash, asset), ledger.Credit, amount),
	}
}

// payEntries collects a payment from the customer into the clearing house.
func payEntries(a accounts.AccountID, ch econ.Facility, asset assets.AssetID, p payment) []ledger.Entry {
	c := p.collected()
	if !c.IsPositive() {
		return nil
	}
	out := []ledger.Entry{entry(cust(a, ledger.CodeRealizedPnL, asset), ledger.Debit, c)}
	if p.FromWallet.IsPositive() {
		out = append(out, entry(cust(a, ledger.CodeWallet, asset), ledger.Credit, p.FromWallet))
	}
	if p.FromMargin.IsPositive() {
		out = append(out, entry(cust(a, ledger.CodeDerivativesMargin, asset), ledger.Credit, p.FromMargin))
	}
	return append(out,
		entry(ch.Account(ledger.CodeClearingCash, asset), ledger.Debit, c),
		entry(ch.Account(ledger.CodeClearingResult, asset), ledger.Credit, c),
	)
}

// premiumPaidEntries: an option buyer pays premium to the clearing house.
func premiumPaidEntries(a accounts.AccountID, ch econ.Facility, asset assets.AssetID, premium money.Quantity) []ledger.Entry {
	if !premium.IsPositive() {
		return nil
	}
	return []ledger.Entry{
		entry(cust(a, ledger.CodeOptionPremiumPaid, asset), ledger.Debit, premium),
		entry(cust(a, ledger.CodeWallet, asset), ledger.Credit, premium),
		entry(ch.Account(ledger.CodeClearingCash, asset), ledger.Debit, premium),
		entry(ch.Account(ledger.CodeClearingResult, asset), ledger.Credit, premium),
	}
}

// premiumReceivedEntries: an option writer receives premium from the
// clearing house.
func premiumReceivedEntries(a accounts.AccountID, ch econ.Facility, asset assets.AssetID, premium money.Quantity) []ledger.Entry {
	if !premium.IsPositive() {
		return nil
	}
	return []ledger.Entry{
		entry(cust(a, ledger.CodeWallet, asset), ledger.Debit, premium),
		entry(cust(a, ledger.CodeOptionPremiumReceived, asset), ledger.Credit, premium),
		entry(ch.Account(ledger.CodeClearingResult, asset), ledger.Debit, premium),
		entry(ch.Account(ledger.CodeClearingCash, asset), ledger.Credit, premium),
	}
}

// recognizePremiumEntries moves the premium share of contracts that closed
// out of the open-premium account into realized PnL. allocatedCost is the
// position's signed cost share: positive (premium paid) for a holder,
// negative (premium received) for a writer.
func recognizePremiumEntries(a accounts.AccountID, asset assets.AssetID, allocatedCost money.Quantity) []ledger.Entry {
	switch allocatedCost.Sign() {
	case 1:
		return []ledger.Entry{
			entry(cust(a, ledger.CodeRealizedPnL, asset), ledger.Debit, allocatedCost),
			entry(cust(a, ledger.CodeOptionPremiumPaid, asset), ledger.Credit, allocatedCost),
		}
	case -1:
		q := allocatedCost.Neg()
		return []ledger.Entry{
			entry(cust(a, ledger.CodeOptionPremiumReceived, asset), ledger.Debit, q),
			entry(cust(a, ledger.CodeRealizedPnL, asset), ledger.Credit, q),
		}
	}
	return nil
}

// clearingFundingEntries capitalizes the clearing house.
func clearingFundingEntries(ch econ.Facility, asset assets.AssetID, q money.Quantity) []ledger.Entry {
	if !q.IsPositive() {
		return nil
	}
	return []ledger.Entry{
		entry(ch.Account(ledger.CodeClearingCash, asset), ledger.Debit, q),
		entry(ch.Account(ledger.CodeClearingCapital, asset), ledger.Credit, q),
	}
}

// cashEntries is the clearing-house cash flow of signed amount for a customer:
// received when positive, paid (through pay) when negative.
func cashEntries(a accounts.AccountID, ch econ.Facility, asset assets.AssetID, amount money.Quantity, pay payment) []ledger.Entry {
	if amount.IsPositive() {
		return receiveEntries(a, ch, asset, amount)
	}
	return payEntries(a, ch, asset, pay)
}
