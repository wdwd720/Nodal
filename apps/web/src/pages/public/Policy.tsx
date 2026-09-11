/**
 * `/terms`, `/privacy`, `/risk`, `/credits-terms`, `/withdrawal-disclosure` —
 * one component, five documents, none of them written here.
 *
 * Every word of the document comes from `GET /v1/terms` (D-080), which serves
 * the same registry and the same bytes as the acceptance endpoint. That is the
 * whole design: what a visitor reads before they have an account is what the
 * acceptance record will later name, and the `content_hash` on the page is the
 * value that record stores, so the two can be compared rather than trusted.
 *
 * The text is rendered verbatim in a `<pre>`, exactly as `/welcome/terms` does.
 * A Markdown renderer would be nicer and would also be a place where a
 * character could differ between what was displayed and what was hashed, and a
 * legal document is the wrong thing to be approximately right about.
 *
 * The draft notice is the document's own `counsel_review_required`, not a
 * constant in this repository. Every document currently served is marked, and
 * when one stops being marked this page stops saying it — which is the point of
 * reading the flag instead of asserting the fact.
 */
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { usePublicTerms, type PublicLegalDocument } from "../../api/queries.ts";
import { AsyncPanel } from "../../components/DataState.tsx";
import { IdentifierShort } from "../../components/Identifier.tsx";
import { Panel } from "../../components/Panel.tsx";
import { Refusal } from "../../components/Refusal.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import {
  ACCEPTANCE_NOTE,
  POLICY_PAGES,
  policyPageBySlug,
  type PolicySlug,
} from "../../content/policies/index.ts";
import { SitePageHead, SiteSection } from "./SiteChrome.tsx";

function Document(props: {
  readonly doc: PublicLegalDocument;
  readonly title: string;
}): ReactNode {
  return (
    <>
      <SiteSection
        title={props.doc.title}
        lead={`Version ${props.doc.version}, served by Nodal. This is the text an acceptance records.`}
      >
        <div className="policy">
          {props.doc.counsel_review_required && (
            <Refusal
              what="This document has not been reviewed by a lawyer."
              rule="Nodal marks it as requiring counsel review, and the API reports that flag on the document itself rather than as a claim by this page."
              code="counsel_review_required"
              remedy="Read it as a description of what the software does. It is not a final agreement and it is not legal advice."
            />
          )}
          <p className="policy-meta mono-small">
            <StatusBadge tone={props.doc.counsel_review_required ? "warn" : "neutral"}>
              {props.doc.counsel_review_required ? "Draft" : "Reviewed"}
            </StatusBadge>
            <span>{props.doc.document_id}</span>
            <span>version {props.doc.version}</span>
            <IdentifierShort value={props.doc.content_hash} what="content hash" />
          </p>
          <p className="note">{ACCEPTANCE_NOTE}</p>
          <div
            className="doc-scroll doc-scroll-tall"
            role="group"
            aria-label={`${props.doc.title}, full text`}
            tabIndex={0}
          >
            <pre className="doc-source">{props.doc.body}</pre>
          </div>
        </div>
      </SiteSection>

      <SiteSection title="The other documents" lead="Nodal serves five. Each one says whether it still needs a lawyer.">
        <Panel
          title="Documents"
          description="The first three apply when you create an account. The last two apply at the points they name."
        >
          <ul className="policy-index">
            {POLICY_PAGES.map((page) => (
              <li key={page.slug}>
                <Link
                  to={`/${page.slug}`}
                  aria-current={page.title === props.title ? "page" : undefined}
                >
                  {page.title}
                </Link>
              </li>
            ))}
          </ul>
        </Panel>
      </SiteSection>
    </>
  );
}

export function PolicyPage(props: { readonly slug: PolicySlug }): ReactNode {
  const page = policyPageBySlug(props.slug);
  const terms = usePublicTerms();

  // The registry is typed, so this cannot happen from inside the app. It is
  // handled anyway rather than asserted away, because the alternative to a
  // stated failure is a blank legal page.
  if (page === undefined) {
    return (
      <SiteSection title="Document not found" lead="This address does not name a legal document.">
        <Refusal
          what="This document could not be shown."
          rule="The requested address is not one of the documents this build serves."
          code="NOT_FOUND"
          remedy="Use one of the documents listed in the footer."
        />
      </SiteSection>
    );
  }

  return (
    <>
      <SitePageHead title={page.title} lead={page.lead} />
      <AsyncPanel
        query={terms}
        loadingLabel="Loading the document from Nodal…"
        errorExtra={
          <p className="note">
            The document could not be read from the API. Nothing is shown in its place: a summary
            written here would not be the text you would be asked to accept.
          </p>
        }
      >
        {(documents: PublicLegalDocument[]) => {
          const doc = documents.find((candidate) => candidate.document_id === page.documentId);
          if (doc === undefined) {
            return (
              <SiteSection title={page.title} lead="This deployment does not serve this document.">
                <div className="policy">
                  <Refusal
                    what={`Nodal is not serving ${page.documentId} at the moment.`}
                    rule="The public registry answered without it, and this page will not substitute text of its own."
                    code="NOT_SERVED"
                    remedy="The other documents are listed in the footer."
                  />
                </div>
              </SiteSection>
            );
          }
          return <Document doc={doc} title={page.title} />;
        }}
      </AsyncPanel>
    </>
  );
}
