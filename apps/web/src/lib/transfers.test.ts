/**
 * The transfers section's pure helpers (src/lib/transfers.ts) and its runtime
 * contract (src/api/advanced/transfers.ts).
 *
 * Each spec is shown a realistic answer, which it must accept unchanged, and
 * then that answer broken one way at a time, which it must refuse with a
 * ContractViolation naming the field: a refused answer becomes an explicit
 * error state on the page, an accepted malformed one would become a figure.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import { ApiProblem } from "@controlplane/generated-client";

import { advancedKeys } from "../api/advanced/core.ts";
import {
  faucetAllowanceSpec,
  faucetGrantSpec,
  handleSpec,
  reversalDecisionResultSpec,
  reversalProposalPageSpec,
  reversalProposalSpec,
  transferKeys,
  transferPageSpec,
  transferSpec,
} from "../api/advanced/transfers.ts";
import { ContractViolation, validated, type Spec } from "../api/contract.ts";
import {
  REVERSAL_SELF_APPROVAL,
  TRANSFER_MODES,
  TRANSFER_REVERSAL_MEANING,
  TRANSFER_REVERSAL_STATUSES,
  TRANSFER_STATUSES,
  TRANSFER_STATUS_MEANING,
  approvalBlockedReason,
  checkReasonCode,
  checkReasonText,
  pendingProposal,
  reversalStatusTone,
  checkAccountId,
  checkAmount,
  checkHandle,
  checkRequired,
  confirmationKey,
  notReversibleReason,
  recordedRefusal,
  reversible,
  transferSide,
  transferStatusTone,
} from "./transfers.ts";

const ALICE = "01a075f1-f847-7a98-adbc-5fc08c37e83f";
const BOB = "01a075f1-f846-719e-bcbb-493c8281c524";
const TRANSFER = "01a075f1-f82c-78ad-b6cb-a0b5e6e58683";
const SANDBOX_DOLLAR = "01920000-0000-7000-8000-5342582d5553";
const HASH = "9f2c4be1d0a7c3e85b16f4a2d9e07c31a5b8f6e2c4d1a9b7e3f05c8d2a6b4e19";

/** A COMPLETED transfer as its sender sees it after paying a handle: no recipient account id. */
const SENT_BY_HANDLE = {
  id: TRANSFER,
  intent_id: BOB,
  status: "COMPLETED",
  sender_account_id: ALICE,
  recipient_handle: "bob",
  asset_id: SANDBOX_DOLLAR,
  asset_symbol: "SBX-USD",
  asset_decimals: 6,
  quantity: "12345678",
  notional_usd: "12.35",
  mode: "SANDBOX",
  memo: "rent for october",
  reason_codes: [],
  policy_version: "transfers-sandbox-v1",
  policy_hash: HASH,
  risk_decision_id: ALICE,
  effect_id: BOB,
  journal_transaction_id: TRANSFER,
  evidence_ref: "proof://advanced/p2p/x.json",
  evidence_sha256: HASH,
  decided_at: "2026-10-07T12:00:00Z",
  created_at: "2026-10-07T12:00:00Z",
  updated_at: "2026-10-07T12:00:00Z",
  value_domain: "SIMULATED",
} as const;

function broken<T>(value: T, change: (copy: Record<string, unknown>) => void): unknown {
  const copy = structuredClone(value) as Record<string, unknown>;
  change(copy);
  return copy;
}

function refuses(value: unknown, spec: Spec, path: RegExp): void {
  assert.throws(
    () => validated(value, spec, "/t"),
    (error: unknown) => {
      assert.ok(error instanceof ContractViolation, `expected a ContractViolation, got ${String(error)}`);
      assert.match(error.path, path);
      return true;
    },
  );
}

/* --------------------------------------------------------------------------
 * The contract
 * ------------------------------------------------------------------------ */

test("a transfer as its sender sees it is accepted unchanged", () => {
  assert.doesNotThrow(() => validated(SENT_BY_HANDLE, transferSpec, "/t"));
  // The recipient's account id is optional: a sender who paid a handle is not shown it.
  assert.equal("recipient_account_id" in SENT_BY_HANDLE, false);
  const received = broken(SENT_BY_HANDLE, (copy) => {
    copy["recipient_account_id"] = BOB;
    delete copy["risk_decision_id"];
    delete copy["evidence_ref"];
    delete copy["evidence_sha256"];
  });
  assert.doesNotThrow(() => validated(received, transferSpec, "/t"));
});

test("a transfer that is not simulated, not exact or not a declared state is refused", () => {
  refuses(broken(SENT_BY_HANDLE, (c) => delete c["value_domain"]), transferSpec, /\.value_domain$/);
  refuses(broken(SENT_BY_HANDLE, (c) => (c["value_domain"] = "REAL")), transferSpec, /\.value_domain$/);
  refuses(broken(SENT_BY_HANDLE, (c) => (c["mode"] = "LIVE")), transferSpec, /\.mode$/);
  refuses(broken(SENT_BY_HANDLE, (c) => (c["mode"] = "DEVNET")), transferSpec, /\.mode$/);
  refuses(broken(SENT_BY_HANDLE, (c) => (c["status"] = "SETTLED")), transferSpec, /\.status$/);
  refuses(broken(SENT_BY_HANDLE, (c) => (c["quantity"] = "12.345678")), transferSpec, /\.quantity$/);
  refuses(broken(SENT_BY_HANDLE, (c) => (c["quantity"] = 12345678)), transferSpec, /\.quantity$/);
  refuses(broken(SENT_BY_HANDLE, (c) => (c["notional_usd"] = "12.3")), transferSpec, /\.notional_usd$/);
  refuses(broken(SENT_BY_HANDLE, (c) => (c["asset_decimals"] = "6")), transferSpec, /\.asset_decimals$/);
  refuses(broken(SENT_BY_HANDLE, (c) => delete c["reason_codes"]), transferSpec, /\.reason_codes$/);
  refuses(broken(SENT_BY_HANDLE, (c) => (c["recipient_account_id"] = "bob")), transferSpec, /\.recipient_account_id$/);
});

test("a page of transfers states its domain and its cursor", () => {
  const page = { items: [SENT_BY_HANDLE], next_cursor: null, value_domain: "SIMULATED" };
  assert.doesNotThrow(() => validated(page, transferPageSpec, "/t"));
  assert.doesNotThrow(() => validated({ ...page, next_cursor: TRANSFER }, transferPageSpec, "/t"));
  refuses(broken(page, (c) => delete c["next_cursor"]), transferPageSpec, /\.next_cursor$/);
  refuses(broken(page, (c) => delete c["value_domain"]), transferPageSpec, /\.value_domain$/);
  refuses(
    broken(page, (c) => (c["items"] = [{ ...SENT_BY_HANDLE, quantity: "1e6" }])),
    transferPageSpec,
    /\.items\[0\]\.quantity$/,
  );
});

test("the faucet's allowance and grant are exact base units in the simulated domain", () => {
  const allowance = {
    account_id: ALICE,
    asset_id: SANDBOX_DOLLAR,
    asset_symbol: "SBX-USD",
    asset_decimals: 6,
    cap: "100000000000",
    used: "2500000000",
    remaining: "97500000000",
    window_ms: 86400000,
    policy_version: "sandbox-faucet-v1",
    policy_hash: HASH,
    environment: "LOCAL",
    value_domain: "SIMULATED",
  };
  assert.doesNotThrow(() => validated(allowance, faucetAllowanceSpec, "/t"));
  refuses(broken(allowance, (c) => (c["remaining"] = "97500.000000")), faucetAllowanceSpec, /\.remaining$/);
  refuses(broken(allowance, (c) => delete c["cap"]), faucetAllowanceSpec, /\.cap$/);
  refuses(broken(allowance, (c) => (c["value_domain"] = "SELF_CUSTODIAL_CRYPTO")), faucetAllowanceSpec, /\.value_domain$/);

  const grant = {
    id: TRANSFER,
    account_id: ALICE,
    asset_id: SANDBOX_DOLLAR,
    asset_symbol: "SBX-USD",
    asset_decimals: 6,
    quantity: "2500000000",
    policy_version: "sandbox-faucet-v1",
    policy_hash: HASH,
    environment: "LOCAL",
    effect_id: BOB,
    journal_transaction_id: ALICE,
    granted_at: "2026-10-07T12:00:00Z",
    value_domain: "SIMULATED",
  };
  assert.doesNotThrow(() => validated(grant, faucetGrantSpec, "/t"));
  refuses(broken(grant, (c) => delete c["journal_transaction_id"]), faucetGrantSpec, /\.journal_transaction_id$/);
  refuses(broken(grant, (c) => (c["quantity"] = "-1.5")), faucetGrantSpec, /\.quantity$/);

  assert.doesNotThrow(() =>
    validated({ id: TRANSFER, account_id: ALICE, handle: "alice", created_at: "2026-10-07T12:00:00Z" }, handleSpec, "/t"),
  );
  refuses({ id: TRANSFER, account_id: ALICE, created_at: "2026-10-07T12:00:00Z" }, handleSpec, /\.handle$/);
});

test("every transfer key sits under its account's advanced prefix", () => {
  const prefix = advancedKeys.account(ALICE);
  for (const key of [transferKeys.list(ALICE, "", 25), transferKeys.one(ALICE, TRANSFER), transferKeys.allowance(ALICE)]) {
    assert.deepEqual(key.slice(0, prefix.length), [...prefix]);
  }
  assert.notDeepEqual(transferKeys.list(ALICE, "", 25), transferKeys.list(BOB, "", 25));
});

/* --------------------------------------------------------------------------
 * Statuses, sides, reversal
 * ------------------------------------------------------------------------ */

test("every status is named and toned, and only a COMPLETED transfer is offered for reversal", () => {
  assert.equal(new Set(TRANSFER_STATUSES).size, TRANSFER_STATUSES.length);
  for (const status of TRANSFER_STATUSES) {
    assert.ok(TRANSFER_STATUS_MEANING[status].length > 20, status);
    assert.equal(reversible(status), status === "COMPLETED", status);
    if (!reversible(status)) assert.ok(notReversibleReason(status).length > 20, status);
  }
  assert.equal(transferStatusTone("COMPLETED"), "good");
  assert.equal(transferStatusTone("REJECTED"), "bad");
  assert.equal(transferStatusTone("SOMETHING_NEW"), "neutral");
  assert.deepEqual([...TRANSFER_MODES].sort(), ["PAPER", "SANDBOX"]);
});

test("the side of a transfer is the backend's naming of the account, never a guess", () => {
  assert.equal(transferSide(SENT_BY_HANDLE, ALICE), "sent");
  assert.equal(transferSide(SENT_BY_HANDLE, ALICE.toUpperCase()), "sent");
  // Paid by handle: the recipient's id is absent, so no account is "the recipient" here.
  assert.equal(transferSide(SENT_BY_HANDLE, BOB), "neither");
  assert.equal(transferSide({ ...SENT_BY_HANDLE, recipient_account_id: BOB }, BOB), "received");
  assert.equal(transferSide(SENT_BY_HANDLE, undefined), "neither");
});

/* --------------------------------------------------------------------------
 * What a person typed
 * ------------------------------------------------------------------------ */

test("an amount is sent as typed when it is a plain decimal, and refused otherwise", () => {
  for (const ok of ["1", "12.345678", "0.5", "1000", " 25 ", "007"]) {
    const result = checkAmount(ok);
    assert.equal(result.ok, true, ok);
    assert.equal(result.value, ok.trim(), "nothing is normalised beyond trimming");
  }
  for (const bad of ["", "   ", "-1", "+1", "1e6", "1,000", "1.", ".5", "1.2.3", "ten", "0", "0.000"]) {
    const result = checkAmount(bad);
    assert.equal(result.ok, false, bad);
    assert.ok(result.error.length > 0, bad);
  }
});

test("a handle, an account id and a reason are checked only for what the page can know", () => {
  assert.deepEqual(checkHandle("  @Bob "), { ok: true, value: "@Bob", error: "" });
  assert.equal(checkHandle(" ").ok, false);
  assert.deepEqual(checkAccountId(` ${BOB.toUpperCase()} `), { ok: true, value: BOB, error: "" });
  assert.equal(checkAccountId("not-an-id").ok, false);
  assert.equal(checkAccountId("").ok, false);
  assert.deepEqual(checkRequired(" DISPUTE_UPHELD ", "a code"), { ok: true, value: "DISPUTE_UPHELD", error: "" });
  assert.equal(checkRequired("", "a code").error, "Enter a code.");
});

test("a confirmation creates a key once and every retry of the attempt reuses it", () => {
  let created = 0;
  const fresh = (): string => {
    created = created + 1;
    return `key-${String(created)}-000000`;
  };
  const first = confirmationKey(undefined, fresh);
  assert.equal(first, "key-1-000000");
  assert.equal(confirmationKey(first, fresh), first, "a retry reuses the attempt's key");
  assert.equal(confirmationKey(first, fresh), first);
  assert.equal(created, 1, "no second key for the same attempt");
  assert.equal(confirmationKey(undefined, fresh), "key-2-000000", "a new attempt gets a new key");
});

/* --------------------------------------------------------------------------
 * The backend's refusals
 * ------------------------------------------------------------------------ */

function problem(code: string, status: number, fields?: Record<string, unknown>): ApiProblem {
  return new ApiProblem({ type: "about:blank", title: code, status, code, ...(fields === undefined ? {} : { fields }) });
}

test("a recorded refusal is read from the problem the backend answered with", () => {
  const refused = problem("RISK_REJECTED", 422, {
    transfer_id: TRANSFER,
    rejection_code: "RISK_IDENTITY_UNVERIFIED",
    reason_codes: ["RISK_IDENTITY_UNVERIFIED", 7, "RISK_VELOCITY_COUNT"],
  });
  assert.deepEqual(recordedRefusal(refused), {
    code: "RISK_REJECTED",
    transferId: TRANSFER,
    rejectionCode: "RISK_IDENTITY_UNVERIFIED",
    reasonCodes: ["RISK_IDENTITY_UNVERIFIED", "RISK_VELOCITY_COUNT"],
  });

  // Refusals that recorded nothing name no transfer.
  assert.equal(recordedRefusal(problem("NOT_FOUND", 404)), undefined);
  assert.equal(recordedRefusal(problem("STEP_UP_REQUIRED", 403, { step_up_above: "1000.00" })), undefined);
  assert.equal(recordedRefusal(problem("INVALID_IDEMPOTENCY_REUSE", 409, { transfer_id: TRANSFER })), undefined);
  assert.equal(recordedRefusal(problem("RISK_REJECTED", 422, { transfer_id: "x", rejection_code: "R" })), undefined);
  assert.equal(recordedRefusal(new Error("network")), undefined);
  assert.equal(recordedRefusal(undefined), undefined);
});

/* --------------------------------------------------------------------------
 * Reversal under dual control
 * ------------------------------------------------------------------------ */

const OPERATOR_A = "01a075f1-f848-7d10-8f3a-1c0e5e2f4a11";
const OPERATOR_B = "01a075f1-f849-7c2b-9a4d-2d1f6f3e5b22";

/** A PENDING proposal, with the transfer it would reverse as an operator sees it. */
const PENDING_PROPOSAL = {
  id: BOB,
  transfer_id: TRANSFER,
  seq: 1,
  status: "PENDING",
  reason_code: "DISPUTE_UPHELD",
  reason: "the sender reported the payment as unauthorised",
  proposed_by: OPERATOR_A,
  proposed_at: "2026-10-08T12:00:00Z",
  transfer: { ...SENT_BY_HANDLE, recipient_account_id: BOB },
  value_domain: "SIMULATED",
} as const;

test("a reversal proposal is accepted unchanged, pending or decided", () => {
  assert.doesNotThrow(() => validated(PENDING_PROPOSAL, reversalProposalSpec, "/t"));
  const approved = {
    ...PENDING_PROPOSAL,
    status: "APPROVED",
    decided_by: OPERATOR_B,
    decided_at: "2026-10-08T12:05:00Z",
    transfer: {
      ...PENDING_PROPOSAL.transfer,
      status: "REVERSED",
      reversal_journal_transaction_id: ALICE,
      reversal_reason_code: "DISPUTE_UPHELD",
      reversal_proposal_id: BOB,
      reversed_at: "2026-10-08T12:05:00Z",
    },
  };
  assert.doesNotThrow(() => validated(approved, reversalProposalSpec, "/t"));
  assert.doesNotThrow(() =>
    validated({ proposal: approved, replayed: true, value_domain: "SIMULATED" }, reversalDecisionResultSpec, "/t"),
  );
  const page = { items: [PENDING_PROPOSAL], next_cursor: null, value_domain: "SIMULATED" };
  assert.doesNotThrow(() => validated(page, reversalProposalPageSpec, "/t"));
});

test("a reversal proposal that is not simulated, not a declared state or lacks its transfer is refused", () => {
  refuses(broken(PENDING_PROPOSAL, (c) => (c["status"] = "CONSUMED")), reversalProposalSpec, /\.status$/);
  refuses(broken(PENDING_PROPOSAL, (c) => delete c["value_domain"]), reversalProposalSpec, /\.value_domain$/);
  refuses(broken(PENDING_PROPOSAL, (c) => delete c["transfer"]), reversalProposalSpec, /\.transfer$/);
  refuses(broken(PENDING_PROPOSAL, (c) => delete c["proposed_by"]), reversalProposalSpec, /\.proposed_by$/);
  refuses(broken(PENDING_PROPOSAL, (c) => (c["seq"] = "1")), reversalProposalSpec, /\.seq$/);
  refuses(
    broken(PENDING_PROPOSAL, (c) => (c["transfer"] = { ...PENDING_PROPOSAL.transfer, quantity: "12.5" })),
    reversalProposalSpec,
    /\.transfer\.quantity$/,
  );
  refuses(
    broken(PENDING_PROPOSAL, (c) => (c["transfer"] = { ...PENDING_PROPOSAL.transfer, reversal_proposal_id: "p-1" })),
    reversalProposalSpec,
    /\.transfer\.reversal_proposal_id$/,
  );
  refuses({ proposal: PENDING_PROPOSAL, value_domain: "SIMULATED" }, reversalDecisionResultSpec, /\.replayed$/);
  refuses({ items: [PENDING_PROPOSAL], value_domain: "SIMULATED" }, reversalProposalPageSpec, /\.next_cursor$/);
});

test("every proposal status is named and toned; the reversal keys sit under the advanced prefix", () => {
  assert.deepEqual([...TRANSFER_REVERSAL_STATUSES], ["PENDING", "APPROVED", "REJECTED"]);
  for (const status of TRANSFER_REVERSAL_STATUSES) {
    assert.ok(TRANSFER_REVERSAL_MEANING[status].length > 40, status);
  }
  assert.match(TRANSFER_REVERSAL_MEANING.PENDING, /never approve/);
  assert.match(TRANSFER_REVERSAL_MEANING.APPROVED, /consumed/);
  assert.equal(reversalStatusTone("PENDING"), "info");
  assert.equal(reversalStatusTone("SOMETHING_NEW"), "neutral");
  const prefix = advancedKeys.all;
  assert.deepEqual(transferKeys.reversals.slice(0, prefix.length), [...prefix]);
  assert.deepEqual(transferKeys.reversalList(TRANSFER, "PENDING", "").slice(0, transferKeys.reversals.length), [
    ...transferKeys.reversals,
  ]);
});

test("a reason code and a reason are checked for the shape the API accepts", () => {
  assert.deepEqual(checkReasonCode(" DISPUTE_UPHELD "), { ok: true, value: "DISPUTE_UPHELD", error: "" });
  for (const raw of ["", "dispute_upheld", "DI", "1DISPUTE", "DISPUTE-UPHELD", `D${"X".repeat(64)}`]) {
    assert.equal(checkReasonCode(raw).ok, false, raw);
  }
  assert.equal(checkReasonCode(`D${"X".repeat(63)}`).ok, true, "64 characters is the bound");
  assert.equal(checkReasonText("  why  ", "a reason").value, "why");
  assert.equal(checkReasonText("   ", "a reason").ok, false);
  assert.equal(checkReasonText("x".repeat(500), "a reason").ok, true);
  assert.equal(checkReasonText("é".repeat(251), "a reason").ok, false, "the bound is 500 bytes, not characters");
});

test("at most one proposal is pending, and its proposer is never offered the approval", () => {
  assert.equal(pendingProposal([]), undefined);
  assert.equal(
    pendingProposal([{ status: "REJECTED" }, { status: "PENDING", id: "p2" }, { status: "APPROVED" }])?.status,
    "PENDING",
  );
  const blocked = approvalBlockedReason(OPERATOR_A, OPERATOR_A.toUpperCase());
  assert.ok(blocked !== undefined && /can never approve/.test(blocked));
  assert.equal(approvalBlockedReason(OPERATOR_A, OPERATOR_B), undefined);
  assert.equal(approvalBlockedReason(OPERATOR_A, undefined), undefined, "an unknown principal is left to the backend");
  assert.equal(REVERSAL_SELF_APPROVAL, "TRANSFER_REVERSAL_SELF_APPROVAL");
});
