//go:build integration

package notifications_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/notifications"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The two sources the product domains left for the follower (D-082),
// and the two claims that needed proving rather than assuming: that a circuit
// breaker already notifies through the market transition it writes, and that
// closing an account already notifies through the account transition.

func mustExec(t *testing.T, d *db.DB, sql string, args ...any) {
	t.Helper()
	_, err := d.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

func uuidText() string { return id.New[id.Any]().String() }

// anAgent creates the rows an agent needs to exist: a strategy and the agent
// itself, born DRAFT (00736 refuses anything else).
func anAgent(t *testing.T, d *db.DB, owner accounts.UserID, acct accounts.AccountID) string {
	t.Helper()
	strategyID := uuidText()
	mustExec(t, d, `INSERT INTO strategies
		(id, owner_account_id, owner_user_id, name, source_kind, status, created_by_actor_type, created_by_actor_id)
		VALUES ($1::uuid, $2, $3, $4, 'NATURAL_LANGUAGE', 'ACTIVE', 'USER', $5)`,
		strategyID, acct, owner, "strategy-"+strategyID[:8], owner.String())
	agentID := uuidText()
	mustExec(t, d, `INSERT INTO agents
		(id, account_id, strategy_id, name, stage, state, created_by_actor_type, created_by_actor_id)
		VALUES ($1::uuid, $2, $3::uuid, 'an agent', 'DRAFT', 'DRAFT', 'USER', $4)`,
		agentID, acct, strategyID, owner.String())
	return agentID
}

// aMarketWithAFillBy creates a native market this account has traded, which is
// the population the market-pause source notifies.
func aMarketWithAFillBy(t *testing.T, d *db.DB, acct accounts.AccountID) (marketID, symbol string) {
	t.Helper()
	ctx := context.Background()
	repo := assets.NewRepository()
	credit := creditAsset(t, d)
	symbol = "ORB" + uuidText()[:4]
	native, err := repo.Create(ctx, d, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindNativeAsset,
		ValueDomain: valuedomain.InternalNativeAsset,
		Symbol:      symbol, Name: "Orb test", Decimals: 6,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	mustExec(t, d, `INSERT INTO native_assets
		(asset_id, creator_account_id, symbol, name, description, max_supply, status)
		VALUES ($1, $2, $3, 'Orb test', '', 1000000, 'DRAFT')`, native.ID, acct, symbol)
	marketID = uuidText()
	mustExec(t, d, `INSERT INTO native_markets
		(id, asset_id, credit_asset_id, virtual_credit_reserve, initial_asset_reserve, status)
		VALUES ($1::uuid, $2, $3, 1000000, 1000000, 'PENDING')`, marketID, native.ID, credit)
	// A market is born PENDING and reaches ACTIVE through a transition row
	// (00746); a fill against a PENDING market is refused by the trigger, which
	// is the schema saying what this test would otherwise have faked.
	mustExec(t, d, `INSERT INTO native_market_transitions
		(id, market_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1::uuid, $2::uuid, 'PENDING', 'ACTIVE', 'OPERATOR', 'itest', 'the market opened', now())`,
		uuidText(), marketID)
	// A fill is what makes this account a participant, and a fill names a
	// journal transaction. The ledger's own constraint refuses a transaction
	// with no entries or with entries that do not balance per asset, so the
	// transaction below is a real balanced pair rather than a stub -- the
	// invariant is not this test's to weaken.
	txID := uuidText()
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions
			(id, kind, idempotency_key, reference_type, reference_id, effective_at,
			 posted_by_actor_type, posted_by_actor_id, content_hash)
			VALUES ($1::uuid, 'TRADE_FILL', $2, 'native_market_fill', $1, now(), 'SYSTEM', 'itest', decode('00', 'hex'))`,
			txID, "itest-"+txID); err != nil {
			return err
		}
		debit := ledgerAccount(t, tx, "CUSTOMER", acct.String(), "TRADING_OUTFLOW", credit, "DEBIT")
		creditSide := ledgerAccount(t, tx, "CUSTOMER", acct.String(), "CAPITAL", credit, "CREDIT")
		for seq, e := range []struct {
			account string
			side    string
		}{{debit, "DEBIT"}, {creditSide, "CREDIT"}} {
			if _, err := tx.Exec(ctx, `INSERT INTO journal_entries
				(id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
				VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6, 100)`,
				uuidText(), txID, seq, e.account, credit, e.side); err != nil {
				return err
			}
		}
		return nil
	}))
	mustExec(t, d, `INSERT INTO native_market_fills
		(id, market_id, seq, account_id, side, credits_in, assets_out, credits_to_pool,
		 state_version_before, real_credit_reserve_after, asset_reserve_after,
		 journal_transaction_id, idempotency_key)
		-- 100 Credits in against a 1,000,000/1,000,000 curve leaves the asset
		-- reserve at ceil(k/(C+100)) = 999,901. Rounding the other way lowers
		-- the constant product, which the trigger refuses -- correctly.
		VALUES ($1::uuid, $2::uuid, 1, $3, 'BUY', 100, 99, 100, 0, 100, 999901, $4::uuid, $5)`,
		uuidText(), marketID, acct, txID, "fill-"+txID)
	return marketID, symbol
}

// ledgerAccount returns the id of one ledger account, creating it if absent.
func ledgerAccount(t *testing.T, tx pgx.Tx, ownerType, ownerID, code string, asset assets.AssetID, side string) string {
	t.Helper()
	var out string
	err := tx.QueryRow(context.Background(), `INSERT INTO ledger_accounts
		(id, owner_type, owner_id, code, asset_id, normal_side)
		VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6)
		ON CONFLICT (owner_type, owner_id, code, asset_id) DO UPDATE SET status = ledger_accounts.status
		RETURNING id::text`, uuidText(), ownerType, ownerID, code, asset, side).Scan(&out)
	require.NoError(t, err)
	return out
}

func creditAsset(t *testing.T, d *db.DB) assets.AssetID {
	t.Helper()
	ctx := context.Background()
	var existing assets.AssetID
	if err := d.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing); err == nil {
		return existing
	}
	created, err := assets.NewRepository().Create(ctx, d, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindCredit,
		ValueDomain: valuedomain.InternalCredit,
		Symbol:      "CREDIT", Name: "Nodal Credit", Decimals: 6,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	return created.ID
}

// TestIntegration_AVerificationDecisionTellsThePersonItIsAbout (D-082).
//
// The source row is the transition the schema says is the only way a
// verification state moves (00761), so a notification exists for a decision
// that happened and for nothing else.
func TestIntegration_AVerificationDecisionTellsThePersonItIsAbout(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	f := newFollower()
	ctx := context.Background()

	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)

	mustExec(t, d, `INSERT INTO compliance_profiles (user_id, identity_state) VALUES ($1, 'UNVERIFIED')`, uid)
	// Every step, because migration 00806 holds the schema to the §20 edge set:
	// nothing reaches VERIFIED except from PENDING (F-224). Only the last row is
	// news, which is what the assertion below is about.
	for _, edge := range [][2]string{{"UNVERIFIED", "STARTED"}, {"STARTED", "PENDING"}} {
		mustExec(t, d, `INSERT INTO compliance_profile_transitions
			(id, user_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
			VALUES ($1::uuid, $2, $3, $4, 'SYSTEM', 'verify_sandbox', 'the session moved', now())`,
			uuidText(), uid, edge[0], edge[1])
	}
	mustExec(t, d, `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		VALUES ($1::uuid, $2, 'PENDING', 'VERIFIED', 'SYSTEM', 'verify_sandbox', 'the provider decided', now())`,
		uuidText(), uid)

	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	require.Equal(t, 1, n, "a session in flight is not news; the decision is")
	got := rec.notified[0]
	assert.Equal(t, notifications.KindVerificationUpdated, got.Kind)
	assert.Equal(t, uid, got.UserID)
	assert.Equal(t, "Your identity is verified", got.Title)
	assert.Equal(t, notifications.Ref{Type: "compliance_profile", ID: uid.String()}, got.Ref)
	assert.Nil(t, got.AccountID,
		"verification is a property of a person, so the notification names no account")
	var data map[string]any
	require.NoError(t, json.Unmarshal(got.Data, &data))
	assert.Equal(t, "VERIFIED", data["to_state"])
	assert.Equal(t, false, data["sandbox"])
	assert.NotContains(t, got.Body, "verify_sandbox", "a provider is not named to the customer")

	// An expired decision and a rejected one land in the same inbox a year
	// apart, and one of them is a judgement about the person while the other is
	// a clock (D-061).
	mustExec(t, d, `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		VALUES ($1::uuid, $2, 'VERIFIED', 'EXPIRED', 'SYSTEM', 'verification:expiry-sweep', 'the window elapsed', now())`,
		uuidText(), uid)
	rec2 := &recorder{}
	n, err = f.RunOnce(ctx, d, rec2)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Contains(t, rec2.notified[0].Body, "not a rejection")
	assert.Equal(t, 2, countFor(t, d, uid))

	// And the lap re-reads both and writes neither again.
	n, err = f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, 2, countFor(t, d, uid))
}

// TestIntegration_AProviderSessionInFlightTellsNobody: STARTED and PENDING are
// the machinery of a check the person is standing in front of, and a follower
// that reported every internal move would bury the one message that matters.
func TestIntegration_AProviderSessionInFlightTellsNobody(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	f := newFollower()
	ctx := context.Background()

	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)

	mustExec(t, d, `INSERT INTO compliance_profiles (user_id, identity_state) VALUES ($1, 'UNVERIFIED')`, uid)
	for _, edge := range [][2]string{{"UNVERIFIED", "STARTED"}, {"STARTED", "PENDING"}} {
		mustExec(t, d, `INSERT INTO compliance_profile_transitions
			(id, user_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
			VALUES ($1::uuid, $2, $3, $4, 'SYSTEM', 'test', 'a session moved', now())`,
			uuidText(), uid, edge[0], edge[1])
	}

	n, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	assert.Zero(t, n, "a session in flight is not news")
	assert.Zero(t, countFor(t, d, uid))
}

// TestIntegration_ASandboxVerificationSaysItIsARehearsal.
//
// Producer stamps notifications.sandbox from the DEPLOYMENT, so a producer that
// is not a sandbox tier cannot label the row. A rehearsal decision would
// otherwise be indistinguishable from an approval, so it says so in the words.
func TestIntegration_ASandboxVerificationSaysItIsARehearsal(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	f := newFollower()
	ctx := context.Background()

	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)

	mustExec(t, d, `INSERT INTO compliance_profiles (user_id, identity_state) VALUES ($1, 'UNVERIFIED')`, uid)
	sessionID := uuidText()
	mustExec(t, d, `INSERT INTO verification_sessions
		(id, user_id, purpose, provider, status, rules_version, environment, sandbox)
		VALUES ($1::uuid, $2, 'PAYOUT_KYC', 'verify_sandbox', 'CREATED', 'v1', 'TEST', true)`, sessionID, uid)
	for _, edge := range [][2]string{{"UNVERIFIED", "STARTED"}, {"STARTED", "PENDING"}} {
		mustExec(t, d, `INSERT INTO compliance_profile_transitions
			(id, user_id, from_state, to_state, actor_type, actor_id, reason, session_id, occurred_at)
			VALUES ($1::uuid, $2, $3, $4, 'SYSTEM', 'verify_sandbox', 'a rehearsal', $5::uuid, now())`,
			uuidText(), uid, edge[0], edge[1], sessionID)
	}
	mustExec(t, d, `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, actor_type, actor_id, reason, session_id, occurred_at)
		VALUES ($1::uuid, $2, 'PENDING', 'VERIFIED', 'SYSTEM', 'verify_sandbox', 'a rehearsal', $3::uuid, now())`,
		uuidText(), uid, sessionID)

	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Contains(t, rec.notified[0].Body, "SANDBOX")
	assert.Contains(t, rec.notified[0].Body, "not an approval")
	assert.False(t, rec.notified[0].Sandbox,
		"this producer is not a sandbox tier, so the ROW cannot claim to be one; the copy carries the fact")
	var data map[string]any
	require.NoError(t, json.Unmarshal(rec.notified[0].Data, &data))
	assert.Equal(t, true, data["sandbox"])
}

// TestIntegration_OnlySomebodyElsesPauseTellsTheOwner (D-082).
//
// An owner who paused their own agent pressed the button. The one thing they
// cannot know without being told is that somebody else stopped it, so the
// predicate is the pause's own actor type -- and it is the same predicate
// cmd/api's publisher uses, which is what stops the two disagreeing.
func TestIntegration_OnlySomebodyElsesPauseTellsTheOwner(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	agentID := anAgent(t, d, uid, acct)
	f := newFollower()
	ctx := context.Background()

	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)

	ownPause := uuidText()
	mustExec(t, d, `INSERT INTO agent_pauses
		(id, agent_id, reason_code, reason, open_orders_policy, paused_by_actor_type, paused_by_actor_id, paused_at)
		VALUES ($1::uuid, $2::uuid, 'OWNER_REQUEST', 'paused by its owner', 'LEAVE', 'USER', $3, now())`,
		ownPause, agentID, uid.String())

	n, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	assert.Zero(t, n, "a person is not told what they themselves just did")
	assert.Zero(t, countFor(t, d, uid))

	mustExec(t, d, `UPDATE agent_pauses SET resumed_at = now(), resumed_by_actor_type = 'USER',
		resumed_by_actor_id = $2 WHERE id = $1::uuid`, ownPause, uid.String())
	opPause := uuidText()
	mustExec(t, d, `INSERT INTO agent_pauses
		(id, agent_id, reason_code, reason, open_orders_policy, paused_by_actor_type, paused_by_actor_id, paused_at)
		VALUES ($1::uuid, $2::uuid, 'OPERATOR', 'a compliance review', 'LEAVE', 'OPERATOR', 'op-1', now())`,
		opPause, agentID)

	rec := &recorder{}
	n, err = f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got := rec.notified[0]
	assert.Equal(t, notifications.KindAgentPaused, got.Kind)
	assert.Equal(t, uid, got.UserID)
	require.NotNil(t, got.AccountID)
	assert.Equal(t, acct, *got.AccountID)
	assert.Equal(t, notifications.AgentPauseRef(opPause), got.Ref)
	assert.Equal(t, "Your agent was paused by Nodal", got.Title)
	assert.Contains(t, got.Body, "a compliance review")
	assert.NotContains(t, got.Body, "op-1", "the operator who acted is not named to the customer")
	assert.Equal(t, notifications.SeverityWarn, got.Severity)

	n, err = f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, 1, countFor(t, d, uid))
}

// TestIntegration_ABreakerTripTellsTheMarketsTraders.
//
// The circuit breaker pauses a market the way every other status change
// happens: a transition row and the trigger that writes status from it
// (internal/nativemarket.checkBreaker calls SetStatus). So the follower's
// existing native_market_transitions source IS the breaker's notification, and
// this asserts it rather than adding a second source that would duplicate it.
func TestIntegration_ABreakerTripTellsTheMarketsTraders(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	marketID, symbol := aMarketWithAFillBy(t, d, acct)
	f := newFollower()
	ctx := context.Background()

	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)

	// CLOSE_ONLY is what the breaker writes: it stops new risk and leaves
	// holders able to sell (nativemarket.BreakerPauseStatus).
	mustExec(t, d, `INSERT INTO native_market_transitions
		(id, market_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1::uuid, $2::uuid, 'ACTIVE', 'CLOSE_ONLY', 'SYSTEM', 'market:circuit-breaker',
		        'circuit breaker: the price moved 2500 basis points within 300s', now())`,
		uuidText(), marketID)

	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got := rec.notified[0]
	assert.Equal(t, notifications.KindNativeMarketPaused, got.Kind)
	assert.Equal(t, uid, got.UserID)
	assert.Equal(t, symbol+" is close-only", got.Title)
	assert.Contains(t, got.Body, "still sell what you hold")
}

// TestIntegration_AClosedAccountTellsItsOwner.
//
// Effecting a closure request writes the accounts transition (D-055), which the
// account_status_transitions source already follows. ACCOUNT_RESTRICTED
// therefore covers account closure with no source of its own, and this is the
// proof rather than the assumption.
func TestIntegration_AClosedAccountTellsItsOwner(t *testing.T) {
	d := openDB(t)
	uid := newUser(t, d)
	acct := newAccount(t, d, uid)
	f := newFollower()
	ctx := context.Background()

	_, err := f.RunOnce(ctx, d, &recorder{})
	require.NoError(t, err)

	mustExec(t, d, `INSERT INTO account_status_transitions
		(id, account_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1::uuid, $2, 'ACTIVE', 'CLOSED', 'OPERATOR', 'op-1', 'the account holder asked to close it', now())`,
		uuidText(), acct)

	rec := &recorder{}
	n, err := f.RunOnce(ctx, d, rec)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, notifications.KindAccountRestricted, rec.notified[0].Kind)
	assert.Equal(t, "Your account is closed", rec.notified[0].Title)
	assert.Equal(t, notifications.SeverityCritical, rec.notified[0].Severity,
		"an account leaving ACTIVE is not something a person may switch off")
}
