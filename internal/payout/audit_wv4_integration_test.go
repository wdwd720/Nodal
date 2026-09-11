//go:build integration

package payout_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Reproductions for the FOURTH round of the withdrawal-verification audit
// (goal §54). The round is narrow: it audits the THIRD round's fix — D-136,
// D-137, D-138, D-139 and the four smaller repairs — rather than the area.
// Nothing here changes product code.

// ---------------------------------------------------------------------------
// F-wv4-1 — a derived lot frozen because its funding was disputed is never
// thawed when the dispute is resolved in the platform's favour. It is neither
// spendable nor payout-eligible, for ever, and the eligibility page tells its
// holder that waiting will fix it.
//
// D-124's freeze half moves a derived lot to DISPUTED when a parent is DISPUTED
// or REVERSED. D-094 built the mirror for FUNDED value: `UnfreezeFunding`
// advances the funding to REVERSIBLE and "unfreezes the lot with it, the mirror
// of DisputeFunding". There is no mirror for the lots the DERIVED sweep froze.
//
// `SettleDerived`'s promotion clause opens `st.finality = 'REVERSIBLE'`, and a
// frozen child is at DISPUTED; its freeze clause opens "not already frozen", and
// a frozen child is. So a frozen child matches neither clause and is not a
// candidate in any pass. Nothing else writes a lot's finality: `SettleFunding`,
// `UnfreezeFunding` and `DisputeFunding` all key on `credit_fundings.lot_id`,
// which a derived lot has never had (F-230), and no operator route moves a
// lot's finality at all. `credit_lot_state` is a trigger-written projection
// cp_app may only SELECT, so not even a hand-written UPDATE as the application
// role can reach it.
//
// Round three did not introduce this. It made it strictly more reachable: F-273
// widened the freeze clause from "a REVERSIBLE or SETTLED derived lot" to "any
// derived lot that is not itself frozen", which adds every UNFUNDED derived lot
// — the ones funded partly by a grant, which is the ordinary case D-124 names —
// to the population that can be frozen and can never come back.
//
// The user-facing half is F-230's defect restored word for word.
// `WithdrawalFundingNotSettled`'s own declaration says "Waiting fixes it, which
// is why it is a reason of its own rather than folded into the origin", and
// records that this sentence "was false for every earning in this system until
// D-124". It is false again, on this branch.
// ---------------------------------------------------------------------------

func TestAuditWV4_ADerivedLotFrozenByADisputeIsNeverThawedWhenTheDisputeIsWon(t *testing.T) {
	f := newAuditFixture(t)

	// A card purchase with a credit_fundings row behind it, so the dispute,
	// unfreeze and settle paths can move it. 600 Credits.
	fundingID, purchase := f.fundedPurchase(t, 600_000_000)
	require.Equal(t, valuedomain.FinalityReversible, f.finalityOf(purchase),
		"fixture check: a minted funding's lot is REVERSIBLE")

	// A seller's earning derived from it, and a creator fee derived from THAT,
	// so the freeze has a second level to reach.
	earning := f.derive(valuedomain.OriginMarketTradingProceeds, 300_000_000,
		[]credit.LotParent{{
			LotID: purchase, Quantity: money.QuantityFromInt64(300_000_000),
			Finality: valuedomain.FinalityReversible,
		}})
	grand := f.derive(valuedomain.OriginMarketCreatorEarning, 100_000_000,
		[]credit.LotParent{{
			LotID: earning.ID, Quantity: money.QuantityFromInt64(100_000_000),
			Finality: valuedomain.FinalityReversible,
		}})

	// The cardholder disputes. The sweep freezes the earning on the first pass
	// and the grandchild on the second: one level per pass, which is F-273's
	// fix reaching a child of a child and is not the defect.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.DisputeFunding(ctx, tx, fundingID, "the cardholder disputed the charge")
		}))
	f.sweepDerived(t, 3)
	require.Equal(t, valuedomain.FinalityDisputed, f.finalityOf(earning.ID),
		"fixture check: the earning freezes")
	require.Equal(t, valuedomain.FinalityDisputed, f.finalityOf(grand.ID),
		"fixture check: and so does the lot derived from the earning, one pass later")

	// The dispute is resolved in the platform's favour. D-094: winning a dispute
	// returns the funding to its window; the window then closes and it settles.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.UnfreezeFunding(ctx, tx, fundingID, "the dispute was resolved in our favour")
		}))
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SettleFunding(ctx, tx, fundingID, "and then the window closed")
		}))
	require.Equal(t, valuedomain.FinalitySettled, f.finalityOf(purchase),
		"fixture check: the money that funded the earning is now as final as money gets")

	// Every sweep pass from here. The candidate predicate does not select a
	// frozen lot in either direction, so no number of passes moves them.
	f.sweepDerived(t, 10)

	assert.Equal(t, valuedomain.FinalitySettled, f.finalityOf(earning.ID),
		"F-wv4-1: the card that funded this earning has SETTLED. The dispute that froze it was "+
			"resolved in the platform's favour and the funding went REVERSIBLE and then SETTLED, "+
			"which is D-094's mirror of DisputeFunding. The earning stays DISPUTED: "+
			"SettleDerived's promotion clause opens `st.finality = 'REVERSIBLE'` and its freeze "+
			"clause opens `not already frozen`, so a frozen derived lot matches neither, and no "+
			"other writer can move a lot with no credit_fundings row. It is neither spendable nor "+
			"payout-eligible and nothing in this system can ever make it either")
	assert.Equal(t, valuedomain.FinalitySettled, f.finalityOf(grand.ID),
		"F-wv4-1: and neither can the lot derived from it")

	// What the holder is told about it.
	bal, err := f.credits.Balances(f.ctx, testDB, credit.BalanceRequest{
		AccountID: f.account, Policy: valuedomain.SandboxPolicy(),
		Verified:   valuedomain.VerificationPayoutKYC,
		ActiveCaps: map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Now:        f.clk.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, "0", bal.Frozen.String(),
		"F-wv4-1: 400 Credits of a seller's earnings are reported FROZEN after a dispute the "+
			"platform won, and nothing will ever unfreeze them")
	assert.Equal(t, "1000000000", bal.Spendable.String(),
		"F-wv4-1: and they are not spendable either — DISPUTED is neither Spendable() nor "+
			"PayoutEligible(), so the value is inaccessible in both directions")
	assert.Zero(t, bal.IneligibleReasons[valuedomain.ReasonFundingNotFinal],
		"F-wv4-1: the reason reported is FUNDING_NOT_FINAL, which eligibility renders as "+
			"FUNDING_NOT_SETTLED — the reason whose own declaration says 'Waiting fixes it, which "+
			"is why it is a reason of its own'. That sentence is F-230's, and it is false again")
}

// fundedPurchase walks a funding to REVERSIBLE the ordinary way -- created,
// captured, minted -- so the dispute, unfreeze and settle paths can move it.
func (f *auditFixture) fundedPurchase(t *testing.T, qty int64) (credit.FundingID, credit.LotID) {
	t.Helper()
	var fnd credit.Funding
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			fnd, err = f.credits.CreateFunding(ctx, tx, credit.CreateFundingRequest{
				AccountID: f.account, Provider: "audit-wv4-provider", ProviderMode: "fake",
				CreditQuantity: money.QuantityFromInt64(qty),
				PaidAmount:     money.USDFromMinor(qty),
				IdempotencyKey: "audit-wv4-funding-" + uuid.NewString(),
			})
			return err
		}))
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.credits.AdvanceFunding(ctx, tx, fnd.ID, credit.FundingCaptured,
				"the provider captured the payment", "")
			return err
		}))
	var lot credit.Lot
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			lot, err = f.credits.MintFrom(ctx, tx, fnd.ID, f.clk.Now())
			return err
		}))
	return fnd.ID, lot.ID
}

// sweepDerived runs the derived settlement sweep the way cmd/api's ticker does.
func (f *auditFixture) sweepDerived(t *testing.T, passes int) {
	t.Helper()
	for i := 0; i < passes; i++ {
		require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.credits.SettleDerived(ctx, tx, 100)
				return err
			}))
	}
}

// ---------------------------------------------------------------------------
// F-wv4-2 — a payout that was blocked and then CANCELLED still renders "this
// withdrawal cannot be sent … its Credits are still reserved and still yours"
// beside a Reserved figure of zero.
//
// D-139 records the refusal on the request and leaves it there after a
// cancellation, deliberately: "why it could not be sent is part of its history",
// and internal/payout's own test asserts it. The record is right. What is not
// right is the reading: `toAPIPayout` emits `blocked_reason` whenever the string
// is non-empty, with no reference to the state, and `Withdraw.tsx` renders it
// under `{data.blocked_reason !== undefined && data.blocked_reason !== ""}` with
// two sentences of present-tense copy — "Its Credits are still reserved and
// still yours. Cancelling is what releases them" — above a Cancel control that
// `isCancellable` has already removed, because the request is REJECTED.
//
// So one panel says Reserved 0, State REJECTED, and "its Credits are still
// reserved". That is two contradictory statements about one pot of money in one
// payload, which is F-272 exactly, one surface along.
//
// The same test records the second half of the question the fourth round was
// asked: 00822 grants cp_app UPDATE on (blocked_reason, blocked_at) with no
// state predicate in the database. `recordBlocked` carries `AND state =
// 'VERIFIED'` in its WHERE; nothing else does, so the pairing CHECK is the only
// thing the column is held to.
// ---------------------------------------------------------------------------

func TestAuditWV4_ACancelledPayoutStillTellsItsHolderItsCreditsAreReserved(t *testing.T) {
	f := newAuditFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 1_000_000_000)
	req := f.createPayout(t, 500_000_000, "audit-wv4-blocked-cancel")
	require.Equal(t, payout.StateVerified, req.State)

	// The holder removes the destination, which is what strands the request.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := f.svc.TransitionDestination(ctx, tx, f.destination, payout.DestinationDisabled,
				payout.DestinationChange{
					ActorType: security.ActorUser, ActorID: f.account.String(),
					Reason: "this destination was closed", OccurredAt: f.clk.Now().UTC(),
				})
			return terr
		}))
	_, serr := f.svc.Submit(f.ctx, testDB, req.ID, f.provider.Name())
	require.Error(t, serr, "fixture check: F-263's refusal still fires")

	blocked, err := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	require.True(t, blocked.Blocked(), "fixture check: D-139 records the reason")
	require.Contains(t, blocked.BlockedReason, "Cancel it to release the Credits it reserved")

	// The holder does exactly what the sentence told them to do.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, cerr := f.svc.Cancel(ctx, tx, req.ID, "the destination is gone, as the page said")
			return cerr
		}))
	after, err := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, err)

	require.Equal(t, payout.StateRejected, after.State, "fixture check: cancelling rejects it")
	require.Equal(t, "0", after.ReservedQuantity.String(),
		"fixture check: and returns the reservation, which is what the sentence promised")

	assert.False(t, after.Blocked(),
		"F-wv4-2: the request is REJECTED with a reserved quantity of 0, and it still carries "+
			"blocked_reason=%q. `toAPIPayout` emits the field on any state, and Withdraw.tsx "+
			"renders it with the copy 'Its Credits are still reserved and still yours. Cancelling "+
			"is what releases them' — beside a Reserved field showing 0, under a State badge "+
			"reading REJECTED, and above a Cancel control isCancellable has removed. A payout's "+
			"history is the right thing to keep; rendering it in the present tense is not",
		after.BlockedReason)

	// And the column itself. This assertion is INVERTED from the reproduction as
	// written, and the narrative is the auditor's: 00822 granted cp_app UPDATE
	// (blocked_reason, blocked_at) with no state predicate in the database,
	// `recordBlocked` carries `AND state = 'VERIFIED'` in its WHERE and nothing
	// else does, so the only rule the column was held to was that a reason and
	// an instant exist together. 00823 puts the rule in the schema.
	//
	// The reproduction expected the write to affect no rows and report no error,
	// which is what a BEFORE trigger returning NULL does. The fix RAISES
	// instead: a write that silently does nothing is how a caller comes to
	// believe something was recorded, and every other protection on this schema
	// refuses out loud (00807, 00819). A refusal is the stronger of the two
	// shapes, so the assertion tests for it (F-279, D-139 amended).
	_, uerr := testDB.Exec(f.ctx,
		`UPDATE payout_requests SET blocked_reason = $2, blocked_at = now() WHERE id = $1`,
		req.ID, "a reason written straight onto a rejected request by the application role")
	require.Error(t, uerr,
		"F-wv4-2: cp_app must not be able to stamp a blocked reason onto a payout that has "+
			"finished moving")
	assert.Equal(t, "AD001", db.SQLState(uerr), "got %v", uerr)
	assert.Contains(t, uerr.Error(), "PAYOUT_BLOCKED_REASON_ON_FINISHED")

	// The reason it already carries is untouched: the history stays, and only
	// the reading of it is state-scoped.
	stillRecorded, gerr := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, gerr)
	assert.NotEmpty(t, stillRecorded.BlockedReason,
		"D-139: why somebody's reserved value could not be sent is part of its history")
}

// ---------------------------------------------------------------------------
// F-wv4-4 — an EMPTY lot restriction is no restriction at all, and the
// assertion D-136 added to catch exactly this does not run either.
//
// `openLotsQuery` reads `($5::uuid[] IS NULL OR cardinality($5::uuid[]) = 0 OR
// l.id = ANY($5::uuid[]))`, so an empty set selects every lot; and `Consume`
// guards its post-selection assertion with `if len(r.LotIDs) > 0`, so the
// belt-and-braces check is absent on precisely the input where the filter is.
// `reserve` passes `credit.EligibleLotIDs(d.Lots)`, which is empty when a
// decision approved no lots.
//
// It is not reachable through `Create` or `CompleteVerification` today, because
// `Decision.Sufficient()` refuses a decision whose eligible total is below the
// request and an empty lot list cannot cover a positive one. That is one
// guard in a different package standing between an empty set and a consume that
// takes a promotional grant, and D-136's stated design is that the restriction
// "is a parameter of the one constant statement `openLotsQuery` is" and that
// `Consume` "asserts on the way out that every lot it selected is in the set".
// Neither sentence is true of the empty set.
// ---------------------------------------------------------------------------

func TestAuditWV4_AnEmptyLotRestrictionIsNoRestrictionAndTheAssertionDoesNotFire(t *testing.T) {
	f := newAuditFixture(t)
	grant := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 600_000_000)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 600_000_000)

	// The set a decision that approved nothing produces.
	require.Empty(t, credit.EligibleLotIDs(nil),
		"fixture check: an empty decision produces an empty lot set")

	var allocs []credit.Allocation
	cerr := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			post, perr := f.wv4Post(ctx, tx, 100_000_000)
			if perr != nil {
				return perr
			}
			var err error
			allocs, err = f.credits.Consume(ctx, tx, credit.ConsumeRequest{
				AccountID: f.account, Quantity: money.QuantityFromInt64(100_000_000),
				JournalTxID: post,
				Reference:   credit.Reference{Type: "payout_request", ID: uuid.NewString()},
				Reason:      "reserved against a payout request",
				// Exactly what `payout.Service.reserve` passes.
				RequireSpendableFinality: true,
				RequirePayoutFinality:    true,
				RestrictToLots:           true,
				LotIDs:                   credit.EligibleLotIDs(nil),
			})
			return err
		})

	require.Error(t, cerr,
		"F-wv4-4: a consume restricted to the lots of a decision that approved NONE took %d "+
			"allocation(s) — the first from lot %s. An empty set is read as 'no restriction' by "+
			"`openLotsQuery`, and `Consume`'s post-selection assertion is guarded by "+
			"`len(r.LotIDs) > 0`, so the one input on which the filter is absent is also the one "+
			"on which the assertion is. The lot it took is a PROMOTIONAL grant, which "+
			"CREDIT_ECONOMY.md §4 says 'can never leave this system under any policy in this "+
			"build' and which no payout decision could ever approve",
		len(allocs), firstLotOf(allocs))
	_ = grant
}

func firstLotOf(allocs []credit.Allocation) string {
	if len(allocs) == 0 {
		return "<none>"
	}
	return allocs[0].LotID.String() + " (" + string(allocs[0].Origin) + ")"
}

// ---------------------------------------------------------------------------
// F-wv4-5 — `payout_allocations` records the origin and the FLOOR, and D-138
// made the permission answer the ROOT SET. Two lots whose sets differ and whose
// floors do not are recorded identically and reported as one provenance slice.
//
// 00820 says the column exists for "an audit of a settled payout, which is the
// only reader that can ever answer 'what actually left'". Under D-138 what
// actually left is described by the set: `Policy.Permits` releases a lot only
// when the policy releases "the lot's own origin AND every root", and
// `Policy.RefusedRoot` exists because the root a policy refuses "is not always
// the one this build's rank calls the most restricted".
//
// The floor is the most restricted root, so two different sets share a floor
// whenever they share a minimum — {CREATOR_EARNING} and {CREATOR_EARNING,
// PURCHASED} both floor at CREATOR_EARNING. Under the policy B-02 can come back
// with — the closed-loop float is stored value, the gains are not — the first is
// released and the second refused, and `GET /v1/payouts/{id}` renders them as
// one line.
//
// It is a REPORTING gap rather than a lost fact: `payout_allocations.lot_id`
// references `credit_lots`, a lot's root set never moves once written, so the
// set is one join away for anyone who knows to make it. What the fold shows a
// customer, and what an auditor reading the table alone would conclude, is one
// provenance where there are two.
// ---------------------------------------------------------------------------

func TestAuditWV4_TheAllocationRecordFoldsTwoRootSetsIntoOneProvenance(t *testing.T) {
	f := newAuditFixture(t)
	earning := f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 800_000_000)
	purchase := f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 800_000_000)

	// Same origin, same floor, different root sets.
	onlyEarned := f.derive(valuedomain.OriginMarketTradingProceeds, 300_000_000,
		[]credit.LotParent{{
			LotID: earning.ID, Quantity: money.QuantityFromInt64(300_000_000),
			Finality: valuedomain.FinalitySettled,
		}})
	halfPurchased := f.derive(valuedomain.OriginMarketTradingProceeds, 300_000_000,
		[]credit.LotParent{
			{
				LotID: earning.ID, Quantity: money.QuantityFromInt64(150_000_000),
				Finality: valuedomain.FinalitySettled,
			},
			{
				LotID: purchase.ID, Quantity: money.QuantityFromInt64(150_000_000),
				Finality: valuedomain.FinalitySettled,
			},
		})
	require.Equal(t, []valuedomain.CreditOrigin{valuedomain.OriginCreatorEarning},
		rootsOf(t, onlyEarned.ID), "fixture check")
	require.Equal(t, []valuedomain.CreditOrigin{
		valuedomain.OriginCreatorEarning, valuedomain.OriginPurchased,
	},
		rootsOf(t, halfPurchased.ID), "fixture check")
	require.Equal(t, floorOf(t, onlyEarned.ID), floorOf(t, halfPurchased.ID),
		"fixture check: the two provenances are indistinguishable by (origin, floor)")

	// Spend the two funding lots down so the payout draws on the proceeds.
	f.consumeAll(t, earning.ID)
	f.consumeAll(t, purchase.ID)

	req := f.createPayout(t, 600_000_000, "audit-wv4-rootsets")
	require.Equal(t, payout.StateVerified, req.State)

	slices, err := f.svc.Provenance(f.ctx, testDB, req.ID)
	require.NoError(t, err)
	var outstanding []payout.ProvenanceSlice
	for _, s := range slices {
		if !s.Returned {
			outstanding = append(outstanding, s)
		}
	}
	// INVERTED from the reproduction, whose fixture check was that both lots
	// folded into one (origin, floor). The fold keys on the root set as well
	// now, so two provenances are two slices and the sum is unchanged (D-141,
	// F-282).
	require.Len(t, outstanding, 2,
		"two root sets behind one floor are two provenances, not one line of MARKET_TRADING_PROCEEDS")
	total := money.Quantity{}
	for _, slice := range outstanding {
		assert.Equal(t, "300000000", slice.Quantity.String())
		assert.NotEmpty(t, slice.RootOrigins, "each slice says what is behind it")
		total = total.Add(slice.Quantity)
	}
	require.Equal(t, "600000000", total.String(), "and together they are the whole payout")
	require.NotEqual(t, outstanding[0].RootOrigins, outstanding[1].RootOrigins,
		"which is what tells them apart: the floors are equal and the sets are not")

	// What the table itself holds about the two lots.
	rows, qerr := testDB.Query(f.ctx,
		`SELECT a.lot_id, a.origin, a.origin_floor, st.root_origins
		   FROM payout_allocations a JOIN credit_lot_state st ON st.lot_id = a.lot_id
		  WHERE a.request_id = $1 ORDER BY a.lot_id`, req.ID)
	require.NoError(t, qerr)
	type record struct {
		lot         credit.LotID
		origin      string
		floor       string
		rootsOnJoin []string
	}
	var records []record
	for rows.Next() {
		var r record
		require.NoError(t, rows.Scan(&r.lot, &r.origin, &r.floor, &r.rootsOnJoin))
		records = append(records, r)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	require.Len(t, records, 2, "fixture check: two lots were allocated")
	require.Equal(t, records[0].floor, records[1].floor)
	require.NotEqual(t, records[0].rootsOnJoin, records[1].rootsOnJoin,
		"fixture check: and the two provenances really are different")

	// And the table itself holds the set, so a reader of the record alone --
	// 00820's "only reader that can ever answer what actually left" -- can tell
	// the two apart without knowing to join.
	var recordedSets [][]string
	rows2, qerr2 := testDB.Query(f.ctx,
		`SELECT root_origins FROM payout_allocations WHERE request_id = $1 ORDER BY lot_id`, req.ID)
	require.NoError(t, qerr2)
	for rows2.Next() {
		var set []string
		require.NoError(t, rows2.Scan(&set))
		recordedSets = append(recordedSets, set)
	}
	rows2.Close()
	require.NoError(t, rows2.Err())
	require.Len(t, recordedSets, 2)
	assert.NotEqual(t, recordedSets[0], recordedSets[1],
		"the record distinguishes what the join distinguishes")

	var hasRootColumn bool
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                 WHERE table_name = 'payout_allocations' AND column_name = 'root_origins')`).
		Scan(&hasRootColumn))
	assert.True(t, hasRootColumn,
		"F-wv4-5: the two lots this payout drew on carry root sets %v and %v and are recorded "+
			"identically as (origin=%s, origin_floor=%s), then folded into one provenance slice "+
			"by GET /v1/payouts/{id}. D-138 made the permission answer the SET; 00820 records the "+
			"floor because D-136's own words are that a payout's record must say 'what left as "+
			"what it was'. Under the policy B-02 can come back with, one of these may leave and "+
			"the other may not, and the record and the page say they are the same value",
		records[0].rootsOnJoin, records[1].rootsOnJoin, records[0].origin, records[0].floor)
}

// consumeAll spends a lot to zero so a later payout cannot draw on it.
func (f *auditFixture) consumeAll(t *testing.T, lot credit.LotID) {
	t.Helper()
	var remaining money.Quantity
	var raw string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT remaining_quantity::text FROM credit_lot_state WHERE lot_id = $1`, lot).Scan(&raw))
	var perr error
	remaining, perr = money.ParseQuantity(raw)
	require.NoError(t, perr)
	if !remaining.IsPositive() {
		return
	}
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			post, err := f.wv4Post(ctx, tx, 1)
			if err != nil {
				return err
			}
			_, err = f.credits.Consume(ctx, tx, credit.ConsumeRequest{
				AccountID: f.account, Quantity: remaining, JournalTxID: post,
				Reference:                credit.Reference{Type: "audit_wv4_drain", ID: uuid.NewString()},
				Reason:                   "wv4: spend a funding lot so the payout draws on its proceeds",
				RequireSpendableFinality: true,
				RestrictToLots:           true,
				LotIDs:                   []credit.LotID{lot},
			})
			return err
		}))
}

// wv4Post mints a throwaway lot and returns the journal transaction that moved
// it, which is what a Consume in the same transaction must be hung off.
func (f *auditFixture) wv4Post(ctx context.Context, tx pgx.Tx, qty int64) (ledger.TransactionID, error) {
	lot, err := f.credits.Issue(ctx, tx, credit.IssueRequest{
		AccountID: f.account, Quantity: money.QuantityFromInt64(qty),
		Origin: valuedomain.OriginAdminAdjustment, Finality: valuedomain.FinalityUnfunded,
		Reference:      credit.Reference{Type: "audit_wv4_post", ID: uuid.NewString()},
		IdempotencyKey: "audit-wv4-post-" + uuid.NewString(),
		Reason:         "wv4: a journal transaction to hang a consume off",
		EffectiveAt:    f.clk.Now(),
	})
	if err != nil {
		return ledger.TransactionID{}, err
	}
	return lot.JournalTxID, nil
}

// ---------------------------------------------------------------------------
// F-wv4-6 — 00819's recursive root computation answers NOTHING for a lot whose
// provenance contains a cycle, and its own backfill reads that as "this lot has
// no parents" and writes the lot's OWN origin as its whole root set.
//
// `cp_credit_lot_root_origins` selects the reachable nodes that are nobody's
// child; in a cycle every node is somebody's child, so `array_agg` over an empty
// set is NULL. The `UNION` makes it terminate rather than recurse, which is what
// the migration's comment promises and which this test confirms.
//
// The backfill then reads:
//
//	SET root_origins = coalesce(cp_credit_lot_root_origins(st.lot_id),
//	                            ARRAY[(SELECT l.origin FROM credit_lots l WHERE l.id = st.lot_id)])
//
// The fallback is the mint-time rule — "a lot with no parents is its own root" —
// applied to a lot that HAS parents and whose provenance is unknowable. For a
// lot of MARKET_TRADING_PROCEEDS with a promotional grant somewhere in a cycle
// behind it, that writes root_origins = {MARKET_TRADING_PROCEEDS} and floors it
// there, and `SandboxPolicy` releases it. It is the permissive direction, in the
// one migration whose entire subject is that the floor must be conservative.
//
// The trigger path fails closed and is not affected: writing a parent row for
// such a lot raises CREDIT_PARENT_ROOTLESS. Only the backfill substitutes.
//
// A cycle is unwritable through 00819's triggers, which this test also confirms.
// It is reachable only in data restored from a deployment that ran on 00816 to
// 00818, where `cp_credit_lot_parent_has_no_descendant_yet` did not exist and
// two lots minted in one transaction could name each other — which is why the
// migration handles the case at all.
// ---------------------------------------------------------------------------

func TestAuditWV4_ACycleMakesTheBackfillAnswerTheLotsOwnOrigin(t *testing.T) {
	f := newAuditFixture(t)
	grant := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 900_000_000)
	proceeds := f.derive(valuedomain.OriginMarketTradingProceeds, 300_000_000,
		[]credit.LotParent{{
			LotID: grant.ID, Quantity: money.QuantityFromInt64(300_000_000),
			Finality: valuedomain.FinalityUnfunded,
		}})
	require.Equal(t, valuedomain.OriginPromotional, floorOf(t, proceeds.ID),
		"fixture check: the round trip floors at the grant")

	// A cycle is unwritable through the triggers.
	_, cycErr := testDB.Exec(f.ctx,
		`INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity) VALUES ($1,$2,$3::numeric)`,
		grant.ID, proceeds.ID, "1")
	require.Error(t, cycErr, "00819's triggers must refuse the edge that closes a cycle")
	assert.Contains(t, cycErr.Error(), "CR005")

	// The shape a database restored from before 00819 can hold.
	mig, err := db.Open(f.ctx, db.Config{URL: testMigrateURL, AppName: "audit-wv4-migrate", MaxConns: 2})
	require.NoError(t, err)
	defer mig.Close()
	_, err = mig.Exec(f.ctx, `ALTER TABLE credit_lot_parents DISABLE TRIGGER USER`)
	require.NoError(t, err)
	_, err = mig.Exec(f.ctx,
		`INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity) VALUES ($1,$2,$3::numeric)`,
		grant.ID, proceeds.ID, "1")
	require.NoError(t, err)
	_, err = mig.Exec(f.ctx, `ALTER TABLE credit_lot_parents ENABLE TRIGGER USER`)
	require.NoError(t, err)
	defer func() {
		_, _ = mig.Exec(f.ctx, `ALTER TABLE credit_lot_parents DISABLE TRIGGER USER`)
		_, _ = mig.Exec(f.ctx, `DELETE FROM credit_lot_parents WHERE lot_id = $1 AND parent_lot_id = $2`,
			grant.ID, proceeds.ID)
		_, _ = mig.Exec(f.ctx, `ALTER TABLE credit_lot_parents ENABLE TRIGGER USER`)
	}()

	// The recursion terminates, as the migration's UNION promises.
	var roots []string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT cp_credit_lot_root_origins($1)`, proceeds.ID).Scan(&roots))
	assert.Empty(t, roots,
		"the recursion terminates on a cycle rather than recursing, and answers nothing")

	// And the trigger path fails closed on it.
	_, terr := testDB.Exec(f.ctx,
		`INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity) VALUES ($1,$2,$3::numeric)`,
		proceeds.ID, grant.ID, "1")
	require.Error(t, terr, "a further parent row for a lot in a cycle must be refused")

	// What 00819's own backfill expression would write for it.
	var backfilled []string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT coalesce(cp_credit_lot_root_origins($1),
		                 ARRAY[(SELECT l.origin FROM credit_lots l WHERE l.id = $1)])`,
		proceeds.ID).Scan(&backfilled))
	var backfilledFloor string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT o FROM unnest($1::text[]) AS o ORDER BY cp_credit_origin_floor_rank(o), o LIMIT 1`,
		backfilled).Scan(&backfilledFloor))

	// INVERTED from the reproduction, which asserted that the backfill's answer
	// was not the permissive one. It is, and it always will be: a migration that
	// has been applied is not rewritten, and the expression above is 00819's own
	// text. The narrative is the auditor's -- the coalesce turns a provenance the
	// database cannot compute into the lot's OWN origin, which for a round trip
	// out of a promotional grant is the answer SandboxPolicy releases, in the
	// migration whose subject is that a floor must be conservative.
	//
	// What changes is that the expression can no longer be reached with such a
	// row in the table. 00825 refuses to migrate while any lot has parent rows
	// and no computable root, because a migration that cannot compute a
	// provenance has two honest options and writing down a guess is not one of
	// them (F-283, D-137 amended).
	assert.Equal(t, string(valuedomain.OriginMarketTradingProceeds), backfilledFloor,
		"F-wv4-6: 00819's backfill reads a NULL root set as 'this lot has no parents' and would "+
			"write the lot's OWN origin: root_origins=%v, origin_floor=%s. That is why a database "+
			"holding one may not be migrated, rather than migrated with a guess",
		backfilled, backfilledFloor)

	var unknowable []credit.LotID
	rows3, qerr3 := testDB.Query(f.ctx, `SELECT * FROM cp_credit_lots_without_computable_roots()`)
	require.NoError(t, qerr3)
	for rows3.Next() {
		var lot credit.LotID
		require.NoError(t, rows3.Scan(&lot))
		unknowable = append(unknowable, lot)
	}
	rows3.Close()
	require.NoError(t, rows3.Err())
	assert.Contains(t, unknowable, proceeds.ID,
		"F-wv4-6: 00825 names every lot whose provenance is unknowable and refuses to move the "+
			"schema while there is one. The trigger path already raised CREDIT_PARENT_ROOTLESS on "+
			"the same input; the backfill was the one reader that substituted")
	assert.Contains(t, unknowable, grant.ID)
}

// ---------------------------------------------------------------------------
// Verified sound. These are attacks on the third round's fix that did not work;
// they are kept because a fifth round should not have to re-run them.
// ---------------------------------------------------------------------------

// D-136's residual 3: lots that move between the decision and the consume
// REFUSE rather than substituting.
func TestAuditWV4_ALotThatMovedAfterTheDecisionRefusesRatherThanSubstituting(t *testing.T) {
	f := newAuditFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 600_000_000)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 600_000_000)

	engine := payout.NewEngine(f.credits)
	in := f.sandboxInput()
	in.AccountID = f.account
	in.Requested = money.QuantityFromInt64(600_000_000)
	var d payout.Decision
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted, ReadOnly: true},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			d, err = engine.Evaluate(ctx, tx, in)
			return err
		}))
	require.Len(t, d.Lots, 1, "fixture check: one lot covers the request")
	require.True(t, d.Sufficient())

	// The card behind the approved lot is charged back.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return f.credits.SetFinality(ctx, tx, d.Lots[0].ID, valuedomain.FinalityDisputed,
				credit.Reference{Type: "audit_wv4", ID: uuid.NewString()}, "wv4: charged back")
		}))

	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			post, perr := f.wv4Post(ctx, tx, 600_000_000)
			if perr != nil {
				return perr
			}
			_, cerr := f.credits.Consume(ctx, tx, credit.ConsumeRequest{
				AccountID: f.account, Quantity: money.QuantityFromInt64(600_000_000),
				JournalTxID:              post,
				Reference:                credit.Reference{Type: "payout_request", ID: uuid.NewString()},
				Reason:                   "reserved against a payout request",
				RequireSpendableFinality: true, RequirePayoutFinality: true,
				RestrictToLots: true,
				LotIDs:         credit.EligibleLotIDs(d.Lots),
			})
			return cerr
		})
	require.Error(t, err,
		"the identical second lot must NOT be substituted for the one the decision approved")
	assert.Contains(t, err.Error(), "INSUFFICIENT_BUYING_POWER")
}

// A lot id belonging to another account reaches nothing: the account and asset
// predicates are ANDed with the lot restriction inside the same statement.
func TestAuditWV4_AForeignLotIdInTheSetReachesNothing(t *testing.T) {
	f := newAuditFixture(t)
	g := newAuditFixture(t)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 600_000_000)
	theirs := g.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 600_000_000)

	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			post, perr := f.wv4Post(ctx, tx, 600_000_000)
			if perr != nil {
				return perr
			}
			_, cerr := f.credits.Consume(ctx, tx, credit.ConsumeRequest{
				AccountID: f.account, Quantity: money.QuantityFromInt64(600_000_000),
				JournalTxID:              post,
				Reference:                credit.Reference{Type: "payout_request", ID: uuid.NewString()},
				Reason:                   "reserved against a payout request",
				RequireSpendableFinality: true, RequirePayoutFinality: true,
				RestrictToLots: true,
				LotIDs:         []credit.LotID{theirs.ID},
			})
			return cerr
		})
	require.Error(t, err, "another account's lot must not be reachable by naming its id")
	assert.Contains(t, err.Error(), "INSUFFICIENT_BUYING_POWER")

	var touched int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM credit_lot_events WHERE lot_id = $1 AND kind = 'CONSUME'`,
		theirs.ID).Scan(&touched))
	assert.Zero(t, touched, "and nothing was consumed from it")
}

// The provenance a quote previews and the provenance the request records are the
// same answer: `DecisionProvenance` walks the decision's lots in consumption
// order taking `Remaining.Min(remaining)`, which is the loop `Evaluate` filled
// them with and the loop `Consume` then repeats under the lot restriction.
func TestAuditWV4_ThePreviewAndTheRecordAgreeAboutWhatLeaves(t *testing.T) {
	f := newAuditFixture(t)
	// Three lots of two provenances, so the payout draws across a boundary and
	// the last one is taken only in part.
	earning := f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 200_000_000)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 300_000_000)
	f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 300_000_000)
	_ = earning

	engine := payout.NewEngine(f.credits)
	in := f.sandboxInput()
	in.AccountID = f.account
	in.Requested = money.QuantityFromInt64(650_000_000)
	var d payout.Decision
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted, ReadOnly: true},
		func(ctx context.Context, tx pgx.Tx) error {
			var err error
			d, err = engine.Evaluate(ctx, tx, in)
			return err
		}))
	require.True(t, d.Sufficient(), "fixture check")
	preview := payout.DecisionProvenance(d)
	require.NotEmpty(t, preview)

	req := f.createPayout(t, 650_000_000, "audit-wv4-preview")
	require.Equal(t, payout.StateVerified, req.State)
	record, err := f.svc.Provenance(f.ctx, testDB, req.ID)
	require.NoError(t, err)

	require.Len(t, record, len(preview),
		"the quote's preview and the request's record must describe the same provenances")
	for i := range preview {
		assert.Equal(t, preview[i].Origin, record[i].Origin, "slice %d origin", i)
		assert.Equal(t, preview[i].OriginFloor, record[i].OriginFloor, "slice %d floor", i)
		assert.Equal(t, preview[i].Quantity.String(), record[i].Quantity.String(),
			"slice %d quantity: the quote showed what would leave and the request recorded what did", i)
	}
}

// `ConsumeRequest.AllowedOrigins` was set by nothing. D-136 left it in place
// with a comment saying it "is no longer what a payout reservation uses"; it was
// no longer what anything used. It was the coarse filter whose coarseness was
// F-270, still reachable by any future caller, and — like `LotIDs` — reading an
// empty slice as "no restriction" rather than "nothing".
//
// INVERTED from the reproduction, which asserted that setters existed to be
// found. The field is gone (F-281), so the walk finds none, and this test stays
// as the guard the finding asked for: the day somebody adds an origin filter
// back, it has to be declared the way the lot restriction is rather than
// inferred from a slice's length.
func TestAuditWV4_TheOriginFilterIsNowSetByNothing(t *testing.T) {
	root := repoRoot(t)
	var setters []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "apps":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path) // #nosec G304 -- a path this walk produced
		if rerr != nil {
			return rerr
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "AllowedOrigins:") && !strings.Contains(line, "CSRF") {
				rel, _ := filepath.Rel(root, path)
				setters = append(setters, rel+": "+strings.TrimSpace(line))
			}
		}
		return nil
	}))
	assert.Empty(t, setters,
		"F-wv4-4 (second half): ConsumeRequest.AllowedOrigins was set by no production caller. "+
			"It was the filter F-270 was about, and a filter nothing sets is a filter nobody "+
			"notices going wrong — it read an empty slice as 'no restriction' exactly as the lot "+
			"set did. It has been removed rather than fixed. Setters found: %v", setters)

	// And the field itself is gone from the request, not merely unused: a
	// consume is restricted by lot or not at all.
	credits, rerr := os.ReadFile(filepath.Join(root, "internal", "credit", "types.go")) // #nosec G304 -- a path built from the repository root
	require.NoError(t, rerr)
	assert.NotContains(t, string(credits), "AllowedOrigins",
		"the coarse origin filter is not a field of ConsumeRequest any more (F-281)")
	assert.Contains(t, string(credits), "RestrictToLots",
		"and the restriction that remains is DECLARED rather than inferred from a slice's length")
}

// repoRoot walks up from the working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("no module root above the working directory")
	return ""
}

// D-137's residual 2, measured rather than trusted: "two transactions could in
// principle share a transaction_timestamp(), which is what binds a parent row to
// its mint". They do, at about two per cent under trivial concurrency on this
// host — so the at-mint rule's protection is not the absolute the word "in
// principle" suggests.
//
// It does not open a route. A parent row is written only by
// `credit.Service.recordParents`, which runs inside the transaction that mints
// the CHILD, so no application path can write a parent row for a lot a different
// transaction minted, whatever the clock says. The measurement is recorded so a
// fifth round does not have to take "in principle" on trust, and so that any
// future writer of `credit_lot_parents` outside a mint is understood to be
// relying on microsecond uniqueness.
func TestAuditWV4_TwoTransactionsDoShareATransactionTimestamp(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	const n = 400
	stamps := make([]time.Time, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, ReadOnly: true},
				func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&stamps[i])
				})
		}(i)
	}
	wg.Wait()
	seen := map[int64]int{}
	for _, s := range stamps {
		seen[s.UnixMicro()]++
	}
	colliding := 0
	for _, c := range seen {
		if c > 1 {
			colliding += c
		}
	}
	t.Logf("%d concurrent transactions, %d distinct microsecond stamps, %d sharing one",
		n, len(seen), colliding)

	// The rule that makes the collision harmless: nothing outside a mint writes a
	// parent row, so a lot committed by an earlier transaction cannot acquire one.
	f := newAuditFixture(t)
	grant := f.issue(valuedomain.OriginPromotional, valuedomain.FinalityUnfunded, 100_000_000)
	settled := f.issue(valuedomain.OriginPurchased, valuedomain.FinalitySettled, 100_000_000)
	_, perr := testDB.Exec(f.ctx,
		`INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity) VALUES ($1,$2,$3::numeric)`,
		settled.ID, grant.ID, "1")
	require.Error(t, perr,
		"a parent row for a lot an earlier transaction minted must be refused, which is what "+
			"keeps the timestamp collision above from being a route to a stale root set")
	assert.Contains(t, perr.Error(), "CREDIT_PARENT_NOT_AT_MINT")
}
