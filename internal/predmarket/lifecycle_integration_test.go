//go:build integration

package predmarket

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func reasonCodes(t *testing.T, err error) []string {
	t.Helper()
	e, ok := errs.As(err)
	require.True(t, ok, "%v", err)
	codes, _ := e.Fields["reason_codes"].([]string)
	return codes
}

// TestIntegration_NoIssuanceOrTradingAfterClose: from close time on, nothing
// is issued, redeemed or traded. The clock alone refuses before the
// scheduler has closed the market; after it has, the outcome instruments are
// HALTED and the market instrument CLOSE_ONLY, so Risk V2 refuses on every
// path -- this service's and the order book's.
func TestIntegration_NoIssuanceOrTradingAfterClose(t *testing.T) {
	f := newFixture(t)
	a := f.customer(20)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 2)

	// At close time, before anything ran: the session is over.
	f.clk.Set(m.CloseAt)
	req := f.positionRequest(m, a, 1)
	err := f.inTxKeepingRefusal(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Issue(ctx, tx, req)
		return err
	})
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Subset(t, reasonCodes(t, err), []string{risk.ReasonInstrumentSessionClosed, risk.ReasonInstrumentExpired})
	var status, code string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status, coalesce(rejection_code,'') FROM economic_intents
		WHERE account_id = $1 AND idempotency_key = $2`, a, req.IdempotencyKey).Scan(&status, &code))
	assert.Equal(t, "REJECTED", status, "the refusal is on record")
	assert.Equal(t, string(errs.CodeRiskRejected), code)
	_, err = f.redeem(m, a, 1)
	requireCode(t, err, errs.CodeRiskRejected, "no redemption after close either")

	// The scheduler closes it.
	m = f.closeMarket(m)
	for _, o := range m.Outcomes {
		assert.Equal(t, assets.StatusHalted, f.instrumentStatus(o.InstrumentID), "no trading after close, on any path")
	}
	assert.Equal(t, assets.StatusCloseOnly, f.instrumentStatus(m.InstrumentID))
	_, err = f.tryIssue(f.positionRequest(m, a, 1))
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, reasonCodes(t, err), risk.ReasonInstrumentCloseOnly)

	// The order book's path: an order on an outcome instrument, evaluated with
	// the instrument section this service supplies, is refused whichever way
	// it trades.
	for _, o := range m.Outcomes {
		snap, err := f.svc.InstrumentSnapshot(f.ctx, testDB, m.ID, o.InstrumentID)
		require.NoError(t, err)
		assert.Equal(t, string(assets.StatusHalted), snap.Status)
		assert.False(t, snap.SessionOpen)
		assert.True(t, snap.Expired)
		for _, kind := range []string{"ACQUIRE_EXPOSURE", "REDUCE_EXPOSURE"} {
			d := risk.EvaluateAdvanced(risk.DefaultAdvancedPolicy(), risk.AdvancedInput{
				Stage: risk.StagePreTrade, Now: f.clk.Now(),
				Intent: risk.AdvancedIntent{ID: uuid.NewString(), Kind: kind, AccountID: a.String(), ActorType: "USER",
					Capability: string(gates.InternalMatching), Mode: "PAPER", InstrumentID: o.InstrumentID.String(),
					InstrumentKind: "EVENT_OUTCOME", NotionalUSD: money.USDFromMinor(100), RiskIncreasing: kind == "ACQUIRE_EXPOSURE"},
				Identity: &risk.IdentitySnapshot{Verified: true}, Account: &risk.AdvancedAccountSnapshot{Status: "ACTIVE"},
				Instrument: &snap, Oracle: &risk.OracleSnapshot{Available: true, ObservedAt: f.clk.Now()},
			})
			assert.Equal(t, risk.Reject, d.Verdict, kind)
			assert.Contains(t, d.ReasonCodes, risk.ReasonInstrumentNotTradable, kind)
		}
	}
	// Before close the same order would have passed the instrument check.
	f2 := newFixture(t)
	open := f2.openMarket("YES", "NO")
	snap, err := f2.svc.InstrumentSnapshot(f2.ctx, testDB, open.ID, open.Outcomes[0].InstrumentID)
	require.NoError(t, err)
	assert.Equal(t, risk.InstrumentSnapshot{Status: "ACTIVE", SessionOpen: true, Expired: false}, snap)
}

// TestIntegration_NoSettlementBeforeResolution: settlement waits for the
// resolution and then for the dispute window; resolution waits for close.
func TestIntegration_NoSettlementBeforeResolution(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	spec := f.spec("YES", "NO")
	spec.DisputeWindow = time.Hour
	m := f.openMarketFrom(spec)
	f.issue(m, a, 3)

	_, err := f.trySettle(m, f.third)
	requireCode(t, err, errs.CodeInvalidStateTransition, "OPEN")

	// Past close time but not yet closed: a perfectly good report is refused.
	f.clk.Set(m.CloseAt.Add(2 * time.Minute))
	early := f.report(m, "YES")
	_, err = f.tryResolve(m, early, f.resolver)
	requireCode(t, err, errs.CodeInvalidStateTransition, "no resolution before close")

	m = f.closeMarket(m)
	_, err = f.trySettle(m, f.third)
	requireCode(t, err, errs.CodeInvalidStateTransition, "CLOSED")
	_, err = f.tryResolve(m, early, f.resolver)
	requireCode(t, err, errs.CodeInvalidStateTransition, "a report waits for the resolution request")
	f.requestResolution(m)
	_, err = f.trySettle(m, f.third)
	requireCode(t, err, errs.CodeInvalidStateTransition, "RESOLVING")

	// A report observed before close cannot resolve the market.
	beforeClose, err := f.oracle.Sign(OracleReport{MarketID: m.ID, Outcome: "YES", ObservedAt: m.CloseAt.Add(-time.Second), Evidence: []byte("early")})
	require.NoError(t, err)
	_, err = f.tryResolve(m, beforeClose, f.resolver)
	requireCode(t, err, errs.CodeValidationFailed)

	res, err := f.tryResolve(m, early, f.resolver)
	require.NoError(t, err)
	_, err = f.trySettle(m, f.third)
	requireCode(t, err, errs.CodeInvalidStateTransition, "inside the dispute window")
	f.clk.Set(res.Resolution.AcceptedAt.Add(time.Hour))
	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	assert.Equal(t, "3000000", out.Settlement.TotalPaid.String())
}

// TestIntegration_OracleReplayIsIdempotentAndAConflictingReportIsRefused:
// replaying an accepted report changes nothing and posts nothing; a different
// report after resolution is refused unless the market is DISPUTED.
func TestIntegration_OracleReplayIsIdempotentAndAConflictingReportIsRefused(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 2)
	first := f.resolve(m, "YES")
	report, err := f.svc.FetchReport(f.ctx, testDB, m.ID)
	require.NoError(t, err)
	// The oracle then corrects itself: a later, different observation, which
	// FetchReport now returns and a RESOLVED market refuses.
	conflicting := f.report(m, "NO")
	require.NotEqual(t, report.Hash(), conflicting.Hash())

	snapshot := func() [4]int {
		return [4]int{
			f.count(`SELECT count(*) FROM prediction_resolutions WHERE market_id = $1`, m.ID),
			f.count(`SELECT count(*) FROM prediction_market_transitions WHERE market_id = $1`, m.ID),
			f.count(`SELECT count(*) FROM journal_transactions`),
			f.count(`SELECT count(*) FROM audit_events WHERE resource_id = $1`, m.ID.String()),
		}
	}
	before := snapshot()
	for _, by := range []econ.Actor{f.resolver, f.third} {
		again, err := f.tryResolve(m, report, by)
		require.NoError(t, err)
		assert.True(t, again.Replay)
		assert.Equal(t, first.Resolution.ID, again.Resolution.ID)
	}
	assert.Equal(t, before, snapshot(), "a replay records nothing")

	_, err = f.tryResolve(m, conflicting, f.resolver)
	requireCode(t, err, errs.CodeConflict)
	assert.Equal(t, before, snapshot())

	_, err = f.trySettle(m, f.third)
	require.NoError(t, err)
	after := snapshot()
	again, err := f.tryResolve(m, report, f.resolver)
	require.NoError(t, err)
	assert.True(t, again.Replay, "replay after settlement is still a no-op")
	_, err = f.tryResolve(m, conflicting, f.resolver)
	requireCode(t, err, errs.CodeConflict)
	assert.Equal(t, after, snapshot())
}

// TestIntegration_AnUnverifiableOracleReportIsRefused: a report is accepted
// only signed by the market's oracle, for that market, naming one of its
// outcomes, and never from the market's creator.
func TestIntegration_AnUnverifiableOracleReportIsRefused(t *testing.T) {
	f := newFixture(t)
	m := f.openMarket("YES", "NO")
	other := f.openMarket("YES", "NO")
	f.closeMarket(m)
	f.closeMarket(other)
	f.requestResolution(m)
	f.requestResolution(other)
	good := f.report(m, "YES")

	impostor, err := NewSandboxOracle(f.oracle.Source(), sha256Seed("not the market's oracle"))
	require.NoError(t, err)
	forged, err := impostor.Sign(OracleReport{MarketID: m.ID, Outcome: "YES", ObservedAt: good.ObservedAt, Evidence: good.Evidence})
	require.NoError(t, err)
	_, err = f.tryResolve(m, forged, f.resolver)
	requireCode(t, err, errs.CodeForbidden, "a report signed by another key")

	tampered := good
	tampered.Outcome = "NO"
	_, err = f.tryResolve(m, tampered, f.resolver)
	requireCode(t, err, errs.CodeForbidden, "a report whose outcome was changed after signing")

	_, err = f.tryResolve(other, good, f.resolver)
	requireCode(t, err, errs.CodeForbidden, "a report for another market")

	unknown, err := f.oracle.Sign(OracleReport{MarketID: m.ID, Outcome: "MAYBE", ObservedAt: good.ObservedAt, Evidence: good.Evidence})
	require.NoError(t, err)
	_, err = f.tryResolve(m, unknown, f.resolver)
	requireCode(t, err, errs.CodeValidationFailed, "an outcome the market does not have")

	_, err = f.tryResolve(m, good, f.creator)
	requireCode(t, err, errs.CodeForbidden, "the creator cannot resolve their own market")
	_, err = f.tryResolve(m, good, econ.Actor{Type: security.ActorAgent, ID: uuid.NewString()})
	requireCode(t, err, errs.CodeForbidden, "an agent never resolves")

	assert.Equal(t, StatusResolving, f.get(m).Status, "nothing was accepted")
	res, err := f.tryResolve(m, good, f.resolver)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Resolution.OutcomeIndex)
	assert.NotEmpty(t, res.Resolution.EvidenceRef, "the oracle evidence is archived")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM provider_evidence WHERE key LIKE $1`, "advanced/predmarket/oracle/"+m.ID.String()+"/%"))
}

// TestIntegration_ADisputedMarketCannotSettle: a dispute blocks settlement
// until a corrected report or a ruling closes it; the disputer and the
// creator cannot rule, and a ruling is final.
func TestIntegration_ADisputedMarketCannotSettle(t *testing.T) {
	f := newFixture(t)
	yesHolder, noHolder := f.customer(10), f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, yesHolder, 2)
	f.issue(m, noHolder, 2)
	f.trade(yesHolder, noHolder, m.Outcomes[1].ShareAssetID, 2_000_000, 0)
	f.trade(noHolder, yesHolder, m.Outcomes[0].ShareAssetID, 2_000_000, 0)
	f.resolve(m, "YES")

	var d Dispute
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, _, err = f.svc.Dispute(ctx, tx, DisputeRequest{MarketID: m.ID, Actor: f.third,
			Reason: "the board published a correction an hour later", Evidence: map[string]string{"correction": "NO certified"}})
		return err
	}))
	assert.Equal(t, f.third.ID, d.RaisedBy.ID, "who")
	assert.NotEmpty(t, d.Reason, "why")
	assert.NotEmpty(t, d.EvidenceRef, "on what evidence")
	assert.Equal(t, StatusDisputed, f.get(m).Status)

	_, err := f.trySettle(m, f.resolver)
	requireCode(t, err, errs.CodeInvalidStateTransition, "a disputed market cannot settle")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.Dispute(ctx, tx, DisputeRequest{MarketID: m.ID, Actor: f.resolver, Reason: "a second dispute", Evidence: "x"})
		return err
	})
	requireCode(t, err, errs.CodeConflict)

	corrected := f.report(m, "NO")
	_, err = f.tryResolve(m, corrected, f.third)
	requireCode(t, err, errs.CodeForbidden, "the disputer cannot rule")
	_, err = f.tryResolve(m, corrected, f.creator)
	requireCode(t, err, errs.CodeForbidden, "the creator cannot rule")
	fourth := f.operator()
	ruled, err := f.tryResolve(m, corrected, fourth)
	require.NoError(t, err)
	assert.Equal(t, ResolutionDisputeOverturned, ruled.Resolution.Kind)
	assert.Equal(t, 1, ruled.Resolution.OutcomeIndex)
	require.NotNil(t, ruled.Resolution.DisputeID)
	assert.Equal(t, d.ID, *ruled.Resolution.DisputeID)
	assert.Equal(t, StatusResolved, ruled.Market.Status)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.Dispute(ctx, tx, DisputeRequest{MarketID: m.ID, Actor: f.resolver, Reason: "dispute the ruling", Evidence: "x"})
		return err
	})
	requireCode(t, err, errs.CodeConflict, "a ruling is final")

	history, err := f.svc.Resolutions(f.ctx, testDB, m.ID)
	require.NoError(t, err)
	require.Len(t, history, 2, "the disputed resolution stays on record beside the ruling")
	assert.Equal(t, ResolutionOracle, history[0].Kind)
	require.NotNil(t, history[1].SupersedesID)
	assert.Equal(t, history[0].ID, *history[1].SupersedesID)
	disputes, err := f.svc.Disputes(f.ctx, testDB, m.ID)
	require.NoError(t, err)
	require.Len(t, disputes, 1)
	assert.Equal(t, history[0].ID, disputes[0].ResolutionID)

	before := f.wallet(noHolder, f.collateral)
	out, err := f.trySettle(m, f.resolver)
	require.NoError(t, err)
	assert.Equal(t, ruled.Resolution.ID, *out.Settlement.ResolutionID, "settlement pays the ruling, not the disputed report")
	assert.Equal(t, units(4).String(), f.wallet(noHolder, f.collateral).Sub(before).String())
}

// TestIntegration_ADisputeCanBeUpheldOrEndInAVoid covers the two other ways
// out of DISPUTED.
func TestIntegration_ADisputeCanBeUpheldOrEndInAVoid(t *testing.T) {
	f := newFixture(t)
	a := f.customer(10)
	dispute := func(m Market) {
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := f.svc.Dispute(ctx, tx, DisputeRequest{MarketID: m.ID, Actor: f.third,
				Reason: "the source was ambiguous", Evidence: map[string]string{"screenshot": "sha256:..."}})
			return err
		}))
	}
	uphold := func(m Market, by econ.Actor) (ResolveResult, error) {
		var out ResolveResult
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.svc.UpholdResolution(ctx, tx, RulingRequest{MarketID: m.ID, Actor: by, Reason: "the original report stands"})
			return err
		})
		return out, err
	}

	upheld := f.openMarket("YES", "NO")
	f.issue(upheld, a, 2)
	original := f.resolve(upheld, "YES")
	dispute(upheld)
	_, err := uphold(upheld, f.third)
	requireCode(t, err, errs.CodeForbidden, "the disputer cannot rule")
	_, err = uphold(upheld, f.creator)
	requireCode(t, err, errs.CodeForbidden, "the creator cannot rule")
	_, err = uphold(upheld, f.resolver)
	requireCode(t, err, errs.CodeForbidden, "the operator whose resolution is disputed cannot uphold it")
	ruler := f.operator()
	ruled, err := uphold(upheld, ruler)
	require.NoError(t, err)
	assert.Equal(t, ResolutionDisputeUpheld, ruled.Resolution.Kind)
	assert.Equal(t, original.Resolution.OutcomeIndex, ruled.Resolution.OutcomeIndex)
	assert.Equal(t, ruler.ID, ruled.Resolution.AcceptedBy.ID)
	_, err = f.trySettle(upheld, f.third)
	require.NoError(t, err)

	// A void ends a dispute too, approved by an operator who is not the
	// disputer: the approval is the ruling.
	voided := f.openMarket("YES", "NO")
	f.issue(voided, a, 2)
	f.resolve(voided, "NO")
	dispute(voided)
	proposal, err := f.tryProposeVoid(f.get(voided), f.third)
	require.NoError(t, err, "the disputer may propose the void")
	_, err = f.tryApproveVoid(f.get(voided), proposal, f.third)
	requireCode(t, err, errs.CodeForbidden, "and cannot approve it")
	out, err := f.tryApproveVoid(f.get(voided), proposal, f.approver)
	require.NoError(t, err)
	assert.Equal(t, StatusVoided, out.Market.Status)
	assert.Equal(t, "2000000", out.Settlement.TotalPaid.String())
	assert.Equal(t, f.approver.ID, out.Settlement.SettledBy.ID, "the refund is settled by the approver")
}

// TestIntegration_RiskRefusesAnUnverifiedIssuerAndTheRefusalIsOnRecord: the
// identity check of Risk V2 refuses; the decision and the REJECTED intent
// are kept when the caller commits, and a replay is refused again without a
// second evaluation.
func TestIntegration_RiskRefusesAnUnverifiedIssuerAndTheRefusalIsOnRecord(t *testing.T) {
	f := newFixture(t)
	u := f.customerWith(false, 10)
	m := f.openMarket("YES", "NO")
	req := f.positionRequest(m, u, 1)
	issue := func() error {
		return f.inTxKeepingRefusal(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.Issue(ctx, tx, req)
			return err
		})
	}
	err := issue()
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, reasonCodes(t, err), risk.ReasonIdentityUnverified)
	decisions := `SELECT count(*) FROM advanced_risk_decisions WHERE account_id = $1 AND verdict = 'REJECT'`
	assert.Equal(t, 1, f.count(decisions, u))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND status = 'REJECTED'`, u))

	err = issue()
	requireCode(t, err, errs.CodeRiskRejected, "the same request is refused again")
	assert.Equal(t, 1, f.count(decisions, u), "and not evaluated twice")
	assert.True(t, f.books(m).Escrow.IsZero())
	assert.Equal(t, units(10).String(), f.wallet(u, f.collateral).String())
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1`, u))

	// An agent request without the delegation facts the kernel needs is
	// refused too: missing inputs never pass.
	v := f.customer(10)
	agentReq := f.positionRequest(m, v, 1)
	agentReq.ActorType, agentReq.ActorID = security.ActorAgent, "agent-1"
	agentReq.AgentID, agentReq.AgentAuthority = uuid.NewString(), agentauthority.LevelAutonomousPortfolio
	_, err = f.tryIssue(agentReq)
	require.Error(t, err)
	assert.Contains(t, []errs.Code{errs.CodeRiskRejected, errs.CodeForbidden}, errs.CodeOf(err), "%v", err)
	assert.True(t, f.books(m).Escrow.IsZero())
}

// switchableKills is a kill-switch reader a test can flip.
type switchableKills struct {
	mu       sync.Mutex
	switches []killswitch.Switch
}

func (k *switchableKills) Snapshot(context.Context, db.Querier) (killswitch.Snapshot, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return killswitch.Snapshot{Switches: append([]killswitch.Switch(nil), k.switches...)}, nil
}

func (k *switchableKills) set(sw ...killswitch.Switch) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.switches = sw
}

// TestIntegration_AKillSwitchStopsIssuanceAndSettlementButNeverClosing: a
// CAPABILITY_DISABLE switch refuses every economic action of the capability,
// an INSTRUMENT_HALT refuses issuance on the market instrument in Risk V2,
// and closing a market stays possible throughout.
func TestIntegration_AKillSwitchStopsIssuanceAndSettlementButNeverClosing(t *testing.T) {
	kills := &switchableKills{}
	f := newFixture(t, withKillSwitches(kills))
	a := f.customer(10)
	m := f.openMarket("YES", "NO")
	f.issue(m, a, 1)

	kills.set(killswitch.Switch{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.PredictionMarkets), Active: true})
	_, err := f.tryIssue(f.positionRequest(m, a, 1))
	requireCode(t, err, errs.CodeKillSwitchActive)
	f.closeMarket(m) // closing needs no authorization
	f.requestResolution(m)
	_, err = f.tryResolve(m, f.report(m, "YES"), f.resolver)
	require.NoError(t, err)
	_, err = f.trySettle(m, f.third)
	requireCode(t, err, errs.CodeKillSwitchActive)
	kills.set()
	_, err = f.trySettle(m, f.third)
	require.NoError(t, err)

	m2 := f.openMarket("YES", "NO")
	kills.set(killswitch.Switch{Kind: killswitch.InstrumentHalt, ScopeID: m2.InstrumentID.String(), Active: true})
	_, err = f.tryIssue(f.positionRequest(m2, a, 1))
	requireCode(t, err, errs.CodeRiskRejected)
	assert.Contains(t, reasonCodes(t, err), risk.ReasonKillSwitch)
}

// TestIntegration_MarketCreationIsIdempotentAndFailsClosed: the definition is
// stored and hashed; the same symbol with the same definition is a replay,
// with another definition a refusal; only operators define markets, only in
// PAPER or SANDBOX, only over the sandbox dollar.
func TestIntegration_MarketCreationIsIdempotentAndFailsClosed(t *testing.T) {
	f := newFixture(t)
	spec := f.spec("YES", "NO")
	create := func(sp MarketSpec, by econ.Actor) (Market, bool, error) {
		var m Market
		var existing bool
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			m, existing, err = f.svc.CreateMarket(ctx, tx, sp, by)
			return err
		})
		return m, existing, err
	}
	m, existing, err := create(spec, f.creator)
	require.NoError(t, err)
	assert.False(t, existing)
	assert.Equal(t, StatusDraft, m.Status)
	assert.Len(t, m.Outcomes, 2)
	assert.Equal(t, assets.StatusHalted, f.instrumentStatus(m.InstrumentID), "nothing trades in a DRAFT")
	for _, o := range m.Outcomes {
		assert.Equal(t, assets.StatusHalted, f.instrumentStatus(o.InstrumentID))
		share, err := assets.NewRepository().Get(f.ctx, testDB, o.ShareAssetID)
		require.NoError(t, err)
		assert.Equal(t, assets.KindSimulated, share.Kind)
		assert.Equal(t, assets.SandboxChain, share.Chain)
		assert.Equal(t, uint8(6), share.Decimals)
	}
	again, existing, err := create(spec, f.creator)
	require.NoError(t, err)
	assert.True(t, existing)
	assert.Equal(t, m.ID, again.ID)

	changed := spec
	changed.Question = "Something else entirely?"
	_, _, err = create(changed, f.creator)
	requireCode(t, err, errs.CodeInvalidIdempotencyReuse)

	_, _, err = create(f.spec("YES", "NO"), econ.Actor{Type: security.ActorAgent, ID: uuid.NewString()})
	requireCode(t, err, errs.CodeForbidden, "an agent never defines a market")
	_, _, err = create(f.spec("YES", "NO"), econ.Actor{Type: security.ActorUser, ID: uuid.NewString()})
	requireCode(t, err, errs.CodeForbidden, "neither does a customer")

	live := f.spec("YES", "NO")
	live.Mode = gates.ModeLive
	_, _, err = create(live, f.creator)
	requireCode(t, err, errs.CodeValidationFailed, "LIVE is not a mode a prediction market has")

	for name, asset := range map[string]assets.Asset{
		"not pegged":   {Decimals: 6},
		"two decimals": {Decimals: 2, IsStablecoin: true, PegCurrency: "USD"},
	} {
		asset.Chain, asset.Kind, asset.ValueDomain = assets.SandboxChain, assets.KindSimulated, valuedomain.Simulated
		asset.Symbol, asset.Name, asset.RiskClass, asset.Status = "PMX", "test", assets.RiskSettlement, assets.StatusActive
		created, err := assets.NewRepository().Create(f.ctx, testDB, asset)
		require.NoError(t, err, name)
		sp := f.spec("YES", "NO")
		sp.CollateralAssetID = created.ID
		_, _, err = create(sp, f.creator)
		requireCode(t, err, errs.CodeValidationFailed, name)
	}

	past := f.spec("YES", "NO")
	past.OpenAt, past.CloseAt = f.clk.Now().Add(-2*time.Hour), f.clk.Now().Add(-time.Hour)
	_, _, err = create(past, f.creator)
	requireCode(t, err, errs.CodeValidationFailed, "a market closing in the past")

	// A DRAFT issues nothing: its instruments are halted.
	a := f.customer(5)
	_, err = f.tryIssue(f.positionRequest(m, a, 1))
	requireCode(t, err, errs.CodeRiskRejected)
}

// TestIntegration_AMarketOpensAndClosesOnlyOnTime: the clock decides.
func TestIntegration_AMarketOpensAndClosesOnlyOnTime(t *testing.T) {
	f := newFixture(t)
	spec := f.spec("YES", "NO")
	spec.OpenAt = f.clk.Now().Add(10 * time.Minute)
	var m Market
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		m, _, err = f.svc.CreateMarket(ctx, tx, spec, f.creator)
		return err
	}))
	open := func() error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.Open(ctx, tx, m.ID, f.system)
			return err
		})
	}
	closeNow := func() error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.Close(ctx, tx, m.ID, f.system)
			return err
		})
	}
	requireCode(t, open(), errs.CodeInvalidStateTransition, "not before open time")
	requireCode(t, closeNow(), errs.CodeInvalidStateTransition, "a DRAFT is voided, not closed")
	f.clk.Advance(10 * time.Minute)
	require.NoError(t, open())
	require.NoError(t, open(), "opening an OPEN market is a no-op")
	requireCode(t, closeNow(), errs.CodeInvalidStateTransition, "not before close time")
	f.clk.Set(m.CloseAt)
	require.NoError(t, closeNow())
	require.NoError(t, closeNow(), "closing a CLOSED market is a no-op")
	assert.Equal(t, StatusClosed, f.get(m).Status)
	assert.Equal(t, 3, f.count(`SELECT count(*) FROM prediction_market_transitions WHERE market_id = $1`, m.ID))
}
