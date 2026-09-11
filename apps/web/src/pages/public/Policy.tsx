/**
 * `/terms`, `/privacy`, `/risk` — one component, three documents.
 *
 * The page is deliberately thin. Everything a reader sees comes from
 * `src/content/policies/`, which is where legal copy can be replaced without
 * touching a component (goal §48), and the version id the page stamps is the
 * same id the onboarding acceptance step records — so the words somebody
 * accepted and the words the ledger says they accepted are the same object.
 *
 * Two things are non-negotiable on this page and are therefore not props:
 *
 *   1. the version identifier and the draft date are always rendered;
 *   2. `DRAFT_NOTICE` is always rendered, at the top, as a warning rather than
 *      as small print. `LEGAL_APPROVED` is false in this repository, and a
 *      document that hid that would be the one dishonest page on a site whose
 *      whole argument is that it does not have one.
 */
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { Panel } from "../../components/Panel.tsx";
import { Refusal } from "../../components/Refusal.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import {
  CONTACT_NOTE,
  DRAFT_NOTICE,
  POLICIES,
  SERVER_DOCUMENT_NOTE,
  policyBySlug,
  type PolicySlug,
} from "../../content/policies/index.ts";
import { SitePageHead, SiteSection } from "./SiteChrome.tsx";

export function PolicyPage(props: { readonly slug: PolicySlug }): ReactNode {
  const doc = policyBySlug(props.slug);

  // The registry is typed, so this cannot happen from inside the app. It is
  // handled anyway rather than asserted away, because the alternative to a
  // stated failure is a blank legal page.
  if (doc === undefined) {
    return (
      <SiteSection title="Document not found" lead="This address does not name a policy document.">
        <Refusal
          what="This policy document could not be shown."
          rule="The requested document is not in this build's policy registry."
          code="NOT_FOUND"
          remedy="Use one of the documents listed in the footer."
        />
      </SiteSection>
    );
  }

  return (
    <>
      <SitePageHead title={doc.title} lead={doc.summary}>
        <p className="policy-meta mono-small">
          <StatusBadge tone="warn">Draft</StatusBadge>
          <StatusBadge tone="info">Explains {doc.explains}</StatusBadge>
          <span>explainer {doc.id}</span>
          <span>written {doc.drafted}</span>
        </p>
      </SitePageHead>

      <SiteSection title="Before you read it" lead="Two statements of fact about this page.">
        <div className="policy">
          <Refusal
            what="This page is an explanation, not the agreement."
            rule={SERVER_DOCUMENT_NOTE}
            code={doc.explains}
            remedy="The document itself is shown in full, and recorded as you accept it, when you create an account."
          >
            <p className="refusal-body">
              <Link to="/get-started">Create an account</Link> to read it.
            </p>
          </Refusal>
          <Refusal
            what="Nothing here has been reviewed by a lawyer."
            rule={DRAFT_NOTICE}
            code="LEGAL_APPROVED=false"
            remedy="Read it as a description of what the software does. It is not a final agreement and it is not legal advice."
          />
        </div>
      </SiteSection>

      <SiteSection title={doc.title} lead={`Explainer ${doc.id}, written ${doc.drafted}. It explains ${doc.explains}.`}>
        <article className="policy">
          {doc.sections.map((section) => (
            <section key={section.heading}>
              <h2>{section.heading}</h2>
              {section.paragraphs.map((paragraph) => (
                <p key={paragraph}>{paragraph}</p>
              ))}
              {section.bullets !== undefined && (
                <ul>
                  {section.bullets.map((bullet) => (
                    <li key={bullet}>{bullet}</li>
                  ))}
                </ul>
              )}
            </section>
          ))}
          <section>
            <h2>What you actually agree to</h2>
            <p>
              This page is explainer <span className="mono-small">{doc.id}</span>, written{" "}
              <span className="mono-small">{doc.drafted}</span>. It explains{" "}
              <span className="mono-small">{doc.explains}</span>, which is a document Nodal serves
              from its own registry.
            </p>
            <p>
              When you create an account that document is shown in full, and accepting it records
              its identifier, its version and the exact bytes you were shown. Nothing on this page
              is recorded, and where this page and that document differ, the document is what was
              agreed.
            </p>
            <p>{CONTACT_NOTE}</p>
          </section>
        </article>
      </SiteSection>

      <SiteSection title="The other documents" lead="All three are drafts pending legal review.">
        <Panel title="Explainers" description="Nodal serves five legal documents; these three pages explain the ones a new account is asked to accept. The Credits Terms and the Withdrawal and Verification Disclosure are shown in the product, at the points they apply.">
          <ul className="policy-index">
            {POLICIES.map((other) => (
              <li key={other.id}>
                <Link to={`/${other.slug}`} aria-current={other.id === doc.id ? "page" : undefined}>
                  {other.title} — explains {other.explains}
                </Link>
              </li>
            ))}
          </ul>
        </Panel>
      </SiteSection>
    </>
  );
}
