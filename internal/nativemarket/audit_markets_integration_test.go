//go:build integration

package nativemarket

// Reproductions for the independent adversarial audit of the `markets` area
// (goal §54). Every test here asserts the invariant the area CLAIMS, so each
// one FAILS on the audited tree (productization @ b5aa697) and passes once the
// defect it names is fixed. They are evidence; no product code is changed.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
)

// freshAppConn takes a connection out of the pool for good, so a temp relation
// created on it cannot outlive the test on a pooled connection, and so the
// session's plpgsql plan cache starts empty.
func freshAppConn(ctx context.Context, t *testing.T) *pgx.Conn {
	t.Helper()
	pooled, err := testDB.Pool().Acquire(ctx)
	require.NoError(t, err)
	conn := pooled.Hijack()
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var who string
	require.NoError(t, conn.QueryRow(ctx, `SELECT current_user`).Scan(&who))
	require.Equal(t, "cp_app", who, "these reproductions are about what the APPLICATION role can do")
	return conn
}

// --- F-markets-1 -------------------------------------------------------------

// TestAudit_APositionCannotBeForgedThroughAnUnpinnedSearchPath.
//
// ADR-0027 §1 and D-063: "cp_app holds SELECT on native_positions and nothing
// else ... there is no application path by which a cost basis can be set to
// something the trades do not support."
//
// Migration 00772's cp_native_position_allocate() is SECURITY DEFINER with
// `SET search_path = public`. pg_temp is searched FIRST for relation names when
// it is not named in the path, and cp_app holds TEMP on the database, so the
// caller can shadow `native_assets` and the trigger reads the caller's row —
// writing a native_positions quantity and owner of the caller's choosing. This
// is F-48's defect, in a function written after F-48 was fixed.
func TestAudit_APositionCannotBeForgedThroughAnUnpinnedSearchPath(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx

	// An asset that is live and has NO market yet. Its real creator is
	// f.creator and its real creator_allocation is zero.
	suffix := uuid.NewString()[:6]
	var victim nativeasset.Asset
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			a, _, err := f.assetSv.CreateDraft(ctx, tx, nativeasset.CreateRequest{
				CreatorAccountID: f.creator,
				Name:             "Audit " + suffix,
				Symbol:           "AU" + suffix[:4],
				Description:      "an audit asset",
				Supply:           nativeasset.SupplyModel{MaxSupply: qs("1000000000000000")},
			})
			if err != nil {
				return err
			}
			if _, err := f.assetSv.SetStatus(ctx, tx, a.AssetID, nativeasset.StatusPendingReview, "audit"); err != nil {
				return err
			}
			if _, err := f.assetSv.SetModeration(ctx, tx, a.AssetID, nativeasset.ModerationApproved, "audit"); err != nil {
				return err
			}
			if _, err := f.assetSv.Activate(ctx, tx, a.AssetID, "audit"); err != nil {
				return err
			}
			victim = a
			return nil
		}))

	conn := freshAppConn(ctx, t)

	// The stated control: cp_app cannot write the table directly. This holds.
	_, err := conn.Exec(ctx,
		`INSERT INTO native_positions (account_id, asset_id, quantity, allocation_units) VALUES ($1,$2,1,1)`,
		f.trader, victim.AssetID)
	require.Error(t, err, "cp_app must not hold INSERT on native_positions")

	// Shadow native_assets, then open a market. The FK on
	// native_markets.asset_id resolves by OID, so the market still points at
	// the REAL asset; only the trigger's view of it is the caller's.
	_, err = conn.Exec(ctx, `CREATE TEMP TABLE native_assets (LIKE public.native_assets INCLUDING DEFAULTS)`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `GRANT SELECT ON pg_temp.native_assets TO cp_migrate`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx,
		`INSERT INTO pg_temp.native_assets
		   (asset_id, creator_account_id, name, symbol, description, status,
		    max_supply, creator_allocation, treasury_allocation, content_moderation_state)
		 VALUES ($1,$2,'x','x','','ACTIVE',1000000000000000,777000000000,0,'APPROVED')`,
		victim.AssetID, f.trader)
	require.NoError(t, err)

	marketID := NewMarketID()
	_, err = conn.Exec(ctx,
		`INSERT INTO public.native_markets
		   (id, asset_id, credit_asset_id, virtual_credit_reserve, initial_asset_reserve,
		    platform_fee_bps, creator_fee_bps, status)
		 VALUES ($1,$2,$3,30000000000,1000000000000000,0,0,'PENDING')`,
		marketID, victim.AssetID, f.creditAsset)
	require.NoError(t, err, "opening a market is a thing cp_app legitimately does")

	// Read it back on another connection: what committed?
	var qty string
	rerr := testDB.QueryRow(ctx,
		`SELECT quantity::text FROM native_positions WHERE account_id = $1 AND asset_id = $2`,
		f.trader, victim.AssetID).Scan(&qty)

	// Leave the shared database as it was found: a permanent unreconciled row
	// would break TestIntegration_PositionsReconcileAgainstTheLedger.
	t.Cleanup(func() {
		_, _ = testMigrate.Exec(context.Background(),
			`DELETE FROM native_positions WHERE account_id = $1 AND asset_id = $2`, f.trader, victim.AssetID)
	})

	assert.ErrorIs(t, rerr, pgx.ErrNoRows,
		"a native position must not exist for an account no fill and no real allocation gave units to; "+
			"the trigger wrote quantity %q from a relation the caller controls", qty)
}

// TestAudit_TheNM001PrintCheckReadsOnlyTablesTheCallerCannotControl.
//
// ADR-0027 §1: "A print cannot say the market traded somewhere it did not,
// whoever inserts it." Migration 00771's cp_native_market_check_print() is
// SECURITY DEFINER on the same unpinned path and re-derives its expected prices
// from `native_market_fills`, which the caller can shadow in pg_temp.
//
// The foreign key on fill_id resolves by OID and cannot be shadowed, so the row
// is still refused — by the FK, not by the price check. The test asserts on
// WHICH control refused it: NM001 means the check ran against the real fill.
func TestAudit_TheNM001PrintCheckReadsOnlyTablesTheCallerCannotControl(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	// One real fill, so the fabricated temp row can borrow a real journal
	// transaction id for the column the check never reads.
	_, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	fillID := uuid.New()
	// The prices a fill of (credits_in 1, assets_out 1, to_pool 1, R' 1, Y' 1)
	// implies: spot_before = div(V*1e18, 2), spot_after = (V+1)*1e18,
	// effective = 1e18.
	v := f.market.Curve.VirtualCreditReserve.BigInt()
	spotBefore := new(big.Int).Div(new(big.Int).Mul(v, big.NewInt(1e18)), big.NewInt(2))
	spotAfter := new(big.Int).Mul(new(big.Int).Add(v, big.NewInt(1)), big.NewInt(1e18))

	insertPrint := func(conn *pgx.Conn) error {
		_, e := conn.Exec(ctx,
			`INSERT INTO public.native_market_prints
			   (fill_id, market_id, asset_id, seq, side, price_scale,
			    spot_price_before, spot_price_after, effective_price,
			    credit_volume, asset_volume, printed_at)
			 VALUES ($1,$2,$3,9999,'BUY',18,$4::numeric,$5::numeric,1000000000000000000,1,1,now())`,
			fillID, f.market.ID, f.asset.AssetID, spotBefore.String(), spotAfter.String())
		return e
	}

	// CONTROL, on its own session: with no shadow the check refuses with NM001.
	require.Equal(t, "NM001", db.SQLState(insertPrint(freshAppConn(ctx, t))),
		"the NM001 control refuses a print whose fill does not exist")

	// ATTACK, on a session whose plan cache has never resolved the name.
	conn := freshAppConn(ctx, t)
	_, err = conn.Exec(ctx, `CREATE TEMP TABLE native_market_fills (LIKE public.native_market_fills INCLUDING DEFAULTS)`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `GRANT SELECT ON pg_temp.native_market_fills TO cp_migrate`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx,
		`INSERT INTO pg_temp.native_market_fills
		   (id, market_id, seq, account_id, side, credits_in, credits_out, assets_in, assets_out,
		    credits_to_pool, platform_fee, creator_fee, state_version_before,
		    real_credit_reserve_after, asset_reserve_after, journal_transaction_id, idempotency_key)
		 SELECT $1, $2, 9999, $3, 'BUY', 1, 0, 0, 1, 1, 0, 0, 0, 1, 1, fl.journal_transaction_id, 'fabricated'
		   FROM public.native_market_fills fl LIMIT 1`,
		fillID, f.market.ID, f.trader)
	require.NoError(t, err)

	err = insertPrint(conn)
	require.Error(t, err)
	assert.Equal(t, "NM001", db.SQLState(err),
		"the price check must re-derive from the real fill; it was satisfied by the caller's "+
			"own relation and only the foreign key refused the row: %v", err)
}

// --- F-markets-2 -------------------------------------------------------------

// TestAudit_AFillReportsTheStateVersionItProduced.
//
// `NativeFill.state_version_after` is a REQUIRED field of the order response
// and the trade ticket prints it ("Recorded at state version N",
// apps/web/src/pages/markets/Ticket.tsx:575). curve.go never sets
// Fill.StateAfter.Version — its own comment says "the persistence layer assigns
// it" — and Execute never does, so every trade in every deployment reports 0.
func TestAudit_AFillReportsTheStateVersionItProduced(t *testing.T) {
	f := newFixture(t)

	res, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	st, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), st.Version, "the market really did move to version 1")

	assert.Equal(t, st.Version, res.Fill.StateAfter.Version,
		"the fill the API renders as state_version_after must carry the version it produced")

	res2, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), res2.Fill.StateAfter.Version,
		"and it must move: the customer is shown the same number after every trade")
}

// --- F-markets-3 -------------------------------------------------------------

// TestAudit_ThePublicMarketListCarriesNoIdentity.
//
// D-080 made GET /v1/native-markets public (unauthenticated) on the stated
// ground that "the list carries product data only — no balance, position,
// holder or identity". Every row carries creator_account_id, which
// openapi.yaml marks REQUIRED on NativeMarketSummary, and the route accepts
// ?creator_account_id= as a filter, so an unauthenticated caller can both read
// the identifier and enumerate by it.
func TestAudit_ThePublicMarketListCarriesNoIdentity(t *testing.T) {
	f := newFixture(t)

	page, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Limit: 100})
	require.NoError(t, err)
	var mine *MarketSummary
	for i := range page.Markets {
		if page.Markets[i].MarketID == f.market.ID {
			mine = &page.Markets[i]
		}
	}
	require.NotNil(t, mine, "the fixture's market is on the public list")

	assert.True(t, mine.CreatorAccountID.IsZero(),
		"the public, unauthenticated discovery list must carry no account identity; it carries %s",
		mine.CreatorAccountID.String())

	byCreator, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Creator: f.creator, Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, byCreator.Markets,
		"and it must not be an enumeration index over one account's creations")
}

// --- F-markets-4 -------------------------------------------------------------

// TestAudit_NoSignedInAccountLearnsAnotherAccountsHolding.
//
// GET /v1/native-markets/{id}/summary and GET /v1/native-markets/{id} return
// top_holders[] as {account_id, quantity} to any caller holding
// native_asset:read, which every customer role has. prints.go states the
// opposite rule for the tape it owns: "who did it is not the public's business
// (§16's 'never expose ... internal sensitive evidence' applied to other
// people's positions)".
func TestAudit_NoSignedInAccountLearnsAnotherAccountsHolding(t *testing.T) {
	f := newFixture(t)
	_, err := f.buy(f.trader, 5_000_000_000, money.Quantity{})
	require.NoError(t, err)

	holders, err := f.svc.Holders(f.ctx, testDB, f.asset.AssetID, 10)
	require.NoError(t, err)

	for _, h := range holders {
		assert.True(t, h.AccountID.IsZero(),
			"the holder list a signed-in stranger reads must not name the account holding %s: %s",
			h.Quantity.String(), h.AccountID.String())
	}
}

// --- F-markets-5 -------------------------------------------------------------

// TestAudit_AMalformedDiscoveryCursorIsRefusedNotAFiveHundred.
//
// decodeListCursor validates the cursor's sort key with big.Rat.SetString,
// which accepts a quotient ("1/3") and a hexadecimal mantissa with a binary
// exponent ("0x1p2"). PostgreSQL's numeric parser accepts neither, so
// `$8::numeric` fails with 22P02 and mapError's default branch renders it
// CodeInternal — a 500 from a public, unauthenticated route on input the
// caller chose.
func TestAudit_AMalformedDiscoveryCursorIsRefusedNotAFiveHundred(t *testing.T) {
	f := newFixture(t)

	cursor := func(key string) string {
		b, err := json.Marshal(map[string]string{"k": key, "i": f.market.ID.String()})
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	for _, key := range []string{"1/3", "0x1p2"} {
		_, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Cursor: cursor(key)})
		require.Error(t, err, "cursor key %q", key)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err),
			"cursor key %q must be refused as invalid, not surfaced as a server fault: %v", key, err)
	}
}

// --- probe: a trade racing itself on one idempotency key ----------------------

// TestAudit_ATradeRacingItselfOnOneKeyProducesOneFill drives two concurrent
// Executes with the same key and account and asserts the outcome: exactly one
// fill, one version bump, and a refusal the caller can read.
func TestAudit_ATradeRacingItselfOnOneKeyProducesOneFill(t *testing.T) {
	f := newFixture(t)
	key := "race-" + uuid.NewString()

	run := func() (ExecuteResult, error) {
		var res ExecuteResult
		err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				var rerr error
				res, rerr = f.svc.Execute(ctx, tx, ExecuteRequest{
					MarketID: f.market.ID, AccountID: f.trader, Side: Buy,
					Amount: q(2_000_000_000), IdempotencyKey: key, EffectiveAt: f.clk.Now(),
				})
				return rerr
			})
		return res, err
	}

	type outcome struct {
		res ExecuteResult
		err error
	}
	out := make(chan outcome, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			r, e := run()
			out <- outcome{r, e}
		}()
	}
	close(start)
	a, b := <-out, <-out

	var okCount int
	for _, o := range []outcome{a, b} {
		if o.err == nil {
			okCount++
		} else {
			t.Logf("racing order refused with %s: %v", errs.CodeOf(o.err), o.err)
		}
	}

	var fills int64
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM native_market_fills WHERE idempotency_key = $1`, key).Scan(&fills))
	assert.EqualValues(t, 1, fills, "one key, one fill")

	st, err := f.svc.State(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, st.Version, "the market moved once")
	t.Logf("successful callers: %d", okCount)
}

// --- F-markets-7 -------------------------------------------------------------

// TestAudit_RejectedContentLeavesThePublicMarketList.
//
// listMarkets has no predicate on a.content_moderation_state or a.status, and
// GET /v1/native-markets is Public (D-080), so a REJECTED moderation verdict
// removes nothing: the asset's creator-supplied name, symbol, description and
// image_url keep being served to anonymous visitors, with
// `moderation_state: REJECTED` printed beside the content it rejected.
// nativeasset.SetModeration deliberately does not halt the market ("a signal
// for an operator to halt it, not an automatic halt"), and halting does not
// remove it from this list either, because there is no default status filter.
func TestAudit_RejectedContentLeavesThePublicMarketList(t *testing.T) {
	f := newFixture(t)

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.assetSv.SetModeration(ctx, tx, f.asset.AssetID,
				nativeasset.ModerationRejected, "audit: content refused after launch")
			return err
		}))
	// And the operator's follow-up: stop the market trading entirely.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.SetStatus(ctx, tx, f.market.ID, StatusHalted, "audit: content refused")
			return err
		}))

	page, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Limit: 100})
	require.NoError(t, err)
	for _, m := range page.Markets {
		if m.MarketID != f.market.ID {
			continue
		}
		assert.Failf(t, "rejected content is still published",
			"the public markets list still serves name=%q symbol=%q description=%q "+
				"for an asset whose moderation verdict is %s and whose market is %s",
			m.Name, m.Symbol, m.Description, m.Moderation, m.MarketStatus)
	}
}
