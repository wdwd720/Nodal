package risk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

type (
	policyKind   struct{}
	decisionKind struct{}
)

// PolicyID identifies a risk_policies row.
type PolicyID = id.ID[policyKind]

// DecisionID identifies a risk_decisions row.
type DecisionID = id.ID[decisionKind]

// ErrNoPolicy is returned by EffectivePolicy when no GLOBAL policy row is
// effective at the requested time. Callers evaluate with the zero Policy to
// persist a fail-closed decision carrying RISK_POLICY_MISSING.
var ErrNoPolicy = errors.New("risk: no GLOBAL policy is effective at the requested time")

// PolicyRef identifies the composition that produced an effective policy.
type PolicyRef struct {
	// Version is the composite version ("GLOBAL=v1;ACCOUNT=v2;AGENT=v3").
	Version string
	// Hash is the hash of the composed rules.
	Hash           string
	GlobalVersion  string
	AccountVersion string
	AgentVersion   string
}

// Store reads and writes risk_policies and risk_decisions and reads
// trade_intents for order-rate counting. It holds no state and no clock.
type Store struct{}

// NewStore returns a Store.
func NewStore() *Store { return &Store{} }

// PolicyRecord is the input to RecordPolicy.
type PolicyRecord struct {
	Scope       Scope
	ScopeID     string
	Version     string
	Rules       json.RawMessage
	EffectiveAt time.Time
	ExpiresAt   *time.Time
	ActorType   security.ActorType
	ActorID     string
	Reason      string
}

// EffectivePolicy composes the GLOBAL, ACCOUNT(accountID) and AGENT(agentID)
// rows effective at `at`. Missing ACCOUNT or AGENT rows simply do not
// tighten; a missing GLOBAL row is ErrNoPolicy. Stored rules are re-parsed
// and their hashes re-verified so tampering fails closed.
func (s *Store) EffectivePolicy(ctx context.Context, q db.Querier, accountID, agentID string, at time.Time) (Policy, PolicyRef, error) {
	global, err := s.loadScoped(ctx, q, ScopeGlobal, GlobalScopeID, at)
	if err != nil {
		return Policy{}, PolicyRef{}, err
	}
	var account, agent *Policy
	if accountID != "" {
		account, err = s.loadScoped(ctx, q, ScopeAccount, accountID, at)
		if err != nil && !errors.Is(err, ErrNoPolicy) {
			return Policy{}, PolicyRef{}, err
		}
	}
	if agentID != "" {
		agent, err = s.loadScoped(ctx, q, ScopeAgent, agentID, at)
		if err != nil && !errors.Is(err, ErrNoPolicy) {
			return Policy{}, PolicyRef{}, err
		}
	}
	composed := Compose(global, account, agent)
	ref := PolicyRef{Version: composed.Version, Hash: composed.Hash(), GlobalVersion: global.Version}
	if account != nil {
		ref.AccountVersion = account.Version
	}
	if agent != nil {
		ref.AgentVersion = agent.Version
	}
	return composed, ref, nil
}

func (s *Store) loadScoped(ctx context.Context, q db.Querier, scope Scope, scopeID string, at time.Time) (*Policy, error) {
	var (
		version string
		rules   []byte
		hash    []byte
	)
	err := q.QueryRow(ctx, `SELECT version, rules, rules_hash FROM risk_policies
		WHERE scope = $1 AND scope_id = $2 AND effective_at <= $3 AND (expires_at IS NULL OR expires_at > $3)
		ORDER BY effective_at DESC, created_at DESC, id DESC LIMIT 1`, string(scope), scopeID, at.UTC()).Scan(&version, &rules, &hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoPolicy
		}
		return nil, fmt.Errorf("risk: load %s policy: %w", scope, err)
	}
	p, err := ParsePolicy(rules)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "risk: stored policy does not parse").WithField("version", version)
	}
	if hex.EncodeToString(hash) != p.Hash() {
		return nil, errs.New(errs.CodeInternal, "risk: stored policy hash mismatch").WithField("version", version)
	}
	p.Version = version
	return &p, nil
}

// RecordPolicy inserts a new policy version. AGENT actors and AGENT
// principals are refused before any query (PART 60); GLOBAL rows require an
// OPERATOR or SYSTEM actor and must be complete; ACCOUNT and AGENT rows may
// be written by USER, OPERATOR or SYSTEM actors and can only tighten.
func (s *Store) RecordPolicy(ctx context.Context, tx pgx.Tx, rec PolicyRecord) (Policy, error) {
	if err := rejectAgent(ctx, rec.ActorType); err != nil {
		return Policy{}, err
	}
	switch rec.ActorType {
	case security.ActorOperator, security.ActorSystem, security.ActorUser:
	default:
		return Policy{}, errs.Newf(errs.CodeForbidden, "risk: policies may only be recorded by OPERATOR, SYSTEM or USER actors, not %q", rec.ActorType)
	}
	if !rec.Scope.Valid() {
		return Policy{}, errs.Newf(errs.CodeValidationFailed, "risk: unknown policy scope %q", rec.Scope)
	}
	scopeID := rec.ScopeID
	switch rec.Scope {
	case ScopeGlobal:
		if rec.ActorType == security.ActorUser {
			return Policy{}, errs.New(errs.CodeForbidden, "risk: GLOBAL policies may only be recorded by OPERATOR or SYSTEM actors")
		}
		if scopeID == "" {
			scopeID = GlobalScopeID
		}
		if scopeID != GlobalScopeID {
			return Policy{}, errs.New(errs.CodeValidationFailed, "risk: GLOBAL policies use scope id *")
		}
	default:
		if _, err := id.ParseAny(scopeID); err != nil {
			return Policy{}, errs.Newf(errs.CodeValidationFailed, "risk: %s scope id must be a canonical uuid", rec.Scope)
		}
	}
	switch {
	case rec.Version == "":
		return Policy{}, errs.New(errs.CodeValidationFailed, "risk: policy version required")
	case rec.ActorID == "":
		return Policy{}, errs.New(errs.CodeValidationFailed, "risk: actor id required")
	case rec.Reason == "":
		return Policy{}, errs.New(errs.CodeValidationFailed, "risk: reason required")
	case rec.EffectiveAt.IsZero():
		return Policy{}, errs.New(errs.CodeValidationFailed, "risk: effective_at required")
	case rec.ExpiresAt != nil && !rec.ExpiresAt.After(rec.EffectiveAt):
		return Policy{}, errs.New(errs.CodeValidationFailed, "risk: expires_at must be after effective_at")
	}
	p, err := ParsePolicy(rec.Rules)
	if err != nil {
		return Policy{}, err
	}
	if err := p.Validate(rec.Scope); err != nil {
		return Policy{}, err
	}
	canon, err := p.CanonicalJSON()
	if err != nil {
		return Policy{}, fmt.Errorf("risk: canonical rules: %w", err)
	}
	sum := sha256.Sum256(canon)
	var expires *time.Time
	if rec.ExpiresAt != nil {
		t := rec.ExpiresAt.UTC()
		expires = &t
	}
	_, err = tx.Exec(ctx, `INSERT INTO risk_policies
		(id, version, scope, scope_id, rules, rules_hash, effective_at, expires_at, created_by_actor_type, created_by_actor_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		id.New[policyKind](), rec.Version, string(rec.Scope), scopeID, canon, sum[:], rec.EffectiveAt.UTC(), expires,
		string(rec.ActorType), rec.ActorID, rec.Reason)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Policy{}, errs.Wrap(err, errs.CodeConflict, "risk: policy version already recorded").WithField("version", rec.Version)
		}
		return Policy{}, fmt.Errorf("risk: record policy: %w", err)
	}
	p.Version = rec.Version
	return p, nil
}

// persistedAccountSnapshot is what risk_decisions.account_snapshot holds:
// everything account-side the decision depended on.
type persistedAccountSnapshot struct {
	Stage        Stage             `json:"stage"`
	Now          time.Time         `json:"now"`
	Intent       Intent            `json:"intent"`
	Account      AccountSnapshot   `json:"account"`
	Envelope     *EnvelopeSnapshot `json:"envelope"`
	KillSwitches []KillSwitch      `json:"kill_switches"`
}

type persistedMarketSnapshot struct {
	Market MarketSnapshot `json:"market"`
	// NativeMarket is present only for a Nodal-native trade. It is persisted
	// beside the market snapshot rather than omitted because a decision that
	// cannot be recomputed from its own row is not evidence -- the two native
	// limits read nothing else, so without this the stored REJECT would have
	// no reproducible cause.
	NativeMarket *NativeMarketSnapshot `json:"native_market,omitempty"`
}

type persistedConstraints struct {
	Constraints          ResultingConstraints `json:"constraints"`
	ActionClass          ActionClass          `json:"action_class"`
	EffectiveNotionalUSD string               `json:"effective_notional_usd"`
	MatchedKillSwitches  []KillSwitch         `json:"matched_kill_switches"`
	InputHash            string               `json:"input_hash"`
	DecisionHash         string               `json:"decision_hash"`
}

// RecordDecision persists a decision produced by Evaluate with the snapshots
// it was computed from. The decision hash is re-verified so only an
// unmodified Evaluate output can be recorded.
func (s *Store) RecordDecision(ctx context.Context, tx pgx.Tx, in Input, d Decision, correlationID string) (DecisionID, error) {
	if d.Hash == "" || d.Hash != d.ComputeHash() {
		return DecisionID{}, errs.New(errs.CodeValidationFailed, "risk: decision hash mismatch; decisions must come unmodified from Evaluate")
	}
	in = in.normalized()
	if !d.Stage.Valid() {
		return DecisionID{}, errs.Newf(errs.CodeValidationFailed, "risk: stage %q cannot be persisted", d.Stage)
	}
	intentID, err := optionalUUID("intent_id", in.Intent.ID)
	if err != nil {
		return DecisionID{}, err
	}
	accountID, err := optionalUUID("account_id", in.Intent.AccountID)
	if err != nil {
		return DecisionID{}, err
	}
	agentID, err := optionalUUID("agent_id", in.Intent.AgentID)
	if err != nil {
		return DecisionID{}, err
	}
	var quoteID *id.ID[id.Any]
	if in.Market.Quote != nil {
		if quoteID, err = optionalUUID("quote_id", in.Market.Quote.ID); err != nil {
			return DecisionID{}, err
		}
	}
	policyHash := []byte{}
	if d.PolicyHash != "" {
		if policyHash, err = hex.DecodeString(d.PolicyHash); err != nil || len(policyHash) != sha256.Size {
			return DecisionID{}, errs.New(errs.CodeValidationFailed, "risk: decision policy hash is malformed")
		}
	}
	accountSnap, err := canonicalJSON(persistedAccountSnapshot{
		Stage: in.Stage, Now: in.Now, Intent: in.Intent, Account: in.Account, Envelope: in.Envelope, KillSwitches: in.KillSwitches,
	})
	if err != nil {
		return DecisionID{}, errs.Wrap(err, errs.CodeValidationFailed, "risk: account snapshot cannot be rendered")
	}
	marketSnap, err := canonicalJSON(persistedMarketSnapshot{Market: in.Market, NativeMarket: in.NativeMarket})
	if err != nil {
		return DecisionID{}, errs.Wrap(err, errs.CodeValidationFailed, "risk: market snapshot cannot be rendered")
	}
	matched := d.MatchedKillSwitches
	if matched == nil {
		matched = []KillSwitch{}
	}
	constraints, err := canonicalJSON(persistedConstraints{
		Constraints: d.Constraints, ActionClass: d.ActionClass, EffectiveNotionalUSD: d.EffectiveNotionalUSD.String(),
		MatchedKillSwitches: matched, InputHash: d.InputHash, DecisionHash: d.Hash,
	})
	if err != nil {
		return DecisionID{}, errs.Wrap(err, errs.CodeValidationFailed, "risk: constraints cannot be rendered")
	}
	codes := d.ReasonCodes
	if codes == nil {
		codes = []string{}
	}
	decisionID := id.New[decisionKind]()
	_, err = tx.Exec(ctx, `INSERT INTO risk_decisions
		(id, intent_id, account_id, agent_id, stage, policy_version, policy_hash, account_snapshot, market_snapshot, quote_id,
		 decision, reason_codes, resulting_constraints, evaluator_version, evaluated_at, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,NULLIF($16,''))`,
		decisionID, intentID, accountID, agentID, string(d.Stage), d.PolicyVersion, policyHash, accountSnap, marketSnap, quoteID,
		string(d.Verdict), codes, constraints, d.EvaluatorVersion, d.EvaluatedAt.UTC(), correlationID)
	if err != nil {
		return DecisionID{}, fmt.Errorf("risk: record decision: %w", err)
	}
	return decisionID, nil
}

// CountOrders counts the persisted trade intents for the account (and, when
// agentID is non-empty, that agent) received in (at−window, at] whose status
// is not REJECTED or NO_VALID_PLAN. It is the authoritative order-rate
// counter (PART 181): it survives any Redis reset.
func (s *Store) CountOrders(ctx context.Context, q db.Querier, accountID, agentID string, window time.Duration, at time.Time) (int, error) {
	acct, err := id.ParseAny(accountID)
	if err != nil {
		return 0, errs.New(errs.CodeValidationFailed, "risk: account id must be a canonical uuid")
	}
	agent, err := optionalUUID("agent_id", agentID)
	if err != nil {
		return 0, err
	}
	if window <= 0 {
		return 0, errs.New(errs.CodeValidationFailed, "risk: order-rate window must be positive")
	}
	since := at.UTC().Add(-window)
	var n int
	err = q.QueryRow(ctx, `SELECT count(*) FROM trade_intents
		WHERE account_id = $1 AND ($2::uuid IS NULL OR agent_id = $2::uuid)
		  AND received_at > $3 AND received_at <= $4
		  AND status NOT IN ('REJECTED','NO_VALID_PLAN')`, acct, agent, since, at.UTC()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("risk: count orders: %w", err)
	}
	return n, nil
}

func rejectAgent(ctx context.Context, actor security.ActorType) error {
	if actor == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "risk: AGENT actors can never write policy")
	}
	if p, ok := security.PrincipalFrom(ctx); ok && p.IsAgent() {
		return errs.New(errs.CodeForbidden, "risk: AGENT principals can never write policy")
	}
	return nil
}

// optionalUUID parses a canonical UUID string, mapping "" to SQL NULL.
func optionalUUID(field, s string) (*id.ID[id.Any], error) {
	if s == "" {
		return nil, nil
	}
	v, err := id.ParseAny(s)
	if err != nil {
		return nil, errs.Newf(errs.CodeValidationFailed, "risk: %s is not a canonical uuid", field)
	}
	return &v, nil
}
