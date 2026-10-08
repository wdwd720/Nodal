package treasury

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/orders"
)

// Action is one row of the register of house actions: who did what for the
// house book, under which signing and risk policy, with what outcome.
type Action struct {
	ID                   ActionID
	HouseAccountID       econ.HouseAccountID
	Kind                 ActionKind
	Activity             econ.HouseStrategy
	Outcome              Outcome
	RejectionCode        string
	ReasonCodes          []string
	Actor                econ.Actor
	SigningPolicyVersion int
	// RiskPolicyVersion is 0 when no risk policy applied.
	RiskPolicyVersion int
	IdempotencyKey    string
	RequestHash       []byte
	RiskDecisionID    *DecisionID
	OrderID           *orders.OrderID
	PaperFillID       *PaperFillID
	FundingID         *FundingID
	PolicyID          *PolicyID
	SigningPolicyID   *PolicyID
	SnapshotID        *SnapshotID
	EffectID          *econ.EffectID
	EvidenceRef       string
	EvidenceSHA256    []byte
	CorrelationID     string
	OccurredAt        time.Time
}

// Rejection rebuilds the error a REJECTED action was refused with.
func (a Action) Rejection() error {
	if a.Outcome != OutcomeRejected {
		return nil
	}
	code := errs.CodeValidationFailed
	switch a.RejectionCode {
	case string(errs.CodeRiskRejected):
		code = errs.CodeRiskRejected
	case string(errs.CodeKillSwitchActive):
		code = errs.CodeKillSwitchActive
	case string(errs.CodeCapabilityNotApproved):
		code = errs.CodeCapabilityNotApproved
	}
	reason := a.RejectionCode
	if len(a.ReasonCodes) > 0 {
		reason = a.ReasonCodes[0]
	}
	return errs.Newf(code, "treasury: %s was refused: %s", a.Kind, strings.Join(append([]string{a.RejectionCode}, a.ReasonCodes...), ", ")).
		WithField("reason", reason).WithField("rejection_code", a.RejectionCode).WithField("reason_codes", a.ReasonCodes).
		WithField("action_id", a.ID.String())
}

const actionColumns = `id, house_account_id, kind, coalesce(activity, ''), outcome, coalesce(rejection_code, ''), reason_codes,
	actor_type, actor_id, signing_policy_version, coalesce(risk_policy_version, 0), idempotency_key, request_hash,
	risk_decision_id, order_id, paper_fill_id, funding_id, policy_id, signing_policy_id, snapshot_id, effect_id,
	coalesce(evidence_ref, ''), evidence_sha256, coalesce(correlation_id, ''), occurred_at`

func scanAction(row pgx.Row) (Action, error) {
	var a Action
	if err := row.Scan(&a.ID, &a.HouseAccountID, &a.Kind, &a.Activity, &a.Outcome, &a.RejectionCode, &a.ReasonCodes,
		&a.Actor.Type, &a.Actor.ID, &a.SigningPolicyVersion, &a.RiskPolicyVersion, &a.IdempotencyKey, &a.RequestHash,
		&a.RiskDecisionID, &a.OrderID, &a.PaperFillID, &a.FundingID, &a.PolicyID, &a.SigningPolicyID, &a.SnapshotID,
		&a.EffectID, &a.EvidenceRef, &a.EvidenceSHA256, &a.CorrelationID, &a.OccurredAt); err != nil {
		return Action{}, err
	}
	if a.ReasonCodes == nil {
		a.ReasonCodes = []string{}
	}
	a.OccurredAt = a.OccurredAt.UTC()
	return a, nil
}

// Action returns one recorded house action.
func (s *Service) Action(ctx context.Context, q db.Querier, aid ActionID) (Action, error) {
	a, err := scanAction(q.QueryRow(ctx, `SELECT `+actionColumns+` FROM treasury_actions WHERE id = $1`, aid))
	if err != nil {
		return Action{}, notFound(err, "treasury action", aid.String())
	}
	return a, nil
}

// Actions pages a house book's actions, newest first.
func (s *Service) Actions(ctx context.Context, q db.Querier, house econ.HouseAccountID, limit int) ([]Action, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+actionColumns+` FROM treasury_actions WHERE house_account_id = $1
		ORDER BY occurred_at DESC, id DESC LIMIT $2`, house, limit)
	if err != nil {
		return nil, dbErr("list treasury actions", err)
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, dbErr("scan treasury action", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) actionByKey(ctx context.Context, q db.Querier, house econ.HouseAccountID, key string) (Action, bool, error) {
	a, err := scanAction(q.QueryRow(ctx, `SELECT `+actionColumns+` FROM treasury_actions WHERE house_account_id = $1 AND idempotency_key = $2`,
		house, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, dbErr("load treasury action by key", err)
	}
	return a, true, nil
}

// sameRequest refuses a replay that is not the same request.
func sameRequest(prior Action, kind ActionKind, hash []byte) error {
	if prior.Kind != kind || !bytes.Equal(prior.RequestHash, hash) {
		return errs.New(errs.CodeInvalidIdempotencyReuse, "treasury: this idempotency key was used for a different house action").
			WithField("action_id", prior.ID.String())
	}
	return nil
}

// recordAction archives the action's evidence document, inserts the action
// row (the database checks the actor against the signing policy, TY002) and
// appends its audit event, tagged with the house ledger segment.
func (s *Service) recordAction(ctx context.Context, tx pgx.Tx, book Book, a *Action, reason string, detail map[string]any) error {
	if a.ReasonCodes == nil {
		a.ReasonCodes = []string{}
	}
	doc := map[string]any{
		"kind": "treasury.action.v1", "action_id": a.ID.String(), "house_account_id": a.HouseAccountID.String(),
		"action": string(a.Kind), "activity": string(a.Activity), "outcome": string(a.Outcome),
		"rejection_code": a.RejectionCode, "reason_codes": a.ReasonCodes, "actor_type": string(a.Actor.Type),
		"actor_id": a.Actor.ID, "signing_policy_version": a.SigningPolicyVersion, "risk_policy_version": a.RiskPolicyVersion,
		"idempotency_key": a.IdempotencyKey, "request_hash": hexOf(a.RequestHash), "reason": reason,
		"occurred_at": a.OccurredAt, "audit_tags": auditTags(book), "detail": detail,
	}
	if a.RiskDecisionID != nil {
		doc["risk_decision_id"] = a.RiskDecisionID.String()
	}
	if a.OrderID != nil {
		doc["order_id"] = a.OrderID.String()
	}
	if a.PaperFillID != nil {
		doc["paper_fill_id"] = a.PaperFillID.String()
	}
	if a.FundingID != nil {
		doc["funding_id"] = a.FundingID.String()
	}
	if a.EffectID != nil {
		doc["effect_id"] = a.EffectID.String()
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{
		Key: "treasury/" + a.HouseAccountID.String() + "/action/" + a.ID.String(), Document: doc,
	})
	if err != nil {
		return err
	}
	a.EvidenceRef, a.EvidenceSHA256 = ev.URI, ev.SHA256
	var activity, rejectionCode any
	if a.Activity != "" {
		activity = string(a.Activity)
	}
	if a.RejectionCode != "" {
		rejectionCode = a.RejectionCode
	}
	var riskVersion any
	if a.RiskPolicyVersion > 0 {
		riskVersion = a.RiskPolicyVersion
	}
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_actions (id, house_account_id, kind, activity, outcome, rejection_code,
		reason_codes, actor_type, actor_id, signing_policy_version, risk_policy_version, idempotency_key, request_hash,
		risk_decision_id, order_id, paper_fill_id, funding_id, policy_id, signing_policy_id, snapshot_id, effect_id,
		evidence_ref, evidence_sha256, correlation_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,NULLIF($24,''),$25)`,
		a.ID, a.HouseAccountID, a.Kind, activity, a.Outcome, rejectionCode, a.ReasonCodes, a.Actor.Type, a.Actor.ID,
		a.SigningPolicyVersion, riskVersion, a.IdempotencyKey, a.RequestHash, a.RiskDecisionID, a.OrderID, a.PaperFillID,
		a.FundingID, a.PolicyID, a.SigningPolicyID, a.SnapshotID, a.EffectID, a.EvidenceRef, a.EvidenceSHA256,
		a.CorrelationID, a.OccurredAt); err != nil {
		return dbErr("record treasury action", err)
	}
	payload, err := json.Marshal(map[string]any{
		"action_id": a.ID.String(), "action": string(a.Kind), "activity": string(a.Activity), "outcome": string(a.Outcome),
		"rejection_code": a.RejectionCode, "reason_codes": a.ReasonCodes, "audit_tags": auditTags(book),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "treasury: encode audit payload")
	}
	// Policy and registration changes are admin actions. Everything that also
	// claims an economic effect (whose events go to the system stream) stays
	// on the system stream, so no transaction holds both stream locks.
	stream := audit.SystemStream
	if a.Kind == ActionRegister || a.Kind == ActionSetPolicy || a.Kind == ActionSetSigningPolicy {
		stream = audit.AdminStream
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream: stream, ActorType: string(a.Actor.Type), ActorID: a.Actor.ID,
		Action:       "treasury." + strings.ToLower(string(a.Kind)) + "." + strings.ToLower(string(a.Outcome)),
		ResourceType: "treasury_house_book", ResourceID: a.HouseAccountID.String(), CorrelationID: a.CorrelationID,
		Reason: reasonOr(reason, string(a.Kind)), EvidenceRef: ev.URI, Payload: payload, OccurredAt: a.OccurredAt,
	})
	return err
}

// auditTags are the tags every house action's evidence and audit event carry:
// it belongs to the house ledger segment of this book, never to a customer.
func auditTags(b Book) map[string]string {
	return map[string]string{
		"ledger_segment": LedgerSegment, "owner_type": string(ledger.OwnerHouse), "house_account_id": b.HouseAccountID.String(),
		"class": string(b.Class), "strategy": string(b.Strategy), "value_domain": "SIMULATED",
	}
}

func (s *Service) insertSigningPolicy(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID, version int, p SigningPolicy,
	actor econ.Actor, reason string, now time.Time) (SigningPolicyVersion, error) {
	doc, err := p.Document(house)
	if err != nil {
		return SigningPolicyVersion{}, err
	}
	v := SigningPolicyVersion{
		ID: id.New[policyKind](), HouseAccountID: house, Version: version, Policy: p, Document: doc, Hash: hashHex(doc),
		Actor: actor, Reason: reason, EffectiveAt: now,
	}
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_signing_policies (id, house_account_id, version, key_custody,
		service_actor_id, operators_allowed, policy, policy_hash, actor_type, actor_id, reason, effective_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		v.ID, house, version, KeyCustodyNone, p.ServiceActorID, p.OperatorsAllowed, doc, v.Hash, actor.Type, actor.ID,
		reason, now); err != nil {
		return SigningPolicyVersion{}, dbErr("record signing policy", err)
	}
	return v, nil
}

const signingColumns = `id, house_account_id, version, service_actor_id, operators_allowed, policy, policy_hash, actor_type,
	actor_id, reason, effective_at`

func scanSigning(row pgx.Row) (SigningPolicyVersion, error) {
	var v SigningPolicyVersion
	if err := row.Scan(&v.ID, &v.HouseAccountID, &v.Version, &v.Policy.ServiceActorID, &v.Policy.OperatorsAllowed, &v.Document,
		&v.Hash, &v.Actor.Type, &v.Actor.ID, &v.Reason, &v.EffectiveAt); err != nil {
		return SigningPolicyVersion{}, err
	}
	v.EffectiveAt = v.EffectiveAt.UTC()
	return v, nil
}

// currentSigningPolicy returns the signing policy in force. A house book
// always has one: Register writes the first.
func (s *Service) currentSigningPolicy(ctx context.Context, q db.Querier, house econ.HouseAccountID) (SigningPolicyVersion, error) {
	v, err := scanSigning(q.QueryRow(ctx, `SELECT `+signingColumns+` FROM treasury_signing_policies WHERE house_account_id = $1
		ORDER BY version DESC LIMIT 1`, house))
	if err != nil {
		return SigningPolicyVersion{}, notFound(err, "signing policy of house book", house.String())
	}
	return v, nil
}

// SigningPolicy returns the signing policy in force for a house book.
func (s *Service) SigningPolicy(ctx context.Context, q db.Querier, house econ.HouseAccountID) (SigningPolicyVersion, error) {
	return s.currentSigningPolicy(ctx, q, house)
}

func (s *Service) signingPolicyByID(ctx context.Context, q db.Querier, pid PolicyID) (SigningPolicyVersion, error) {
	v, err := scanSigning(q.QueryRow(ctx, `SELECT `+signingColumns+` FROM treasury_signing_policies WHERE id = $1`, pid))
	if err != nil {
		return SigningPolicyVersion{}, notFound(err, "signing policy", pid.String())
	}
	return v, nil
}

const policyColumns = `id, house_account_id, version, policy, policy_hash, actor_type, actor_id, reason, effective_at`

func scanPolicy(row pgx.Row) (PolicyVersion, error) {
	var v PolicyVersion
	if err := row.Scan(&v.ID, &v.HouseAccountID, &v.Version, &v.Document, &v.Hash, &v.Actor.Type, &v.Actor.ID, &v.Reason,
		&v.EffectiveAt); err != nil {
		return PolicyVersion{}, err
	}
	p, house, err := decodePolicy(v.Document)
	if err != nil {
		return PolicyVersion{}, err
	}
	if house != v.HouseAccountID || hashHex(v.Document) != v.Hash {
		return PolicyVersion{}, errs.New(errs.CodeInternal, "treasury: a stored policy does not belong to its row")
	}
	v.Policy = p
	v.EffectiveAt = v.EffectiveAt.UTC()
	return v, nil
}

// currentPolicy returns the risk policy in force, and whether there is one.
func (s *Service) currentPolicy(ctx context.Context, q db.Querier, house econ.HouseAccountID) (PolicyVersion, bool, error) {
	v, err := scanPolicy(q.QueryRow(ctx, `SELECT `+policyColumns+` FROM treasury_policies WHERE house_account_id = $1
		ORDER BY version DESC LIMIT 1`, house))
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyVersion{}, false, nil
	}
	if err != nil {
		return PolicyVersion{}, false, dbErr("load treasury policy", err)
	}
	return v, true, nil
}

// Policy returns the risk policy in force for a house book.
func (s *Service) Policy(ctx context.Context, q db.Querier, house econ.HouseAccountID) (PolicyVersion, error) {
	v, found, err := s.currentPolicy(ctx, q, house)
	if err != nil {
		return PolicyVersion{}, err
	}
	if !found {
		return PolicyVersion{}, rejection(errs.CodeNotFound, ReasonPolicyMissing, "the house book has no risk policy")
	}
	return v, nil
}

func (s *Service) policyByID(ctx context.Context, q db.Querier, pid PolicyID) (PolicyVersion, error) {
	v, err := scanPolicy(q.QueryRow(ctx, `SELECT `+policyColumns+` FROM treasury_policies WHERE id = $1`, pid))
	if err != nil {
		return PolicyVersion{}, notFound(err, "treasury policy", pid.String())
	}
	return v, nil
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

func validKey(k string) error {
	if t := strings.TrimSpace(k); t == "" || t != k || len(k) > 200 {
		return errs.New(errs.CodeValidationFailed, "treasury: an idempotency key of 1..200 bytes without surrounding whitespace is required")
	}
	return nil
}

func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return reason
}

func notFound(err error, what, key string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.New(errs.CodeNotFound, "treasury: "+what+" not found").WithField("id", key)
	}
	return dbErr("load "+what, err)
}

// dbErr keeps retryable failures recognisable to db.InTx and maps the rest.
func dbErr(op string, err error) error {
	if db.IsRetryable(err) {
		return err
	}
	if ee, ok := errs.As(err); ok {
		return ee
	}
	if mapped := ledger.MapError(err); mapped != nil {
		if ee, ok := errs.As(mapped); ok && ee.Code != errs.CodeInternal {
			return ee
		}
	}
	return errs.Wrap(err, errs.CodeInternal, "treasury: "+op)
}
