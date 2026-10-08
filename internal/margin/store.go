package margin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// Account is a margin_accounts row.
type Account struct {
	ID               AccountID
	AccountID        accounts.AccountID
	MarginMode       MarginMode
	Mode             gates.Mode
	Status           AccountStatus
	PolicyVersion    string
	NumeraireAssetID assets.AssetID
	IsolatedAssetID  *assets.AssetID
	LendingPoolID    econ.FacilityID
	InsuranceFundID  econ.FacilityID
	OpenedBy         econ.Actor
	OpenedAt         time.Time
}

// Lot is a margin_borrow_lots row.
type Lot struct {
	ID                   LotID
	MarginAccountID      AccountID
	AssetID              assets.AssetID
	Principal            money.Quantity
	OutstandingPrincipal money.Quantity
	OutstandingInterest  money.Quantity
	RateBPS              money.BPS
	OpenedAt             time.Time
	AccruedThrough       time.Time
	ClosedAt             *time.Time
	CloseReason          string
	BorrowJournalID      ledger.TransactionID
	EffectID             econ.EffectID
}

// Outstanding is principal plus capitalized interest still owed.
func (l Lot) Outstanding() money.Quantity { return l.OutstandingPrincipal.Add(l.OutstandingInterest) }

// Call is a margin_calls row.
type Call struct {
	ID               CallID
	MarginAccountID  AccountID
	Status           CallStatus
	IssuedAt         time.Time
	EquityUnits      money.Quantity
	MaintenanceUnits money.Quantity
	DeficiencyUnits  money.Quantity
	EvaluationHash   string
	ResolvedAt       *time.Time
}

// Case is a margin_liquidation_cases row.
type Case struct {
	ID              CaseID
	MarginAccountID AccountID
	MarginCallID    CallID
	Status          CaseStatus
	PolicyVersion   string
	Target          LiquidationTarget
	MaxSteps        int
	StepsExecuted   int
	// BudgetFromStep is where the current budget window began: the trading
	// steps after it count against MaxSteps. A resumed case starts a fresh
	// window at the steps it had already taken.
	BudgetFromStep    int
	OpenedAt          time.Time
	ClosedAt          *time.Time
	LastReason        string
	DeficitUnits      *money.Quantity
	CoverageJournalID *ledger.TransactionID
	CoveredAt         *time.Time
}

// Step is a margin_liquidation_steps row.
type Step struct {
	ID              StepID
	CaseID          CaseID
	StepNo          int
	Action          StepAction
	AssetID         assets.AssetID
	Quantity        money.Quantity
	Mark            *money.Price
	ExecutionPrice  *money.Price
	SlippageBPS     money.BPS
	SlippageCapBPS  money.BPS
	Proceeds        money.Quantity
	Fee             money.Quantity
	Repaid          money.Quantity
	Excess          money.Quantity
	TradeJournalID  *ledger.TransactionID
	ReleaseJournal  *ledger.TransactionID
	RepayJournalID  *ledger.TransactionID
	ExcessJournalID *ledger.TransactionID
	EquityBefore    money.Quantity
	EquityAfter     money.Quantity
	PlanHash        string
	ExecutedAt      time.Time
}

// store is the SQL of the margin tables. It holds no state; every method
// runs on the caller's transaction or querier.
type store struct {
	audit audit.Writer
}

func dbErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if db.IsRetryable(err) {
		return err
	}
	if ee, ok := errs.As(err); ok {
		return ee
	}
	if mapped := ledger.MapError(err); mapped != nil {
		if ee, ok := errs.As(mapped); ok && ee.Code != errs.CodeInternal {
			return ee
		}
	}
	switch db.SQLState(err) {
	case "MG001", "MG002", "MG003", "MG004", "MG005", "MG007", "MG010", "MG011":
		return errs.Wrap(err, errs.CodeInvalidStateTransition, "margin: "+op)
	case "MG006":
		return errs.Wrap(err, errs.CodeValidationFailed, "margin: "+op)
	case "MG009":
		return errs.Wrap(err, errs.CodeForbidden, "margin: "+op)
	case "MG008":
		// An effect naming a journal its key did not post is a defect, never
		// a request to refuse politely.
		return errs.Wrap(err, errs.CodeInternal, "margin: "+op)
	}
	if db.IsUniqueViolation(err) {
		return errs.Wrap(err, errs.CodeConflict, "margin: "+op)
	}
	return errs.Wrap(err, errs.CodeInternal, "margin: "+op)
}

// ensurePolicy registers a policy version. A version is a name for one
// document: registering the same version with different content is refused.
func (st *store) ensurePolicy(ctx context.Context, tx pgx.Tx, p Policy) error {
	doc, err := p.CanonicalJSON()
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "margin: encode policy")
	}
	hash := p.Hash()
	if _, err := tx.Exec(ctx, `INSERT INTO margin_policies (version, policy_hash, document) VALUES ($1,$2,$3)
		ON CONFLICT (version) DO NOTHING`, p.Version, hash, doc); err != nil {
		return dbErr("register policy", err)
	}
	var stored string
	if err := tx.QueryRow(ctx, `SELECT policy_hash FROM margin_policies WHERE version = $1`, p.Version).Scan(&stored); err != nil {
		return dbErr("read policy", err)
	}
	if stored != hash {
		return errs.New(errs.CodeConflict, "margin: this policy version is already registered with different content").
			WithField("policy_version", p.Version)
	}
	return nil
}

const accountColumns = `id, account_id, margin_mode, mode, status, policy_version, numeraire_asset_id, isolated_asset_id,
	lending_pool_facility_id, insurance_fund_facility_id, opened_by_actor_type, opened_by_actor_id, opened_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	var iso *assets.AssetID
	if err := row.Scan(&a.ID, &a.AccountID, &a.MarginMode, &a.Mode, &a.Status, &a.PolicyVersion, &a.NumeraireAssetID, &iso,
		&a.LendingPoolID, &a.InsuranceFundID, &a.OpenedBy.Type, &a.OpenedBy.ID, &a.OpenedAt); err != nil {
		return Account{}, err
	}
	a.IsolatedAssetID = iso
	a.OpenedAt = a.OpenedAt.UTC()
	return a, nil
}

func (st *store) insertAccount(ctx context.Context, tx pgx.Tx, a Account) (Account, bool, error) {
	row := tx.QueryRow(ctx, `INSERT INTO margin_accounts (id, account_id, margin_mode, mode, status, policy_version,
		numeraire_asset_id, isolated_asset_id, lending_pool_facility_id, insurance_fund_facility_id,
		opened_by_actor_type, opened_by_actor_id, opened_at)
		VALUES ($1,$2,$3,$4,'NORMAL',$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (account_id) DO NOTHING RETURNING `+accountColumns,
		a.ID, a.AccountID, a.MarginMode, a.Mode, a.PolicyVersion, a.NumeraireAssetID, a.IsolatedAssetID,
		a.LendingPoolID, a.InsuranceFundID, a.OpenedBy.Type, a.OpenedBy.ID, a.OpenedAt)
	created, err := scanAccount(row)
	if err == nil {
		return created, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Account{}, false, dbErr("open margin account", err)
	}
	existing, err := st.accountByCustomer(ctx, tx, a.AccountID, false)
	return existing, false, err
}

func (st *store) accountByCustomer(ctx context.Context, q db.Querier, acct accounts.AccountID, lock bool) (Account, error) {
	sql := `SELECT ` + accountColumns + ` FROM margin_accounts WHERE account_id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	a, err := scanAccount(q.QueryRow(ctx, sql, acct))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, errs.New(errs.CodeNotFound, "margin account not found").WithField("account_id", acct.String())
	}
	if err != nil {
		return Account{}, dbErr("load margin account", err)
	}
	return a, nil
}

func (st *store) accountByID(ctx context.Context, q db.Querier, mid AccountID, lock bool) (Account, error) {
	sql := `SELECT ` + accountColumns + ` FROM margin_accounts WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	a, err := scanAccount(q.QueryRow(ctx, sql, mid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, errs.New(errs.CodeNotFound, "margin account not found").WithField("margin_account_id", mid.String())
	}
	if err != nil {
		return Account{}, dbErr("load margin account", err)
	}
	return a, nil
}

// bindingsImmediate makes the caller's transaction check every status
// binding this package's operations touch immediately -- at the end of each
// status UPDATE, against the transition row written just before it --
// instead of at COMMIT. The bindings stay exactly as strict: a status change
// that no transition row describes is still refused (AU001), only sooner.
//
// It exists because a deferred edge binding keeps one edge per row and
// transaction, so a lifecycle of several steps in one transaction (an account
// MARGIN_CALL -> LIQUIDATING -> NORMAL) would fail at COMMIT. The economic
// intent and effect bindings no longer need this: their status machines are
// acyclic, so migration 00733 lets their edges accumulate. These margin
// machines are NOT acyclic (an account can return to NORMAL and be called
// again), so an accumulated edge could license the same change twice; checking
// each change at the end of its own statement, against the row written just
// before it, keeps every change licensed by exactly one row. Every transition
// here writes its row before its UPDATE.
func bindingsImmediate(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET CONSTRAINTS margin_accounts_require_transition, margin_calls_require_transition,
		margin_liquidation_cases_require_transition IMMEDIATE`)
	return dbErr("check transition bindings immediately", err)
}

type transitionEvent struct {
	Resource       string `json:"resource"`
	ResourceID     string `json:"resource_id"`
	From           string `json:"from_status"`
	To             string `json:"to_status"`
	Reason         string `json:"reason"`
	EvaluationHash string `json:"evaluation_hash,omitempty"`
}

func (st *store) auditTransition(ctx context.Context, tx pgx.Tx, acct accounts.AccountID, actor econ.Actor, resource, rid, from, to, reason, evalHash string, at time.Time) error {
	body, err := json.Marshal(transitionEvent{Resource: resource, ResourceID: rid, From: from, To: to, Reason: reason, EvaluationHash: evalHash})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "margin: encode transition event")
	}
	_, err = st.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AccountStream(acct.String()), ActorType: string(actor.Type), ActorID: actor.ID,
		Action: "margin." + resource + "." + strings.ToLower(to), ResourceType: "margin_" + resource, ResourceID: rid,
		Reason: reason, Payload: body, OccurredAt: at,
	})
	return err
}

// setAccountStatus moves a margin account, writing its transition row first
// and its audit event after.
func (st *store) setAccountStatus(ctx context.Context, tx pgx.Tx, a *Account, to AccountStatus, actor econ.Actor, reason, evalHash string, at time.Time) error {
	if a.Status == to {
		return nil
	}
	if !CanTransitionAccount(a.Status, to) {
		return errs.Newf(errs.CodeInvalidStateTransition, "margin account %s cannot move %s -> %s", a.ID, a.Status, to).
			WithField("margin_account_id", a.ID.String())
	}
	if _, err := tx.Exec(ctx, `INSERT INTO margin_account_transitions
		(id, margin_account_id, from_status, to_status, reason, evaluation_hash, actor_type, actor_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9)`,
		id.New[id.Any](), a.ID, a.Status, to, reason, evalHash, actor.Type, actor.ID, at); err != nil {
		return dbErr("record margin account transition", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE margin_accounts SET status = $2 WHERE id = $1`, a.ID, to); err != nil {
		return dbErr("move margin account", err)
	}
	if err := st.auditTransition(ctx, tx, a.AccountID, actor, "account", a.ID.String(), string(a.Status), string(to), reason, evalHash, at); err != nil {
		return err
	}
	a.Status = to
	return nil
}

const lotColumns = `id, margin_account_id, asset_id, principal::text, outstanding_principal::text, outstanding_interest::text,
	rate_bps, opened_at, accrued_through, closed_at, coalesce(close_reason,''), borrow_journal_transaction_id, effect_id`

func scanLot(row pgx.Row) (Lot, error) {
	var l Lot
	var p, op, oi string
	var rate int64
	if err := row.Scan(&l.ID, &l.MarginAccountID, &l.AssetID, &p, &op, &oi, &rate, &l.OpenedAt, &l.AccruedThrough,
		&l.ClosedAt, &l.CloseReason, &l.BorrowJournalID, &l.EffectID); err != nil {
		return Lot{}, err
	}
	var err error
	if l.Principal, err = money.ScanQuantity(p); err != nil {
		return Lot{}, err
	}
	if l.OutstandingPrincipal, err = money.ScanQuantity(op); err != nil {
		return Lot{}, err
	}
	if l.OutstandingInterest, err = money.ScanQuantity(oi); err != nil {
		return Lot{}, err
	}
	l.RateBPS = money.BPS(rate)
	l.OpenedAt, l.AccruedThrough = l.OpenedAt.UTC(), l.AccruedThrough.UTC()
	return l, nil
}

// openLots returns the open lots of an account, oldest first: the order a
// repayment is applied in.
func (st *store) openLots(ctx context.Context, q db.Querier, mid AccountID, lock bool) ([]Lot, error) {
	sql := `SELECT ` + lotColumns + ` FROM margin_borrow_lots WHERE margin_account_id = $1 AND closed_at IS NULL ORDER BY opened_at, id`
	if lock {
		sql += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, sql, mid)
	if err != nil {
		return nil, dbErr("list borrow lots", err)
	}
	defer rows.Close()
	var out []Lot
	for rows.Next() {
		l, err := scanLot(rows)
		if err != nil {
			return nil, dbErr("scan borrow lot", err)
		}
		out = append(out, l)
	}
	return out, dbErr("list borrow lots", rows.Err())
}

func (st *store) lotByEffect(ctx context.Context, q db.Querier, eid econ.EffectID) (Lot, error) {
	l, err := scanLot(q.QueryRow(ctx, `SELECT `+lotColumns+` FROM margin_borrow_lots WHERE effect_id = $1`, eid))
	if err != nil {
		return Lot{}, dbErr("load borrow lot", err)
	}
	return l, nil
}

func (st *store) insertLot(ctx context.Context, tx pgx.Tx, l Lot) error {
	_, err := tx.Exec(ctx, `INSERT INTO margin_borrow_lots (id, margin_account_id, asset_id, principal, outstanding_principal,
		outstanding_interest, rate_bps, opened_at, accrued_through, borrow_journal_transaction_id, effect_id)
		VALUES ($1,$2,$3,$4::numeric,$4::numeric,0,$5,$6,$6,$7,$8)`,
		l.ID, l.MarginAccountID, l.AssetID, l.Principal.String(), int64(l.RateBPS), l.OpenedAt, l.BorrowJournalID, l.EffectID)
	return dbErr("open borrow lot", err)
}

// repayLot applies an allocation to a lot and closes it when nothing is owed.
func (st *store) repayLot(ctx context.Context, tx pgx.Tx, lid LotID, a LoanAllocation, reason LotCloseReason, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE margin_borrow_lots
		SET outstanding_principal = outstanding_principal - $2::numeric,
		    outstanding_interest  = outstanding_interest - $3::numeric,
		    closed_at    = CASE WHEN outstanding_principal - $2::numeric = 0 AND outstanding_interest - $3::numeric = 0 THEN $4::timestamptz END,
		    close_reason = CASE WHEN outstanding_principal - $2::numeric = 0 AND outstanding_interest - $3::numeric = 0 THEN $5 END
		WHERE id = $1 AND closed_at IS NULL`,
		lid, a.Principal.String(), a.Interest.String(), at, string(reason))
	return dbErr("repay borrow lot", err)
}

// insertAccrual records one accrual period and advances the lot to its end.
func (st *store) insertAccrual(ctx context.Context, tx pgx.Tx, l Lot, period Period, amount money.Quantity, journal *ledger.TransactionID, effect *econ.EffectID) error {
	if _, err := tx.Exec(ctx, `INSERT INTO margin_interest_accruals (id, lot_id, margin_account_id, period_start, period_end,
		principal_basis, rate_bps, amount, journal_transaction_id, effect_id)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8::numeric,$9,$10)`,
		newAccrualID(), l.ID, l.MarginAccountID, period.Start, period.End, l.OutstandingPrincipal.String(), int64(l.RateBPS),
		amount.String(), journal, effect); err != nil {
		return dbErr("record interest accrual", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE margin_borrow_lots SET accrued_through = $2,
		outstanding_interest = outstanding_interest + $3::numeric WHERE id = $1`, l.ID, period.End, amount.String()); err != nil {
		return dbErr("advance borrow lot", err)
	}
	return nil
}

func (st *store) insertMove(ctx context.Context, tx pgx.Tx, mid AccountID, asset assets.AssetID, dir MoveDirection, qty money.Quantity,
	valueMinor int64, journal ledger.TransactionID, effect econ.EffectID, intent *econ.IntentID, at time.Time) (MoveID, error) {
	mv := newMoveID()
	_, err := tx.Exec(ctx, `INSERT INTO margin_collateral_moves (id, margin_account_id, asset_id, direction, quantity, value_minor,
		journal_transaction_id, effect_id, intent_id, occurred_at) VALUES ($1,$2,$3,$4,$5::numeric,$6,$7,$8,$9,$10)`,
		mv, mid, asset, dir, qty.String(), valueMinor, journal, effect, intent, at)
	return mv, dbErr("record collateral move", err)
}

func (st *store) moveByEffect(ctx context.Context, q db.Querier, eid econ.EffectID) (MoveID, error) {
	var mv MoveID
	err := q.QueryRow(ctx, `SELECT id FROM margin_collateral_moves WHERE effect_id = $1`, eid).Scan(&mv)
	return mv, dbErr("load collateral move", err)
}

// moveVelocity counts the collateral moves in one direction and their value
// since a time.
func (st *store) moveVelocity(ctx context.Context, q db.Querier, mid AccountID, dir MoveDirection, since time.Time) (int, money.USD, error) {
	var n int
	var sum int64
	err := q.QueryRow(ctx, `SELECT count(*), coalesce(sum(value_minor), 0) FROM margin_collateral_moves
		WHERE margin_account_id = $1 AND direction = $2 AND occurred_at > $3`, mid, dir, since).Scan(&n, &sum)
	return n, money.USDFromMinor(sum), dbErr("read collateral move velocity", err)
}

// inReview reports whether the account's case is held for a person in
// MANUAL_REVIEW (at most one case of an account is unresolved).
func (st *store) inReview(ctx context.Context, q db.Querier, mid AccountID) (bool, error) {
	var open bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM margin_liquidation_cases
		WHERE margin_account_id = $1 AND status = 'MANUAL_REVIEW')`, mid).Scan(&open)
	return open, dbErr("read review status", err)
}

// tradingSteps counts the case's sales in its current budget window: the
// steps the budget bounds.
func (st *store) tradingSteps(ctx context.Context, q db.Querier, c Case) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM margin_liquidation_steps WHERE case_id = $1 AND action = 'SELL' AND step_no > $2`,
		c.ID, c.BudgetFromStep).Scan(&n)
	return n, dbErr("count trading steps", err)
}

const callColumns = `id, margin_account_id, status, issued_at, equity_units::text, maintenance_units::text, deficiency_units::text,
	evaluation_hash, resolved_at`

func scanCall(row pgx.Row) (Call, error) {
	var c Call
	var e, m, d string
	if err := row.Scan(&c.ID, &c.MarginAccountID, &c.Status, &c.IssuedAt, &e, &m, &d, &c.EvaluationHash, &c.ResolvedAt); err != nil {
		return Call{}, err
	}
	var err error
	if c.EquityUnits, err = money.ScanQuantity(e); err != nil {
		return Call{}, err
	}
	if c.MaintenanceUnits, err = money.ScanQuantity(m); err != nil {
		return Call{}, err
	}
	if c.DeficiencyUnits, err = money.ScanQuantity(d); err != nil {
		return Call{}, err
	}
	c.IssuedAt = c.IssuedAt.UTC()
	return c, nil
}

func (st *store) openCall(ctx context.Context, q db.Querier, mid AccountID) (Call, bool, error) {
	c, err := scanCall(q.QueryRow(ctx, `SELECT `+callColumns+` FROM margin_calls WHERE margin_account_id = $1 AND status = 'OPEN'`, mid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Call{}, false, nil
	}
	if err != nil {
		return Call{}, false, dbErr("load margin call", err)
	}
	return c, true, nil
}

func (st *store) issueCall(ctx context.Context, tx pgx.Tx, a Account, e Evaluation, actor econ.Actor, at time.Time) (Call, error) {
	c := Call{ID: newCallID(), MarginAccountID: a.ID, Status: CallOpen, IssuedAt: at, EquityUnits: e.Equity,
		MaintenanceUnits: e.MaintenanceMargin, DeficiencyUnits: e.MaintenanceMargin.Sub(e.Equity), EvaluationHash: e.Hash}
	if _, err := tx.Exec(ctx, `INSERT INTO margin_calls (id, margin_account_id, status, issued_at, equity_units, maintenance_units,
		deficiency_units, evaluation_hash) VALUES ($1,$2,'OPEN',$3,$4::numeric,$5::numeric,$6::numeric,$7)`,
		c.ID, c.MarginAccountID, c.IssuedAt, c.EquityUnits.String(), c.MaintenanceUnits.String(), c.DeficiencyUnits.String(), c.EvaluationHash); err != nil {
		return Call{}, dbErr("issue margin call", err)
	}
	if err := st.auditTransition(ctx, tx, a.AccountID, actor, "call", c.ID.String(), "", string(CallOpen), "equity below maintenance", e.Hash, at); err != nil {
		return Call{}, err
	}
	return c, nil
}

func (st *store) resolveCall(ctx context.Context, tx pgx.Tx, a Account, c *Call, to CallStatus, actor econ.Actor, reason string, at time.Time) error {
	if c.Status == to {
		return nil
	}
	if !CanTransitionCall(c.Status, to) {
		return errs.Newf(errs.CodeInvalidStateTransition, "margin call %s cannot move %s -> %s", c.ID, c.Status, to)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO margin_call_transitions (id, margin_call_id, from_status, to_status, reason, actor_type, actor_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id.New[id.Any](), c.ID, c.Status, to, reason, actor.Type, actor.ID, at); err != nil {
		return dbErr("record margin call transition", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE margin_calls SET status = $2, resolved_at = $3 WHERE id = $1`, c.ID, to, at); err != nil {
		return dbErr("resolve margin call", err)
	}
	if err := st.auditTransition(ctx, tx, a.AccountID, actor, "call", c.ID.String(), string(c.Status), string(to), reason, "", at); err != nil {
		return err
	}
	c.Status = to
	c.ResolvedAt = &at
	return nil
}

const caseColumns = `id, margin_account_id, margin_call_id, status, policy_version, target, max_steps, steps_executed,
	budget_from_step, opened_at, closed_at, last_reason, deficit_units::text, deficit_coverage_journal_transaction_id, deficit_covered_at`

func scanCase(row pgx.Row) (Case, error) {
	var c Case
	var deficit *string
	if err := row.Scan(&c.ID, &c.MarginAccountID, &c.MarginCallID, &c.Status, &c.PolicyVersion, &c.Target, &c.MaxSteps,
		&c.StepsExecuted, &c.BudgetFromStep, &c.OpenedAt, &c.ClosedAt, &c.LastReason, &deficit, &c.CoverageJournalID,
		&c.CoveredAt); err != nil {
		return Case{}, err
	}
	if deficit != nil {
		q, err := money.ScanQuantity(*deficit)
		if err != nil {
			return Case{}, err
		}
		c.DeficitUnits = &q
	}
	c.OpenedAt = c.OpenedAt.UTC()
	return c, nil
}

// latestCase returns the account's most recent case, if any.
func (st *store) latestCase(ctx context.Context, q db.Querier, mid AccountID, lock bool) (Case, bool, error) {
	sql := `SELECT ` + caseColumns + ` FROM margin_liquidation_cases WHERE margin_account_id = $1 ORDER BY opened_at DESC, id DESC LIMIT 1`
	if lock {
		sql += ` FOR UPDATE`
	}
	c, err := scanCase(q.QueryRow(ctx, sql, mid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Case{}, false, nil
	}
	if err != nil {
		return Case{}, false, dbErr("load liquidation case", err)
	}
	return c, true, nil
}

func (st *store) caseByID(ctx context.Context, q db.Querier, cid CaseID, lock bool) (Case, error) {
	sql := `SELECT ` + caseColumns + ` FROM margin_liquidation_cases WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	c, err := scanCase(q.QueryRow(ctx, sql, cid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Case{}, errs.New(errs.CodeNotFound, "liquidation case not found").WithField("case_id", cid.String())
	}
	if err != nil {
		return Case{}, dbErr("load liquidation case", err)
	}
	return c, nil
}

func (st *store) openCase(ctx context.Context, tx pgx.Tx, a Account, call Call, status CaseStatus, p Policy, reason string, actor econ.Actor, at time.Time) (Case, error) {
	c := Case{ID: newCaseID(), MarginAccountID: a.ID, MarginCallID: call.ID, Status: status, PolicyVersion: p.Version,
		Target: p.PartialTarget, MaxSteps: p.MaxLiquidationSteps, OpenedAt: at, LastReason: reason}
	if _, err := tx.Exec(ctx, `INSERT INTO margin_liquidation_cases (id, margin_account_id, margin_call_id, status, policy_version,
		target, max_steps, steps_executed, opened_at, last_reason) VALUES ($1,$2,$3,$4,$5,$6,$7,0,$8,$9)`,
		c.ID, c.MarginAccountID, c.MarginCallID, c.Status, c.PolicyVersion, c.Target, c.MaxSteps, c.OpenedAt, c.LastReason); err != nil {
		return Case{}, dbErr("open liquidation case", err)
	}
	if err := st.auditTransition(ctx, tx, a.AccountID, actor, "case", c.ID.String(), "", string(status), reason, "", at); err != nil {
		return Case{}, err
	}
	return c, nil
}

// setCaseStatus moves a case; a terminal status, or MANUAL_REVIEW, closes it
// for automation. Leaving MANUAL_REVIEW is not a status change this function
// makes: only an approved resolution does (resolveCase).
func (st *store) setCaseStatus(ctx context.Context, tx pgx.Tx, a Account, c *Case, to CaseStatus, reason string, deficit *money.Quantity, actor econ.Actor, at time.Time) error {
	if c.Status == to {
		if c.LastReason != reason {
			if _, err := tx.Exec(ctx, `UPDATE margin_liquidation_cases SET last_reason = $2 WHERE id = $1`, c.ID, reason); err != nil {
				return dbErr("annotate liquidation case", err)
			}
			c.LastReason = reason
		}
		return nil
	}
	if ResolutionRequired(c.Status) {
		return errs.Newf(errs.CodeInvalidStateTransition, "liquidation case %s is in %s; only an approved resolution moves it", c.ID, c.Status).
			WithField("case_id", c.ID.String())
	}
	if !CanTransitionCase(c.Status, to) {
		return errs.Newf(errs.CodeInvalidStateTransition, "liquidation case %s cannot move %s -> %s", c.ID, c.Status, to)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO margin_liquidation_case_transitions (id, case_id, from_status, to_status, reason, actor_type, actor_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id.New[id.Any](), c.ID, c.Status, to, reason, actor.Type, actor.ID, at); err != nil {
		return dbErr("record liquidation case transition", err)
	}
	var closed any
	if to.Closed() {
		closed = at
	}
	var deficitText any
	if deficit != nil {
		deficitText = deficit.String()
	}
	if _, err := tx.Exec(ctx, `UPDATE margin_liquidation_cases SET status = $2, last_reason = $3, closed_at = coalesce($4, closed_at),
		deficit_units = coalesce($5::numeric, deficit_units) WHERE id = $1`, c.ID, to, reason, closed, deficitText); err != nil {
		return dbErr("move liquidation case", err)
	}
	if err := st.auditTransition(ctx, tx, a.AccountID, actor, "case", c.ID.String(), string(c.Status), string(to), reason, "", at); err != nil {
		return err
	}
	c.Status, c.LastReason = to, reason
	if to.Closed() {
		c.ClosedAt = &at
	}
	if deficit != nil {
		d := *deficit
		c.DeficitUnits = &d
	}
	return nil
}

func (st *store) countStep(ctx context.Context, tx pgx.Tx, c *Case, stepNo int) error {
	if _, err := tx.Exec(ctx, `UPDATE margin_liquidation_cases SET steps_executed = $2 WHERE id = $1`, c.ID, stepNo); err != nil {
		return dbErr("count liquidation step", err)
	}
	c.StepsExecuted = stepNo
	return nil
}

func priceCols(p *money.Price) (mantissa, scale, source, at any) {
	if p == nil {
		return nil, nil, nil, nil
	}
	return p.Mantissa.String(), p.Scale, p.Source, p.At
}

func (st *store) insertStep(ctx context.Context, tx pgx.Tx, s Step) error {
	mm, ms, msrc, mat := priceCols(s.Mark)
	em, es, _, _ := priceCols(s.ExecutionPrice)
	_, err := tx.Exec(ctx, `INSERT INTO margin_liquidation_steps (id, case_id, step_no, action, asset_id, quantity,
		mark_mantissa, mark_scale, mark_source, mark_observed_at, execution_mantissa, execution_scale,
		slippage_bps, slippage_cap_bps, proceeds, fee, repaid, excess,
		trade_journal_transaction_id, release_journal_transaction_id, repay_journal_transaction_id, excess_journal_transaction_id,
		equity_before, equity_after, plan_hash, executed_at)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8,$9,$10,$11::numeric,$12,$13,$14,$15::numeric,$16::numeric,$17::numeric,
		        $18::numeric,$19,$20,$21,$22,$23::numeric,$24::numeric,$25,$26)`,
		s.ID, s.CaseID, s.StepNo, s.Action, s.AssetID, s.Quantity.String(), mm, ms, msrc, mat, em, es,
		int64(s.SlippageBPS), int64(s.SlippageCapBPS), s.Proceeds.String(), s.Fee.String(), s.Repaid.String(), s.Excess.String(),
		s.TradeJournalID, s.ReleaseJournal, s.RepayJournalID, s.ExcessJournalID,
		s.EquityBefore.String(), s.EquityAfter.String(), s.PlanHash, s.ExecutedAt)
	return dbErr("record liquidation step", err)
}

const stepColumns = `id, case_id, step_no, action, asset_id, quantity::text, slippage_bps, slippage_cap_bps, proceeds::text, fee::text,
	repaid::text, excess::text, trade_journal_transaction_id, release_journal_transaction_id, repay_journal_transaction_id,
	excess_journal_transaction_id, equity_before::text, equity_after::text, plan_hash, executed_at`

func (st *store) step(ctx context.Context, q db.Querier, cid CaseID, stepNo int) (Step, error) {
	var s Step
	var qty, proceeds, fee, repaid, excess, eb, ea string
	var slip, cap int64
	err := q.QueryRow(ctx, `SELECT `+stepColumns+` FROM margin_liquidation_steps WHERE case_id = $1 AND step_no = $2`, cid, stepNo).
		Scan(&s.ID, &s.CaseID, &s.StepNo, &s.Action, &s.AssetID, &qty, &slip, &cap, &proceeds, &fee, &repaid, &excess,
			&s.TradeJournalID, &s.ReleaseJournal, &s.RepayJournalID, &s.ExcessJournalID, &eb, &ea, &s.PlanHash, &s.ExecutedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Step{}, errs.New(errs.CodeNotFound, "liquidation step not found").WithField("case_id", cid.String()).WithField("step_no", stepNo)
	}
	if err != nil {
		return Step{}, dbErr("load liquidation step", err)
	}
	for _, f := range []struct {
		dst *money.Quantity
		src string
	}{{&s.Quantity, qty}, {&s.Proceeds, proceeds}, {&s.Fee, fee}, {&s.Repaid, repaid}, {&s.Excess, excess}, {&s.EquityBefore, eb}, {&s.EquityAfter, ea}} {
		if *f.dst, err = money.ScanQuantity(f.src); err != nil {
			return Step{}, dbErr("scan liquidation step", err)
		}
	}
	s.SlippageBPS, s.SlippageCapBPS = money.BPS(slip), money.BPS(cap)
	s.ExecutedAt = s.ExecutedAt.UTC()
	return s, nil
}

func (st *store) setCoverage(ctx context.Context, tx pgx.Tx, c *Case, journal ledger.TransactionID, at time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE margin_liquidation_cases SET deficit_coverage_journal_transaction_id = $2, deficit_covered_at = $3
		WHERE id = $1`, c.ID, journal, at); err != nil {
		return dbErr("record deficit coverage", err)
	}
	c.CoverageJournalID, c.CoveredAt = &journal, &at
	return nil
}

func (st *store) insertStressRun(ctx context.Context, tx pgx.Tx, mid AccountID, r StressReport, at time.Time) (StressRunID, error) {
	body, err := audit.CanonicalJSON(r)
	if err != nil {
		return StressRunID{}, errs.Wrap(err, errs.CodeInternal, "margin: encode stress report")
	}
	rid := newStressRunID()
	_, err = tx.Exec(ctx, `INSERT INTO margin_stress_runs (id, margin_account_id, policy_version, policy_hash, input_hash, report, report_hash, run_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, rid, mid, r.PolicyVersion, r.PolicyHash, r.InputHash, body, r.Hash, at)
	return rid, dbErr("record stress run", err)
}
