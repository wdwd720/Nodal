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
          <span>version {doc.id}</span>
          <span>drafted {doc.drafted}</span>
        </p>
      </SitePageHead>

      <SiteSection title="Before you read it" lead="A statement of fact about this document.">
        <div className="policy">
          <Refusal
            what="This document has not been reviewed by a lawyer."
            rule={DRAFT_NOTICE}
            code="LEGAL_APPROVED=false"
            remedy="Read it as a description of what the software does. It is not a final agreement and it is not legal advice."
          />
        </div>
      </SiteSection>

      <SiteSection title={doc.title} lead={`Version ${doc.id}, drafted ${doc.drafted}.`}>
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
            <h2>Version</h2>
            <p>
              This is <span className="mono-small">{doc.id}</span>, drafted{" "}
              <span className="mono-small">{doc.drafted}</span>. A change to these words is a new
              version with a new identifier, never an edit to this one, because an edited document
              with an unchanged identifier makes every recorded acceptance a claim about words
              nobody agreed to.
            </p>
            <p>{CONTACT_NOTE}</p>
          </section>
        </article>
      </SiteSection>

      <SiteSection title="The other documents" lead="All three are drafts pending legal review.">
        <Panel title="Documents" description="What this build asks a new account to acknowledge.">
          <ul className="policy-index">
            {POLICIES.map((other) => (
              <li key={other.id}>
                <Link to={`/${other.slug}`} aria-current={other.id === doc.id ? "page" : undefined}>
                  {other.title} — {other.id}
                </Link>
              </li>
            ))}
          </ul>
        </Panel>
      </SiteSection>
    </>
  );
}
