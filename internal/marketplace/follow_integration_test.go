//go:build integration

package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
)

func TestIntegration_PaperFollowProducesIntentsAndEvidence(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	journalsBefore := f.count(`SELECT count(*) FROM journal_transactions`)

	input := f.input(f.clk.Now())
	run, err := f.follow(f.svc, in, input)
	require.NoError(t, err)
	assert.Equal(t, sandboxrt.TerminationOK, run.Termination)
	assert.Equal(t, 1, run.ProposalsCount)
	assert.Equal(t, 1, run.AcceptedCount)
	assert.Zero(t, run.ViolationsCount)
	require.Len(t, run.Intents, 1)
	fi := run.Intents[0]
	assert.Equal(t, OutcomeDispatched, fi.Outcome)
	assert.Equal(t, "150.25000000", fi.Notional)
	assert.Equal(t, "150.25000000", run.Turnover)
	assert.Equal(t, "10300.50000000", run.Equity, "10000.00 cash + 2 units x 150.25, exactly")
	assert.True(t, strings.HasPrefix(run.EvidenceRef, "pg://"))
	assert.Len(t, run.EvidenceSHA256, 32)

	// What reached execution: one SERVICE intent of the strategy, risk-approved.
	calls := f.exec.snapshot()
	require.Len(t, calls, 1)
	c := calls[0]
	assert.Equal(t, fi.IntentID, c.IntentID)
	assert.Equal(t, econ.StatusRiskApproved, c.StatusAtCall)
	assert.Equal(t, risk.Allow, c.Verdict)
	assert.Equal(t, security.ActorService, c.ActorType)
	assert.Equal(t, v.StrategyRef(), c.StrategyRef)
	assert.Equal(t, gates.ModePaper, c.Mode)

	intent, err := f.intents.Get(f.ctx, testDB, fi.IntentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusCompleted, intent.Status)
	assert.Equal(t, gates.StrategyMarketplace, intent.Capability)
	assert.Equal(t, econ.KindAcquireExposure, intent.Kind)
	assert.Equal(t, f.insA, intent.InstrumentID)
	assert.Equal(t, f.follower, intent.AccountID)
	var terms sandboxrt.IntentTerms
	require.NoError(t, json.Unmarshal(intent.Terms, &terms))
	assert.Equal(t, sandboxrt.OriginSandbox, terms.Origin)
	assert.Equal(t, run.SandboxRunID.String(), terms.SandboxRunID)

	var verdict, sandboxOriginated string
	var violations int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT verdict, input->'intent'->>'sandbox_originated', (input->'sandbox'->>'violations')::int
		FROM advanced_risk_decisions WHERE intent_id = $1`, fi.IntentID).Scan(&verdict, &sandboxOriginated, &violations))
	assert.Equal(t, "ALLOW", verdict)
	assert.Equal(t, "true", sandboxOriginated)
	assert.Zero(t, violations)

	var status, subsystem, kind, evidence string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status, subsystem, effect_kind, coalesce(evidence_ref,'') FROM economic_effects WHERE intent_id = $1`,
		fi.IntentID).Scan(&status, &subsystem, &kind, &evidence))
	assert.Equal(t, "APPLIED", status)
	assert.Equal(t, "MARKETPLACE", subsystem)
	assert.Equal(t, DispatchEffectKind, kind)
	assert.NotEmpty(t, evidence)
	assert.Equal(t, journalsBefore, f.count(`SELECT count(*) FROM journal_transactions`), "the marketplace itself never posts")

	// The same snapshot again is the same run: nothing runs or dispatches twice.
	again, err := f.follow(f.svc, in, input)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, run.ID, again.ID)
	assert.Len(t, f.exec.snapshot(), 1)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM marketplace_follow_runs WHERE install_id = $1`, in.ID))

	changed := input
	changed.Instruments = append([]sandboxrt.InputInstrument(nil), input.Instruments...)
	changed.Instruments[0].Mark = "151.00"
	_, err = f.follow(f.svc, in, changed)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err), "one snapshot time, one input")

	// The manifest's execution frequency holds.
	f.clk.Advance(30 * time.Minute)
	_, err = f.follow(f.svc, in, f.input(f.clk.Now()))
	assert.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	f.clk.Advance(30 * time.Minute)
	_, err = f.follow(f.svc, in, f.input(f.clk.Now().Add(-10*time.Minute)))
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err), "a stale snapshot is refused, not followed")
	second, err := f.follow(f.svc, in, f.input(f.clk.Now()))
	require.NoError(t, err)
	assert.NotEqual(t, run.ID, second.ID)
	assert.Len(t, f.exec.snapshot(), 2)
}

func TestIntegration_ProposalsBeyondManifestDroppedAndCounted(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("greedy")
	in := f.install(v)
	run, err := f.follow(f.svc, in, f.input(f.clk.Now()))
	require.NoError(t, err)
	assert.Equal(t, sandboxrt.TerminationOK, run.Termination)
	assert.Equal(t, 6, run.ProposalsCount)
	assert.Equal(t, 2, run.AcceptedCount)
	assert.Equal(t, 4, run.ViolationsCount)

	violations, err := f.sandbox.RunViolations(f.ctx, testDB, run.SandboxRunID)
	require.NoError(t, err)
	got := map[int]sandboxrt.ViolationCode{}
	for _, viol := range violations {
		got[viol.Seq] = viol.Code
	}
	assert.Equal(t, map[int]sandboxrt.ViolationCode{
		2: sandboxrt.ViolationManifestKind, 3: sandboxrt.ViolationManifestInstrument,
		4: sandboxrt.ViolationManifestNotional, 6: sandboxrt.ViolationManifestMaxIntents,
	}, got)

	// The two proposals inside the manifest became intents, and the risk
	// kernel rejected both: a run that tried to exceed its manifest gets
	// nothing executed.
	assert.Equal(t, 2, run.IntentsSubmitted)
	assert.Equal(t, 2, run.IntentsRiskRejected)
	assert.Zero(t, run.IntentsDispatched)
	for _, fi := range run.Intents {
		assert.Equal(t, OutcomeRiskRejected, fi.Outcome)
		assert.Contains(t, fi.ReasonCodes, risk.ReasonSandboxViolation)
		intent, err := f.intents.Get(f.ctx, testDB, fi.IntentID)
		require.NoError(t, err)
		assert.Equal(t, econ.StatusRejected, intent.Status)
		assert.Equal(t, "RISK_REJECTED", intent.RejectionCode)
	}
	assert.Empty(t, f.exec.snapshot())
}

func TestIntegration_ConcurrentFollowsOfOneSnapshotDispatchOnce(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	input := f.input(f.clk.Now())
	const n = 8
	var wg sync.WaitGroup
	type outcome struct {
		run FollowRun
		err error
	}
	results := make(chan outcome, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := f.follow(f.svc, in, input)
			results <- outcome{run, err}
		}()
	}
	wg.Wait()
	close(results)
	ids := map[FollowRunID]bool{}
	fresh := 0
	for o := range results {
		require.NoError(t, o.err)
		ids[o.run.ID] = true
		if !o.run.Replayed {
			fresh++
		}
	}
	assert.Len(t, ids, 1, "every caller sees the same follow run")
	assert.Equal(t, 1, fresh, "exactly one ran it")
	assert.Len(t, f.exec.snapshot(), 1, "exactly one dispatch")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_intents WHERE strategy_ref = $1`, v.StrategyRef()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM sandbox_runs WHERE strategy_ref = $1`, v.StrategyRef()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_effects e JOIN economic_intents i ON i.id = e.intent_id
		WHERE i.strategy_ref = $1`, v.StrategyRef()))
}

func TestIntegration_MissingRiskSectionsFailClosed(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	f.sections.omit = true
	run, err := f.follow(f.svc, in, f.input(f.clk.Now()))
	require.NoError(t, err)
	require.Len(t, run.Intents, 1)
	assert.Equal(t, OutcomeRiskRejected, run.Intents[0].Outcome)
	assert.Contains(t, run.Intents[0].ReasonCodes, risk.ReasonInputInvalid)
	assert.Empty(t, f.exec.snapshot(), "nothing the kernel could not evaluate reaches execution")
}

func TestIntegration_DispatchFailureAndUnknownAreRecordedNotRetried(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)

	f.exec.mode = "fail"
	failed, err := f.follow(f.svc, in, f.input(f.clk.Now()))
	require.NoError(t, err)
	require.Len(t, failed.Intents, 1)
	assert.Equal(t, OutcomeDispatchFailed, failed.Intents[0].Outcome)
	assert.Equal(t, 1, failed.IntentsFailed)
	var status, code string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status, coalesce(failure_code,'') FROM economic_effects WHERE intent_id = $1`,
		failed.Intents[0].IntentID).Scan(&status, &code))
	assert.Equal(t, "FAILED", status)
	assert.Equal(t, string(errs.CodeVenueUnavailable), code)
	intent, err := f.intents.Get(f.ctx, testDB, failed.Intents[0].IntentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusRejected, intent.Status)

	f.clk.Advance(time.Hour)
	f.exec.mode = "unknown"
	unknown, err := f.follow(f.svc, in, f.input(f.clk.Now()))
	require.NoError(t, err)
	require.Len(t, unknown.Intents, 1)
	assert.Equal(t, OutcomeDispatchUnknown, unknown.Intents[0].Outcome)
	intent, err = f.intents.Get(f.ctx, testDB, unknown.Intents[0].IntentID)
	require.NoError(t, err)
	assert.Equal(t, econ.StatusExecuting, intent.Status, "what execution recorded before losing the outcome is kept")
	open, err := f.effects.ListUnresolved(f.ctx, testDB, econ.SubsystemMarketplace, time.Now().Add(time.Hour), 1000)
	require.NoError(t, err)
	found := false
	for _, e := range open {
		if e.IntentID != nil && *e.IntentID == unknown.Intents[0].IntentID {
			found = true
			assert.Equal(t, econ.EffectUnknown, e.Status)
		}
	}
	assert.True(t, found, "an unknown dispatch is on the reconciliation worklist")

	calls := len(f.exec.snapshot())
	replay, err := f.follow(f.svc, in, f.input(f.clk.Now()))
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, calls, len(f.exec.snapshot()), "an unknown outcome is never retried")
}

func TestIntegration_ADispatchWithoutAnAllowDecisionIsRefusedByTheDatabase(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("greedy")
	in := f.install(v)
	run, err := f.follow(f.svc, in, f.input(f.clk.Now()))
	require.NoError(t, err)
	require.NotEmpty(t, run.Intents)
	rejected := run.Intents[0]
	require.Equal(t, OutcomeRiskRejected, rejected.Outcome)
	// Write down that the risk-rejected intent was dispatched, with an
	// effect of its own: the database refuses the record.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		iid := rejected.IntentID
		e, _, err := f.effects.Begin(ctx, tx, econ.BeginRequest{Key: "itest-forged:" + uuid.NewString(), Subsystem: econ.SubsystemMarketplace,
			Kind: DispatchEffectKind, IntentID: &iid, Request: map[string]string{"x": "1"}, Actor: econ.SystemActor("forger")})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO marketplace_follow_intents (id, follow_run_id, proposal_seq, intent_id, outcome, risk_decision_hash, effect_id, notional)
			VALUES ($1,$2,99,$3,'DISPATCHED',$4,$5,1)`, uuid.New(), run.ID, rejected.IntentID, rejected.DecisionHash, e.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK009", db.SQLState(err))
}

type rolledBack struct{}

func (rolledBack) Error() string { return "rolled back on purpose" }

func TestIntegration_KillSwitchesStopFollowing(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	ctl, err := killswitch.NewController(f.clk, discardAudit{}, noApprovals{})
	require.NoError(t, err)
	op := security.Principal{SubjectID: uuid.NewString(), ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleOperations}, AuthTime: f.clk.Now(), AMR: []string{"mfa"}}
	follow := func(scope string) (FollowRun, error) {
		var run FollowRun
		var followErr error
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			if _, err := ctl.Activate(security.WithPrincipal(ctx, op), tx, killswitch.CapabilityDisable, scope, "incident"); err != nil {
				return err
			}
			run, followErr = f.svc.PaperFollow(ctx, tx, FollowRequest{InstallID: in.ID, Input: f.input(f.clk.Now()), Trigger: TriggerExplicit, Actor: f.followerActor})
			return rolledBack{}
		})
		require.ErrorAs(t, err, &rolledBack{})
		return run, followErr
	}
	_, err = follow(string(gates.StrategyMarketplace))
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err), "the marketplace's own switch stops the follow")
	run, err := follow(string(gates.UserCodeSandbox))
	require.NoError(t, err)
	assert.Equal(t, sandboxrt.TerminationKilledBySwitch, run.Termination, "the sandbox switch stops the code; the refusal is recorded")
	assert.Empty(t, run.Intents)
	assert.Empty(t, f.exec.snapshot())
}

type discardAudit struct{}

func (discardAudit) Append(context.Context, pgx.Tx, killswitch.AuditEvent) error { return nil }

type noApprovals struct{}

func (noApprovals) VerifyApproved(context.Context, db.Querier, string, string, string) (killswitch.Approval, error) {
	return killswitch.Approval{}, errs.New(errs.CodeForbidden, "no approvals in this test")
}

func TestIntegration_SimulateRecordsAReproducibleEvaluation(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	var snaps []sandboxrt.Input
	for i, mark := range []string{"150.25", "160.00", "140.00"} {
		in := f.input(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * 24 * time.Hour))
		in.Instruments[0].Mark = mark
		snaps = append(snaps, in)
	}
	simulate := func() Evaluation {
		var ev Evaluation
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			ev, err = f.svc.Simulate(ctx, tx, SimulationRequest{VersionID: v.ID, DatasetVersion: "closes-2026-09", Snapshots: snaps, Actor: f.operator})
			return err
		}))
		return ev
	}
	first, second := simulate(), simulate()
	assert.NotEqual(t, first.ID, second.ID)
	assert.Equal(t, first.SnapshotsDigest, second.SnapshotsDigest)
	assert.Equal(t, first.ResultsDigest, second.ResultsDigest, "same artifact, same snapshots, same results")
	assert.Equal(t, 3, first.RunsOK)
	assert.Equal(t, 3, first.ProposalsCount)
	assert.Equal(t, 3, first.AcceptedCount)
	assert.Zero(t, f.count(`SELECT count(*) FROM economic_intents WHERE strategy_ref = $1`, v.StrategyRef()), "a simulation proposes nothing")
	assert.Equal(t, 3, f.count(`SELECT count(*) FROM marketplace_evaluation_runs WHERE evaluation_id = $1`, first.ID))

	p, err := f.svc.Provenance(f.ctx, testDB, v.ID)
	require.NoError(t, err)
	assert.Equal(t, "closes-2026-09", p.DatasetVersion)
	require.NotNil(t, p.EvaluationID)

	_, draft := f.draft("momentum")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Simulate(ctx, tx, SimulationRequest{VersionID: draft.ID, DatasetVersion: "x", Snapshots: snaps, Actor: f.operator})
		return err
	})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err), "a draft has nothing built to simulate")
}

func TestIntegration_PaperPerformanceFromTheFollowHistory(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	for i, mark := range []string{"150.25", "160.00", "140.00"} {
		if i > 0 {
			f.clk.Advance(time.Hour)
		}
		input := f.input(f.clk.Now())
		input.Instruments[0].Mark = mark
		run, err := f.follow(f.svc, in, input)
		require.NoError(t, err)
		require.Equal(t, 1, run.IntentsDispatched)
	}
	var snap PerformanceSnapshot
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		snap, err = f.svc.RecordPerformance(ctx, tx, in.ID, f.operator)
		return err
	}))
	// Equity 10300.50, 10320.00, 10280.00; dispatched 150.25, 160.00, 140.00.
	assert.Equal(t, 3, snap.Points)
	assert.Equal(t, int64(-20), snap.ReturnBPS, "-20.5 / 10300.5")
	assert.Equal(t, int64(39), snap.MaxDrawdownBPS, "40 / 10320")
	assert.Equal(t, int64(28), snap.VolatilityBPS, "|18.931120 - (-38.759690)| / 2, rounded down")
	assert.Equal(t, int64(437), snap.TurnoverBPS, "450.25 / mean(10300.1667)")
	var stored int64
	var digest []byte
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT return_bps, history_digest FROM marketplace_performance_snapshots WHERE install_id = $1`,
		in.ID).Scan(&stored, &digest))
	assert.Equal(t, int64(-20), stored)
	assert.Equal(t, snap.HistoryDigest, digest)
}

// TestIntegration_ExecutionPostingsAreExactlyOnceWithTheFollow: when paper
// execution posts a real SIM_TRADE, the posting commits with the follow run
// (inside its savepoint), balances per party (the database's SM002), and a
// replay of the follow posts nothing again.
func TestIntegration_ExecutionPostingsAreExactlyOnceWithTheFollow(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	led := ledger.NewService(f.clk, "itest")
	base := f.baseOf[f.insA]
	var house econ.HouseAccount
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		house, err = econ.NewRegistry().CreateHouseAccount(ctx, tx, "mkt-itest-"+uuid.NewString(), econ.HouseMarketMaking, econ.SystemActor("itest"))
		if err != nil {
			return err
		}
		fund := func(kind ledger.Kind, debit, credit ledger.AccountRef, qty int64) error {
			ref := uuid.NewString()
			_, err := led.Post(ctx, tx, ledger.Posting{Kind: kind, IdempotencyKey: "fund:" + ref, EffectiveAt: f.clk.Now(),
				Reference: ledger.FinancialEventReference{Type: "itest_funding", ID: ref},
				Entries: []ledger.Entry{{Account: debit, Side: ledger.Debit, Quantity: money.QuantityFromInt64(qty)},
					{Account: credit, Side: ledger.Credit, Quantity: money.QuantityFromInt64(qty)}}})
			return err
		}
		if err := fund(ledger.KindSandboxFunding, ledger.CustomerAccount(f.follower, ledger.CodeWallet, f.quoteAsset),
			ledger.CustomerAccount(f.follower, ledger.CodeCapital, f.quoteAsset), 10_000_000_000); err != nil {
			return err
		}
		return fund(ledger.KindHouseFunding, house.Account(ledger.CodeHouseInventory, base), house.Account(ledger.CodeHouseCapital, base), 1_000_000_000)
	}))
	f.exec.mode, f.exec.ledger, f.exec.house, f.exec.quote, f.exec.baseOf = "post", led, house, f.quoteAsset, f.baseOf

	paperFills := func() int {
		return f.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'SIM_TRADE' AND reference_type = 'paper_fill'`)
	}
	before := paperFills()
	input := f.input(f.clk.Now())
	run, err := f.follow(f.svc, in, input)
	require.NoError(t, err)
	require.Equal(t, 1, run.IntentsDispatched)
	balance := func(ref ledger.AccountRef) string {
		b, err := led.Balance(f.ctx, testDB, ref)
		require.NoError(t, err)
		return b.String()
	}
	assert.Equal(t, "1000000", balance(ledger.CustomerAccount(f.follower, ledger.CodeWallet, base)), "one whole unit bought")
	assert.Equal(t, "9849750000", balance(ledger.CustomerAccount(f.follower, ledger.CodeWallet, f.quoteAsset)), "10000.000000 - 150.250000")
	assert.Equal(t, "999000000", balance(house.Account(ledger.CodeHouseInventory, base)))
	assert.Equal(t, "150250000", balance(house.Account(ledger.CodeHouseInventory, f.quoteAsset)))
	assert.Equal(t, before+1, paperFills())

	again, err := f.follow(f.svc, in, input)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, before+1, paperFills(), "a replayed follow posts nothing again")
	assert.Equal(t, "1000000", balance(ledger.CustomerAccount(f.follower, ledger.CodeWallet, base)))
}

type fixedSnapshots struct{ f *fixture }

func (s fixedSnapshots) Snapshot(_ context.Context, _ db.Querier, _ Install, _ Version, asOf time.Time) (sandboxrt.Input, error) {
	return s.f.input(asOf), nil
}

func TestIntegration_ScheduledFollowsRunWhenDue(t *testing.T) {
	f := newFixture(t)
	svc, err := NewService(Config{Clock: f.clk, Intents: f.intents, Effects: f.effects, Auth: f.auth, Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: f.router, Sandbox: f.sandbox, Builder: testBuilder, Sections: f.sections,
		Executor: f.exec, Audit: f.aw, Snapshots: fixedSnapshots{f}})
	require.NoError(t, err)
	_, v := f.approved("momentum")
	in := f.install(v)
	isDue := func(at time.Time) bool {
		due, err := svc.DueInstalls(f.ctx, testDB, at, 500)
		require.NoError(t, err)
		for _, d := range due {
			if d.ID == in.ID {
				return true
			}
		}
		return false
	}
	assert.True(t, isDue(f.clk.Now()), "never followed: due")
	var run FollowRun
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, err = svc.FollowScheduled(ctx, tx, in.ID, f.clk.Now())
		return err
	}))
	assert.Equal(t, TriggerScheduled, run.Trigger)
	assert.Equal(t, 1, run.IntentsDispatched)
	assert.False(t, isDue(f.clk.Now().Add(59*time.Minute)), "the manifest's frequency is respected")
	assert.True(t, isDue(f.clk.Now().Add(time.Hour)))
}

// scriptedEngine stands in for the wasm runtime: it returns whatever stdout
// the test scripts, exactly as a strategy could have written it. Everything
// after it -- output parsing, proposal validation, the manifest, intents,
// routing, Risk V2, dispatch -- is the production path.
type scriptedEngine struct {
	mu     sync.Mutex
	stdout []byte
}

func (e *scriptedEngine) set(out []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stdout = out
}

func (e *scriptedEngine) Run(_ context.Context, req sandboxrt.RunRequest) (sandboxrt.RawRun, error) {
	input, err := req.Input.Canonical()
	if err != nil {
		return sandboxrt.RawRun{}, err
	}
	digest := sha256.Sum256(input)
	e.mu.Lock()
	defer e.mu.Unlock()
	return sandboxrt.RawRun{Termination: sandboxrt.TerminationOK, ArtifactVerified: true, Stdout: append([]byte(nil), e.stdout...),
		ArtifactDigest: req.Artifact.Digest, InputDigest: digest[:], Input: input}, nil
}

// TestIntegration_Prop_UntrustedOutputOnlyBecomesRiskCheckedIntents: whatever
// a strategy writes, the only effects are econ intents that then face Risk
// V2. Every call into execution is for a SERVICE intent of a proposable kind
// that the kernel ALLOWED (and the decision is recorded); nothing is posted
// to the ledger; the only economic effects are the marketplace's own
// dispatches, one per dispatched intent; and an output that breaks the
// protocol yields no intent at all.
func TestIntegration_Prop_UntrustedOutputOnlyBecomesRiskCheckedIntents(t *testing.T) {
	f := newFixture(t)
	engine := &scriptedEngine{}
	svc := f.newService(sandboxrt.NewService(f.clk, f.auth, engine, f.aw))
	_, v := f.approved("momentum")
	in := f.install(v)
	allKinds := append(econ.AllIntentKinds(), "SIGN_TRANSACTION", "EXPORT_KEY")
	var dispatched, rejected, invalid int

	rapid.Check(t, func(rt *rapid.T) {
		var b strings.Builder
		n := rapid.IntRange(0, 6).Draw(rt, "lines")
		for i := 1; i <= n; i++ {
			seq := strconv.Itoa(i)
			switch rapid.IntRange(0, 6).Draw(rt, "shape") {
			case 0, 1:
				b.WriteString(`{"type":"intent","seq":` + seq + `,"kind":"ACQUIRE_EXPOSURE","instrument_id":"` + f.insA + `","side":"BUY","quantity":"1000000"}`)
			case 2:
				b.WriteString(`{"type":"intent","seq":` + seq + `,"kind":"` + string(rapid.SampledFrom(allKinds).Draw(rt, "kind")) +
					`","instrument_id":"` + rapid.SampledFrom([]string{f.insA, f.insB, "x"}).Draw(rt, "ins") +
					`","side":"` + rapid.SampledFrom([]string{"BUY", "SELL", "ALL"}).Draw(rt, "side") +
					`","quantity":"` + rapid.SampledFrom([]string{"1", "1000000", "90000000", "0", "-1"}).Draw(rt, "qty") + `"}`)
			case 3:
				b.WriteString(`{"type":"` + rapid.SampledFrom([]string{"sign", "transfer", "withdraw", "approve"}).Draw(rt, "type") + `","seq":` + seq + `}`)
			case 4:
				b.WriteString(`{"type":"intent","seq":` + seq + `,"kind":"ACQUIRE_EXPOSURE","instrument_id":"` + f.insA +
					`","side":"BUY","quantity":"1","` + rapid.SampledFrom([]string{"destination", "to_account", "signature"}).Draw(rt, "field") + `":"x"}`)
			case 5:
				b.WriteString(`{"type":"log","seq":` + seq + `,"event":"tick"}`)
			default:
				b.WriteString(rapid.StringN(0, 30, -1).Draw(rt, "garbage"))
			}
			b.WriteString("\n")
		}
		engine.set([]byte(b.String()))
		f.clk.Advance(time.Hour)
		journals := f.count(`SELECT count(*) FROM journal_transactions`)
		before := len(f.exec.snapshot())

		run, err := f.follow(svc, in, f.input(f.clk.Now()))
		if err != nil {
			rt.Fatalf("follow failed: %v", err)
		}
		calls := f.exec.snapshot()[before:]
		if len(calls) != run.IntentsDispatched {
			rt.Fatalf("%d executor calls for %d dispatched intents", len(calls), run.IntentsDispatched)
		}
		for _, c := range calls {
			if c.Verdict != risk.Allow || c.StatusAtCall != econ.StatusRiskApproved || c.ActorType != security.ActorService ||
				c.Mode != gates.ModePaper || c.StrategyRef != v.StrategyRef() || !sandboxrt.Proposable(c.Kind) {
				rt.Fatalf("execution saw %+v", c)
			}
			if f.count(`SELECT count(*) FROM advanced_risk_decisions WHERE intent_id = $1 AND verdict = 'ALLOW' AND decision_hash = $2`,
				c.IntentID, c.DecisionHash) != 1 {
				rt.Fatalf("intent %s reached execution without its recorded ALLOW", c.IntentID)
			}
		}
		if run.IntentsSubmitted != run.AcceptedCount || len(run.Intents) != run.IntentsSubmitted {
			rt.Fatalf("accepted %d, submitted %d", run.AcceptedCount, run.IntentsSubmitted)
		}
		for _, fi := range run.Intents {
			if !sandboxrt.Proposable(fi.Kind) {
				rt.Fatalf("an intent of kind %s exists", fi.Kind)
			}
			if fi.Outcome != OutcomeDispatched && fi.Outcome != OutcomeRiskRejected {
				rt.Fatalf("unexpected outcome %s", fi.Outcome)
			}
			effects := f.count(`SELECT count(*) FROM economic_effects WHERE intent_id = $1`, fi.IntentID)
			marketplace := f.count(`SELECT count(*) FROM economic_effects WHERE intent_id = $1 AND subsystem = 'MARKETPLACE' AND effect_kind = $2`,
				fi.IntentID, DispatchEffectKind)
			if fi.Outcome == OutcomeDispatched && (effects != 1 || marketplace != 1) {
				rt.Fatalf("dispatched intent has %d effects (%d marketplace)", effects, marketplace)
			}
			if fi.Outcome == OutcomeRiskRejected && effects != 0 {
				rt.Fatalf("a rejected intent has %d effects", effects)
			}
		}
		if run.Termination == sandboxrt.TerminationInvalidOutput && len(run.Intents) > 0 {
			rt.Fatal("an output that broke the protocol produced intents")
		}
		if f.count(`SELECT count(*) FROM journal_transactions`) != journals {
			rt.Fatal("strategy output reached the ledger")
		}
		dispatched += run.IntentsDispatched
		rejected += run.IntentsRiskRejected
		if run.Termination == sandboxrt.TerminationInvalidOutput {
			invalid++
		}
	})
	// The generator reached every branch the property speaks about.
	assert.Positive(t, dispatched, "some outputs were dispatched")
	assert.Positive(t, rejected, "some were rejected by the risk kernel")
	assert.Positive(t, invalid, "some broke the protocol")
}
