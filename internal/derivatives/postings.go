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
//	                        payer:    C: Dr REALIZED_PNL / Cr DERIVATIVES_MARGIN (the position's own
//	                                     margin: the share a reduction frees first, then what it keeps),
//	                                     Cr WALLET (available funds only, under a consumed reservation)
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

// payment is how an obligation of the customer is met, in this order: from
// the margin a reduction itself frees (before it ever reaches the wallet),
// from the customer's available wallet funds -- what is neither reserved nor
// held for anything else, taken under a capital reservation that is consumed
// -- and from the margin the position keeps. Whatever none of them covers is
// the package's default path: a shortfall the clearing house absorbs,
// recorded on the cash flow and the position (and reported by reconciliation
// as the register's shortfall), never posted as a negative balance and never
// taken from funds reserved or held for something else.
type payment struct {
	FromRelease money.Quantity
	FromWallet  money.Quantity
	FromMargin  money.Quantity
	Shortfall   money.Quantity
}

// collected is what actually moves.
func (p payment) collected() money.Quantity {
	return p.FromRelease.Add(p.FromWallet).Add(p.FromMargin)
}

// fromMarginAccount is what the payment takes from DERIVATIVES_MARGIN.
func (p payment) fromMarginAccount() money.Quantity { return p.FromRelease.Add(p.FromMargin) }

// splitPayment allocates amount (>= 0) across the margin a reduction
// releases, the customer's available wallet funds and the margin the position
// keeps, in that order.
func splitPayment(amount, released, available, kept money.Quantity) payment {
	zero := money.Quantity{}
	r := amount.Min(released.Max(zero))
	rest := amount.Sub(r)
	w := rest.Min(available.Max(zero))
	rest = rest.Sub(w)
	m := rest.Min(kept.Max(zero))
	return payment{FromRelease: r, FromWallet: w, FromMargin: m, Shortfall: rest.Sub(m)}
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
	if m := p.fromMarginAccount(); m.IsPositive() {
		out = append(out, entry(cust(a, ledger.CodeDerivativesMargin, asset), ledger.Credit, m))
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
