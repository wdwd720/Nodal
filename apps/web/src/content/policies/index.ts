/**
 * The plain-language explainers the public site serves, and the binding
 * documents they explain.
 *
 * The acceptance step does NOT read this module. `POST /v1/me/terms-acceptances`
 * names a document in the server's own registry and records the sha256 of the
 * bytes the server served, so the only honest place to ask for an acceptance is
 * the screen that renders those bytes — `/welcome/terms`. What lives here is
 * the explanation a visitor can read before they have a session at all, because
 * the legal registry has no public route.
 */
import { PRIVACY_V1 } from "./privacy.ts";
import { RISK_V1 } from "./risk.ts";
import { TERMS_V1 } from "./terms.ts";
import type { PolicyDocument, PolicyId, PolicySlug, ServerDocumentId } from "./types.ts";

export type { PolicyDocument, PolicyId, PolicySection, PolicySlug, ServerDocumentId } from "./types.ts";
export { CONTACT_NOTE, DRAFT_NOTICE, SERVER_DOCUMENT_NOTE } from "./types.ts";

/** Every document, in the order the onboarding step presents them. */
export const POLICIES: readonly PolicyDocument[] = [TERMS_V1, PRIVACY_V1, RISK_V1];

/**
 * The revisions of the explainers this build serves.
 *
 * Exported for a page that wants to stamp what a reader saw. It is deliberately
 * NOT named after acceptance and is never sent anywhere: the ids an acceptance
 * records are `ServerDocumentId`s, and the authority on which of those are
 * outstanding is `GET /v1/me/terms-acceptances`.
 */
export const EXPLAINER_REVISIONS: readonly PolicyId[] = POLICIES.map((doc) => doc.id);

/**
 * Which binding document each public page explains, so the two can be shown
 * side by side rather than left to look like alternatives.
 */
export const EXPLAINED_DOCUMENTS: Readonly<Record<PolicySlug, ServerDocumentId>> = Object.fromEntries(
  POLICIES.map((doc) => [doc.slug, doc.explains]),
) as Readonly<Record<PolicySlug, ServerDocumentId>>;

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
