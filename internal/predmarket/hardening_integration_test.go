//go:build integration

package predmarket

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/migrations"
)

// Tests of the review-2 hardening (migration 00756): an outcome share exists
// only as its market issued it (F1), voids are dual-controlled (F4), one
// holder never blocks a settlement or a void (F5), a resolver never rules on
// the dispute of their own resolution (F14), and the sandbox oracle's
// observations are durable.

// rawApprovedVoid writes a void proposal by proposer and its approval by
// approver straight into the tables, bypassing this package: what a raw-SQL
// bypass would need to put behind a VOIDED market. The guards of migration
// 00756 still check both rows.
func rawApprovedVoid(ctx context.Context, tx pgx.Tx, m Market, proposer, approver econ.Actor) error {
	pid := id.New[voidProposalKind]()
	at := time.Now().UTC().Add(365 * 24 * time.Hour)
	if _, err := tx.Exec(ctx, `INSERT INTO prediction_void_proposals (id, market_id, market_status, resolution_id, reason,
		evidence_ref, evidence_sha256, proposed_by_actor_type, proposed_by_actor_id, proposed_at)
		VALUES ($1,$2,$3,$4,'a raw proposal for the bypass','raw',$5,'OPERATOR',$6,$7)`,
		pid, m.ID, m.Status, m.CurrentResolutionID, make([]byte, 32), proposer.ID, at); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO prediction_void_approvals (id, proposal_id, market_id, reason, approved_by_actor_type,
		approved_by_actor_id, approved_at) VALUES ($1,$2,$3,'raw approval','OPERATOR',$4,$5)`,
		id.New[voidApprovalKind](), pid, m.ID, approver.ID, at)
	return err
}

// replica is a second service over the same database with no oracle at all:
// another API replica, or this one after a restart.
func (f *fixture) replica() *Service {
	f.t.Helper()
	out := event.NewOutbox(f.clk)
	aw := audit.NewWriterWithBuildVersion("predmarket-replica")
	gc, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, f.clk)
	require.NoError(f.t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(f.t, err)
	svc, err := NewService(Config{
		Clock: f.clk, Intents: econ.NewIntentStore(f.clk, out, aw), Effects: econ.NewEffectStore(f.clk, out, aw),
		Authorizer: econ.NewAuthorizer(gc, kills), Registry: econ.NewRegistry(), KillSwitches: kills,
		Ledger: ledger.NewService(f.clk, "predmarket-replica"), Capital: capital.NewService(f.clk, capital.OutboxEmitter{Outbox: out, Clock: f.clk}),
		Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Instruments: instruments.NewRepository(),
		Assets: assets.NewRepository(), Reconciliation: reconciliation.NewRepository(f.clk, out, aw), Audit: aw,
	})
	require.NoError(f.t, err)
	return svc
}

func (f *fixture) exec(sql string, args ...any) error {
	return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

func (f *fixture) raw(kind ledger.Kind, entries ...rawEntry) error {
	return f.inTx(func(ctx context.Context, tx pgx.Tx) error { return rawJournal(ctx, f, tx, kind, entries) })
}

func (f *fixture) dispute(m Market, by econ.Actor) Dispute {
	f.t.Helper()
	var d Dispute
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, _, err = f.svc.Dispute(ctx, tx, DisputeRequest{MarketID: m.ID, Actor: by,
			Reason: "the board published a correction", Evidence: map[string]string{"correction": "posted"}})
		return err
	}))
	return d
}

func (f *fixture) reservationStatus(rid capital.ReservationID) string {
	f.t.Helper()
	var s string
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT status FROM asset_reservations WHERE id = $1`, rid).Scan(&s))
	return s
}

func errField(t *testing.T, err error, name string) any {
	t.Helper()
	e, ok := errs.As(err)
	require.True(t, ok, "%v", err)
	return e.Fields[name]
}

// --- F1: an outcome share exists only as its market issued it --------------------

// TestIntegration_AnOutcomeShareExistsOnlyAsItsMarketIssuedIt: every way a
// share could appear, move or vanish outside its market is refused by the
// database (PM008), whatever bypasses this package: a faucet grant, a
// transfer, a withdrawal, a one-sided "paper fill", another market's facility,
// a facility funding, an issuance nobody holds. A trade between customers --
// the internal order book -- still moves shares, and the market settles to
// exactly its escrow afterwards.
func TestIntegration_AnOutcomeShareExistsOnlyAsItsMarketIssuedIt(t *testing.T) {
	f := newFixture(t)
	a, b := f.customer(10), f.customer(10)
	m := f.openMarket("YES", "NO")
	other := f.openMarket("UP", "DOWN")
	f.issue(m, a, 3)
	yes := m.Outcomes[0].ShareAssetID
	fac, otherFac := m.Facility(), other.Facility()
	cust := func(acct accounts.AccountID, code ledger.Code) ledger.AccountRef {
		return ledger.CustomerAccount(acct, code, yes)
	}

	refused := map[string]error{
		"a faucet grant": f.raw(ledger.KindSandboxFunding,
			rawEntry{cust(b, ledger.CodeWallet), ledger.Debit, 1}, rawEntry{cust(b, ledger.CodeCapital), ledger.Credit, 1}),
		"a customer-to-customer transfer": f.raw(ledger.KindP2PTransfer,
			rawEntry{cust(a, ledger.CodeTransferOut), ledger.Debit, 1}, rawEntry{cust(a, ledger.CodeWallet), ledger.Credit, 1},
			rawEntry{cust(b, ledger.CodeWallet), ledger.Debit, 1}, rawEntry{cust(b, ledger.CodeTransferIn), ledger.Credit, 1}),
		"a sandbox withdrawal": f.raw(ledger.KindSandboxWithdrawal,
			rawEntry{cust(a, ledger.CodeWithdrawn), ledger.Debit, 1}, rawEntry{cust(a, ledger.CodeWallet), ledger.Credit, 1}),
		"a one-sided paper fill that creates a share": f.raw(ledger.KindSimTrade,
			rawEntry{cust(b, ledger.CodeWallet), ledger.Debit, 1}, rawEntry{cust(b, ledger.CodeTradingInflow), ledger.Credit, 1}),
		"a share parked as margin collateral": f.raw(ledger.KindSimTrade,
			rawEntry{cust(a, ledger.CodeMarginCollateral), ledger.Debit, 1}, rawEntry{cust(a, ledger.CodeWallet), ledger.Credit, 1}),
		"another market's facility issuing the share": f.raw(ledger.KindPredictionSetIssuance,
			rawEntry{cust(b, ledger.CodeWallet), ledger.Debit, 1}, rawEntry{cust(b, ledger.CodeTradingInflow), ledger.Credit, 1},
			rawEntry{otherFac.Account(ledger.CodePredictionSharesIssued, yes), ledger.Debit, 1},
			rawEntry{otherFac.Account(ledger.CodePredictionSharesOutstanding, yes), ledger.Credit, 1}),
		"a facility funding of the share accounts": f.raw(ledger.KindFacilityFunding,
			rawEntry{fac.Account(ledger.CodePredictionSharesIssued, yes), ledger.Debit, 1},
			rawEntry{fac.Account(ledger.CodePredictionSharesOutstanding, yes), ledger.Credit, 1}),
		"an issuance that no customer holds": f.raw(ledger.KindPredictionSetIssuance,
			rawEntry{fac.Account(ledger.CodePredictionSharesIssued, yes), ledger.Debit, 1},
			rawEntry{fac.Account(ledger.CodePredictionSharesOutstanding, yes), ledger.Credit, 1}),
		"a redemption that retires shares the facility keeps outstanding": f.raw(ledger.KindPredictionSetRedemption,
			rawEntry{cust(a, ledger.CodeWallet), ledger.Credit, 1}, rawEntry{cust(a, ledger.CodeTradingOutflow), ledger.Debit, 1}),
	}
	for name, err := range refused {
		assert.Equal(t, "PM008", db.SQLState(err), "%s: %v", name, err)
	}

	// A share asset some ledger account already names can never be registered
	// as an outcome share: both sides of the identity start at zero.
	stray, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: "PMSTRAY", Name: "a pre-funded simulated asset", Decimals: 6, RiskClass: assets.RiskSpeculative, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	f.fund(b, stray.ID, 5)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		shell, err := f.marketShell(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO prediction_outcomes_registry (id, market_id, outcome_index, symbol, share_asset_id, instrument_id)
			VALUES ($1,$2,0,'YES',$3,$4)`, uuid.New(), shell.ID, stray.ID, shell.InstrumentID)
		return err
	})
	assert.Equal(t, "PM008", db.SQLState(err), "%v", err)

	// What the internal book does -- a trade between two customers -- still
	// moves shares, and nothing refused moved anything.
	f.trade(a, b, yes, 1_000_000, 400_000)
	f.requireConserved(m, money.Quantity{})
	assert.Equal(t, "1000000", f.wallet(b, yes).String())
	assert.Equal(t, "2000000", f.wallet(a, yes).String())
	f.resolve(m, "YES")
	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	assert.Equal(t, "3000000", out.Settlement.TotalPaid.String(), "the escrow pays exactly the shares the market issued")
	assert.True(t, out.Settlement.TotalHeld.IsZero())
	drift, err := ledger.VerifyBalances(f.ctx, testDB)
	require.NoError(t, err)
	assert.Empty(t, drift)
}

// marketShell writes a DRAFT market row with no outcomes registered yet, so a
// test can try to register a share by hand.
func (f *fixture) marketShell(ctx context.Context, tx pgx.Tx) (Market, error) {
	spec := f.spec("YES", "NO")
	mid := NewMarketID()
	fac, err := f.svc.registry.EnsureFacility(ctx, tx, ledger.FacilityPredictionMarket, "predmarket:"+mid.String(), f.creator)
	if err != nil {
		return Market{}, err
	}
	inst, _, err := f.svc.instr.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
		Terms: marketTerms(spec), CanonicalName: "PREDICTION " + spec.Symbol, RiskClass: assets.RiskSpeculative,
		Status: assets.StatusHalted, ActiveFrom: spec.OpenAt, ActorType: string(f.creator.Type), ActorID: f.creator.ID,
	})
	if err != nil {
		return Market{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO prediction_markets (id, symbol, question, mode, facility_id, instrument_id, collateral_asset_id,
		outcome_count, unit_quantity, open_at, close_at, dispute_window_ms, oracle_source, oracle_public_key, definition_hash,
		status, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,'q','SANDBOX',$3,$4,$5,2,1000000,now(),now() + interval '1 hour',0,'o',$6,$7,'DRAFT','OPERATOR',$8)`,
		mid, spec.Symbol, fac.ID, inst.ID, f.collateral, make([]byte, 32), make([]byte, 32), f.creator.ID)
	return Market{ID: mid, InstrumentID: inst.ID, FacilityID: fac.ID}, err
}

// TestIntegration_NoHouseOrFacilityBookCanHoldAnOutcomeShare is the decision
// on HOUSE and FACILITY holdings of shares: forbidden, in SQL. A house book
// buying a share from a customer (as a house order on an outcome book would),
// a lending pool parking one, the platform taking one as a fee, or the
// market's own facility booking one to its escrow are all refused, so the
// customers' wallets always hold every outstanding share and settlement pays
// them all.
func TestIntegration_NoHouseOrFacilityBookCanHoldAnOutcomeShare(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 2)
	yes := m.Outcomes[0].ShareAssetID
	var house econ.HouseAccount
	var pool econ.Facility
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if house, err = econ.NewRegistry().CreateHouseAccount(ctx, tx, "pm-house-"+uuid.NewString(), econ.HouseMarketMaking, f.system); err != nil {
			return err
		}
		pool, err = econ.NewRegistry().EnsureFacility(ctx, tx, ledger.FacilityLendingPool, "pm-pool-"+uuid.NewString(), f.system)
		return err
	}))
	seller := func(q int64) []rawEntry {
		return []rawEntry{
			{ledger.CustomerAccount(a, ledger.CodeWallet, yes), ledger.Credit, q},
			{ledger.CustomerAccount(a, ledger.CodeTradingOutflow, yes), ledger.Debit, q},
		}
	}
	for name, err := range map[string]error{
		"a house book buys a share": f.raw(ledger.KindSimTrade, append(seller(1),
			rawEntry{house.Account(ledger.CodeHouseInventory, yes), ledger.Debit, 1},
			rawEntry{house.Account(ledger.CodeHouseTradingInflow, yes), ledger.Credit, 1})...),
		"a lending pool parks a share": f.raw(ledger.KindSimTrade, append(seller(1),
			rawEntry{pool.Account(ledger.CodePoolCash, yes), ledger.Debit, 1},
			rawEntry{pool.Account(ledger.CodePoolCapital, yes), ledger.Credit, 1})...),
		"the platform takes a share as a fee": f.raw(ledger.KindPredictionPayout, append(seller(1),
			rawEntry{ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, yes), ledger.Debit, 1},
			rawEntry{ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, yes), ledger.Credit, 1})...),
		"the market books a share to its escrow": f.raw(ledger.KindPredictionSetRedemption, append(seller(1),
			rawEntry{m.Facility().Account(ledger.CodePredictionEscrow, yes), ledger.Debit, 1},
			rawEntry{m.Facility().Account(ledger.CodePredictionCollateral, yes), ledger.Credit, 1})...),
	} {
		assert.Equal(t, "PM008", db.SQLState(err), "%s: %v", name, err)
	}
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM ledger_balances b JOIN ledger_accounts la ON la.id = b.ledger_account_id
		WHERE la.asset_id = $1 AND la.owner_type IN ('HOUSE','PLATFORM') AND b.balance <> 0`, yes), "no house or platform balance in a share")
	f.requireConserved(m, money.Quantity{})
}

// TestProp_NoJournalCanBreakTheShareHoldingsIdentity throws random journals --
// every journal kind that could plausibly carry a share, booked to customers,
// a house book, the platform, the market's facility and another market's --
// at a market's share asset, bypassing this package and the ledger service.
// Whatever the database accepts, the customers' wallets hold exactly the
// market's outstanding shares, and no other book holds any.
func TestProp_NoJournalCanBreakTheShareHoldingsIdentity(t *testing.T) {
	f := newFixture(t)
	a, b := f.customer(20), f.customer(20)
	m := f.openMarket("YES", "NO")
	other := f.openMarket("UP", "DOWN")
	f.issue(m, a, 5)
	f.issue(m, b, 5)
	yes := m.Outcomes[0].ShareAssetID
	var house econ.HouseAccount
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		house, err = econ.NewRegistry().CreateHouseAccount(ctx, tx, "pm-prop-house-"+uuid.NewString(), econ.HouseHedging, f.system)
		return err
	}))
	type pair struct{ dr, cr ledger.AccountRef }
	pairs := []pair{
		{ledger.CustomerAccount(a, ledger.CodeWallet, yes), ledger.CustomerAccount(a, ledger.CodeTradingInflow, yes)},
		{ledger.CustomerAccount(a, ledger.CodeTradingOutflow, yes), ledger.CustomerAccount(a, ledger.CodeWallet, yes)},
		{ledger.CustomerAccount(b, ledger.CodeWallet, yes), ledger.CustomerAccount(b, ledger.CodeTradingInflow, yes)},
		{ledger.CustomerAccount(b, ledger.CodeTradingOutflow, yes), ledger.CustomerAccount(b, ledger.CodeWallet, yes)},
		{ledger.CustomerAccount(b, ledger.CodeWallet, yes), ledger.CustomerAccount(b, ledger.CodeCapital, yes)},
		{m.Facility().Account(ledger.CodePredictionSharesIssued, yes), m.Facility().Account(ledger.CodePredictionSharesOutstanding, yes)},
		{m.Facility().Account(ledger.CodePredictionSharesOutstanding, yes), m.Facility().Account(ledger.CodePredictionSharesIssued, yes)},
		{other.Facility().Account(ledger.CodePredictionSharesIssued, yes), other.Facility().Account(ledger.CodePredictionSharesOutstanding, yes)},
		{house.Account(ledger.CodeHouseInventory, yes), house.Account(ledger.CodeHouseTradingInflow, yes)},
		{ledger.PlatformAccount(ledger.CodePlatformFeeReceivable, yes), ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, yes)},
	}
	kinds := []ledger.Kind{ledger.KindSimTrade, ledger.KindPredictionSetIssuance, ledger.KindPredictionSetRedemption,
		ledger.KindPredictionPayout, ledger.KindPredictionRefund, ledger.KindSandboxFunding, ledger.KindP2PTransfer, ledger.KindHouseFunding}
	identity := func() (money.Quantity, money.Quantity) {
		var wallets, outstanding string
		require.NoError(t, testDB.QueryRow(f.ctx, `SELECT
			coalesce((SELECT sum(b.balance) FROM ledger_accounts la JOIN ledger_balances b ON b.ledger_account_id = la.id
			           WHERE la.asset_id = $1 AND la.owner_type = 'CUSTOMER' AND la.code = 'WALLET'), 0)::text,
			coalesce((SELECT b.balance FROM ledger_accounts la JOIN ledger_balances b ON b.ledger_account_id = la.id
			           WHERE la.asset_id = $1 AND la.owner_type = 'FACILITY' AND la.owner_id = $2
			             AND la.code = 'PREDICTION_SHARES_OUTSTANDING'), 0)::text`, yes, m.FacilityID).Scan(&wallets, &outstanding))
		w, err := money.ScanQuantity(wallets)
		require.NoError(t, err)
		o, err := money.ScanQuantity(outstanding)
		require.NoError(t, err)
		return w, o
	}
	accepted := 0
	rapid.Check(t, func(rt *rapid.T) {
		kind := kinds[rapid.IntRange(0, len(kinds)-1).Draw(rt, "kind")]
		var entries []rawEntry
		for i, n := 0, rapid.IntRange(1, 3).Draw(rt, "legs"); i < n; i++ {
			p := pairs[rapid.IntRange(0, len(pairs)-1).Draw(rt, "pair")]
			q := rapid.Int64Range(1, 2_000_000).Draw(rt, "qty")
			entries = append(entries, rawEntry{p.dr, ledger.Debit, q}, rawEntry{p.cr, ledger.Credit, q})
		}
		if err := f.raw(kind, entries...); err == nil {
			accepted++
		}
		w, o := identity()
		if w.Cmp(o) != 0 {
			rt.Fatalf("after a %s journal the customers hold %s shares and %s are outstanding", kind, w, o)
		}
		var foreign int
		require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM ledger_balances b JOIN ledger_accounts la ON la.id = b.ledger_account_id
			WHERE la.asset_id = $1 AND b.balance <> 0 AND NOT (
			    (la.owner_type = 'CUSTOMER' AND la.code IN ('WALLET','TRADING_INFLOW','TRADING_OUTFLOW'))
			 OR (la.owner_type = 'FACILITY' AND la.owner_id = $2))`, yes, m.FacilityID).Scan(&foreign))
		if foreign != 0 {
			rt.Fatalf("a book other than a customer's and the market's holds the share")
		}
	})
	t.Logf("%d random journals were accepted (trades between customers, issuances and retirements that move both sides)", accepted)
	f.requireConserved(m, money.Quantity{})
}

// --- F4: voids are dual-controlled ---------------------------------------------------

// TestIntegration_AVoidNeedsTwoDistinctOperators: a void is a proposal by one
// operator and an approval by another; a proposal alone refunds nothing, the
// proposer cannot approve, the creator can do neither, and the refund is
// settled and the VOIDED transition written by the approver.
func TestIntegration_AVoidNeedsTwoDistinctOperators(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 2)

	_, err := f.tryProposeVoid(m, f.creator)
	requireCode(t, err, errs.CodeForbidden, "the creator cannot propose")
	for _, nobody := range []econ.Actor{
		{Type: security.ActorAgent, ID: uuid.NewString()}, {Type: security.ActorUser, ID: uuid.NewString()},
		{Type: security.ActorSystem, ID: "predmarket-scheduler"},
	} {
		_, err = f.tryProposeVoid(m, nobody)
		requireCode(t, err, errs.CodeForbidden, "%s", nobody.Type)
	}
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.ProposeVoid(ctx, tx, VoidProposalRequest{MarketID: m.ID, Actor: f.third, Reason: "the event was cancelled"})
		return err
	})
	requireCode(t, err, errs.CodeValidationFailed, "a proposal carries its evidence")

	p, err := f.tryProposeVoid(m, f.third)
	require.NoError(t, err)
	assert.Equal(t, StatusOpen, p.MarketStatus)
	assert.Nil(t, p.ResolutionID)
	assert.NotEmpty(t, p.EvidenceRef)
	assert.Nil(t, p.Approval)
	assert.Equal(t, StatusOpen, f.get(m).Status, "a proposal voids nothing")
	assert.Equal(t, units(8).String(), f.wallet(a, f.collateral).String(), "and refunds nothing")

	_, err = f.tryApproveVoid(m, p, f.third)
	requireCode(t, err, errs.CodeForbidden, "the proposer cannot approve")
	_, err = f.tryApproveVoid(m, p, f.creator)
	requireCode(t, err, errs.CodeForbidden, "the creator cannot approve")
	assert.Equal(t, StatusOpen, f.get(m).Status)

	out, err := f.tryApproveVoid(m, p, f.resolver)
	require.NoError(t, err)
	assert.Equal(t, StatusVoided, out.Market.Status)
	assert.Equal(t, units(10).String(), f.wallet(a, f.collateral).String())
	assert.Equal(t, f.resolver.ID, out.Settlement.SettledBy.ID, "the refund is the approver's")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM prediction_market_transitions
		WHERE market_id = $1 AND to_status = 'VOIDED' AND actor_id = $2`, m.ID, f.resolver.ID))
	proposals, err := f.svc.VoidProposals(f.ctx, testDB, m.ID)
	require.NoError(t, err)
	require.Len(t, proposals, 1)
	require.NotNil(t, proposals[0].Approval)
	assert.Equal(t, f.resolver.ID, proposals[0].Approval.ApprovedBy.ID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM audit_events WHERE resource_id = $1 AND action = 'predmarket.void.proposed'`, m.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM audit_events WHERE resource_id = $1 AND action = 'predmarket.void.approved'`, m.ID.String()))
}

// TestIntegration_AResolvedMarketInsideItsDisputeWindowIsVoidedOnlyThroughADispute
// is review-2 P2 turned around: the operator who accepted the resolution
// cannot void it, nobody can void a RESOLVED market inside its dispute window,
// and the way out is a dispute, after which a void by two other operators
// rules on it. After the window, a RESOLVED market can still be voided under
// dual control.
func TestIntegration_AResolvedMarketInsideItsDisputeWindowIsVoidedOnlyThroughADispute(t *testing.T) {
	f := newFixture(t)
	spec := f.spec("YES", "NO")
	spec.DisputeWindow = 24 * time.Hour
	m := f.openMarketFrom(spec)
	yes, no := m.Outcomes[0].ShareAssetID, m.Outcomes[1].ShareAssetID
	winner, loser := f.customer(10), f.customer(10)
	f.issue(m, winner, 4)
	f.issue(m, loser, 4)
	f.trade(winner, loser, no, 4_000_000, 0)
	f.trade(loser, winner, yes, 4_000_000, 0)
	f.resolve(m, "YES")

	_, err := f.tryProposeVoid(f.get(m), f.resolver)
	requireCode(t, err, errs.CodeForbidden, "the author of the resolution in force takes no part")
	_, err = f.tryProposeVoid(f.get(m), f.third)
	requireCode(t, err, errs.CodeInvalidStateTransition, "inside the dispute window")
	assert.Equal(t, "DISPUTE_WINDOW_OPEN", errField(t, err, "void_refusal"))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM prediction_void_proposals WHERE market_id = $1`, m.ID))

	f.dispute(f.get(m), f.third)
	p, err := f.tryProposeVoid(f.get(m), f.third)
	require.NoError(t, err, "a DISPUTED market can be voided")
	_, err = f.tryApproveVoid(f.get(m), p, f.resolver)
	requireCode(t, err, errs.CodeForbidden, "the author of the disputed resolution cannot approve")
	out, err := f.tryApproveVoid(f.get(m), p, f.approver)
	require.NoError(t, err)
	assert.Equal(t, StatusVoided, out.Market.Status)
	assert.Equal(t, units(10).String(), f.wallet(winner, f.collateral).String())
	assert.Equal(t, units(10).String(), f.wallet(loser, f.collateral).String())

	// Past the window, two operators may still void a RESOLVED market.
	late := f.openMarket("YES", "NO")
	f.issue(late, winner, 1)
	res := f.resolve(late, "NO")
	f.clk.Set(res.Resolution.AcceptedAt.Add(time.Second))
	_, err = f.tryVoid(f.get(late), f.third)
	require.NoError(t, err)
	assert.Equal(t, StatusVoided, f.get(late).Status)
}

// TestIntegration_AVoidProposalLapsesWhenAResolutionComesIntoForce: what a
// proposer proposed was voiding the market as it stood; once a resolution is
// in force the proposal cannot be approved.
func TestIntegration_AVoidProposalLapsesWhenAResolutionComesIntoForce(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 1)
	m = f.closeMarket(m)
	f.requestResolution(m)
	p, err := f.tryProposeVoid(f.get(m), f.third)
	require.NoError(t, err)
	assert.Equal(t, StatusResolving, p.MarketStatus)
	_, err = f.tryResolve(m, f.report(m, "YES"), f.resolver)
	require.NoError(t, err)
	_, err = f.tryApproveVoid(f.get(m), p, f.approver)
	requireCode(t, err, errs.CodeConflict)
	assert.Equal(t, StatusResolved, f.get(m).Status)
}

// TestIntegration_ConcurrentApprovalsVoidOnce: approvals of two proposals
// racing in separate transactions void the market once and refund every
// holder once.
func TestIntegration_ConcurrentApprovalsVoidOnce(t *testing.T) {
	f := newFixture(t)
	holders := []accounts.AccountID{f.customer(10), f.customer(10)}
	m := f.openMarket("YES", "NO")
	for _, h := range holders {
		f.issue(m, h, 2)
	}
	p1, err := f.tryProposeVoid(m, f.third)
	require.NoError(t, err)
	p2, err := f.tryProposeVoid(m, f.resolver)
	require.NoError(t, err)
	const n = 8
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := p1
			if i%2 == 1 {
				p = p2
			}
			_, err := f.tryApproveVoid(m, p, f.approver)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
			continue
		}
		assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "%v", err)
	}
	assert.GreaterOrEqual(t, ok, 1)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM prediction_void_approvals WHERE market_id = $1`, m.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM prediction_settlements WHERE market_id = $1`, m.ID))
	for _, h := range holders {
		assert.Equal(t, units(10).String(), f.wallet(h, f.collateral).String(), "refunded once")
	}
}

// TestIntegration_TheDatabaseBindsAVoidToAnApprovedProposalByTwoOperators
// drives the guards of migration 00756 around voids directly.
func TestIntegration_TheDatabaseBindsAVoidToAnApprovedProposalByTwoOperators(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 1)
	m = f.closeMarket(m)

	// No approval behind a refund record or a VOIDED transition row: PM007.
	err := f.exec(`INSERT INTO prediction_settlements (id, market_id, kind, outcome_count, unit_quantity, refund_rate_per_share,
		rounding_policy, escrow_before, total_paid, residual, holder_count, evidence_ref, evidence_sha256,
		settled_by_actor_type, settled_by_actor_id, settled_at)
		VALUES ($1,$2,'REFUND',2,1000000,500000,'FLOOR_PER_HOLDER',1000000,1000000,0,1,'x',$3,'OPERATOR',$4,now())`,
		uuid.New(), m.ID, make([]byte, 32), f.third.ID)
	assert.Equal(t, "PM007", db.SQLState(err), "%v", err)
	err = f.exec(`INSERT INTO prediction_market_transitions (id, market_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1,$2,'CLOSED','VOIDED','OPERATOR',$3,'bypass',now())`, uuid.New(), m.ID, f.third.ID)
	assert.Equal(t, "PM007", db.SQLState(err), "%v", err)

	// Dual control in the approval and proposal guards: PM006.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error { return rawApprovedVoid(ctx, tx, m, f.third, f.third) })
	assert.Equal(t, "PM006", db.SQLState(err), "a proposer approving their own void: %v", err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error { return rawApprovedVoid(ctx, tx, m, f.creator, f.third) })
	assert.Equal(t, "PM006", db.SQLState(err), "the creator proposing: %v", err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error { return rawApprovedVoid(ctx, tx, m, f.third, f.creator) })
	assert.Equal(t, "PM006", db.SQLState(err), "the creator approving: %v", err)

	// A proposal that does not describe the market as it stands: PM007.
	err = f.exec(`INSERT INTO prediction_void_proposals (id, market_id, market_status, reason, evidence_ref, evidence_sha256,
		proposed_by_actor_type, proposed_by_actor_id, proposed_at) VALUES ($1,$2,'OPEN','a stale proposal','x',$3,'OPERATOR',$4,now())`,
		uuid.New(), m.ID, make([]byte, 32), f.third.ID)
	assert.Equal(t, "PM007", db.SQLState(err), "%v", err)

	// A RESOLVED market inside its dispute window, and its resolution's author.
	spec := f.spec("YES", "NO")
	spec.DisputeWindow = time.Hour
	r := f.openMarketFrom(spec)
	f.issue(r, a, 1)
	res := f.resolve(r, "YES")
	err = f.exec(`INSERT INTO prediction_void_proposals (id, market_id, market_status, resolution_id, reason, evidence_ref,
		evidence_sha256, proposed_by_actor_type, proposed_by_actor_id, proposed_at)
		VALUES ($1,$2,'RESOLVED',$3,'inside the window','x',$4,'OPERATOR',$5,$6)`,
		uuid.New(), r.ID, res.Resolution.ID, make([]byte, 32), f.third.ID, res.Resolution.AcceptedAt.Add(time.Minute))
	assert.Equal(t, "PM007", db.SQLState(err), "%v", err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawApprovedVoid(ctx, tx, f.get(r), f.resolver, f.third)
	})
	assert.Equal(t, "PM006", db.SQLState(err), "the resolution's author proposing: %v", err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawApprovedVoid(ctx, tx, f.get(r), f.third, f.resolver)
	})
	assert.Equal(t, "PM006", db.SQLState(err), "the resolution's author approving: %v", err)

	// The disputer cannot approve the void that rules on their dispute.
	f.dispute(f.get(r), f.third)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawApprovedVoid(ctx, tx, f.get(r), f.approver, f.third)
	})
	assert.Equal(t, "PM006", db.SQLState(err), "the disputer approving: %v", err)

	// One approval per market, and both tables are append-only.
	p, err := f.tryProposeVoid(m, f.third)
	require.NoError(t, err)
	_, err = f.tryApproveVoid(m, p, f.approver)
	require.NoError(t, err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return rawApprovedVoid(ctx, tx, f.get(m), f.resolver, f.observer)
	})
	require.Error(t, err, "a voided market takes no second approval")
	for _, table := range []string{"prediction_void_proposals", "prediction_void_approvals"} {
		err = f.exec(`UPDATE ` + table + ` SET created_at = now()`)
		assert.Equal(t, "42501", db.SQLState(err), "%s: %v", table, err)
	}
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `DELETE FROM prediction_void_approvals WHERE market_id = $1`, m.ID)
	require.Error(t, err)
	assert.True(t, db.IsImmutableRow(err), "%v", err)
}

// --- F14: rulings are independent of the resolution they rule on ------------------

// TestIntegration_TheResolverCannotRuleOnTheDisputeOfTheirOwnResolution: the
// operator whose resolution is disputed can neither uphold it (by ruling or by
// resubmitting the same outcome) nor overturn it, in Go and in the database;
// an independent operator rules.
func TestIntegration_TheResolverCannotRuleOnTheDisputeOfTheirOwnResolution(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 1)
	original := f.resolve(m, "YES")
	d := f.dispute(f.get(m), f.third)

	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.UpholdResolution(ctx, tx, RulingRequest{MarketID: m.ID, Actor: f.resolver, Reason: "my resolution stands"})
		return err
	})
	requireCode(t, err, errs.CodeForbidden, "upholding their own resolution")
	sameOutcome := f.reportAt(m, "YES", time.Minute)
	_, err = f.tryResolve(m, sameOutcome, f.resolver)
	requireCode(t, err, errs.CodeForbidden, "resubmitting their own outcome")
	overturn := f.reportAt(m, "NO", 2*time.Minute)
	_, err = f.tryResolve(m, overturn, f.resolver)
	requireCode(t, err, errs.CodeForbidden, "overturning their own resolution")

	// The database refuses the same ruling written by hand.
	err = f.exec(`INSERT INTO prediction_resolutions (id, market_id, kind, outcome_index, supersedes_id, dispute_id,
		evidence_ref, evidence_sha256, accepted_by_actor_type, accepted_by_actor_id, reason, accepted_at)
		VALUES ($1,$2,'DISPUTE_UPHELD',0,$3,$4,'x',$5,'OPERATOR',$6,'a ruling by hand',now())`,
		uuid.New(), m.ID, original.Resolution.ID, d.ID, make([]byte, 32), f.resolver.ID)
	assert.Equal(t, "PM006", db.SQLState(err), "%v", err)

	ruler := f.operator()
	ruled, err := f.tryResolve(m, sameOutcome, ruler)
	require.NoError(t, err)
	assert.Equal(t, ResolutionDisputeUpheld, ruled.Resolution.Kind)
	_, err = f.trySettle(m, f.third)
	require.NoError(t, err)
}

// reportAt records, as the observer, a further observation of outcome made
// offset after the first one, and returns the stored report.
func (f *fixture) reportAt(m Market, outcome string, offset time.Duration) OracleReport {
	f.t.Helper()
	at := m.CloseAt.Add(30*time.Second + offset)
	if f.clk.Now().Before(at) {
		f.clk.Set(at.Add(time.Second))
	}
	_, err := f.observe(m, outcome, at, f.observer)
	require.NoError(f.t, err)
	r, err := f.svc.FetchReport(f.ctx, testDB, m.ID)
	require.NoError(f.t, err)
	require.Equal(f.t, outcome, r.Outcome)
	return r
}

// --- the durable sandbox oracle --------------------------------------------------------

// TestIntegration_SandboxObservationsAreDurableAuditedAndSharedAcrossReplicas:
// an observation is a stored, audited row written in the caller's
// transaction; a replica with no oracle at all -- or this service after a
// restart -- fetches the same verified report; recording the same
// observation again is a replay; the latest observation is the one fetched,
// and every one stays on record.
func TestIntegration_SandboxObservationsAreDurableAuditedAndSharedAcrossReplicas(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	m = f.closeMarket(m)
	f.requestResolution(m)
	at := m.CloseAt.Add(30 * time.Second)
	f.clk.Set(at.Add(time.Second))

	// Not recorded if the caller does not commit.
	require.Error(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.svc.RecordObservation(ctx, tx, ObservationRequest{MarketID: m.ID, Outcome: "YES", ObservedAt: at,
			Evidence: []byte("rolled back"), Actor: f.observer}); err != nil {
			return err
		}
		return errors.New("the caller changes its mind")
	}))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM prediction_oracle_observations WHERE market_id = $1`, m.ID))

	first, err := f.observe(m, "YES", at, f.observer)
	require.NoError(t, err)
	assert.False(t, first.Replay)
	o := first.Observation
	assert.Equal(t, f.observer.ID, o.RecordedBy.ID)
	assert.Equal(t, 0, o.OutcomeIndex)
	sum := sha256.Sum256(o.Report.Evidence)
	assert.Equal(t, sum[:], o.EvidenceSHA256)
	require.NoError(t, VerifyReport(m.OraclePublicKey, o.Report))
	again, err := f.observe(m, "YES", at, f.observer)
	require.NoError(t, err)
	assert.True(t, again.Replay)
	assert.Equal(t, o.ID, again.Observation.ID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM prediction_oracle_observations WHERE market_id = $1`, m.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM audit_events WHERE resource_id = $1 AND action = 'predmarket.oracle.observed'
		AND actor_id = $2`, m.ID.String(), f.observer.ID))

	replica := f.replica()
	fromReplica, err := replica.FetchReport(f.ctx, testDB, m.ID)
	require.NoError(t, err)
	fromHere, err := f.svc.FetchReport(f.ctx, testDB, m.ID)
	require.NoError(t, err)
	assert.Equal(t, fromHere, fromReplica, "the report lives in the database, not in a process")
	assert.Equal(t, o.ReportHash, fromReplica.Hash())

	// The oracle corrects itself: the later observation is the one fetched.
	_, err = f.observe(m, "NO", at.Add(time.Second), f.observer)
	require.NoError(t, err)
	latest, err := replica.FetchReport(f.ctx, testDB, m.ID)
	require.NoError(t, err)
	assert.Equal(t, "NO", latest.Outcome)
	all, err := f.svc.Observations(f.ctx, testDB, m.ID)
	require.NoError(t, err)
	assert.Len(t, all, 2, "every observation stays on record")

	// A replica resolves from what it reads.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := replica.Resolve(ctx, tx, ResolveRequest{MarketID: m.ID, Report: latest, Actor: f.resolver})
		return err
	})
	require.NoError(t, err)

	// Without a sandbox oracle nothing can be recorded; a market that named
	// another oracle is never observed by this one; the future and unknown
	// outcomes are refused.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := replica.RecordObservation(ctx, tx, ObservationRequest{MarketID: m.ID, Outcome: "YES", ObservedAt: at,
			Evidence: []byte("x"), Actor: f.observer})
		return err
	})
	requireCode(t, err, errs.CodeProviderUnavailable)
	elsewhere := f.spec("YES", "NO")
	impostor, err := NewSandboxOracle("another-board", sha256Seed("another oracle"))
	require.NoError(t, err)
	elsewhere.OracleSource, elsewhere.OraclePublicKey = impostor.Source(), impostor.PublicKey()
	e := f.openMarketFrom(elsewhere)
	_, err = f.observe(e, "YES", f.clk.Now(), f.observer)
	requireCode(t, err, errs.CodeForbidden)
	_, err = f.svc.FetchReport(f.ctx, testDB, e.ID)
	requireCode(t, err, errs.CodeProviderUnavailable, "a market of another oracle is that oracle's to report")
	open := f.openMarket("YES", "NO")
	_, err = f.observe(open, "YES", f.clk.Now().Add(time.Minute), f.observer)
	requireCode(t, err, errs.CodeValidationFailed, "the future")
	_, err = f.observe(open, "MAYBE", f.clk.Now(), f.observer)
	requireCode(t, err, errs.CodeValidationFailed, "an unknown outcome")
	_, err = f.svc.FetchReport(f.ctx, testDB, open.ID)
	requireCode(t, err, errs.CodeNotFound, "nothing observed yet")
}

// TestIntegration_TheObservationRecorderCannotResolveTheMarket: the operator
// who recorded what the oracle saw never accepts a resolution of that market
// -- not the oracle resolution, not a ruling -- in Go and in the database.
func TestIntegration_TheObservationRecorderCannotResolveTheMarket(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 1)
	m = f.closeMarket(m)
	f.requestResolution(m)
	r := f.report(m, "YES")

	_, err := f.tryResolve(m, r, f.observer)
	requireCode(t, err, errs.CodeForbidden)
	err = f.exec(`INSERT INTO prediction_resolutions (id, market_id, kind, outcome_index, report_hash, report_source,
		report_observed_at, report_signature, evidence_ref, evidence_sha256, accepted_by_actor_type, accepted_by_actor_id, reason, accepted_at)
		VALUES ($1,$2,'ORACLE',0,$3,$4,$5,$6,'x',$7,'OPERATOR',$8,'by hand',now())`,
		uuid.New(), m.ID, r.Hash(), r.Source, r.ObservedAt, r.Signature, make([]byte, 32), f.observer.ID)
	assert.Equal(t, "PM006", db.SQLState(err), "%v", err)
	assert.Equal(t, StatusResolving, f.get(m).Status)

	_, err = f.tryResolve(m, r, f.resolver)
	require.NoError(t, err)
	f.dispute(f.get(m), f.third)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.UpholdResolution(ctx, tx, RulingRequest{MarketID: m.ID, Actor: f.observer, Reason: "the oracle saw it"})
		return err
	})
	requireCode(t, err, errs.CodeForbidden, "nor rule on its dispute")
}

// TestIntegration_AStoredObservationThatDoesNotVerifyIsNeverReturned: a row
// written by hand with a signature the market's key does not verify is
// refused by FetchReport, never handed to a resolution.
func TestIntegration_AStoredObservationThatDoesNotVerifyIsNeverReturned(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	evidence := []byte(`{"forged":true}`)
	sum := sha256.Sum256(evidence)
	err := f.exec(`INSERT INTO prediction_oracle_observations (id, market_id, outcome_index, outcome_symbol, source, observed_at,
		evidence, evidence_sha256, signature, report_hash, recorded_by_actor_type, recorded_by_actor_id, recorded_at)
		VALUES ($1,$2,0,'YES',$3,$4,$5,$6,$7,$8,'OPERATOR',$9,$10)`,
		id.New[observationKind](), m.ID, m.OracleSource, f.clk.Now(), evidence, sum[:], make([]byte, 64), make([]byte, 32), f.observer.ID, f.clk.Now())
	require.NoError(t, err)
	_, err = f.svc.FetchReport(f.ctx, testDB, m.ID)
	requireCode(t, err, errs.CodeForbidden)
	assert.Equal(t, "BAD_SIGNATURE", errField(t, err, "oracle_refusal"))
}

// TestIntegration_TheDatabaseBindsOracleObservationsToTheirMarket: an
// observation names one of its market's outcomes under the market's own
// oracle source, its evidence digest is the evidence's, and it is
// append-only.
func TestIntegration_TheDatabaseBindsOracleObservationsToTheirMarket(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	insert := func(symbol, source string, evidence, digest []byte) error {
		return f.exec(`INSERT INTO prediction_oracle_observations (id, market_id, outcome_index, outcome_symbol, source, observed_at,
			evidence, evidence_sha256, signature, report_hash, recorded_by_actor_type, recorded_by_actor_id, recorded_at)
			VALUES ($1,$2,0,$3,$4,now(),$5,$6,$7,$8,'OPERATOR',$9,now())`,
			id.New[observationKind](), m.ID, symbol, source, evidence, digest, make([]byte, 64), make([]byte, 32), f.observer.ID)
	}
	ev := []byte("evidence")
	sum := sha256.Sum256(ev)
	assert.Equal(t, "PM010", db.SQLState(insert("NO", m.OracleSource, ev, sum[:])), "index 0 is YES")
	assert.Equal(t, "PM010", db.SQLState(insert("YES", "another-board", ev, sum[:])), "another oracle source")
	assert.Equal(t, "prediction_oracle_observations_evidence_digest",
		db.ConstraintName(insert("YES", m.OracleSource, ev, make([]byte, 32))), "the digest is the evidence's")
	_, err := f.observe(m, "YES", f.clk.Now(), f.observer)
	require.NoError(t, err)
	err = f.exec(`UPDATE prediction_oracle_observations SET outcome_symbol = 'NO' WHERE market_id = $1`, m.ID)
	assert.Equal(t, "42501", db.SQLState(err), "%v", err)
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `DELETE FROM prediction_oracle_observations WHERE market_id = $1`, m.ID)
	require.Error(t, err)
	assert.True(t, db.IsImmutableRow(err), "%v", err)

	// Nothing is observed for a market that has settled or been voided.
	_, err = f.tryVoid(m, f.third)
	require.NoError(t, err)
	assert.Equal(t, "PM010", db.SQLState(insert("YES", m.OracleSource, ev, sum[:])))
	_, err = f.observe(m, "YES", f.clk.Now(), f.observer)
	requireCode(t, err, errs.CodeInvalidStateTransition)
}

// TestIntegration_APushedOracleReportStillResolves: the push path is intact --
// a report delivered as JSON, decoded by DecodeOracleReport and never stored
// as an observation, resolves the market once it verifies.
func TestIntegration_APushedOracleReportStillResolves(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	m = f.closeMarket(m)
	f.requestResolution(m)
	signed, err := f.oracle.Sign(OracleReport{MarketID: m.ID, Outcome: "NO", ObservedAt: m.CloseAt.Add(time.Second), Evidence: []byte("pushed")})
	require.NoError(t, err)
	wire, err := EncodeOracleReport(signed)
	require.NoError(t, err)
	pushed, err := DecodeOracleReport(wire)
	require.NoError(t, err)
	res, err := f.tryResolve(m, pushed, f.resolver)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Resolution.OutcomeIndex)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM prediction_oracle_observations WHERE market_id = $1`, m.ID))
}

// --- F5: one holder never blocks a settlement or a void ---------------------------------

// TestIntegration_AFrozenHolderCannotBlockSettlementAndClaimsOnceEligible is
// review-2 P3 turned around: the market settles, the active holder is paid,
// the frozen holder's shares are retired and their payout is held in the
// escrow; their claim is refused while frozen and delivers exactly the held
// amount, once, when the account is active again; conservation holds at
// every step.
func TestIntegration_AFrozenHolderCannotBlockSettlementAndClaimsOnceEligible(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	alice, carol := f.customer(10), f.customer(10)
	f.issue(m, alice, 3)
	f.issue(m, carol, 1)
	f.setAccountStatus(carol, accounts.StatusFrozen)
	f.resolve(m, "YES")

	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	assert.Equal(t, StatusSettled, out.Market.Status)
	assert.Equal(t, units(10).String(), f.wallet(alice, f.collateral).String(), "the active holder is paid")
	assert.Equal(t, units(9).String(), f.wallet(carol, f.collateral).String(), "the frozen holder's payout is held")
	for _, o := range m.Outcomes {
		assert.True(t, f.wallet(carol, o.ShareAssetID).IsZero(), "and their shares are retired")
	}
	st := out.Settlement
	assert.Equal(t, units(4).String(), st.TotalPaid.String(), "escrow before = total paid (delivered + held) + residual")
	assert.Equal(t, units(1).String(), st.TotalHeld.String())
	require.Len(t, out.Held, 1)
	h := out.Held[0]
	assert.Equal(t, carol, h.AccountID)
	assert.Equal(t, units(1).String(), h.Amount.String())
	assert.Contains(t, h.ReasonCodes, risk.ReasonAccountStatus)
	assert.False(t, h.Claimed())
	assert.Equal(t, SettlementPayout, h.Kind)
	for _, l := range out.Lines {
		if l.Intent.AccountID == carol {
			assert.Equal(t, econ.StatusRejected, l.Intent.Status, "the refusal of the delivery is on record")
			assert.False(t, l.JournalTransactionID.IsZero(), "the shares were retired by a journal")
		}
	}
	books := f.requireConserved(m, money.Quantity{})
	assert.Equal(t, units(1).String(), books.Escrow.String(), "the escrow holds exactly the held payout")
	assert.Equal(t, units(1).String(), books.Held.String())
	assert.Equal(t, assets.StatusCloseOnly, f.instrumentStatus(m.InstrumentID), "the market instrument admits the claim")

	// Still frozen: refused, on record, nothing moves.
	_, err = f.tryClaim(m, carol)
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, reasonCodes(t, err), risk.ReasonAccountStatus)
	assert.Equal(t, units(9).String(), f.wallet(carol, f.collateral).String())
	f.requireConserved(m, money.Quantity{})

	// Eligible again: the claim delivers exactly the held amount, once.
	f.setAccountStatus(carol, accounts.StatusActive)
	claimed, err := f.tryClaim(m, carol)
	require.NoError(t, err)
	require.True(t, claimed.HeldPayout.Claimed())
	assert.Equal(t, econ.StatusCompleted, claimed.Action.Intent.Status)
	assert.Equal(t, units(10).String(), f.wallet(carol, f.collateral).String())
	after := f.requireConserved(m, money.Quantity{})
	assert.True(t, after.Escrow.IsZero())
	assert.True(t, after.Held.IsZero())
	assert.Equal(t, assets.StatusHalted, f.instrumentStatus(m.InstrumentID), "the last claim halts the market instrument")
	_, err = f.tryClaim(m, carol)
	requireCode(t, err, errs.CodeConflict, "a held payout is claimed once")
	_, err = f.tryClaim(m, alice)
	requireCode(t, err, errs.CodeNotFound, "nothing is held for a holder who was paid")

	drift, err := ledger.VerifyBalances(f.ctx, testDB)
	require.NoError(t, err)
	assert.Empty(t, drift)
	var rec reconciliation.Record
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = f.svc.ReconcileEscrow(ctx, tx, ReconcileRequest{MarketID: m.ID})
		return err
	}))
	assert.Equal(t, reconciliation.StatusMatched, rec.Status)
}

// TestIntegration_UnverifiedAndClosedHoldersCannotBlockAVoid: a void refunds
// everyone; an unverified holder's and a closed account's refunds are held,
// the unverified one is delivered once the identity is verified, and the
// closed account's stays held -- and conserved -- for as long as it is
// closed.
func TestIntegration_UnverifiedAndClosedHoldersCannotBlockAVoid(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	a := f.customer(10)
	unverified, closed := f.customerWith(false, 0), f.customer(0)
	f.issue(m, a, 4)
	f.trade(a, unverified, m.Outcomes[0].ShareAssetID, 2_000_000, 0)
	f.trade(a, closed, m.Outcomes[1].ShareAssetID, 2_000_000, 0)
	f.setAccountStatus(closed, accounts.StatusClosed)

	out, err := f.tryVoid(m, f.third)
	require.NoError(t, err)
	assert.Equal(t, StatusVoided, out.Market.Status)
	assert.Equal(t, units(4).String(), out.Settlement.TotalPaid.String())
	assert.Equal(t, units(2).String(), out.Settlement.TotalHeld.String())
	assert.Equal(t, units(8).String(), f.wallet(a, f.collateral).String(), "6 + half a unit for each of its 4 shares")
	require.Len(t, out.Held, 2)
	reasons := map[accounts.AccountID][]string{}
	for _, h := range out.Held {
		assert.Equal(t, SettlementRefund, h.Kind)
		assert.Equal(t, units(1).String(), h.Amount.String())
		reasons[h.AccountID] = h.ReasonCodes
	}
	assert.Contains(t, reasons[unverified], risk.ReasonIdentityUnverified)
	assert.Contains(t, reasons[closed], risk.ReasonAccountStatus)
	b := f.requireConserved(m, out.Settlement.Residual)
	assert.Equal(t, units(2).String(), b.Escrow.String())

	_, err = f.tryClaim(m, unverified)
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, reasonCodes(t, err), risk.ReasonIdentityUnverified)
	var user accounts.UserID
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT owner_user_id FROM accounts WHERE id = $1`, unverified).Scan(&user))
	digest := sha256.Sum256([]byte(uuid.NewString()))
	require.NoError(t, accounts.NewRepository().SetEmailHash(f.ctx, testDB, user, digest[:]))
	_, err = f.tryClaim(m, unverified)
	require.NoError(t, err)
	assert.Equal(t, units(1).String(), f.wallet(unverified, f.collateral).String())

	_, err = f.tryClaim(m, closed)
	requireCode(t, err, errs.CodeRiskRejected, "a closed account never becomes eligible")
	b = f.requireConserved(m, out.Settlement.Residual)
	assert.Equal(t, units(1).String(), b.Escrow.String(), "the closed account's refund is still held")
	assert.Equal(t, assets.StatusCloseOnly, f.instrumentStatus(m.InstrumentID), "a claim is still owed")
}

// TestIntegration_ARestrictedHolderIsPaidDirectly: RESTRICTED accounts may
// reduce risk, so the kernel admits their payout and nothing is held.
func TestIntegration_ARestrictedHolderIsPaidDirectly(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	r := f.customer(5)
	f.issue(m, r, 2)
	f.setAccountStatus(r, accounts.StatusRestricted)
	f.resolve(m, "YES")
	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	assert.Empty(t, out.Held)
	assert.Equal(t, units(5).String(), f.wallet(r, f.collateral).String())
	assert.Equal(t, assets.StatusHalted, f.instrumentStatus(m.InstrumentID))
}

// TestIntegration_AGlobalRefusalStillStopsTheWholeSettlement: only a refusal
// that is the holder's own is held; a refusal that is not -- here the
// instrument's own halt switch, which names no holder -- stops everything.
func TestIntegration_AGlobalRefusalStillStopsTheWholeSettlement(t *testing.T) {
	kills := &switchableKills{}
	f := newFixture(t, withKillSwitches(kills))
	m := f.openMarket("YES", "NO")
	a, frozen := f.customer(5), f.customer(5)
	f.issue(m, a, 1)
	f.issue(m, frozen, 1)
	f.setAccountStatus(frozen, accounts.StatusFrozen)
	f.resolve(m, "YES")
	kills.set(killswitch.Switch{Kind: killswitch.InstrumentHalt, ScopeID: m.InstrumentID.String(), Active: true})
	_, err := f.trySettle(m, f.third)
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Equal(t, StatusResolved, f.get(m).Status, "nothing settled, nothing held")
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM prediction_held_payouts WHERE market_id = $1`, m.ID))
	kills.set()
	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	assert.Len(t, out.Held, 1)
}

// TestIntegration_AHeldPayoutIsClaimedExactlyOnceUnderConcurrency: N claims
// with different keys, in separate transactions, deliver the held amount once;
// a replay of the delivering request returns its first outcome.
func TestIntegration_AHeldPayoutIsClaimedExactlyOnceUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	h := f.customer(10)
	f.issue(m, h, 2)
	f.setAccountStatus(h, accounts.StatusFrozen)
	f.resolve(m, "NO")
	_, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	f.setAccountStatus(h, accounts.StatusActive)

	const n = 8
	var wg sync.WaitGroup
	keys := make(chan string, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := "itest-claim-" + uuid.NewString()
			err := f.inTxKeepingRefusal(func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.svc.ClaimHeldPayout(ctx, tx, ClaimRequest{MarketID: m.ID, AccountID: h, IdempotencyKey: key,
					ActorType: security.ActorUser, ActorID: "user-" + h.String()})
				return err
			})
			if err == nil {
				keys <- key
				return
			}
			assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "%v", err)
		}()
	}
	wg.Wait()
	close(keys)
	var winners []string
	for k := range keys {
		winners = append(winners, k)
	}
	require.Len(t, winners, 1, "exactly one claim delivers")
	assert.Equal(t, units(10).String(), f.wallet(h, f.collateral).String())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM prediction_held_payout_claims WHERE market_id = $1`, m.ID))

	var replay ClaimResult
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		replay, err = f.svc.ClaimHeldPayout(ctx, tx, ClaimRequest{MarketID: m.ID, AccountID: h, IdempotencyKey: winners[0],
			ActorType: security.ActorUser, ActorID: "user-" + h.String()})
		return err
	}))
	assert.True(t, replay.Action.Replay)
	assert.Equal(t, units(10).String(), f.wallet(h, f.collateral).String())
	f.requireConserved(m, money.Quantity{})
}

// TestProp_HeldPayoutsConserveTheEscrowExactly: whoever is frozen when a
// market settles, Σ delivered + Σ held = escrow, the escrow afterwards holds
// exactly the held payouts, and once every holder is eligible and claims,
// every holder has received exactly their winning shares and the escrow is
// empty.
func TestProp_HeldPayoutsConserveTheEscrowExactly(t *testing.T) {
	requireEnv(t)
	rapid.Check(t, func(rt *rapid.T) {
		f := newFixture(t)
		n := rapid.IntRange(2, 3).Draw(rt, "outcomes")
		names := []string{"P", "Q", "R"}[:n]
		m := f.openMarket(names...)
		holders := make([]accounts.AccountID, rapid.IntRange(1, 4).Draw(rt, "holders"))
		frozen := map[accounts.AccountID]bool{}
		for i := range holders {
			holders[i] = f.customer(50)
			f.issue(m, holders[i], rapid.Int64Range(1, 5).Draw(rt, "sets"))
		}
		for i := 0; i < len(holders); i++ {
			from, to := holders[i], holders[rapid.IntRange(0, len(holders)-1).Draw(rt, "to")]
			o := m.Outcomes[rapid.IntRange(0, n-1).Draw(rt, "outcome")]
			if from != to {
				f.trade(from, to, o.ShareAssetID, 1_000_000, 0)
			}
		}
		winning := rapid.IntRange(0, n-1).Draw(rt, "winning")
		winners := map[accounts.AccountID]money.Quantity{}
		before := map[accounts.AccountID]money.Quantity{}
		for _, h := range holders {
			winners[h] = f.wallet(h, m.Outcomes[winning].ShareAssetID)
			before[h] = f.wallet(h, f.collateral)
			if rapid.Bool().Draw(rt, "frozen") {
				frozen[h] = true
				f.setAccountStatus(h, accounts.StatusFrozen)
			}
		}
		escrow := f.books(m).Escrow
		f.resolve(m, names[winning])
		out, err := f.trySettle(m, f.third)
		if err != nil {
			rt.Fatalf("settle: %v", err)
		}
		delivered, held := money.Quantity{}, money.Quantity{}
		for _, h := range holders {
			delivered = delivered.Add(f.wallet(h, f.collateral).Sub(before[h]))
		}
		for _, hp := range out.Held {
			held = held.Add(hp.Amount)
		}
		if delivered.Add(held).Cmp(escrow) != 0 || out.Settlement.TotalHeld.Cmp(held) != 0 || out.Settlement.TotalPaid.Cmp(escrow) != 0 {
			rt.Fatalf("escrow %s: delivered %s + held %s (record: paid %s, held %s)", escrow, delivered, held,
				out.Settlement.TotalPaid, out.Settlement.TotalHeld)
		}
		b := f.books(m)
		if b.Escrow.Cmp(held) != 0 || len(b.Violations(money.Quantity{})) > 0 {
			rt.Fatalf("after settlement escrow %s, held %s, violations %v", b.Escrow, held, b.Violations(money.Quantity{}))
		}
		for _, h := range holders {
			if !frozen[h] {
				continue
			}
			f.setAccountStatus(h, accounts.StatusActive)
			if _, err := f.tryClaim(m, h); err != nil && errs.CodeOf(err) != errs.CodeNotFound {
				rt.Fatalf("claim: %v", err)
			}
		}
		for _, h := range holders {
			if got := f.wallet(h, f.collateral).Sub(before[h]); got.Cmp(winners[h]) != 0 {
				rt.Fatalf("holder received %s for %s winning shares", got, winners[h])
			}
		}
		if b := f.books(m); !b.Escrow.IsZero() || len(b.Violations(money.Quantity{})) > 0 {
			rt.Fatalf("after every claim escrow %s, violations %v", b.Escrow, b.Violations(money.Quantity{}))
		}
	})
}

// TestIntegration_TheDatabaseBindsHeldPayoutsToTheirSettlement drives the
// held-payout guards of migration 00756 directly.
func TestIntegration_TheDatabaseBindsHeldPayoutsToTheirSettlement(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	alice, carol := f.customer(10), f.customer(10)
	f.issue(m, alice, 1)
	f.issue(m, carol, 1)
	f.setAccountStatus(carol, accounts.StatusFrozen)
	f.resolve(m, "YES")
	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	require.Len(t, out.Held, 1)
	h := out.Held[0]

	claim := func(account accounts.AccountID, amount int64, journal ledger.TransactionID) error {
		return f.exec(`INSERT INTO prediction_held_payout_claims (id, held_payout_id, market_id, account_id, amount, intent_id,
			effect_id, journal_transaction_id, claimed_by_actor_type, claimed_by_actor_id, evidence_ref, evidence_sha256, claimed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'USER','x','x',$9,now())`,
			uuid.New(), h.ID, m.ID, account, amount, h.RefusedIntentID, h.EffectID, journal, make([]byte, 32))
	}
	assert.Equal(t, "PM009", db.SQLState(claim(carol, 1_000_000, h.JournalTransactionID)),
		"a claim must name its own completed intent, applied effect and paying journal")
	assert.Equal(t, "PM009", db.SQLState(claim(alice, 1_000_000, h.JournalTransactionID)), "another account")
	assert.Equal(t, "PM009", db.SQLState(claim(carol, 1, h.JournalTransactionID)), "another amount")

	// A settlement record stating a held total its held payouts do not sum
	// to is refused at COMMIT.
	r := f.openMarket("YES", "NO")
	f.issue(r, alice, 1)
	f.resolve(r, "YES")
	resolved := f.get(r)
	err = f.exec(`INSERT INTO prediction_settlements (id, market_id, kind, resolution_id, winning_outcome_index, outcome_count,
		unit_quantity, rounding_policy, escrow_before, total_paid, total_held, residual, holder_count, evidence_ref, evidence_sha256,
		settled_by_actor_type, settled_by_actor_id, settled_at)
		VALUES ($1,$2,'PAYOUT',$3,0,2,1000000,'EXACT',1000000,1000000,1000000,0,1,'x',$4,'OPERATOR',$5,now())`,
		uuid.New(), r.ID, *resolved.CurrentResolutionID, make([]byte, 32), f.third.ID)
	assert.Equal(t, "PM009", db.SQLState(err), "%v", err)
	err = f.exec(`INSERT INTO prediction_settlements (id, market_id, kind, resolution_id, winning_outcome_index, outcome_count,
		unit_quantity, rounding_policy, escrow_before, total_paid, total_held, residual, holder_count, evidence_ref, evidence_sha256,
		settled_by_actor_type, settled_by_actor_id, settled_at)
		VALUES ($1,$2,'PAYOUT',$3,0,2,1000000,'EXACT',1000000,1000000,1000001,0,1,'x',$4,'OPERATOR',$5,now())`,
		uuid.New(), r.ID, *resolved.CurrentResolutionID, make([]byte, 32), f.third.ID)
	assert.Equal(t, "prediction_settlements_held_within_paid", db.ConstraintName(err), "%v", err)

	// A held payout must match its settlement, effect, journal and refused intent.
	err = f.exec(`INSERT INTO prediction_held_payouts (id, market_id, settlement_id, account_id, kind, collateral_asset_id, amount,
		reason_codes, refused_intent_id, effect_id, journal_transaction_id, evidence_ref, evidence_sha256, held_at)
		VALUES ($1,$2,$3,$4,'REFUND',$5,1,$6,$7,$8,$9,'x',$10,now())`,
		uuid.New(), m.ID, h.SettlementID, alice, f.collateral, []string{"RISK_ACCOUNT_STATUS"}, h.RefusedIntentID, h.EffectID,
		h.JournalTransactionID, make([]byte, 32))
	require.Error(t, err)
	assert.Contains(t, []string{"PM009", "23505"}, db.SQLState(err), "%v", err)

	// Append-only.
	err = f.exec(`UPDATE prediction_held_payouts SET amount = 0 WHERE id = $1`, h.ID)
	assert.Equal(t, "42501", db.SQLState(err), "%v", err)
	f.requireConserved(m, money.Quantity{})
}

// --- F5: resting orders at close --------------------------------------------------------

// TestIntegration_WithoutAnOrderBookAReservedShareRefusesSettlementNamingTheReservation:
// with no OutcomeBooks configured, an outcome share still reserved by a
// resting order refuses the settlement and the void, naming the
// reservation; once the order is cancelled the market settles.
func TestIntegration_WithoutAnOrderBookAReservedShareRefusesSettlementNamingTheReservation(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	a := f.customer(10)
	f.issue(m, a, 2)
	order := f.reserveShares(a, m.Outcomes[0].ShareAssetID, 1_000_000)
	f.resolve(m, "YES")

	_, err := f.trySettle(m, f.third)
	requireCode(t, err, errs.CodeConflict)
	assert.Equal(t, "SHARES_RESERVED", errField(t, err, "refusal"))
	assert.Equal(t, false, errField(t, err, "outcome_books_configured"))
	named, _ := errField(t, err, "reservations").([]string)
	require.Len(t, named, 1)
	assert.Contains(t, named[0], order.ID.String())
	assert.Contains(t, named[0], a.String())
	assert.Equal(t, StatusResolved, f.get(m).Status, "nothing settled")
	assert.Equal(t, units(2).String(), f.books(m).Escrow.String())

	p, err := f.tryProposeVoid(f.get(m), f.third)
	require.NoError(t, err)
	_, err = f.tryApproveVoid(f.get(m), p, f.approver)
	requireCode(t, err, errs.CodeConflict, "a void is refused the same way")

	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.capital.Release(ctx, tx, order.ID, "the order book cancelled the resting order")
		return err
	}))
	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	assert.Equal(t, units(2).String(), out.Settlement.TotalPaid.String())
}

// TestIntegration_TheOrderBookCancelsRestingOrdersAtCloseVoidAndSettlement:
// with OutcomeBooks configured, closing a market (by the scheduler or an
// operator) cancels every resting order on its outcome instruments in the
// same transaction, a void of an OPEN market does too, and a settlement
// finds nothing reserved. A failing order book fails the close and leaves
// the market as it was.
func TestIntegration_TheOrderBookCancelsRestingOrdersAtCloseVoidAndSettlement(t *testing.T) {
	books := newFakeBooks()
	f := newFixture(t, withBooks(books))
	a := f.customer(20)

	m := f.openMarket("YES", "NO")
	f.issue(m, a, 2)
	order := f.reserveShares(a, m.Outcomes[0].ShareAssetID, 1_000_000)
	books.rest(m.Outcomes[0].InstrumentID, order.ID)
	m = f.closeMarket(m)
	require.NotEmpty(t, books.calls)
	assert.ElementsMatch(t, []instruments.InstrumentID{m.Outcomes[0].InstrumentID, m.Outcomes[1].InstrumentID}, books.calls[0])
	assert.Equal(t, f.system, books.actors[0], "the scheduler closed it")
	assert.Equal(t, "RELEASED", f.reservationStatus(order.ID), "no order outlives its market")
	var payload json.RawMessage
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT payload FROM audit_events WHERE resource_id = $1 AND action = 'predmarket.market.closed'`,
		m.ID.String()).Scan(&payload))
	assert.JSONEq(t, `1`, string(jsonField(t, payload, "resting_orders_cancelled")))
	f.resolve(m, "YES")
	_, err := f.trySettle(m, f.third)
	require.NoError(t, err)

	// A void of an OPEN market cancels its resting orders too.
	v := f.openMarket("YES", "NO")
	f.issue(v, a, 1)
	vOrder := f.reserveShares(a, v.Outcomes[1].ShareAssetID, 1_000_000)
	books.rest(v.Outcomes[1].InstrumentID, vOrder.ID)
	_, err = f.tryVoid(v, f.third)
	require.NoError(t, err)
	assert.Equal(t, "RELEASED", f.reservationStatus(vOrder.ID))

	// A failing order book fails the close: all or nothing.
	c := f.openMarket("YES", "NO")
	books.fail = errs.New(errs.CodeProviderUnavailable, "the order book is unavailable")
	f.clk.Set(c.CloseAt.Add(time.Minute))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Close(ctx, tx, c.ID, f.resolver)
		return err
	})
	requireCode(t, err, errs.CodeProviderUnavailable)
	assert.Equal(t, StatusOpen, f.get(c).Status)
	assert.Equal(t, assets.StatusActive, f.instrumentStatus(c.Outcomes[0].InstrumentID))
	books.fail = nil
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Close(ctx, tx, c.ID, f.resolver)
		return err
	})
	require.NoError(t, err)
}

var (
	sqlstateBlock = regexp.MustCompile(`(?m)^--\s*Custom SQLSTATEs[^\n]*(?:\n--[^\n]*)*`)
	sqlstateCode  = regexp.MustCompile(`\b([A-Z]{2}[0-9]{3})\b`)
)

func migrationText(name string) (string, error) {
	b, err := migrations.FS.ReadFile(name)
	return string(b), err
}

func jsonField(t *testing.T, raw json.RawMessage, name string) json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &doc))
	return doc[name]
}

// TestIntegration_TheHardeningMigrationKeepsTheSchemaContracts checks, for
// migration 00756 alone, what test/integration/migrations holds every
// migration to: least privilege, read-only operator roles, append-only
// history, and every documented SQLSTATE raised.
func TestIntegration_TheHardeningMigrationKeepsTheSchemaContracts(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	has := func(role, table, p string) bool {
		var ok bool
		require.NoError(t, testDB.QueryRow(ctx, `SELECT has_table_privilege($1, $2, $3)`, role, "public."+table, p).Scan(&ok))
		return ok
	}
	for _, table := range []string{"prediction_oracle_observations", "prediction_void_proposals", "prediction_void_approvals",
		"prediction_held_payouts", "prediction_held_payout_claims"} {
		assert.True(t, has("cp_app", table, "SELECT"), table)
		assert.True(t, has("cp_app", table, "INSERT"), table)
		for _, p := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
			assert.False(t, has("cp_app", table, p), "%s is append-only (%s)", table, p)
		}
		for _, ro := range []string{"cp_readonly", "cp_ops"} {
			assert.True(t, has(ro, table, "SELECT"), "%s %s", ro, table)
			for _, p := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
				assert.False(t, has(ro, table, p), "%s may not %s %s", ro, p, table)
			}
		}
	}
	body, err := migrationText("00756_predmarket_hardening.sql")
	require.NoError(t, err)
	block := sqlstateBlock.FindString(body)
	documented := sqlstateCode.FindAllString(block, -1)
	assert.Equal(t, []string{"PM006", "PM007", "PM008", "PM009", "PM010"}, documented)
	for _, code := range documented {
		assert.Contains(t, body, "ERRCODE = '"+code+"'", "%s is documented and must be raised", code)
	}
}
