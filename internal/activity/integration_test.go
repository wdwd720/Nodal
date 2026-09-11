//go:build integration

package activity_test

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/activity"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The timeline against the real schema (D-081).
//
// internal/activity reads eighteen branches over tables owned by eleven other
// packages, and the union is ONE compiled-in statement: a renamed column in any
// branch is not a missing item, it is a feed that returns an error for every
// kind. Nothing in the unit tests can see that, because they hold the SQL to a
// column contract by reading its text.
//
// So this runs the statement against the migrated schema and then drives the
// rows the domains actually write, through the tables the schema says are the
// only way those states move.

func openDB(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 8, MinConns: 1, AppName: "activity-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func newUser(t *testing.T, d *db.DB) accounts.UserID {
	t.Helper()
	u, err := accounts.NewRepository().CreateUser(context.Background(), d.Pool(),
		"https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	return u.ID
}

func newAccount(t *testing.T, d *db.DB, owner accounts.UserID) accounts.AccountID {
	t.Helper()
	a, err := accounts.NewRepository().CreateAccount(context.Background(), d.Pool(), owner, accounts.KindCustomer)
	require.NoError(t, err)
	return a.ID
}

func exec(t *testing.T, d *db.DB, sql string, args ...any) {
	t.Helper()
	_, err := d.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

func newUUID() string { return id.New[id.Any]().String() }

// feedOf reads the whole timeline for one account, newest first.
func feedOf(t *testing.T, d *db.DB, acct accounts.AccountID, kinds ...activity.Kind) []activity.Item {
	t.Helper()
	page, err := activity.NewFeed(false).Activity(context.Background(), d, activity.Request{
		AccountID: acct, Kinds: kinds, Limit: activity.MaxLimit,
	})
	require.NoError(t, err)
	return page.Items
}

func itemsByKind(items []activity.Item) map[activity.Kind]activity.Item {
	out := map[activity.Kind]activity.Item{}
	for _, it := range items {
		if _, seen := out[it.Kind]; !seen {
			out[it.Kind] = it
		}
	}
	return out
}

// TestIntegration_EveryBranchOfTheUnionRunsAgainstTheRealSchema.
//
// Eighteen branches, one statement. This asks for every kind and then asks for
// each kind on its own, because the kind filter is a bound parameter each
// branch tests against its own literal: a branch whose literal does not match
// its registered Kind would be a filter that silently returns nothing.
func TestIntegration_EveryBranchOfTheUnionRunsAgainstTheRealSchema(t *testing.T) {
	d := openDB(t)
	acct := newAccount(t, d, newUser(t, d))

	items := feedOf(t, d, acct)
	assert.Empty(t, items, "a fresh account has no history")

	for _, k := range activity.AllKinds() {
		_, err := activity.NewFeed(false).Activity(context.Background(), d, activity.Request{
			AccountID: acct, Kinds: []activity.Kind{k}, Limit: 10,
		})
		require.NoError(t, err, "the %s branch must execute against the migrated schema", k)
	}
}

// TestIntegration_TheProductDomainsAppearOnTheTimeline drives one real row per
// new kind through the table the schema says is the only way that state moves,
// and asserts the item comes back scoped to the right account.
func TestIntegration_TheProductDomainsAppearOnTheTimeline(t *testing.T) {
	d := openDB(t)
	owner := newUser(t, d)
	acct := newAccount(t, d, owner)

	// A second account belonging to a DIFFERENT person, with its own rows, so
	// the owner joins are proved to scope rather than merely to compile.
	stranger := newUser(t, d)
	strangerAcct := newAccount(t, d, stranger)

	// --- verification: a profile born UNVERIFIED, moved by a transition row.
	exec(t, d, `INSERT INTO compliance_profiles (user_id, identity_state) VALUES ($1, 'UNVERIFIED')`, owner)
	exec(t, d, `INSERT INTO compliance_profiles (user_id, identity_state) VALUES ($1, 'UNVERIFIED')`, stranger)
	exec(t, d, `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, actor_type, actor_id, reason)
		VALUES ($1::uuid, $2, 'UNVERIFIED', 'REQUIRED', 'SYSTEM', 'test', 'a payout was asked for')`,
		newUUID(), owner)
	exec(t, d, `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, actor_type, actor_id, reason)
		VALUES ($1::uuid, $2, 'UNVERIFIED', 'REQUIRED', 'SYSTEM', 'test', 'somebody else entirely')`,
		newUUID(), stranger)

	// --- payout destinations: born UNVERIFIED, disabled by a transition row.
	destID := newUUID()
	exec(t, d, `INSERT INTO payout_destinations
		(id, account_id, kind, provider, provider_reference, display_label, currency, status)
		VALUES ($1::uuid, $2, 'BANK', 'test_provider', $3, 'Test bank', 'USD', 'UNVERIFIED')`,
		destID, acct, "ref-"+destID)
	exec(t, d, `INSERT INTO payout_destination_transitions
		(id, destination_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1::uuid, $2::uuid, 'UNVERIFIED', 'DISABLED', 'USER', 'test', 'the account holder stopped using it')`,
		newUUID(), destID)

	// --- terms.
	const contentHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	exec(t, d, `INSERT INTO terms_acceptances
		(id, user_id, document_id, version, content_hash, actor_type, actor_id)
		VALUES ($1::uuid, $2, 'WITHDRAWAL_DISCLOSURE', '2026-09-10.1', $3, 'USER', $4)`,
		newUUID(), owner, contentHash, owner.String())

	// --- account closure: born PENDING, decided by a transition row.
	closureID := newUUID()
	exec(t, d, `INSERT INTO account_closure_requests (id, user_id, state, requested_at, cooling_off_until)
		VALUES ($1::uuid, $2, 'PENDING', now(), now() + interval '14 days')`, closureID, owner)
	exec(t, d, `INSERT INTO account_closure_request_transitions
		(id, request_id, from_state, to_state, actor_type, actor_id, reason)
		VALUES ($1::uuid, $2::uuid, 'PENDING', 'CANCELLED', 'USER', 'test', 'changed their mind')`,
		newUUID(), closureID)

	// --- agents: created, paused, resumed, revoked.
	strategyID := newUUID()
	exec(t, d, `INSERT INTO strategies
		(id, owner_account_id, owner_user_id, name, source_kind, status, created_by_actor_type, created_by_actor_id)
		VALUES ($1::uuid, $2, $3, $4, 'NATURAL_LANGUAGE', 'ACTIVE', 'USER', $5)`,
		strategyID, acct, owner, "strategy-"+strategyID[:8], owner.String())
	agentID := newUUID()
	exec(t, d, `INSERT INTO agents
		(id, account_id, strategy_id, name, stage, state, created_by_actor_type, created_by_actor_id)
		VALUES ($1::uuid, $2, $3::uuid, 'an agent', 'DRAFT', 'DRAFT', 'USER', $4)`,
		agentID, acct, strategyID, owner.String())
	pauseID := newUUID()
	exec(t, d, `INSERT INTO agent_pauses
		(id, agent_id, reason_code, reason, open_orders_policy, paused_by_actor_type, paused_by_actor_id,
		 paused_at, resumed_at, resumed_by_actor_type, resumed_by_actor_id, resume_reason)
		VALUES ($1::uuid, $2::uuid, 'OPERATOR', 'an operator stopped it', 'LEAVE', 'OPERATOR', 'op-1',
		        now() - interval '1 hour', now(), 'USER', $3, 'the owner started it again')`,
		pauseID, agentID, owner.String())
	exec(t, d, `INSERT INTO agent_lifecycle_transitions
		(id, agent_id, from_state, to_state, from_stage, to_stage, actor_type, actor_id, reason)
		VALUES ($1::uuid, $2::uuid, 'DRAFT', 'REVOKED', 'DRAFT', 'DRAFT', 'USER', $3, 'disabled by its owner')`,
		newUUID(), agentID, owner.String())

	items := feedOf(t, d, acct)
	byKind := itemsByKind(items)

	for _, want := range []activity.Kind{
		activity.KindVerificationUpdated,
		activity.KindPayoutDestinationAdded,
		activity.KindPayoutDestinationDisabled,
		activity.KindTermsAccepted,
		activity.KindAccountClosureRequested,
		activity.KindAccountClosureDecided,
		activity.KindAgentCreated,
		activity.KindAgentPaused,
		activity.KindAgentResumed,
		activity.KindAgentDisabled,
	} {
		it, ok := byKind[want]
		require.True(t, ok, "%s is not on the timeline", want)
		assert.NotEmpty(t, it.Summary, "%s has no sentence", want)
		assert.NotEqual(t, string(want), it.Summary, "%s falls through to the default template", want)
		assert.NotEmpty(t, it.Reference.Type, "%s has no reference", want)
		assert.NotEmpty(t, it.Reference.ID, "%s has no reference id", want)
		assert.False(t, it.OccurredAt.IsZero(), "%s has no instant", want)
	}

	assert.Equal(t, "REQUIRED", byKind[activity.KindVerificationUpdated].Status)
	assert.Equal(t, "WITHDRAWAL_DISCLOSURE", byKind[activity.KindTermsAccepted].Status)
	assert.Equal(t, "CANCELLED", byKind[activity.KindAccountClosureDecided].Status)
	assert.Equal(t, "OPERATOR", byKind[activity.KindAgentPaused].Status)
	assert.Equal(t, "REVOKED", byKind[activity.KindAgentDisabled].Status)

	// Newest first, and one instant per item.
	for i := 1; i < len(items); i++ {
		assert.False(t, items[i].OccurredAt.After(items[i-1].OccurredAt),
			"the feed is reverse-chronological")
	}

	// The stranger's verification decision is on the stranger's timeline and
	// on nobody else's. This is the assertion the owner join exists for.
	strangerItems := itemsByKind(feedOf(t, d, strangerAcct))
	_, ok := strangerItems[activity.KindVerificationUpdated]
	assert.True(t, ok, "the stranger sees their own verification")
	assert.Len(t, feedOf(t, d, strangerAcct), 1,
		"the stranger sees ONLY their own row; a user-keyed table read without the owner pin leaks")

	// And the owner sees exactly one verification item, not one per account.
	verifications := feedOf(t, d, acct, activity.KindVerificationUpdated)
	assert.Len(t, verifications, 1)
}

// TestIntegration_ASandboxVerificationIsLabelledSimulated.
//
// ADR-0023: a sandbox outcome is labelled sandbox everywhere it is stored and
// shown. A rehearsal verification is the one a person is most likely to
// mistake for an approval, so its timeline item says so on a deployment that is
// not itself stamped.
func TestIntegration_ASandboxVerificationIsLabelledSimulated(t *testing.T) {
	d := openDB(t)
	owner := newUser(t, d)
	acct := newAccount(t, d, owner)
	exec(t, d, `INSERT INTO compliance_profiles (user_id, identity_state) VALUES ($1, 'UNVERIFIED')`, owner)

	sessionID := newUUID()
	exec(t, d, `INSERT INTO verification_sessions
		(id, user_id, purpose, provider, status, rules_version, environment, sandbox)
		VALUES ($1::uuid, $2, 'PAYOUT_KYC', 'verify_sandbox', 'CREATED', 'v1', 'TEST', true)`,
		sessionID, owner)
	exec(t, d, `INSERT INTO compliance_profile_transitions
		(id, user_id, from_state, to_state, actor_type, actor_id, reason, session_id)
		VALUES ($1::uuid, $2, 'UNVERIFIED', 'REQUIRED', 'SYSTEM', 'test', 'a rehearsal', $3::uuid)`,
		newUUID(), owner, sessionID)

	items := feedOf(t, d, acct, activity.KindVerificationUpdated)
	require.Len(t, items, 1)
	assert.True(t, items[0].Simulated,
		"a sandbox verification session labels its timeline item, on a tier that stamps nothing else")
}

// TestIntegration_AMarketPauseReachesItsTradersAndItsCreator.
//
// The branch's population is the interesting half: a pause is a fact about a
// market and the timeline is per account, so the query decides who hears about
// it. Everyone who traded it, plus the creator of the asset -- who may never
// have traded their own market and is who a delisting matters most to.
func TestIntegration_AMarketPauseReachesItsTradersAndItsCreator(t *testing.T) {
	d := openDB(t)
	creator := newAccount(t, d, newUser(t, d))
	bystander := newAccount(t, d, newUser(t, d))

	// A native asset is an `assets` registry row plus its native half, which is
	// what nativeasset.CreateDraft writes; the registry row is created through
	// the registry so the shape cannot drift from it.
	registered, err := assets.NewRepository().Create(context.Background(), d, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindNativeAsset,
		ValueDomain: valuedomain.InternalNativeAsset,
		Symbol:      "ORBTEST", Name: "Orb test", Decimals: 6,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	exec(t, d, `INSERT INTO native_assets
		(asset_id, creator_account_id, symbol, name, description, max_supply, status)
		VALUES ($1, $2, 'ORBTEST', 'Orb test', '', 1000000, 'DRAFT')`, registered.ID, creator)
	creditAsset := creditAssetID(t, d)
	marketID := newUUID()
	exec(t, d, `INSERT INTO native_markets
		(id, asset_id, credit_asset_id, virtual_credit_reserve, initial_asset_reserve, status)
		VALUES ($1::uuid, $2, $3, 1000000, 1000000, 'PENDING')`, marketID, registered.ID, creditAsset)
	exec(t, d, `INSERT INTO native_market_transitions
		(id, market_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1::uuid, $2::uuid, 'PENDING', 'HALTED', 'OPERATOR', 'op-1', 'a safety review')`,
		newUUID(), marketID)

	got := feedOf(t, d, creator, activity.KindNativeMarketPaused)
	require.Len(t, got, 1, "the creator of the asset hears that their market stopped trading")
	assert.Equal(t, "HALTED", got[0].Status)
	assert.Contains(t, got[0].Summary, "ORBTEST")
	assert.Contains(t, got[0].Summary, "unchanged", "a halt takes nothing away and must not read as though it did")

	assert.Empty(t, feedOf(t, d, bystander, activity.KindNativeMarketPaused),
		"somebody who never touched this market is not told about it")
}

// creditAssetID returns the deployment's Credit asset, creating it through the
// asset registry if this database has none. A native market references one;
// nothing in this test is about the registry, so it uses the registry's own
// constructor rather than a hand-written INSERT that would drift from it.
func creditAssetID(t *testing.T, d *db.DB) assets.AssetID {
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
