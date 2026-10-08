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
	assert.Equal(t, "10000.00", in.PaperCapital, "every paper book starts with the configured paper capital")
	journalsBefore := f.count(`SELECT count(*) FROM journal_transactions`)

	run, err := f.follow(f.svc, in)
	require.NoError(t, err)
	assert.Equal(t, sandboxrt.TerminationOK, run.Termination)
	assert.Equal(t, 1, run.ProposalsCount)
	assert.Equal(t, 1, run.AcceptedCount)
	assert.Zero(t, run.ViolationsCount)
	require.Len(t, run.Intents, 1)
	fi := run.Intents[0]
	assert.Equal(t, OutcomeDispatched, fi.Outcome)
	assert.Equal(t, "150.25000000", fi.Notional, "one whole unit at the reference price")
	assert.Equal(t, "150.25", fi.ReferencePrice)
	assert.Equal(t, "150.25000000", run.Turnover)
	assert.Equal(t, "10000.00", run.PaperCash, "the book's cash when the run started")
	assert.Equal(t, "10000.00", run.Equity, "an empty book is worth its cash")
	require.Len(t, run.Marks, 2, "both instruments had a fresh reference price")
	assert.Equal(t, f.insA, run.Marks[0].InstrumentID)
	assert.Equal(t, "150.25", run.Marks[0].Mark)
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
	assert.Equal(t, "150.25", terms.ReferencePrice)

	var verdict, sandboxOriginated, available string
	var violations int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT verdict, input->'intent'->>'sandbox_originated', (input->'sandbox'->>'violations')::int,
		input->'balance'->>'available' FROM advanced_risk_decisions WHERE intent_id = $1`, fi.IntentID).Scan(&verdict, &sandboxOriginated,
		&violations, &available))
	assert.Equal(t, "ALLOW", verdict)
	assert.Equal(t, "true", sandboxOriginated)
	assert.Zero(t, violations)
	assert.Equal(t, "10000.00", available, "a buy is judged against what the paper book holds")

	var status, subsystem, kind, evidence string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status, subsystem, effect_kind, coalesce(evidence_ref,'') FROM economic_effects WHERE intent_id = $1`,
		fi.IntentID).Scan(&status, &subsystem, &kind, &evidence))
	assert.Equal(t, "APPLIED", status)
	assert.Equal(t, "MARKETPLACE", subsystem)
	assert.Equal(t, DispatchEffectKind, kind)
	assert.NotEmpty(t, evidence)
	assert.Equal(t, journalsBefore, f.count(`SELECT count(*) FROM journal_transactions`), "the marketplace itself never posts")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM marketplace_paper_fills WHERE install_id = $1 AND intent_id = $2 AND notional = 150.25`,
		in.ID, fi.IntentID), "the applied dispatch is a fill of the paper book at its reference price")
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM marketplace_follow_marks WHERE follow_run_id = $1`, run.ID))

	// The same instant again is the same run: nothing runs or dispatches twice.
	again, err := f.follow(f.svc, in)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, run.ID, again.ID)
	assert.Equal(t, run.Marks, again.Marks)
	assert.Len(t, f.exec.snapshot(), 1)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM marketplace_follow_runs WHERE install_id = $1`, in.ID))

	// The manifest's execution frequency holds.
	f.clk.Advance(30 * time.Minute)
	_, err = f.follow(f.svc, in)
	assert.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))

	// An hour on, the book holds the bought unit; its reference price is stale
	// (the last observation is an hour old), so the book cannot be valued and
	// the follow is refused rather than valued at an old price.
	f.clk.Advance(30 * time.Minute)
	_, err = f.follow(f.svc, in)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))
	f.mark(f.insA, f.clk.Now(), "151.00")
	second, err := f.follow(f.svc, in)
	require.NoError(t, err)
	assert.NotEqual(t, run.ID, second.ID)
	assert.Equal(t, "9849.75000000", second.PaperCash)
	assert.Equal(t, "10000.75000000", second.Equity, "9849.75 cash + the unit at the fresh 151.00")
	require.Len(t, second.Marks, 1, "the other instrument has no fresh price and is not in the run")
	assert.Len(t, f.exec.snapshot(), 2)

	// A scheduler that fell behind does not follow the past.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.FollowScheduled(ctx, tx, in.ID, f.clk.Now().Add(-10*time.Minute))
		return err
	})
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))
}

func TestIntegration_ProposalsBeyondManifestDroppedAndCounted(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("greedy")
	in := f.install(v)
	run, err := f.follow(f.svc, in)
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
	assert.Zero(t, f.count(`SELECT count(*) FROM marketplace_paper_fills WHERE install_id = $1`, in.ID), "nothing rejected fills the book")
}

func TestIntegration_ConcurrentFollowsOfOneInstantDispatchOnce(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
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
			run, err := f.follow(f.svc, in)
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
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM marketplace_paper_fills WHERE install_id = $1`, in.ID), "and exactly one fill")
}

func TestIntegration_MissingRiskSectionsFailClosed(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	f.sections.omit = true
	run, err := f.follow(f.svc, in)
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
	failed, err := f.follow(f.svc, in)
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
	f.trend()
	f.exec.mode = "unknown"
	unknown, err := f.follow(f.svc, in)
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
	assert.Zero(t, f.count(`SELECT count(*) FROM marketplace_paper_fills WHERE install_id = $1`, in.ID),
		"neither a refused nor an unknown dispatch is a fill")

	calls := len(f.exec.snapshot())
	replay, err := f.follow(f.svc, in)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, calls, len(f.exec.snapshot()), "an unknown outcome is never retried")
}

func TestIntegration_ADispatchWithoutAnAllowDecisionIsRefusedByTheDatabase(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("greedy")
	in := f.install(v)
	run, err := f.follow(f.svc, in)
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
			run, followErr = f.svc.PaperFollow(ctx, tx, FollowRequest{InstallID: in.ID, Trigger: TriggerExplicit, Actor: f.followerActor})
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

func (f *fixture) simulation(v Version, dataset string, snaps []sandboxrt.Input) (Evaluation, error) {
	var ev Evaluation
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ev, err = f.svc.Simulate(ctx, tx, SimulationRequest{VersionID: v.ID, AccountID: f.creatorAccount, DatasetVersion: dataset,
			Snapshots: snaps, Actor: f.creator})
		return err
	})
	return ev, err
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
	first, err := f.simulation(v, "closes-2026-09", snaps)
	require.NoError(t, err)
	second, err := f.simulation(v, "closes-2026-09", snaps)
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)
	assert.Equal(t, f.creatorAccount, first.AccountID)
	assert.Equal(t, first.SnapshotsDigest, second.SnapshotsDigest)
	assert.Equal(t, first.ResultsDigest, second.ResultsDigest, "same artifact, same snapshots, same results")
	assert.Equal(t, 3, first.RunsOK)
	assert.Equal(t, 3, first.ProposalsCount)
	assert.Equal(t, 3, first.AcceptedCount)
	assert.Zero(t, f.count(`SELECT count(*) FROM economic_intents WHERE strategy_ref = $1`, v.StrategyRef()), "a simulation proposes nothing")
	assert.Equal(t, 3, f.count(`SELECT count(*) FROM marketplace_evaluation_runs WHERE evaluation_id = $1`, first.ID))
	assert.Equal(t, 6, f.count(`SELECT count(*) FROM sandbox_usage WHERE account_id = $1 AND purpose = 'SIMULATION'`, f.creatorAccount),
		"every run is charged to the creator's account")

	p, err := f.svc.Provenance(f.ctx, testDB, v.ID)
	require.NoError(t, err)
	assert.Equal(t, "closes-2026-09", p.DatasetVersion)
	require.NotNil(t, p.EvaluationID)

	_, draft := f.draft("momentum")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Simulate(ctx, tx, SimulationRequest{VersionID: draft.ID, AccountID: f.creatorAccount, DatasetVersion: "x",
			Snapshots: snaps, Actor: f.creator})
		return err
	})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err), "a draft has nothing built to simulate")
}

// TestIntegration_ASimulationIsBoundedAndChargedToTheCreatorsAccount is
// review-2 F13 at the marketplace: a simulation is the listing creator's, by
// the creator's account owner, charged to that account, and refused whole --
// before any run starts -- when it asks for more than a call may have or the
// account's window has left. Never a thousand runs in one transaction.
func TestIntegration_ASimulationIsBoundedAndChargedToTheCreatorsAccount(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	snap := f.input(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	snaps := func(n int) []sandboxrt.Input {
		out := make([]sandboxrt.Input, n)
		for i := range out {
			out[i] = snap
			out[i].AsOf = snap.AsOf.Add(time.Duration(i) * time.Hour)
		}
		return out
	}
	runsBefore := f.count(`SELECT count(*) FROM sandbox_runs`)
	_, err := f.simulation(v, "ds", snaps(1000))
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "never a thousand runs in one call")
	_, err = f.simulation(v, "ds", snaps(sandboxrt.DefaultBudget().MaxRunsPerCall+1))
	require.Error(t, err)
	assert.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	e, _ := errs.As(err)
	assert.Equal(t, sandboxrt.BudgetCallRuns, e.Fields["reason"])
	assert.Equal(t, runsBefore, f.count(`SELECT count(*) FROM sandbox_runs`), "a refused call runs nothing")

	// Someone else's account, or someone else for the creator's account.
	stranger, strangerActor := f.account()
	for _, req := range []SimulationRequest{
		{VersionID: v.ID, AccountID: stranger, DatasetVersion: "ds", Snapshots: snaps(1), Actor: strangerActor},
		{VersionID: v.ID, AccountID: f.creatorAccount, DatasetVersion: "ds", Snapshots: snaps(1), Actor: strangerActor},
	} {
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.Simulate(ctx, tx, req)
			return err
		})
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	}

	// A tight budget: the window's room is the whole story.
	tight := sandboxrt.Budget{Version: "itest", Window: time.Hour, MaxRunsPerWindow: 4, MaxCPUMSPerWindow: 600_000,
		MaxWallMSPerWindow: 600_000, MaxRunsPerCall: 3, MaxCPUMSPerCall: 40_000}
	sb, err := f.sandbox.WithBudget(tight)
	require.NoError(t, err)
	f.svc = f.newService(sb)
	_, err = f.simulation(v, "ds", snaps(3))
	require.NoError(t, err)
	_, err = f.simulation(v, "ds", snaps(2))
	require.Error(t, err)
	e, _ = errs.As(err)
	assert.Equal(t, sandboxrt.BudgetWindowRuns, e.Fields["reason"], "three of four used: two more do not fit")
	_, err = f.simulation(v, "ds", snaps(1))
	require.NoError(t, err)

	// Around Go: an evaluation by someone who does not own the account it is
	// charged to.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_evaluations (id, version_id, kind, dataset_version, snapshots_count, snapshots_digest,
			results_digest, runs_ok, proposals_count, accepted_count, violations_count, evidence_ref, evidence_sha256, actor_type, actor_id,
			account_id) VALUES ($1,$2,'SIMULATION','x',1,$3,$3,0,0,0,0,'e',$3,'USER',$4,$5)`, uuid.New(), v.ID, make([]byte, 32), strangerActor.ID,
			f.creatorAccount)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK012", db.SQLState(err))
}

// TestIntegration_PaperPerformanceComesFromTheBooks: three follows an hour
// apart at reference prices 150.25, 160.00 and 140.00. Momentum buys one
// unit each time; each run's equity is the paper book -- 10 000.00 of paper
// capital moved by the earlier fills -- at that run's reference price, and the
// performance is computed from those equities alone.
func TestIntegration_PaperPerformanceComesFromTheBooks(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	var equities, cash []string
	for i, mark := range []string{"150.25", "160.00", "140.00"} {
		if i > 0 {
			f.clk.Advance(time.Hour)
			f.mark(f.insA, f.clk.Now(), mark)
		}
		run, err := f.follow(f.svc, in)
		require.NoError(t, err)
		require.Equal(t, 1, run.IntentsDispatched, "run %d", i+1)
		equities, cash = append(equities, run.Equity), append(cash, run.PaperCash)
	}
	assert.Equal(t, []string{"10000.00", "9849.75000000", "9689.75000000"}, cash)
	assert.Equal(t, []string{"10000.00", "10009.75000000", "9969.75000000"}, equities)

	var snap PerformanceSnapshot
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		snap, err = f.svc.RecordPerformance(ctx, tx, in.ID, f.followerActor)
		return err
	}))
	assert.Equal(t, 3, snap.Points)
	assert.Equal(t, int64(-30), snap.ReturnBPS, "-30.25 / 10000, half-even")
	assert.Equal(t, int64(40), snap.MaxDrawdownBPS, "40 / 10009.75")
	assert.Equal(t, int64(24), snap.VolatilityBPS, "|9.75 - (-39.961039)| / 2, rounded down")
	assert.Equal(t, int64(451), snap.TurnoverBPS, "450.25 / mean(9993.1667)")
	var stored int64
	var digest []byte
	var fromBook bool
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT return_bps, history_digest, from_paper_book FROM marketplace_performance_snapshots
		WHERE install_id = $1`, in.ID).Scan(&stored, &digest, &fromBook))
	assert.Equal(t, int64(-30), stored)
	assert.Equal(t, snap.HistoryDigest, digest)
	assert.True(t, fromBook)

	// An operator may record it too; a stranger may not.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.RecordPerformance(ctx, tx, in.ID, f.operator)
		return err
	}))
	_, stranger := f.account()
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.RecordPerformance(ctx, tx, in.ID, stranger)
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

// TestIntegration_ThePaperBookNeverSpendsMoreThanItHolds: a buy is judged by
// Risk V2 against the paper book's cash (rounded down to the cent) and the
// notional (rounded up), so the book's cash never goes below zero.
func TestIntegration_ThePaperBookNeverSpendsMoreThanItHolds(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(f.sandbox)
	cfg.PaperCapital = money.USDFromMinor(20_000) // 200.00
	svc, err := NewService(cfg)
	require.NoError(t, err)
	f.svc = svc
	_, v := f.approved("momentum")
	in := f.install(v)
	assert.Equal(t, "200.00", in.PaperCapital)
	first, err := f.follow(f.svc, in)
	require.NoError(t, err)
	require.Equal(t, 1, first.IntentsDispatched)
	f.clk.Advance(time.Hour)
	f.mark(f.insA, f.clk.Now(), "151.00")
	second, err := f.follow(f.svc, in)
	require.NoError(t, err)
	assert.Equal(t, "49.75000000", second.PaperCash)
	require.Len(t, second.Intents, 1)
	assert.Equal(t, OutcomeRiskRejected, second.Intents[0].Outcome)
	assert.Contains(t, second.Intents[0].ReasonCodes, risk.ReasonInsufficientBalance, "151.00 is more than the 49.75 the book holds")
	assert.Len(t, f.exec.snapshot(), 1)
}

// TestIntegration_AStrangerCannotActForSomeoneElsesAccount is review-2 F10's
// actor binding: installing, following and recording are the account
// owner's (an operator may remove or record; the scheduler follows on its
// schedule), in Go and in SQL.
func TestIntegration_AStrangerCannotActForSomeoneElsesAccount(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	_, stranger := f.account()
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.InstallVersion(ctx, tx, InstallRequest{VersionID: v.ID, AccountID: f.follower, Mode: gates.ModePaper, Actor: stranger})
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "a stranger cannot install on someone else's account")
	in := f.install(v)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.PaperFollow(ctx, tx, FollowRequest{InstallID: in.ID, Trigger: TriggerExplicit, Actor: stranger})
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "nor follow it")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.RemoveInstall(ctx, tx, in.ID, stranger, "mine now")
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "nor remove it")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.CreateListing(ctx, tx, ListingRequest{CreatorAccountID: f.follower, Creator: stranger, Title: "not mine",
			Version: f.versionRequest("momentum")})
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "nor list on it")

	// The scheduler follows a due install; the run is the SYSTEM's.
	var run FollowRun
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, err = f.svc.FollowScheduled(ctx, tx, in.ID, f.clk.Now())
		return err
	}))
	assert.Equal(t, TriggerScheduled, run.Trigger)

	// Around Go: the database refuses a non-owner's install, listing and
	// follow run.
	for name, sql := range map[string]string{
		"install": `INSERT INTO marketplace_installs (id, account_id, version_id, mode, status, installed_by_actor_type, installed_by_actor_id)
			VALUES ($1,$2,$3,'PAPER','ACTIVE','USER',$4)`,
		"listing": `INSERT INTO marketplace_listings (id, creator_account_id, creator_actor_type, creator_actor_id, title)
			SELECT $1, $2, 'USER', $4, 'forged' WHERE $3::uuid IS NOT NULL`,
	} {
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			// A REMOVED install leaves room for another ACTIVE one.
			if _, err := tx.Exec(ctx, `UPDATE marketplace_installs SET status = 'REMOVED' WHERE id = $1`, in.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO marketplace_install_transitions (id, install_id, from_status, to_status, actor_type, actor_id, reason)
				VALUES ($1,$2,'ACTIVE','REMOVED','SYSTEM','itest','make room')`, uuid.New(), in.ID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, sql, uuid.New(), f.follower, v.ID, stranger.ID)
			return err
		})
		require.Error(t, err, name)
		assert.Equal(t, "MK012", db.SQLState(err), name)
	}
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_follow_runs (id, install_id, version_id, account_id, sandbox_run_id, trigger, as_of,
			input_digest, run_digest, proposals_count, accepted_count, violations_count, intents_submitted, intents_dispatched,
			intents_risk_rejected, intents_failed, equity, turnover, evidence_ref, evidence_sha256, actor_type, actor_id, paper_cash, mark_max_age_ms)
			SELECT $1, install_id, version_id, account_id, sandbox_run_id, 'EXPLICIT', as_of + interval '3 hours', input_digest, run_digest,
			0, 0, 0, 0, 0, 0, 0, equity, 0, evidence_ref, evidence_sha256, 'USER', $3, paper_cash, mark_max_age_ms
			FROM marketplace_follow_runs WHERE id = $2`, uuid.New(), run.ID, stranger.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK012", db.SQLState(err), "a stranger's follow run")
}

// TestIntegration_PaperPerformanceNeverComesFromACallersFigures is review-2
// F10's book binding, around Go: a follow run whose equity, cash or turnover
// is not its paper book at its recorded marks, a mark that is not a fresh
// recorded observation, a fill that is not an applied dispatch at its run's
// mark, and a performance snapshot over anything but the book-valued runs are
// all refused by the database.
func TestIntegration_PaperPerformanceNeverComesFromACallersFigures(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	run, err := f.follow(f.svc, in)
	require.NoError(t, err)
	require.Equal(t, 1, run.IntentsDispatched)

	// A second run, written by hand with a fabricated equity of 1 000 000.
	copyRun := func(equity, cash string) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO marketplace_follow_runs (id, install_id, version_id, account_id, sandbox_run_id, trigger, as_of,
				input_digest, run_digest, proposals_count, accepted_count, violations_count, intents_submitted, intents_dispatched,
				intents_risk_rejected, intents_failed, equity, turnover, evidence_ref, evidence_sha256, actor_type, actor_id, paper_cash, mark_max_age_ms)
				SELECT $1, install_id, version_id, account_id, sandbox_run_id, 'EXPLICIT', as_of + interval '1 hour', input_digest, run_digest,
				0, 0, 0, 0, 0, 0, 0, $3::numeric, 0, evidence_ref, evidence_sha256, actor_type, actor_id, $4::numeric, mark_max_age_ms
				FROM marketplace_follow_runs WHERE id = $2`, uuid.New(), run.ID, equity, cash)
			return err
		})
	}
	err = copyRun("1000000.00", "9849.75")
	require.Error(t, err)
	assert.Equal(t, "MK013", db.SQLState(err), "a position with no recorded mark, and an equity that is not the book's")
	err = copyRun("1000000.00", "1000000.00")
	require.Error(t, err)
	assert.Equal(t, "MK013", db.SQLState(err), "a cash that is not the capital moved by the fills")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_follow_runs (id, install_id, version_id, account_id, sandbox_run_id, trigger, as_of,
			input_digest, run_digest, proposals_count, accepted_count, violations_count, intents_submitted, intents_dispatched,
			intents_risk_rejected, intents_failed, equity, turnover, evidence_ref, evidence_sha256, actor_type, actor_id)
			SELECT $1, install_id, version_id, account_id, sandbox_run_id, 'EXPLICIT', as_of + interval '2 hours', input_digest, run_digest,
			0, 0, 0, 0, 0, 0, 0, 1000000, 0, evidence_ref, evidence_sha256, actor_type, actor_id
			FROM marketplace_follow_runs WHERE id = $2`, uuid.New(), run.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK013", db.SQLState(err), "a run that is not valued from its paper book at all")

	// A mark that is not the price store's observation of the pair.
	var priceID uuid.UUID
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT price_id FROM marketplace_follow_marks WHERE follow_run_id = $1 AND instrument_id = $2`,
		run.ID, f.insA).Scan(&priceID))
	markErr := func(instrument, mark string, price uuid.UUID) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO marketplace_follow_marks (id, follow_run_id, instrument_id, price_id, mark, base_decimals, observed_at)
				SELECT $1, $2, $3, id, $4::numeric, 6, observed_at FROM asset_prices WHERE id = $5`, uuid.New(), run.ID, instrument, mark, price)
			return err
		})
	}
	err = markErr(f.insB, "150.25", priceID)
	require.Error(t, err)
	assert.Equal(t, "MK013", db.SQLState(err), "another pair's observation")

	// A fill nobody dispatched, or at a price other than the run's mark.
	var rejectedIntent econ.IntentID
	{
		_, greedy := f.approved("greedy")
		gin := f.install(greedy)
		grun, err := f.follow(f.svc, gin)
		require.NoError(t, err)
		rejectedIntent = grun.Intents[0].IntentID
		err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO marketplace_paper_fills (id, install_id, follow_run_id, intent_id, instrument_id, side, quantity, mark, notional)
				VALUES ($1,$2,$3,$4,$5,'BUY',1000000,150.25,150.25)`, uuid.New(), gin.ID, grun.ID, rejectedIntent, f.insA)
			return err
		})
		require.Error(t, err)
		assert.Equal(t, "MK013", db.SQLState(err), "a fill of a risk-rejected intent")
	}

	// A performance snapshot that covers a run the book did not value.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_performance_snapshots (id, install_id, version_id, follow_runs_count, first_as_of,
			last_as_of, history_digest, return_bps, max_drawdown_bps, volatility_bps, turnover_bps, computed_at, from_paper_book)
			VALUES ($1,$2,$3,2,$4,$4,$5,990000,0,0,0,now(),true)`, uuid.New(), in.ID, v.ID, run.AsOf, make([]byte, 32))
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK013", db.SQLState(err), "a snapshot over runs that are not the book's")

	// A follow intent whose notional is not its quantity at the reference mark.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_follow_intents (id, follow_run_id, proposal_seq, intent_id, outcome, risk_decision_hash, notional)
			SELECT $1, $2, 98, intent_id, 'RISK_REJECTED', 'h', 0.000001 FROM marketplace_follow_intents WHERE follow_run_id = $2 LIMIT 1`,
			uuid.New(), run.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK014", db.SQLState(err), "a notional at the strategy's own price")
}

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
	run, err := f.follow(f.svc, in)
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

	again, err := f.follow(f.svc, in)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, before+1, paperFills(), "a replayed follow posts nothing again")
	assert.Equal(t, "1000000", balance(ledger.CustomerAccount(f.follower, ledger.CodeWallet, base)))
	// A deposit into the account is not the strategy's performance: the
	// paper book is the install's own.
	f.clk.Advance(time.Hour)
	f.mark(f.insA, f.clk.Now(), "150.25")
	next, err := f.follow(f.svc, in)
	require.NoError(t, err)
	assert.Equal(t, "10000.00000000", next.Equity, "the book, not the account's 10 000 of wallet and its deposit")
}

func TestIntegration_ScheduledFollowsRunWhenDue(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	in := f.install(v)
	isDue := func(at time.Time) bool {
		due, err := f.svc.DueInstalls(f.ctx, testDB, at, 500)
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
		run, err = f.svc.FollowScheduled(ctx, tx, in.ID, f.clk.Now())
		return err
	}))
	assert.Equal(t, TriggerScheduled, run.Trigger)
	assert.Equal(t, 1, run.IntentsDispatched)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM marketplace_follow_runs WHERE id = $1 AND actor_type = 'SYSTEM' AND actor_id = $2`,
		run.ID, SchedulerActor().ID), "a scheduled run is the scheduler's")
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

// TestIntegration_AFollowValuesProposalsAtTheReferencePriceNeverTheStrategysLimit
// is review-2 F11 in a follow: a strategy that asks for a million whole units
// at a limit of 0.000001 is valued at the reference price the service read
// and dropped by the manifest's cap; a proposal for an instrument with no
// fresh reference price is not in the run at all. Both are recorded.
func TestIntegration_AFollowValuesProposalsAtTheReferencePriceNeverTheStrategysLimit(t *testing.T) {
	f := newFixture(t)
	engine := &scriptedEngine{}
	svc := f.newService(sandboxrt.NewService(f.clk, f.auth, engine, f.aw, testBudget))
	_, v := f.approved("momentum")
	in := f.install(v)
	// The second instrument's price is an hour old by the time of the run.
	f.clk.Advance(time.Hour)
	f.mark(f.insA, f.clk.Now(), "150.25")
	engine.set([]byte(`{"type":"intent","seq":1,"kind":"ACQUIRE_EXPOSURE","instrument_id":"` + f.insA +
		`","side":"BUY","quantity":"1000000000000","limit_price":"0.000001"}` + "\n" +
		`{"type":"intent","seq":2,"kind":"ACQUIRE_EXPOSURE","instrument_id":"` + f.insB + `","side":"BUY","quantity":"1"}` + "\n"))
	run, err := f.follow(svc, in)
	require.NoError(t, err)
	assert.Equal(t, sandboxrt.TerminationOK, run.Termination)
	assert.Zero(t, run.AcceptedCount, "nothing a limit price made small, nothing without a fresh price")
	assert.Empty(t, run.Intents)
	violations, err := f.sandbox.RunViolations(f.ctx, testDB, run.SandboxRunID)
	require.NoError(t, err)
	require.Len(t, violations, 2)
	assert.Equal(t, sandboxrt.ViolationManifestNotional, violations[0].Code)
	assert.Contains(t, violations[0].Detail, "reference price 150.25")
	assert.Equal(t, sandboxrt.ViolationInstrumentNotInSnapshot, violations[1].Code)
	require.Len(t, run.Marks, 1, "the stale instrument was not shown to the strategy")
	assert.Equal(t, f.insA, run.Marks[0].InstrumentID)
}

// TestIntegration_Prop_UntrustedOutputOnlyBecomesRiskCheckedIntents: whatever
// a strategy writes, the only effects are econ intents that then face Risk
// V2. Every call into execution is for a SERVICE intent of a proposable kind
// that the kernel ALLOWED (and the decision is recorded); nothing is posted
// to the ledger; the only economic effects are the marketplace's own
// dispatches, one per dispatched intent; the paper book's cash never goes
// below zero; and an output that breaks the protocol yields no intent at all.
func TestIntegration_Prop_UntrustedOutputOnlyBecomesRiskCheckedIntents(t *testing.T) {
	f := newFixture(t)
	engine := &scriptedEngine{}
	svc := f.newService(sandboxrt.NewService(f.clk, f.auth, engine, f.aw, testBudget))
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
		f.mark(f.insA, f.clk.Now(), "150.25")
		f.mark(f.insB, f.clk.Now(), "40.10")
		journals := f.count(`SELECT count(*) FROM journal_transactions`)
		before := len(f.exec.snapshot())

		run, err := f.follow(svc, in)
		if err != nil {
			rt.Fatalf("follow failed: %v", err)
		}
		if strings.HasPrefix(run.PaperCash, "-") {
			rt.Fatalf("the paper book's cash went negative: %s", run.PaperCash)
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
