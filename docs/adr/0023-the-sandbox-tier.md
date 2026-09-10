# ADR-0023 — The sandbox tier

Status: **Accepted** (2026-09-10)

Supersedes nothing. Constrains every affordance by which a non-production
deployment exercises a gated surface. Answers §9 of the product goal: *"create
an explicitly isolated test/sandbox capability mechanism that cannot exist in
real-money PROD and preserves governance semantics."*

## Context

Every capability gate reaches ACTIVE by one path: a proposal, an approval by a
distinct principal, an activation by a third, four evidence references for a
high-risk capability, and a step-up inside fifteen minutes for each
(POLICY_AUTHORITY §1, migration 00701). Above the gates, the Settlement
Compiler evaluates a legal policy, and the only one a production-like
deployment may load is the conservative one, which permits simulation and
denies every internal-economy product and every payout. Above that, the
payout policy is fail-closed for every origin, the payout provider registry
refuses a provider with no contract outside LOCAL and TEST, and the
verification resolver cannot report a level above `NODAL_IDENTITY`.

Each of those is correct for PROD. Together they mean the STAGING deployment
— sandbox Stripe, a devnet mint, no live provider (config refuses one outside
PROD) — cannot buy Credits, trade a native asset, verify, or request a payout:
the product it exists to rehearse is refused before any domain code runs. The
only path through was the real ceremony with fabricated references, which the
goal forbids and which would defeat the control it imitates.

## Decision

A deployment may declare itself a **sandbox tier** with one configuration
value, `CP_API_LEGAL_POLICY=SANDBOX`, and `config.Validate` refuses that
value when `CP_ENV` is PROD. Because provider mode `live` is already refused
outside PROD, a sandbox tier is by construction a deployment where no real
value can move. Every sandbox affordance keys off that one declaration and
exists nowhere else:

1. **A `SANDBOX` gate state** (migration 00755). A gate enters it from
   DISABLED, REVOKED or EXPIRED by a single operator with `gate:propose` and a
   step-up, through a SECURITY DEFINER function that writes its own history
   row and never touches the approval chain, the evidence references or the
   validity window. It leaves only to DISABLED or REVOKED; it is not on the
   path to ACTIVE, and the real ceremony starts from DISABLED as it always
   has. A table CHECK refuses a SANDBOX row whose environment is PROD, and the
   function refuses to write one there. `gates.EvaluateWith` reads a SANDBOX
   row as active only for a Checker built with sandbox allowed, which
   `cmd/api` does exactly when the deployment is a sandbox tier; anywhere else
   the verdict is inactive with the reason "this deployment is not a sandbox
   tier". The verdict carries `Sandbox: true` and the API exposes it, so an
   active sandbox gate is never shown as an approval.
2. **Boot-time activation from the blueprint.** `CP_API_SANDBOX_GATES` lists
   the capabilities a sandbox tier activates at boot, idempotently, as the
   SYSTEM actor `config:CP_API_SANDBOX_GATES`. The authority is the blueprint
   line, reviewed like every other line there. A gate in any state that is
   not a legal source of SANDBOX — a real proposal or approval — is an error,
   never a move. Each listed capability must also be in
   `CP_API_ENABLED_CAPABILITIES`: configuration remains condition 1.
3. **A sandbox legal policy** (`legalrouter.SandboxPolicy`), evaluated by the
   real router and the real compiler, with real required capabilities. It
   permits the internal economy the way the development policy does, and —
   unlike it — permits a payout for an account whose verification is
   `PAYOUT_KYC` or higher under `PAYOUT_RESERVE`, answering
   `REQUIRES_VERIFICATION` below that. Every permission's approval reference
   reads `NOT-AN-APPROVAL-SANDBOX-TIER-ONLY`.
4. **A sandbox payout policy** (`valuedomain.SandboxPolicy`,
   `CP_API_PAYOUT_POLICY=SANDBOX`, refused in PROD and refused unless the
   legal policy is SANDBOX): purchased and earned value withdrawable once
   verified; promotional, refund, adjustment and provider-settlement value
   never. `DefaultPolicy` stays what every other deployment runs under.
5. **A sandbox payout provider** (`internal/provider/payoutsandbox`) that
   accepts, settles ten seconds later, moves nothing, answers UNKNOWN for a key
   it has never seen, and refuses to be constructed in PROD. Registered only
   when the payout slot names it on a sandbox tier. It is a first-class
   provider rather than the test double, because production wiring may not
   import a test double.

What does not change: how PROD reaches ACTIVE; the five conditions; the
`cp_gate_transition` function; the conservative policy; the fail-closed payout
default; the value-domain isolation that keeps Credits closed-loop; and the
rule that no approval reference is fabricated. A production deployment that
somehow carried any of the sandbox values refuses to start.

## Why this and not the alternatives

- *Activating the real gates in STAGING with placeholder references* fabricates
  approval and is forbidden. It would also teach the register that a gate can
  be green without the ceremony.
- *A flag that skips the router or the gates* means the code path STAGING
  exercises is not the code path PROD runs, which is the one thing worth
  knowing from a rehearsal. The sandbox tier runs the real router, compiler,
  gates, engine and ledger with a policy that says what it is.
- *Loading the development policy in STAGING* would permit the economy but
  keep payouts denied, so the withdrawal boundary — the product's most
  important new surface — could never be exercised end to end.

## Consequences

- STAGING's blueprint enables `CREDIT_PURCHASE, NATIVE_ASSET_CREATION,
  NATIVE_MARKET_TRADING, MARKETPLACE, PAYOUT_RESERVE, PAYOUT_SETTLE` and
  sandbox-activates them; its payout slot names `sandbox_payout`. Sandbox
  Stripe test cards mint sandbox Credits; native markets trade; a verified
  sandbox identity can request a payout that a provider settles without
  moving value.
- `test/integration/gates` proves a SANDBOX gate is active only for a
  sandbox-tier checker, carries no approval, cannot be proposed or approved
  from SANDBOX, and cannot exist in PROD; `test/integration/enums` keeps the
  SQL and Go state lists identical; `internal/config` proves the declaration
  is refused in PROD and the payout policy is refused without it.
- The launch-gate matrix does not move: `LIVE_READY` still needs three
  principals, four references and a licensed provider, because PROD does.

## Evidence

Migration 00755; `internal/gates/sandbox.go`, `sandbox_bootstrap.go`,
`sandbox_test.go`, `sandbox_integration_test.go`;
`internal/legalrouter/sandbox.go` and its test;
`internal/valuedomain/sandboxpolicy.go` and its test;
`internal/provider/payoutsandbox`; `cmd/api/sandboxtier.go`;
`internal/config` validation tests; `render.yaml`.
