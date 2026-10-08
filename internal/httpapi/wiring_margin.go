package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
)

// MarginService is the margin service AdvancedDeps carries. It is an alias,
// declared here so the shared wiring file needs no import of its own.
type MarginService = margin.Service

// marginAdapter serves MarginPort over the margin service.
//
// Every command runs exactly one service operation in its own transaction.
// Every read runs in one read-only REPEATABLE READ snapshot, so a response
// never mixes two states of the same account. The service decides every
// figure and every refusal; this adapter only chooses the transaction and
// reads the margin tables back, read-only, where the margin package exposes
// no listing of its own (calls, cases, steps, stress runs, the queue).
type marginAdapter struct {
	svc    *margin.Service
	ledger *ledger.Service
	db     *db.DB
}

var _ MarginPort = marginAdapter{}

func (a marginAdapter) snapshot(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

// marginIntentKey names the economic intent an operation submits: the
// margin service keys intents on the customer account and the caller's
// idempotency key.
type marginIntentKey struct {
	account accounts.AccountID
	key     string
}

// command runs one margin operation in its own transaction.
//
// The margin service writes a refusal before it returns it -- the Risk
// Kernel V2 decision and the economic intent moved to REJECTED -- so that
// the record survives and a replay of the same key answers the same refusal
// (margin.Service). When the operation's intent ended REJECTED with a
// business refusal the transaction therefore commits and the refusal is
// still returned. Every other error rolls everything back: an operation that
// failed part way through execution is not a fact.
func (a marginAdapter) command(ctx context.Context, intent *marginIntentKey, fn func(ctx context.Context, tx pgx.Tx) error) error {
	var refusal error
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		refusal = nil
		ferr := fn(ctx, tx)
		if ferr == nil || intent == nil {
			return ferr
		}
		if errs.HTTPStatus(errs.CodeOf(ferr)) < 500 && marginRefusalRecorded(ctx, tx, *intent) {
			refusal = ferr
			return nil
		}
		return ferr
	})
	if err != nil {
		return err
	}
	return refusal
}

// marginRefusalRecorded reports whether the operation's economic intent is
// REJECTED: the service refused it before anything executed. A transaction
// that can no longer be read from answers false, and so rolls back.
func marginRefusalRecorded(ctx context.Context, tx pgx.Tx, k marginIntentKey) bool {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM economic_intents WHERE account_id = $1 AND idempotency_key = $2`,
		k.account, k.key).Scan(&status)
	return err == nil && status == string(econ.StatusRejected)
}

// --- reads of one account ----------------------------------------------------

func (a marginAdapter) Account(ctx context.Context, acct accounts.AccountID) (MarginAccountView, error) {
	var out MarginAccountView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		m, err := a.svc.Account(ctx, tx, acct)
		if err != nil {
			return err
		}
		st, err := a.svc.State(ctx, tx, acct)
		if err != nil {
			return err
		}
		ev, err := margin.Evaluate(a.svc.Policy(), st)
		if err != nil {
			return err
		}
		v := MarginAccountView{Account: m, State: st, Evaluation: ev}
		if v.OpenCall, err = marginOpenCall(ctx, tx, m.ID); err != nil {
			return err
		}
		if v.LatestCase, err = marginLatestCase(ctx, tx, m.ID); err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

func (a marginAdapter) Calls(ctx context.Context, acct accounts.AccountID, cursor string, limit int) (MarginCallPage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return MarginCallPage{}, err
	}
	limit = clampLimit(limit)
	items, err := marginCalls(ctx, a.db, marginAccountCallsSQL, acct, at, after, limit+1)
	if err != nil {
		return MarginCallPage{}, err
	}
	return marginCallPage(items, limit), nil
}

func (a marginAdapter) Cases(ctx context.Context, acct accounts.AccountID, cursor string, limit int) (MarginCasePage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return MarginCasePage{}, err
	}
	limit = clampLimit(limit)
	rows, err := a.db.Query(ctx, marginAccountCasesSQL, acct, at, after, limit+1)
	if err != nil {
		return MarginCasePage{}, err
	}
	defer rows.Close()
	var items []MarginCaseView
	for rows.Next() {
		v, err := scanMarginCase(rows)
		if err != nil {
			return MarginCasePage{}, err
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return MarginCasePage{}, err
	}
	page := MarginCasePage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1].Case
		page.NextCursor = encodeCursor(keysetCursor{At: last.OpenedAt, ID: last.ID.String()})
	}
	return page, nil
}

func (a marginAdapter) Case(ctx context.Context, cid margin.CaseID) (MarginCaseView, error) {
	var out MarginCaseView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		v, err := scanMarginCase(tx.QueryRow(ctx, marginCaseByIDSQL, cid))
		if errors.Is(err, pgx.ErrNoRows) {
			return errs.New(errs.CodeNotFound, "liquidation case not found")
		}
		if err != nil {
			return err
		}
		if v.Steps, err = marginSteps(ctx, tx, v); err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

func (a marginAdapter) StressRuns(ctx context.Context, acct accounts.AccountID, cursor string, limit int) (MarginStressRunPage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return MarginStressRunPage{}, err
	}
	limit = clampLimit(limit)
	rows, err := a.db.Query(ctx, marginAccountStressRunsSQL, acct, at, after, limit+1)
	if err != nil {
		return MarginStressRunPage{}, err
	}
	defer rows.Close()
	var items []MarginStressRunView
	for rows.Next() {
		v, err := scanMarginStressRun(rows)
		if err != nil {
			return MarginStressRunPage{}, err
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return MarginStressRunPage{}, err
	}
	page := MarginStressRunPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(keysetCursor{At: last.RunAt, ID: last.ID.String()})
	}
	return page, nil
}

func (a marginAdapter) StressRun(ctx context.Context, rid margin.StressRunID) (MarginStressRunView, error) {
	var out MarginStressRunView
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		v, err := marginStressRunByID(ctx, tx, rid)
		if err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

// --- commands of the account's owner -------------------------------------------

func (a marginAdapter) Open(ctx context.Context, r MarginOpen) (margin.Account, error) {
	var out margin.Account
	err := a.command(ctx, nil, func(ctx context.Context, tx pgx.Tx) error {
		// The numeraire is the deployment's one sandbox dollar. It is read,
		// never created here: a deployment that has not provisioned it has no
		// simulated value at all, which is UNSUPPORTED, not "no such account"
		// (as buying power V2 answers).
		usd, err := econ.SandboxDollar(ctx, tx)
		if errs.HasCode(err, errs.CodeNotFound) {
			return errs.New(errs.CodeUnsupported, "the sandbox dollar is not provisioned in this deployment, so no margin account can be opened")
		}
		if err != nil {
			return err
		}
		acct, err := a.svc.OpenAccount(ctx, tx, margin.OpenRequest{
			AccountID: r.AccountID, MarginMode: r.MarginMode, Mode: r.Mode, NumeraireAssetID: usd.ID,
			IsolatedAssetID: r.IsolatedAssetID, Actor: r.Actor,
		})
		if err != nil {
			return err
		}
		out = acct
		return nil
	})
	return out, err
}

func (a marginAdapter) Close(ctx context.Context, acct accounts.AccountID, by econ.Actor) (margin.Account, error) {
	var out margin.Account
	err := a.command(ctx, nil, func(ctx context.Context, tx pgx.Tx) error {
		closed, err := a.svc.CloseAccount(ctx, tx, acct, by)
		if err != nil {
			return err
		}
		out = closed
		return nil
	})
	return out, err
}

func (a marginAdapter) PostCollateral(ctx context.Context, r margin.CollateralRequest) (margin.CollateralResult, error) {
	var out margin.CollateralResult
	err := a.command(ctx, nil, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.PostCollateral(ctx, tx, r)
		if err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}

func (a marginAdapter) ReleaseCollateral(ctx context.Context, r margin.CollateralRequest) (margin.CollateralResult, error) {
	var out margin.CollateralResult
	err := a.command(ctx, &marginIntentKey{account: r.AccountID, key: r.IdempotencyKey}, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.ReleaseCollateral(ctx, tx, r)
		if err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}

func (a marginAdapter) Borrow(ctx context.Context, r margin.LoanRequest) (margin.LoanResult, error) {
	var out margin.LoanResult
	err := a.command(ctx, &marginIntentKey{account: r.AccountID, key: r.IdempotencyKey}, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.Borrow(ctx, tx, r)
		if err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}

func (a marginAdapter) Repay(ctx context.Context, r margin.LoanRequest) (margin.LoanResult, error) {
	var out margin.LoanResult
	err := a.command(ctx, &marginIntentKey{account: r.AccountID, key: r.IdempotencyKey}, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.Repay(ctx, tx, r)
		if err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}

func (a marginAdapter) LeveragedBuy(ctx context.Context, r margin.SpotRequest) (margin.SpotResult, error) {
	var out margin.SpotResult
	err := a.command(ctx, &marginIntentKey{account: r.AccountID, key: r.IdempotencyKey}, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.OpenLeveragedSpot(ctx, tx, r)
		if err != nil {
			return err
		}
		out = res
		return nil
	})
	return out, err
}

func (a marginAdapter) RunStress(ctx context.Context, acct accounts.AccountID) (MarginStressRunView, error) {
	var out MarginStressRunView
	err := a.command(ctx, nil, func(ctx context.Context, tx pgx.Tx) error {
		run, err := a.svc.RunStress(ctx, tx, acct)
		if err != nil {
			return err
		}
		// The stored row, read back in the same transaction, supplies when the
		// run was recorded and whose it is; the report is the one just made.
		v, err := scanMarginStressRun(tx.QueryRow(ctx, marginStressRunSummaryByIDSQL, run.ID))
		if err != nil {
			return err
		}
		v.Report = run.Report
		out = v
		return nil
	})
	return out, err
}

// --- operator actions and reads across accounts --------------------------------

func (a marginAdapter) SeedFacilities(ctx context.Context, r MarginSeed) (MarginFacilitiesView, error) {
	var out MarginFacilitiesView
	err := a.command(ctx, nil, func(ctx context.Context, tx pgx.Tx) error {
		// Seeding the facilities is how a deployment bootstraps margin, so the
		// sandbox dollar they hold is provisioned here if it is absent, through
		// econ's one constructor (there is only ever one sandbox dollar).
		usd, err := econ.EnsureSandboxDollar(ctx, tx)
		if err != nil {
			return err
		}
		f, err := a.svc.SeedFacilities(ctx, tx, margin.SeedRequest{
			Asset: usd.ID, PoolAmount: r.PoolAmount, FundAmount: r.FundAmount, Key: r.Key, Actor: r.Actor,
		})
		if err != nil {
			return err
		}
		pool, err := a.ledger.Balance(ctx, tx, f.Pool.Account(ledger.CodePoolCash, usd.ID))
		if err != nil {
			return err
		}
		fund, err := a.ledger.Balance(ctx, tx, f.Fund.Account(ledger.CodeInsuranceFundCash, usd.ID))
		if err != nil {
			return err
		}
		out = MarginFacilitiesView{AssetID: usd.ID, AssetDecimals: usd.Decimals, Pool: f.Pool, Fund: f.Fund, PoolAvailable: pool, FundAvailable: fund}
		return nil
	})
	return out, err
}

func (a marginAdapter) MarkToMarket(ctx context.Context, limit int) (MarginMarkView, error) {
	var out MarginMarkView
	err := a.command(ctx, nil, func(ctx context.Context, tx pgx.Tx) error {
		evaluated, failures, err := a.svc.MarkToMarket(ctx, tx, limit)
		if err != nil {
			return err
		}
		out = MarginMarkView{Evaluated: evaluated, Failures: failures}
		return nil
	})
	return out, err
}

func (a marginAdapter) Queue(ctx context.Context, limit int) ([]MarginQueueEntry, error) {
	limit = clampLimit(limit)
	var out []MarginQueueEntry
	err := a.snapshot(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The selection is ProcessLiquidationQueue's: accounts in MARGIN_CALL
		// or LIQUIDATING under the policy this service runs. The order (most
		// under-margined first) is the run's to decide; this lists by id.
		rows, err := tx.Query(ctx, marginQueueSQL, a.svc.Policy().Version, limit)
		if err != nil {
			return err
		}
		var entries []MarginQueueEntry
		for rows.Next() {
			e, err := scanMarginQueueAccount(rows)
			if err != nil {
				rows.Close()
				return err
			}
			entries = append(entries, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range entries {
			e := &entries[i]
			// Each account is evaluated in its own savepoint: one account the
			// engine cannot vouch for is reported as such, not as the queue's
			// failure.
			if err := marginReadSavepoint(ctx, tx, func(sp pgx.Tx) error {
				st, err := a.svc.State(ctx, sp, e.Account.AccountID)
				if err != nil {
					return err
				}
				ev, err := margin.Evaluate(a.svc.Policy(), st)
				if err != nil {
					return err
				}
				e.State, e.Evaluation = &st, &ev
				return nil
			}); err != nil {
				e.EvaluationErr = err
			}
			if e.OpenCall, err = marginOpenCall(ctx, tx, e.Account.ID); err != nil {
				return err
			}
			if e.LatestCase, err = marginLatestCase(ctx, tx, e.Account.ID); err != nil {
				return err
			}
		}
		out = entries
		return nil
	})
	return out, err
}

func (a marginAdapter) RunQueue(ctx context.Context, limit int) (MarginQueueRunView, error) {
	var out MarginQueueRunView
	err := a.command(ctx, nil, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.ProcessLiquidationQueue(ctx, tx, limit)
		if err != nil {
			return err
		}
		v := MarginQueueRunView{Failures: res.Failures}
		for _, r := range res.Results {
			facts, err := marginFactsOf(ctx, tx, r.MarginAccountID)
			if err != nil {
				return err
			}
			v.Results = append(v.Results, MarginLiquidationView{Result: r, Facts: facts})
		}
		out = v
		return nil
	})
	return out, err
}

func (a marginAdapter) Liquidate(ctx context.Context, r margin.LiquidateRequest) (MarginLiquidationView, error) {
	var out MarginLiquidationView
	err := a.command(ctx, nil, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.Liquidate(ctx, tx, r)
		if err != nil {
			return err
		}
		facts, err := marginFactsOf(ctx, tx, res.MarginAccountID)
		if err != nil {
			return err
		}
		out = MarginLiquidationView{Result: res, Facts: facts}
		return nil
	})
	return out, err
}

func (a marginAdapter) CoverDeficit(ctx context.Context, cid margin.CaseID) (MarginDeficitView, error) {
	var out MarginDeficitView
	err := a.command(ctx, nil, func(ctx context.Context, tx pgx.Tx) error {
		res, err := a.svc.CoverDeficit(ctx, tx, cid)
		if err != nil {
			return err
		}
		c, err := scanMarginCase(tx.QueryRow(ctx, marginCaseByIDSQL, cid))
		if err != nil {
			return err
		}
		out = MarginDeficitView{Result: res, Case: c}
		return nil
	})
	return out, err
}

func (a marginAdapter) AllCalls(ctx context.Context, status margin.CallStatus, cursor string, limit int) (MarginCallPage, error) {
	at, after, err := marginCursor(cursor)
	if err != nil {
		return MarginCallPage{}, err
	}
	limit = clampLimit(limit)
	items, err := marginCalls(ctx, a.db, marginAllCallsSQL, string(status), at, after, limit+1)
	if err != nil {
		return MarginCallPage{}, err
	}
	return marginCallPage(items, limit), nil
}

// --- the read projection ---------------------------------------------------------
//
// Strictly read-only SELECTs over the margin tables, for the records the
// margin package writes but does not list. Nothing here decides anything:
// values are returned exactly as the service stored them.

const marginFactsColumns = `m.account_id, m.numeraire_asset_id, n.decimals`

const marginCallColumns = `c.id, c.margin_account_id, c.status, c.issued_at, c.equity_units::text, c.maintenance_units::text,
	c.deficiency_units::text, c.evaluation_hash, c.resolved_at, ` + marginFactsColumns

const marginCallFrom = ` FROM margin_calls c JOIN margin_accounts m ON m.id = c.margin_account_id
	JOIN assets n ON n.id = m.numeraire_asset_id`

const marginAccountCallsSQL = `SELECT ` + marginCallColumns + marginCallFrom + `
	WHERE m.account_id = $1 AND ($2::timestamptz IS NULL OR (c.issued_at, c.id) < ($2::timestamptz, $3::uuid))
	ORDER BY c.issued_at DESC, c.id DESC LIMIT $4`

const marginAllCallsSQL = `SELECT ` + marginCallColumns + marginCallFrom + `
	WHERE ($1::text = '' OR c.status = $1::text) AND ($2::timestamptz IS NULL OR (c.issued_at, c.id) < ($2::timestamptz, $3::uuid))
	ORDER BY c.issued_at DESC, c.id DESC LIMIT $4`

const marginOpenCallSQL = `SELECT ` + marginCallColumns + marginCallFrom + `
	WHERE c.margin_account_id = $1 AND c.status = 'OPEN'`

const marginCaseColumns = `k.id, k.margin_account_id, k.margin_call_id, k.status, k.policy_version, k.target, k.max_steps,
	k.steps_executed, k.opened_at, k.closed_at, k.last_reason, k.deficit_units::text, k.deficit_coverage_journal_transaction_id,
	k.deficit_covered_at, ` + marginFactsColumns

const marginCaseFrom = ` FROM margin_liquidation_cases k JOIN margin_accounts m ON m.id = k.margin_account_id
	JOIN assets n ON n.id = m.numeraire_asset_id`

const marginAccountCasesSQL = `SELECT ` + marginCaseColumns + marginCaseFrom + `
	WHERE m.account_id = $1 AND ($2::timestamptz IS NULL OR (k.opened_at, k.id) < ($2::timestamptz, $3::uuid))
	ORDER BY k.opened_at DESC, k.id DESC LIMIT $4`

const marginCaseByIDSQL = `SELECT ` + marginCaseColumns + marginCaseFrom + ` WHERE k.id = $1`

const marginLatestCaseSQL = `SELECT ` + marginCaseColumns + marginCaseFrom + `
	WHERE k.margin_account_id = $1 ORDER BY k.opened_at DESC, k.id DESC LIMIT 1`

const marginStepsSQL = `SELECT id, step_no, action, asset_id, quantity::text, mark_mantissa::text, mark_scale, mark_source,
	mark_observed_at, execution_mantissa::text, execution_scale, slippage_bps, slippage_cap_bps, proceeds::text, fee::text,
	repaid::text, excess::text, trade_journal_transaction_id, release_journal_transaction_id, repay_journal_transaction_id,
	excess_journal_transaction_id, equity_before::text, equity_after::text, plan_hash, executed_at
	FROM margin_liquidation_steps WHERE case_id = $1 ORDER BY step_no`

const marginStressColumns = `r.id, r.margin_account_id, r.policy_version, r.policy_hash, r.input_hash, r.report_hash, r.run_at, ` +
	marginFactsColumns

const marginStressFrom = ` FROM margin_stress_runs r JOIN margin_accounts m ON m.id = r.margin_account_id
	JOIN assets n ON n.id = m.numeraire_asset_id`

const marginAccountStressRunsSQL = `SELECT ` + marginStressColumns + marginStressFrom + `
	WHERE m.account_id = $1 AND ($2::timestamptz IS NULL OR (r.run_at, r.id) < ($2::timestamptz, $3::uuid))
	ORDER BY r.run_at DESC, r.id DESC LIMIT $4`

const marginStressRunSummaryByIDSQL = `SELECT ` + marginStressColumns + marginStressFrom + ` WHERE r.id = $1`

const marginStressRunByIDSQL = `SELECT ` + marginStressColumns + `, r.report` + marginStressFrom + ` WHERE r.id = $1`

const marginQueueSQL = `SELECT m.id, m.account_id, m.margin_mode, m.mode, m.status, m.policy_version, m.numeraire_asset_id,
	m.isolated_asset_id, m.lending_pool_facility_id, m.insurance_fund_facility_id, m.opened_by_actor_type, m.opened_by_actor_id,
	m.opened_at, n.decimals
	FROM margin_accounts m JOIN assets n ON n.id = m.numeraire_asset_id
	WHERE m.status IN ('MARGIN_CALL','LIQUIDATING') AND m.policy_version = $1
	ORDER BY m.id LIMIT $2`

const marginFactsSQL = `SELECT ` + marginFactsColumns + ` FROM margin_accounts m
	JOIN assets n ON n.id = m.numeraire_asset_id WHERE m.id = $1`

// marginCursor decodes a page cursor into the bound parameters of a keyset
// query: (nil, nil) is the first page.
func marginCursor(cursor string) (*time.Time, *string, error) {
	c, ok, err := decodeCursor(cursor)
	if err != nil || !ok {
		return nil, nil, err
	}
	u, err := uuid.Parse(c.ID)
	if err != nil {
		return nil, nil, validationError("cursor", "cursor is not a valid pagination cursor")
	}
	at, after := c.At, u.String()
	return &at, &after, nil
}

func marginDecimals(d int16) (uint8, error) {
	if d < 0 || d > 38 {
		return 0, errs.New(errs.CodeInternal, "stored asset decimals are out of range")
	}
	return uint8(d), nil
}

// marginQty is one numeric column read as text and the quantity it fills.
type marginQty struct {
	dst *money.Quantity
	src string
}

func marginScanQuantities(cols ...marginQty) error {
	for _, c := range cols {
		q, err := money.ScanQuantity(c.src)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "a stored margin quantity is not an integer")
		}
		*c.dst = q
	}
	return nil
}

func scanMarginCall(row pgx.Row) (MarginCallView, error) {
	var v MarginCallView
	var equity, maintenance, deficiency string
	var decimals int16
	if err := row.Scan(&v.Call.ID, &v.Call.MarginAccountID, &v.Call.Status, &v.Call.IssuedAt, &equity, &maintenance, &deficiency,
		&v.Call.EvaluationHash, &v.Call.ResolvedAt, &v.Facts.AccountID, &v.Facts.NumeraireAssetID, &decimals); err != nil {
		return MarginCallView{}, err
	}
	if err := marginScanQuantities(marginQty{&v.Call.EquityUnits, equity}, marginQty{&v.Call.MaintenanceUnits, maintenance},
		marginQty{&v.Call.DeficiencyUnits, deficiency}); err != nil {
		return MarginCallView{}, err
	}
	var err error
	if v.Facts.NumeraireDecimals, err = marginDecimals(decimals); err != nil {
		return MarginCallView{}, err
	}
	v.Call.IssuedAt = v.Call.IssuedAt.UTC()
	return v, nil
}

func marginCalls(ctx context.Context, q db.Querier, sql string, args ...any) ([]MarginCallView, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MarginCallView
	for rows.Next() {
		v, err := scanMarginCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func marginCallPage(items []MarginCallView, limit int) MarginCallPage {
	page := MarginCallPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1].Call
		page.NextCursor = encodeCursor(keysetCursor{At: last.IssuedAt, ID: last.ID.String()})
	}
	return page
}

func marginOpenCall(ctx context.Context, q db.Querier, mid margin.AccountID) (*MarginCallView, error) {
	v, err := scanMarginCall(q.QueryRow(ctx, marginOpenCallSQL, mid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func scanMarginCase(row pgx.Row) (MarginCaseView, error) {
	var v MarginCaseView
	var deficit *string
	var decimals int16
	c := &v.Case
	if err := row.Scan(&c.ID, &c.MarginAccountID, &c.MarginCallID, &c.Status, &c.PolicyVersion, &c.Target, &c.MaxSteps,
		&c.StepsExecuted, &c.OpenedAt, &c.ClosedAt, &c.LastReason, &deficit, &c.CoverageJournalID, &c.CoveredAt,
		&v.Facts.AccountID, &v.Facts.NumeraireAssetID, &decimals); err != nil {
		return MarginCaseView{}, err
	}
	if deficit != nil {
		var d money.Quantity
		if err := marginScanQuantities(marginQty{&d, *deficit}); err != nil {
			return MarginCaseView{}, err
		}
		c.DeficitUnits = &d
	}
	var err error
	if v.Facts.NumeraireDecimals, err = marginDecimals(decimals); err != nil {
		return MarginCaseView{}, err
	}
	c.OpenedAt = c.OpenedAt.UTC()
	return v, nil
}

func marginLatestCase(ctx context.Context, q db.Querier, mid margin.AccountID) (*MarginCaseView, error) {
	v, err := scanMarginCase(q.QueryRow(ctx, marginLatestCaseSQL, mid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// marginSteps returns a case's executed steps, oldest first, with the marks
// they sold at. A mark is quoted in the account's numeraire.
func marginSteps(ctx context.Context, q db.Querier, c MarginCaseView) ([]margin.Step, error) {
	rows, err := q.Query(ctx, marginStepsSQL, c.Case.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	quote := c.Facts.NumeraireAssetID.String()
	out := []margin.Step{}
	for rows.Next() {
		var s margin.Step
		var quantity, proceeds, fee, repaid, excess, before, after string
		var markMantissa, markSource, execMantissa *string
		var markScale, execScale *int32
		var markAt *time.Time
		var slippage, slippageCap int64
		if err := rows.Scan(&s.ID, &s.StepNo, &s.Action, &s.AssetID, &quantity, &markMantissa, &markScale, &markSource, &markAt,
			&execMantissa, &execScale, &slippage, &slippageCap, &proceeds, &fee, &repaid, &excess, &s.TradeJournalID,
			&s.ReleaseJournal, &s.RepayJournalID, &s.ExcessJournalID, &before, &after, &s.PlanHash, &s.ExecutedAt); err != nil {
			return nil, err
		}
		if err := marginScanQuantities(marginQty{&s.Quantity, quantity}, marginQty{&s.Proceeds, proceeds}, marginQty{&s.Fee, fee},
			marginQty{&s.Repaid, repaid}, marginQty{&s.Excess, excess}, marginQty{&s.EquityBefore, before}, marginQty{&s.EquityAfter, after}); err != nil {
			return nil, err
		}
		if markMantissa != nil && markScale != nil {
			p := money.Price{Scale: *markScale, QuoteAsset: quote}
			if err := marginScanQuantities(marginQty{&p.Mantissa, *markMantissa}); err != nil {
				return nil, err
			}
			if markSource != nil {
				p.Source = *markSource
			}
			if markAt != nil {
				p.At = markAt.UTC()
			}
			s.Mark = &p
		}
		if execMantissa != nil && execScale != nil {
			p := money.Price{Scale: *execScale, QuoteAsset: quote}
			if err := marginScanQuantities(marginQty{&p.Mantissa, *execMantissa}); err != nil {
				return nil, err
			}
			s.ExecutionPrice = &p
		}
		s.CaseID = c.Case.ID
		s.SlippageBPS, s.SlippageCapBPS = money.BPS(slippage), money.BPS(slippageCap)
		s.ExecutedAt = s.ExecutedAt.UTC()
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanMarginStressRun(row pgx.Row, extra ...any) (MarginStressRunView, error) {
	var v MarginStressRunView
	var decimals int16
	dst := []any{&v.ID, &v.MarginAccountID, &v.Report.PolicyVersion, &v.Report.PolicyHash, &v.Report.InputHash, &v.Report.Hash,
		&v.RunAt, &v.Facts.AccountID, &v.Facts.NumeraireAssetID, &decimals}
	if err := row.Scan(append(dst, extra...)...); err != nil {
		return MarginStressRunView{}, err
	}
	var err error
	if v.Facts.NumeraireDecimals, err = marginDecimals(decimals); err != nil {
		return MarginStressRunView{}, err
	}
	v.Report.AccountID = v.Facts.AccountID.String()
	v.RunAt = v.RunAt.UTC()
	return v, nil
}

// marginStressRunByID reads one stored stress run with its report. The
// report is history: it is checked against the hash it was stored with, and
// a report that no longer matches is refused rather than shown.
func marginStressRunByID(ctx context.Context, q db.Querier, rid margin.StressRunID) (MarginStressRunView, error) {
	var report []byte
	v, err := scanMarginStressRun(q.QueryRow(ctx, marginStressRunByIDSQL, rid), &report)
	if errors.Is(err, pgx.ErrNoRows) {
		return MarginStressRunView{}, errs.New(errs.CodeNotFound, "stress run not found")
	}
	if err != nil {
		return MarginStressRunView{}, err
	}
	var rep margin.StressReport
	if err := json.Unmarshal(report, &rep); err != nil {
		return MarginStressRunView{}, errs.Wrap(err, errs.CodeInternal, "a stored stress report could not be decoded")
	}
	if rep.Hash != v.Report.Hash || rep.ComputeHash() != v.Report.Hash {
		return MarginStressRunView{}, errs.New(errs.CodeInternal, "a stored stress report does not match its hash")
	}
	v.Report = rep
	return v, nil
}

func scanMarginQueueAccount(row pgx.Row) (MarginQueueEntry, error) {
	var e MarginQueueEntry
	var decimals int16
	m := &e.Account
	if err := row.Scan(&m.ID, &m.AccountID, &m.MarginMode, &m.Mode, &m.Status, &m.PolicyVersion, &m.NumeraireAssetID,
		&m.IsolatedAssetID, &m.LendingPoolID, &m.InsuranceFundID, &m.OpenedBy.Type, &m.OpenedBy.ID, &m.OpenedAt, &decimals); err != nil {
		return MarginQueueEntry{}, err
	}
	var err error
	if e.Facts.NumeraireDecimals, err = marginDecimals(decimals); err != nil {
		return MarginQueueEntry{}, err
	}
	e.Facts.AccountID, e.Facts.NumeraireAssetID = m.AccountID, m.NumeraireAssetID
	m.OpenedAt = m.OpenedAt.UTC()
	return e, nil
}

func marginFactsOf(ctx context.Context, q db.Querier, mid margin.AccountID) (MarginFacts, error) {
	var f MarginFacts
	var decimals int16
	if err := q.QueryRow(ctx, marginFactsSQL, mid).Scan(&f.AccountID, &f.NumeraireAssetID, &decimals); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MarginFacts{}, errs.New(errs.CodeNotFound, "margin account not found")
		}
		return MarginFacts{}, err
	}
	var err error
	f.NumeraireDecimals, err = marginDecimals(decimals)
	return f, err
}

// marginReadSavepoint runs fn in a savepoint of a read-only transaction and
// always rolls the savepoint back: it reads, so there is nothing to keep, and
// a failure inside it leaves the transaction usable.
func marginReadSavepoint(ctx context.Context, tx pgx.Tx, fn func(sp pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = sp.Rollback(ctx) }()
	return fn(sp)
}
