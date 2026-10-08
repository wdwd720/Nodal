//go:build integration

package derivatives

import (
	"context"
	"crypto/sha256"
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
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// Regression tests for the review-2 findings against this package. Each is
// named after the property it proves; each finding also has a test proving the
// database refuses the invalid state when Go is bypassed.

func operator(id string) econ.Actor { return econ.Actor{Type: security.ActorOperator, ID: id} }

func (f *fixture) settleWith(r SettleRequest) (SettlementResult, error) {
	var out SettlementResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.SettleExpiry(ctx, tx, r)
		return err
	})
	return out, err
}

func (f *fixture) propose(r ProposeOverrideRequest) (SettlementOverride, error) {
	var out SettlementOverride
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.ProposeSettlementOverride(ctx, tx, r)
		return err
	})
	return out, err
}

func (f *fixture) approve(r OverrideDecisionRequest) (SettlementOverride, SettlementResult, error) {
	var o SettlementOverride
	var res SettlementResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		o, res, err = f.svc.ApproveSettlementOverride(ctx, tx, r)
		return err
	})
	return o, res, err
}

func (f *fixture) reject(r OverrideDecisionRequest) (SettlementOverride, error) {
	var out SettlementOverride
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.RejectSettlementOverride(ctx, tx, r)
		return err
	})
	return out, err
}

func (f *fixture) preview(ins instruments.InstrumentID) SettlementPreview {
	f.t.Helper()
	p, err := f.svc.SettlementPreview(f.ctx, testDB, ins)
	require.NoError(f.t, err)
	return p
}

// evidence is the archived evidence document under key, as text.
func (f *fixture) evidence(key string) string {
	f.t.Helper()
	var body string
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT convert_from(body, 'UTF8') FROM provider_evidence WHERE key = $1`,
		"advanced/"+key+".json").Scan(&body), key)
	return body
}

// expiredFuture lists a future expiring two hours after the fixture start with
// a long and a short of two contracts each, and returns it with its accounts.
func (f *fixture) expiredFutureBook() (instruments.InstrumentID, time.Time, accounts.AccountID, accounts.AccountID) {
	f.t.Helper()
	expiry := start.Add(2 * time.Hour)
	fut := f.future(expiry)
	a, b := f.account(), f.account()
	f.fund(a, usdUnits(100_000))
	f.fund(b, usdUnits(100_000))
	f.mark(fut, 65_000, 65_000)
	f.open(a, fut, Long, 2)
	f.open(b, fut, Short, 2)
	return fut, expiry, a, b
}

func requireCode(t *testing.T, err error, code errs.Code) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, code, errs.CodeOf(err), "%v", err)
}

func requireSQLState(t *testing.T, err error, state string) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, state, db.SQLState(err), "%v", err)
}

// --- F2: the settlement price is the rule's, never the caller's -----------------

// TestIntegration_ASettlementIsPricedByTheRuleNeverByTheCaller: a price in the
// request only confirms the rule's price; any other refuses the settlement and
// nothing settles. The evidence states the rule and the mark it read.
func TestIntegration_ASettlementIsPricedByTheRuleNeverByTheCaller(t *testing.T) {
	f := newFixture(t)
	fut, expiry, _, _ := f.expiredFutureBook()
	f.clk.Set(expiry)
	m := f.markAt(fut, dollars(65_000), dollars(65_000), expiry)

	chosen := dollars(90_000)
	_, err := f.settleWith(SettleRequest{InstrumentID: fut, Price: &chosen, PriceSource: "anything"})
	requireCode(t, err, errs.CodeConflict)
	_, err = f.settleWith(SettleRequest{InstrumentID: fut, PriceSource: "mark:" + uuid.NewString()})
	requireCode(t, err, errs.CodeConflict)
	assert.Zero(t, f.count(`SELECT count(*) FROM derivative_settlements WHERE instrument_id = $1`, fut), "nothing settled")

	p := f.preview(fut)
	require.True(t, p.HasReference)
	assert.True(t, p.Settleable)
	assert.Equal(t, "6500000", p.ReferencePrice.String())
	assert.Equal(t, "mark:"+m.ID.String(), p.PriceSource)
	price := p.ReferencePrice
	r, err := f.settleWith(SettleRequest{InstrumentID: fut, Price: &price, PriceSource: p.PriceSource})
	require.NoError(t, err)
	assert.Equal(t, "6500000", r.Price.String())
	assert.Equal(t, RuleExpiryMark, r.Rule)
	require.NotNil(t, r.MarkID)
	assert.Equal(t, m.ID, *r.MarkID)
	assert.Nil(t, r.OverrideID)
	doc := f.evidence("derivatives/settlement/" + r.ID.String())
	assert.Contains(t, doc, `"rule":"EXPIRY_MARK"`)
	assert.Contains(t, doc, "EXPIRY_MARK v1:")
	assert.Contains(t, doc, m.ID.String())

	rec, ok, err := GetSettlement(f.ctx, testDB, fut)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, RuleExpiryMark, rec.Rule)

	again, err := f.settleWith(SettleRequest{InstrumentID: fut, Price: &price})
	require.NoError(t, err)
	assert.True(t, again.Replay)
	_, err = f.settleWith(SettleRequest{InstrumentID: fut, Price: &chosen})
	requireCode(t, err, errs.CodeConflict)
}

// TestIntegration_ASettlementReadsOnlyMarksRecordedByExpiry: a mark observed
// just before expiry but recorded after it -- in order and within the lag, so
// RecordMark accepts it -- is not the settlement's mark: the rule reads only
// what was recorded by expiry.
func TestIntegration_ASettlementReadsOnlyMarksRecordedByExpiry(t *testing.T) {
	f := newFixture(t)
	fut, expiry, _, _ := f.expiredFutureBook()
	f.clk.Set(expiry.Add(-30 * time.Second))
	last := f.markAt(fut, dollars(65_000), dollars(65_000), f.clk.Now())
	f.clk.Set(expiry.Add(20 * time.Second))
	late := f.markAt(fut, dollars(80_000), dollars(80_000), expiry.Add(-10*time.Second))
	assert.True(t, late.RecordedAt.After(expiry))

	r, err := f.settleWith(SettleRequest{InstrumentID: fut})
	require.NoError(t, err)
	assert.Equal(t, "6500000", r.Price.String())
	require.NotNil(t, r.MarkID)
	assert.Equal(t, last.ID, *r.MarkID)
}

// TestIntegration_AStaleExpiryWaitsForAnOverrideTwoOperatorsAgreeOn: with no
// fresh mark at expiry nothing settles by the rule. One operator proposes a
// price within the policy's bound of the rule's reference, with a reason and
// evidence; the proposer cannot approve it; a second operator's approval
// settles the contract at exactly that price, and the evidence names both.
func TestIntegration_AStaleExpiryWaitsForAnOverrideTwoOperatorsAgreeOn(t *testing.T) {
	f := newFixture(t)
	fut, expiry, a, b := f.expiredFutureBook()
	f.clk.Set(expiry.Add(-5 * time.Minute))
	ref := f.markAt(fut, dollars(66_000), dollars(66_000), f.clk.Now())
	f.clk.Set(expiry.Add(time.Hour))

	_, err := f.settleWith(SettleRequest{InstrumentID: fut})
	requireCode(t, err, errs.CodeStaleMarketData)

	p := f.preview(fut)
	require.True(t, p.HasReference)
	assert.False(t, p.Settleable)
	assert.Equal(t, ref.ID, p.ReferenceMark.ID)
	assert.Equal(t, "6600000", p.ReferencePrice.String())
	assert.Equal(t, "5940000", p.OverrideMinPrice.String(), "10%% below, on the tick grid")
	assert.Equal(t, "7260000", p.OverrideMaxPrice.String())

	digest := sha256.Sum256([]byte("sandbox venue settlement bulletin"))
	req := ProposeOverrideRequest{InstrumentID: fut, Price: dollars(67_000),
		Reason:      "the feed stalled five minutes before expiry; the venue's settlement print is 67 000",
		EvidenceRef: "s3://sandbox-evidence/settlement-bulletin.pdf", EvidenceSHA256: digest[:], Actor: operator("op-alice")}
	user := req
	user.Actor = econ.Actor{Type: security.ActorUser, ID: "user-1"}
	_, err = f.propose(user)
	requireCode(t, err, errs.CodeForbidden)
	far := req
	far.Price = dollars(73_000)
	_, err = f.propose(far)
	requireCode(t, err, errs.CodeValidationFailed)
	off := req
	off.Price = q(6_700_010)
	_, err = f.propose(off)
	requireCode(t, err, errs.CodeValidationFailed)
	bare := req
	bare.EvidenceSHA256 = nil
	_, err = f.propose(bare)
	requireCode(t, err, errs.CodeValidationFailed)

	o, err := f.propose(req)
	require.NoError(t, err)
	assert.Equal(t, OverrideProposed, o.Status)
	assert.Equal(t, ref.ID, o.ReferenceMarkID)
	assert.Equal(t, "6600000", o.ReferenceMantissa.String())
	assert.Equal(t, money.BPS(1_000), o.MaxDeviationBPS)
	assert.Equal(t, money.BPS(152), o.DeviationBPS())
	assert.Contains(t, f.evidence("derivatives/settlement-override/"+o.ID.String()), "settlement-bulletin.pdf")
	assert.Zero(t, f.count(`SELECT count(*) FROM derivative_settlements WHERE instrument_id = $1`, fut), "a proposal settles nothing")

	_, _, err = f.approve(OverrideDecisionRequest{OverrideID: o.ID, Reason: "agreed", Actor: operator("op-alice")})
	requireCode(t, err, errs.CodeForbidden)
	wrong := dollars(68_000)
	_, _, err = f.approve(OverrideDecisionRequest{OverrideID: o.ID, Reason: "agreed", Actor: operator("op-bob"), Price: &wrong})
	requireCode(t, err, errs.CodeConflict)

	confirmed := dollars(67_000)
	approved, r, err := f.approve(OverrideDecisionRequest{OverrideID: o.ID, Reason: "checked against the bulletin",
		Actor: operator("op-bob"), Price: &confirmed})
	require.NoError(t, err)
	assert.Equal(t, OverrideApproved, approved.Status)
	assert.Equal(t, "6700000", r.Price.String())
	assert.Equal(t, RuleOperatorOverride, r.Rule)
	require.NotNil(t, r.OverrideID)
	assert.Equal(t, o.ID, *r.OverrideID)
	for _, acct := range []accounts.AccountID{a, b} {
		assert.Equal(t, PositionSettled, f.position(acct, fut).Status)
	}
	doc := f.evidence("derivatives/settlement/" + r.ID.String())
	assert.Contains(t, doc, `"rule":"OPERATOR_OVERRIDE"`)
	assert.Contains(t, doc, "OPERATOR:op-alice")
	assert.Contains(t, doc, "OPERATOR:op-bob")
	assert.True(t, f.reconcile().Matched)

	listed, err := SettlementOverrides(f.ctx, testDB, fut)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, OverrideApproved, listed[0].Status)
	require.NotNil(t, listed[0].SettlementID)
	assert.Equal(t, r.ID, *listed[0].SettlementID)

	// Nothing is left to propose or decide.
	_, err = f.propose(req)
	requireCode(t, err, errs.CodeConflict)
	_, err = f.reject(OverrideDecisionRequest{OverrideID: o.ID, Reason: "too late", Actor: operator("op-carol")})
	requireCode(t, err, errs.CodeConflict)
}

// TestIntegration_ARejectedOverrideSettlesNothing: a rejection (here the
// proposer withdrawing its own proposal) is final and moves no value.
func TestIntegration_ARejectedOverrideSettlesNothing(t *testing.T) {
	f := newFixture(t)
	fut, expiry, _, _ := f.expiredFutureBook()
	f.clk.Set(expiry.Add(-5 * time.Minute))
	f.markAt(fut, dollars(66_000), dollars(66_000), f.clk.Now())
	f.clk.Set(expiry.Add(time.Hour))
	digest := sha256.Sum256([]byte("bulletin"))
	o, err := f.propose(ProposeOverrideRequest{InstrumentID: fut, Price: dollars(66_500), Reason: "stalled feed",
		EvidenceRef: "s3://sandbox-evidence/b.pdf", EvidenceSHA256: digest[:], Actor: operator("op-alice")})
	require.NoError(t, err)
	rejected, err := f.reject(OverrideDecisionRequest{OverrideID: o.ID, Reason: "the bulletin was superseded", Actor: operator("op-alice")})
	require.NoError(t, err)
	assert.Equal(t, OverrideRejected, rejected.Status)
	_, _, err = f.approve(OverrideDecisionRequest{OverrideID: o.ID, Reason: "agreed", Actor: operator("op-bob")})
	requireCode(t, err, errs.CodeConflict)
	assert.Zero(t, f.count(`SELECT count(*) FROM derivative_settlements WHERE instrument_id = $1`, fut))
}

// TestIntegration_ConcurrentOverrideApprovalsSettleOnce: two proposals, eight
// second operators approving them at once: exactly one approval lands and the
// contract settles once.
func TestIntegration_ConcurrentOverrideApprovalsSettleOnce(t *testing.T) {
	f := newFixture(t)
	fut, expiry, _, _ := f.expiredFutureBook()
	f.clk.Set(expiry.Add(-5 * time.Minute))
	f.markAt(fut, dollars(66_000), dollars(66_000), f.clk.Now())
	f.clk.Set(expiry.Add(time.Hour))
	digest := sha256.Sum256([]byte("bulletin"))
	var proposals []SettlementOverride
	for i, by := range []string{"op-alice", "op-bob"} {
		o, err := f.propose(ProposeOverrideRequest{InstrumentID: fut, Price: dollars(66_500 + int64(i)*100), Reason: "stalled feed",
			EvidenceRef: "s3://sandbox-evidence/b.pdf", EvidenceSHA256: digest[:], Actor: operator(by)})
		require.NoError(t, err)
		proposals = append(proposals, o)
	}
	const n = 8
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := f.approve(OverrideDecisionRequest{OverrideID: proposals[i%2].ID, Reason: "agreed",
				Actor: operator("op-approver-" + uuid.NewString()[:8])})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	settled := 0
	for err := range results {
		if err == nil {
			settled++
			continue
		}
		assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "%v", err)
	}
	assert.Equal(t, 1, settled)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM derivative_settlements WHERE instrument_id = $1`, fut))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM derivative_settlement_override_decisions d
		JOIN derivative_settlement_overrides o ON o.id = d.override_id WHERE o.instrument_id = $1`, fut))
}

// TestIntegration_TheDatabaseRefusesASettlementNoRuleOrSecondOperatorExplains:
// written around Go, a settlement naming no rule, a price the expiry rule
// does not derive, an override nobody else approved, a self-approval and an
// unbounded or unreferenced proposal are all refused.
func TestIntegration_TheDatabaseRefusesASettlementNoRuleOrSecondOperatorExplains(t *testing.T) {
	f := newFixture(t)
	expiry := start.Add(2 * time.Hour)
	fut := f.future(expiry)
	early := f.mark(fut, 64_000, 64_000)
	f.clk.Set(expiry)
	m := f.markAt(fut, dollars(65_000), dollars(65_000), expiry)

	rawSettlement := func(rule any, price int64, mark, override any) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO derivative_settlements (id, instrument_id, kind, settlement_mantissa, price_scale,
				price_source, price_rule, mark_id, override_id, positions, settled_at) VALUES ($1,$2,'FUTURE',$3,2,'raw',$4,$5,$6,0,$7)`,
				uuid.New(), fut, price, rule, mark, override, expiry)
			return err
		})
	}
	requireSQLState(t, rawSettlement(nil, 6_500_000, m.ID, nil), "DV008")
	requireSQLState(t, rawSettlement("EXPIRY_MARK", 9_000_000, m.ID, nil), "DV008")
	requireSQLState(t, rawSettlement("EXPIRY_MARK", 6_400_000, early.ID, nil), "DV008")

	rawProposal := func(price int64, ref uuid.UUID, refPrice int64, actorType string) (uuid.UUID, error) {
		oid := uuid.New()
		digest := sha256.Sum256([]byte("raw"))
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO derivative_settlement_overrides (id, instrument_id, price_mantissa, price_scale,
				reference_mark_id, reference_mantissa, max_deviation_bps, reason, evidence_ref, evidence_sha256,
				proposed_by_actor_type, proposed_by_actor_id, proposed_at) VALUES ($1,$2,$3,2,$4,$5,1000,'raw','raw',$6,$7,'op-alice',$8)`,
				oid, fut, price, ref, refPrice, digest[:], actorType, expiry)
			return err
		})
		return oid, err
	}
	refID := uuid.UUID(m.ID.Bytes())
	_, err := rawProposal(9_000_000, refID, 6_500_000, "OPERATOR")
	requireSQLState(t, err, "23514") // beyond its bound: derivative_settlement_overrides_bounded
	assert.Contains(t, err.Error(), "derivative_settlement_overrides_bounded")
	_, err = rawProposal(6_400_000, uuid.UUID(early.ID.Bytes()), 6_400_000, "OPERATOR")
	requireSQLState(t, err, "DV009") // not the rule's reference
	_, err = rawProposal(6_500_000, refID, 6_500_000, "USER")
	requireSQLState(t, err, "23514") // only an operator proposes
	oid, err := rawProposal(6_550_000, refID, 6_500_000, "OPERATOR")
	require.NoError(t, err)

	requireSQLState(t, rawSettlement("OPERATOR_OVERRIDE", 6_550_000, m.ID, oid), "DV010") // nobody approved it
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO derivative_settlement_override_decisions (id, override_id, decision, reason,
			decided_by_actor_type, decided_by_actor_id, decided_at) VALUES ($1,$2,'APPROVED','self','OPERATOR','op-alice',$3)`,
			uuid.New(), oid, expiry)
		return err
	})
	requireSQLState(t, err, "DV010") // the proposer approving itself

	// The rule still settles it, exactly.
	r, err := f.settleWith(SettleRequest{InstrumentID: fut})
	require.NoError(t, err)
	assert.Equal(t, "6500000", r.Price.String())
}

// --- F3: marks are recorded in order and on time; runs read their cutoff -----------

func (f *fixture) record(ins instruments.InstrumentID, source string, at time.Time, markDollars int64) (Mark, error) {
	var m Mark
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		mp := money.Price{Mantissa: dollars(markDollars), Scale: 2, QuoteAsset: f.usd.String(), Source: source, At: at}
		ip := money.Price{Mantissa: dollars(markDollars), Scale: 2, QuoteAsset: f.usd.String(), Source: source + "-index", At: at}
		var err error
		m, err = f.svc.RecordMark(ctx, tx, RecordMarkRequest{InstrumentID: ins, Mark: mp, Source: source, ObservedAt: at, Index: &ip})
		return err
	})
	return m, err
}

// TestIntegration_AMarkIsRecordedOnlyInOrderAndOnTime: never from the future,
// never older than the policy's lag, never behind the latest observation; the
// same moment from another source is in order, and a replay of an old
// observation is answered from the record.
func TestIntegration_AMarkIsRecordedOnlyInOrderAndOnTime(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	first, err := f.record(perp, "itest", start, 65_000)
	require.NoError(t, err)

	_, err = f.record(perp, "itest", start.Add(time.Second), 65_000)
	requireCode(t, err, errs.CodeValidationFailed)

	f.clk.Advance(2 * time.Minute)
	_, err = f.record(perp, "itest", f.clk.Now().Add(-61*time.Second), 65_000)
	requireCode(t, err, errs.CodeValidationFailed)

	second, err := f.record(perp, "itest", f.clk.Now().Add(-10*time.Second), 65_100)
	require.NoError(t, err)
	_, err = f.record(perp, "other", f.clk.Now().Add(-20*time.Second), 65_100)
	requireCode(t, err, errs.CodeConflict)
	_, err = f.record(perp, "other", second.ObservedAt, 65_150)
	require.NoError(t, err, "the same moment from another source is in order")

	again, err := f.record(perp, "itest", first.ObservedAt, 65_000)
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID)
	_, err = f.record(perp, "itest", first.ObservedAt, 66_000)
	requireCode(t, err, errs.CodeConflict)
}

// TestIntegration_TheDatabaseKeepsMarksInOrder: written around Go, a mark
// behind the latest observation (DV005) or observed after it was recorded is
// refused.
func TestIntegration_TheDatabaseKeepsMarksInOrder(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	f.mark(perp, 65_000, 65_000)
	raw := func(observed, recorded time.Time) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO derivative_marks (id, instrument_id, mark_mantissa, mark_scale, source, observed_at, recorded_at)
				VALUES ($1,$2,6500000,2,'raw',$3,$4)`, uuid.New(), perp, observed, recorded)
			return err
		})
	}
	requireSQLState(t, raw(start.Add(-time.Second), start.Add(time.Hour)), "DV005")
	err := raw(start.Add(2*time.Hour), start.Add(time.Hour))
	requireSQLState(t, err, "23514")
	assert.Contains(t, err.Error(), "derivative_marks_observed_before_recorded")
}

// TestIntegration_FundingIgnoresMarksRecordedAfterTheInterval: a mark observed
// in the funding interval but recorded after it ended is not a sample.
func TestIntegration_FundingIgnoresMarksRecordedAfterTheInterval(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a, b := f.account(), f.account()
	f.fund(a, usdUnits(100_000))
	f.fund(b, usdUnits(100_000))
	m1 := f.mark(perp, 65_000, 64_935)
	f.open(a, perp, Long, 1)
	f.open(b, perp, Short, 1)
	end := fundingWindow.Add(8 * time.Hour)
	f.clk.Set(end.Add(30 * time.Second))
	late := f.markAt(perp, dollars(70_000), dollars(65_000), end.Add(-10*time.Second))
	assert.True(t, late.RecordedAt.After(end))

	r, err := f.runFunding(perp, fundingWindow)
	require.NoError(t, err)
	premium, err := PremiumIndexE8([]IndexSample{sampleOf(m1)})
	require.NoError(t, err)
	assert.Equal(t, premium, r.PremiumIndexE8, "the late mark is not a sample")
	assert.Equal(t, m1.ID, r.MarkID)
}

// --- F6: a trade is backed by the decision that allowed exactly it -------------------

func (f *fixture) decision(acct accounts.AccountID, ins instruments.InstrumentID, capability gates.Capability, at time.Time, allow bool) risk.DecisionID {
	f.t.Helper()
	in := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{Kind: "TAKE_LIQUIDITY", AccountID: acct.String(), ActorType: "USER",
			Capability: string(capability), Mode: string(gates.ModePaper), InstrumentID: ins.String(), RiskIncreasing: true},
		Account:    &risk.AdvancedAccountSnapshot{Status: "ACTIVE"},
		Instrument: &risk.InstrumentSnapshot{Status: "ACTIVE", SessionOpen: true},
		Oracle:     &risk.OracleSnapshot{Available: true, ObservedAt: at},
		Now:        at,
	}
	if allow {
		in.Identity = &risk.IdentitySnapshot{Verified: true}
	}
	d := risk.EvaluateAdvanced(risk.DefaultAdvancedPolicy(), in)
	require.Equal(f.t, allow, d.Verdict == risk.Allow, "%v", d.ReasonCodes)
	var did risk.DecisionID
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		did, err = f.riskSt.RecordAdvancedDecision(ctx, tx, in, d, "itest")
		return err
	}))
	return did
}

// TestIntegration_AFillNeedsAnAllowForItsOwnAccountIntentAndInstrument: a
// REJECT, another account's ALLOW, an ALLOW for another instrument or another
// capability, one made after the trade, or one that already allowed a trade:
// none of them backs a fill, and none changes a position.
func TestIntegration_AFillNeedsAnAllowForItsOwnAccountIntentAndInstrument(t *testing.T) {
	f := newFixture(t)
	perp, other := f.perp(), f.perp()
	a, b := f.account(), f.account()
	f.fund(a, usdUnits(100_000))
	m := f.mark(perp, 65_000, 65_000)
	fill := func(did risk.DecisionID) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.ApplyFill(ctx, tx, Fill{Key: "book:" + uuid.NewString(), AccountID: a, InstrumentID: perp, Side: Buy,
				Quantity: q(1), Price: dollars(65_000), Source: SourceBook, MarkID: m.ID, Direction: FillEither, DecisionID: did,
				OccurredAt: f.clk.Now()})
			return err
		})
	}
	now := f.clk.Now()
	for name, did := range map[string]risk.DecisionID{
		"a REJECT":                        f.decision(a, perp, gates.Derivatives, now, false),
		"another account's ALLOW":         f.decision(b, perp, gates.Derivatives, now, true),
		"an ALLOW for another instrument": f.decision(a, other, gates.Derivatives, now, true),
		"an ALLOW for another capability": f.decision(a, perp, gates.Margin, now, true),
		"an ALLOW made after the trade":   f.decision(a, perp, gates.Derivatives, now.Add(time.Second), true),
	} {
		requireCode(t, fill(did), errs.CodeForbidden)
		assert.Zero(t, f.count(`SELECT count(*) FROM derivative_positions WHERE account_id = $1`, a), name)
	}
	allowed := f.decision(a, perp, gates.Derivatives, now, true)
	require.NoError(t, fill(allowed))
	assert.Equal(t, "1", f.position(a, perp).Quantity.String())
	requireCode(t, fill(allowed), errs.CodeForbidden) // one decision, one trade
	assert.Equal(t, "1", f.position(a, perp).Quantity.String())
}

// TestIntegration_TheDatabaseRefusesATradeItsDecisionDoesNotAllow: written
// around Go, a trade on a REJECT (DV006) or on a decision that already backs a
// trade (unique) cannot be written.
func TestIntegration_TheDatabaseRefusesATradeItsDecisionDoesNotAllow(t *testing.T) {
	f := newFixture(t)
	p := f.openPosition(2)
	reject := f.decision(p.AccountID, p.InstrumentID, gates.Derivatives, f.clk.Now(), false)
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error { return f.rawFill(ctx, tx, p, "BOOK", 1, reject) })
	requireSQLState(t, err, "DV006")
	foreign := f.decision(f.account(), p.InstrumentID, gates.Derivatives, f.clk.Now(), true)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error { return f.rawFill(ctx, tx, p, "BOOK", 1, foreign) })
	requireSQLState(t, err, "DV006")

	var did, intent uuid.UUID
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT risk_decision_id, intent_id FROM derivative_fills WHERE position_id = $1`, p.ID).
		Scan(&did, &intent))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO derivative_fills (id, fill_key, position_id, account_id, instrument_id, source, side, quantity,
			price_mantissa, price_scale, value, intent_id, risk_decision_id, occurred_at) VALUES ($1,$2,$3,$4,$5,'PAPER','BUY',1,6500000,2,0,$6,$7,$8)`,
			uuid.New(), "raw:"+uuid.NewString(), p.ID, p.AccountID, p.InstrumentID, intent, did, f.clk.Now())
		return err
	})
	require.Error(t, err)
	assert.True(t, db.IsUniqueViolation(err), "%v", err)
}

// --- F7: a close is priced only at a fresh mark ---------------------------------------

// TestIntegration_ACloseAtAStaleMarkIsRefusedAndRecorded: a week-old mark
// prices no close. The refusal is recorded -- the intent REJECTED with
// STALE_MARKET_DATA, its transition naming the mark, the evidence archived --
// and findable among the account's intents; a fresh mark then prices it.
func TestIntegration_ACloseAtAStaleMarkIsRefusedAndRecorded(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a := f.account()
	f.fund(a, usdUnits(100_000))
	m := f.mark(perp, 65_000, 65_000)
	f.open(a, perp, Long, 5)
	f.clk.Advance(7 * 24 * time.Hour)

	_, err := f.tryClose(CloseRequest{IntentRequest: f.req(a, perp)})
	ref := requireRefusal(t, err, errs.CodeStaleMarketData, ReasonMarkStale)
	var status, code string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status, rejection_code FROM economic_intents WHERE id = $1`, ref.IntentID).Scan(&status, &code))
	assert.Equal(t, "REJECTED", status)
	assert.Equal(t, "STALE_MARKET_DATA", code)
	var reason string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT reason FROM economic_intent_transitions WHERE intent_id = $1 AND to_status = 'REJECTED'`,
		ref.IntentID).Scan(&reason))
	assert.Contains(t, reason, m.ID.String())
	doc := f.evidence("derivatives/refusal/" + ref.IntentID.String())
	assert.Contains(t, doc, ReasonMarkStale)
	assert.Contains(t, doc, m.ID.String())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND kind = 'CLOSE_POSITION'
		AND status = 'REJECTED' AND rejection_code = 'STALE_MARKET_DATA'`, a))
	assert.Equal(t, "5", f.position(a, perp).Quantity.String())
	assert.Zero(t, f.count(`SELECT count(*) FROM derivative_fills WHERE intent_id = $1`, ref.IntentID))

	f.mark(perp, 64_000, 64_000)
	closed := f.closeAll(a, perp)
	assert.Equal(t, econ.StatusCompleted, closed.Intent.Status)
	require.NotNil(t, closed.Fill)
	assert.Equal(t, "6393600", closed.Fill.PriceMantissa.String(), "the fresh 64 000 mark less the disclosed slippage")
}

// --- F8: obligations take only available funds ------------------------------------------

func (f *fixture) available(acct accounts.AccountID) capital.Availability {
	f.t.Helper()
	av, err := f.svc.capital.Availability(f.ctx, testDB, acct.String(), f.usd, f.clk.Now())
	require.NoError(f.t, err)
	return av
}

func (f *fixture) requireBacked(acct accounts.AccountID) {
	f.t.Helper()
	av := f.available(acct)
	require.True(f.t, av.WalletBalance.Cmp(av.Reserved.Add(av.Held)) >= 0,
		"the wallet (%s) no longer backs what is reserved (%s) and held (%s)", av.WalletBalance, av.Reserved, av.Held)
}

// TestIntegration_CollectionsNeverSpendFundsReservedForSomethingElse: another
// order reserves all but $50 of the wallet. A variation-margin loss of $106.50
// takes exactly the $50 that is available (under its own consumed
// reservation, named by the cash flow) and the rest from the position's
// margin; a closing loss is paid from the margin the close frees. The other
// order's reservation stays fully backed throughout.
func TestIntegration_CollectionsNeverSpendFundsReservedForSomethingElse(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a := f.account()
	f.mark(perp, 65_000, 65_000)
	f.fund(a, usdUnits(2_000))
	f.open(a, perp, Long, 10) // $796.25 of margin
	wallet := f.balance(a, ledger.CodeWallet)
	var other capital.Reservation
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		other, err = f.svc.capital.Reserve(ctx, tx, capitalReserve(a, f.usd, wallet.Sub(usdUnits(50))))
		return err
	}))
	margin := f.position(a, perp).MarginPosted

	// At $64 000 the position owes 10 × ($640 − $650.65) = $106.50.
	f.clk.Set(start.Add(time.Hour))
	f.mark(perp, 64_000, 64_000)
	var r RunResult
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.RunVariationMargin(ctx, tx, RunRequest{InstrumentID: perp, IntervalStart: start})
		return err
	}))
	assert.Equal(t, "106500000", r.Paid.String())
	assert.True(t, r.Shortfall.IsZero())
	assert.Equal(t, wallet.Sub(usdUnits(50)).String(), f.balance(a, ledger.CodeWallet).String(), "only the available $50 left the wallet")
	assert.Equal(t, margin.Sub(q(56_500_000)).String(), f.position(a, perp).MarginPosted.String(), "the rest came from the margin")
	f.requireBacked(a)
	var flowReservation uuid.UUID
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT reservation_id FROM derivative_cash_flows WHERE account_id = $1 AND kind = 'VARIATION_MARGIN'`, a).
		Scan(&flowReservation))
	var status string
	var consumed money.Quantity
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status, consumed_quantity FROM asset_reservations WHERE id = $1`, flowReservation).
		Scan(&status, &consumed))
	assert.Equal(t, "CONSUMED", status)
	assert.Equal(t, usdUnits(50).String(), consumed.String())
	got, err := f.svc.capital.Get(f.ctx, testDB, other.ID)
	require.NoError(t, err)
	assert.Equal(t, capital.ReservationActive, got.Status, "the other order's reservation is untouched")

	// A loss on closing is paid from the margin the close frees.
	f.clk.Set(start.Add(2 * time.Hour))
	f.mark(perp, 63_000, 63_000)
	before := f.balance(a, ledger.CodeWallet)
	closed := f.closeAll(a, perp)
	require.NotNil(t, closed.Fill)
	assert.True(t, closed.Fill.CashSettled.IsNegative(), "a loss")
	assert.True(t, f.balance(a, ledger.CodeWallet).Cmp(before) >= 0, "nothing was taken from the wallet")
	f.requireBacked(a)
	assert.True(t, f.reconcile().Matched)
}

// TestIntegration_TheDatabaseRefusesAWalletCollectionWithoutItsReservation:
// written around Go, a cash flow whose journal takes from the wallet must name
// a consumed reservation of the same account covering it (DV011, at COMMIT).
func TestIntegration_TheDatabaseRefusesAWalletCollectionWithoutItsReservation(t *testing.T) {
	f := newFixture(t)
	p := f.openPosition(1)
	ch := f.clearingHouse()
	collect := func(consume bool, reserve bool) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var reservation any
			if reserve {
				res, err := f.svc.capital.Reserve(ctx, tx, capitalReserve(p.AccountID, f.usd, q(50)))
				if err != nil {
					return err
				}
				if consume {
					if _, err := f.svc.capital.ConsumeFinal(ctx, tx, res.ID, q(50), 0, ""); err != nil {
						return err
					}
				}
				reservation = res.ID
			}
			jid, err := rawJournal(ctx, f, tx, ledger.KindVariationMargin, []rawEntry{
				{ledger.CustomerAccount(p.AccountID, ledger.CodeRealizedPnL, f.usd), ledger.Debit, 50},
				{ledger.CustomerAccount(p.AccountID, ledger.CodeWallet, f.usd), ledger.Credit, 50},
				{ch.Account(ledger.CodeClearingCash, f.usd), ledger.Debit, 50},
				{ch.Account(ledger.CodeClearingResult, f.usd), ledger.Credit, 50},
			})
			if err != nil {
				return err
			}
			e, _, err := f.effects.Begin(ctx, tx, econ.BeginRequest{Key: "raw:" + uuid.NewString(), Subsystem: econ.SubsystemDerivatives,
				Kind: "RAW", Request: map[string]string{"raw": "1"}, PostsLedger: true, Actor: econ.Actor{Type: security.ActorSystem, ID: "raw"}})
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO derivative_cash_flows (id, flow_key, position_id, account_id, instrument_id,
				settlement_asset_id, clearing_house_id, kind, amount, journal_transaction_id, effect_id, reservation_id, reference_type,
				reference_id, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,'VARIATION_MARGIN',-50,$8,$9,$10,'raw','raw',now())`,
				uuid.New(), "raw:"+uuid.NewString(), p.ID, p.AccountID, p.InstrumentID, f.usd, ch.ID, jid, e.ID, reservation)
			return err
		})
	}
	requireSQLState(t, collect(false, false), "DV011")
	requireSQLState(t, collect(false, true), "DV011") // reserved but never consumed
	require.NoError(t, collect(true, true))
}

// TestIntegration_ConcurrentReservationsAndAVariationMarginRunNeverOverdrawTheWallet:
// a variation-margin run collecting from the wallet and six other orders
// reserving it, all at once: whatever order they land in, the wallet backs
// every reservation left standing.
func TestIntegration_ConcurrentReservationsAndAVariationMarginRunNeverOverdrawTheWallet(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a := f.account()
	f.mark(perp, 65_000, 65_000)
	f.fund(a, usdUnits(2_000))
	f.open(a, perp, Long, 10) // leaves $1 203.75 in the wallet
	f.clk.Set(start.Add(time.Hour))
	f.mark(perp, 64_000, 64_000)

	var wg sync.WaitGroup
	errsCh := make(chan error, 7)
	wg.Add(7)
	go func() {
		defer wg.Done()
		errsCh <- f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.RunVariationMargin(ctx, tx, RunRequest{InstrumentID: perp, IntervalStart: start})
			return err
		})
	}()
	for range 6 {
		go func() {
			defer wg.Done()
			errsCh <- f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.svc.capital.Reserve(ctx, tx, capitalReserve(a, f.usd, usdUnits(200)))
				return err
			})
		}()
	}
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			assert.Equal(t, errs.CodeInsufficientBuyingPower, errs.CodeOf(err), "%v", err)
		}
	}
	f.requireBacked(a)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM variation_margin_runs WHERE instrument_id = $1`, perp))
}

// --- F9: terms are frozen while a contract has open positions ---------------------------

// TestIntegration_TermsAreFrozenWhileTheContractHasOpenPositions: the Go
// pre-check and the database both refuse an amendment while a position is
// open; once every position is closed the terms can change again.
func TestIntegration_TermsAreFrozenWhileTheContractHasOpenPositions(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a := f.account()
	f.fund(a, usdUnits(100_000))
	f.mark(perp, 65_000, 65_000)
	f.open(a, perp, Long, 1)

	err := RequireTermsAmendable(f.ctx, testDB, perp)
	requireCode(t, err, errs.CodeConflict)
	amend := func() error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			cur, err := f.repo.CurrentTerms(ctx, tx, perp)
			if err != nil {
				return err
			}
			next := cur.Terms
			next.ContractMultiplier = q(2_000_000)
			_, err = f.repo.AmendTerms(ctx, tx, perp, next, "contract size change", "OPERATOR", "ops")
			return err
		})
	}
	requireSQLState(t, amend(), "DV007")

	f.closeAll(a, perp)
	require.NoError(t, RequireTermsAmendable(f.ctx, testDB, perp))
	require.NoError(t, amend())
	assert.Equal(t, "2000000", f.spec(perp).Multiplier.String())
}

// TestIntegration_TheDatabaseRefusesATermsChangeUnderOpenPositions: written
// around Go, neither a new terms version nor an edit of the current terms row
// can land while a position is open (DV007).
func TestIntegration_TheDatabaseRefusesATermsChangeUnderOpenPositions(t *testing.T) {
	f := newFixture(t)
	p := f.openPosition(1)
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE instrument_terms SET contract_multiplier = contract_multiplier * 2 WHERE instrument_id = $1`, p.InstrumentID)
		return err
	})
	requireSQLState(t, err, "DV007")
	digest := sha256.Sum256([]byte("raw terms"))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO instrument_versions (id, instrument_id, version, terms, content_hash, reason, actor_type, actor_id)
			VALUES ($1,$2,2,'{}'::jsonb,$3,'raw','OPERATOR','raw')`, uuid.New(), p.InstrumentID, digest[:])
		return err
	})
	requireSQLState(t, err, "DV007")
}

// TestIntegration_ATermsAmendmentAndAFirstOpenCannotPassEachOther: an
// amendment racing the first open of a contract either loses (the open
// committed first, so the amendment finds an open position and is refused) or
// wins entirely (the open waited for it and was computed on the new terms).
// Never both on different terms.
func TestIntegration_ATermsAmendmentAndAFirstOpenCannotPassEachOther(t *testing.T) {
	f := newFixture(t)
	for range 6 {
		perp := f.perp()
		a := f.account()
		f.fund(a, usdUnits(100_000))
		f.mark(perp, 65_000, 65_000)
		var wg sync.WaitGroup
		var openErr, amendErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, openErr = f.tryOpen(OpenRequest{IntentRequest: f.req(a, perp), Side: Long, Quantity: q(1)})
		}()
		go func() {
			defer wg.Done()
			amendErr = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				cur, err := f.repo.CurrentTerms(ctx, tx, perp)
				if err != nil {
					return err
				}
				next := cur.Terms
				next.ContractMultiplier = q(2_000_000)
				_, err = f.repo.AmendTerms(ctx, tx, perp, next, "contract size change", "OPERATOR", "ops")
				return err
			})
		}()
		wg.Wait()
		require.NoError(t, openErr)
		if amendErr != nil {
			requireSQLState(t, amendErr, "DV007")
			continue
		}
		pos := f.position(a, perp)
		price := f.fillPrice(pos.ID)
		want, err := f.spec(perp).Value(q(1), price)
		require.NoError(t, err)
		assert.Equal(t, want.String(), pos.CostValue.String(), "the open waited for the amendment and used its terms")
	}
}

func (f *fixture) fillPrice(pid PositionID) money.Quantity {
	f.t.Helper()
	var p money.Quantity
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT price_mantissa FROM derivative_fills WHERE position_id = $1 ORDER BY occurred_at LIMIT 1`, pid).Scan(&p))
	return p
}

// --- SQL and Go apply the same arithmetic -------------------------------------------

// TestIntegration_SQLHalfEvenIsGoHalfEven: the database's half-even division
// (which binds an option's settlement price) agrees with money.RoundHalfEven
// on every input.
func TestIntegration_SQLHalfEvenIsGoHalfEven(t *testing.T) {
	requireEnv(t)
	rapid.Check(t, func(rt *rapid.T) {
		num := q(rapid.Int64Range(0, 1<<60).Draw(rt, "num"))
		den := q(rapid.Int64Range(1, 1<<40).Draw(rt, "den"))
		want, err := num.Div(den, money.RoundHalfEven)
		if err != nil {
			rt.Fatal(err)
		}
		var got money.Quantity
		if err := testDB.QueryRow(context.Background(), `SELECT cp_div_half_even($1::numeric, $2::numeric)::numeric(38,0)`, num, den).Scan(&got); err != nil {
			rt.Fatal(err)
		}
		if !got.Equal(want) {
			rt.Fatalf("%s / %s: SQL %s, Go %s", num, den, got, want)
		}
	})
}

// TestIntegration_EveryHardeningEnumCheckIsItsGoList pairs the CHECK lists of
// migration 00755 with their Go functions.
func TestIntegration_EveryHardeningEnumCheckIsItsGoList(t *testing.T) {
	requireEnv(t)
	for _, p := range []struct {
		table, constraint string
		want              []string
	}{
		{"derivative_settlements", "derivative_settlements_price_rule_check", enumStrings(AllSettlementPriceRules())},
		{"derivative_settlement_override_decisions", "derivative_settlement_override_decisions_decision_check", enumStrings(AllOverrideDecisions())},
	} {
		var def string
		require.NoError(t, testDB.QueryRow(context.Background(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid = $1::regclass AND conname = $2`, p.table, p.constraint).Scan(&def), p.constraint)
		var got []string
		for _, m := range enumQuoted.FindAllStringSubmatch(def, -1) {
			got = append(got, m[1])
		}
		assert.Equal(t, p.want, got, "%s.%s: %s", p.table, p.constraint, def)
	}
}

var enumQuoted = regexp.MustCompile(`'([^']*)'::text`)
