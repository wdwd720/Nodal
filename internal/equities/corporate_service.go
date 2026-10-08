package equities

import (
	"context"
	"crypto/sha256"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// CorporateActionSpec announces a corporate action on an equity or ETF.
type CorporateActionSpec struct {
	InstrumentID instruments.InstrumentID
	Type         CorporateActionType
	// EffectiveDate is the first trading day of the change (split, reverse
	// split, symbol change) or the day a delisting begins. A dividend's
	// effective date is its ex-date and may be left zero.
	EffectiveDate Date
	// Ratio is new shares per old share (splits only).
	Ratio *Ratio
	// CashPerShare is the dividend per whole share (dividends only).
	CashPerShare *PricePoint
	// CashInLieuPrice is the price per whole new share at which a fractional
	// remainder is paid out; required when the ratio can leave one.
	CashInLieuPrice *PricePoint
	ExDate          Date
	RecordDate      Date
	PayDate         Date
	// FinalDate is when a delisting ends trading altogether.
	FinalDate Date
	NewSymbol string
	// Rounding defaults to RoundingSharesDownCashDown.
	Rounding CorporateRounding
	Reason   string
	Actor    econ.Actor
}

type canonicalAction struct {
	InstrumentID    string      `json:"instrument_id"`
	Type            string      `json:"type"`
	EffectiveDate   string      `json:"effective_date"`
	Ratio           *Ratio      `json:"ratio"`
	CashPerShare    *PricePoint `json:"cash_per_share"`
	CashInLieuPrice *PricePoint `json:"cash_in_lieu_price"`
	ExDate          string      `json:"ex_date"`
	RecordDate      string      `json:"record_date"`
	PayDate         string      `json:"pay_date"`
	FinalDate       string      `json:"final_date"`
	NewSymbol       string      `json:"new_symbol"`
	Rounding        string      `json:"rounding"`
	Reason          string      `json:"reason"`
}

func (c CorporateActionSpec) contentHash() ([]byte, error) {
	b, err := audit.CanonicalJSON(canonicalAction{
		InstrumentID: c.InstrumentID.String(), Type: string(c.Type), EffectiveDate: c.EffectiveDate.String(), Ratio: c.Ratio,
		CashPerShare: c.CashPerShare, CashInLieuPrice: c.CashInLieuPrice, ExDate: c.ExDate.String(),
		RecordDate: c.RecordDate.String(), PayDate: c.PayDate.String(), FinalDate: c.FinalDate.String(),
		NewSymbol: c.NewSymbol, Rounding: string(c.Rounding), Reason: c.Reason,
	})
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "equities: canonicalise corporate action")
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// validate checks the spec against its type and the calendar. Each type
// carries exactly the fields it needs.
func (c *CorporateActionSpec) validate(cal *Calendar, currentSymbol string) error {
	var p []string
	if !c.Type.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "equities: unknown corporate action type %q", c.Type)
	}
	if c.Rounding == "" {
		c.Rounding = RoundingSharesDownCashDown
	}
	if _, _, err := c.Rounding.Modes(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Reason) == "" {
		p = append(p, "a corporate action states its reason")
	}
	split := c.Type == ActionSplit || c.Type == ActionReverseSplit
	if split != (c.Ratio != nil) {
		p = append(p, "a split or reverse split, and only one, has a ratio")
	}
	if (c.Type == ActionCashDividend) != (c.CashPerShare != nil) {
		p = append(p, "a dividend, and only one, has an amount per share")
	}
	if c.CashInLieuPrice != nil && !split {
		p = append(p, "only a split pays for fractional shares")
	}
	if (c.Type == ActionSymbolChange) != (c.NewSymbol != "") {
		p = append(p, "a symbol change, and only one, has a new symbol")
	}
	if (c.Type == ActionDelisting) != !c.FinalDate.IsZero() {
		p = append(p, "a delisting, and only one, has a final date")
	}
	if c.Type != ActionCashDividend && (!c.ExDate.IsZero() || !c.RecordDate.IsZero() || !c.PayDate.IsZero()) {
		p = append(p, "only a dividend has ex, record and pay dates")
	}
	switch c.Type {
	case ActionSplit, ActionReverseSplit:
		if c.Ratio != nil {
			if err := c.Ratio.validateFor(c.Type); err != nil {
				return err
			}
			if c.Ratio.Denominator > 1 && c.CashInLieuPrice == nil {
				p = append(p, "this ratio can leave fractional shares, so it needs a fractional-share price")
			}
		}
		if c.CashInLieuPrice != nil {
			if err := c.CashInLieuPrice.validate("a fractional-share price"); err != nil {
				return err
			}
		}
		if !cal.IsTradingDay(c.EffectiveDate) {
			p = append(p, "a split takes effect on a trading day")
		}
	case ActionCashDividend:
		if c.CashPerShare != nil {
			if err := c.CashPerShare.validate("a dividend per share"); err != nil {
				return err
			}
		}
		if c.ExDate.IsZero() || c.RecordDate.IsZero() || c.PayDate.IsZero() {
			return errs.New(errs.CodeValidationFailed, "equities: a dividend has ex, record and pay dates")
		}
		if err := cal.ValidateExDate(c.ExDate, c.RecordDate); err != nil {
			return err
		}
		if c.PayDate.Before(c.RecordDate) || !cal.IsTradingDay(c.PayDate) {
			p = append(p, "a dividend is paid on a trading day on or after its record date")
		}
		if !c.EffectiveDate.IsZero() && !c.EffectiveDate.Equal(c.ExDate) {
			p = append(p, "a dividend's effective date is its ex-date")
		}
		c.EffectiveDate = c.ExDate
	case ActionSymbolChange:
		if !symbolValid(c.NewSymbol) {
			p = append(p, "the new symbol is not a valid symbol")
		}
		if c.NewSymbol == currentSymbol {
			p = append(p, "the new symbol is the current one")
		}
		if !cal.IsTradingDay(c.EffectiveDate) {
			p = append(p, "a symbol change takes effect on a trading day")
		}
	case ActionDelisting:
		if c.EffectiveDate.IsZero() || c.FinalDate.Before(c.EffectiveDate) {
			p = append(p, "a delisting ends on or after it begins")
		}
	}
	if c.EffectiveDate.IsZero() {
		p = append(p, "an effective date is required")
	}
	if len(p) > 0 {
		return errs.New(errs.CodeValidationFailed, "equities: invalid corporate action").WithField("problems", p)
	}
	return nil
}

func symbolValid(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || (i > 0 && strings.ContainsRune("._/-", rune(c)))
		if !ok {
			return false
		}
	}
	return true
}

// AnnounceCorporateAction records an ANNOUNCED corporate action. Announcing
// the same action twice is a replay; a different action for the same
// (instrument, type, effective date) is a CONFLICT while the first is live. A
// cancelled action frees its date: announcing after a cancellation records a
// new action.
func (s *Service) AnnounceCorporateAction(ctx context.Context, tx pgx.Tx, spec CorporateActionSpec) (CorporateAction, error) {
	if err := validActor(spec.Actor); err != nil {
		return CorporateAction{}, err
	}
	l, err := s.LoadListing(ctx, tx, spec.InstrumentID)
	if err != nil {
		return CorporateAction{}, err
	}
	cal, err := s.Calendar(ctx, tx, l.Terms.Terms.TradingCalendar)
	if err != nil {
		return CorporateAction{}, err
	}
	if err := spec.validate(cal, l.Terms.Terms.Symbol); err != nil {
		return CorporateAction{}, err
	}
	hash, err := spec.contentHash()
	if err != nil {
		return CorporateAction{}, err
	}
	a, inserted, err := insertAction(ctx, tx, CorporateAction{
		ID: id.New[actionKind](), InstrumentID: spec.InstrumentID, Type: spec.Type, EffectiveDate: spec.EffectiveDate,
		ShareAssetID: l.ShareAsset.ID, CashAssetID: l.CashAsset.ID, Ratio: spec.Ratio, CashPerShare: spec.CashPerShare,
		CashInLieuPrice: spec.CashInLieuPrice, ExDate: spec.ExDate, RecordDate: spec.RecordDate, PayDate: spec.PayDate,
		FinalDate: spec.FinalDate, NewSymbol: spec.NewSymbol, Rounding: spec.Rounding, Reason: spec.Reason, ContentHash: hash,
		AnnouncedBy: spec.Actor, CreatedAt: s.clk.Now().UTC(),
	})
	if err != nil {
		return CorporateAction{}, err
	}
	if !inserted {
		if string(a.ContentHash) != string(hash) {
			return CorporateAction{}, errs.New(errs.CodeConflict,
				"equities: a different corporate action of this type is already announced for this instrument and date").
				WithField("action_id", a.ID.String())
		}
		return a, nil
	}
	return a, s.record(ctx, tx, audit.SystemStream, "equities.corporate_action.announced", "equity_corporate_action",
		a.ID.String(), spec.Reason, map[string]string{"instrument_id": a.InstrumentID.String(), "type": string(a.Type),
			"effective_date": a.EffectiveDate.String()})
}

// CancelCorporateAction withdraws an announced action that has not been
// applied.
func (s *Service) CancelCorporateAction(ctx context.Context, tx pgx.Tx, aid CorporateActionID, actor econ.Actor, reason string) (CorporateAction, error) {
	if err := validActor(actor); err != nil {
		return CorporateAction{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return CorporateAction{}, errs.New(errs.CodeValidationFailed, "equities: a cancellation states its reason")
	}
	a, err := getAction(ctx, tx, aid, true)
	if err != nil {
		return CorporateAction{}, err
	}
	if a.Status == ActionCancelled {
		return a, nil
	}
	out, err := transitionAction(ctx, tx, a, ActionCancelled, actor, reason, s.clk.Now())
	if err != nil {
		return CorporateAction{}, err
	}
	return out, s.record(ctx, tx, audit.SystemStream, "equities.corporate_action.cancelled", "equity_corporate_action",
		a.ID.String(), reason, map[string]string{"type": string(a.Type)})
}

// CorporateActionResult is what one application call did.
type CorporateActionResult struct {
	Action       CorporateAction
	Applications []Application
	// Replayed is true when the action had been applied already and nothing
	// was posted.
	Replayed bool
	// TermsVersion is the instrument terms version a symbol change wrote.
	TermsVersion *instruments.Version
	// Instrument is the instrument after a delisting step.
	Instrument *instruments.Instrument
}

// ApplyCorporateAction applies an announced action on or after the date it
// takes effect, in one transaction, under the instrument's exclusive lock:
//
//   - SPLIT / REVERSE_SPLIT on its effective date, to every settled holding,
//     once no order on the instrument is live and nothing is unsettled or
//     reserved in its shares: CORPORATE_ACTION per holder, with cash-in-lieu
//     for any fractional remainder under the action's rounding policy;
//   - CASH_DIVIDEND on its pay date, once every split effective before its
//     ex-date is applied, to the holders of record (the trade-date position
//     at 00:00 New York time on the ex-date, in the ex-date's units): Dr
//     WALLET / Cr DIVIDENDS per holder;
//   - SYMBOL_CHANGE: a new terms version, no posting;
//   - DELISTING: the instrument becomes DELISTING on the effective date (buys
//     stop, holders may sell) and DELISTED on the final date (a second call).
//
// Applying an applied action is a replay that posts nothing and returns the
// recorded applications: one posting per account, ever.
func (s *Service) ApplyCorporateAction(ctx context.Context, tx pgx.Tx, aid CorporateActionID, actor econ.Actor) (CorporateActionResult, error) {
	if err := validActor(actor); err != nil {
		return CorporateActionResult{}, err
	}
	a, err := getAction(ctx, tx, aid, true)
	if err != nil {
		return CorporateActionResult{}, err
	}
	switch a.Status {
	case ActionApplied:
		apps, err := Applications(ctx, tx, aid)
		if err != nil {
			return CorporateActionResult{}, err
		}
		return CorporateActionResult{Action: a, Applications: apps, Replayed: true}, nil
	case ActionCancelled:
		return CorporateActionResult{}, errs.Newf(errs.CodeInvalidStateTransition, "equities: corporate action %s was cancelled", a.ID)
	}
	if err := lockInstrument(ctx, tx, a.InstrumentID, true); err != nil {
		return CorporateActionResult{}, err
	}
	l, err := s.LoadListing(ctx, tx, a.InstrumentID)
	if err != nil {
		return CorporateActionResult{}, err
	}
	cal, err := s.Calendar(ctx, tx, l.Terms.Terms.TradingCalendar)
	if err != nil {
		return CorporateActionResult{}, err
	}
	now := s.clk.Now().UTC()
	today := cal.TradeDate(now)
	switch a.Type {
	case ActionSplit, ActionReverseSplit:
		return s.applySplit(ctx, tx, a, l, today, actor, now)
	case ActionCashDividend:
		return s.applyDividend(ctx, tx, a, l, cal, today, actor, now)
	case ActionSymbolChange:
		return s.applySymbolChange(ctx, tx, a, l, today, actor, now)
	case ActionDelisting:
		return s.applyDelisting(ctx, tx, a, l, today, actor, now)
	}
	return CorporateActionResult{}, errs.New(errs.CodeValidationFailed, "equities: unknown corporate action type")
}

func notYet(a CorporateAction, when Date) error {
	return errs.Newf(errs.CodeValidationFailed, "equities: corporate action %s cannot be applied before %s", a.ID, when).
		WithField("action_id", a.ID.String()).WithField("not_before", when.String())
}

func (s *Service) requireNoLiveOrders(ctx context.Context, tx pgx.Tx, a CorporateAction, createdBefore *time.Time) error {
	var n int
	var err error
	if createdBefore == nil {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM equity_orders WHERE instrument_id = $1 AND status IN ('ACCEPTED','OPEN')`,
			a.InstrumentID).Scan(&n)
	} else {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM equity_orders WHERE instrument_id = $1 AND status IN ('ACCEPTED','OPEN')
			AND created_at < $2`, a.InstrumentID, *createdBefore).Scan(&n)
	}
	if err != nil {
		return storeErr("count live orders", err)
	}
	if n > 0 {
		return errs.Newf(errs.CodeConflict, "equities: %d orders on the instrument are still live; poll, expire or cancel them before %s",
			n, a.Type).WithField("action_id", a.ID.String())
	}
	return nil
}

func (s *Service) applySplit(ctx context.Context, tx pgx.Tx, a CorporateAction, l Listing, today Date, actor econ.Actor, now time.Time) (CorporateActionResult, error) {
	if today.Before(a.EffectiveDate) {
		return CorporateActionResult{}, notYet(a, a.EffectiveDate)
	}
	if err := s.requireNoLiveOrders(ctx, tx, a, nil); err != nil {
		return CorporateActionResult{}, err
	}
	var unsettled, reserved int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts la JOIN ledger_balances lb ON lb.ledger_account_id = la.id
		WHERE la.owner_type = 'CUSTOMER' AND la.code = 'UNSETTLED_RECEIVABLE' AND la.asset_id = $1 AND lb.balance <> 0`,
		l.ShareAsset.ID).Scan(&unsettled); err != nil {
		return CorporateActionResult{}, storeErr("count unsettled shares", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM asset_reservations WHERE asset_id = $1 AND status = 'ACTIVE'`,
		l.ShareAsset.ID).Scan(&reserved); err != nil {
		return CorporateActionResult{}, storeErr("count share reservations", err)
	}
	if unsettled > 0 || reserved > 0 {
		return CorporateActionResult{}, errs.Newf(errs.CodeConflict,
			"equities: %d unsettled and %d reserved holdings of the shares; settle them before the %s", unsettled, reserved, a.Type).
			WithField("action_id", a.ID.String())
	}
	holders, err := s.holdings(ctx, tx, `SELECT la.owner_id::text, lb.balance::text
		FROM ledger_accounts la JOIN ledger_balances lb ON lb.ledger_account_id = la.id
		WHERE la.owner_type = 'CUSTOMER' AND la.code = 'WALLET' AND la.asset_id = $1 AND lb.balance > 0
		ORDER BY la.owner_id`, l.ShareAsset.ID)
	if err != nil {
		return CorporateActionResult{}, err
	}
	res := CorporateActionResult{}
	for _, h := range holders {
		o, err := ComputeSplit(h.qty, *a.Ratio, a.Type, l.ShareAsset.Decimals, l.CashAsset.Decimals, a.CashInLieuPrice, a.Rounding)
		if err != nil {
			return CorporateActionResult{}, err
		}
		app, err := s.applyToAccount(ctx, tx, a, h.account, h.qty, o.SharesDelta(), o.CashInLieu, o.CashExactNum, o.CashExactDen,
			SplitEntries(h.account, l.ShareAsset.ID, l.CashAsset.ID, o), o, now)
		if err != nil {
			return CorporateActionResult{}, err
		}
		res.Applications = append(res.Applications, app)
	}
	if res.Action, err = transitionAction(ctx, tx, a, ActionApplied, actor, string(a.Type)+" applied", now); err != nil {
		return CorporateActionResult{}, err
	}
	return res, s.recordAction(ctx, tx, res.Action, len(res.Applications))
}

func (s *Service) applyDividend(ctx context.Context, tx pgx.Tx, a CorporateAction, l Listing, cal *Calendar, today Date, actor econ.Actor, now time.Time) (CorporateActionResult, error) {
	if today.Before(a.PayDate) {
		return CorporateActionResult{}, notYet(a, a.PayDate)
	}
	cutoff := cal.StartOfDay(a.ExDate)
	// An order created before the cutoff could still produce a fill that
	// predates it and changes who held the shares.
	if err := s.requireNoLiveOrders(ctx, tx, a, &cutoff); err != nil {
		return CorporateActionResult{}, err
	}
	// A split effective before the ex-date changes the units the holders of
	// record hold; until it is applied the ledger's history does not show it.
	var pendingSplits int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM equity_corporate_actions WHERE instrument_id = $1
		AND action_type IN ('SPLIT','REVERSE_SPLIT') AND status = 'ANNOUNCED' AND effective_date < $2`,
		a.InstrumentID, a.ExDate.Time()).Scan(&pendingSplits); err != nil {
		return CorporateActionResult{}, storeErr("count pending splits", err)
	}
	if pendingSplits > 0 {
		return CorporateActionResult{}, errs.Newf(errs.CodeConflict,
			"equities: %d splits effective before the ex-date are not applied; apply them before paying the dividend", pendingSplits).
			WithField("action_id", a.ID.String())
	}
	// Holders of record: the trade-date position (WALLET plus shares bought
	// but unsettled) before 00:00 New York time on the ex-date, from the
	// ledger's own history, in the units of the ex-date: a split effective
	// before the ex-date counts even when it was applied after the cutoff
	// (nothing traded in between, so it adjusts exactly that position), and
	// one effective on or after the ex-date never does.
	holders, err := s.holdings(ctx, tx, `SELECT la.owner_id::text,
		       sum(CASE WHEN e.side = 'DEBIT' THEN e.quantity ELSE -e.quantity END)::text
		  FROM journal_entries e
		  JOIN ledger_accounts la ON la.id = e.ledger_account_id
		  JOIN journal_transactions t ON t.id = e.transaction_id
		 WHERE la.owner_type = 'CUSTOMER' AND la.asset_id = $1 AND la.code IN ('WALLET','UNSETTLED_RECEIVABLE')
		   AND (t.effective_at < $2
		        OR (t.kind = 'CORPORATE_ACTION' AND EXISTS (
		              SELECT 1 FROM corporate_action_applications app
		                JOIN equity_corporate_actions ca ON ca.id = app.action_id
		               WHERE app.journal_transaction_id = t.id AND ca.instrument_id = $3
		                 AND ca.action_type IN ('SPLIT','REVERSE_SPLIT') AND ca.effective_date < $4)))
		 GROUP BY la.owner_id
		HAVING sum(CASE WHEN e.side = 'DEBIT' THEN e.quantity ELSE -e.quantity END) > 0
		 ORDER BY la.owner_id`, l.ShareAsset.ID, cutoff, a.InstrumentID, a.ExDate.Time())
	if err != nil {
		return CorporateActionResult{}, err
	}
	res := CorporateActionResult{}
	for _, h := range holders {
		d, err := ComputeDividend(h.qty, l.ShareAsset.Decimals, l.CashAsset.Decimals, *a.CashPerShare, a.Rounding)
		if err != nil {
			return CorporateActionResult{}, err
		}
		app, err := s.applyToAccount(ctx, tx, a, h.account, h.qty, money.QuantityFromInt64(0), d.Amount, d.ExactNum, d.ExactDen,
			DividendEntries(h.account, l.CashAsset.ID, d.Amount), d, now)
		if err != nil {
			return CorporateActionResult{}, err
		}
		res.Applications = append(res.Applications, app)
	}
	if res.Action, err = transitionAction(ctx, tx, a, ActionApplied, actor, "dividend paid", now); err != nil {
		return CorporateActionResult{}, err
	}
	return res, s.recordAction(ctx, tx, res.Action, len(res.Applications))
}

func (s *Service) applySymbolChange(ctx context.Context, tx pgx.Tx, a CorporateAction, l Listing, today Date, actor econ.Actor, now time.Time) (CorporateActionResult, error) {
	if today.Before(a.EffectiveDate) {
		return CorporateActionResult{}, notYet(a, a.EffectiveDate)
	}
	if err := s.requireNoLiveOrders(ctx, tx, a, nil); err != nil {
		return CorporateActionResult{}, err
	}
	next := l.Terms.Terms
	old := next.Symbol
	next.Symbol = a.NewSymbol
	v, err := s.instruments.AmendTerms(ctx, tx, a.InstrumentID, next,
		"symbol change "+old+" -> "+a.NewSymbol+" (corporate action "+a.ID.String()+")", string(actor.Type), actor.ID)
	if err != nil {
		return CorporateActionResult{}, err
	}
	res := CorporateActionResult{TermsVersion: &v}
	if res.Action, err = transitionAction(ctx, tx, a, ActionApplied, actor, "symbol changed", now); err != nil {
		return CorporateActionResult{}, err
	}
	return res, s.recordAction(ctx, tx, res.Action, 0)
}

func (s *Service) applyDelisting(ctx context.Context, tx pgx.Tx, a CorporateAction, l Listing, today Date, actor econ.Actor, now time.Time) (CorporateActionResult, error) {
	to, next, when := assets.StatusDelisting, ActionApplying, a.EffectiveDate
	if a.Status == ActionApplying {
		to, next, when = assets.StatusDelisted, ActionApplied, a.FinalDate
		if !today.Before(when) {
			if err := s.requireNoLiveOrders(ctx, tx, a, nil); err != nil {
				return CorporateActionResult{}, err
			}
		}
	}
	if today.Before(when) {
		return CorporateActionResult{}, notYet(a, when)
	}
	// An operator may have moved the instrument already; the action then
	// records the step without moving it again (the same status is not a
	// transition the instrument has).
	inst, err := l.Instrument, error(nil)
	if inst.Status != to {
		if inst, err = s.instruments.TransitionStatus(ctx, tx, a.InstrumentID, instruments.StatusChange{
			To: to, ActorType: string(actor.Type), ActorID: actor.ID, Reason: "delisting (corporate action " + a.ID.String() + ")",
		}); err != nil {
			return CorporateActionResult{}, err
		}
	}
	res := CorporateActionResult{Instrument: &inst}
	if res.Action, err = transitionAction(ctx, tx, a, next, actor, "instrument "+string(to), now); err != nil {
		return CorporateActionResult{}, err
	}
	return res, s.recordAction(ctx, tx, res.Action, 0)
}

type holding struct {
	account accounts.AccountID
	qty     money.Quantity
}

func (s *Service) holdings(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]holding, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, storeErr("read holdings", err)
	}
	defer rows.Close()
	var out []holding
	for rows.Next() {
		var acct, qty string
		if err := rows.Scan(&acct, &qty); err != nil {
			return nil, storeErr("scan holding", err)
		}
		a, err := accounts.ParseAccountID(acct)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "equities: a holder is not an account")
		}
		q, err := money.ScanQuantity(qty)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "equities: a holding is not a whole number of base units")
		}
		out = append(out, holding{account: a, qty: q})
	}
	return out, rows.Err()
}

type applicationRequest struct {
	ActionID     string         `json:"action_id"`
	AccountID    string         `json:"account_id"`
	Entitled     money.Quantity `json:"entitled_quantity"`
	SharesDelta  money.Quantity `json:"shares_delta"`
	CashAmount   money.Quantity `json:"cash_amount"`
	RoundingRule string         `json:"rounding_policy"`
}

// applyToAccount applies one account's share of an action exactly once: an
// existing application is returned as it is, and a new one that moves value
// is an econ effect posting CORPORATE_ACTION.
func (s *Service) applyToAccount(ctx context.Context, tx pgx.Tx, a CorporateAction, acct accounts.AccountID, entitled,
	sharesDelta, cash, exactNum, exactDen money.Quantity, entries []ledger.Entry, detail any, now time.Time) (Application, error) {
	if existing, ok, err := applicationFor(ctx, tx, a.ID, acct); err != nil || ok {
		return existing, err
	}
	app := Application{
		ID: id.New[applicationKind](), ActionID: a.ID, AccountID: acct, EntitledQuantity: entitled, SharesDelta: sharesDelta,
		CashAmount: cash, ExactCashNumerator: exactNum, ExactCashDenominator: exactDen, Rounding: a.Rounding, CreatedAt: now,
	}
	if len(entries) > 0 {
		key := "equities:ca:" + a.ID.String() + ":" + acct.String()
		effect, replay, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
			Key: key, Subsystem: econ.SubsystemEquities, Kind: "CORPORATE_ACTION", AccountID: acct.String(), PostsLedger: true,
			Actor: s.actor, Request: applicationRequest{ActionID: a.ID.String(), AccountID: acct.String(), Entitled: entitled,
				SharesDelta: sharesDelta, CashAmount: cash, RoundingRule: string(a.Rounding)},
		})
		if err != nil {
			return Application{}, err
		}
		if replay {
			return Application{}, errs.Newf(errs.CodeReconciliationRequired,
				"equities: corporate action %s has an effect for account %s (%s) but no application; reconcile", a.ID, acct, effect.Status)
		}
		posted, err := s.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindCorporateAction, IdempotencyKey: key,
			Reference:   ledger.FinancialEventReference{Type: "equity_corporate_action", ID: app.ID.String()},
			EffectiveAt: now, Description: strings.ToLower(string(a.Type)) + " " + a.EffectiveDate.String(), Entries: entries,
			Metadata: map[string]any{"action_id": a.ID.String(), "action_type": string(a.Type), "rounding_policy": string(a.Rounding)},
		})
		if err != nil {
			return Application{}, err
		}
		ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "equities/corporate-action/" + a.ID.String() + "/" + acct.String(),
			Document: map[string]any{"kind": "equities.corporate_action.v1", "action_id": a.ID.String(), "type": string(a.Type),
				"account_id": acct.String(), "entitled_quantity": entitled.String(), "shares_delta": sharesDelta.String(),
				"cash_amount": cash.String(), "exact_cash_numerator": exactNum.String(), "exact_cash_denominator": exactDen.String(),
				"rounding_policy": string(a.Rounding), "detail": detail, "journal_transaction_id": posted.TransactionID.String()}})
		if err != nil {
			return Application{}, err
		}
		if _, err := s.effects.Apply(ctx, tx, effect.ID, econ.ApplyRequest{JournalTransactionID: &posted.TransactionID,
			Evidence: &ev, Actor: s.actor, Reason: string(a.Type) + " applied"}); err != nil {
			return Application{}, err
		}
		app.JournalTransactionID, app.EffectID = &posted.TransactionID, &effect.ID
	}
	out, err := insertApplication(ctx, tx, app)
	if err != nil {
		return Application{}, err
	}
	return out, s.record(ctx, tx, audit.AccountStream(acct.String()), "equities.corporate_action.applied",
		"corporate_action_application", out.ID.String(), string(a.Type), map[string]string{
			"action_id": a.ID.String(), "shares_delta": sharesDelta.String(), "cash_amount": cash.String()})
}

func (s *Service) recordAction(ctx context.Context, tx pgx.Tx, a CorporateAction, applications int) error {
	return s.record(ctx, tx, audit.SystemStream, "equities.corporate_action."+strings.ToLower(string(a.Status)),
		"equity_corporate_action", a.ID.String(), string(a.Type), map[string]any{
			"instrument_id": a.InstrumentID.String(), "type": string(a.Type), "applications": applications})
}

// CorporateAction returns one action.
func (s *Service) CorporateAction(ctx context.Context, q db.Querier, aid CorporateActionID) (CorporateAction, error) {
	return getAction(ctx, q, aid, false)
}
