package gates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// The sandbox tier.
//
// A gate reaches ACTIVE by dual control with evidence, and nothing here changes
// that. What a STAGING deployment could not do was exercise the product it
// exists to rehearse: every gated action was refused before the domain code
// ran, and the only way past was the real ceremony with fabricated references,
// which is forbidden and would defeat the control it imitates.
//
// SANDBOX is the state that answers that, with three properties (migration
// 00755 holds the first in the schema):
//
//  1. It can only exist where nothing real can move. The table refuses a
//     SANDBOX row whose environment is PROD, and the function refuses to write
//     one there.
//  2. It is entered by a single operator with a reason, through a SECURITY
//     DEFINER function that writes its own history row and CLEARS the approval
//     chain, the approval version, the evidence references and the validity
//     window in the same statement (migration 00791). A sandbox gate carries no
//     approval and cannot be mistaken for one, because there is nothing on the
//     row to mistake -- not even when it was sandboxed out of EXPIRED or
//     REVOKED, which is the case 00755 left carrying the whole approval version
//     that reached ACTIVE (F-161). The cleared version is not lost: every
//     transition of it, with the digest each principal attested, is in
//     capability_gate_transitions.
//  3. It is active only for a Checker built with sandbox allowed -- which
//     cmd/api does exactly when the deployment declared itself a sandbox tier
//     (CP_API_LEGAL_POLICY=SANDBOX, refused in PROD). Anywhere else a SANDBOX
//     row is inactive with a reason that says so.

// Operations understood by cp_gate_sandbox (migration 00755).
const (
	opSandbox   = "sandbox"
	opUnsandbox = "unsandbox"
)

// ReasonSandboxNotAllowedHere is the verdict on a SANDBOX row read by a
// deployment that is not a sandbox tier.
const ReasonSandboxNotAllowedHere = "gate is SANDBOX and this deployment is not a sandbox tier"

// sandboxEvidenceDigest is the digest a sandbox transition attests.
//
// Every transition row carries the digest of the evidence the acting principal
// attested to, and a sandbox transition has none to attest: it is entered by one
// operator on the strength of the deployment's own declaration. The two sandbox
// operations used to pass the digest of the row they found, so a gate sandboxed
// out of EXPIRED wrote the digest of the real approval's four evidence
// references onto a transition nobody produced evidence for, and the audit event
// repeated it (F-161).
//
// It is the digest of a Gate with no evidence at all rather than 32 zero bytes,
// so that it is exactly the digest the resulting row hashes to -- the transition,
// the audit event and the row it produced all say the same thing, and that thing
// is "nothing".
func sandboxEvidenceDigest() [32]byte { return Gate{}.EvidenceDigest() }

// WithSandbox returns an Admin that may move gates into and out of SANDBOX.
// cmd/api sets it from the same condition the Checker reads.
func (a *Admin) WithSandbox(allowed bool) *Admin {
	cp := *a
	cp.sandboxAllowed = allowed
	return &cp
}

// WithSandbox returns a Checker that reads a SANDBOX row as active. It is a
// copy: a Checker holds no mutable state and this keeps it that way.
func (c *Checker) WithSandbox(allowed bool) *Checker {
	cp := *c
	cp.sandboxAllowed = allowed
	return &cp
}

// SandboxAllowed reports whether this checker reads SANDBOX rows as active.
func (c *Checker) SandboxAllowed() bool { return c.sandboxAllowed }

// Sandbox moves DISABLED|REVOKED|EXPIRED → SANDBOX. Requires gate:propose and
// a step-up, like the first step of the real ceremony, and refuses outright
// unless this Admin was built for a sandbox tier. The reason is recorded on
// the transition and in the audit stream.
func (a *Admin) Sandbox(ctx context.Context, tx pgx.Tx, c Capability, reason string) (Gate, error) {
	return a.sandboxOp(ctx, tx, c, reason, opSandbox, StateSandbox)
}

// Unsandbox moves SANDBOX → DISABLED. Same requirements as Sandbox.
func (a *Admin) Unsandbox(ctx context.Context, tx pgx.Tx, c Capability, reason string) (Gate, error) {
	return a.sandboxOp(ctx, tx, c, reason, opUnsandbox, StateDisabled)
}

func (a *Admin) sandboxOp(ctx context.Context, tx pgx.Tx, c Capability, reason, op string, to GateState) (Gate, error) {
	p, err := actor(ctx)
	if err != nil {
		return Gate{}, err
	}
	if !a.sandboxAllowed {
		return Gate{}, errs.New(errs.CodeForbidden,
			"sandbox activation is not available in this deployment; a capability is activated by dual control or not at all").
			WithField("capability", string(c))
	}
	if err := a.require(ctx, security.PermGatePropose); err != nil {
		return Gate{}, err
	}
	if err := a.requireStepUp(ctx); err != nil {
		return Gate{}, err
	}
	if !c.Valid() {
		return Gate{}, errs.Newf(errs.CodeValidationFailed, "unknown capability %q", c)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Gate{}, errs.New(errs.CodeValidationFailed, "reason is required").WithField("reason", "required")
	}
	g, err := a.load(ctx, tx, c)
	if err != nil {
		return Gate{}, err
	}
	if !CanTransition(g.State, to) {
		return Gate{}, invalidTransition(g, to)
	}
	now := a.clk.Now()
	from := g.State
	digest := sandboxEvidenceDigest()
	tid := NewTransitionID()
	out, err := scanGate(tx.QueryRow(ctx, `SELECT `+gateColumns+` FROM cp_gate_sandbox(
			$1::uuid, $2::bigint, $3::text, $4::text, $5::text, $6::text, $7::uuid, $8::bytea, $9::timestamptz) AS g`,
		g.ID, g.Version, op, string(p.ActorType), p.SubjectID, reason, tid, digest[:], now))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Gate{}, errs.New(errs.CodeConflict, "gate was modified concurrently").WithField("capability", string(c))
		}
		return Gate{}, fmt.Errorf("gates: %s %s/%s: %w", op, c, a.env, err)
	}
	*g = out
	payload, err := json.Marshal(struct {
		Capability   Capability `json:"capability"`
		Environment  string     `json:"environment"`
		From         GateState  `json:"from"`
		To           GateState  `json:"to"`
		TransitionID string     `json:"transition_id"`
		Sandbox      bool       `json:"sandbox"`
	}{g.Capability, g.Environment, from, g.State, tid.String(), true})
	if err != nil {
		return Gate{}, fmt.Errorf("gates: encode audit payload: %w", err)
	}
	if err := a.audit.Append(ctx, tx, AuditEvent{
		Stream: AuditStream, ActorType: string(p.ActorType), ActorID: p.SubjectID,
		Action: "capability_gate." + op, ResourceType: "capability_gate", ResourceID: g.ID.String(),
		Reason: reason, EvidenceRef: g.EvidenceDigestHex(), PolicyVersion: "sandbox_tier",
		Payload: payload, OccurredAt: now,
	}); err != nil {
		return Gate{}, fmt.Errorf("gates: audit %s: %w", op, err)
	}
	return *g, nil
}

// evaluateSandbox is Evaluate's answer for a SANDBOX row. Condition 1
// (configuration) has already passed. The dual-control conditions are not
// consulted -- that is the point of the state -- and the deployment's own
// declaration is the condition that replaces them.
//
// A revoke is a STATE, not a column, and this is the one place that distinction
// decides an outcome. Revoking a gate moves it to REVOKED, where the row is
// refused by ReasonStateNotActive above; `revoked_at` on a SANDBOX row is
// therefore residue of an earlier life, which migration 00791 now clears when
// the row enters SANDBOX and has backfilled for the rows the old function wrote.
// Reading that residue as a refusal made REVOKED -- a source of SANDBOX named by
// ADR-0023, by 00755 and by CanTransition -- produce a gate that reported
// success, logged "sandbox-activated at boot", and could never be active, with
// no way back that did not run the ceremony the sandbox tier exists to avoid
// fabricating (F-160).
func evaluateSandbox(g *Gate, sandboxAllowed bool, now time.Time, v Verdict) Verdict {
	fail := func(reason string) Verdict {
		v.Active = false
		v.Reason = reason
		return v
	}
	if !sandboxAllowed {
		return fail(ReasonSandboxNotAllowedHere)
	}
	if g.EffectiveAt == nil {
		return fail(ReasonNoEffectiveAt)
	}
	if now.Before(*g.EffectiveAt) {
		return fail(ReasonNotYetEffective)
	}
	if g.ExpiresAt != nil && !now.Before(*g.ExpiresAt) {
		return fail(ReasonExpired)
	}
	v.Active = true
	v.Sandbox = true
	v.Reason = ""
	return v
}
