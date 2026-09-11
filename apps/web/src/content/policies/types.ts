/**
 * The shape of a plain-language explainer for one of Nodal's legal documents.
 *
 * # What these are, and what they are emphatically not
 *
 * The BINDING documents live in `internal/terms`, are embedded in the API, and
 * are served — bytes and all — by `GET /v1/me/terms-acceptances`. An acceptance
 * records the document id, the version, and the sha256 of exactly those bytes,
 * so the only screen that may ask for an acceptance is the one that renders
 * them: `/welcome/terms`.
 *
 * These modules are something else. They are plain-language explanations of
 * what the software does, written for a visitor who has no account yet and
 * therefore cannot be shown the real documents at all — `GET /me/terms-acceptances`
 * needs a session, and this API has no public route to the legal registry. Each
 * one names the document it explains, and every page carries `SERVER_DOCUMENT_NOTE`
 * so nobody can mistake the explainer for the agreement.
 *
 * That distinction is why `id` below is called a revision and never appears in
 * a request. An earlier version of this file called it a version id and fed it
 * to the acceptance step; the server's registry is a closed enum
 * (`TERMS_OF_SERVICE`, `PRIVACY_POLICY`, …) at one version for all of them, so
 * that would have recorded an acceptance of words the customer never read.
 *
 * `LEGAL_APPROVED` is **false** in this repository
 * (docs/audit/LAUNCH_GATE_MATRIX.md), and every document the API serves is
 * marked `counsel_review_required`. Both say the same thing, and every surface
 * here repeats it. Goal §60: build the boundary, do not self-certify the law.
 */

/**
 * A document id in the server's registry. The set is closed there, and the
 * database CHECK knows the same five.
 */
export type ServerDocumentId =
  | "TERMS_OF_SERVICE"
  | "PRIVACY_POLICY"
  | "RISK_DISCLOSURE"
  | "CREDITS_TERMS"
  | "WITHDRAWAL_DISCLOSURE";

/**
 * The revision of an explainer.
 *
 * It identifies THIS text, so a reader can tell whether the explanation changed
 * under them. It is never sent to the API and never recorded anywhere: an
 * acceptance names a `ServerDocumentId` and the server's own version.
 */
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
  /** The binding document this text explains. Shown on the page. */
  readonly explains: ServerDocumentId;
  /** The `<h1>` of the page and the label in the acceptance step. */
  readonly title: string;
  /** One sentence, shown under the title and in the acceptance step. */
  readonly summary: string;
  /** ISO date this draft was written. Not an effective date: it is a draft. */
  readonly drafted: string;
  readonly sections: readonly PolicySection[];
}

/**
 * Shown at the top of every policy page.
 *
 * It is a statement of fact about this repository, not a disclaimer bolted on
 * for comfort: no lawyer has read these words, and the product may not claim
 * one has. The documents the API serves carry the same flag
 * (`counsel_review_required`), and the terms step repeats it there.
 */
export const DRAFT_NOTICE =
  "Draft pending legal review. These words were written by the team building the product, not by " +
  "a lawyer, and no counsel has approved them. They describe what the software actually does " +
  "today so that you can read it before you use it. They are not a final agreement and they are " +
  "not legal advice.";

/**
 * The line that keeps an explainer from being mistaken for the agreement.
 *
 * On every policy page, above the text. It is not small print: it is the most
 * important sentence on the page, because a reader who thinks they have read
 * the agreement has been misled by a page that meant well.
 */
export const SERVER_DOCUMENT_NOTE =
  "This page is a plain-language explanation, not the agreement. The documents Nodal asks you to " +
  "accept are served by Nodal itself and shown in full when you create an account; the acceptance " +
  "records the exact text you were shown. Where this page and that text differ, that text is what " +
  "was agreed.";

/** The line every document ends with, above the version stamp. */
export const CONTACT_NOTE =
  "There is no published contact address for legal notices yet. When one exists it will be named " +
  "here and in the footer; until then this document does not pretend otherwise.";
