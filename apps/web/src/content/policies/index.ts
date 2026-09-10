/**
 * The policy documents this deployment shows, keyed by the version id an
 * acceptance records.
 *
 * The onboarding acceptance step (`POST /v1/me/terms-acceptances`) sends these
 * ids, and the policy pages render these documents, so the words a customer
 * accepted and the words the ledger says they accepted are the same object. A
 * new version is a new id and a new entry — never an edit to an existing one,
 * because an edited document with an unchanged id makes every recorded
 * acceptance a claim about words nobody agreed to.
 */
import { PRIVACY_V1 } from "./privacy.ts";
import { RISK_V1 } from "./risk.ts";
import { TERMS_V1 } from "./terms.ts";
import type { PolicyDocument, PolicyId, PolicySlug } from "./types.ts";

export type { PolicyDocument, PolicyId, PolicySection, PolicySlug } from "./types.ts";
export { CONTACT_NOTE, DRAFT_NOTICE } from "./types.ts";

/** Every document, in the order the onboarding step presents them. */
export const POLICIES: readonly PolicyDocument[] = [TERMS_V1, PRIVACY_V1, RISK_V1];

/** The version ids the current build shows. What onboarding asks a user to accept. */
export const CURRENT_POLICY_IDS: readonly PolicyId[] = POLICIES.map((doc) => doc.id);

/** The document at a URL segment, or undefined for a segment that is not one. */
export function policyBySlug(slug: string): PolicyDocument | undefined {
  return POLICIES.find((doc) => doc.slug === slug);
}

/** The document a recorded acceptance refers to. */
export function policyById(id: string): PolicyDocument | undefined {
  return POLICIES.find((doc) => doc.id === id);
}

/** Every slug, for the router and for the footer. */
export const POLICY_SLUGS: readonly PolicySlug[] = POLICIES.map((doc) => doc.slug);
