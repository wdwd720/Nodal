//go:build integration

package profile_test

// Adversarial audit (goal §54), area accounts-auth. These demonstrated defects;
// they are the regressions for the fixes now (F-174, F-176, F-177, F-178, F-181).

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/profile"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// migrateConn opens a connection as the MIGRATION role.
//
// Two of the blockers below live in tables the application role cannot write --
// native_positions is SELECT-only (00772) and a credit lot must be backed by a
// journal transaction (00711) -- and seeding them through their real producers
// would mean standing up a market and the whole double-entry ledger to prove a
// three-line SELECT reads the right column. The rows are therefore written as
// the owner, which is what the owner is for.
func migrateConn(t *testing.T) *pgx.Conn {
	t.Helper()
	url := os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_MIGRATE_DATABASE_URL not set; skipping")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close(ctx) })
	return c
}

// creditAssetID returns the deployment's Credit asset, creating it through the
// asset registry if this database has none. The schema permits exactly one
// (assets_single_credit_asset), and the registry's own constructor is used
// rather than a hand-written INSERT that would drift from it.
func creditAssetID(t *testing.T, f *fixture) assets.AssetID {
	t.Helper()
	ctx := context.Background()
	var existing assets.AssetID
	if err := f.db.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing); err == nil {
		return existing
	}
	created, err := assets.NewRepository().Create(ctx, f.db, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindCredit,
		ValueDomain: valuedomain.InternalCredit,
		Symbol:      "CREDIT", Name: "Nodal Credit", Decimals: 6,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	return created.ID
}

// seedAsset writes one ordinary asset row and returns its id.
func seedAsset(t *testing.T, f *fixture) string {
	t.Helper()
	assetID := id.New[id.Any]().String()
	_, err := f.db.Pool().Exec(context.Background(),
		`INSERT INTO assets (id, chain, mint_address, kind, symbol, name, decimals, risk_class, status, value_domain)
		 VALUES ($1, 'solana-devnet', $2, 'SPL_TOKEN', 'TST', 'Test asset', 6, 'STANDARD', 'ACTIVE', 'SELF_CUSTODIAL_CRYPTO')`,
		assetID, "mint-"+assetID)
	require.NoError(t, err)
	return assetID
}

// seedCreditLot issues qty Credits to an account the way the ledger requires: a
// balanced journal transaction with an entry on the account's own CREDIT_BALANCE,
// and a lot backed by it. cp_credit_lot_open (00711) refuses a lot that no
// journal transaction backs, so this seeds the whole chain rather than switching
// the guard off -- and credit_lot_state, which the closure check reads, is
// therefore produced by exactly the trigger that produces it in production.
func seedCreditLot(t *testing.T, f *fixture, accountID, assetID string, qty int) {
	t.Helper()
	ctx := context.Background()
	customerLedger, platformLedger := id.New[id.Any]().String(), id.New[id.Any]().String()
	// One transaction, because the balanced-transaction rule is a DEFERRED
	// constraint trigger: a header committed on its own is a transaction with
	// no entries, which is what it exists to refuse.
	require.NoError(t, f.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		exec := func(sql string, args ...any) {
			t.Helper()
			_, err := tx.Exec(ctx, sql, args...)
			require.NoError(t, err, sql)
		}
		// The sides are the chart's, which ledger_accounts_match_chart (00717)
		// pins: CREDIT_BALANCE is debit-normal and CREDIT_ISSUANCE is its credit
		// -normal counterpart, and neither may run negative.
		exec(`INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side)
	      VALUES ($1, 'CUSTOMER', $2, 'CREDIT_BALANCE', $3, 'DEBIT')`, customerLedger, accountID, assetID)
		exec(`INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side)
	      VALUES ($1, 'PLATFORM', $2, 'CREDIT_ISSUANCE', $3, 'CREDIT')`,
			platformLedger, id.New[id.Any]().String(), assetID)

		txID := id.New[id.Any]().String()
		exec(`INSERT INTO journal_transactions
	      (id, kind, idempotency_key, reference_type, reference_id, effective_at,
	       posted_by_actor_type, posted_by_actor_id, content_hash)
	      VALUES ($1, 'SEED', $2, 'test', $3, now(), 'SYSTEM', 'closure-blocker-test', $4)`,
			txID, "idem-"+txID, txID, []byte{0})
		exec(`INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
	      VALUES ($1, $2, 0, $3, $4, 'DEBIT', $5)`, id.New[id.Any](), txID, customerLedger, assetID, qty)
		exec(`INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
	      VALUES ($1, $2, 1, $3, $4, 'CREDIT', $5)`, id.New[id.Any](), txID, platformLedger, assetID, qty)

		exec(`INSERT INTO credit_lots
	      (id, account_id, asset_id, origin, initial_finality, quantity, journal_transaction_id,
	       issued_by_actor_type, issued_by_actor_id)
	      VALUES ($1, $2, $3, 'PROMOTIONAL', 'SETTLED', $4, $5, 'SYSTEM', 'closure-blocker-test')`,
			id.New[id.Any](), accountID, assetID, qty, txID)
		return nil
	}))
}

// F-174. Migration 00758 said the cooling-off period is "enforced by the trigger
// below, not only by the service, because the whole point of a cooling-off
// period is that it survives a bug in code that is in a hurry."
//
// It did not. cp_closure_request_apply_transition compared `NEW.occurred_at` --
// a column the INSERTing role chooses -- instead of the transaction's own clock,
// so a caller that stamped the row in the future effected the closure at once
// and the wall clock was never consulted.
//
// 00798 measures the wait against statement_timestamp() and bounds occurred_at
// to two minutes either side of it, so both halves of the forgery are refused.
func TestAudit_ClosureCoolingOffIsMeasuredAgainstACallerSuppliedTimestamp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, _, _ := f.newUser(t)

	view, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)
	require.NotNil(t, view.Closure)
	requestID := view.Closure.ID

	// The wall clock is nowhere near the end of the wait.
	var stillCooling bool
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT now() < cooling_off_until FROM account_closure_requests WHERE id = $1`, requestID).
		Scan(&stillCooling))
	require.True(t, stillCooling, "the fixture must still be inside the cooling-off period")

	// One INSERT, stamped whenever the caller likes.
	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO account_closure_request_transitions
		 (id, request_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		 VALUES ($1, $2, 'PENDING', 'EFFECTED', 'OPERATOR', 'forged', 'in a hurry', now() + interval '400 days')`,
		id.New[id.Any](), requestID)
	require.Error(t, err, "the database accepted an EFFECTED transition inside the cooling-off period")
	assert.Equal(t, "AD001", db.SQLState(err), "got %v", err)
	assert.Contains(t, err.Error(), "TRANSITION_STAMP_SKEWED",
		"the stamp is refused before the wait is even consulted: occurred_at is an audit record, not an argument")

	// And an honestly-stamped one inside the wait is refused by the wait
	// itself, which is the invariant 00758 claimed and 00798 makes true.
	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO account_closure_request_transitions
		 (id, request_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		 VALUES ($1, $2, 'PENDING', 'EFFECTED', 'OPERATOR', 'forged', 'in a hurry', now())`,
		id.New[id.Any](), requestID)
	require.Error(t, err)
	assert.Equal(t, "AD001", db.SQLState(err), "got %v", err)
	assert.Contains(t, err.Error(), "CLOSURE_STILL_COOLING")

	var state string
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT state FROM account_closure_requests WHERE id = $1`, requestID).Scan(&state))
	assert.Equal(t, "PENDING", state, "the cooling-off period is a database invariant, not a service convention")
}

// F-176. internal/profile declares the closure edge set (closureTransitions:
// PENDING is the only origin; CANCELLED, REFUSED and EFFECTED are terminal) and
// CanCloseTransition enforced it in Go. The schema enforced only that a
// transition row DESCRIBES the change it makes (00726's edge binding), never
// that the change was legal, so the application role could walk a terminal
// request onto another state -- and decided_at/decided_reason are coalesced, so
// the row kept the reason given for the decision it no longer recorded.
//
// 00798 puts the edge set in a CHECK on the transition table.
func TestAudit_TheClosureStateMachineHasNoEdgeSetInTheDatabase(t *testing.T) {
	f := newEffectableFixture(t)
	ctx := context.Background()
	actor, _, _ := f.newUser(t)
	op := f.operator(t)

	view, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)
	require.NotNil(t, view.Closure)
	requestID := view.Closure.ID

	f.passCoolingOff(t)
	admin, err := f.svc.Decide(ctx, op, actor.UserID, profile.DecisionEffect, "the cooling-off period has passed")
	require.NoError(t, err)
	require.NotNil(t, admin.Closure)
	require.Equal(t, profile.ClosureEffected, admin.Closure.State)
	require.True(t, admin.Closure.State.Terminal(), "EFFECTED is terminal in Go")
	require.False(t, profile.CanCloseTransition(profile.ClosureEffected, profile.ClosureCancelled),
		"Go refuses EFFECTED -> CANCELLED")

	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO account_closure_request_transitions
		 (id, request_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		 VALUES ($1, $2, 'EFFECTED', 'CANCELLED', 'USER', 'forged', 'undo a terminal decision', now())`,
		id.New[id.Any](), requestID)
	require.Error(t, err, "the database accepted an edge the state machine forbids")
	assert.Contains(t, err.Error(), "account_closure_request_transitions_edge_check")

	var state, decidedReason string
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT state, coalesce(decided_reason,'') FROM account_closure_requests WHERE id = $1`, requestID).
		Scan(&state, &decidedReason))
	assert.Equal(t, "EFFECTED", state, "a terminal request moved")
	assert.Equal(t, "the cooling-off period has passed", decidedReason,
		"and the row still says EFFECTED for the reason the EFFECT was given")
}

// F-177. Migration 00757: "CLOSED is terminal: there is no edge out of it, here
// or in Go. ... Nothing in the product needs it today, so the schema does not
// quietly permit it." The schema permitted it: cp_user_apply_status_transition
// wrote whatever to_status the row named, and the edge binding only required the
// row to describe the change, not to be a legal one.
//
// 00798 puts the four legal edges in a CHECK, and internal/profile refuses the
// same move before it reaches the table.
func TestAudit_AClosedUserCanBeReopenedByTheApplicationRole(t *testing.T) {
	f := newEffectableFixture(t)
	ctx := context.Background()
	actor, user, _ := f.newUser(t)
	op := f.operator(t)

	_, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)
	f.passCoolingOff(t)
	admin, err := f.svc.Decide(ctx, op, actor.UserID, profile.DecisionEffect, "the cooling-off period has passed")
	require.NoError(t, err)
	require.Equal(t, "CLOSED", admin.UserStatus)

	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO user_status_transitions
		 (id, user_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		 VALUES ($1, $2, 'CLOSED', 'ACTIVE', 'OPERATOR', 'forged', 'reopen a closed account', now())`,
		id.New[id.Any](), user.ID)
	require.Error(t, err, "the database accepted CLOSED -> ACTIVE")
	assert.Contains(t, err.Error(), "user_status_transitions_edge_check")

	var status string
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, user.ID).Scan(&status))
	assert.Equal(t, "CLOSED", status, "a terminal user status moved")
	assert.False(t, profile.CanUserStatusTransition("CLOSED", "ACTIVE"), "and Go says the same thing")
}

// F-181 (service half). Decide(EFFECT) closed the account without consulting
// anything financial. The nearest thing to a check was the cooling-off period;
// the three conditions 00758 and closure.go name as the reason REFUSED exists --
// an unsettled payout, an open dispute, a balance to deal with first -- were
// enforced nowhere and reported nowhere.
//
// They are read now, in one statement, by the same call that fills the operator's
// view, so the surface and the refusal cannot disagree.
func TestAudit_EffectingAClosureChecksNothingFinancial(t *testing.T) {
	f := newEffectableFixture(t)
	ctx := context.Background()
	actor, user, acct := f.newUser(t)
	op := f.operator(t)

	_, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)

	// What the operator is shown before deciding.
	before, err := f.svc.AdminUser(ctx, actor.UserID)
	require.NoError(t, err)
	require.NotNil(t, before.Closure)
	require.Len(t, before.Accounts, 1)
	assert.True(t, before.Blockers.Clear(), "a fresh account holds nothing")
	assert.Equal(t, "0", before.Blockers.CreditBalance)

	// One unsettled payout is enough. It is born ELIGIBILITY_CHECK (00737):
	// reservation, submission and settlement are each a transition, and a
	// request sitting at the start is exactly what this blocker is about.
	creditAsset := creditAssetID(t, f)
	payoutID := id.New[id.Any]().String()
	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO payout_requests (id, account_id, credit_asset_id, state, requested_quantity,
		     policy_version, policy_hash, idempotency_key)
		 VALUES ($1, $2, $3, 'ELIGIBILITY_CHECK', 500, 'v1', 'hash', $4)`,
		payoutID, acct.ID, creditAsset, "idem-"+payoutID)
	require.NoError(t, err)

	shown, err := f.svc.AdminUser(ctx, actor.UserID)
	require.NoError(t, err)
	assert.False(t, shown.Blockers.Clear())
	assert.Equal(t, 1, shown.Blockers.OpenPayoutRequests)
	require.Len(t, shown.Blockers.Reasons(), 1)
	assert.Contains(t, shown.Blockers.Reasons()[0], "payout request")

	f.passCoolingOff(t)
	_, err = f.svc.Decide(ctx, op, actor.UserID, profile.DecisionEffect, "the cooling-off period has passed")
	require.Error(t, err, "nothing refused the closure")
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "payout request",
		"the refusal names the blocker, so the operator can tell the person what to do about it")

	// Nothing moved.
	var userStatus, acctStatus string
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, user.ID).Scan(&userStatus))
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1`, acct.ID).Scan(&acctStatus))
	assert.Equal(t, "ACTIVE", userStatus)
	assert.Equal(t, "ACTIVE", acctStatus)

	// REFUSE is still available, and is the decision the blocker asks for: the
	// person is shown the reason and can ask again once it is dealt with.
	refused, err := f.svc.Decide(ctx, op, actor.UserID, profile.DecisionRefuse,
		"a payout is still in flight; ask again once it has settled")
	require.NoError(t, err)
	require.NotNil(t, refused.Closure)
	assert.Equal(t, profile.ClosureRefused, refused.Closure.State)
}

// F-181, blocker two: a Credit balance.
func TestAudit_EffectingAClosureIsRefusedWhileCreditsRemain(t *testing.T) {
	f := newEffectableFixture(t)
	ctx := context.Background()
	actor, _, acct := f.newUser(t)
	op := f.operator(t)

	_, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)

	seedCreditLot(t, f, acct.ID.String(), creditAssetID(t, f).String(), 1200)

	shown, err := f.svc.AdminUser(ctx, actor.UserID)
	require.NoError(t, err)
	assert.False(t, shown.Blockers.Clear())
	assert.Equal(t, "1200", shown.Blockers.CreditBalance,
		"the gross balance is the sum of remaining lot quantities, which is what credit.Balances calls Gross")

	f.passCoolingOff(t)
	_, err = f.svc.Decide(ctx, op, actor.UserID, profile.DecisionEffect, "the cooling-off period has passed")
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "1200 Credits")
}

// F-181, blocker three: an open native position.
func TestAudit_EffectingAClosureIsRefusedWhileAPositionIsOpen(t *testing.T) {
	f := newEffectableFixture(t)
	ctx := context.Background()
	actor, _, acct := f.newUser(t)
	op := f.operator(t)

	_, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)

	assetID := seedAsset(t, f)
	_, err = f.db.Pool().Exec(ctx, `INSERT INTO native_assets
		(asset_id, creator_account_id, name, symbol, status, max_supply, creator_allocation)
		VALUES ($1, $2, 'Test native asset', 'TSTN', 'DRAFT', 1000000, 0)`, assetID, acct.ID)
	require.NoError(t, err)

	owner := migrateConn(t)
	_, err = owner.Exec(ctx, `INSERT INTO native_positions
		(account_id, asset_id, quantity, allocation_units) VALUES ($1, $2, 42, 42)`, acct.ID, assetID)
	require.NoError(t, err)

	shown, err := f.svc.AdminUser(ctx, actor.UserID)
	require.NoError(t, err)
	assert.False(t, shown.Blockers.Clear())
	assert.Equal(t, 1, shown.Blockers.OpenNativePositions)

	f.passCoolingOff(t)
	_, err = f.svc.Decide(ctx, op, actor.UserID, profile.DecisionEffect, "the cooling-off period has passed")
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "native position")
}

// F-178. operator_roles is the only source of operator authority in the system
// (ADR-0022, ADR-0024) and cp_app held blanket UPDATE on it from 00010 -- the
// one authority-bearing table that never got the treatment 00744 gave accounts,
// 00757 gave users and 00758 gave account_closure_requests. Nothing in Go ever
// updated it, so the grant served nothing, and with it a revocation did not stay
// revoked and a role could be rewritten in place while granted_by, granted_at
// and reason kept describing the grant that was made.
//
// 00799 takes the grant away, gives the directory the transition table every
// other authority-bearing table has, and makes revocation one-way.
func TestAudit_TheOperatorDirectoryIsRewritableByTheApplicationRole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, user, _ := f.newUser(t)

	_, err := f.db.Pool().Exec(ctx,
		`INSERT INTO operator_roles (user_id, role, reason) VALUES ($1, 'SUPPORT_READ_ONLY', 'support desk')`, user.ID)
	require.NoError(t, err)

	// The bare UPDATE the application used to hold.
	_, err = f.db.Pool().Exec(ctx,
		`UPDATE operator_roles SET revoked_at = now() WHERE user_id = $1`, user.ID)
	require.Error(t, err, "the application role can still write the operator directory")
	assert.Equal(t, "42501", db.SQLState(err), "got %v", err)

	// A revocation is a row, and the trigger makes the change.
	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO operator_role_transitions (id, user_id, role, action, actor_type, actor_id, reason)
		 VALUES ($1, $2, 'SUPPORT_READ_ONLY', 'REVOKE', 'OPERATOR', 'an operator', 'left the support desk')`,
		id.New[id.Any](), user.ID)
	require.NoError(t, err)

	var revoked *time.Time
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT revoked_at FROM operator_roles WHERE user_id = $1`, user.ID).Scan(&revoked))
	require.NotNil(t, revoked, "the transition row did not revoke the grant")

	// And the application cannot put it back, or promote it, by any route.
	_, err = f.db.Pool().Exec(ctx,
		`UPDATE operator_roles SET revoked_at = NULL, role = 'ADMIN', expires_at = NULL WHERE user_id = $1`, user.ID)
	require.Error(t, err, "ADR-0024 says a revoked grant stays revoked")
	assert.Equal(t, "42501", db.SQLState(err), "got %v", err)

	// Nor can the owner: the invariant is the schema's, not a grant's.
	owner := migrateConn(t)
	_, err = owner.Exec(ctx, `UPDATE operator_roles SET revoked_at = NULL WHERE user_id = $1`, user.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OPERATOR_ROLE_REVOKED")
	_, err = owner.Exec(ctx, `UPDATE operator_roles SET role = 'ADMIN' WHERE user_id = $1`, user.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OPERATOR_ROLE_PROVENANCE")

	var role, reason string
	var grantedBy *string
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT role, coalesce(reason,''), granted_by::text FROM operator_roles WHERE user_id = $1`, user.ID).
		Scan(&role, &reason, &grantedBy))
	assert.Equal(t, "SUPPORT_READ_ONLY", role, "a SUPPORT_READ_ONLY row became ADMIN in place")
	assert.Equal(t, "support desk", reason,
		"and the provenance columns still describe the grant that was actually made")

	// The movement is recorded, which is the half that did not exist at all.
	var transitions int
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM operator_role_transitions WHERE user_id = $1`, user.ID).Scan(&transitions))
	assert.Equal(t, 1, transitions, "operator_roles has no transition table")
}
