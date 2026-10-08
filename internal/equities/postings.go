package equities

import (
	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// The posting templates of ADVANCED_FINANCIAL_MODEL §5 for equities. Every
// builder is pure and returns entries that balance per party; Service.Post
// and the database check the same thing again.

type entries []ledger.Entry

// add appends an entry unless its quantity is zero: a zero leg carries no
// value and the ledger refuses it.
func (e *entries) add(ref ledger.AccountRef, side ledger.Side, q money.Quantity) {
	if q.IsZero() {
		return
	}
	*e = append(*e, ledger.Entry{Account: ref, Side: side, Quantity: q})
}

// TradeCharges is how a fee quote's charges are booked.
type TradeCharges struct {
	// Notional is the reference notional paid to (BUY) or received from
	// (SELL) the external venue.
	Notional money.Quantity
	// Spread is the disclosed spread: FEES_SPREAD / SPREAD_REVENUE.
	Spread money.Quantity
	// PlatformFees are the explicit fixed, percentage, minimum-top-up and
	// strategy fees: FEES_PLATFORM / PLATFORM_FEE_REVENUE.
	PlatformFees money.Quantity
	// VenueFees are the venue pass-through: FEES_VENUE, paid out to the
	// external venue like the notional.
	VenueFees money.Quantity
	// Total is what the customer pays (BUY) or receives (SELL).
	Total money.Quantity
}

// ChargesOf derives the booking of a fee quote from its revenue split and
// proves it sums to the quote's totals. A network cost has no meaning for an
// equity trade and is refused.
func ChargesOf(q fees.Quote) (TradeCharges, error) {
	split := q.RevenueSplit()
	line := func(c fees.BookingCategory) money.Quantity {
		l, _ := split.Line(c)
		return l.Amount
	}
	if net := line(fees.BookNetworkFeePassThrough); !net.IsZero() {
		return TradeCharges{}, errs.New(errs.CodeValidationFailed, "equities: an equity trade carries no network cost")
	}
	c := TradeCharges{
		Notional:     q.NotionalAtReference,
		Spread:       line(fees.BookSpreadRevenue),
		PlatformFees: line(fees.BookPlatformFeeRevenue).Add(line(fees.BookStrategyFeeRevenue)),
		VenueFees:    line(fees.BookVenueFeePassThrough),
		Total:        q.Total(),
	}
	charged := c.Spread.Add(c.PlatformFees).Add(c.VenueFees)
	var want money.Quantity
	switch q.Input.Side {
	case fees.SideBuy:
		want = c.Notional.Add(charged)
	case fees.SideSell:
		want = c.Notional.Sub(charged)
	default:
		return TradeCharges{}, errs.New(errs.CodeValidationFailed, "equities: a quote is a BUY or a SELL")
	}
	if !want.Equal(c.Total) {
		return TradeCharges{}, errs.New(errs.CodeInternal, "equities: the quote's charges do not sum to its total")
	}
	if !c.Notional.IsPositive() || !c.Total.IsPositive() {
		return TradeCharges{}, errs.New(errs.CodeValidationFailed,
			"equities: a trade must move value both ways: its notional and its total must be positive").
			WithField("rejection_code", string(RejectNotionalTooSmall))
	}
	return c, nil
}

// TradeEntries builds the SIM_TRADE posting of a fill on the trade date.
//
//	BUY   customer cash    Cr WALLET total
//	                       Dr TRADING_OUTFLOW notional, Dr FEES_SPREAD spread,
//	                       Dr FEES_PLATFORM fees, Dr FEES_VENUE venue
//	      customer shares  Dr UNSETTLED_RECEIVABLE qty / Cr TRADING_INFLOW qty
//	SELL  customer shares  Cr WALLET qty / Dr TRADING_OUTFLOW qty
//	      customer cash    Dr UNSETTLED_RECEIVABLE total, Dr FEES_SPREAD spread,
//	                       Dr FEES_PLATFORM fees, Dr FEES_VENUE venue
//	                       Cr TRADING_INFLOW notional
//	both  platform cash    Dr PLATFORM_FEE_RECEIVABLE spread + fees
//	                       Cr SPREAD_REVENUE spread, Cr PLATFORM_FEE_REVENUE fees
//
// A seller pays the spread and fees out of the proceeds, so the net proceeds
// are what waits in UNSETTLED_RECEIVABLE. The venue is external and never
// booked: the notional leaves (or arrives) through TRADING_OUTFLOW /
// TRADING_INFLOW, exactly as on the spot venue.
func TradeEntries(acct accounts.AccountID, side Side, share, cash assets.AssetID, qty money.Quantity, c TradeCharges) ([]ledger.Entry, error) {
	if !qty.IsPositive() {
		return nil, errs.New(errs.CodeValidationFailed, "equities: a fill quantity is positive")
	}
	cust := func(code ledger.Code, a assets.AssetID) ledger.AccountRef {
		return ledger.CustomerAccount(acct, code, a)
	}
	var e entries
	switch side {
	case SideBuy:
		e.add(cust(ledger.CodeWallet, cash), ledger.Credit, c.Total)
		e.add(cust(ledger.CodeTradingOutflow, cash), ledger.Debit, c.Notional)
		e.add(cust(ledger.CodeUnsettledReceivable, share), ledger.Debit, qty)
		e.add(cust(ledger.CodeTradingInflow, share), ledger.Credit, qty)
	case SideSell:
		e.add(cust(ledger.CodeWallet, share), ledger.Credit, qty)
		e.add(cust(ledger.CodeTradingOutflow, share), ledger.Debit, qty)
		e.add(cust(ledger.CodeUnsettledReceivable, cash), ledger.Debit, c.Total)
		e.add(cust(ledger.CodeTradingInflow, cash), ledger.Credit, c.Notional)
	default:
		return nil, errs.New(errs.CodeValidationFailed, "equities: unknown side")
	}
	e.add(cust(ledger.CodeFeesSpread, cash), ledger.Debit, c.Spread)
	e.add(cust(ledger.CodeFeesPlatform, cash), ledger.Debit, c.PlatformFees)
	e.add(cust(ledger.CodeFeesVenue, cash), ledger.Debit, c.VenueFees)
	e.add(ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, cash), ledger.Debit, c.Spread.Add(c.PlatformFees))
	e.add(ledger.PlatformAccount(ledger.CodeSpreadRevenue, cash), ledger.Credit, c.Spread)
	e.add(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, cash), ledger.Credit, c.PlatformFees)
	return e, nil
}

// SettlementEntries builds the EQUITY_SETTLEMENT posting: the asset a trade
// delivered moves from UNSETTLED_RECEIVABLE into WALLET (shares for a buy,
// net proceeds for a sell).
func SettlementEntries(acct accounts.AccountID, asset assets.AssetID, qty money.Quantity) ([]ledger.Entry, error) {
	if !qty.IsPositive() {
		return nil, errs.New(errs.CodeValidationFailed, "equities: a settlement quantity is positive")
	}
	return []ledger.Entry{
		{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: qty},
		{Account: ledger.CustomerAccount(acct, ledger.CodeUnsettledReceivable, asset), Side: ledger.Credit, Quantity: qty},
	}, nil
}

// SplitEntries builds the CORPORATE_ACTION posting of one holder's split or
// reverse split: new shares Dr WALLET / Cr CORPORATE_ACTIONS (a split),
// shares withdrawn Cr WALLET / Dr CORPORATE_ACTIONS (a reverse split), and
// cash-in-lieu Dr WALLET(cash) / Cr CORPORATE_ACTIONS(cash). It returns no
// entries when nothing changes.
func SplitEntries(acct accounts.AccountID, share, cash assets.AssetID, o SplitOutcome) []ledger.Entry {
	cust := func(code ledger.Code, a assets.AssetID) ledger.AccountRef {
		return ledger.CustomerAccount(acct, code, a)
	}
	var e entries
	delta := o.SharesDelta()
	switch delta.Sign() {
	case 1:
		e.add(cust(ledger.CodeWallet, share), ledger.Debit, delta)
		e.add(cust(ledger.CodeCorporateActions, share), ledger.Credit, delta)
	case -1:
		e.add(cust(ledger.CodeWallet, share), ledger.Credit, delta.Neg())
		e.add(cust(ledger.CodeCorporateActions, share), ledger.Debit, delta.Neg())
	}
	e.add(cust(ledger.CodeWallet, cash), ledger.Debit, o.CashInLieu)
	e.add(cust(ledger.CodeCorporateActions, cash), ledger.Credit, o.CashInLieu)
	return e
}

// DividendEntries builds the CORPORATE_ACTION posting of one holder's cash
// dividend: Dr WALLET(cash) / Cr DIVIDENDS(cash). No entries for zero.
func DividendEntries(acct accounts.AccountID, cash assets.AssetID, amount money.Quantity) []ledger.Entry {
	var e entries
	e.add(ledger.CustomerAccount(acct, ledger.CodeWallet, cash), ledger.Debit, amount)
	e.add(ledger.CustomerAccount(acct, ledger.CodeDividends, cash), ledger.Credit, amount)
	return e
}
