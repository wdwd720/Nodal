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

// SkippedGate is a capability the boot-time sandbox step left exactly as it
// found it, and why. It is returned rather than logged inside this package so
// the deployment's log, and the tests, can name the capability and its state.
type SkippedGate struct {
	Capability Capability
	State      GateState
	Reason     string
}

// SandboxBootstrap is the outcome of a boot-time sandbox activation: the gates
// it moved into SANDBOX, and the gates it left alone.
type SandboxBootstrap struct {
	Moved   []Gate
	Skipped []SkippedGate
}

// skipRealCeremony is why a listed capability is left where it is.
const skipRealCeremony = "a real ceremony is in progress for this capability; a configuration line does not move an approval's state"

// BootstrapSandbox is SandboxAtBoot for a caller that only needs what moved.
func BootstrapSandbox(ctx context.Context, tx pgx.Tx, env string, caps []Capability, clk clock.Clock, audit AuditAppender) ([]Gate, error) {
	out, err := SandboxAtBoot(ctx, tx, env, caps, clk, audit)
	return out.Moved, err
}

// SandboxAtBoot moves each capability into SANDBOX unless it is there already,
// creating the DISABLED row first when none exists.
//
// A capability in a state that is not a legal source of SANDBOX -- one that is
// part of a real ceremony -- is never moved: a configuration line does not
// change the state of an approval. It is reported as skipped, and the rest of
// the list is still activated.
//
// It used to be an error, and cmd/api returned that error from wire, so the
// deployment could not start. Rehearsing the real ceremony is exactly what a
// sandbox tier is for, and doing it on a capability the blueprint also lists
// meant the next restart failed until somebody edited the blueprint -- with the
// only in-machine escape being a revoke, which before migration 00791 produced a
// permanently inert gate. Refusing to MOVE the gate is the control; refusing to
// BOOT was a second, unintended one that cost more than it protected (F-162,
// D-091). The skip is loud: cmd/api logs a WARN naming the capability and its
// state, which is the operator-facing half of the refusal.
//
// It is the caller's responsibility to invoke this only on a sandbox tier;
// cmd/api does, and the database refuses a PROD row regardless.
func SandboxAtBoot(ctx context.Context, tx pgx.Tx, env string, caps []Capability, clk clock.Clock, audit AuditAppender) (SandboxBootstrap, error) {
	var out SandboxBootstrap
	if !validEnvironment(env) {
		return out, errs.Newf(errs.CodeValidationFailed, "unknown environment %q", env)
	}
	if env == "PROD" {
		return out, errs.New(errs.CodeForbidden, "gates: a sandbox gate cannot exist in PROD")
	}
	if clk == nil || audit == nil {
		return out, errs.New(errs.CodeValidationFailed, "gates: clock and audit appender are required")
	}
	for _, c := range caps {
		if !c.Valid() {
			return out, errs.Newf(errs.CodeValidationFailed, "unknown capability %q", c)
		}
		if _, err := insertDisabled(ctx, tx, c, env); err != nil {
			return out, err
		}
		g, err := loadGate(ctx, tx, c, env, true)
		if err != nil {
			return out, err
		}
		if g == nil {
			return out, errs.Newf(errs.CodeInternal, "gates: %s/%s vanished after insert", c, env)
		}
		if g.State == StateSandbox {
			continue
		}
		if !CanTransition(g.State, StateSandbox) {
			out.Skipped = append(out.Skipped, SkippedGate{Capability: c, State: g.State, Reason: skipRealCeremony})
			continue
		}
		now := clk.Now()
		from := g.State
		digest := sandboxEvidenceDigest()
		tid := NewTransitionID()
		const reason = "sandbox tier: activated from deployment configuration (CP_API_SANDBOX_GATES); carries no approval"
		sbx, err := scanGate(tx.QueryRow(ctx, `SELECT `+gateColumns+` FROM cp_gate_sandbox(
				$1::uuid, $2::bigint, $3::text, $4::text, $5::text, $6::text, $7::uuid, $8::bytea, $9::timestamptz) AS g`,
			g.ID, g.Version, opSandbox, string(security.ActorSystem), bootstrapActorID, reason, tid, digest[:], now))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return out, errs.New(errs.CodeConflict, "gate was modified concurrently").WithField("capability", string(c))
			}
			return out, fmt.Errorf("gates: sandbox %s/%s: %w", c, env, err)
		}
		payload, err := json.Marshal(struct {
			Capability   Capability `json:"capability"`
			Environment  string     `json:"environment"`
			From         GateState  `json:"from"`
			To           GateState  `json:"to"`
			TransitionID string     `json:"transition_id"`
			Sandbox      bool       `json:"sandbox"`
		}{sbx.Capability, sbx.Environment, from, sbx.State, tid.String(), true})
		if err != nil {
			return out, fmt.Errorf("gates: encode audit payload: %w", err)
		}
		if err := audit.Append(ctx, tx, AuditEvent{
			Stream: AuditStream, ActorType: string(security.ActorSystem), ActorID: bootstrapActorID,
			Action: "capability_gate." + opSandbox, ResourceType: "capability_gate", ResourceID: sbx.ID.String(),
			Reason: reason, EvidenceRef: sbx.EvidenceDigestHex(), PolicyVersion: "sandbox_tier",
			Payload: payload, OccurredAt: now,
		}); err != nil {
			return out, fmt.Errorf("gates: audit sandbox: %w", err)
		}
		out.Moved = append(out.Moved, sbx)
	}
	return out, nil
}
