/**
 * The mapping from the API's `AdminAction` to the shape the decision logic
 * reads.
 *
 * `decide.test.ts` proves the decision itself against every vector Go
 * generates. It cannot prove that the queue *feeds* the decision correctly,
 * because the vectors start from an already-mapped action — and that is exactly
 * where F-71 came back: `decide.ts` grew `targetId` and the check that a
 * BREAK_GLASS_GRANT's own grantee may not approve it, `decisions.json` covered
 * it, and `toDecideAction` still did not copy the field across. The console
 * rendered a live Approve button to the one person the server refuses.
 *
 * So this test asserts the mapping over the real authority document: every
 * field the decision reads arrives, and the self-elevation case comes out
 * refused.
 */
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

import type { AdminAction } from "../api.ts";
import { AuthorityIndex, parseAuthority } from "../authority.ts";
import { decide } from "../decide.ts";
import type { DecidePrincipal } from "../decide.ts";
import { toDecideAction } from "./actions.ts";

const here = path.dirname(fileURLToPath(import.meta.url));

function authority(): AuthorityIndex {
  const raw = fs.readFileSync(path.join(here, "..", "generated", "authority.json"), "utf8");
  return new AuthorityIndex(parseAuthority(JSON.parse(raw)));
}

const GRANTEE = "11111111-1111-4111-8111-111111111111";
const PROPOSER = "22222222-2222-4222-8222-222222222222";
const NOW = new Date("2026-09-10T12:00:00Z");

function grant(index: AuthorityIndex): AdminAction {
  const kind = index.doc.break_glass.kind;
  const spec = index.kind(kind);
  assert.ok(spec, `${kind} must be a declared action kind`);
  assert.equal(spec.approver_is_not_target, true, `${kind} must set approver_is_not_target`);
  return {
    id: "33333333-3333-4333-8333-333333333333",
    kind,
    target_type: "user",
    target_id: GRANTEE,
    status: "PROPOSED",
    requires_dual: true,
    proposed_by: PROPOSER,
    proposed_at: "2026-09-10T11:59:00Z",
    expires_at: "2026-09-10T12:15:00Z",
  };
}

/**
 * A principal holding a standing role that grants the kind's approve
 * permission, freshly stepped up. Read from the document rather than named, so
 * a role rename in Go arrives here on the next regeneration instead of turning
 * this into a test that passes for the wrong reason.
 */
function approver(index: AuthorityIndex, subjectId: string, permission: string): DecidePrincipal {
  const role = index.doc.roles.find((r) => r.standing && r.permissions.includes(permission));
  assert.ok(role, `no standing role grants ${permission}`);
  return {
    subjectId,
    actorType: "OPERATOR",
    roles: [role.role],
    authTime: "2026-09-10T11:59:30Z",
    amr: ["pwd", "mfa"],
    breakGlassUntil: null,
    accountIds: [],
  };
}

function approvePermission(index: AuthorityIndex): string {
  const spec = index.kind(index.doc.break_glass.kind);
  assert.ok(spec?.approve_permission, "the grant kind must declare an approve permission");
  return spec.approve_permission;
}

test("the mapping carries every field the decision reads", () => {
  const index = authority();
  const action = grant(index);
  const mapped = toDecideAction(action);
  assert.deepEqual(mapped, {
    kind: action.kind,
    status: action.status,
    requiresDual: action.requires_dual,
    proposedBy: action.proposed_by,
    approvedBy: null,
    targetId: action.target_id,
    expiresAt: action.expires_at,
  });
});

test("the grantee of a break-glass grant is refused their own approval", () => {
  const index = authority();
  const action = toDecideAction(grant(index));
  const verdict = decide(approver(index, GRANTEE, approvePermission(index)), action, "approve", NOW, index);
  assert.equal(verdict.allowed, false, "approving your own elevation is self-approval");
  assert.equal(verdict.reason, "SELF_APPROVAL");
});

test("a third party with the same permission may approve the same grant", () => {
  const index = authority();
  const action = toDecideAction(grant(index));
  const other = "44444444-4444-4444-8444-444444444444";
  const verdict = decide(approver(index, other, approvePermission(index)), action, "approve", NOW, index);
  assert.equal(verdict.allowed, true, `expected an allowed approval, got ${verdict.reason}`);
});
