//go:build integration

package payout_test

// Reproductions for the THIRD round of the withdrawal-verification audit
// (goal §54, "repeat until findings flatten"). Every test here is expected to
// FAIL on 4ce0299, and each asserts the invariant the migrations, the product
// document and the decision register claim, so a fix makes it pass rather than
// making it moot.
//
// Nothing here changes product code.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/provider/verifysandbox"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/verification"
	"github.com/nodal/controlplane/internal/verification/rules"
)

// allocatedLots is which lots a payout actually reserved, and how much of each.
func allocatedLots(t *testing.T, id payout.RequestID) map[credit.LotID]string {
	t.Helper()
	rows, err := testDB.Query(context.Background(),
		`SELECT lot_id, quantity::text FROM payout_allocations WHERE request_id = $1 AND NOT returned`, id)
	require.NoError(t, err)
	defer rows.Close()
	out := map[credit.LotID]string{}
	for rows.Next() {
		var (
			lot credit.LotID
			qty string
		)
		require.NoError(t, rows.Scan(&lot, &qty))
		out[lot] = qty
	}
	require.NoError(t, rows.Err())
	return out
}

// floorOf reads a lot's origin floor from the projection Policy.Permits is fed.
func floorOf(t *testing.T, id credit.LotID) valuedomain.CreditOrigin {
	t.Helper()
	var out valuedomain.CreditOrigin
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT origin_floor FROM credit_lot_state WHERE lot_id = $1`, id).Scan(&out))
	return out
}

// createPayout is the whole commit path: a quote, then POST /v1/payouts'
// domain call, on a sandbox tier with everything else permitting.
func (f *auditFixture) createPayout(t *testing.T, qty int64, key string) payout.Request {
	t.Helper()
	quote, qerr := f.quote(qty)
	require.NoError(t, qerr)
	require.True(t, quote.MinimumOK, "fixture check: the amount must clear the provider minimum")
	dest := f.destination
	var req payout.Request
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var cerr error
			req, _, cerr = f.svc.Create(ctx, tx, payout.CreateRequest{
				AccountID: f.account, DestinationID: &dest, QuoteID: &quote.ID,
				Quantity:           money.QuantityFromInt64(qty),
				ProviderTerms:      f.terms(),
				Environment:        "TEST",
				DisclosureAccepted: true,
				IdempotencyKey:     key + "-" + uuid.NewString(),
				EffectiveAt:        f.clk.Now(),
			}, f.sandboxInput())
			return cerr
		}))
	return req
}

// ---------------------------------------------------------------------------
// F-wv3-1 — the reservation consumes by ORIGIN, but eligibility is decided per
// LOT, so a payout approved for one lot is filled from a different lot of the
// same origin that the same evaluation refused.
//
// payout.Engine.Evaluate walks credit.EligibleLots — a per-lot
// valuedomain.Policy.Permits, which reads the lot's FINALITY and (since D-131)
// its ORIGIN FLOOR — and records the exact lots it approved in Decision.Lots.
// payout.Service.reserve then throws that away: it passes
// credit.EligibleOrigins(d.Lots), the SET OF ORIGIN STRINGS, as
// ConsumeRequest.AllowedOrigins, and credit.Consume selects whatever sorts
// first in consumption order among lots of those origins whose finality is
// merely SPENDABLE (UNFUNDED, REVERSIBLE, SETTLED).
//
// credit.ConsumeRequest.LotIDs exists for exactly this and is not used here.
// Its own comment says why the origin filter is not enough: "two purchases
// produce two lots of the same origin".
//
// Two independently reachable consequences, one per sub-test.
// ---------------------------------------------------------------------------

// (a) FINALITY. valuedomain.FundingFinality.PayoutEligible() admits only
// SETTLED and UNFUNDED — "paying out value that a card issuer can still
// reclaim turns a chargeback into an uncollateralised loss, which PART XI
// forbids" — but Spendable() also admits REVERSIBLE, which is the filter the
// consume actually applies.
func TestAuditWV3_APayoutApprovedOnASettledLotIsFilledFromAReversibleOne(t *testing.T) {
	f := newAuditFixture(t)

	// A card payment still inside its dispute window, minted FIRST so it sorts
	// first in consumption order (same origin ⇒ the tiebreak is created_at).
	reversible := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 200_000_000)
	// The settled purchase the policy actually releases.
	settled := f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 200_000_000)

	// What the engine decided: only the settled lot may leave.
	eligible, total, err := f.credits.EligibleLots(f.ctx, testDB, credit.BalanceRequest{
		AccountID:  f.account,
		Policy:     valuedomain.SandboxPolicy(),
		Verified:   valuedomain.VerificationPayoutKYC,
		ActiveCaps: map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Now:        f.clk.Now(),
	})
	require.NoError(t, err)
	require.Len(t, eligible, 1, "fixture check: exactly one lot is payout-eligible")
	require.Equal(t, settled.ID, eligible[0].ID, "fixture check: it is the SETTLED one")
	require.Equal(t, "200000000", total.String())

	req := f.createPayout(t, 200_000_000, "audit3-finality")
	require.Equal(t, payout.StateVerified, req.State, "the request was reserved")
	require.Equal(t, "200000000", req.ReservedQuantity.String())

	got := allocatedLots(t, req.ID)
	assert.NotContains(t, got, reversible.ID,
		"F-wv3-1(a): the payout reserved a REVERSIBLE lot the policy refused; "+
			"payout.Service.reserve restricts consumption to the ORIGINS of the approved "+
			"lots and credit.Consume then takes whichever lot of that origin sorts first, "+
			"so a card payment inside its dispute window was reserved in place of the "+
			"settled purchase the decision approved")
	assert.Contains(t, got, settled.ID,
		"F-wv3-1(a): the lot payout.Engine.Evaluate approved is not the lot that was reserved")
}

// (b) ORIGIN FLOOR. D-131: "Policy.Permits releases a lot only when it releases
// BOTH the lot's origin and its floor", and the floor is per LOT. Two lots of
// MARKET_TRADING_PROCEEDS — one round-tripped out of a promotional grant, one
// out of a settled purchase — are one ORIGIN, so approving the second approves
// consumption of the first. That is F-261's laundering route reopened through
// the filter the reservation uses.
func TestAuditWV3_APayoutApprovedOnAPurchasedFloorIsFilledFromAPromotionalOne(t *testing.T) {
	f := newAuditFixture(t)

	grant := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 200_000_000)
	purchase := f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 200_000_000)
	require.Equal(t, valuedomain.OriginPromotional, floorOf(t, grant.ID))
	require.Equal(t, valuedomain.OriginPurchased, floorOf(t, purchase.ID))

	// The grant round-tripped through a market: proceeds whose floor is the
	// grant. Minted FIRST so it sorts first in consumption order.
	laundered := f.derive(valuedomain.OriginMarketTradingProceeds, 200_000_000,
		[]credit.LotParent{{LotID: grant.ID, Quantity: money.QuantityFromInt64(200_000_000),
			Finality: valuedomain.FinalityUnfunded}})
	// Honest proceeds out of the settled purchase.
	honest := f.derive(valuedomain.OriginMarketTradingProceeds, 200_000_000,
		[]credit.LotParent{{LotID: purchase.ID, Quantity: money.QuantityFromInt64(200_000_000),
			Finality: valuedomain.FinalitySettled}})

	require.Equal(t, valuedomain.OriginPromotional, floorOf(t, laundered.ID),
		"fixture check: D-131 gives the round trip a PROMOTIONAL floor")
	require.Equal(t, valuedomain.OriginPurchased, floorOf(t, honest.ID),
		"fixture check: the honest proceeds carry a PURCHASED floor")

	eligible, _, err := f.credits.EligibleLots(f.ctx, testDB, credit.BalanceRequest{
		AccountID:  f.account,
		Policy:     valuedomain.SandboxPolicy(),
		Verified:   valuedomain.VerificationPayoutKYC,
		ActiveCaps: map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Now:        f.clk.Now(),
	})
	require.NoError(t, err)
	ids := map[credit.LotID]bool{}
	for _, l := range eligible {
		ids[l.ID] = true
	}
	require.False(t, ids[laundered.ID],
		"fixture check: the promotional-floored proceeds are NOT payout-eligible")
	require.True(t, ids[honest.ID], "fixture check: the purchased-floored proceeds are")
	require.True(t, ids[purchase.ID], "fixture check: so is the settled purchase itself")

	// Ask for exactly what the engine approved: the settled purchase plus the
	// honest proceeds, 400 Credits. The promotional round trip is not in the
	// approved set and its 200 are not counted towards the request.
	req := f.createPayout(t, 400_000_000, "audit3-floor")
	require.Equal(t, payout.StateVerified, req.State)
	require.Equal(t, "400000000", req.ReservedQuantity.String())

	got := allocatedLots(t, req.ID)
	assert.NotContains(t, got, laundered.ID,
		"F-wv3-1(b): a lot whose ORIGIN FLOOR is PROMOTIONAL was reserved against a payout. "+
			"D-131 and CREDIT_ECONOMY.md §4 say promotional value 'can never leave this "+
			"system under any policy in this build'; the reservation filters by origin, "+
			"and MARKET_TRADING_PROCEEDS is one origin whatever funded it")
	assert.Contains(t, got, honest.ID,
		"F-wv3-1(b): the purchased-floored proceeds the decision approved were not the ones reserved")
}

// ---------------------------------------------------------------------------
// F-wv3-2 — the origin floor a lot is born with depends on the ORDER parent
// rows are inserted in, so a lot minted in the same transaction as its parent
// can be given a floor better than that parent's real one.
//
// 00816's trigger computes a child's floor from its parents' CURRENT
// credit_lot_state.origin_floor. A lot's own floor is opened at its OWN origin
// by cp_credit_lot_open and is only lowered when ITS parent rows land — and
// cp_credit_lot_parent_is_written_at_mint permits a parent row for any lot
// created in this transaction. So within one transaction the floor is mutable,
// and a grandchild whose parent row is written BEFORE its parent's own parent
// row reads a floor that has not fallen yet.
//
// F-266's recorded residual argues the opposite in as many words: "both derived
// rules take the WORST parent … so naming an EXTRA parent can only make a lot
// LESS withdrawable. The direction that could launder is OMITTING a parent."
// Nothing is omitted here; every true parent row is written, and the floor is
// still better than the provenance.
// ---------------------------------------------------------------------------

func TestAuditWV3_AFloorCannotDependOnTheOrderParentRowsWereInserted(t *testing.T) {
	f := newAuditFixture(t)

	grant := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 100_000_000)
	require.Equal(t, valuedomain.OriginPromotional, floorOf(t, grant.ID))

	// One transaction that mints a child of the grant and a child of THAT, the
	// way a chain of derived value inside a single settlement would. The only
	// thing the audit chooses is the order of the two parent rows.
	var child, grandchild credit.Lot
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			// Both lots exist before either gets its provenance. No Parents
			// field: the rows are written below, in the order this test is
			// about, exactly as a mint site writing a chain would.
			if child, err = f.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: f.account, Quantity: money.QuantityFromInt64(100_000_000),
				Origin:         valuedomain.OriginMarketTradingProceeds,
				Finality:       valuedomain.FinalityUnfunded,
				Reference:      credit.Reference{Type: "audit_wv3", ID: uuid.NewString()},
				IdempotencyKey: "audit3-chain-child-" + uuid.NewString(),
				Reason:         "wv3: a lot whose provenance is written in a moment",
				EffectiveAt:    f.clk.Now(),
			}); err != nil {
				return err
			}
			if grandchild, err = f.credits.Issue(ctx, tx, credit.IssueRequest{
				AccountID: f.account, Quantity: money.QuantityFromInt64(100_000_000),
				Origin:         valuedomain.OriginMarketTradingProceeds,
				Finality:       valuedomain.FinalityUnfunded,
				Reference:      credit.Reference{Type: "audit_wv3", ID: uuid.NewString()},
				IdempotencyKey: "audit3-chain-grandchild-" + uuid.NewString(),
				Reason:         "wv3: a child of a child",
				EffectiveAt:    f.clk.Now(),
			}); err != nil {
				return err
			}
			// The grandchild's parent row FIRST, while the child's floor is
			// still its own origin.
			if _, err = tx.Exec(ctx,
				`INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity)
				 VALUES ($1,$2,$3::numeric)`,
				grandchild.ID, child.ID, "100000000"); err != nil {
				return err
			}
			// ...and only then the child's own, which lowers ITS floor to the
			// grant and does not revisit the grandchild.
			_, err = tx.Exec(ctx,
				`INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity)
				 VALUES ($1,$2,$3::numeric)`,
				child.ID, grant.ID, "100000000")
			return err
		}))

	require.Equal(t, valuedomain.OriginPromotional, floorOf(t, child.ID),
		"fixture check: the child of a grant floors at PROMOTIONAL")
	assert.Equal(t, valuedomain.OriginPromotional, floorOf(t, grandchild.ID),
		"F-wv3-2: the grandchild of a promotional grant carries a "+
			"MARKET_TRADING_PROCEEDS floor because its parent row was written before its "+
			"parent's was. 00816 says a floor is 'the most restricted origin anywhere in "+
			"this value's provenance'; here it is computed from a parent's floor that had "+
			"not fallen yet, and nothing recomputes it afterwards")

	// And the consequence: SandboxPolicy releases it.
	ok, reasons := valuedomain.SandboxPolicy().Permits(valuedomain.PermitInput{
		Origin:      valuedomain.OriginMarketTradingProceeds,
		OriginFloor: floorOf(t, grandchild.ID),
		Finality:    valuedomain.FinalityUnfunded,
		Domain:      valuedomain.InternalCredit,
		Verified:    valuedomain.VerificationPayoutKYC,
		ActiveCaps:  map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		PolicyValid: true,
	})
	assert.False(t, ok,
		"F-wv3-2: value whose provenance bottoms out in a promotional grant is withdrawable "+
			"under SandboxPolicy; reasons=%v", reasons)
}

// ---------------------------------------------------------------------------
// F-wv3-5 — SettleDerived's freeze direction cannot see a lot at UNFUNDED, so
// a derived lot with one UNFUNDED parent is never frozen when another parent
// is charged back.
//
// D-124's freeze half: "a REVERSIBLE or SETTLED derived lot with a DISPUTED or
// REVERSED parent is moved to DISPUTED, which is neither spendable nor
// payout-eligible." The candidate predicate opens
// `st.finality IN ('REVERSIBLE','SETTLED')`, and valuedomain's finality
// ordering puts UNFUNDED between REVERSIBLE and SETTLED — so
// DerivedFinality({UNFUNDED, SETTLED}) is UNFUNDED, and a lot minted there is
// outside the predicate for ever.
//
// It is reached by an ordinary purchase. A buyer holding a grant and a settled
// card purchase spends across both; consumption order takes the grant first,
// so the seller's earning names both lots as parents and is minted UNFUNDED.
// The card is then charged back. Had the buyer held only the purchase, the
// earning would have been SETTLED and the sweep would have frozen it; because
// a grant was in the mix, nothing ever will.
//
// F-260 named this direction precisely: "Freezing starving is worse and
// quieter: an earning whose funding was charged back stays spendable."
// ---------------------------------------------------------------------------

func TestAuditWV3_ADerivedLotAtUnfundedIsStillFrozenByADisputedParent(t *testing.T) {
	f := newAuditFixture(t)

	grant := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 60_000_000)
	purchase := f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 40_000_000)

	// The seller's earning, funded by both, exactly as internal/commerce mints
	// one: the parents are the lots the buyer's spend consumed.
	earning := f.derive(valuedomain.OriginCreatorEarning, 100_000_000, []credit.LotParent{
		{LotID: grant.ID, Quantity: money.QuantityFromInt64(60_000_000),
			Finality: valuedomain.FinalityUnfunded},
		{LotID: purchase.ID, Quantity: money.QuantityFromInt64(40_000_000),
			Finality: valuedomain.FinalitySettled},
	})
	require.Equal(t, valuedomain.FinalityUnfunded, f.finalityOf(earning.ID),
		"fixture check: the least final parent is the UNFUNDED grant")

	// The card payment is charged back.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SetFinality(ctx, tx, purchase.ID, valuedomain.FinalityDisputed,
				credit.Reference{Type: "audit_wv3_chargeback", ID: uuid.NewString()},
				"the card issuer raised a dispute")
		}))
	require.Equal(t, valuedomain.FinalityDisputed, f.finalityOf(purchase.ID))

	// Every pass the deployment would ever run.
	for i := 0; i < 3; i++ {
		require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, serr := f.credits.SettleDerived(ctx, tx, 500)
				return serr
			}))
	}

	assert.Equal(t, valuedomain.FinalityDisputed, f.finalityOf(earning.ID),
		"F-wv3-5: an earning funded in part by a charged-back card payment is still %s. "+
			"D-124's freeze direction says a derived lot with a DISPUTED parent is moved to "+
			"DISPUTED; settleDerivedCandidates only ever looks at lots that are REVERSIBLE "+
			"or SETTLED, and this one was minted UNFUNDED because one of its parents was a "+
			"grant", f.finalityOf(earning.ID))
}

// ---------------------------------------------------------------------------
// Round three's probes at round two's fixes. These are expected to PASS on
// 4ce0299: each is an attempt to get around a fix F-259…F-267 made, and each
// records that the fix holds. They are committed so a later round can rerun
// them rather than reasoning about them.
// ---------------------------------------------------------------------------

// F-259's edge table refuses an edge that IS in the table but is not the one
// this destination is on: 00731's deferred binding compares the row's
// from_status with the status the destination was actually in.
func TestAuditWV3_ALegalEdgeStillCannotBeClaimedFromTheWrongStatus(t *testing.T) {
	f := newAuditFixture(t)

	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := f.svc.TransitionDestination(ctx, tx, f.destination, payout.DestinationDisabled,
				payout.DestinationChange{
					ActorType: security.ActorUser, ActorID: f.account.String(),
					Reason: "the holder removed it", OccurredAt: f.clk.Now().UTC(),
				})
			return terr
		}))

	// UNVERIFIED -> VERIFIED is a real edge. The destination is DISABLED.
	_, err := testDB.Exec(f.ctx, `INSERT INTO payout_destination_transitions
		  (id, destination_id, from_status, to_status, actor_type, actor_id, reason)
		VALUES ($1,$2,'UNVERIFIED','VERIFIED','SYSTEM','audit-probe','a legal edge claimed from the wrong place')`,
		uuid.New(), f.destination)
	assert.Error(t, err, "a transition row may claim an edge the destination is not standing on")

	after, gerr := f.svc.Destination(f.ctx, testDB, f.destination)
	require.NoError(t, gerr)
	assert.Equal(t, payout.DestinationDisabled, after.Status)
	assert.False(t, after.Status.Usable())
}

// F-264's payout_requests_reservation_backed is anchored at the request, so a
// reservation with no allocations behind it is refused at COMMIT whichever edge
// carries it -- not only the same-state row the round-two test drives.
func TestAuditWV3_ALegalEdgeCannotCarryAReservationWithNoAllocations(t *testing.T) {
	f := newAuditFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 500_000_000)

	// REVERSIBLE value is eligible for nothing, so the request lands REJECTED
	// with no reservation and no allocations.
	req := f.createPayout(t, 500_000_000, "audit3-po001")
	require.Equal(t, payout.StateRejected, req.State)
	require.Zero(t, countAllocations(t, req.ID))

	_, err := testDB.Exec(f.ctx, `INSERT INTO payout_request_transitions
		  (id, request_id, from_state, to_state, actor_type, actor_id, reason, reserved_quantity)
		VALUES ($1,$2,'ELIGIBILITY_CHECK','VERIFIED','SYSTEM','audit-probe','a reservation with nothing behind it',$3::numeric)`,
		uuid.New(), req.ID, "500000000")
	assert.Error(t, err, "a reserved_quantity was written with no allocation rows behind it")

	var reserved string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT reserved_quantity::text FROM payout_requests WHERE id = $1`, req.ID).Scan(&reserved))
	assert.Equal(t, "0", reserved)
}

// F-266's CR005 refuses a parent row written outside the creating transaction,
// which is the direction that laundered a card payment.
func TestAuditWV3_AParentRowStillCannotBeWrittenAfterTheMint(t *testing.T) {
	f := newAuditFixture(t)
	parent := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 10_000_000)
	child := f.issue(valuedomain.OriginPurchased, valuedomain.FinalityReversible, 10_000_000)

	_, err := testDB.Exec(f.ctx,
		`INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity) VALUES ($1,$2,$3::numeric)`,
		child.ID, parent.ID, "10000000")
	assert.Error(t, err, "a lot minted in an earlier transaction was given a parent")
	assert.Equal(t, valuedomain.OriginPurchased, floorOf(t, child.ID), "and its floor did not move")
}

// The residual D-121 records, confirmed rather than assumed: a same-state
// compliance row may still change the SANCTIONS SCREEN, there is no legal-edge
// table for that column in Go or in SQL, and the column is one
// payout.restricted() reads before a conversion request may proceed.
func TestAuditWV3_TheSanctionsScreenResidualIsStillReachable(t *testing.T) {
	f := newAuditFixture(t)
	svc, repo := newAuditVerificationService(t, f.clk)
	session := newAuditSession(t, repo, f.user)
	_, err := svc.Ingest(f.ctx, testDB, session, verification.Result{
		ProviderRef: session.ProviderRef,
		Status:      verification.SessionApproved,
		RawStatus:   "audit_approved",
		Sandbox:     true,
		AgeAtLeast:  verifysandbox.AttestsAgeAtLeast,
		Jurisdiction: rules.Jurisdiction{
			Country: session.JurisdictionCountry, Region: session.JurisdictionRegion,
		},
		Checks: []verification.CheckResult{
			{Kind: verification.CheckIdentityDocument, Outcome: verification.OutcomePass, Detail: "AUDIT"},
			{Kind: verification.CheckAge, Outcome: verification.OutcomePass, Detail: "AUDIT"},
			{Kind: verification.CheckJurisdiction, Outcome: verification.OutcomePass, Detail: "AUDIT"},
			{Kind: verification.CheckSanctions, Outcome: verification.OutcomeFail, Detail: "AUDIT"},
		},
	}, "audit fixture: a provider decision with a sanctions failure", "audit3")
	require.NoError(t, err)

	var screen string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT sanctions_state FROM compliance_profiles WHERE user_id = $1`, f.user).Scan(&screen))
	require.NotEqual(t, "CLEAR", screen, "fixture check: the provider's screen is not clear")

	// One INSERT as cp_app, on the one path 00815 still allows.
	_, err = testDB.Exec(f.ctx, `INSERT INTO compliance_profile_transitions
		  (id, user_id, from_state, to_state, from_sanctions_state, to_sanctions_state,
		   actor_type, actor_id, reason)
		SELECT $1, user_id, identity_state, identity_state, sanctions_state, 'CLEAR',
		       'SYSTEM', 'audit-probe', 'a row that moves no state and clears a screen'
		  FROM compliance_profiles WHERE user_id = $2`, uuid.New(), f.user)
	require.NoError(t, err, "D-121's residual: the same-state row carrying a screen is legal")

	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT sanctions_state FROM compliance_profiles WHERE user_id = $1`, f.user).Scan(&screen))
	assert.Equal(t, "CLEAR", screen,
		"recorded, not asserted as correct: the sanctions screen is the fifth state column in "+
			"this area carried on a transition row, and the only one with no legal-edge table "+
			"in Go or SQL; payout.restricted() and eligibility.applyAccountFacts both read it")
}
