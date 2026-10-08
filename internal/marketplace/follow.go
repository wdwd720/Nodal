package marketplace

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
	"github.com/nodal/controlplane/internal/settlement"
)

// DispatchEffectKind is the economic effect kind of handing one intent to
// paper execution.
const DispatchEffectKind = "PAPER_DISPATCH"

// FollowRequest is one explicit paper-follow run of an install, by the
// owner of the install's account.
type FollowRequest struct {
	InstallID InstallID
	// Input must be left empty. A follow's input -- the market it runs on
	// and the portfolio it manages -- is built by the service itself from the
	// install's paper book and the reference prices it reads (review-2 F10);
	// a caller's snapshot is refused, never ignored.
	Input sandboxrt.Input
	// Trigger must be EXPLICIT: scheduled follows run through
	// FollowScheduled, as the SYSTEM scheduler.
	Trigger       FollowTrigger
	Actor         econ.Actor
	CorrelationID string
}

// FollowIntent is what became of one proposal of a follow run.
type FollowIntent struct {
	ProposalSeq  int             `json:"proposal_seq"`
	IntentID     econ.IntentID   `json:"intent_id"`
	Kind         econ.IntentKind `json:"kind"`
	Outcome      FollowOutcome   `json:"outcome"`
	DecisionHash string          `json:"decision_hash"`
	ReasonCodes  []string        `json:"reason_codes"`
	EffectID     *econ.EffectID  `json:"effect_id"`
	// Notional is the proposal's quantity at ReferencePrice, the price the
	// service read itself -- what the manifest's cap and Risk V2 judged.
	Notional       string `json:"notional"`
	ReferencePrice string `json:"reference_price"`
	ExternalRef    string `json:"external_ref"`
}

// FollowRun is a recorded paper-follow run.
type FollowRun struct {
	ID                  FollowRunID
	InstallID           InstallID
	VersionID           VersionID
	AccountID           accounts.AccountID
	SandboxRunID        sandboxrt.RunID
	Trigger             FollowTrigger
	AsOf                time.Time
	InputDigest         []byte
	RunDigest           []byte
	Termination         sandboxrt.Termination
	ProposalsCount      int
	AcceptedCount       int
	ViolationsCount     int
	IntentsSubmitted    int
	IntentsDispatched   int
	IntentsRiskRejected int
	IntentsFailed       int
	// PaperCash is the install's paper book's cash when the run started;
	// Equity is that cash plus every position at its reference price;
	// Turnover is the exact notional the run dispatched. All in simulated
	// dollars, all from the books. PaperCash is empty for a run recorded
	// before review-2 F10, whose equity came from a caller's snapshot.
	PaperCash      string
	Equity         string
	Turnover       string
	Marks          []FollowMark
	EvidenceRef    string
	EvidenceSHA256 []byte
	Intents        []FollowIntent
	// Replayed is true when the install was already followed at this as-of
	// time: nothing was run or dispatched again.
	Replayed  bool
	CreatedAt time.Time
}

// BookValued reports whether the run's equity was computed from the paper
// book and recorded reference prices, as every run since review-2 F10 is.
func (r FollowRun) BookValued() bool { return r.PaperCash != "" }

// SchedulerActor is the actor of every scheduled follow.
func SchedulerActor() econ.Actor { return econ.SystemActor("marketplace.scheduler") }

// PaperFollow runs an installed version once, now, for the owner of the
// install's account. The service builds the run's input itself -- the
// install's paper book and the reference prices it reads -- and puts every
// proposal the manifest permits through the same path as any other intent:
// econ intent (SERVICE, PAPER, STRATEGY_MARKETPLACE) -> settlement compiler
// -> Risk V2 with the strategy, sandbox and paper-book balance sections ->
// exactly-once dispatch to the IntentExecutor, only on ALLOW. A dispatch the
// executor applied is a fill of the paper book at its reference price. It
// records the follow run, the reference prices, what became of each intent,
// the fills, the exact cash, equity and turnover, and an evidence document,
// in the caller's transaction.
//
// One as-of time of one install is one follow run: a second call at the same
// time returns the recorded run and does nothing.
func (s *Service) PaperFollow(ctx context.Context, tx pgx.Tx, req FollowRequest) (FollowRun, error) {
	if err := requireActor(req.Actor, TransitionActorTypes(), "follow a strategy"); err != nil {
		return FollowRun{}, err
	}
	if req.Trigger != TriggerExplicit {
		return FollowRun{}, errs.Newf(errs.CodeValidationFailed, "marketplace: PaperFollow is an EXPLICIT follow (a %q follow runs through FollowScheduled)", req.Trigger)
	}
	if !inputIsEmpty(req.Input) {
		return FollowRun{}, errs.New(errs.CodeValidationFailed,
			"marketplace: a follow runs on the input the service builds from the install's paper book and the reference prices it reads; a caller's snapshot is refused")
	}
	return s.follow(ctx, tx, req.InstallID, TriggerExplicit, req.Actor, s.now(), req.CorrelationID)
}

// FollowScheduled runs a due install at asOf, as the SYSTEM scheduler, on the
// input the service builds itself.
func (s *Service) FollowScheduled(ctx context.Context, tx pgx.Tx, iid InstallID, asOf time.Time) (FollowRun, error) {
	return s.follow(ctx, tx, iid, TriggerScheduled, SchedulerActor(), asOf.UTC(), "")
}

func inputIsEmpty(in sandboxrt.Input) bool {
	return in.Schema == "" && in.AsOf.IsZero() && len(in.Instruments) == 0 && in.Portfolio.Cash == "" &&
		len(in.Portfolio.Positions) == 0 && len(in.Config) == 0
}

func (s *Service) follow(ctx context.Context, tx pgx.Tx, iid InstallID, trigger FollowTrigger, actor econ.Actor, asOf time.Time,
	correlationID string) (FollowRun, error) {
	// The install row lock serializes every follow of one install.
	install, err := s.loadInstall(ctx, tx, iid, true)
	if err != nil {
		return FollowRun{}, err
	}
	if trigger == TriggerExplicit {
		if err := requireOwner(ctx, tx, actor, install.AccountID, "follow a strategy"); err != nil {
			return FollowRun{}, err
		}
	}
	if install.Status != InstallActive || install.Mode != gates.ModePaper {
		return FollowRun{}, errs.Newf(errs.CodeForbidden, "marketplace: install is %s/%s", install.Status, install.Mode)
	}
	v, err := s.Version(ctx, tx, install.VersionID)
	if err != nil {
		return FollowRun{}, err
	}
	if !v.Followable() {
		return FollowRun{}, errs.Newf(errs.CodeForbidden, "marketplace: a %s/%s version cannot be followed", v.Status, v.Moderation).
			WithField("version_id", v.ID.String())
	}
	if existing, found, err := s.followAt(ctx, tx, install.ID, asOf); err != nil {
		return FollowRun{}, err
	} else if found {
		existing.Replayed = true
		return existing, nil
	}
	now := s.now()
	if asOf.After(now) || now.Sub(asOf) > s.cfg.MaxSnapshotAge {
		return FollowRun{}, errs.New(errs.CodeStaleMarketData, "marketplace: a follow's as-of time is stale or in the future").
			WithField("as_of", asOf).WithField("now", now)
	}
	var last *time.Time
	if err := tx.QueryRow(ctx, `SELECT max(as_of) FROM marketplace_follow_runs WHERE install_id = $1`, install.ID).Scan(&last); err != nil {
		return FollowRun{}, dbErr("last follow", err)
	}
	if last != nil {
		next := last.UTC().Add(time.Duration(v.Manifest.MinIntervalSeconds) * time.Second)
		if asOf.Before(next) {
			return FollowRun{}, errs.New(errs.CodeRateLimited, "marketplace: the manifest's execution frequency does not allow another run yet").
				WithField("next_allowed_at", next)
		}
	}
	if err := s.authorize(ctx, tx, gates.ModePaper); err != nil {
		return FollowRun{}, err
	}
	val, ok, err := s.ValidationOf(ctx, tx, v.ID)
	if err != nil {
		return FollowRun{}, err
	}
	if !ok || val.Outcome != ValidationPassed {
		return FollowRun{}, errs.New(errs.CodeInternal, "marketplace: an approved version has no passed validation")
	}

	// The run's world, read by the service itself.
	book, err := s.loadBook(ctx, tx, install.ID)
	if err != nil {
		return FollowRun{}, err
	}
	refs, err := s.references(ctx, tx, universe(v, book), asOf)
	if err != nil {
		return FollowRun{}, err
	}
	for _, held := range book.held() {
		if _, ok := refs[held]; !ok {
			return FollowRun{}, errs.New(errs.CodeStaleMarketData,
				"marketplace: the paper book holds an instrument with no fresh reference price, so it cannot be valued").
				WithField("instrument_id", held).WithField("as_of", asOf)
		}
	}
	in := followInput(asOf, refs, book, v.Config)
	equity, err := in.Equity()
	if err != nil {
		return FollowRun{}, err
	}
	if equity.Sign() <= 0 {
		return FollowRun{}, errs.New(errs.CodeForbidden, "marketplace: the install's paper book has no equity left to follow with").
			WithField("reason", "PAPER_BOOK_EXHAUSTED").WithField("equity", equity.String())
	}
	inputDigest, err := in.Digest()
	if err != nil {
		return FollowRun{}, err
	}
	startCash := book.cash

	exec, err := s.cfg.Sandbox.Execute(ctx, tx, sandboxrt.ExecuteRequest{
		ArtifactDigest: val.ArtifactDigest, Input: in, Limits: s.cfg.Limits, Manifest: v.Manifest, ReferencePrices: referencePrices(refs),
		AccountID: install.AccountID, Purpose: sandboxrt.PurposeFollow, StrategyRef: v.StrategyRef(), Actor: actor, CorrelationID: correlationID,
	})
	if err != nil {
		return FollowRun{}, err
	}

	run := FollowRun{
		ID: id.New[followKind](), InstallID: install.ID, VersionID: v.ID, AccountID: install.AccountID, SandboxRunID: exec.RunID,
		Trigger: trigger, AsOf: asOf, InputDigest: inputDigest, RunDigest: exec.Result.RunDigest,
		Termination: exec.Result.Termination, ProposalsCount: len(exec.Result.Proposals), AcceptedCount: len(exec.Accepted),
		ViolationsCount: len(exec.Violations), PaperCash: startCash.String(), Equity: equity.String(),
	}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := refs[id]
		run.Marks = append(run.Marks, FollowMark{InstrumentID: id, PriceID: r.PriceID, Mark: r.Mark.String(), ObservedAt: r.ObservedAt})
	}
	turnover := nodalsdk.Decimal{}
	var fills []paperFill
	for _, a := range exec.Accepted {
		fi, fill, err := s.followIntent(ctx, tx, install, v, exec, run.ID, a, &book, correlationID)
		if err != nil {
			return FollowRun{}, err
		}
		run.Intents = append(run.Intents, fi)
		run.IntentsSubmitted++
		switch fi.Outcome {
		case OutcomeDispatched:
			run.IntentsDispatched++
			n, err := nodalsdk.ParseDecimal(fi.Notional)
			if err != nil {
				return FollowRun{}, errs.Wrap(err, errs.CodeInternal, "marketplace: notional unreadable")
			}
			turnover = turnover.Add(n)
			if fill != nil {
				fills = append(fills, *fill)
			}
		case OutcomeRiskRejected:
			run.IntentsRiskRejected++
		default:
			run.IntentsFailed++
		}
	}
	run.Turnover = turnover.String()

	canonicalInput, err := in.Canonical()
	if err != nil {
		return FollowRun{}, err
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "marketplace/follow/" + run.ID.String(), Document: followEvidence{
		FollowRunID: run.ID.String(), InstallID: install.ID.String(), VersionID: v.ID.String(), AccountID: install.AccountID.String(),
		StrategyRef: v.StrategyRef(), ManifestHash: hex.EncodeToString(v.ManifestHash), ArtifactDigest: hex.EncodeToString(val.ArtifactDigest),
		SandboxRunID: exec.RunID.String(), InputDigest: hex.EncodeToString(inputDigest), RunDigest: hex.EncodeToString(exec.Result.RunDigest),
		OutputDigest: hex.EncodeToString(exec.Result.OutputDigest), Termination: string(exec.Result.Termination),
		Input: json.RawMessage(canonicalInput), Violations: violationsOrEmpty(exec.Violations), Intents: intentsOrEmpty(run.Intents),
		Marks: marksOrEmpty(run.Marks), PaperCash: run.PaperCash, Equity: run.Equity, Turnover: run.Turnover, AsOf: asOf,
		Trigger: string(trigger), MarkMaxAgeMS: s.cfg.MaxPriceAge.Milliseconds(),
	}})
	if err != nil {
		return FollowRun{}, err
	}
	run.EvidenceRef, run.EvidenceSHA256 = ev.URI, ev.SHA256
	// The run, then the marks it valued at, then what became of each
	// intent, then the fills: each row is checked against the ones before
	// it, and the run's book against all of them at COMMIT (MK013, MK014).
	if err := tx.QueryRow(ctx, `INSERT INTO marketplace_follow_runs (id, install_id, version_id, account_id, sandbox_run_id, trigger, as_of,
		input_digest, run_digest, proposals_count, accepted_count, violations_count, intents_submitted, intents_dispatched,
		intents_risk_rejected, intents_failed, equity, turnover, evidence_ref, evidence_sha256, actor_type, actor_id, paper_cash,
		mark_max_age_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17::numeric,$18::numeric,$19,$20,$21,$22,$23::numeric,$24)
		RETURNING created_at`,
		run.ID, run.InstallID, run.VersionID, run.AccountID, run.SandboxRunID, run.Trigger, run.AsOf, run.InputDigest, run.RunDigest,
		run.ProposalsCount, run.AcceptedCount, run.ViolationsCount, run.IntentsSubmitted, run.IntentsDispatched, run.IntentsRiskRejected,
		run.IntentsFailed, run.Equity, run.Turnover, run.EvidenceRef, run.EvidenceSHA256, actor.Type, actor.ID, run.PaperCash,
		s.cfg.MaxPriceAge.Milliseconds()).Scan(&run.CreatedAt); err != nil {
		return FollowRun{}, dbErr("record follow run", err)
	}
	run.CreatedAt = run.CreatedAt.UTC()
	for _, id := range ids {
		r := refs[id]
		if _, err := tx.Exec(ctx, `INSERT INTO marketplace_follow_marks (id, follow_run_id, instrument_id, price_id, mark, base_decimals, observed_at)
			VALUES ($1,$2,$3,$4,$5::numeric,$6,$7)`, id2(), run.ID, id, r.PriceID, r.Mark.String(), int16(r.BaseDecimals), r.ObservedAt); err != nil {
			return FollowRun{}, dbErr("record follow mark", err)
		}
	}
	for _, fi := range run.Intents {
		var decision any
		if fi.DecisionHash != "" {
			decision = fi.DecisionHash
		}
		if _, err := tx.Exec(ctx, `INSERT INTO marketplace_follow_intents (id, follow_run_id, proposal_seq, intent_id, outcome,
			risk_decision_hash, effect_id, notional) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric)`,
			id2(), run.ID, fi.ProposalSeq, fi.IntentID, fi.Outcome, decision, fi.EffectID, fi.Notional); err != nil {
			return FollowRun{}, dbErr("record follow intent", err)
		}
	}
	for _, f := range fills {
		if _, err := tx.Exec(ctx, `INSERT INTO marketplace_paper_fills (id, install_id, follow_run_id, intent_id, instrument_id, side, quantity,
			mark, notional) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9::numeric)`,
			id2(), install.ID, run.ID, f.IntentID, f.InstrumentID, f.Side, f.Quantity, f.Mark.String(), f.Notional.String()); err != nil {
			return FollowRun{}, dbErr("record paper fill", err)
		}
	}
	if err := s.auditEvent(ctx, tx, audit.AccountStream(install.AccountID.String()), actor, "marketplace.follow.recorded",
		"marketplace_follow_run", run.ID.String(), string(exec.Result.Termination), map[string]any{
			"install_id": install.ID.String(), "strategy_ref": v.StrategyRef(), "termination": string(exec.Result.Termination),
			"submitted": run.IntentsSubmitted, "dispatched": run.IntentsDispatched, "risk_rejected": run.IntentsRiskRejected,
			"failed": run.IntentsFailed, "violations": run.ViolationsCount, "equity": run.Equity, "paper_cash": run.PaperCash,
		}); err != nil {
		return FollowRun{}, err
	}
	return run, nil
}

func id2() id.ID[id.Any] { return id.New[id.Any]() }

type dispatchRequest struct {
	IntentID     string `json:"intent_id"`
	ContentHash  string `json:"content_hash"`
	DecisionHash string `json:"decision_hash"`
}

type dispatchEvidence struct {
	IntentID      string                     `json:"intent_id"`
	StrategyRef   string                     `json:"strategy_ref"`
	SandboxRunID  string                     `json:"sandbox_run_id"`
	RunDigest     string                     `json:"run_digest"`
	Proposal      sandboxrt.AcceptedProposal `json:"proposal"`
	DecisionHash  string                     `json:"decision_hash"`
	DecisionID    string                     `json:"decision_id"`
	ReceiptRef    string                     `json:"receipt_ref"`
	PolicyVersion string                     `json:"policy_version"`
}

type followEvidence struct {
	FollowRunID    string                `json:"follow_run_id"`
	InstallID      string                `json:"install_id"`
	VersionID      string                `json:"version_id"`
	AccountID      string                `json:"account_id"`
	StrategyRef    string                `json:"strategy_ref"`
	ManifestHash   string                `json:"manifest_hash"`
	ArtifactDigest string                `json:"artifact_digest"`
	SandboxRunID   string                `json:"sandbox_run_id"`
	InputDigest    string                `json:"input_digest"`
	RunDigest      string                `json:"run_digest"`
	OutputDigest   string                `json:"output_digest"`
	Termination    string                `json:"termination"`
	Trigger        string                `json:"trigger"`
	AsOf           time.Time             `json:"as_of"`
	Input          json.RawMessage       `json:"input"`
	Violations     []sandboxrt.Violation `json:"violations"`
	Intents        []FollowIntent        `json:"intents"`
	Marks          []FollowMark          `json:"marks"`
	MarkMaxAgeMS   int64                 `json:"mark_max_age_ms"`
	PaperCash      string                `json:"paper_cash"`
	Equity         string                `json:"equity"`
	Turnover       string                `json:"turnover"`
}

func marksOrEmpty(m []FollowMark) []FollowMark {
	if m == nil {
		return []FollowMark{}
	}
	return m
}

func violationsOrEmpty(v []sandboxrt.Violation) []sandboxrt.Violation {
	if v == nil {
		return []sandboxrt.Violation{}
	}
	return v
}

func intentsOrEmpty(v []FollowIntent) []FollowIntent {
	out := make([]FollowIntent, len(v))
	for i, fi := range v {
		if fi.ReasonCodes == nil {
			fi.ReasonCodes = []string{}
		}
		out[i] = fi
	}
	return out
}

// routeRefusalReasons are why a route may not execute now, sorted: the
// compiler's refusals, and for a route it permits only after a person's
// confirmation or an operator's review, that step (REQUIRES_USER_CONFIRMATION,
// REQUIRES_MANUAL_REVIEW).
func routeRefusalReasons(route settlement.Route) []string {
	reasons := append([]string{}, route.Reasons...)
	if route.Permitted && route.RequiredConfirmation != settlement.ConfirmNone {
		reasons = append(reasons, "REQUIRES_"+string(route.RequiredConfirmation))
	}
	sort.Strings(reasons)
	return reasons
}

// followIntent takes one accepted proposal through intent -> route -> risk ->
// dispatch, and reports what became of it. A dispatch the executor applied is
// a fill of the paper book, returned and applied to book.
func (s *Service) followIntent(ctx context.Context, tx pgx.Tx, install Install, v Version, exec sandboxrt.Execution, followID FollowRunID,
	a sandboxrt.AcceptedProposal, book *paperBook, correlationID string) (FollowIntent, *paperFill, error) {
	fi, fill, err := s.followIntentOnce(ctx, tx, install, v, exec, followID, a, book, correlationID)
	if err != nil {
		return FollowIntent{}, nil, err
	}
	if fill != nil {
		if err := book.apply(*fill); err != nil {
			return FollowIntent{}, nil, err
		}
	}
	return fi, fill, nil
}

func (s *Service) followIntentOnce(ctx context.Context, tx pgx.Tx, install Install, v Version, exec sandboxrt.Execution, followID FollowRunID,
	a sandboxrt.AcceptedProposal, book *paperBook, correlationID string) (FollowIntent, *paperFill, error) {
	actor := s.serviceActor()
	proposed, err := a.Intent(sandboxrt.IntentContext{
		AccountID: install.AccountID, Mode: gates.ModePaper, Capability: gates.StrategyMarketplace, ActorID: s.cfg.ServiceID,
		StrategyRef: v.StrategyRef(), RunID: exec.RunID, RunDigest: exec.Result.RunDigest, ArtifactDigest: exec.Result.ArtifactDigest,
		KeyPrefix: "mkt-follow:" + followID.String(), CorrelationID: correlationID,
	})
	if err != nil {
		return FollowIntent{}, nil, err
	}
	intent, _, err := s.cfg.Intents.Submit(ctx, tx, proposed)
	if err != nil {
		return FollowIntent{}, nil, err
	}
	fi := FollowIntent{ProposalSeq: a.Seq, IntentID: intent.ID, Kind: intent.Kind, Notional: a.Notional, ReferencePrice: a.ReferencePrice,
		ReasonCodes: []string{}}

	rc, err := s.cfg.Sections.RiskContext(ctx, tx, intent, a)
	if err != nil {
		return FollowIntent{}, nil, err
	}
	route, err := econ.Route(econ.RouteRequest{
		Intent: intent, Subject: settlement.Subject{Type: settlement.SubjectInstrument, ID: intent.InstrumentID},
		Jurisdiction: rc.Jurisdiction, Verification: rc.Verification,
	}, s.cfg.Router, s.now())
	if err != nil {
		return FollowIntent{}, nil, err
	}
	// A follow dispatches what it routes, so it needs a route that may
	// execute now: one the legal router permits only once a person has
	// confirmed it (or an operator reviewed it) is permission to ask, and a
	// follow has nobody to ask. It is refused like any other refusal, naming
	// the step it would have skipped (review 3, F5).
	if !route.MayExecuteNow() {
		reasons := routeRefusalReasons(route)
		fi.Outcome, fi.ReasonCodes = OutcomeRouteRefused, reasons
		_, err := s.cfg.Intents.Transition(ctx, tx, intent.ID, econ.StatusRejected, actor, "route refused: "+strings.Join(reasons, ","), "ROUTE_REFUSED")
		return fi, nil, err
	}
	if intent, err = s.cfg.Intents.Transition(ctx, tx, intent.ID, econ.StatusRouted, actor, "routed", ""); err != nil {
		return FollowIntent{}, nil, err
	}

	ri, err := sandboxrt.RiskIntent(intent, a)
	if err != nil {
		return FollowIntent{}, nil, err
	}
	input := rc.Input
	input.Stage = risk.StagePreTrade
	input.Now = s.now()
	input.Intent = ri
	// The sections only the marketplace can vouch for. A provider's versions
	// of them are discarded.
	input.Strategy = &risk.StrategySnapshot{Approved: v.Followable(), ManifestPermits: true}
	input.Sandbox = sandboxrt.SandboxSection(exec.Result.ArtifactVerified, len(exec.Violations))
	// A buy spends the paper book's cash: what it may spend is what the book
	// holds now (after this run's earlier fills), rounded down to the cent,
	// against the notional rounded up -- the book's cash never goes below
	// zero.
	input.Balance = nil
	if a.Side == sandboxrt.SideBuy {
		notional, err := nodalsdk.ParseDecimal(a.Notional)
		if err != nil {
			return FollowIntent{}, nil, errs.Wrap(err, errs.CodeInternal, "marketplace: notional unreadable")
		}
		available, err := usdFloor(book.cash)
		if err != nil {
			return FollowIntent{}, nil, err
		}
		required, err := usdCeil(notional)
		if err != nil {
			return FollowIntent{}, nil, err
		}
		input.Balance = &risk.BalanceSnapshot{Available: available, Required: required}
	}
	decision := risk.EvaluateAdvanced(s.cfg.RiskPolicy, input)
	decisionID, err := s.cfg.Risk.RecordAdvancedDecision(ctx, tx, input, decision, correlationID)
	if err != nil {
		return FollowIntent{}, nil, dbErr("record risk decision", err)
	}
	fi.DecisionHash, fi.ReasonCodes = decision.Hash, append([]string{}, decision.ReasonCodes...)
	if decision.Verdict != risk.Allow {
		fi.Outcome = OutcomeRiskRejected
		_, err := s.cfg.Intents.Transition(ctx, tx, intent.ID, econ.StatusRejected, actor,
			"risk rejected: "+strings.Join(decision.ReasonCodes, ","), "RISK_REJECTED")
		return fi, nil, err
	}
	if intent, err = s.cfg.Intents.Transition(ctx, tx, intent.ID, econ.StatusRiskApproved, actor, "risk approved", ""); err != nil {
		return FollowIntent{}, nil, err
	}

	iid := intent.ID
	effect, replay, err := s.cfg.Effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "mkt-dispatch:" + intent.ID.String(), Subsystem: econ.SubsystemMarketplace, Kind: DispatchEffectKind, IntentID: &iid,
		AccountID: intent.AccountID.String(), PostsLedger: false, Actor: actor, CorrelationID: correlationID,
		Request: dispatchRequest{IntentID: intent.ID.String(), ContentHash: hex.EncodeToString(intent.ContentHash), DecisionHash: decision.Hash},
	})
	if err != nil {
		return FollowIntent{}, nil, err
	}
	eid := effect.ID
	fi.EffectID = &eid
	if replay && effect.Status != econ.EffectPending {
		// Exactly once: a dispatch already decided is never repeated, and an
		// unknown one is reconciled, never retried.
		switch effect.Status {
		case econ.EffectApplied:
			fi.Outcome, fi.ExternalRef = OutcomeDispatched, effect.ExternalRef
		case econ.EffectUnknown:
			fi.Outcome = OutcomeDispatchUnknown
		default:
			fi.Outcome = OutcomeDispatchFailed
		}
		return fi, nil, nil
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return FollowIntent{}, nil, dbErr("open dispatch savepoint", err)
	}
	receipt, execErr := s.cfg.Executor.ExecutePaper(ctx, sp, PaperExecution{Intent: intent, Decision: decision, DecisionID: decisionID, Proposal: a, EffectID: effect.ID})
	switch {
	case execErr == nil:
		if err := sp.Commit(ctx); err != nil {
			return FollowIntent{}, nil, dbErr("release dispatch savepoint", err)
		}
		ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "marketplace/dispatch/" + intent.ID.String(), Document: dispatchEvidence{
			IntentID: intent.ID.String(), StrategyRef: v.StrategyRef(), SandboxRunID: exec.RunID.String(),
			RunDigest: hex.EncodeToString(exec.Result.RunDigest), Proposal: a, DecisionHash: decision.Hash, DecisionID: decisionID.String(),
			ReceiptRef: receipt.ExternalRef, PolicyVersion: decision.PolicyVersion,
		}})
		if err != nil {
			return FollowIntent{}, nil, err
		}
		if _, err := s.cfg.Effects.Apply(ctx, tx, effect.ID, econ.ApplyRequest{ExternalRef: receipt.ExternalRef, Evidence: &ev,
			Actor: actor, Reason: "dispatched to paper execution"}); err != nil {
			return FollowIntent{}, nil, err
		}
		fi.Outcome, fi.ExternalRef = OutcomeDispatched, receipt.ExternalRef
		mark, err := nodalsdk.ParseDecimal(a.ReferencePrice)
		if err != nil {
			return FollowIntent{}, nil, errs.Wrap(err, errs.CodeInternal, "marketplace: reference price unreadable")
		}
		notional, err := nodalsdk.ParseDecimal(a.Notional)
		if err != nil {
			return FollowIntent{}, nil, errs.Wrap(err, errs.CodeInternal, "marketplace: notional unreadable")
		}
		return fi, &paperFill{IntentID: intent.ID, InstrumentID: a.InstrumentID, Side: a.Side, Quantity: a.Quantity, Mark: mark,
			Notional: notional}, nil
	case errs.CodeOf(execErr) == errs.CodeSubmissionStateUnknown:
		// What execution recorded is kept: it describes the attempt whose
		// outcome reconciliation must establish.
		if err := sp.Commit(ctx); err != nil {
			_ = sp.Rollback(ctx)
		}
		if _, err := s.cfg.Effects.MarkUnknown(ctx, tx, effect.ID, receipt.ExternalRef, actor, "paper execution outcome unknown"); err != nil {
			return FollowIntent{}, nil, err
		}
		fi.Outcome, fi.ExternalRef = OutcomeDispatchUnknown, receipt.ExternalRef
	default:
		_ = sp.Rollback(ctx)
		code := string(errs.CodeOf(execErr))
		if _, err := s.cfg.Effects.Fail(ctx, tx, effect.ID, code, actor, "paper execution refused: "+truncate(execErr.Error(), 200)); err != nil {
			return FollowIntent{}, nil, err
		}
		if _, err := s.cfg.Intents.Transition(ctx, tx, intent.ID, econ.StatusRejected, actor, "paper execution refused", "DISPATCH_FAILED"); err != nil {
			return FollowIntent{}, nil, err
		}
		fi.Outcome, fi.ReasonCodes = OutcomeDispatchFailed, []string{code}
	}
	return fi, nil, nil
}

// followColumns are the columns scanFollow reads, over marketplace_follow_runs
// f joined with sandbox_runs r on the run's sandbox run.
const followColumns = `f.id, f.install_id, f.version_id, f.account_id, f.sandbox_run_id, f.trigger, f.as_of, f.input_digest, f.run_digest,
	r.termination_reason, f.proposals_count, f.accepted_count, f.violations_count, f.intents_submitted, f.intents_dispatched,
	f.intents_risk_rejected, f.intents_failed, coalesce(f.paper_cash::text, ''), f.equity::text, f.turnover::text, f.evidence_ref,
	f.evidence_sha256, f.created_at`

func scanFollow(row pgx.Row) (FollowRun, error) {
	var f FollowRun
	if err := row.Scan(&f.ID, &f.InstallID, &f.VersionID, &f.AccountID, &f.SandboxRunID, &f.Trigger, &f.AsOf, &f.InputDigest, &f.RunDigest,
		&f.Termination, &f.ProposalsCount, &f.AcceptedCount, &f.ViolationsCount, &f.IntentsSubmitted, &f.IntentsDispatched,
		&f.IntentsRiskRejected, &f.IntentsFailed, &f.PaperCash, &f.Equity, &f.Turnover, &f.EvidenceRef, &f.EvidenceSHA256,
		&f.CreatedAt); err != nil {
		return FollowRun{}, err
	}
	f.AsOf, f.CreatedAt = f.AsOf.UTC(), f.CreatedAt.UTC()
	return f, nil
}

func (s *Service) followAt(ctx context.Context, q db.Querier, iid InstallID, asOf time.Time) (FollowRun, bool, error) {
	f, err := scanFollow(q.QueryRow(ctx, `SELECT `+followColumns+` FROM marketplace_follow_runs f
		JOIN sandbox_runs r ON r.id = f.sandbox_run_id WHERE f.install_id = $1 AND f.as_of = $2`, iid, asOf))
	if errors.Is(err, pgx.ErrNoRows) {
		return FollowRun{}, false, nil
	}
	if err != nil {
		return FollowRun{}, false, dbErr("load follow run", err)
	}
	if f.Intents, err = s.FollowIntents(ctx, q, f.ID); err != nil {
		return FollowRun{}, false, err
	}
	if f.Marks, err = s.FollowMarks(ctx, q, f.ID); err != nil {
		return FollowRun{}, false, err
	}
	return f, true, nil
}

// FollowMarks returns the reference prices a follow run valued its paper book
// and its proposals at.
func (s *Service) FollowMarks(ctx context.Context, q db.Querier, fid FollowRunID) ([]FollowMark, error) {
	return followMarks(ctx, q, fid)
}

func followMarks(ctx context.Context, q db.Querier, fid FollowRunID) ([]FollowMark, error) {
	rows, err := q.Query(ctx, `SELECT instrument_id::text, price_id, mark::text, observed_at FROM marketplace_follow_marks
		WHERE follow_run_id = $1 ORDER BY instrument_id`, fid)
	if err != nil {
		return nil, dbErr("list follow marks", err)
	}
	defer rows.Close()
	out := []FollowMark{}
	for rows.Next() {
		var m FollowMark
		if err := rows.Scan(&m.InstrumentID, &m.PriceID, &m.Mark, &m.ObservedAt); err != nil {
			return nil, dbErr("scan follow mark", err)
		}
		m.ObservedAt = m.ObservedAt.UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// FollowRuns returns an install's follow history, oldest first.
func (s *Service) FollowRuns(ctx context.Context, q db.Querier, iid InstallID) ([]FollowRun, error) {
	rows, err := q.Query(ctx, `SELECT `+followColumns+` FROM marketplace_follow_runs f
		JOIN sandbox_runs r ON r.id = f.sandbox_run_id WHERE f.install_id = $1 ORDER BY f.as_of, f.id`, iid)
	if err != nil {
		return nil, dbErr("list follow runs", err)
	}
	defer rows.Close()
	var out []FollowRun
	for rows.Next() {
		f, err := scanFollow(rows)
		if err != nil {
			return nil, dbErr("scan follow run", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FollowIntents returns what became of each proposal of a follow run.
func (s *Service) FollowIntents(ctx context.Context, q db.Querier, fid FollowRunID) ([]FollowIntent, error) {
	return followIntents(ctx, q, fid)
}

func followIntents(ctx context.Context, q db.Querier, fid FollowRunID) ([]FollowIntent, error) {
	rows, err := q.Query(ctx, `SELECT fi.proposal_seq, fi.intent_id, ei.kind, fi.outcome, coalesce(fi.risk_decision_hash, ''), fi.effect_id,
		fi.notional::text, coalesce(ei.terms->>'reference_price', ''), coalesce(ee.external_ref, '') FROM marketplace_follow_intents fi
		JOIN economic_intents ei ON ei.id = fi.intent_id
		LEFT JOIN economic_effects ee ON ee.id = fi.effect_id
		WHERE fi.follow_run_id = $1 ORDER BY fi.proposal_seq`, fid)
	if err != nil {
		return nil, dbErr("list follow intents", err)
	}
	defer rows.Close()
	var out []FollowIntent
	for rows.Next() {
		var fi FollowIntent
		if err := rows.Scan(&fi.ProposalSeq, &fi.IntentID, &fi.Kind, &fi.Outcome, &fi.DecisionHash, &fi.EffectID, &fi.Notional, &fi.ReferencePrice,
			&fi.ExternalRef); err != nil {
			return nil, dbErr("scan follow intent", err)
		}
		out = append(out, fi)
	}
	return out, rows.Err()
}

// DueInstalls returns ACTIVE installs of followable versions whose next run
// is due at now under their manifest's execution frequency.
func (s *Service) DueInstalls(ctx context.Context, q db.Querier, now time.Time, limit int) ([]Install, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT i.id, i.account_id, i.version_id, i.mode, i.status, i.installed_by_actor_type, i.installed_by_actor_id,
		i.paper_capital::text, i.created_at, (SELECT max(as_of) FROM marketplace_follow_runs f WHERE f.install_id = i.id)
		FROM marketplace_installs i JOIN marketplace_listing_versions v ON v.id = i.version_id
		WHERE i.status = 'ACTIVE' AND v.status = 'APPROVED' AND v.moderation_state = 'CLEARED'
		ORDER BY i.created_at, i.id`)
	if err != nil {
		return nil, dbErr("list installs", err)
	}
	type candidate struct {
		in   Install
		last *time.Time
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.in.ID, &c.in.AccountID, &c.in.VersionID, &c.in.Mode, &c.in.Status, &c.in.InstalledBy.Type,
			&c.in.InstalledBy.ID, &c.in.PaperCapital, &c.in.CreatedAt, &c.last); err != nil {
			rows.Close()
			return nil, dbErr("scan install", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, dbErr("list installs", err)
	}
	var out []Install
	for _, c := range cands {
		if len(out) >= limit {
			break
		}
		if c.last != nil {
			v, err := s.Version(ctx, q, c.in.VersionID)
			if err != nil {
				return nil, err
			}
			if now.Before(c.last.UTC().Add(time.Duration(v.Manifest.MinIntervalSeconds) * time.Second)) {
				continue
			}
		}
		c.in.CreatedAt = c.in.CreatedAt.UTC()
		out = append(out, c.in)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
