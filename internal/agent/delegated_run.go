package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Evidence kinds of the delegated path. agent_runs.evidence is an array: the
// run's freshness checks (which carry no kind) and, when delegated authority
// is wired, one DELEGATED_TOOLS element saying which delegated tools the run
// held (or why none) and one DELEGATED_CALL element per delegated request it
// made, admitted or refused.
const (
	EvidenceKindDelegatedTools = "DELEGATED_TOOLS"
	EvidenceKindDelegatedCall  = "DELEGATED_CALL"
)

// DelegatedToolsRecord is a run's delegated toolset as its evidence records
// it.
type DelegatedToolsRecord struct {
	Kind   string             `json:"kind"`
	Tools  []DelegatedToolRef `json:"tools"`
	Reason string             `json:"reason,omitempty"`
	Detail string             `json:"detail,omitempty"`
	At     time.Time          `json:"at"`
}

// DelegatedToolRef names one tool a run held.
type DelegatedToolRef struct {
	Tool         string `json:"tool"`
	CapabilityID string `json:"capability_id"`
}

// DelegatedRecord is one delegated request a run made, as its evidence
// records it and RunResult returns it: what was asked under which
// capability, and what came back -- the subject delegation admitted and its
// status, or the refusal, whoever refused it.
type DelegatedRecord struct {
	Kind         string `json:"kind"`
	Action       string `json:"action"`
	Tool         string `json:"tool"`
	CapabilityID string `json:"capability_id,omitempty"`
	Nonce        string `json:"nonce,omitempty"`
	// SubjectKind is SWAP or WITHDRAWAL; SubjectID and Status are the
	// admitted subject's at the instant it was admitted, IntentID its
	// economic intent's.
	SubjectKind string `json:"subject_kind,omitempty"`
	SubjectID   string `json:"subject_id,omitempty"`
	Status      string `json:"status,omitempty"`
	IntentID    string `json:"intent_id,omitempty"`
	Replay      bool   `json:"replay,omitempty"`
	// Refusal is the refusal's error code, Reason its stable reason (the
	// runtime's DelegatedReason* or delegation's own) and Detail its
	// client-safe text. Empty when the request was admitted.
	Refusal    string    `json:"refusal,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Refused reports whether the request was refused.
func (r DelegatedRecord) Refused() bool { return r.Refusal != "" }

// refusalError rebuilds a recorded refusal, so a run that resumes after one
// ends exactly as the attempt that recorded it would have.
func (r DelegatedRecord) refusalError() error {
	return errs.New(errs.Code(r.Refusal), r.Detail).WithField("reason", r.Reason)
}

// maxRecordedDetail bounds the text a record keeps.
const maxRecordedDetail = 1000

// refusedBy fills the record's refusal from err.
func (r DelegatedRecord) refusedBy(err error) DelegatedRecord {
	r.Refusal = string(errs.CodeOf(err))
	r.Detail = err.Error()
	r.Reason = r.Refusal
	if e, ok := errs.As(err); ok {
		r.Detail = e.Detail
		if reason, ok := e.Fields["reason"].(string); ok && reason != "" {
			r.Reason = reason
		}
	}
	if len(r.Detail) > maxRecordedDetail {
		r.Detail = r.Detail[:maxRecordedDetail]
	}
	return r
}

// answered fills the record from delegation's outcome.
func (r DelegatedRecord) answered(o DelegatedOutcome) DelegatedRecord {
	r.CapabilityID = o.CapabilityID
	r.Nonce = strconv.FormatUint(o.Nonce, 10)
	r.SubjectKind, r.Replay = o.SubjectKind, o.Replay
	if o.Refusal != nil {
		return r.refusedBy(o.Refusal)
	}
	r.SubjectID, r.Status, r.IntentID = o.SubjectID, o.Status, o.IntentID
	return r
}

// runEvidence assembles agent_runs.evidence: the freshness checks of this
// attempt, the delegated toolset when delegation is wired, and every
// delegated request of the run -- including those recorded by an earlier
// attempt, which are carried forward and never asked again.
type runEvidence struct {
	checks []FreshnessCheck
	tools  *DelegatedToolsRecord
	calls  []DelegatedRecord
}

// delegatedRecordsOf reads the delegated requests a stored run already
// recorded. Elements that are not DELEGATED_CALL are not its business.
func delegatedRecordsOf(raw json.RawMessage) []DelegatedRecord {
	var elems []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &elems) != nil {
		return nil
	}
	var out []DelegatedRecord
	for _, el := range elems {
		var kind struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(el, &kind) != nil || kind.Kind != EvidenceKindDelegatedCall {
			continue
		}
		var rec DelegatedRecord
		if json.Unmarshal(el, &rec) == nil && rec.Action != "" {
			out = append(out, rec)
		}
	}
	return out
}

// call returns the request recorded for an action, if any.
func (e *runEvidence) call(action string) (DelegatedRecord, bool) {
	for _, c := range e.calls {
		if c.Action == action {
			return c, true
		}
	}
	return DelegatedRecord{}, false
}

// with returns the evidence with rec recorded, leaving e unchanged.
func (e runEvidence) with(rec DelegatedRecord) runEvidence {
	e.calls = append(append([]DelegatedRecord(nil), e.calls...), rec)
	return e
}

// render encodes the array. With nothing delegated it is exactly the
// freshness checks, as it always was.
func (e runEvidence) render() json.RawMessage {
	elems := make([]any, 0, len(e.checks)+1+len(e.calls))
	for _, c := range e.checks {
		elems = append(elems, c)
	}
	if e.tools != nil {
		elems = append(elems, *e.tools)
	}
	for _, c := range e.calls {
		elems = append(elems, c)
	}
	return mustJSON(elems)
}

// delegatedTools builds the run's delegated proposer and reads its toolset.
// With no delegated authority wired the run holds no tool, and says so.
func (r *Runner) delegatedTools(ctx context.Context, auth Authority) (DelegatedProposer, DelegatedToolset, error) {
	if r.deps.DelegatedFor == nil {
		return nil, DelegatedToolset{Reason: DelegatedReasonNotWired,
			Detail: "no delegated authority is wired in this runtime"}, nil
	}
	p, err := r.deps.DelegatedFor(auth)
	if err != nil {
		return nil, DelegatedToolset{}, err
	}
	ts, err := p.Tools(ctx, r.deps.DB)
	if err != nil {
		return nil, DelegatedToolset{}, err
	}
	return p, ts, nil
}

// toolsRecord is the toolset as the evidence records it.
func toolsRecord(ts DelegatedToolset, at time.Time) *DelegatedToolsRecord {
	rec := &DelegatedToolsRecord{Kind: EvidenceKindDelegatedTools, Tools: []DelegatedToolRef{}, Reason: ts.Reason,
		Detail: ts.Detail, At: at}
	for _, t := range ts.Tools {
		rec.Tools = append(rec.Tools, DelegatedToolRef{Tool: string(t.Kind), CapabilityID: t.CapabilityID})
	}
	return rec
}

// delegate performs one delegated decision and records it on the run. A
// request an earlier attempt of this run recorded is answered from that
// record and never made again: admitted, it stands; refused, the run ends
// with the same refusal -- never a blind retry. Otherwise the request and its
// record on the run commit in one transaction, so no attempt can make a
// request and lose the record of it. A returned error ends the run: a
// refusal (whoever refused it) is recorded first.
func (r *Runner) delegate(ctx context.Context, run *Run, d Decision, proposer DelegatedProposer, toolset DelegatedToolset,
	ev *runEvidence) (DelegatedRecord, error) {
	if prior, ok := ev.call(d.ActionName); ok {
		if prior.Refused() {
			return prior, prior.refusalError()
		}
		return prior, nil
	}
	rec := DelegatedRecord{Kind: EvidenceKindDelegatedCall, Action: d.ActionName, Tool: string(d.Kind),
		RecordedAt: r.deps.Clock.Now().UTC()}
	if d.DelegatedSwap != nil {
		rec.CapabilityID = d.DelegatedSwap.CapabilityID
	}
	if d.DelegatedWithdrawal != nil {
		rec.CapabilityID = d.DelegatedWithdrawal.CapabilityID
	}
	refuse := func(err error) (DelegatedRecord, error) {
		rec = rec.refusedBy(err)
		*ev = ev.with(rec)
		run.Evidence = ev.render()
		return rec, err
	}
	if proposer == nil {
		return refuse(errs.New(errs.CodeEffectForbidden, "agent: "+toolset.Detail).WithField("reason", toolset.Reason))
	}
	if run.PredictionID == "" {
		return refuse(errs.New(errs.CodeValidationFailed, "agent: a delegated request needs the prediction that preceded it").
			WithField("prediction_id", "required").WithField("reason", DelegatedReasonPredictionRequired))
	}
	call := DelegatedCall{RunID: run.ID, ActionName: d.ActionName, PredictionID: run.PredictionID,
		CorrelationID: run.CorrelationID, Kind: d.Kind, Swap: d.DelegatedSwap, Withdrawal: d.DelegatedWithdrawal}
	var answered DelegatedRecord
	err := r.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
		func(ctx context.Context, tx pgx.Tx) error {
			// Only a transaction Postgres rolled back (serialization or
			// deadlock) is run again, and then nothing of it was kept.
			out, perr := proposer.Propose(ctx, tx, toolset, call)
			if perr != nil {
				return perr
			}
			answered = rec.answered(out)
			return r.deps.Store.RecordEvidence(ctx, tx, run.ID, ev.with(answered).render())
		})
	if err != nil {
		return refuse(err)
	}
	*ev = ev.with(answered)
	run.Evidence = ev.render()
	if answered.Refused() {
		return answered, answered.refusalError()
	}
	return answered, nil
}

// delegatedSkipReason is the skip a delegated refusal ends its run with: the
// bounds the runtime itself names (the tool the run does not hold, a pause or
// kill switch, the order-rate budget, a stale price). Every other refusal --
// scope, limits, expiry, revocation, allow-lists, the account's own policy --
// ends the run FAILED with the refusal recorded, as a refused trade intent
// does.
func delegatedSkipReason(err error) SkipReason {
	switch errs.CodeOf(err) {
	case errs.CodeEffectForbidden:
		return SkipEffectMismatch
	case errs.CodeKillSwitchActive:
		return SkipKillSwitch
	case errs.CodeRateLimited:
		return SkipRateLimited
	case errs.CodeStaleMarketData:
		return SkipStaleData
	}
	return ""
}
