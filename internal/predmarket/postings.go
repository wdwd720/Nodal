package predmarket

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// The ledger templates of ADVANCED_FINANCIAL_MODEL §5 for a prediction
// market. Every posting is a simulated-only kind and balances per party: the
// customer's books and the facility's books each balance per asset, so value
// moves between them only as explicit pairs. The builders are pure and
// validate what they build (ledger.Posting.Validate), so a posting that would
// not balance per party never reaches the database -- which refuses it anyway
// (SM002).

// postingMeta is what every prediction posting carries besides its entries.
type postingMeta struct {
	Key       string
	Reference ledger.FinancialEventReference
	At        time.Time
	// Description is human-readable and covered by the content hash.
	Description string
}

type legs struct{ entries []ledger.Entry }

func (l *legs) dr(ref ledger.AccountRef, q money.Quantity) { l.add(ref, ledger.Debit, q) }
func (l *legs) cr(ref ledger.AccountRef, q money.Quantity) { l.add(ref, ledger.Credit, q) }

// add appends one entry. Zero quantities are skipped: an entry is always
// positive, and a leg that moves nothing is not a leg.
func (l *legs) add(ref ledger.AccountRef, side ledger.Side, q money.Quantity) {
	if q.IsPositive() {
		l.entries = append(l.entries, ledger.Entry{Account: ref, Side: side, Quantity: q})
	}
}

func (m Market) requireBookable() error {
	if len(m.Outcomes) < MinOutcomes || len(m.Outcomes) > MaxOutcomes {
		return errs.Newf(errs.CodeInternal, "predmarket: market %s has %d outcomes", m.ID, len(m.Outcomes))
	}
	for i, o := range m.Outcomes {
		if o.Index != i || o.ShareAssetID.IsZero() {
			return errs.Newf(errs.CodeInternal, "predmarket: market %s outcome %d is malformed", m.ID, i)
		}
	}
	if m.FacilityID.IsZero() || m.CollateralAssetID.IsZero() {
		return errs.Newf(errs.CodeInternal, "predmarket: market %s has no facility or collateral", m.ID)
	}
	return nil
}

func finish(kind ledger.Kind, meta postingMeta, l legs) (ledger.Posting, error) {
	p := ledger.Posting{
		Kind: kind, IdempotencyKey: meta.Key, Reference: meta.Reference, EffectiveAt: meta.At.UTC(),
		Description: meta.Description, Entries: l.entries,
	}
	if err := p.Validate(); err != nil {
		return ledger.Posting{}, err
	}
	return p, nil
}

// IssuancePosting is PREDICTION_SET_ISSUANCE of q complete sets bought by
// account:
//
//	C: Cr WALLET(cash) / Dr TRADING_OUTFLOW(cash),
//	   Dr WALLET(each outcome) / Cr TRADING_INFLOW(each outcome)
//	F: Dr PREDICTION_ESCROW / Cr PREDICTION_COLLATERAL (cash),
//	   Dr PREDICTION_SHARES_ISSUED / Cr PREDICTION_SHARES_OUTSTANDING (each outcome)
func IssuancePosting(m Market, account accounts.AccountID, q money.Quantity, meta postingMeta) (ledger.Posting, error) {
	if err := m.requireBookable(); err != nil {
		return ledger.Posting{}, err
	}
	if err := ValidateSetQuantity(q); err != nil {
		return ledger.Posting{}, err
	}
	f := m.Facility()
	cash := m.CollateralAssetID
	var l legs
	l.cr(ledger.CustomerAccount(account, ledger.CodeWallet, cash), q)
	l.dr(ledger.CustomerAccount(account, ledger.CodeTradingOutflow, cash), q)
	l.dr(f.Account(ledger.CodePredictionEscrow, cash), q)
	l.cr(f.Account(ledger.CodePredictionCollateral, cash), q)
	for _, o := range m.Outcomes {
		l.dr(ledger.CustomerAccount(account, ledger.CodeWallet, o.ShareAssetID), q)
		l.cr(ledger.CustomerAccount(account, ledger.CodeTradingInflow, o.ShareAssetID), q)
		l.dr(f.Account(ledger.CodePredictionSharesIssued, o.ShareAssetID), q)
		l.cr(f.Account(ledger.CodePredictionSharesOutstanding, o.ShareAssetID), q)
	}
	return finish(ledger.KindPredictionSetIssuance, meta, l)
}

// RedemptionPosting is PREDICTION_SET_REDEMPTION of q complete sets, the
// exact inverse of an issuance:
//
//	C: Dr WALLET(cash) / Cr TRADING_INFLOW(cash),
//	   Cr WALLET(each outcome) / Dr TRADING_OUTFLOW(each outcome)
//	F: Dr PREDICTION_COLLATERAL / Cr PREDICTION_ESCROW (cash),
//	   Dr PREDICTION_SHARES_OUTSTANDING / Cr PREDICTION_SHARES_ISSUED (each outcome)
func RedemptionPosting(m Market, account accounts.AccountID, q money.Quantity, meta postingMeta) (ledger.Posting, error) {
	if err := m.requireBookable(); err != nil {
		return ledger.Posting{}, err
	}
	if err := ValidateSetQuantity(q); err != nil {
		return ledger.Posting{}, err
	}
	f := m.Facility()
	cash := m.CollateralAssetID
	var l legs
	l.dr(ledger.CustomerAccount(account, ledger.CodeWallet, cash), q)
	l.cr(ledger.CustomerAccount(account, ledger.CodeTradingInflow, cash), q)
	l.dr(f.Account(ledger.CodePredictionCollateral, cash), q)
	l.cr(f.Account(ledger.CodePredictionEscrow, cash), q)
	for _, o := range m.Outcomes {
		l.cr(ledger.CustomerAccount(account, ledger.CodeWallet, o.ShareAssetID), q)
		l.dr(ledger.CustomerAccount(account, ledger.CodeTradingOutflow, o.ShareAssetID), q)
		l.dr(f.Account(ledger.CodePredictionSharesOutstanding, o.ShareAssetID), q)
		l.cr(f.Account(ledger.CodePredictionSharesIssued, o.ShareAssetID), q)
	}
	return finish(ledger.KindPredictionSetRedemption, meta, l)
}

// SettlementPosting is one holder's PREDICTION_PAYOUT (or PREDICTION_REFUND):
// every share the holder has, of every outcome, is retired, and the holder is
// paid line.Amount of collateral out of the escrow.
//
//	C: Cr WALLET(each outcome held) / Dr TRADING_OUTFLOW(each outcome held),
//	   Dr WALLET(cash) / Cr TRADING_INFLOW(cash)            (when paid)
//	F: Dr PREDICTION_SHARES_OUTSTANDING / Cr PREDICTION_SHARES_ISSUED (each outcome held),
//	   Dr PREDICTION_COLLATERAL / Cr PREDICTION_ESCROW (cash) (when paid)
func SettlementPosting(m Market, kind SettlementKind, line PlanLine, meta postingMeta) (ledger.Posting, error) {
	if err := m.requireBookable(); err != nil {
		return ledger.Posting{}, err
	}
	var jk ledger.Kind
	switch kind {
	case SettlementPayout:
		jk = ledger.KindPredictionPayout
	case SettlementRefund:
		jk = ledger.KindPredictionRefund
	default:
		return ledger.Posting{}, errs.Newf(errs.CodeValidationFailed, "predmarket: unknown settlement kind %q", kind)
	}
	if len(line.Shares) != len(m.Outcomes) {
		return ledger.Posting{}, errs.New(errs.CodeInternal, "predmarket: a settlement line does not name every outcome")
	}
	if line.Amount.IsNegative() {
		return ledger.Posting{}, errs.New(errs.CodeInternal, "predmarket: a settlement line cannot pay a negative amount")
	}
	f := m.Facility()
	cash := m.CollateralAssetID
	var l legs
	for i, o := range m.Outcomes {
		h := line.Shares[i]
		if h.IsNegative() {
			return ledger.Posting{}, errs.New(errs.CodeInternal, "predmarket: a settlement line retires a negative holding")
		}
		l.cr(ledger.CustomerAccount(line.AccountID, ledger.CodeWallet, o.ShareAssetID), h)
		l.dr(ledger.CustomerAccount(line.AccountID, ledger.CodeTradingOutflow, o.ShareAssetID), h)
		l.dr(f.Account(ledger.CodePredictionSharesOutstanding, o.ShareAssetID), h)
		l.cr(f.Account(ledger.CodePredictionSharesIssued, o.ShareAssetID), h)
	}
	l.dr(ledger.CustomerAccount(line.AccountID, ledger.CodeWallet, cash), line.Amount)
	l.cr(ledger.CustomerAccount(line.AccountID, ledger.CodeTradingInflow, cash), line.Amount)
	l.dr(f.Account(ledger.CodePredictionCollateral, cash), line.Amount)
	l.cr(f.Account(ledger.CodePredictionEscrow, cash), line.Amount)
	return finish(jk, meta, l)
}
