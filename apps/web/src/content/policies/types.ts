/**
 * The shape of a policy document, and the fact that none of them is approved.
 *
 * The documents themselves are plain typed strings in sibling modules so that
 * legal copy can be replaced without a code rewrite (goal §48): a page renders
 * whatever `POLICIES` holds, and the onboarding acceptance step records the
 * `id` it showed. The id is the version — `terms-v1`, not `terms` — because an
 * acceptance that does not name a version is not an acceptance of anything.
 *
 * `LEGAL_APPROVED` is **false** in this repository (docs/audit/LAUNCH_GATE_MATRIX.md).
 * Every document therefore carries `DRAFT_NOTICE` on the page, and nothing here
 * may say or imply that counsel has reviewed it. Goal §60: build the boundary,
 * do not self-certify the law.
 */

/** A version identifier. It is what an acceptance row stores. */
export type PolicyId = "terms-v1" | "privacy-v1" | "risk-v1";

/** The URL segment a document lives at. */
export type PolicySlug = "terms" | "privacy" | "risk";

export interface PolicySection {
  readonly heading: string;
  readonly paragraphs: readonly string[];
  /** Rendered as a list under the paragraphs, when the section has one. */
  readonly bullets?: readonly string[];
}

export interface PolicyDocument {
  readonly id: PolicyId;
  readonly slug: PolicySlug;
  /** The `<h1>` of the page and the label in the acceptance step. */
  readonly title: string;
  /** One sentence, shown under the title and in the acceptance step. */
  readonly summary: string;
  /** ISO date this draft was written. Not an effective date: it is a draft. */
  readonly drafted: string;
  readonly sections: readonly PolicySection[];
}

/**
 * Shown at the top of every policy page and beside every acceptance control.
 *
 * It is a statement of fact about this repository, not a disclaimer bolted on
 * for comfort: no lawyer has read these words, and the product may not claim
 * one has.
 */
export const DRAFT_NOTICE =
  "Draft pending legal review. These words were written by the team building the product, not by " +
  "a lawyer, and no counsel has approved them. They describe what the software actually does " +
  "today so that you can read it before you use it. They are not a final agreement and they are " +
  "not legal advice.";

/** The line every document ends with, above the version stamp. */
export const CONTACT_NOTE =
  "There is no published contact address for legal notices yet. When one exists it will be named " +
  "here and in the footer; until then this document does not pretend otherwise.";
