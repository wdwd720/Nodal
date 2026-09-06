// Package security defines the authorization model of the control plane:
// roles, actor types, the static role→permission matrix, the Principal that
// every request carries in its context, and the Require* checks that every
// command and query handler must call before touching domain state.
//
// Responsibilities
//
//   - Roles (PART 91) and the fixed permission matrix (RolePermissions), with
//     a golden copy in the tests so any privilege expansion fails CI.
//   - Tenant scoping (PART 92): RequireAccount refuses cross-tenant access
//     unless the principal holds account:read_any.
//   - Agent containment (PART 9): an AGENT principal is structurally unable
//     to carry roles; it receives only AgentPermissions on its single bound
//     account, and can never satisfy kill/withdrawal/gate/ledger/risk
//     permissions.
//   - Dual control: the approve-side permissions (IsDualControl) are held by
//     no standing role. They are granted only through the time-boxed
//     BREAK_GLASS role while Principal.BreakGlassUntil is in the future, and
//     RequireDualControl refuses self-approval.
//   - Step-up (PART 90/93): RequireStepUp demands a recent AuthTime and a
//     strong AMR value (see StrongAMR).
//
// Clocks are injected as func() time.Time (a clock.Clock's Now method value
// satisfies it). Require, RequireAny and RequireAccount use the wall clock
// only to decide whether a break-glass grant is still live; the *At variants
// take the clock explicitly and are what tests and long-running code use.
//
// This package must never:
//
//   - derive authority from anything a client supplies (headers, query
//     parameters, cookies, JSON bodies). A Principal is only ever attached by
//     the session middleware from a server-side session;
//   - grant roles to an AGENT actor, or evaluate roles on one if present;
//   - grant an approve-side (dual-control) permission to a standing role;
//   - let a break-glass grant outlive Principal.BreakGlassUntil;
//   - trust identifier obscurity in place of RequireAccount;
//   - import packages that hold connections, keys or provider clients.
package security
