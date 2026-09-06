package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
)

// The read model is the API's own projection over tables the domain packages
// own but do not expose a listing for. It is strictly read-only: every
// statement here is a SELECT, and nothing in this file decides anything
// financial. Writes always go through the domain package that owns the table.

// ReadModel answers the paged read ports from the database.
type ReadModel struct {
	q db.Querier
}

// NewReadModel returns a read model over q.
func NewReadModel(q db.Querier) *ReadModel { return &ReadModel{q: q} }

// --- cursors ----------------------------------------------------------------

// clampLimit bounds a page size. Handlers already clamp, but the read model is
// also called directly, and a non-positive limit must never reach SQL.
func clampLimit(limit int) int {
	switch {
	case limit < 1:
		return 1
	case limit > maxPageLimit:
		return maxPageLimit
	default:
		return limit
	}
}

// keysetCursor is an opaque "(timestamp, id)" position. It is base64 so a
// client cannot construct one from a guessed id and so its shape can change
// without breaking clients.
type keysetCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

func encodeCursor(c keysetCursor) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (keysetCursor, bool, error) {
	if s == "" {
		return keysetCursor{}, false, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return keysetCursor{}, false, validationError("cursor", "cursor is not a valid pagination cursor")
	}
	var c keysetCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return keysetCursor{}, false, validationError("cursor", "cursor is not a valid pagination cursor")
	}
	return c, true, nil
}

// --- orders -----------------------------------------------------------------

const orderColumns = `id, intent_id, plan_id, account_id, instrument_id, venue_listing_id, side, mode, status,
	input_asset_id, input_quantity, output_asset_id, min_output_quantity,
	filled_input_quantity, filled_output_quantity, reservation_id, quote_id,
	rejection_code, correlation_id, terminal_at, created_at, updated_at`

// ListOrders pages an account's orders, newest first.
func (m *ReadModel) ListOrders(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) (OrderPage, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := decodeCursor(cursor)
	if err != nil {
		return OrderPage{}, err
	}
	sql := `SELECT ` + orderColumns + ` FROM orders WHERE account_id = $1`
	args := []any{accountID}
	if hasCursor {
		sql += ` AND (created_at, id) < ($2, $3)`
		args = append(args, c.At, c.ID)
	}
	sql += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT %d`, limit+1)

	rows, err := m.q.Query(ctx, sql, args...)
	if err != nil {
		return OrderPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	defer rows.Close()

	var out []execution.Order
	for rows.Next() {
		o, serr := scanOrder(rows)
		if serr != nil {
			return OrderPage{}, serr
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return OrderPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}

	page := OrderPage{Items: out}
	if len(out) > limit {
		page.Items = out[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
	}
	return page, nil
}

func scanOrder(rows pgx.Rows) (execution.Order, error) {
	var (
		o                                         execution.Order
		intentID, planID, instrumentID, listingID string
		reservationID, quoteID                    string
		rejection, correlation                    *string
		terminalAt                                *time.Time
		inQty, minOut, filledIn, filledOut        money.Quantity
		side, mode, status                        string
	)
	if err := rows.Scan(&o.ID, &intentID, &planID, &o.AccountID, &instrumentID, &listingID,
		&side, &mode, &status, &o.InputAssetID, &inQty, &o.OutputAssetID, &minOut,
		&filledIn, &filledOut, &reservationID, &quoteID,
		&rejection, &correlation, &terminalAt, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return execution.Order{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	o.IntentID = intentID
	o.PlanID = planID
	o.InstrumentID = instrumentID
	o.VenueListingID = listingID
	o.Side = execution.Side(side)
	o.Mode = execution.Mode(mode)
	o.Status = execution.OrderStatus(status)
	o.InputQuantity = inQty
	o.MinOutputQuantity = minOut
	o.FilledInputQuantity = filledIn
	o.FilledOutputQuantity = filledOut
	o.ReservationID = reservationID
	o.QuoteID = quoteID
	if rejection != nil {
		o.RejectionCode = *rejection
	}
	if correlation != nil {
		o.CorrelationID = *correlation
	}
	o.TerminalAt = terminalAt
	return o, nil
}

// --- intent transitions -----------------------------------------------------

// IntentTransitions returns an intent's transition history, oldest first.
func (m *ReadModel) IntentTransitions(ctx context.Context, intentID string) ([]StateTransition, error) {
	return m.transitions(ctx,
		`SELECT from_status, to_status, reason, occurred_at FROM intent_transitions
		 WHERE intent_id = $1 ORDER BY occurred_at, id`, intentID)
}

// DepositTransitions returns a deposit's transition history, oldest first.
func (m *ReadModel) DepositTransitions(ctx context.Context, depositID string) ([]StateTransition, error) {
	return m.transitions(ctx,
		`SELECT from_status, to_status, reason, occurred_at FROM deposit_transitions
		 WHERE deposit_id = $1 ORDER BY occurred_at, id`, depositID)
}

func (m *ReadModel) transitions(ctx context.Context, sql, aggregateID string) ([]StateTransition, error) {
	rows, err := m.q.Query(ctx, sql, aggregateID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	defer rows.Close()
	var out []StateTransition
	for rows.Next() {
		var (
			t      StateTransition
			reason *string
		)
		if err := rows.Scan(&t.From, &t.To, &reason, &t.OccurredAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
		}
		if reason != nil {
			t.Reason = *reason
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	return out, nil
}

// --- account search ---------------------------------------------------------

// SearchAccounts pages accounts for operators. The query matches an account id
// or an owner user id exactly; it never matches on email or any other
// personal datum, which lives encrypted in identity_pii and is not this
// endpoint's business.
func (m *ReadModel) SearchAccounts(ctx context.Context, query, cursor string, limit int) (AccountPage, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := decodeCursor(cursor)
	if err != nil {
		return AccountPage{}, err
	}
	sql := `SELECT id, owner_user_id, kind, status, status_reason, frozen_at, cost_basis_method, created_at, updated_at
		FROM accounts WHERE true`
	args := []any{}
	if q := strings.TrimSpace(query); q != "" {
		if _, perr := id.ParseAny(q); perr != nil {
			return AccountPage{}, validationError("q", "q must be an account id or an owner user id")
		}
		args = append(args, q)
		sql += fmt.Sprintf(` AND (id = $%d OR owner_user_id = $%d)`, len(args), len(args))
	}
	if hasCursor {
		args = append(args, c.At, c.ID)
		sql += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	sql += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT %d`, limit+1)

	rows, err := m.q.Query(ctx, sql, args...)
	if err != nil {
		return AccountPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	defer rows.Close()
	var out []accounts.Account
	for rows.Next() {
		var (
			a            accounts.Account
			statusReason *string
			frozenAt     *time.Time
			kind, status string
		)
		if err := rows.Scan(&a.ID, &a.OwnerUserID, &kind, &status, &statusReason, &frozenAt,
			&a.CostBasisMethod, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return AccountPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
		}
		a.Kind = accounts.Kind(kind)
		a.Status = accounts.Status(status)
		if statusReason != nil {
			a.StatusReason = *statusReason
		}
		a.FrozenAt = frozenAt
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return AccountPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	page := AccountPage{Items: out}
	if len(out) > limit {
		page.Items = out[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
	}
	return page, nil
}

// --- admin actions ----------------------------------------------------------

// ListAdminActions pages administrative actions, newest first.
func (m *ReadModel) ListAdminActions(ctx context.Context, status, cursor string, limit int) (AdminActionPage, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := decodeCursor(cursor)
	if err != nil {
		return AdminActionPage{}, err
	}
	sql := `SELECT id, kind, target_type, target_id, params, params_hash, reason, requires_dual, status,
		proposed_by_user_id, proposed_at, proposer_step_up_at, approved_by_user_id, approved_at,
		approver_step_up_at, approval_note, rejected_by_user_id, rejected_at, rejected_reason,
		executed_at, execution_result, execution_error, expires_at, correlation_id, updated_at
		FROM admin_actions WHERE true`
	args := []any{}
	if s := strings.TrimSpace(status); s != "" {
		st := admin.Status(strings.ToUpper(s))
		if !st.Valid() {
			return AdminActionPage{}, validationError("status", "unknown administrative action status")
		}
		args = append(args, string(st))
		sql += fmt.Sprintf(` AND status = $%d`, len(args))
	}
	if hasCursor {
		args = append(args, c.At, c.ID)
		sql += fmt.Sprintf(` AND (proposed_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	sql += fmt.Sprintf(` ORDER BY proposed_at DESC, id DESC LIMIT %d`, limit+1)

	rows, err := m.q.Query(ctx, sql, args...)
	if err != nil {
		return AdminActionPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	defer rows.Close()
	var out []admin.Action
	for rows.Next() {
		var (
			a          admin.Action
			kind, stat string
		)
		if err := rows.Scan(&a.ID, &kind, &a.TargetType, &a.TargetID, &a.Params, &a.ParamsHash,
			&a.Reason, &a.RequiresDual, &stat, &a.ProposedBy, &a.ProposedAt, &a.ProposerStepUpAt,
			&a.ApprovedBy, &a.ApprovedAt, &a.ApproverStepUpAt, &a.ApprovalNote,
			&a.RejectedBy, &a.RejectedAt, &a.RejectedReason,
			&a.ExecutedAt, &a.ExecutionResult, &a.ExecutionError, &a.ExpiresAt,
			&a.CorrelationID, &a.UpdatedAt); err != nil {
			return AdminActionPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
		}
		a.Kind = admin.Kind(kind)
		a.Status = admin.Status(stat)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return AdminActionPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	page := AdminActionPage{Items: out}
	if len(out) > limit {
		page.Items = out[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(keysetCursor{At: last.ProposedAt, ID: last.ID.String()})
	}
	return page, nil
}

// --- kill switches ----------------------------------------------------------

// ListKillSwitches returns every persisted switch, active or not, so an
// operator can see what was released and why.
func (m *ReadModel) ListKillSwitches(ctx context.Context) ([]killswitch.Switch, error) {
	rows, err := m.q.Query(ctx, `
		SELECT id, kind, scope_id, active, severity, reason, activated_by_actor_id, activated_at,
		       released_by_actor_id, released_at, release_approval_id, release_reason, version, updated_at
		FROM kill_switches ORDER BY active DESC, kind, scope_id`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	defer rows.Close()
	var out []killswitch.Switch
	for rows.Next() {
		var (
			sw                        killswitch.Switch
			kind, severity            string
			activatedBy, releasedBy   *string
			approvalID, releaseReason *string
		)
		if err := rows.Scan(&sw.ID, &kind, &sw.ScopeID, &sw.Active, &severity, &sw.Reason,
			&activatedBy, &sw.ActivatedAt, &releasedBy, &sw.ReleasedAt,
			&approvalID, &releaseReason, &sw.Version, &sw.UpdatedAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
		}
		sw.Kind = killswitch.Kind(kind)
		sw.Severity = killswitch.Severity(severity)
		if activatedBy != nil {
			sw.ActivatedBy = *activatedBy
		}
		if releasedBy != nil {
			sw.ReleasedBy = *releasedBy
		}
		if approvalID != nil {
			sw.ReleaseApprovalID = *approvalID
		}
		if releaseReason != nil {
			sw.ReleaseReason = *releaseReason
		}
		out = append(out, sw)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	return out, nil
}

// --- reconciliation ---------------------------------------------------------

// ListReconciliationRecords pages reconciliation records, newest first.
func (m *ReadModel) ListReconciliationRecords(ctx context.Context, status string, accountID *accounts.AccountID, cursor string, limit int) (ReconciliationPage, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := decodeCursor(cursor)
	if err != nil {
		return ReconciliationPage{}, err
	}
	sql := `SELECT id, kind, mode, scope_type, scope_id, account_id, asset_id, expected, observed, difference,
		status, material, blocks_new_risk, opened_at, resolved_at, resolution_reason, resolution_evidence_ref,
		compensating_journal_transaction_id
		FROM reconciliation_records WHERE true`
	args := []any{}
	if s := strings.TrimSpace(status); s != "" {
		args = append(args, strings.ToUpper(s))
		sql += fmt.Sprintf(` AND status = $%d`, len(args))
	}
	if accountID != nil {
		args = append(args, *accountID)
		sql += fmt.Sprintf(` AND account_id = $%d`, len(args))
	}
	if hasCursor {
		args = append(args, c.At, c.ID)
		sql += fmt.Sprintf(` AND (opened_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	sql += fmt.Sprintf(` ORDER BY opened_at DESC, id DESC LIMIT %d`, limit+1)

	rows, err := m.q.Query(ctx, sql, args...)
	if err != nil {
		return ReconciliationPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	defer rows.Close()
	var out []ReconciliationRecord
	for rows.Next() {
		var (
			r                              ReconciliationRecord
			acct, asset, comp              *string
			expected, observed, difference []byte
			reason, evidence               *string
		)
		if err := rows.Scan(&r.ID, &r.Kind, &r.Mode, &r.ScopeType, &r.ScopeID, &acct, &asset,
			&expected, &observed, &difference, &r.Status, &r.Material, &r.BlocksNewRisk,
			&r.OpenedAt, &r.ResolvedAt, &reason, &evidence, &comp); err != nil {
			return ReconciliationPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
		}
		if acct != nil {
			r.AccountID = *acct
		}
		if asset != nil {
			r.AssetID = *asset
		}
		if comp != nil {
			r.CompensatingJournalTx = *comp
		}
		if reason != nil {
			r.ResolutionReason = *reason
		}
		if evidence != nil {
			r.ResolutionEvidenceRef = *evidence
		}
		r.Expected, _ = decodeObject(expected)
		r.Observed, _ = decodeObject(observed)
		r.Difference, _ = decodeObject(difference)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return ReconciliationPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	page := ReconciliationPage{Items: out}
	if len(out) > limit {
		page.Items = out[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(keysetCursor{At: last.OpenedAt, ID: last.ID})
	}
	return page, nil
}

// --- wallets ----------------------------------------------------------------

// Wallet returns the account's active wallet id and address. It is the
// destination a funding session must deliver to and the source a withdrawal
// must not be sent to; both are resolved from the registry, never from the
// client. An account with no active wallet yields empty strings.
func (m *ReadModel) Wallet(ctx context.Context, accountID accounts.AccountID) (walletID, address string, err error) {
	err = m.q.QueryRow(ctx, `
		SELECT id::text, address FROM wallets
		WHERE account_id = $1 AND status = 'ACTIVE'
		ORDER BY created_at LIMIT 1`, accountID).Scan(&walletID, &address)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	return walletID, address, nil
}

// CustodyAddress returns the account's active wallet address, which is the
// custody location shown against every holding. An account with no wallet
// yields the empty string rather than an invented location.
func (m *ReadModel) CustodyAddress(ctx context.Context, accountID accounts.AccountID) (string, error) {
	var address string
	err := m.q.QueryRow(ctx, `
		SELECT address FROM wallets
		WHERE account_id = $1 AND status = 'ACTIVE'
		ORDER BY created_at LIMIT 1`, accountID).Scan(&address)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	return address, nil
}

// --- activity ---------------------------------------------------------------

// Activity pages the account's timeline: intents, eligibility and risk
// decisions, orders, fills, funding transitions, ledger postings and
// reconciliation records, newest first. Every row is a fact that already
// happened; nothing is derived here.
func (m *ReadModel) Activity(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) (ActivityPage, error) {
	limit = clampLimit(limit)
	c, hasCursor, err := decodeCursor(cursor)
	if err != nil {
		return ActivityPage{}, err
	}
	var (
		cursorAt time.Time
		cursorID string
	)
	if hasCursor {
		cursorAt, cursorID = c.At, c.ID
	} else {
		// A far-future bound keeps one SQL statement for both pages.
		cursorAt = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
		cursorID = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	}

	const sql = `
WITH items AS (
    SELECT it.occurred_at AS occurred_at, it.id AS id, 'INTENT' AS kind,
           ('intent ' || it.from_status || ' → ' || it.to_status) AS summary,
           ''::text AS correlation_id,
           jsonb_build_object('intent_id', it.intent_id::text) AS refs
    FROM intent_transitions it JOIN trade_intents ti ON ti.id = it.intent_id
    WHERE ti.account_id = $1
  UNION ALL
    SELECT ot.occurred_at, ot.id, 'EXECUTION',
           ('order ' || ot.from_status || ' → ' || ot.to_status),
           coalesce(o.correlation_id, ''),
           jsonb_build_object('order_id', ot.order_id::text, 'intent_id', o.intent_id::text)
    FROM order_transitions ot JOIN orders o ON o.id = ot.order_id
    WHERE o.account_id = $1
  UNION ALL
    SELECT f.observed_at, f.id, 'FILL',
           ('fill on ' || f.venue || ' (' || f.finality || ')'),
           '',
           jsonb_build_object('order_id', f.order_id::text, 'fill_id', f.id::text)
    FROM fills f WHERE f.account_id = $1
  UNION ALL
    SELECT dt.occurred_at, dt.id, 'FUNDING',
           ('deposit ' || dt.from_status || ' → ' || dt.to_status),
           coalesce(dt.correlation_id, ''),
           jsonb_build_object('deposit_id', dt.deposit_id::text)
    FROM deposit_transitions dt JOIN deposits d ON d.id = dt.deposit_id
    WHERE d.account_id = $1
  UNION ALL
    SELECT jt.posted_at, jt.id, 'LEDGER',
           ('journal transaction ' || jt.kind),
           coalesce(jt.correlation_id, ''),
           jsonb_build_object('journal_transaction_id', jt.id::text)
    FROM journal_transactions jt
    WHERE EXISTS (
        SELECT 1 FROM journal_entries je JOIN ledger_accounts la ON la.id = je.ledger_account_id
        WHERE je.transaction_id = jt.id AND la.owner_type = 'CUSTOMER' AND la.owner_id = $5)
  UNION ALL
    SELECT rr.opened_at, rr.id, 'RECONCILIATION',
           ('reconciliation ' || rr.kind || ' ' || rr.status),
           coalesce(rr.correlation_id, ''),
           jsonb_build_object('record_id', rr.id::text)
    FROM reconciliation_records rr WHERE rr.account_id = $1
)
SELECT occurred_at, id, kind, summary, correlation_id, refs
FROM items
WHERE (occurred_at, id) < ($2, $3)
ORDER BY occurred_at DESC, id DESC
LIMIT $4`

	rows, err := m.q.Query(ctx, sql, accountID, cursorAt, cursorID, limit+1, accountID.String())
	if err != nil {
		return ActivityPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	defer rows.Close()

	var out []ActivityItem
	for rows.Next() {
		var (
			it   ActivityItem
			refs []byte
		)
		if err := rows.Scan(&it.OccurredAt, &it.ID, &it.Kind, &it.Summary, &it.CorrelationID, &refs); err != nil {
			return ActivityPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
		}
		if obj, derr := decodeObject(refs); derr == nil {
			it.References = make(map[string]string, len(obj))
			for k, v := range obj {
				if sv, ok := v.(string); ok {
					it.References[k] = sv
				}
			}
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return ActivityPage{}, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	page := ActivityPage{Items: out}
	if len(out) > limit {
		page.Items = out[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(keysetCursor{At: last.OccurredAt, ID: last.ID})
	}
	return page, nil
}

// AssetRef is the minimal asset metadata the holdings view needs.
type AssetRef struct {
	Symbol      string
	Chain       string
	Mint        string
	Decimals    uint8
	AssetStatus assets.Status
}

// AssetRefs loads display metadata for a set of assets in one query.
func (m *ReadModel) AssetRefs(ctx context.Context, ids []assets.AssetID) (map[assets.AssetID]AssetRef, error) {
	out := make(map[assets.AssetID]AssetRef, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := m.q.Query(ctx,
		`SELECT id, symbol, chain, mint_address, decimals, status FROM assets WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	defer rows.Close()
	for rows.Next() {
		var (
			aid      assets.AssetID
			ref      AssetRef
			decimals int16
			status   string
		)
		if err := rows.Scan(&aid, &ref.Symbol, &ref.Chain, &ref.Mint, &decimals, &status); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
		}
		ref.Decimals = uint8(decimals) //nolint:gosec // assets.decimals is CHECKed to 0..38
		ref.AssetStatus = assets.Status(status)
		out[aid] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	return out, nil
}
