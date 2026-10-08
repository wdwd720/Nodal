package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// The delegated path: an agent acting under authority its account's owner
// delegated to it (internal/delegation).
//
// Why an emitter path and not two ToolBroker tools. The broker is the path
// from agent code to the outside world for READS: its tools are registered
// rows whose effect is one of five read or model effects (the tools.effect
// CHECK), it dials adapters through an egress-guarded HTTP client, charges
// the data budget and records tool_invocations. A delegated request is none
// of that -- it is an in-process, money-adjacent proposal that platform code
// then decides, signs and settles -- which is exactly what IntentEmitter
// already is for a trade intent. So the delegated path is the emitter's
// sibling: bound to the run's frozen Authority, AGENT principal only, the
// pause re-read inside the transaction, the order-rate budget, the strategy's
// effect set and the run's mode, and it only ever proposes. Its two actions
// are runtime decision kinds beside CREATE_TRADE_INTENT, never broker tools.

// The two delegated action kinds. A Decision of either kind carries its
// request in Decision.DelegatedSwap or Decision.DelegatedWithdrawal. They are
// runtime actions, like ir.ActionCreateTradeIntent: no compiled strategy
// declares them, and the evaluator may return one only for a tool its
// EvalInput.Delegated offered.
const (
	ActionRequestDelegatedSwap       ir.ActionKind = "REQUEST_DELEGATED_SWAP"
	ActionProposeDelegatedWithdrawal ir.ActionKind = "PROPOSE_DELEGATED_WITHDRAWAL"
)

// DelegatedActions returns the delegated action kinds.
func DelegatedActions() []ir.ActionKind {
	return []ir.ActionKind{ActionProposeDelegatedWithdrawal, ActionRequestDelegatedSwap}
}

// Reasons the runtime records when a run has no delegated tool, or refuses a
// delegated request before delegation sees it. Stable and machine-readable;
// a refusal by delegation itself carries delegation's own reason instead.
const (
	DelegatedReasonNotWired           = "DELEGATED_AUTHORITY_NOT_WIRED"
	DelegatedReasonModeNotAdmitted    = "DELEGATED_MODE_NOT_ADMITTED"
	DelegatedReasonEnvelopeBound      = "DELEGATED_ENVELOPE_BOUND"
	DelegatedReasonEffectNotDeclared  = "DELEGATED_EFFECT_NOT_DECLARED"
	DelegatedReasonNoCapability       = "DELEGATED_NO_USABLE_CAPABILITY"
	DelegatedReasonToolUnavailable    = "DELEGATED_TOOL_UNAVAILABLE"
	DelegatedReasonPredictionRequired = "DELEGATED_PREDICTION_REQUIRED"
	DelegatedReasonInvalidRequest     = "DELEGATED_INVALID_REQUEST"
	DelegatedReasonAgentPaused        = "AGENT_PAUSED"
	DelegatedReasonOrderRate          = "ORDER_RATE"
)

// DelegatedTool is one delegated action a run may take: a kind, under one
// capability the account's owner granted this agent, with the grant's terms
// the evaluator sizes against. It is platform state read before the
// evaluation, never content; delegation measures every request against the
// capability again, as it stands at that instant.
type DelegatedTool struct {
	Kind         ir.ActionKind
	CapabilityID string
	// Mode is the capability's mode: SANDBOX or DEVNET.
	Mode string
	// Actions are the action classes the capability grants for this kind:
	// ACQUIRE_EXPOSURE and/or REDUCE_EXPOSURE for a swap, PROPOSE_WITHDRAWAL
	// for a withdrawal. They are reported as granted; the runtime decides
	// nothing on a swap's label (delegation's admission does).
	Actions               []string
	InstrumentClasses     []string
	Chains                []string
	MaxSingleTradeUSD     money.USD
	MaxDailyNotionalUSD   money.USD
	ExpiresAt             time.Time
	RequiresHumanApproval bool
}

// DelegatedToolset is a run's delegated tools, or why it has none.
type DelegatedToolset struct {
	// Tools are sorted by capability, then kind.
	Tools []DelegatedTool
	// Reason and Detail say why Tools is empty; both are empty when it is not.
	Reason string
	Detail string
}

// Lookup returns the tool of kind under capabilityID, if the run holds it.
func (s DelegatedToolset) Lookup(kind ir.ActionKind, capabilityID string) (DelegatedTool, bool) {
	for _, t := range s.Tools {
		if t.Kind == kind && strings.EqualFold(t.CapabilityID, strings.TrimSpace(capabilityID)) {
			return t, true
		}
	}
	return DelegatedTool{}, false
}

// DelegationAdmitted reports whether a run under a may hold delegated tools
// at all, and the reason and detail when it may not. Every existing bound of
// the run applies:
//
//   - Mode. Delegated authority moves SIMULATED value on simulated chains
//     (SANDBOX and DEVNET), so only a PAPER run, whose actions settle in
//     simulated value, may use it. A BACKTEST run is a replay and must touch
//     nothing now; a SHADOW run records what it would have done and does
//     nothing; CANARY, LIMITED and LIVE runs deploy real capital, and a
//     simulated-chain request has no place in their record (PART 159).
//   - Envelope. A run bound to a capital envelope gets none: the envelope
//     bounds real capital, and a simulated-chain request cannot be measured
//     against it.
//   - Effects. A delegated request is a proposal of the same class as a trade
//     intent, so a strategy that does not declare CREATE_TRADE_INTENT may not
//     make one.
func DelegationAdmitted(a Authority) (bool, string, string) {
	switch {
	case a.Mode() != ModePaper:
		return false, DelegatedReasonModeNotAdmitted,
			"delegated authority moves simulated value and is offered only to PAPER runs, not " + a.Mode().String()
	case a.EnvelopeID() != "":
		return false, DelegatedReasonEnvelopeBound,
			"a run bound to a capital envelope is offered no delegated authority"
	case !a.AllowsEffect(ir.EffectCreateTradeIntent):
		return false, DelegatedReasonEffectNotDeclared,
			"the strategy does not declare CREATE_TRADE_INTENT, so it may propose nothing"
	}
	return true, "", ""
}

// DelegatedToolsFor derives a run's delegated toolset from its frozen
// authority and the capabilities the agent holds, at now. A capability yields
// tools only when it is the agent's own, on the run's account, ACTIVE, inside
// its lifetime and intact (delegation.Capability.UsableAt): the swap tool
// (REQUEST_DELEGATED_SWAP) when it grants ACQUIRE_EXPOSURE or
// REDUCE_EXPOSURE, the withdrawal tool (PROPOSE_DELEGATED_WITHDRAWAL) when it
// grants PROPOSE_WITHDRAWAL. It is pure.
func DelegatedToolsFor(a Authority, caps []delegation.Capability, now time.Time) DelegatedToolset {
	if ok, reason, detail := DelegationAdmitted(a); !ok {
		return DelegatedToolset{Reason: reason, Detail: detail}
	}
	var tools []DelegatedTool
	for _, c := range caps {
		if c.Terms.AccountID.String() != a.AccountID() || !strings.EqualFold(c.Terms.AgentID, a.AgentID().String()) || !c.UsableAt(now) {
			continue
		}
		base := DelegatedTool{
			CapabilityID: c.ID.String(), Mode: string(c.Terms.Mode),
			InstrumentClasses: stringsOf(c.Terms.InstrumentClasses), Chains: stringsOf(c.Terms.ChainAllowlist),
			MaxSingleTradeUSD: c.Terms.MaxSingleTrade, MaxDailyNotionalUSD: c.Terms.MaxDailyNotional,
			ExpiresAt: c.Terms.ExpiresAt, RequiresHumanApproval: c.Terms.RequiresHumanApproval,
		}
		var swaps []string
		for _, ac := range c.Terms.ActionClasses {
			if ac.IsSwap() {
				swaps = append(swaps, string(ac))
			}
		}
		if len(swaps) > 0 {
			t := base
			t.Kind, t.Actions = ActionRequestDelegatedSwap, sortedStrings(swaps)
			tools = append(tools, t)
		}
		if c.Terms.Grants(delegation.ActionProposeWithdrawal) {
			t := base
			t.Kind, t.Actions = ActionProposeDelegatedWithdrawal, []string{string(delegation.ActionProposeWithdrawal)}
			tools = append(tools, t)
		}
	}
	if len(tools) == 0 {
		return DelegatedToolset{Reason: DelegatedReasonNoCapability,
			Detail: "the agent holds no ACTIVE, unexpired capability on this account"}
	}
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].CapabilityID != tools[j].CapabilityID {
			return tools[i].CapabilityID < tools[j].CapabilityID
		}
		return tools[i].Kind < tools[j].Kind
	})
	return DelegatedToolset{Tools: tools}
}

// DelegatedSwapRequest is what an evaluator asks for with a
// REQUEST_DELEGATED_SWAP decision: pay AmountIn base units of the pay asset
// for the receive asset at a registered sandbox venue, under one of its
// capabilities. The runtime supplies the account, the request nonce and the
// correlation; an evaluator can choose none of them.
type DelegatedSwapRequest struct {
	CapabilityID string
	// Action is the swap's label, ACQUIRE_EXPOSURE or REDUCE_EXPOSURE. The
	// runtime gives it no meaning: no bound here is relaxed or tightened by
	// it, and delegation's admission alone judges it.
	Action         string
	VenueID        string
	PayAssetID     string
	ReceiveAssetID string
	AmountIn       money.Quantity
	MaxSlippageBPS money.BPS
	Jurisdiction   string
}

// DelegatedWithdrawalRequest is what an evaluator asks for with a
// PROPOSE_DELEGATED_WITHDRAWAL decision: withdraw Amount base units of an
// asset from the account's delegated wallet to one of the account's
// allow-listed destinations. It is a proposal; a person decides it, unless
// the account's own policy auto-approves it in a test environment.
type DelegatedWithdrawalRequest struct {
	CapabilityID  string
	DestinationID string
	AssetID       string
	Amount        money.Quantity
	Jurisdiction  string
}

// Bounds delegation applies to a request's free-form fields.
const (
	maxDelegatedJurisdiction = 32
	maxDelegatedCorrelation  = 200
)

// DelegatedNonce is the request nonce of a run's delegated action: derived
// from the run and the action name, never chosen by the evaluator. Delegation
// keys a request by (capability, nonce) for ever, so a second attempt of the
// same action is the same request and is answered with the original subject
// or the original refusal -- the property that makes the runner resumable
// without ever asking twice.
func DelegatedNonce(runID RunID, action string) uint64 {
	sum := sha256.Sum256([]byte("nodal/agent/delegated-request/v1\x00" + runID.String() + "\x00" + action))
	n := binary.BigEndian.Uint64(sum[:8]) >> 1 // within int64, so every reader of the numeric column agrees
	if n == 0 {
		return 1
	}
	return n
}

// swapRequestFor checks r against the tool it names and builds delegation's
// request. Every refusal is VALIDATION_FAILED with the offending fields.
func swapRequestFor(r DelegatedSwapRequest, tool DelegatedTool, account accounts.AccountID, nonce uint64, correlationID string) (delegation.SwapRequest, error) {
	p := problems{}
	cid, err := delegation.ParseCapabilityID(tool.CapabilityID)
	if err != nil || tool.Kind != ActionRequestDelegatedSwap {
		p.add("capability_id", "names no swap tool the run holds")
	}
	// The label is passed through and nothing here interprets it: whether a
	// label is granted, and whether it matches the direction value actually
	// moves in, is delegation's admission to decide on every request. The
	// runtime only refuses what is not a swap label at all.
	action := delegation.ActionClass(r.Action)
	if !action.IsSwap() {
		p.add("action", "must be ACQUIRE_EXPOSURE or REDUCE_EXPOSURE")
	}
	venue, err := delegation.ParseVenueID(strings.TrimSpace(r.VenueID))
	if err != nil {
		p.add("venue_id", "must be a canonical uuid")
	}
	pay, perr := assets.ParseAssetID(strings.TrimSpace(r.PayAssetID))
	if perr != nil {
		p.add("pay_asset_id", "must be a canonical uuid")
	}
	recv, rerr := assets.ParseAssetID(strings.TrimSpace(r.ReceiveAssetID))
	if rerr != nil {
		p.add("receive_asset_id", "must be a canonical uuid")
	}
	if perr == nil && rerr == nil && pay == recv {
		p.add("receive_asset_id", "must differ from the pay asset")
	}
	p.amount("amount_in", r.AmountIn)
	if r.MaxSlippageBPS < 0 || r.MaxSlippageBPS > money.OneHundredPercent {
		p.add("max_slippage_bps", "must be 0..10000")
	}
	p.bounded("jurisdiction", r.Jurisdiction, maxDelegatedJurisdiction)
	p.bounded("correlation_id", correlationID, maxDelegatedCorrelation)
	if err := p.err("agent: invalid delegated swap request"); err != nil {
		return delegation.SwapRequest{}, err
	}
	return delegation.SwapRequest{
		CapabilityID: cid, AccountID: account, Nonce: nonce, Action: action, VenueID: venue,
		PayAssetID: pay, ReceiveAssetID: recv, AmountIn: r.AmountIn, MaxSlippageBPS: r.MaxSlippageBPS,
		Jurisdiction: r.Jurisdiction, CorrelationID: correlationID,
	}, nil
}

// withdrawalRequestFor checks r against the tool it names and builds
// delegation's proposal.
func withdrawalRequestFor(r DelegatedWithdrawalRequest, tool DelegatedTool, account accounts.AccountID, nonce uint64, correlationID string) (delegation.ProposalRequest, error) {
	p := problems{}
	cid, err := delegation.ParseCapabilityID(tool.CapabilityID)
	if err != nil || tool.Kind != ActionProposeDelegatedWithdrawal {
		p.add("capability_id", "names no withdrawal tool the run holds")
	}
	dest, err := delegation.ParseDestinationID(strings.TrimSpace(r.DestinationID))
	if err != nil {
		p.add("destination_id", "must be a canonical uuid")
	}
	asset, err := assets.ParseAssetID(strings.TrimSpace(r.AssetID))
	if err != nil {
		p.add("asset_id", "must be a canonical uuid")
	}
	p.amount("amount", r.Amount)
	p.bounded("jurisdiction", r.Jurisdiction, maxDelegatedJurisdiction)
	p.bounded("correlation_id", correlationID, maxDelegatedCorrelation)
	if err := p.err("agent: invalid delegated withdrawal proposal"); err != nil {
		return delegation.ProposalRequest{}, err
	}
	return delegation.ProposalRequest{
		CapabilityID: cid, AccountID: account, Nonce: nonce, DestinationID: dest, AssetID: asset,
		Amount: r.Amount, Jurisdiction: r.Jurisdiction, CorrelationID: correlationID,
	}, nil
}

// problems collects a request's field refusals, first one per field.
type problems map[string]any

func (p problems) add(field, msg string) {
	if _, dup := p[field]; !dup {
		p[field] = msg
	}
}

func (p problems) amount(field string, q money.Quantity) {
	if !q.IsPositive() {
		p.add(field, "must be positive")
		return
	}
	if _, err := devnet.AmountOf(q); err != nil {
		p.add(field, "must fit a devnet amount (u64)")
	}
}

func (p problems) bounded(field, s string, limit int) {
	if len(s) > limit {
		p.add(field, "must be at most "+strconv.Itoa(limit)+" bytes")
	}
}

func (p problems) err(msg string) error {
	if len(p) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, msg).WithFields(p).WithField("reason", DelegatedReasonInvalidRequest)
}

// DelegatedAuthority is the slice of delegated agent authority the runtime
// holds: *delegation.AgentAdmission, the facade an agent tree may hold. It
// admits a swap request, records a withdrawal proposal and reads the agent's
// own capabilities. It has no method that signs, executes, approves, decides,
// grants or revokes, and nothing in this package can reach one.
type DelegatedAuthority interface {
	RequestSwap(ctx context.Context, tx pgx.Tx, req delegation.SwapRequest) (delegation.SwapTicket, error)
	ProposeWithdrawal(ctx context.Context, tx pgx.Tx, req delegation.ProposalRequest) (delegation.WithdrawalTicket, error)
	AgentCapabilities(ctx context.Context, q db.Querier, account accounts.AccountID) ([]delegation.Capability, error)
}

var _ DelegatedAuthority = (*delegation.AgentAdmission)(nil)

// DelegatedProposer is what the runner holds for one run's delegated path.
type DelegatedProposer interface {
	// Tools reads the run's delegated toolset under the agent principal in
	// ctx.
	Tools(ctx context.Context, q db.Querier) (DelegatedToolset, error)
	// Propose makes one delegated request inside the caller's transaction.
	// A refusal delegation recorded comes back in DelegatedOutcome.Refusal
	// with a nil error: the transaction must commit, because the refusal is
	// part of it. An error means nothing was recorded and the transaction
	// rolls back.
	Propose(ctx context.Context, tx pgx.Tx, toolset DelegatedToolset, call DelegatedCall) (DelegatedOutcome, error)
}

// DelegatedCall is one delegated action the runner asks for. The runner fills
// the run, the action, the prediction and the correlation; the evaluator
// supplied only the request.
type DelegatedCall struct {
	RunID         RunID
	ActionName    string
	PredictionID  string
	CorrelationID string
	Kind          ir.ActionKind
	Swap          *DelegatedSwapRequest
	Withdrawal    *DelegatedWithdrawalRequest
}

// DelegatedOutcome is delegation's answer to one request: the subject it
// admitted and its status, or the refusal it recorded.
type DelegatedOutcome struct {
	CapabilityID string
	Nonce        uint64
	// SubjectKind is SWAP or WITHDRAWAL; SubjectID and Status are the
	// admitted subject's, IntentID its economic intent's.
	SubjectKind string
	SubjectID   string
	Status      string
	IntentID    string
	// Replay reports that delegation had already answered this request.
	Replay bool
	// Refusal is the refusal delegation recorded in the transaction.
	Refusal error
}

// DelegatedEmitterDeps are the delegated emitter's collaborators.
type DelegatedEmitterDeps struct {
	Clock     clock.Clock
	Authority DelegatedAuthority
	Pauses    PauseChecker
	Budgets   BudgetReader
}

// DelegatedEmitter proposes one run's delegated actions. With IntentEmitter
// it is the only money-adjacent write agent code can reach, and like it, it
// only proposes: the owner's decision, the delegated signer's inspection and
// signature, submission and settlement are performed afterwards by code the
// agent cannot reach.
type DelegatedEmitter struct {
	deps      DelegatedEmitterDeps
	authority Authority
}

var _ DelegatedProposer = (*DelegatedEmitter)(nil)

// NewDelegatedEmitter binds a delegated emitter to one run's frozen
// authority. Every collaborator is required.
func NewDelegatedEmitter(deps DelegatedEmitterDeps, authority Authority) (*DelegatedEmitter, error) {
	switch {
	case deps.Clock == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: delegated emitter requires a clock")
	case deps.Authority == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: delegated emitter requires delegated authority")
	case deps.Pauses == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: delegated emitter requires a pause checker")
	case deps.Budgets == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: delegated emitter requires a budget reader")
	case authority.AgentID().IsZero():
		return nil, errs.New(errs.CodeValidationFailed, "agent: delegated emitter requires a frozen authority")
	}
	return &DelegatedEmitter{deps: deps, authority: authority}, nil
}

// Tools reads the agent's capabilities through delegation's own read and
// derives the run's toolset. A run delegation is not admitted to (mode,
// envelope, effects) reads nothing.
func (e *DelegatedEmitter) Tools(ctx context.Context, q db.Querier) (DelegatedToolset, error) {
	if ok, reason, detail := DelegationAdmitted(e.authority); !ok {
		return DelegatedToolset{Reason: reason, Detail: detail}, nil
	}
	if err := e.requireAgent(ctx); err != nil {
		return DelegatedToolset{}, err
	}
	account, err := accounts.ParseAccountID(e.authority.AccountID())
	if err != nil {
		return DelegatedToolset{}, errs.Wrap(err, errs.CodeInternal, "agent: the authority's account is not an account id")
	}
	caps, err := e.deps.Authority.AgentCapabilities(ctx, q, account)
	if err != nil {
		return DelegatedToolset{}, err
	}
	return DelegatedToolsFor(e.authority, caps, e.deps.Clock.Now()), nil
}

// Propose makes one delegated request. It refuses, in order: a principal that
// is not this run's agent on this account, a call without its run, action or
// prediction, a run delegation is not admitted to, a tool the run does not
// hold, a malformed request, an open pause, and a spent order-rate budget.
// Then delegation admits or refuses it under the capability -- scope, limits,
// expiry, revocation, allow-lists, kill switches, Risk V2 -- recording either
// way. Nothing here signs, executes, approves or decides.
func (e *DelegatedEmitter) Propose(ctx context.Context, tx pgx.Tx, toolset DelegatedToolset, call DelegatedCall) (DelegatedOutcome, error) {
	if err := e.requireAgent(ctx); err != nil {
		return DelegatedOutcome{}, err
	}
	if call.RunID.IsZero() || call.ActionName == "" {
		return DelegatedOutcome{}, errs.New(errs.CodeValidationFailed, "agent: a delegated request must name its run and action").
			WithField("reason", DelegatedReasonInvalidRequest)
	}
	if call.PredictionID == "" {
		return DelegatedOutcome{}, errs.New(errs.CodeValidationFailed,
			"agent: a delegated request needs the prediction that preceded it").
			WithField("prediction_id", "required").WithField("reason", DelegatedReasonPredictionRequired)
	}
	if ok, reason, detail := DelegationAdmitted(e.authority); !ok {
		return DelegatedOutcome{}, errs.New(errs.CodeEffectForbidden, "agent: "+detail).WithField("reason", reason)
	}
	account, err := accounts.ParseAccountID(e.authority.AccountID())
	if err != nil {
		return DelegatedOutcome{}, errs.Wrap(err, errs.CodeInternal, "agent: the authority's account is not an account id")
	}
	nonce := DelegatedNonce(call.RunID, call.ActionName)

	switch call.Kind {
	case ActionRequestDelegatedSwap:
		if call.Swap == nil || call.Withdrawal != nil {
			return DelegatedOutcome{}, errs.New(errs.CodeValidationFailed, "agent: a REQUEST_DELEGATED_SWAP decision carries exactly a swap request").
				WithField("reason", DelegatedReasonInvalidRequest)
		}
		tool, held := toolset.Lookup(call.Kind, call.Swap.CapabilityID)
		if !held {
			return DelegatedOutcome{}, toolUnavailable(call.Kind, call.Swap.CapabilityID, toolset)
		}
		req, err := swapRequestFor(*call.Swap, tool, account, nonce, call.CorrelationID)
		if err != nil {
			return DelegatedOutcome{}, err
		}
		if err := e.admitNow(ctx, tx, call.RunID); err != nil {
			return DelegatedOutcome{}, err
		}
		ticket, err := e.deps.Authority.RequestSwap(ctx, tx, req)
		if err != nil {
			return DelegatedOutcome{}, err
		}
		out := DelegatedOutcome{CapabilityID: tool.CapabilityID, Nonce: nonce, SubjectKind: string(delegation.SubjectSwap),
			Replay: ticket.Replay, Refusal: ticket.Refusal}
		if ticket.Refusal == nil {
			out.SubjectID, out.Status, out.IntentID = ticket.Swap.ID.String(), string(ticket.Swap.Status), ticket.Swap.IntentID.String()
		}
		return out, nil

	case ActionProposeDelegatedWithdrawal:
		if call.Withdrawal == nil || call.Swap != nil {
			return DelegatedOutcome{}, errs.New(errs.CodeValidationFailed, "agent: a PROPOSE_DELEGATED_WITHDRAWAL decision carries exactly a withdrawal proposal").
				WithField("reason", DelegatedReasonInvalidRequest)
		}
		tool, held := toolset.Lookup(call.Kind, call.Withdrawal.CapabilityID)
		if !held {
			return DelegatedOutcome{}, toolUnavailable(call.Kind, call.Withdrawal.CapabilityID, toolset)
		}
		req, err := withdrawalRequestFor(*call.Withdrawal, tool, account, nonce, call.CorrelationID)
		if err != nil {
			return DelegatedOutcome{}, err
		}
		if err := e.admitNow(ctx, tx, call.RunID); err != nil {
			return DelegatedOutcome{}, err
		}
		ticket, err := e.deps.Authority.ProposeWithdrawal(ctx, tx, req)
		if err != nil {
			return DelegatedOutcome{}, err
		}
		out := DelegatedOutcome{CapabilityID: tool.CapabilityID, Nonce: nonce, SubjectKind: string(delegation.SubjectWithdrawal),
			Replay: ticket.Replay, Refusal: ticket.Refusal}
		if ticket.Refusal == nil {
			out.SubjectID, out.Status, out.IntentID = ticket.Withdrawal.ID.String(), string(ticket.Withdrawal.Status), ticket.Withdrawal.IntentID.String()
		}
		return out, nil
	}
	return DelegatedOutcome{}, errs.Newf(errs.CodeValidationFailed, "agent: %s is not a delegated action", call.Kind).
		WithField("reason", DelegatedReasonInvalidRequest)
}

// requireAgent is IntentEmitter's principal rule: this run's AGENT, holding
// the agent intent permission, on its one bound account.
func (e *DelegatedEmitter) requireAgent(ctx context.Context) error {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return errs.New(errs.CodeUnauthenticated, "agent: a delegated request requires a principal")
	}
	if p.ActorType != security.ActorAgent {
		return errs.Newf(errs.CodeForbidden, "agent: the delegated path is only for AGENT principals, not %s", p.ActorType).
			WithField("actor_type", string(p.ActorType))
	}
	if p.SubjectID != e.authority.AgentID().String() {
		return errs.New(errs.CodeForbidden, "agent: principal is a different agent").WithField("subject_id", p.SubjectID)
	}
	if err := security.RequireAt(ctx, security.PermIntentCreateAgent, e.deps.Clock.Now); err != nil {
		return authError(err)
	}
	if err := security.RequireAccount(ctx, e.authority.AccountID()); err != nil {
		return authError(err)
	}
	return nil
}

// admitNow applies the bounds that are judged at the instant of the request,
// inside its transaction: the pause, then the order-rate budget, read under a
// per-agent lock on its delegated requests so two concurrent runs cannot
// spend one allowance twice. The lock is not the broker's, which is held
// across a provider call.
func (e *DelegatedEmitter) admitNow(ctx context.Context, tx pgx.Tx, runID RunID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "agent-delegated:"+e.authority.AgentID().String()); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agent: lock agent for a delegated request")
	}
	if _, paused, err := e.deps.Pauses.OpenPause(ctx, tx, e.authority.AgentID()); err != nil {
		return err
	} else if paused {
		return errs.New(errs.CodeKillSwitchActive, "agent: paused; no delegated request is made").
			WithField("agent_id", e.authority.AgentID().String()).WithField("reason", DelegatedReasonAgentPaused)
	}
	snap, err := e.deps.Budgets.Snapshot(ctx, tx, e.authority, runID, e.deps.Clock.Now())
	if err != nil {
		return err
	}
	if err := snap.CheckIntent(); err != nil {
		var ee *errs.Error
		if errors.As(err, &ee) {
			return ee.WithField("reason", DelegatedReasonOrderRate)
		}
		return err
	}
	return nil
}

func toolUnavailable(kind ir.ActionKind, capabilityID string, toolset DelegatedToolset) error {
	detail := "the run holds no " + string(kind) + " tool under capability " + strings.TrimSpace(capabilityID)
	if toolset.Reason != "" {
		detail += ": " + toolset.Detail
	}
	return errs.New(errs.CodeEffectForbidden, "agent: "+detail).WithField("reason", DelegatedReasonToolUnavailable)
}

func stringsOf[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	return sortedStrings(out)
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
