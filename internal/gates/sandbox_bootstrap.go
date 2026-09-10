package gates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// bootstrapActorID names the deployment configuration as the actor of a
// boot-time sandbox activation. There is no person to name: the authority is
// the blueprint line, reviewed like every other line there, and refused
// anywhere but a sandbox tier by config.Validate.
const bootstrapActorID = "config:CP_API_SANDBOX_GATES"

// BootstrapSandbox moves each capability into SANDBOX unless it is there
// already, creating the DISABLED row first when none exists. A capability in
// a state that is not a legal source of SANDBOX -- an approved or active one
// -- is an error rather than a move: a configuration line must never change
// the state of a real approval. Returns the gates it moved.
//
// It is the caller's responsibility to invoke this only on a sandbox tier;
// cmd/api does, and the database refuses a PROD row regardless.
func BootstrapSandbox(ctx context.Context, tx pgx.Tx, env string, caps []Capability, clk clock.Clock, audit AuditAppender) ([]Gate, error) {
	if !validEnvironment(env) {
		return nil, errs.Newf(errs.CodeValidationFailed, "unknown environment %q", env)
	}
	if env == "PROD" {
		return nil, errs.New(errs.CodeForbidden, "gates: a sandbox gate cannot exist in PROD")
	}
	if clk == nil || audit == nil {
		return nil, errs.New(errs.CodeValidationFailed, "gates: clock and audit appender are required")
	}
	var moved []Gate
	for _, c := range caps {
		if !c.Valid() {
			return moved, errs.Newf(errs.CodeValidationFailed, "unknown capability %q", c)
		}
		if _, err := insertDisabled(ctx, tx, c, env); err != nil {
			return moved, err
		}
		g, err := loadGate(ctx, tx, c, env, true)
		if err != nil {
			return moved, err
		}
		if g == nil {
			return moved, errs.Newf(errs.CodeInternal, "gates: %s/%s vanished after insert", c, env)
		}
		if g.State == StateSandbox {
			continue
		}
		if !CanTransition(g.State, StateSandbox) {
			return moved, errs.Newf(errs.CodeInvalidStateTransition,
				"gates: %s/%s is %s; a configuration line does not move a real approval's state -- revoke it first if that is intended",
				c, env, g.State)
		}
		now := clk.Now()
		from := g.State
		digest := g.EvidenceDigest()
		tid := NewTransitionID()
		const reason = "sandbox tier: activated from deployment configuration (CP_API_SANDBOX_GATES); carries no approval"
		out, err := scanGate(tx.QueryRow(ctx, `SELECT `+gateColumns+` FROM cp_gate_sandbox(
				$1::uuid, $2::bigint, $3::text, $4::text, $5::text, $6::text, $7::uuid, $8::bytea, $9::timestamptz) AS g`,
			g.ID, g.Version, opSandbox, string(security.ActorSystem), bootstrapActorID, reason, tid, digest[:], now))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return moved, errs.New(errs.CodeConflict, "gate was modified concurrently").WithField("capability", string(c))
			}
			return moved, fmt.Errorf("gates: sandbox %s/%s: %w", c, env, err)
		}
		payload, err := json.Marshal(struct {
			Capability   Capability `json:"capability"`
			Environment  string     `json:"environment"`
			From         GateState  `json:"from"`
			To           GateState  `json:"to"`
			TransitionID string     `json:"transition_id"`
			Sandbox      bool       `json:"sandbox"`
		}{out.Capability, out.Environment, from, out.State, tid.String(), true})
		if err != nil {
			return moved, fmt.Errorf("gates: encode audit payload: %w", err)
		}
		if err := audit.Append(ctx, tx, AuditEvent{
			Stream: AuditStream, ActorType: string(security.ActorSystem), ActorID: bootstrapActorID,
			Action: "capability_gate." + opSandbox, ResourceType: "capability_gate", ResourceID: out.ID.String(),
			Reason: reason, EvidenceRef: out.EvidenceDigestHex(), PolicyVersion: "sandbox_tier",
			Payload: payload, OccurredAt: now,
		}); err != nil {
			return moved, fmt.Errorf("gates: audit sandbox: %w", err)
		}
		moved = append(moved, out)
	}
	return moved, nil
}
