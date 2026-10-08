/**
 * Pure helpers for the transfers section of the advanced area: customer-to-
 * customer transfers of simulated value and the sandbox faucet (ADR-0020).
 *
 * Nothing here computes a figure or decides a transfer. The backend decides
 * every rule -- limits, velocity, recipient rules, Risk Kernel V2, the
 * compliance screen, step-up, the faucet's cap -- and these functions only
 * name statuses, say which side of a transfer an account is on, check that
 * what a person typed has the shape the API accepts, and read the backend's
 * own refusal.
 */
import { ApiProblem, type Schemas } from "@controlplane/generated-client";

import type { AdvancedMode, Tone } from "./advanced.ts";

/** True only when A and B are the same set of literals. */
type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
/** Compiles only when T is `true`. */
type Assert<T extends true> = T;

/* --------------------------------------------------------------------------
 * Contract enumerations
 * ------------------------------------------------------------------------ */

export type TransferStatus = Schemas["AdvancedTransferStatus"];
/** Every transfer status, in lifecycle order. */
export const TRANSFER_STATUSES = ["PENDING", "COMPLETED", "REJECTED", "REVERSED"] as const;
export type TransferStatusesMatchContract = Assert<Same<(typeof TRANSFER_STATUSES)[number], TransferStatus>>;

/**
 * The modes a transfer exists in: the simulated modes P2P_TRANSFERS
 * implements, the database's own check. A transfer that claims any other mode
 * is refused as a contract violation rather than drawn.
 */
export const TRANSFER_MODES = ["SANDBOX", "PAPER"] as const satisfies readonly AdvancedMode[];

export type RecipientRuleKind = Schemas["AdvancedTransferRecipientRuleKind"];
export const RECIPIENT_RULE_KINDS = ["ALLOW", "BLOCK", "CLEAR"] as const;
export type RecipientRuleKindsMatchContract = Assert<Same<(typeof RECIPIENT_RULE_KINDS)[number], RecipientRuleKind>>;

/** What each status means. The status machine is enforced in SQL. */
export const TRANSFER_STATUS_MEANING: Readonly<Record<TransferStatus, string>> = {
  PENDING:
    "Being decided. A committed transfer is never left in this state, so a PENDING row is a register fault, " +
    "not a transfer in progress.",
  COMPLETED: "Moved: the simulated value left the sender and reached the recipient in one journal posting.",
  REJECTED:
    "Refused before anything moved. The refusal, its codes and its evidence are the sender's record; " +
    "the recipient is never shown it.",
  REVERSED:
    "Moved, then returned to the sender by an operator's compensating journal. The original posting is " +
    "never edited.",
};

export function transferStatusTone(status: string): Tone {
  switch (status) {
    case "COMPLETED":
      return "good";
    case "REJECTED":
      return "bad";
    case "REVERSED":
      return "warn";
    case "PENDING":
      return "info";
    default:
      return "neutral";
  }
}

/* --------------------------------------------------------------------------
 * Reading a transfer
 * ------------------------------------------------------------------------ */

/** Which side of a transfer an account is on. */
export type TransferSide = "sent" | "received" | "neither";

/**
 * The side `accountId` is on. A sender who paid a handle is never shown the
 * recipient's account id, so "received" can only be said of an account the
 * backend named as the recipient.
 */
export function transferSide(
  transfer: { readonly sender_account_id: string; readonly recipient_account_id?: string },
  accountId: string | undefined,
): TransferSide {
  if (accountId === undefined) return "neither";
  const own = accountId.toLowerCase();
  if (transfer.sender_account_id.toLowerCase() === own) return "sent";
  if (transfer.recipient_account_id !== undefined && transfer.recipient_account_id.toLowerCase() === own) {
    return "received";
  }
  return "neither";
}

/** True when an operator may be offered the reversal of this transfer: only a COMPLETED one can be reversed. */
export function reversible(status: string): boolean {
  return status === "COMPLETED";
}

/** Why the reversal is not offered for a transfer in this status. */
export function notReversibleReason(status: string): string {
  switch (status) {
    case "REVERSED":
      return "This transfer is already REVERSED; the compensating journal is shown above.";
    case "REJECTED":
      return "A REJECTED transfer moved nothing, so there is nothing to reverse.";
    default:
      return `Only a COMPLETED transfer can be reversed, and this one is ${status}.`;
  }
}

/* --------------------------------------------------------------------------
 * Reversal under dual control
 * ------------------------------------------------------------------------ */

export type TransferReversalStatus = Schemas["AdvancedTransferReversalStatus"];
/**
 * Every status of a reversal proposal. It is derived by the backend from the
 * proposal's one decision and never stored: an APPROVED proposal was executed
 * in its approval's own transaction, so it is consumed.
 */
export const TRANSFER_REVERSAL_STATUSES = ["PENDING", "APPROVED", "REJECTED"] as const;
export type TransferReversalStatusesMatchContract = Assert<
  Same<(typeof TRANSFER_REVERSAL_STATUSES)[number], TransferReversalStatus>
>;

/** What each proposal status means. Two distinct operators are enforced by the backend and its database. */
export const TRANSFER_REVERSAL_MEANING: Readonly<Record<TransferReversalStatus, string>> = {
  PENDING:
    "Proposed by one operator and awaiting a second, different operator: nothing has moved. The proposer can " +
    "never approve it; anyone holding the permission, the proposer included, may reject it.",
  APPROVED:
    "Approved by a second operator and executed in the same transaction: the compensating journal is posted and " +
    "the transfer is REVERSED. An approved proposal is consumed and can never execute again.",
  REJECTED: "Rejected with a stated reason. Nothing moved, and a rejected proposal can never be executed.",
};

export function reversalStatusTone(status: string): Tone {
  switch (status) {
    case "PENDING":
      return "info";
    case "APPROVED":
      return "warn";
    case "REJECTED":
      return "neutral";
    default:
      return "neutral";
  }
}

/** The refusal codes the backend names when a reversal decision cannot go ahead. */
export const REVERSAL_SELF_APPROVAL = "TRANSFER_REVERSAL_SELF_APPROVAL";
export const REVERSAL_FUNDS_UNAVAILABLE = "TRANSFER_REVERSAL_FUNDS_UNAVAILABLE";

/** The reversal's stable reason code, in the shape the API accepts. */
const REASON_CODE = /^[A-Z][A-Z0-9_]{2,63}$/;

/**
 * A reason code as an operator typed it: capitals, digits and underscores,
 * three to sixty-four characters, starting with a letter. It becomes the
 * compensating journal's reason code, so it is checked for shape here and
 * judged by the backend.
 */
export function checkReasonCode(raw: string): InputCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: "Enter a reason code, such as DISPUTE_UPHELD." };
  if (!REASON_CODE.test(value)) {
    return {
      ok: false,
      value: "",
      error: "A reason code is 3 to 64 capitals, digits or underscores, starting with a capital letter.",
    };
  }
  return { ok: true, value, error: "" };
}

/** Free text the API bounds at 500 bytes: a reversal's reason, a decision's reason. */
export function checkReasonText(raw: string, what: string): InputCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: `Enter ${what}.` };
  if (new TextEncoder().encode(value).length > 500) {
    return { ok: false, value: "", error: "Keep it to 500 bytes; the backend refuses anything longer." };
  }
  return { ok: true, value, error: "" };
}

/** The proposal of a transfer that is still awaiting a decision, if any. The backend allows at most one. */
export function pendingProposal<P extends { readonly status: string }>(proposals: readonly P[]): P | undefined {
  return proposals.find((proposal) => proposal.status === "PENDING");
}

/**
 * Why this operator is not offered the approval of a pending proposal, or
 * undefined when it may be offered. The backend makes the decision either
 * way; the interface only declines to offer a button it knows will be refused
 * for its own principal, and says why.
 */
export function approvalBlockedReason(proposedBy: string, subjectId: string | undefined): string | undefined {
  if (subjectId !== undefined && proposedBy.toLowerCase() === subjectId.toLowerCase()) {
    return "You proposed this reversal, and the operator who proposed it can never approve it. A second, different operator approves it; you may still reject it.";
  }
  return undefined;
}

/* --------------------------------------------------------------------------
 * What a person typed
 * ------------------------------------------------------------------------ */

export interface InputCheck {
  readonly ok: boolean;
  /** The value to send, trimmed, when `ok`. */
  readonly value: string;
  /** A message to show under the field when not `ok`. */
  readonly error: string;
}

const PLAIN_DECIMAL = /^[0-9]+(\.[0-9]+)?$/;

/**
 * A plain decimal amount in whole units of the asset, exactly as the API's
 * `amount` accepts it: digits with at most one decimal point, no sign, no
 * grouping. It is sent as typed. The backend converts it at the asset's own
 * precision and refuses (PRECISION_LOSS) an amount it would have to round;
 * nothing here rounds either.
 */
export function checkAmount(raw: string): InputCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: "Enter an amount." };
  if (!PLAIN_DECIMAL.test(value)) {
    return {
      ok: false,
      value: "",
      error: "Use digits and at most one decimal point, with no sign, spaces or grouping commas.",
    };
  }
  if (/^[0.]+$/.test(value)) return { ok: false, value: "", error: "Enter an amount greater than zero." };
  return { ok: true, value, error: "" };
}

/**
 * A handle as a person typed it. Only emptiness is judged here: the handle
 * grammar is the backend's, and it answers VALIDATION_FAILED for one it
 * cannot accept and NOT_FOUND for one that names nobody.
 */
export function checkHandle(raw: string): InputCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: "Enter a handle, such as @alice." };
  return { ok: true, value, error: "" };
}

const CANONICAL_UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** An account id: the canonical 36-character UUID. Whether it names an account is the backend's answer. */
export function checkAccountId(raw: string): InputCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: "Enter the recipient's account id." };
  if (!CANONICAL_UUID.test(value)) {
    return { ok: false, value: "", error: "An account id is a UUID in its canonical 36-character form." };
  }
  return { ok: true, value: value.toLowerCase(), error: "" };
}

/** Non-empty text, trimmed: a reason, a reason code. The backend judges the rest. */
export function checkRequired(raw: string, what: string): InputCheck {
  const value = raw.trim();
  if (value === "") return { ok: false, value: "", error: `Enter ${what}.` };
  return { ok: true, value, error: "" };
}

/* --------------------------------------------------------------------------
 * Idempotency
 * ------------------------------------------------------------------------ */

/**
 * The key a confirmation sends. It is created at the first confirmation of an
 * attempt and reused by every retry of that same attempt, so a retried send
 * can never become a second transfer. Changing the request starts a new
 * attempt, which is the caller dropping the key.
 */
export function confirmationKey(current: string | undefined, fresh: () => string): string {
  return current ?? fresh();
}

/* --------------------------------------------------------------------------
 * The backend's refusals
 * ------------------------------------------------------------------------ */

/** A refusal the backend recorded as a REJECTED transfer, read from its problem document. */
export interface RecordedRefusal {
  /** The backend's stable error code (RISK_REJECTED, INSUFFICIENT_BUYING_POWER, ...). */
  readonly code: string;
  readonly transferId: string;
  /** The primary rejection code. */
  readonly rejectionCode: string;
  /** Every code that applied, primary first. */
  readonly reasonCodes: readonly string[];
}

/**
 * Reads the recorded refusal a send answered with: a problem that names the
 * REJECTED transfer it recorded. Any other failure -- a validation, an unknown
 * handle, step-up -- recorded nothing, and this answers undefined.
 */
export function recordedRefusal(error: unknown): RecordedRefusal | undefined {
  if (!(error instanceof ApiProblem) || error.fields === undefined) return undefined;
  const transferId = error.fields["transfer_id"];
  const rejectionCode = error.fields["rejection_code"];
  if (typeof transferId !== "string" || !CANONICAL_UUID.test(transferId)) return undefined;
  if (typeof rejectionCode !== "string" || rejectionCode === "") return undefined;
  const raw = error.fields["reason_codes"];
  const reasonCodes = Array.isArray(raw) ? raw.filter((code): code is string => typeof code === "string") : [];
  return { code: error.code, transferId, rejectionCode, reasonCodes };
}
