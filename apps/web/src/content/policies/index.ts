/**
 * Which public page shows which legal document, and the one line of context
 * above each.
 *
 * # What used to be here, and why it is gone
 *
 * This module held three hand-written policy documents with version ids of
 * their own. They existed for one reason: `GET /me/terms-acceptances` needed a
 * session, so a visitor with no account could not be shown the real text at
 * all, and a plain-language explainer was the honest maximum. It was still two
 * texts about the same subject, and a reader could only be misled by the one
 * that was not binding.
 *
 * D-080 removed the reason. `GET /v1/terms` serves the registry — bodies and
 * all, same bytes and same `content_hash` as the acceptance endpoint — without
 * a session, so the public pages render the document itself. The explainers,
 * their revision ids and `SERVER_DOCUMENT_NOTE` retired with the gap they were
 * covering.
 *
 * What is left is the smallest thing that still has to live in the client: a
 * document is identified by an enum value, and `/terms` is a nicer address than
 * `/TERMS_OF_SERVICE`. The leads below are one sentence each, they make no
 * claim the document does not, and they are the only words on those pages that
 * are not the server's.
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

/** The URL segment a document lives at. */
export type PolicySlug =
  | "terms"
  | "privacy"
  | "risk"
  | "credits-terms"
  | "withdrawal-disclosure";

export interface PolicyPageSpec {
  readonly slug: PolicySlug;
  readonly documentId: ServerDocumentId;
  /** The page's `<h1>`, so the address and the heading agree before the fetch. */
  readonly title: string;
  /** One sentence of context. It never restates or softens the document. */
  readonly lead: string;
}

/**
 * The five documents, in the order the footer lists them: the three a new
 * account is asked to accept, then the two that apply later.
 */
export const POLICY_PAGES: readonly PolicyPageSpec[] = [
  {
    slug: "terms",
    documentId: "TERMS_OF_SERVICE",
    title: "Terms of Service",
    lead: "What Nodal is, what it is not, and the rules for using it.",
  },
  {
    slug: "privacy",
    documentId: "PRIVACY_POLICY",
    title: "Privacy Notice",
    lead: "What personal data the product holds, who else sees it, and what it deliberately never has.",
  },
  {
    slug: "risk",
    documentId: "RISK_DISCLOSURE",
    title: "Risk Disclosure",
    lead: "What can go wrong, written to be read before you spend anything.",
  },
  {
    slug: "credits-terms",
    documentId: "CREDITS_TERMS",
    title: "Credits Terms",
    lead: "What a Credit is, how it enters and leaves, and why it is not money.",
  },
  {
    slug: "withdrawal-disclosure",
    documentId: "WITHDRAWAL_DISCLOSURE",
    title: "Withdrawal and Verification Disclosure",
    lead: "What has to be true before value can leave Nodal. It is asked for when you request a withdrawal, not at sign-up.",
  },
];

/** The pages the footer links. All five: a document with no page is a document nobody reads. */
export const POLICY_SLUGS: readonly PolicySlug[] = POLICY_PAGES.map((page) => page.slug);

export function policyPageBySlug(slug: string): PolicyPageSpec | undefined {
  return POLICY_PAGES.find((page) => page.slug === slug);
}

/**
 * Shown above every document, under the lead.
 *
 * It says where acceptance happens, because the page itself records nothing:
 * reading a document here is not agreeing to it, and a page that let somebody
 * believe otherwise would be the one dishonest surface on the site.
 */
export const ACCEPTANCE_NOTE =
  "Reading this page records nothing. You are asked to accept the documents that apply when you " +
  "create an account, and the acceptance names the document, its version and the exact text you " +
  "were shown — which is the text below.";
