package gates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

const gateColumns = `id, capability, environment, state, approval_version,
	coalesce(legal_review_ref,''), coalesce(provider_contract_ref,''), coalesce(risk_approval_ref,''), coalesce(security_approval_ref,''),
	proposed_by_user_id::text, approvers, evidence_hashes, effective_at, expires_at, revoked_at, coalesce(revoke_reason,''),
	version, created_at, updated_at`

func scanGate(row pgx.Row) (Gate, error) {
	var (
		g          Gate
		proposedBy *string
		approvers  []byte
		hashes     []byte
	)
	if err := row.Scan(&g.ID, &g.Capability, &g.Environment, &g.State, &g.ApprovalVersion,
		&g.LegalReviewRef, &g.ProviderContractRef, &g.RiskApprovalRef, &g.SecurityApprovalRef,
		&proposedBy, &approvers, &hashes, &g.EffectiveAt, &g.ExpiresAt, &g.RevokedAt, &g.RevokeReason,
		&g.Version, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return Gate{}, err
	}
	if len(approvers) > 0 {
		if err := json.Unmarshal(approvers, &g.Approvers); err != nil {
			return Gate{}, fmt.Errorf("decode approvers: %w", err)
		}
	}
	if len(hashes) > 0 {
		if err := json.Unmarshal(hashes, &g.EvidenceHashes); err != nil {
			return Gate{}, fmt.Errorf("decode evidence_hashes: %w", err)
		}
	}
	for _, a := range g.Approvers {
		if a.Step == StepPropose && a.UserID != "" {
			g.ProposedBy = a.UserID
			break
		}
	}
	if g.ProposedBy == "" && proposedBy != nil {
		g.ProposedBy = *proposedBy
	}
	return g, nil
}

// loadGate returns the gate row, or nil when none exists. forUpdate locks
// the row for the rest of the transaction.
func loadGate(ctx context.Context, q db.Querier, c Capability, env string, forUpdate bool) (*Gate, error) {
	sql := `SELECT ` + gateColumns + ` FROM capability_gates WHERE capability = $1 AND environment = $2`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	g, err := scanGate(q.QueryRow(ctx, sql, string(c), env))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("gates: load %s/%s: %w", c, env, err)
	}
	return &g, nil
}

// insertDisabled persists the default DISABLED row for (c, env). It is a
// no-op when the row already exists; created reports whether it inserted.
func insertDisabled(ctx context.Context, q db.Querier, c Capability, env string) (created bool, err error) {
	tag, err := q.Exec(ctx, `INSERT INTO capability_gates (id, capability, environment, state)
		VALUES ($1, $2, $3, $4) ON CONFLICT (capability, environment) DO NOTHING`,
		NewGateID(), string(c), env, string(StateDisabled))
	if err != nil {
		return false, fmt.Errorf("gates: insert disabled %s/%s: %w", c, env, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Operations understood by cp_gate_transition (migration 00701). The values
// are the database's contract and double as the audit verb.
const (
	opPropose  = "propose"
	opApprove  = "approve"
	opResume   = "resume"
	opActivate = "activate"
	opSuspend  = "suspend"
	opRevoke   = "revoke"
	opExpire   = "expire"
)

// applyTransition moves the gate through cp_gate_transition, the only
// database path that may change capability_gates.state: cp_app holds no
// UPDATE privilege on that table and no INSERT on capability_gate_transitions
// (migration 00701), so the row and its history can only be written together,
// by a SECURITY DEFINER function that re-derives the legal-transition table,
// the dual-control rules and the five activation conditions from the *stored*
// row.
//
// g carries the transition this package intends: the last entry of its
// approval chain is the one to append, and on a propose its evidence columns
// and validity window are the new approval version's. Columns an operation
// does not own are ignored — the database keeps what it has stored — so this
// call cannot smuggle a rewritten chain or a cleared revoked_at past the
// checks. The row the function wrote replaces *g, which is therefore always
// exactly what the database holds.
//
// The checks in this package run first and produce the operator-facing
// errors; a GT0xx failure here means a caller reached the database by some
// other route, and is deliberately reported as an internal error.
func applyTransition(ctx context.Context, tx pgx.Tx, g *Gate, op string, p security.Principal, reason string, tid TransitionID, evidenceHash []byte, at time.Time) error {
	var approverJSON []byte
	switch op {
	case opPropose, opApprove, opResume, opActivate:
		if len(g.Approvers) == 0 {
			return fmt.Errorf("gates: %s recorded no approval chain entry", op)
		}
		b, err := json.Marshal(g.Approvers[len(g.Approvers)-1])
		if err != nil {
			return fmt.Errorf("gates: encode approver: %w", err)
		}
		approverJSON = b
	}
	// Only a propose opens a new approval version, so only a propose carries
	// evidence; every other operation leaves these NULL and the function keeps
	// the stored values.
	var (
		legal, provider, risk, sec *string
		hashesJSON                 []byte
		effectiveAt, expiresAt     *time.Time
	)
	if op == opPropose {
		hashes := g.EvidenceHashes
		if hashes == nil {
			hashes = []string{}
		}
		b, err := json.Marshal(hashes)
		if err != nil {
			return fmt.Errorf("gates: encode evidence hashes: %w", err)
		}
		hashesJSON = b
		legal, provider, risk, sec = &g.LegalReviewRef, &g.ProviderContractRef, &g.RiskApprovalRef, &g.SecurityApprovalRef
		effectiveAt, expiresAt = g.EffectiveAt, g.ExpiresAt
	}
	out, err := scanGate(tx.QueryRow(ctx, `SELECT `+gateColumns+` FROM cp_gate_transition(
			$1::uuid, $2::bigint, $3::text, $4::text, $5::text, $6::text, $7::uuid, $8::bytea, $9::timestamptz,
			$10::jsonb, $11::text, $12::text, $13::text, $14::text, $15::jsonb, $16::timestamptz, $17::timestamptz) AS g`,
		g.ID, g.Version, op, string(p.ActorType), p.SubjectID, reason, tid, evidenceHash, at,
		approverJSON, legal, provider, risk, sec, hashesJSON, effectiveAt, expiresAt))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errs.New(errs.CodeConflict, "gate was modified concurrently").WithField("capability", string(g.Capability))
		}
		return fmt.Errorf("gates: %s %s/%s: %w", op, g.Capability, g.Environment, err)
	}
	*g = out
	return nil
}

// Transition is a capability_gate_transitions row. It is written only by
// cp_gate_transition, in the same statement as the state change it records
// (migration 00701); this package reads it back with Transitions.
type Transition struct {
	ID           TransitionID
	GateID       GateID
	From         GateState
	To           GateState
	ActorType    security.ActorType
	ActorID      string
	Reason       string
	EvidenceHash []byte
	OccurredAt   time.Time
}

// Transitions lists the recorded transitions of a gate, oldest first.
func Transitions(ctx context.Context, q db.Querier, gateID GateID) ([]Transition, error) {
	rows, err := q.Query(ctx, `SELECT id, gate_id, from_state, to_state, actor_type, actor_id, reason, evidence_hash, occurred_at
		FROM capability_gate_transitions WHERE gate_id = $1 ORDER BY occurred_at, id`, gateID)
	if err != nil {
		return nil, fmt.Errorf("gates: list transitions: %w", err)
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var t Transition
		if err := rows.Scan(&t.ID, &t.GateID, &t.From, &t.To, &t.ActorType, &t.ActorID, &t.Reason, &t.EvidenceHash, &t.OccurredAt); err != nil {
			return nil, fmt.Errorf("gates: scan transition: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gates: list transitions: %w", err)
	}
	return out, nil
}

// Get returns the gate for (c, env) without evaluating it. It fails with
// NOT_FOUND when no row exists and never requires a principal: gate state is
// operator-visible information, and callers gate access with gate:read.
func Get(ctx context.Context, q db.Querier, c Capability, env string) (Gate, error) {
	if !c.Valid() {
		return Gate{}, errs.Newf(errs.CodeValidationFailed, "unknown capability %q", c)
	}
	if !validEnvironment(env) {
		return Gate{}, errs.Newf(errs.CodeValidationFailed, "unknown environment %q", env)
	}
	g, err := loadGate(ctx, q, c, env, false)
	if err != nil {
		return Gate{}, err
	}
	if g == nil {
		return Gate{}, errs.New(errs.CodeNotFound, "gate not found").WithField("capability", string(c)).WithField("environment", env)
	}
	return *g, nil
}

// List returns every gate row of the environment in capability declaration
// order (missing rows are simply absent; see Bootstrap).
func List(ctx context.Context, q db.Querier, env string) ([]Gate, error) {
	if !validEnvironment(env) {
		return nil, errs.Newf(errs.CodeValidationFailed, "unknown environment %q", env)
	}
	rows, err := q.Query(ctx, `SELECT `+gateColumns+` FROM capability_gates WHERE environment = $1`, env)
	if err != nil {
		return nil, fmt.Errorf("gates: list: %w", err)
	}
	defer rows.Close()
	byCap := map[Capability]Gate{}
	for rows.Next() {
		g, err := scanGate(rows)
		if err != nil {
			return nil, fmt.Errorf("gates: list scan: %w", err)
		}
		byCap[g.Capability] = g
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gates: list: %w", err)
	}
	out := make([]Gate, 0, len(byCap))
	for _, c := range allCapabilities {
		if g, ok := byCap[c]; ok {
			out = append(out, g)
		}
	}
	return out, nil
}

// Bootstrap ensures a DISABLED row exists for every capability in env. It is
// idempotent and returns the number of rows it created. Run it at startup so
// that a fresh deployment's "everything DISABLED" is a persisted fact
// (PART 244). It rejects an AGENT principal if one is attached, but needs
// no principal: it never changes an existing row.
func Bootstrap(ctx context.Context, tx pgx.Tx, env string) (int, error) {
	if p, ok := security.PrincipalFrom(ctx); ok && p.ActorType == security.ActorAgent {
		return 0, errs.New(errs.CodeForbidden, "agents cannot reach capability gates")
	}
	if !validEnvironment(env) {
		return 0, errs.Newf(errs.CodeValidationFailed, "unknown environment %q", env)
	}
	created := 0
	for _, c := range allCapabilities {
		ok, err := insertDisabled(ctx, tx, c, env)
		if err != nil {
			return created, err
		}
		if ok {
			created++
		}
	}
	return created, nil
}
