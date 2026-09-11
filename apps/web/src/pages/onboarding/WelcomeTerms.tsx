/**
 * `/welcome/terms` — the documents, and the acceptance.
 *
 * # The bytes on the screen are the bytes in the record
 *
 * `POST /v1/me/terms-acceptances` records the document id, the version, and the
 * sha256 of the exact bytes the server served. So this screen renders
 * `LegalDocument.body` — the Markdown the API returned — and nothing else. It
 * does not render the plain-language explainers in `src/content/policies/`,
 * because an acceptance of a hash over text the customer never read is not an
 * acceptance of anything, and it would be the one dishonest screen in a product
 * built on not having any.
 *
 * That is also why the body is shown verbatim in a `<pre>` rather than passed
 * through a Markdown renderer. Any transformation is a chance for a character
 * to be dropped between what was displayed and what was hashed, and a legal
 * document is exactly the wrong place to be approximately right. The face is
 * the text face and the wrapping is soft, so it reads as prose; the characters
 * are the server's.
 *
 * A document that arrives with no `body` cannot be honestly accepted, and this
 * screen refuses to offer acceptance for it rather than asking somebody to
 * agree to a title.
 *
 * # What is asked for, and what is not
 *
 * Only the documents the API lists as outstanding and required at onboarding.
 * The withdrawal disclosure is `requirement: WITHDRAWAL` and is deliberately
 * not here: asking for it at signup would be the frontloaded KYC goal §6
 * forbids, and the withdrawal surface asks for it at the moment it applies.
 */
import { useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useAcceptTerms,
  useTermsState,
  type LegalDocument,
  type TermsDocumentId,
  type TermsState,
} from "../../api/queries.ts";
import { Button } from "../../components/Button.tsx";
import { AsyncPanel, Explanation } from "../../components/DataState.tsx";
import { Panel } from "../../components/Panel.tsx";
import { Refusal } from "../../components/Refusal.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { IdentifierShort } from "../../components/Identifier.tsx";
import { OnboardingFrame, StepList } from "./OnboardingFrame.tsx";

/** The two acknowledgements USER_JOURNEY §1 asks for, in the customer's words. */
const ACKNOWLEDGEMENTS: readonly string[] = [
  "Credits are internal platform value and aren't directly withdrawable. Value leaves Nodal only through a withdrawal request, once I am verified and only if an approved payout path is active.",
  "Nodal-native markets are speculative, prices are set by participants rather than by an outside reference, and I can lose everything I put in.",
];

function DocumentBody(props: { readonly doc: LegalDocument }): ReactNode {
  const body = props.doc.body;
  if (body === undefined || body === "") {
    return (
      <Refusal
        what={`The text of ${props.doc.title} did not arrive with this response.`}
        rule="An acceptance records the sha256 of the bytes that were shown, so a document with no body cannot be accepted."
        code="CONTRACT_INCOMPLETE"
        remedy="Reload the page. If it keeps happening the deployment is serving an incomplete document and nobody should be asked to accept it."
      />
    );
  }
  return (
    <div className="doc-scroll" role="group" aria-label={`${props.doc.title}, full text`} tabIndex={0}>
      <pre className="doc-source">{body}</pre>
    </div>
  );
}

function Documents(props: { readonly state: TermsState }): ReactNode {
  const navigate = useNavigate();
  const accept = useAcceptTerms();
  const [acknowledged, setAcknowledged] = useState(false);
  const [attempted, setAttempted] = useState(false);

  const outstanding = props.state.outstanding;
  const required = props.state.documents.filter(
    (doc) => doc.requirement === "ONBOARDING" && outstanding.includes(doc.document_id),
  );
  const missingBody = required.some((doc) => doc.body === undefined || doc.body === "");

  // Nothing outstanding: the step is done and this screen has nothing to ask.
  if (required.length === 0) {
    return (
      <Panel title="Nothing outstanding" description="Every document required at this point is already accepted at the version now served.">
        <div className="form-actions">
          <Button
            variant="primary"
            onClick={() => {
              void navigate("/welcome/done");
            }}
          >
            Continue
          </Button>
        </div>
      </Panel>
    );
  }

  return (
    <>
      {required.map((doc) => (
        <Panel
          key={doc.document_id}
          title={doc.title}
          description="Shown exactly as Nodal serves it. The acceptance records the hash of these bytes."
          actions={
            doc.counsel_review_required ? <StatusBadge tone="warn">Draft</StatusBadge> : undefined
          }
        >
          {doc.counsel_review_required && (
            <p className="note">
              This document has not been reviewed by a lawyer. It describes what the software does;
              it is not a final agreement and it is not legal advice.
            </p>
          )}
          <p className="policy-meta mono-small">
            <span>{doc.document_id}</span>
            <span>version {doc.version}</span>
            <IdentifierShort value={doc.content_hash} what="content hash" />
          </p>
          <DocumentBody doc={doc} />
        </Panel>
      ))}

      <Panel
        title="What you are acknowledging"
        description="Two statements, because these are the two things people most often assume the other way round."
      >
        <ul className="claims claims-not">
          {ACKNOWLEDGEMENTS.map((line) => (
            <li key={line}>
              <span className="claims-glyph" aria-hidden="true">
                △
              </span>
              <span>{line}</span>
            </li>
          ))}
        </ul>

        <label className="checkbox">
          <input
            type="checkbox"
            checked={acknowledged}
            onChange={(event) => {
              setAcknowledged(event.target.checked);
            }}
          />
          <span>
            I have read the {required.length === 1 ? "document" : "documents"} above and I
            acknowledge both statements.
          </span>
        </label>

        {attempted && !acknowledged && (
          <p className="field-error" role="alert">
            Tick the box to record your acceptance. Nothing is recorded until you do.
          </p>
        )}

        {accept.isError && <Explanation error={accept.error} />}

        <div className="form-actions">
          {missingBody ? (
            <Button disabledReason="One of these documents arrived without its text, and Nodal will not record an acceptance of words you were not shown.">
              Accept and continue
            </Button>
          ) : (
            <Button
              variant="primary"
              busy={accept.isPending}
              busyLabel="Recording…"
              onClick={() => {
                setAttempted(true);
                if (!acknowledged) return;
                accept.mutate(
                  {
                    documentIds: required.map((doc) => doc.document_id as TermsDocumentId),
                    // Minted at the moment of confirmation. Accepting the same
                    // bytes twice is idempotent on the server too, so a retry
                    // after a re-authentication records one acceptance.
                    idempotencyKey: newIdempotencyKey(),
                  },
                  {
                    onSuccess: () => {
                      void navigate("/welcome/done");
                    },
                  },
                );
              }}
            >
              Accept and continue
            </Button>
          )}
        </div>
      </Panel>
    </>
  );
}

export function WelcomeTerms(): ReactNode {
  const terms = useTermsState();
  return (
    <OnboardingFrame
      title="What you are agreeing to"
      lead="The documents Nodal asks you to accept, shown in full and exactly as they are served. All of them are drafts pending review by a lawyer, and each one says so."
      step="TERMS"
    >
      <StepList current="TERMS" />
      <AsyncPanel query={terms} loadingLabel="Loading the documents…">
        {(state: TermsState) => <Documents state={state} />}
      </AsyncPanel>
    </OnboardingFrame>
  );
}
