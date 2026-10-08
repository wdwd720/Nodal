package delegation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Withdrawal refusal reasons beyond the policy's own codes.
const (
	ReasonApprovalExpired     = "DELEGATION_APPROVAL_EXPIRED"
	ReasonPolicyMissing       = "DELEGATION_WITHDRAWAL_POLICY_MISSING"
	RefusalAccountFrozen      = "ACCOUNT_FROZEN"
	RefusalPolicyRejected     = "WITHDRAWAL_POLICY_REJECTED"
	RefusalCancelledByOwner   = "CANCELLED_BY_OWNER"
	withdrawalLockKeyPrefix   = "delegation/withdrawals/"
	withdrawalEvidencePrefix  = "delegation/withdrawal/"
	evaluationEvidencePrefix  = "delegation/withdrawal-evaluation/"
	withdrawalEffectKeyPrefix = "delegation:withdrawal:"
)

// --- Destinations ------------------------------------------------------------------

// DestinationRequest allowlists a withdrawal destination for an account.
type DestinationRequest struct {
	AccountID accounts.AccountID
	Chain     devnet.ChainID
	Address   devnet.PublicKey
	Label     string
}

// AddDestination allowlists a destination: the account's owner, strongly
// authenticated. The destination's age starts now.
func (s *Service) AddDestination(ctx context.Context, tx pgx.Tx, req DestinationRequest) (Destination, error) {
	label := strings.TrimSpace(req.Label)
	if !contains(Chains(), req.Chain) || req.Address.IsZero() || label == "" || len(label) > 120 {
		return Destination{}, errs.New(errs.CodeValidationFailed, "delegation: a destination needs a chain, an address and a label")
	}
	p, err := s.ownerFor(ctx, tx, req.AccountID, s.params.StepUpMaxAge)
	if err != nil {
		return Destination{}, err
	}
	if s.signer.PublicKey(req.AccountID, req.Chain) == req.Address {
		return Destination{}, errs.New(errs.CodeValidationFailed, "delegation: the delegated wallet cannot be its own destination")
	}
	did := id.New[destinationKind]()
	now := dbTime(s.clk.Now())
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_destinations (id, account_id, chain_id, address, label, status,
		added_by_actor_type, added_by_actor_id, added_at) VALUES ($1,$2,$3,$4,$5,'ACTIVE','USER',$6,$7)`,
		did, req.AccountID, req.Chain, req.Address.String(), label, p.SubjectID, now); err != nil {
		if db.IsUniqueViolation(err) {
			return Destination{}, errs.Wrap(err, errs.CodeConflict, "delegation: this destination is already allowlisted")
		}
		return Destination{}, dbErr("allowlist destination", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_destination_transitions (id, destination_id, from_status, to_status,
		actor_type, actor_id, reason) VALUES ($1,$2,'NONE','ACTIVE','USER',$3,'allowlisted')`, id.New[id.Any](), did, p.SubjectID); err != nil {
		return Destination{}, dbErr("record destination", err)
	}
	if err := s.auditEvent(ctx, tx, Actor{Type: security.ActorUser, ID: p.SubjectID}, "delegation.destination.added",
		"delegated_withdrawal_destination", did.String(), req.AccountID, "destination allowlisted",
		map[string]string{"destination_id": did.String(), "chain": string(req.Chain), "address": req.Address.String()}); err != nil {
		return Destination{}, err
	}
	return getDestination(ctx, tx, did, false)
}

// RemoveDestination removes a destination from the allowlist: the account's
// owner or an operator. Nothing not yet signed can go to it afterwards.
func (s *Service) RemoveDestination(ctx context.Context, tx pgx.Tx, did DestinationID, reason string) (Destination, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 500 {
		return Destination{}, errs.New(errs.CodeValidationFailed, "delegation: a removal needs a reason of 1..500 characters")
	}
	d, err := getDestination(ctx, tx, did, true)
	if err != nil {
		return Destination{}, err
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return Destination{}, errs.New(errs.CodeUnauthenticated, "delegation: a removal needs an authenticated customer or operator")
	}
	actor := Actor{Type: security.ActorOperator, ID: p.SubjectID}
	switch {
	case p.IsAgent():
		return Destination{}, errs.New(errs.CodeForbidden, "delegation: an agent never edits the allowlist")
	case p.ActorType != security.ActorOperator:
		if _, err := s.ownerFor(ctx, tx, d.AccountID, s.params.StepUpMaxAge); err != nil {
			return Destination{}, err
		}
		actor.Type = security.ActorUser
	}
	if d.Status == DestinationRemoved {
		return d, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_destination_transitions (id, destination_id, from_status, to_status,
		actor_type, actor_id, reason) VALUES ($1,$2,'ACTIVE','REMOVED',$3,$4,$5)`, id.New[id.Any](), d.ID, actor.Type, actor.ID, reason); err != nil {
		return Destination{}, dbErr("record destination removal", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE delegated_withdrawal_destinations SET status = 'REMOVED', removed_by_actor_type = $2,
		removed_by_actor_id = $3, removed_at = $4 WHERE id = $1`, d.ID, actor.Type, actor.ID, dbTime(s.clk.Now())); err != nil {
		return Destination{}, dbErr("remove destination", err)
	}
	if err := s.auditEvent(ctx, tx, actor, "delegation.destination.removed", "delegated_withdrawal_destination", d.ID.String(),
		d.AccountID, reason, map[string]string{"destination_id": d.ID.String()}); err != nil {
		return Destination{}, err
	}
	return getDestination(ctx, tx, d.ID, false)
}

// --- Policies ----------------------------------------------------------------------

// SetWithdrawalPolicy writes the account's next withdrawal policy version:
// the account's owner, strongly authenticated. Versions are immutable; the
// latest one applies to every new proposal.
func (s *Service) SetWithdrawalPolicy(ctx context.Context, tx pgx.Tx, p WithdrawalPolicy) (WithdrawalPolicy, error) {
	if err := p.Validate(); err != nil {
		return WithdrawalPolicy{}, err
	}
	owner, err := s.ownerFor(ctx, tx, p.AccountID, s.params.StepUpMaxAge)
	if err != nil {
		return WithdrawalPolicy{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, withdrawalLockKeyPrefix+p.AccountID.String()); err != nil {
		return WithdrawalPolicy{}, dbErr("lock account withdrawals", err)
	}
	latest, found, err := latestPolicy(ctx, tx, p.AccountID)
	if err != nil {
		return WithdrawalPolicy{}, err
	}
	p.Version = 1
	if found {
		p.Version = latest.Version + 1
	}
	p.ID = id.New[policyKind]()
	if p.PolicyHash, err = p.ComputeHash(); err != nil {
		return WithdrawalPolicy{}, err
	}
	now := dbTime(s.clk.Now())
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_policies (id, account_id, version, auto_approve_enabled,
		auto_approve_max_minor, auto_approve_max_risk_score, max_risk_score, per_withdrawal_limit_minor, daily_limit_minor,
		velocity_max_count, min_destination_age_seconds, min_account_age_seconds, cooling_period_seconds, strong_auth_required,
		strong_auth_max_age_seconds, policy_hash, created_by_actor_type, created_by_actor_id, effective_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'USER',$17,$18)`,
		p.ID, p.AccountID, p.Version, p.AutoApproveEnabled, p.AutoApproveMax.Minor(), p.AutoApproveMaxRiskScore, p.MaxRiskScore,
		p.PerWithdrawalLimit.Minor(), p.DailyLimit.Minor(), p.VelocityMaxCount, int64(p.MinDestinationAge/time.Second),
		int64(p.MinAccountAge/time.Second), int64(p.CoolingPeriod/time.Second), p.StrongAuthRequired,
		int64(p.StrongAuthMaxAge/time.Second), p.PolicyHash[:], owner.SubjectID, now); err != nil {
		return WithdrawalPolicy{}, dbErr("record withdrawal policy", err)
	}
	if err := s.auditEvent(ctx, tx, Actor{Type: security.ActorUser, ID: owner.SubjectID}, "delegation.withdrawal_policy.set",
		"delegated_withdrawal_policy", p.ID.String(), p.AccountID, "withdrawal policy version "+strconv.Itoa(p.Version),
		map[string]string{"policy_id": p.ID.String(), "version": strconv.Itoa(p.Version), "policy_hash": hex.EncodeToString(p.PolicyHash[:])}); err != nil {
		return WithdrawalPolicy{}, err
	}
	return getPolicy(ctx, tx, p.ID)
}

// --- Proposals -----------------------------------------------------------------------

// ProposalRequest is an agent's proposal to withdraw Amount of an asset
// from the account's delegated wallet to an allowlisted destination.
type ProposalRequest struct {
	CapabilityID  CapabilityID
	AccountID     accounts.AccountID
	Nonce         uint64
	DestinationID DestinationID
	AssetID       assets.AssetID
	Amount        money.Quantity
	Jurisdiction  string
	CorrelationID string
}

func (r ProposalRequest) validate() error {
	var problems []string
	if r.CapabilityID.IsZero() || r.AccountID.IsZero() || r.DestinationID.IsZero() || r.AssetID.IsZero() {
		problems = append(problems, "capability, account, destination and asset are required")
	}
	if r.Nonce == 0 {
		problems = append(problems, "the proposal nonce must be positive")
	}
	if !r.Amount.IsPositive() {
		problems = append(problems, "the amount must be positive")
	} else if _, err := devnet.AmountOf(r.Amount); err != nil {
		problems = append(problems, "the amount must fit a devnet amount (u64)")
	}
	if len(r.CorrelationID) > 200 || len(r.Jurisdiction) > 32 {
		problems = append(problems, "correlation id and jurisdiction are bounded")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "delegation: invalid withdrawal proposal").WithField("problems", problems)
	}
	return nil
}

// WithdrawalTicket is what ProposeWithdrawal recorded. A Refusal is recorded
// in the transaction and the caller commits it.
type WithdrawalTicket struct {
	Withdrawal Withdrawal
	Replay     bool
	Refusal    error
}

type withdrawalTerms struct {
	DestinationID      string `json:"destination_id"`
	DestinationAddress string `json:"destination_address"`
	Chain              string `json:"chain"`
	ProposalNonce      string `json:"proposal_nonce"`
	ProposedByAgent    string `json:"proposed_by_agent"`
}

// ProposeWithdrawal records an agent's withdrawal proposal and everything
// that follows from it in one transaction: proposal -> reservation -> policy
// evaluation, and the policy's auto-approval in low-risk test mode. The
// WITHDRAW intent is the withdrawal service's, never the agent's: the
// settlement compiler forbids an agent to withdraw at every level.
func (s *admitter) ProposeWithdrawal(ctx context.Context, tx pgx.Tx, req ProposalRequest) (WithdrawalTicket, error) {
	if err := req.validate(); err != nil {
		return WithdrawalTicket{}, err
	}
	_, agent, err := s.agentFor(ctx, tx, req.AccountID)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	if !agent.usable() {
		return WithdrawalTicket{}, errs.New(errs.CodeForbidden, "delegation: the agent is "+agent.State).WithField("reason", ReasonAgentNotActive)
	}
	c, err := getCapability(ctx, tx, req.CapabilityID, true)
	if err != nil && !errs.HasCode(err, errs.CodeNotFound) {
		return WithdrawalTicket{}, err
	}
	if err != nil || c.Terms.AccountID != req.AccountID || !strings.EqualFold(c.Terms.AgentID, agent.ID) {
		return WithdrawalTicket{}, errs.New(errs.CodeForbidden, "delegation: the agent holds no such capability on this account").
			WithField("reason", ReasonAccountMismatch)
	}
	if err := s.authorizer.Require(ctx, tx, gates.AgentWithdrawals, c.Terms.Mode); err != nil {
		return WithdrawalTicket{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, withdrawalLockKeyPrefix+req.AccountID.String()); err != nil {
		return WithdrawalTicket{}, dbErr("lock account withdrawals", err)
	}
	dest, err := getDestination(ctx, tx, req.DestinationID, false)
	if err != nil || dest.AccountID != req.AccountID {
		return WithdrawalTicket{}, errs.New(errs.CodeNotFound, "delegation: no such destination on this account").
			WithField("reason", ReasonWithdrawalDestinationNotAllowlisted)
	}
	asset, err := loadAsset(ctx, tx, req.AssetID)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	mint, _, err := devnetMints(dest.Chain, asset, asset)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	// SANDBOX_WITHDRAWAL is refused for a supply-controlled asset (supply.go):
	// refused before anything is written, so the chain never moves units the
	// books could not record leaving.
	if err := refuseSupplyControlledWithdrawal(ctx, tx, asset.ID); err != nil {
		return WithdrawalTicket{}, err
	}
	policy, found, err := latestPolicy(ctx, tx, req.AccountID)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	if !found {
		return WithdrawalTicket{}, errs.New(errs.CodeValidationFailed, "delegation: the account has no withdrawal policy; nothing is withdrawn without one").
			WithField("reason", ReasonPolicyMissing)
	}
	wallet, err := admittedWallet(ctx, tx, req.AccountID, dest.Chain)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	now := dbTime(s.clk.Now())
	terms, err := json.Marshal(withdrawalTerms{
		DestinationID: dest.ID.String(), DestinationAddress: dest.Address.String(),
		Chain: string(dest.Chain), ProposalNonce: strconv.FormatUint(req.Nonce, 10), ProposedByAgent: agent.ID,
	})
	if err != nil {
		return WithdrawalTicket{}, errs.Wrap(err, errs.CodeInternal, "delegation: encode terms")
	}
	amount := req.Amount
	in, existing, err := s.intents.Submit(ctx, tx, econ.Intent{
		AccountID: req.AccountID, Kind: econ.KindWithdraw, Mode: c.Terms.Mode, Capability: gates.AgentWithdrawals,
		ActorType: withdrawActor.Type, ActorID: withdrawActor.ID, StrategyRef: c.Terms.StrategyID, DelegationID: c.ID.String(),
		AssetID: &asset.ID, Quantity: &amount, Terms: terms,
		IdempotencyKey: "delegation:withdrawal:" + c.ID.String() + ":" + strconv.FormatUint(req.Nonce, 10), CorrelationID: req.CorrelationID,
	})
	if err != nil {
		return WithdrawalTicket{}, err
	}
	if existing {
		return s.replayWithdrawal(ctx, tx, in)
	}
	refuse := func(code errs.Code, reason, detail string) (WithdrawalTicket, error) {
		rerr := refused(code, reason, detail)
		if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, withdrawActor, boundReason(rerr.Error()), reason); err != nil {
			return WithdrawalTicket{}, err
		}
		if err := s.auditEvent(ctx, tx, Actor{Type: security.ActorAgent, ID: agent.ID}, "delegation.withdrawal.refused",
			"economic_intent", in.ID.String(), req.AccountID, reason, map[string]string{"intent_id": in.ID.String(), "reason": reason}); err != nil {
			return WithdrawalTicket{}, err
		}
		return WithdrawalTicket{Refusal: rerr}, nil
	}

	rt, err := econ.Route(econ.RouteRequest{
		Intent:       in,
		Subject:      settlement.Subject{Type: settlement.SubjectDestination, ID: dest.ID.String(), AssetID: asset.ID.String()},
		Jurisdiction: req.Jurisdiction, Verification: valuedomain.VerificationNone,
	}, s.router, now)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	if !rt.MayExecuteNow() {
		return refuse(errs.CodeForbidden, RefusalRouteDenied, "the settlement compiler refused the withdrawal: "+strings.Join(rt.Reasons, ","))
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRouted, withdrawActor, "routed through the settlement compiler", ""); err != nil {
		return WithdrawalTicket{}, err
	}
	if code, detail, err := s.withdrawalBlocked(ctx, tx, req.AccountID, agent); err != nil {
		return WithdrawalTicket{}, err
	} else if code != "" {
		ecode := errs.CodeKillSwitchActive
		if code == RefusalAccountFrozen {
			ecode = errs.CodeAccountFrozen
		}
		return refuse(ecode, code, detail)
	}
	price, par, err := s.priceOf(ctx, tx, asset, now)
	if err != nil {
		if errs.HasCode(err, errs.CodeStaleMarketData) {
			return refuse(errs.CodeStaleMarketData, RefusalPriceUnavailable, "no fresh price for the asset")
		}
		return WithdrawalTicket{}, err
	}
	notional, err := ValueUSD(amount, asset.Decimals, price, money.RoundUp)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	if !notional.IsPositive() {
		return refuse(errs.CodeValidationFailed, ReasonAmountTooSmall, "the withdrawal is worth nothing at the current price")
	}
	var classes []InstrumentClass
	if cl, ok := ClassifyAsset(asset); ok {
		classes = []InstrumentClass{cl}
	}
	dayUsed, totalUsed, err := usageTotals(ctx, tx, c.ID, now)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	check := c.Check(Use{
		AccountID: req.AccountID, AgentID: agent.ID, StrategyID: agent.StrategyID, Action: ActionProposeWithdrawal,
		Classes: classes, Chain: dest.Chain, Destination: dest.Address, Notional: notional, DayUsed: dayUsed, TotalUsed: totalUsed,
		Environment: s.env, Mode: c.Terms.Mode, Now: now,
	})
	acct, err := s.loadAccount(ctx, tx, req.AccountID)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	facts, err := s.withdrawalFacts(ctx, tx, req.AccountID, notional, now)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	decision, err := s.evaluateWithdrawalRisk(ctx, tx, in, agent, acct, asset, price, amount, notional, dest, c, check, facts, now)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	if rej := decision.RejectionError(); rej != nil {
		return refuse(errs.CodeRiskRejected, RefusalRiskRejected, rej.Error()+" "+strings.Join(check.Codes, ","))
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRiskApproved, withdrawActor, "risk kernel allowed the withdrawal", ""); err != nil {
		return WithdrawalTicket{}, err
	}
	res, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: req.AccountID.String(), AssetID: asset.ID, Quantity: amount, ActorType: withdrawActor.Type, ActorID: withdrawActor.ID,
		EconomicIntentID: in.ID.String(), IdempotencyKey: "delegation:withdrawal:" + in.ID.String(), TTL: s.params.ReservationTTL,
		Reason: "delegated withdrawal: funds held for the proposal",
	})
	if err != nil {
		switch errs.CodeOf(err) {
		case errs.CodeInsufficientBuyingPower, errs.CodeAccountFrozen, errs.CodeAssetRestricted:
			return refuse(errs.CodeOf(err), RefusalInsufficientFunds, err.Error())
		}
		return WithdrawalTicket{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusReserved, withdrawActor, "funds reserved", ""); err != nil {
		return WithdrawalTicket{}, err
	}
	// Policy evaluation, after the reservation: the customer's own rules.
	verified, err := s.identity.IdentityVerified(ctx, tx, req.AccountID)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	sanctionsClear, err := s.sanctionsClear(ctx, tx, acct)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	ev := Evaluate(policy, EvaluationInput{
		Environment: s.env, Now: now, Amount: notional,
		DestinationActive:  dest.Status == DestinationActive && dest.AccountID == req.AccountID,
		DestinationAddedAt: dest.AddedAt, AccountCreatedAt: acct.CreatedAt, IdentityVerified: verified, SanctionsClear: sanctionsClear,
		DayTotal: facts.dayTotal, CountLast24h: facts.count24h, Anomalies: facts.anomalies,
		AgentInScope:      check.Present && check.InScope && !check.Expired && !check.Revoked,
		AgentWithinLimits: check.WithinLimits,
	})
	if ev.Outcome == OutcomeReject {
		if _, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: evaluationEvidencePrefix + in.ID.String(), Document: map[string]any{
			"disclaimer": evidenceDisclaimer, "intent_id": in.ID.String(), "evaluation": ev,
		}}); err != nil {
			return WithdrawalTicket{}, err
		}
		if _, err := s.capital.Release(ctx, tx, res.ID, "withdrawal refused by the account's policy"); err != nil {
			return WithdrawalTicket{}, err
		}
		return refuse(errs.CodeForbidden, ev.ReasonCodes[0], "the account's withdrawal policy refused: "+strings.Join(ev.ReasonCodes, ","))
	}
	wid := id.New[withdrawalKind]()
	usage := newUsageID()
	if err := insertUsage(ctx, tx, usage, c, SubjectWithdrawal, wid.String(), ActionProposeWithdrawal, notional, now); err != nil {
		return WithdrawalTicket{}, err
	}
	fee, ok := HostFees(s.chain)(dest.Chain)
	if !ok {
		return WithdrawalTicket{}, errs.New(errs.CodeValidationFailed, "delegation: the host does not run chain "+string(dest.Chain))
	}
	plan := WithdrawalPlan{
		Version: withdrawalPlanVersion, WithdrawalID: wid.String(), AccountID: req.AccountID.String(), AgentID: agent.ID,
		CapabilityID: c.ID.String(), CapabilityHash: c.HashHex(), IntentID: in.ID.String(),
		ProposalNonce: strconv.FormatUint(req.Nonce, 10), Mode: string(c.Terms.Mode), Chain: string(dest.Chain),
		Wallet: wallet.String(), DestinationID: dest.ID.String(), DestinationAddress: dest.Address.String(),
		AssetID: asset.ID.String(), Mint: mint.String(), Decimals: strconv.Itoa(int(asset.Decimals)), Amount: amount.String(),
		NotionalUSD: notional.String(), Price: planPrice(price, par), PolicyID: policy.ID.String(),
		PolicyVersion: strconv.Itoa(policy.Version), PolicyHash: hex.EncodeToString(policy.PolicyHash[:]),
		EvaluationOutcome: string(ev.Outcome), RiskScore: strconv.Itoa(ev.RiskScore), FeePerTx: strconv.FormatUint(fee, 10),
		ProposedAt: now, ExecutableAt: now.Add(policy.CoolingPeriod), ApprovalExpiresAt: now.Add(s.params.ApprovalTTL),
	}
	planHash, err := plan.Hash()
	if err != nil {
		return WithdrawalTicket{}, err
	}
	planDoc, err := json.Marshal(plan)
	if err != nil {
		return WithdrawalTicket{}, errs.Wrap(err, errs.CodeInternal, "delegation: encode plan")
	}
	evDoc, err := json.Marshal(ev)
	if err != nil {
		return WithdrawalTicket{}, errs.Wrap(err, errs.CodeInternal, "delegation: encode evaluation")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawals (id, capability_id, account_id, agent_id, proposal_nonce, intent_id,
		usage_id, reservation_id, policy_id, destination_id, environment, mode, chain_id, wallet, destination_address, asset_id,
		amount, notional_usd_minor, proposed_at, withdrawal_day, approval_expires_at, executable_at, evaluation_outcome,
		risk_score, evaluation, plan, plan_hash, status, network)
		VALUES ($1,$2,$3,$4::uuid,$5::numeric,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17::numeric,$18,$19,$20::date,$21,$22,$23,
		        $24,$25,$26,$27,'PROPOSED',$28)`,
		wid, c.ID, req.AccountID, agent.ID, strconv.FormatUint(req.Nonce, 10), in.ID, usage, res.ID, policy.ID, dest.ID, s.env,
		c.Terms.Mode, dest.Chain, wallet.String(), dest.Address.String(), asset.ID, amount.String(), notional.Minor(), now,
		now.Format(time.DateOnly), plan.ApprovalExpiresAt, plan.ExecutableAt, ev.Outcome, ev.RiskScore, evDoc, planDoc,
		planHash[:], s.Network()); err != nil {
		return WithdrawalTicket{}, dbErr("record withdrawal", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_transitions (id, withdrawal_id, from_status, to_status, actor_type,
		actor_id, reason) VALUES ($1,$2,'NONE','PROPOSED',$3,$4,'proposed by the agent')`, id.New[id.Any](), wid, security.ActorAgent, agent.ID); err != nil {
		return WithdrawalTicket{}, dbErr("record withdrawal proposal", err)
	}
	if err := s.capital.LockForOrder(ctx, tx, res.ID, wid.String()); err != nil {
		return WithdrawalTicket{}, err
	}
	w, err := getWithdrawal(ctx, tx, wid, true)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	if ev.Outcome == OutcomeAutoApprove {
		if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_approvals (id, withdrawal_id, decision, plan_hash,
			approver_actor_type, approver_actor_id, decided_at, reason) VALUES ($1,$2,'APPROVE',$3,$4,$5,$6,$7)`,
			id.New[id.Any](), wid, planHash[:], policyActor.Type, policyActor.ID, now,
			"auto-approved in low-risk test mode under policy version "+strconv.Itoa(policy.Version)); err != nil {
			return WithdrawalTicket{}, dbErr("record auto-approval", err)
		}
		w, err = s.transitionWithdrawal(ctx, tx, w, WithdrawalApproved, policyActor, "auto-approved by the account's policy", subjectSet{})
	} else {
		w, err = s.transitionWithdrawal(ctx, tx, w, WithdrawalAwaitingApproval, policyActor,
			"the account's policy requires the owner: "+strings.Join(ev.ReasonCodes, ","), subjectSet{})
	}
	if err != nil {
		return WithdrawalTicket{}, err
	}
	if err := s.auditEvent(ctx, tx, Actor{Type: security.ActorAgent, ID: agent.ID}, "delegation.withdrawal.proposed",
		"delegated_withdrawal", wid.String(), req.AccountID, "agent proposed a delegated withdrawal", map[string]string{
			"withdrawal_id": wid.String(), "plan_hash": hex.EncodeToString(planHash[:]), "outcome": string(ev.Outcome),
			"risk_score": strconv.Itoa(ev.RiskScore),
		}); err != nil {
		return WithdrawalTicket{}, err
	}
	return WithdrawalTicket{Withdrawal: w}, nil
}

func (s *admitter) replayWithdrawal(ctx context.Context, tx pgx.Tx, in econ.Intent) (WithdrawalTicket, error) {
	w, found, err := withdrawalByIntent(ctx, tx, in.ID)
	if err != nil {
		return WithdrawalTicket{}, err
	}
	if found {
		return WithdrawalTicket{Withdrawal: w, Replay: true}, nil
	}
	if in.Status == econ.StatusRejected {
		return WithdrawalTicket{Replay: true, Refusal: refused(errs.CodeConflict, in.RejectionCode, "this proposal was refused")}, nil
	}
	return WithdrawalTicket{}, errs.New(errs.CodeInternal, "delegation: an intent without its withdrawal").WithField("intent_id", in.ID.String())
}

// withdrawalBlocked is the freeze check every step before submission makes
// (proposal, approval, signing, the last check before the submission): the
// account must be ACTIVE, no switch that stops withdrawals may apply, and no
// switch that stops the proposing agent's activity -- on its account, the
// agent itself or the strategy version it runs -- may apply either.
func (s *admitter) withdrawalBlocked(ctx context.Context, tx pgx.Tx, account accounts.AccountID, agent agentRow) (string, string, error) {
	acct, err := s.loadAccount(ctx, tx, account)
	if err != nil {
		return "", "", err
	}
	if acct.Status != accounts.StatusActive {
		return RefusalAccountFrozen, "the account is " + string(acct.Status), nil
	}
	actions := []killswitch.Action{
		agent.killAction(killswitch.Withdraw, account, gates.AgentWithdrawals),
		agent.killAction(killswitch.NewRisk, account, gates.AgentWithdrawals),
	}
	for _, a := range actions {
		if err := s.kills.Check(ctx, tx, a); err != nil {
			if errs.CodeOf(err) == errs.CodeKillSwitchActive {
				return RefusalKillSwitch, err.Error(), nil
			}
			return "", "", err
		}
	}
	return "", "", nil
}

// proposingAgent is the agent that proposed w, as it is now. An agent row
// that cannot be found is its id alone, which a switch on the agent still
// matches.
func proposingAgent(ctx context.Context, q db.Querier, w Withdrawal) (agentRow, error) {
	a, found, err := getAgent(ctx, q, w.AgentID)
	if err != nil {
		return agentRow{}, err
	}
	if !found {
		return agentRow{ID: w.AgentID}, nil
	}
	return a, nil
}

// withdrawalFacts are the account's measured withdrawal history.
type withdrawalFacts struct {
	dayTotal   money.USD
	count24h   int
	notional24 money.USD
	anomalies  []Anomaly
	unknown    int
}

// liveStatuses excludes the withdrawals that never left and never will.
const liveWithdrawal = `status NOT IN ('REJECTED','DECLINED','CANCELLED','FAILED')`

func (s *admitter) withdrawalFacts(ctx context.Context, tx pgx.Tx, account accounts.AccountID, amount money.USD, now time.Time) (withdrawalFacts, error) {
	var f withdrawalFacts
	var day, notional24 int64
	if err := tx.QueryRow(ctx, `SELECT
		coalesce(sum(notional_usd_minor) FILTER (WHERE withdrawal_day = $2::date), 0)::bigint,
		count(*) FILTER (WHERE proposed_at > $3 AND proposed_at <= $4),
		coalesce(sum(notional_usd_minor) FILTER (WHERE proposed_at > $3 AND proposed_at <= $4), 0)::bigint
		FROM delegated_withdrawals WHERE account_id = $1 AND `+liveWithdrawal,
		account, now.Format(time.DateOnly), now.Add(-24*time.Hour), now).Scan(&day, &f.count24h, &notional24); err != nil {
		return withdrawalFacts{}, dbErr("measure withdrawals", err)
	}
	f.dayTotal, f.notional24 = money.USDFromMinor(day), money.USDFromMinor(notional24)
	unknown, err := unknownEffects(ctx, tx, account)
	if err != nil {
		return withdrawalFacts{}, err
	}
	f.unknown = unknown
	if unknown > 0 {
		f.anomalies = append(f.anomalies, AnomalyUnresolvedEffects)
	}
	var declined bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM delegated_withdrawal_approvals a
		JOIN delegated_withdrawals w ON w.id = a.withdrawal_id
		WHERE w.account_id = $1 AND a.decision = 'DECLINE' AND a.decided_at > $2)`, account, now.Add(-24*time.Hour)).Scan(&declined); err != nil {
		return withdrawalFacts{}, dbErr("measure declines", err)
	}
	if declined {
		f.anomalies = append(f.anomalies, AnomalyRecentDecline)
	}
	var largest *int64
	if err := tx.QueryRow(ctx, `SELECT max(notional_usd_minor) FROM delegated_withdrawals
		WHERE account_id = $1 AND status = 'SETTLED' AND proposed_at > $2`, account, now.Add(-30*24*time.Hour)).Scan(&largest); err != nil {
		return withdrawalFacts{}, dbErr("measure settled withdrawals", err)
	}
	if largest != nil && *largest > 0 {
		ten, err := money.USDFromMinor(*largest).MulInt64(10)
		if err != nil || amount.Cmp(ten) > 0 {
			f.anomalies = append(f.anomalies, AnomalyAmountSpike)
		}
	}
	return f, nil
}

func (s *admitter) sanctionsClear(ctx context.Context, q db.Querier, acct accounts.Account) (bool, error) {
	p, err := s.profiles.Get(ctx, q, acct.OwnerUserID)
	if err != nil {
		if errs.HasCode(err, errs.CodeNotFound) {
			return false, nil
		}
		return false, err
	}
	return p.SanctionsState == compliance.SanctionsClear, nil
}

// evaluateWithdrawalRisk runs Risk V2 with every section WITHDRAW requires
// (account, identity, balance, velocity, destination, chain, settlement,
// strategy) plus the agent's capability as the authority section, and
// persists the decision whatever it says. A withdrawal is risk-increasing:
// value leaves, so an account freeze stops it in the kernel too.
func (s *admitter) evaluateWithdrawalRisk(ctx context.Context, tx pgx.Tx, in econ.Intent, agent agentRow, acct accounts.Account,
	asset assets.Asset, price money.Price, amount money.Quantity, notional money.USD, dest Destination, c Capability,
	check AuthorityCheck, facts withdrawalFacts, now time.Time,
) (risk.AdvancedDecision, error) {
	verified, err := s.identity.IdentityVerified(ctx, tx, in.AccountID)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	avail, err := s.capital.Availability(ctx, tx, in.AccountID.String(), asset.ID, now)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	available, err := ValueUSD(avail.Available, asset.Decimals, price, money.RoundDown)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	required, err := ValueUSD(amount, asset.Decimals, price, money.RoundDown)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	approved, err := strategyActive(ctx, tx, agent.StrategyID)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	kills, err := s.killSwitches(ctx, tx)
	if err != nil {
		return risk.AdvancedDecision{}, err
	}
	class, _ := ClassifyAsset(asset)
	input := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{
			ID: in.ID.String(), Kind: string(in.Kind), AccountID: in.AccountID.String(),
			ActorType: string(in.ActorType), AgentID: agent.ID, StrategyRef: in.StrategyRef,
			StrategyVersionID: agent.StrategyVersionID, Capability: string(in.Capability), Mode: string(in.Mode),
			InstrumentKind: string(class), NotionalUSD: notional, RiskIncreasing: true,
		},
		Identity:     &risk.IdentitySnapshot{Verified: verified},
		Account:      &risk.AdvancedAccountSnapshot{Status: string(acct.Status)},
		Balance:      &risk.BalanceSnapshot{Available: available, Required: required},
		Velocity:     &risk.VelocitySnapshot{CountInWindow: facts.count24h, NotionalInWindow: facts.notional24},
		Destination:  &risk.DestinationSnapshot{Allowlisted: dest.Status == DestinationActive && c.Terms.AllowsDestination(dest.Address), AddedAt: dest.AddedAt},
		Chain:        &risk.ChainSnapshot{Healthy: s.chain.Health(ctx, dest.Chain) == nil},
		Settlement:   &risk.SettlementSnapshot{UnknownEffects: facts.unknown},
		Authority:    check.Snapshot(),
		Strategy:     &risk.StrategySnapshot{Approved: approved && agent.usable(), ManifestPermits: check.InScope},
		KillSwitches: kills,
		Now:          now,
	}
	decision := risk.EvaluateAdvanced(s.policy, input)
	if _, err := s.risk.RecordAdvancedDecision(ctx, tx, input, decision, in.CorrelationID); err != nil {
		return risk.AdvancedDecision{}, err
	}
	return decision, nil
}

// transitionWithdrawal moves a withdrawal and writes the transition row, the
// set-once columns and the audit event.
func (s *admitter) transitionWithdrawal(ctx context.Context, tx pgx.Tx, w Withdrawal, to WithdrawalStatus, actor Actor, reason string,
	set subjectSet,
) (Withdrawal, error) {
	if !CanTransitionWithdrawal(w.Status, to) {
		return Withdrawal{}, errs.Newf(errs.CodeInvalidStateTransition, "delegation: withdrawal %s cannot move %s -> %s", w.ID, w.Status, to)
	}
	reason = boundReason(reason)
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_transitions (id, withdrawal_id, from_status, to_status, actor_type,
		actor_id, reason) VALUES ($1,$2,$3,$4,$5,$6,$7)`, id.New[id.Any](), w.ID, w.Status, to, actor.Type, actor.ID, reason); err != nil {
		return Withdrawal{}, dbErr("record withdrawal transition", err)
	}
	args, err := set.setArgs()
	if err != nil {
		return Withdrawal{}, err
	}
	out, err := scanWithdrawal(tx.QueryRow(ctx, `UPDATE delegated_withdrawals SET `+subjectSetSQL+` WHERE id = $1 RETURNING `+withdrawalColumns,
		append([]any{w.ID, to}, args...)...))
	if err != nil {
		return Withdrawal{}, dbErr("move withdrawal", err)
	}
	if err := s.auditEvent(ctx, tx, actor, "delegation.withdrawal."+strings.ToLower(string(to)), "delegated_withdrawal", w.ID.String(),
		w.AccountID, reason, map[string]string{
			"withdrawal_id": w.ID.String(), "from": string(w.Status), "to": string(to),
			"tx_id": string(out.TxID), "failure_code": out.FailureCode,
		}); err != nil {
		return Withdrawal{}, err
	}
	return out, nil
}

func (s *Service) setWithdrawalColumns(ctx context.Context, tx pgx.Tx, w Withdrawal, set subjectSet) (Withdrawal, error) {
	if set.empty() {
		return w, nil
	}
	args, err := set.setArgs()
	if err != nil {
		return Withdrawal{}, err
	}
	// A nil status ($2) keeps the stored one: this writes set-once columns only.
	out, err := scanWithdrawal(tx.QueryRow(ctx, `UPDATE delegated_withdrawals SET `+subjectSetSQL+` WHERE id = $1 RETURNING `+withdrawalColumns,
		append([]any{w.ID, nil}, args...)...))
	if err != nil {
		return Withdrawal{}, dbErr("record compiled withdrawal", err)
	}
	return out, nil
}

// --- Decisions -----------------------------------------------------------------------

// WithdrawalDecision is the account owner's answer for a withdrawal awaiting
// approval. PlanHash is the plan the owner was shown.
type WithdrawalDecision struct {
	WithdrawalID WithdrawalID
	PlanHash     [32]byte
	Approve      bool
	Reason       string
}

// DecideWithdrawal records the owner's decision, bound to the plan hash.
// When the policy requires strong authentication the owner must have
// authenticated within its window. A frozen account cannot approve (nothing
// changes; the withdrawal waits or expires); a decline returns the funds.
func (s *Service) DecideWithdrawal(ctx context.Context, tx pgx.Tx, d WithdrawalDecision) (Withdrawal, error) {
	reason := strings.TrimSpace(d.Reason)
	if reason == "" || len(reason) > 500 {
		return Withdrawal{}, errs.New(errs.CodeValidationFailed, "delegation: a decision needs a reason of 1..500 characters")
	}
	w, err := getWithdrawal(ctx, tx, d.WithdrawalID, true)
	if err != nil {
		return Withdrawal{}, err
	}
	policy, err := getPolicy(ctx, tx, w.PolicyID)
	if err != nil {
		return Withdrawal{}, err
	}
	stepUp := time.Duration(0)
	if policy.StrongAuthRequired {
		stepUp = policy.StrongAuthMaxAge
	}
	p, err := s.ownerFor(ctx, tx, w.AccountID, stepUp)
	if err != nil {
		return Withdrawal{}, err
	}
	if w.Status != WithdrawalAwaitingApproval {
		return Withdrawal{}, errs.Newf(errs.CodeInvalidStateTransition, "delegation: withdrawal %s is %s, not awaiting approval", w.ID, w.Status)
	}
	if d.PlanHash != w.PlanHash {
		return Withdrawal{}, errs.New(errs.CodeConflict, "delegation: the decision names a plan other than the withdrawal's").
			WithField("reason", ReasonApprovalPlanMismatch)
	}
	now := dbTime(s.clk.Now())
	if !now.Before(w.ApprovalExpiresAt) {
		return Withdrawal{}, errs.New(errs.CodeConflict, "delegation: the withdrawal's approval window has closed").
			WithField("reason", ReasonApprovalExpired)
	}
	if d.Approve {
		if err := s.authorizer.Require(ctx, tx, gates.AgentWithdrawals, w.Mode); err != nil {
			return Withdrawal{}, err
		}
		agent, err := proposingAgent(ctx, tx, w)
		if err != nil {
			return Withdrawal{}, err
		}
		if code, detail, err := s.withdrawalBlocked(ctx, tx, w.AccountID, agent); err != nil {
			return Withdrawal{}, err
		} else if code != "" {
			ecode := errs.CodeKillSwitchActive
			if code == RefusalAccountFrozen {
				ecode = errs.CodeAccountFrozen
			}
			return Withdrawal{}, refused(ecode, code, "the withdrawal cannot be approved now: "+detail)
		}
	}
	decision := DecisionDecline
	if d.Approve {
		decision = DecisionApprove
	}
	owner := Actor{Type: security.ActorUser, ID: p.SubjectID}
	if _, err := tx.Exec(ctx, `INSERT INTO delegated_withdrawal_approvals (id, withdrawal_id, decision, plan_hash,
		approver_actor_type, approver_actor_id, authenticated_at, decided_at, reason) VALUES ($1,$2,$3,$4,'USER',$5,$6,$7,$8)`,
		id.New[id.Any](), w.ID, decision, w.PlanHash[:], p.SubjectID, p.AuthTime.UTC(), now, reason); err != nil {
		return Withdrawal{}, dbErr("record withdrawal decision", err)
	}
	if d.Approve {
		return s.transitionWithdrawal(ctx, tx, w, WithdrawalApproved, owner, reason, subjectSet{})
	}
	if _, err := s.capital.Release(ctx, tx, w.ReservationID, "delegated withdrawal declined by the account owner"); err != nil {
		return Withdrawal{}, err
	}
	if err := s.moveIntent(ctx, tx, w.IntentID, econ.StatusCancelled, "declined by the account owner", ""); err != nil {
		return Withdrawal{}, err
	}
	return s.transitionWithdrawal(ctx, tx, w, WithdrawalDeclined, owner, reason, subjectSet{failureCode: RefusalDeclinedByOwner})
}

// CancelWithdrawal lets the account's owner stop a withdrawal that has not
// been signed. Cancelling only reduces risk, so it needs no step-up.
func (s *Service) CancelWithdrawal(ctx context.Context, tx pgx.Tx, wid WithdrawalID, reason string) (Withdrawal, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 500 {
		return Withdrawal{}, errs.New(errs.CodeValidationFailed, "delegation: a cancellation needs a reason of 1..500 characters")
	}
	w, err := getWithdrawal(ctx, tx, wid, true)
	if err != nil {
		return Withdrawal{}, err
	}
	p, err := s.ownerFor(ctx, tx, w.AccountID, 0)
	if err != nil {
		return Withdrawal{}, err
	}
	if w.Status != WithdrawalAwaitingApproval && w.Status != WithdrawalApproved {
		return Withdrawal{}, errs.Newf(errs.CodeInvalidStateTransition, "delegation: withdrawal %s is %s and can no longer be cancelled", w.ID, w.Status)
	}
	if err := s.releaseIfActive(ctx, tx, w.ReservationID, "delegated withdrawal cancelled by the account owner"); err != nil {
		return Withdrawal{}, err
	}
	if err := s.moveIntent(ctx, tx, w.IntentID, econ.StatusCancelled, "cancelled by the account owner", ""); err != nil {
		return Withdrawal{}, err
	}
	return s.transitionWithdrawal(ctx, tx, w, WithdrawalCancelled, Actor{Type: security.ActorUser, ID: p.SubjectID}, reason,
		subjectSet{failureCode: RefusalCancelledByOwner})
}

// --- Signing, submission, settlement ------------------------------------------------

// withdrawalStanding is whether an approved (or awaiting) withdrawal may
// still go ahead: the gate, the freeze, the capability, the agent and the
// destination, now.
func (s *Service) withdrawalStanding(ctx context.Context, tx pgx.Tx, w Withdrawal, now time.Time) (string, string, error) {
	if err := s.authorizer.Require(ctx, tx, gates.AgentWithdrawals, w.Mode); err != nil {
		if e, ok := errs.As(err); ok && (e.Code == errs.CodeCapabilityNotApproved || e.Code == errs.CodeKillSwitchActive) {
			return RefusalGateClosed, err.Error(), nil
		}
		return "", "", err
	}
	agent, found, err := getAgent(ctx, tx, w.AgentID)
	if err != nil {
		return "", "", err
	}
	if !found {
		agent = agentRow{ID: w.AgentID}
	}
	if code, detail, err := s.withdrawalBlocked(ctx, tx, w.AccountID, agent); err != nil || code != "" {
		return code, detail, err
	}
	if !found || !agent.usable() {
		return ReasonAgentNotActive, "the proposing agent may not act now", nil
	}
	dest, err := getDestination(ctx, tx, w.DestinationID, false)
	if err != nil {
		return "", "", err
	}
	if dest.Status != DestinationActive {
		return ReasonDestinationRemoved, "the destination was removed from the allowlist", nil
	}
	c, err := getCapability(ctx, tx, w.CapabilityID, false)
	if err != nil {
		return "", "", err
	}
	asset, err := loadAsset(ctx, tx, w.AssetID)
	if err != nil {
		return "", "", err
	}
	var classes []InstrumentClass
	if cl, ok := ClassifyAsset(asset); ok {
		classes = []InstrumentClass{cl}
	}
	dayUsed, totalUsed, err := usageExcluding(ctx, tx, w.CapabilityID, w.UsageID, w.ProposedAt)
	if err != nil {
		return "", "", err
	}
	if check := c.Check(Use{
		AccountID: w.AccountID, AgentID: w.AgentID, StrategyID: agent.StrategyID, Action: ActionProposeWithdrawal,
		Classes: classes, Chain: w.Chain, Destination: w.DestinationAddress, Notional: w.NotionalUSD, DayUsed: dayUsed,
		TotalUsed: totalUsed, Environment: s.env, Mode: w.Mode, Now: now,
	}); !check.OK() {
		return check.Codes[0], "the capability no longer covers the withdrawal: " + strings.Join(check.Codes, ","), nil
	}
	return "", "", nil
}

// rejectWithdrawal ends a withdrawal that will not be signed.
func (s *Service) rejectWithdrawal(ctx context.Context, tx pgx.Tx, w Withdrawal, code, detail string) (Withdrawal, error) {
	if err := s.releaseIfActive(ctx, tx, w.ReservationID, "delegated withdrawal refused: "+code); err != nil {
		return Withdrawal{}, err
	}
	if err := s.moveIntent(ctx, tx, w.IntentID, econ.StatusRejected, boundReason(detail), boundCode(code)); err != nil {
		return Withdrawal{}, err
	}
	return s.transitionWithdrawal(ctx, tx, w, WithdrawalRejected, withdrawActor, detail, subjectSet{failureCode: boundCode(code)})
}

// SignWithdrawal compiles an approved withdrawal's transaction from its
// frozen plan, has the provider decide and sign it, inspects the signed
// bytes again and records them with the claimed effect: SIGNED. Before its
// cooling period has elapsed it refuses without changing anything.
func (s *Service) SignWithdrawal(ctx context.Context, tx pgx.Tx, wid WithdrawalID) (SignOutcome, error) {
	if err := requireExecutor(ctx); err != nil {
		return SignOutcome{}, err
	}
	w, err := getWithdrawal(ctx, tx, wid, true)
	if err != nil {
		return SignOutcome{}, err
	}
	if w.Status != WithdrawalApproved {
		return SignOutcome{Withdrawal: w}, nil
	}
	if err := s.requireNetwork(SubjectWithdrawal, w.ID.String(), w.Network); err != nil {
		return SignOutcome{}, err
	}
	now := dbTime(s.clk.Now())
	refuse := func(code, detail string) (SignOutcome, error) {
		out, err := s.rejectWithdrawal(ctx, tx, w, code, detail)
		if err != nil {
			return SignOutcome{}, err
		}
		return SignOutcome{Withdrawal: out, Refusal: refused(errs.CodeSigningRejected, code, detail)}, nil
	}
	if code, detail, err := s.withdrawalStanding(ctx, tx, w, now); err != nil {
		return SignOutcome{}, err
	} else if code != "" {
		return refuse(code, detail)
	}
	if now.Before(w.ExecutableAt) {
		return SignOutcome{Withdrawal: w}, errs.New(errs.CodeInvalidStateTransition, "delegation: the withdrawal's cooling period has not elapsed").
			WithField("reason", ReasonCoolingPeriod).WithField("executable_at", w.ExecutableAt)
	}
	if v, found, err := decisionOf(ctx, tx, SubjectWithdrawal, w.ID.String()); err != nil {
		return SignOutcome{}, err
	} else if found {
		return refuse(RefusalSigningRejected, "the signing provider already decided this withdrawal ("+string(v)+"); it decides once")
	}
	tip, err := tipOf(ctx, s.host, tx, w.Chain)
	if err != nil {
		return SignOutcome{}, err
	}
	msg, err := w.Plan.Message(tip.Hash)
	if err != nil {
		return refuse(InspectInvalidPlan, "the frozen plan does not compile: "+err.Error())
	}
	msgBytes, err := msg.Bytes()
	if err != nil {
		return refuse(InspectInvalidPlan, "the compiled message does not encode")
	}
	height := tip.Height
	if w, err = s.setWithdrawalColumns(ctx, tx, w, subjectSet{message: msgBytes, recentBlockhash: &tip.Hash, blockhashHeight: &height}); err != nil {
		return SignOutcome{}, err
	}
	decision, err := s.signer.Sign(ctx, tx, SignRequest{
		Kind: SubjectWithdrawal, SubjectID: w.ID.String(), Message: msgBytes,
		MessageSHA256: sha256.Sum256(msgBytes),
	})
	if err != nil {
		return SignOutcome{}, err
	}
	if decision.Verdict != VerdictApproved {
		return refuse(RefusalSigningRejected, "the signing provider refused: "+strings.Join(decision.ReasonCodes, ","))
	}
	raw, err := assemble(msg, decision.Signature)
	if err != nil {
		return SignOutcome{}, err
	}
	c, err := getCapability(ctx, tx, w.CapabilityID, false)
	if err != nil {
		return SignOutcome{}, err
	}
	dest, err := getDestination(ctx, tx, w.DestinationID, false)
	if err != nil {
		return SignOutcome{}, err
	}
	fee, _ := HostFees(s.host)(w.Chain)
	txID, err := InspectWithdrawalTransaction(WithdrawalExpectation{
		Plan: w.Plan, PlanHash: w.PlanHash, Blockhash: tip.Hash,
		Wallet: w.Wallet, AllowedChains: c.Terms.ChainAllowlist, AllowedDestinations: c.Terms.DestinationAllowlist,
		CustomerDestination: dest.Address, ChainFee: fee, MaxFee: s.signer.Policy().MaxFeePerTx,
	}, raw)
	if err != nil {
		return refuse(RefusalSigningRejected, "the signed transaction failed inspection: "+reasonOf(err))
	}
	effect, replayed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: withdrawalEffectKeyPrefix + w.ID.String(), Subsystem: econ.SubsystemWithdrawals, Kind: effectKindSandboxWithdraw,
		IntentID: &w.IntentID, AccountID: w.AccountID.String(), PostsLedger: true, Actor: withdrawActor,
		Request: map[string]string{"withdrawal_id": w.ID.String(), "plan_hash": hex.EncodeToString(w.PlanHash[:]), "tx_id": string(txID)},
	})
	if err != nil {
		return SignOutcome{}, err
	}
	if replayed {
		return SignOutcome{}, errs.New(errs.CodeInternal, "delegation: a withdrawal being signed found its effect already claimed")
	}
	if err := s.moveIntent(ctx, tx, w.IntentID, econ.StatusExecuting, "signed; submitting once", ""); err != nil {
		return SignOutcome{}, err
	}
	out, err := s.transitionWithdrawal(ctx, tx, w, WithdrawalSigned, withdrawActor, "signed by the delegated wallet after inspection",
		subjectSet{signedAt: &now, txID: txID, signedTx: raw, effectID: &effect.ID})
	if err != nil {
		return SignOutcome{}, err
	}
	return SignOutcome{Withdrawal: out, Fresh: true}, nil
}

// GateWithdrawalSubmission is the freeze check right before the one
// submission. A withdrawal it stops is FAILED, never submitted, and its funds
// return.
func (s *Service) GateWithdrawalSubmission(ctx context.Context, tx pgx.Tx, wid WithdrawalID) (Withdrawal, error) {
	if err := requireExecutor(ctx); err != nil {
		return Withdrawal{}, err
	}
	w, err := getWithdrawal(ctx, tx, wid, true)
	if err != nil {
		return Withdrawal{}, err
	}
	if w.Status != WithdrawalSigned {
		return w, errs.Newf(errs.CodeInvalidStateTransition, "delegation: withdrawal %s is %s, not awaiting its submission", w.ID, w.Status)
	}
	code, detail, err := s.withdrawalStanding(ctx, tx, w, dbTime(s.clk.Now()))
	if err != nil {
		return Withdrawal{}, err
	}
	if code == "" {
		return w, nil
	}
	out, err := s.failWithdrawal(ctx, tx, w, BlockedBeforeSubmission(code), "stopped before submission: "+detail)
	if err != nil {
		return Withdrawal{}, err
	}
	return out, refused(errs.CodeAccountFrozen, ReasonBlockedBeforeSubmission, detail)
}

func (s *Service) failWithdrawal(ctx context.Context, tx pgx.Tx, w Withdrawal, code, reason string) (Withdrawal, error) {
	if w.EffectID != nil {
		e, err := s.effects.Get(ctx, tx, *w.EffectID)
		if err != nil {
			return Withdrawal{}, err
		}
		if e.Status == econ.EffectPending || e.Status == econ.EffectUnknown {
			if _, err := s.effects.Fail(ctx, tx, *w.EffectID, boundCode(code), withdrawActor, reason); err != nil {
				return Withdrawal{}, err
			}
		}
	}
	if err := s.releaseIfActive(ctx, tx, w.ReservationID, reason); err != nil {
		return Withdrawal{}, err
	}
	if err := s.finishIntent(ctx, tx, w.IntentID, false, reason); err != nil {
		return Withdrawal{}, err
	}
	return s.transitionWithdrawal(ctx, tx, w, WithdrawalFailed, withdrawActor, reason, subjectSet{failureCode: boundCode(code)})
}

// RecordWithdrawalSubmission records how the one submission ended.
func (s *Service) RecordWithdrawalSubmission(ctx context.Context, tx pgx.Tx, wid WithdrawalID, exec execution) (Withdrawal, error) {
	w, err := getWithdrawal(ctx, tx, wid, true)
	if err != nil || w.Status != WithdrawalSigned {
		return w, err
	}
	switch exec.outcome {
	case execSubmitted:
		return s.transitionWithdrawal(ctx, tx, w, WithdrawalSubmitted, withdrawActor, "the chain accepted the transaction", subjectSet{})
	case execUnknown:
		reason := "the submission's outcome is unknown; it is reconciled by transaction id and never resubmitted"
		if exec.err != nil {
			reason += ": " + exec.err.Error()
		}
		if _, err := s.effects.MarkUnknown(ctx, tx, *w.EffectID, externalRef(w.Chain, w.TxID), withdrawActor, boundReason(reason)); err != nil {
			return Withdrawal{}, err
		}
		if err := s.moveIntent(ctx, tx, w.IntentID, econ.StatusUnknown, boundReason(reason), ""); err != nil {
			return Withdrawal{}, err
		}
		return s.transitionWithdrawal(ctx, tx, w, WithdrawalUnknown, withdrawActor, reason, subjectSet{})
	default:
		return s.failWithdrawal(ctx, tx, w, RefusalSubmissionRefused+":"+string(devnet.ReasonOf(exec.err)), "the chain refused the transaction")
	}
}

// SettleWithdrawal acts on the chain's word for a signed withdrawal:
// FINALIZED and reconciled posts SANDBOX_WITHDRAWAL, consumes the reservation,
// archives the evidence and applies the effect; a final failure or an
// expired, never-landed submission fails it and returns the funds.
func (s *Service) SettleWithdrawal(ctx context.Context, tx pgx.Tx, wid WithdrawalID) (SettleResult, error) {
	if err := requireExecutor(ctx); err != nil {
		return SettleResult{}, err
	}
	w, err := getWithdrawal(ctx, tx, wid, true)
	if err != nil {
		return SettleResult{}, err
	}
	if w.Status != WithdrawalSigned && w.Status != WithdrawalSubmitted && w.Status != WithdrawalUnknown {
		return SettleResult{Withdrawal: w}, nil
	}
	if err := s.requireNetwork(SubjectWithdrawal, w.ID.String(), w.Network); err != nil {
		return SettleResult{}, err
	}
	obs, err := observe(ctx, s.host, tx, w.Chain, w.TxID, w.BlockhashHeight)
	if err != nil {
		return SettleResult{}, err
	}
	switch {
	case obs.finalized():
		match, err := matchesChain(ctx, s.host, w.Chain, w.TxID, w.SignedTx)
		if err != nil {
			return SettleResult{}, err
		}
		if !match {
			if err := s.openChainMismatch(ctx, tx, SubjectWithdrawal, w.ID.String(), w.AccountID, w.Chain, w.TxID); err != nil {
				return SettleResult{}, err
			}
			return SettleResult{Withdrawal: w, Mismatch: true}, nil
		}
		now := dbTime(s.clk.Now())
		posted, err := s.ledger.Post(serviceContext(ctx), tx, WithdrawalPosting(w, now))
		if err != nil {
			return SettleResult{}, err
		}
		if _, err := s.capital.Consume(ctx, tx, w.ReservationID, w.Amount, 0, w.ID.String()); err != nil {
			return SettleResult{}, err
		}
		approval, found, err := withdrawalApproval(ctx, tx, w.ID)
		if err != nil {
			return SettleResult{}, err
		}
		var approvalPtr *approvalRow
		if found {
			approvalPtr = &approval
		}
		signedSum := sha256.Sum256(w.SignedTx)
		ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: withdrawalEvidencePrefix + w.ID.String(), Document: settlementEvidence{
			Disclaimer: evidenceDisclaimer, Kind: string(SubjectWithdrawal), SubjectID: w.ID.String(), CapabilityID: w.CapabilityID.String(),
			IntentID: w.IntentID.String(), Plan: w.Plan, PlanHash: hex.EncodeToString(w.PlanHash[:]), Approval: approvalEvidence(approvalPtr),
			Chain: string(w.Chain), TxID: string(w.TxID), SignedTxSHA256: hex.EncodeToString(signedSum[:]), ObservedState: obs.State,
			ObservedSlot: formatUint(obs.Slot), FinalizedHeight: formatUint(obs.FinalizedHeight), Reconciled: true,
			JournalTransactionID: posted.TransactionID.String(),
		}})
		if err != nil {
			return SettleResult{}, err
		}
		if _, err := s.effects.Apply(ctx, tx, *w.EffectID, econ.ApplyRequest{
			JournalTransactionID: &posted.TransactionID,
			ReservationID:        w.ReservationID.String(), ExternalRef: externalRef(w.Chain, w.TxID), Evidence: &ev, Actor: withdrawActor,
			Reason: "finalized on chain and reconciled",
		}); err != nil {
			return SettleResult{}, err
		}
		if err := s.finishIntent(ctx, tx, w.IntentID, true, "finalized on chain and posted"); err != nil {
			return SettleResult{}, err
		}
		if w.Status == WithdrawalSigned {
			if w, err = s.transitionWithdrawal(ctx, tx, w, WithdrawalSubmitted, withdrawActor, "observed on chain", subjectSet{}); err != nil {
				return SettleResult{}, err
			}
		}
		out, err := s.transitionWithdrawal(ctx, tx, w, WithdrawalSettled, withdrawActor, "finalized, reconciled and posted",
			subjectSet{journal: &posted.TransactionID})
		return SettleResult{Withdrawal: out}, err
	case obs.failedFinal():
		out, err := s.failWithdrawal(ctx, tx, w, RefusalChainFailed+":"+string(obs.Status.Err), "the transaction failed on chain")
		return SettleResult{Withdrawal: out}, err
	case obs.Expired:
		out, err := s.failWithdrawal(ctx, tx, w, RefusalSubmissionExpired,
			"the chain does not hold the transaction and its blockhash expired below finality; it can never land")
		return SettleResult{Withdrawal: out}, err
	}
	return SettleResult{Withdrawal: w}, nil
}

// WithdrawalResult is a withdrawal's state when ExecuteWithdrawal returns.
type WithdrawalResult struct {
	Withdrawal Withdrawal
	Replay     bool
	Settled    bool
}

// ExecuteWithdrawal carries an approved withdrawal to its end, exactly as
// ExecuteSwap does a swap: sign, gate, submit once, record, settle.
func (s *Service) ExecuteWithdrawal(ctx context.Context, wid WithdrawalID) (WithdrawalResult, error) {
	if err := requireExecutor(ctx); err != nil {
		return WithdrawalResult{}, err
	}
	ctx = serviceContext(ctx)
	w, err := getWithdrawal(ctx, s.db, wid, false)
	if err != nil {
		return WithdrawalResult{}, err
	}
	if err := s.requireNetwork(SubjectWithdrawal, w.ID.String(), w.Network); err != nil {
		return WithdrawalResult{Withdrawal: w}, err
	}
	if w.Status == WithdrawalApproved {
		if err := s.host.EnsureNativeBalance(ctx, w.Chain, w.Wallet, s.params.GasMinimum, s.params.GasTopUp); err != nil {
			return WithdrawalResult{Withdrawal: w}, err
		}
	}
	var out SignOutcome
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.SignWithdrawal(ctx, tx, wid)
		return err
	}); err != nil {
		return WithdrawalResult{Withdrawal: w}, err
	}
	if out.Refusal != nil {
		return WithdrawalResult{Withdrawal: out.Withdrawal, Settled: true}, out.Refusal
	}
	if !out.Fresh {
		res, err := s.driveWithdrawal(ctx, wid, false, false)
		res.Replay = true
		return res, err
	}
	var gateErr error
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out.Withdrawal, err = s.GateWithdrawalSubmission(ctx, tx, wid)
		if err != nil && reasonOf(err) == ReasonBlockedBeforeSubmission {
			gateErr, err = err, nil
		}
		return err
	}); err != nil {
		return WithdrawalResult{Withdrawal: out.Withdrawal}, err
	}
	if gateErr != nil {
		return WithdrawalResult{Withdrawal: out.Withdrawal, Settled: true}, gateErr
	}
	exec := submitOnce(ctx, s.host, out.Withdrawal.Chain, out.Withdrawal.SignedTx)
	var recorded Withdrawal
	if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recorded, err = s.RecordWithdrawalSubmission(ctx, tx, wid, exec)
		return err
	}); err != nil {
		return WithdrawalResult{Withdrawal: out.Withdrawal}, err
	}
	switch {
	case recorded.Status == WithdrawalFailed:
		return WithdrawalResult{Withdrawal: recorded, Settled: true}, refused(errs.CodeConflict, recorded.FailureCode, "the chain refused the transaction")
	case exec.outcome == execUnknown:
		return WithdrawalResult{Withdrawal: recorded}, submissionUnknown(recorded.TxID, exec.err)
	}
	return s.driveWithdrawal(ctx, wid, recorded.Mode == gates.ModeSandbox, false)
}

// driveWithdrawal observes a signed withdrawal until it settles or stops
// changing, producing blocks as driveSwap does.
func (s *Service) driveWithdrawal(ctx context.Context, wid WithdrawalID, produce, produceSigned bool) (WithdrawalResult, error) {
	for step := 0; step < maxDriveSteps; step++ {
		w, err := getWithdrawal(ctx, s.db, wid, false)
		if err != nil {
			return WithdrawalResult{}, err
		}
		if w.Status != WithdrawalSigned && w.Status != WithdrawalSubmitted && w.Status != WithdrawalUnknown {
			return WithdrawalResult{Withdrawal: w, Settled: w.Status.Terminal()}, nil
		}
		if _, err := awaitFinal(ctx, s.host, w.Chain, w.TxID, w.BlockhashHeight, produce && (w.Status != WithdrawalSigned || produceSigned)); err != nil {
			return WithdrawalResult{Withdrawal: w}, err
		}
		var res SettleResult
		if err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = s.SettleWithdrawal(ctx, tx, wid)
			return err
		}); err != nil {
			return WithdrawalResult{Withdrawal: w}, err
		}
		if res.Mismatch {
			return WithdrawalResult{Withdrawal: res.Withdrawal}, errs.New(errs.CodeReconciliationRequired,
				"delegation: the chain finalized something other than the signed transaction; a reconciliation record is open")
		}
		if res.Withdrawal.Status == w.Status {
			return WithdrawalResult{Withdrawal: res.Withdrawal, Settled: res.Withdrawal.Status.Terminal()}, nil
		}
	}
	w, err := getWithdrawal(ctx, s.db, wid, false)
	return WithdrawalResult{Withdrawal: w, Settled: w.Status.Terminal()}, err
}

// GetWithdrawal returns one withdrawal.
func (s *Service) GetWithdrawal(ctx context.Context, q db.Querier, wid WithdrawalID) (Withdrawal, error) {
	return getWithdrawal(ctx, q, wid, false)
}
