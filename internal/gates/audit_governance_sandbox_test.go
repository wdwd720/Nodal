package gates

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Adversarial audit (goal §54), area governance-sandbox-tier.
//
// Written by an auditor who did not write the sandbox tier. Each test FAILS
// against the current tree and reproduces one defect from the audit report.
// Nothing here changes product code.

// F-gov-1, the pure half.
//
// ADR-0023 and migration 00755 both name REVOKED as a legal source of SANDBOX:
// "A gate enters it from DISABLED, REVOKED or EXPIRED". `cp_gate_sandbox`
// accepts the move and `CanTransition(REVOKED, SANDBOX)` agrees. But the
// function's UPDATE writes only `state`, `effective_at`, `expires_at` and
// `version`, so `revoked_at` survives the move -- and `evaluateSandbox`
// refuses a row whose `revoked_at` is set before it looks at anything else.
//
// The result is a state operators were told is reachable and that can never be
// active: `Admin.Sandbox` returns success, the console shows SANDBOX, the boot
// path logs "capability sandbox-activated at boot", and the capability stays
// refused with "gate is revoked" forever.
func TestAUDIT_ASandboxGateEnteredFromRevokedCanNeverBeActive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	revokedAt := now.Add(-time.Hour)
	effectiveAt := now.Add(-time.Minute)

	// Exactly the row cp_gate_sandbox leaves behind for REVOKED -> SANDBOX:
	// the state and effective_at it writes, plus the revoked_at it does not
	// clear. Reproduced against the real function in
	// TestIntegration_AUDIT_SandboxFromRevokedKeepsTheRevoke.
	g := &Gate{
		Capability:  NativeMarketTrading,
		Environment: "STAGING",
		State:       StateSandbox,
		EffectiveAt: &effectiveAt,
		RevokedAt:   &revokedAt,
	}

	v := EvaluateWith(g, true, true, now)
	assert.True(t, v.Active,
		"a SANDBOX row on a sandbox tier must be active; REVOKED is a documented source of SANDBOX "+
			"(ADR-0023, migration 00755) and cp_gate_sandbox does not clear revoked_at, so every gate "+
			"sandbox-activated out of REVOKED is permanently inert, here with reason %q", v.Reason)
	assert.True(t, v.Sandbox, "and the verdict must say the activation came from a sandbox row")
}

// F-gov-1, the consequence: there is no way back on a sandbox tier.
//
// Once a gate is REVOKED, the only operation that clears `revoked_at` is
// `propose` (cp_gate_transition, migration 00701 step 3), and PENDING_APPROVAL
// leads only to APPROVED -- which needs a second distinct principal -- or back
// to REVOKED. So a sandbox tier that revokes a capability cannot
// sandbox-activate it again without running part of the dual-control ceremony
// the sandbox tier exists so a rehearsal need not fabricate.
//
// The model below is the transition table plus, for each operation a SINGLE
// operator can drive alone, what it does to revoked_at. It searches for any
// path from REVOKED to a usable (revoked_at IS NULL) SANDBOX row.
func TestAUDIT_ASandboxTierCanRecoverAGateItRevoked(t *testing.T) {
	t.Parallel()
	// Edges one operator may drive without a second principal:
	// propose/sandbox/unsandbox (gate:propose + step-up), suspend/revoke
	// (kill:activate). approve, resume and activate each demand a principal
	// distinct from the previous one, so they are excluded.
	solo := map[GateState][]GateState{
		StateDisabled:        {StatePendingApproval, StateRevoked, StateSandbox},
		StatePendingApproval: {StateRevoked},
		StateApproved:        {StateExpired, StateRevoked},
		StateActive:          {StateSuspended, StateExpired, StateRevoked},
		StateSuspended:       {StateRevoked},
		StateRevoked:         {StatePendingApproval, StateSandbox},
		StateExpired:         {StatePendingApproval, StateRevoked, StateSandbox},
		StateSandbox:         {StateDisabled, StateRevoked},
	}
	// What each destination does to revoked_at: `propose` clears it, `revoke`
	// sets it, entering SANDBOX clears it (migration 00791 -- the fix for this
	// finding; before it, both sandbox operations left the column exactly as
	// they found it and this search had no answer), and nothing else touches it.
	revokedAfter := func(before bool, to GateState) bool {
		switch to {
		case StatePendingApproval, StateSandbox:
			return false
		case StateRevoked:
			return true
		default:
			return before
		}
	}

	type node struct {
		state   GateState
		revoked bool
	}
	start := node{state: StateRevoked, revoked: true}
	seen := map[node]bool{start: true}
	frontier := []node{start}
	for len(frontier) > 0 {
		var next []node
		for _, n := range frontier {
			for _, to := range solo[n.state] {
				m := node{state: to, revoked: revokedAfter(n.revoked, to)}
				if m.state == StateSandbox && !m.revoked {
					return // recoverable: nothing to report
				}
				if !seen[m] {
					seen[m] = true
					next = append(next, m)
				}
			}
		}
		frontier = next
	}
	assert.Fail(t,
		"a revoked capability cannot be sandbox-activated again",
		"no sequence of single-principal transitions takes a REVOKED gate to a SANDBOX row with "+
			"revoked_at cleared, because cp_gate_sandbox never clears it and only `propose` does; "+
			"a sandbox tier that revokes a capability must run the dual-control ceremony to get it back")
}
