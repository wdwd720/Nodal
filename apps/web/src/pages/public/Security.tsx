/**
 * `/security` — what is true, in terms somebody could check.
 *
 * A security page usually lists reassurances. This one lists **mechanisms**,
 * and ends with the one thing a visitor can verify from their own browser: the
 * deployment's environment, build and configuration digest, read live from
 * `GET /v1/version`, which is one of the three endpoints the API serves without
 * a session.
 *
 * That last panel is the page's argument. Anyone can write "secure by design";
 * publishing the SHA-256 of the running configuration means a deployment can be
 * matched to the blueprint that produced it.
 */
import type { ReactNode } from "react";

import { API_ORIGIN } from "../../api/client.ts";
import { useVersion } from "../../api/queries.ts";
import { AsyncPanel } from "../../components/DataState.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Identifier } from "../../components/Identifier.tsx";
import { Panel } from "../../components/Panel.tsx";
import { SECURITY_POINTS } from "../../content/site.ts";
import { SiteItem, SitePageHead, SiteSection } from "./SiteChrome.tsx";

const LIMITS: ReadonlyArray<{ readonly title: string; readonly body: string }> = [
  {
    title: "No system is beyond compromise",
    body:
      "This page describes a design that assumes a breach is possible: the key is not where the " +
      "data is, the roles cannot rewrite state, and the ledger records rather than overwrites. It " +
      "does not claim the design has never been wrong.",
  },
  {
    title: "Nothing here has been certified",
    body:
      "No audit, no certification and no regulatory approval is claimed. Where a document in this " +
      "product would normally carry a legal review, it says instead that it is a draft pending one.",
  },
  {
    title: "A sandbox deployment is not a real one",
    body:
      "On a sandbox tier, verification outcomes and payouts are produced by sandbox providers and " +
      "no value moves. Everything they produce is labelled sandbox in the interface and in the API " +
      "response, and none of it is evidence about a real deployment.",
  },
];

export function Security(): ReactNode {
  const version = useVersion();

  return (
    <>
      <SitePageHead
        title="Security"
        lead="Seven mechanisms, three limits, and one thing you can check for yourself from this page."
      />

      <SiteSection
        title="How it is built"
        lead="Each of these is a property of the code rather than a policy somebody agreed to follow."
      >
        <div className="site-grid-2">
          {SECURITY_POINTS.map((point) => (
            <SiteItem key={point.title} title={point.title}>
              <p>{point.body}</p>
            </SiteItem>
          ))}
        </div>
      </SiteSection>

      <SiteSection
        title="This deployment, right now"
        lead="Read from the API's public version endpoint by your own browser as this page loaded."
      >
        <Panel
          title="Build and configuration"
          description="The configuration digest is the SHA-256 of the non-secret configuration the running service was started with, so a deployment can be matched to the blueprint that produced it."
        >
          <AsyncPanel
            query={version}
            loadingLabel="Asking the API which build is running…"
            errorExtra={
              <p className="note">
                This is a failure to reach the API, not a statement about the deployment. Nothing
                on this page depends on it.
              </p>
            }
          >
            {(data) => (
              <FieldGrid columns={3}>
                <Field label="Environment" note="Which tier answered this request.">
                  {data.environment}
                </Field>
                <Field label="Build" note="The commit the service was built from.">
                  <Identifier value={data.build_version} />
                </Field>
                <Field label="Configuration digest" note="SHA-256 of the non-secret configuration.">
                  <Identifier value={data.config_hash} />
                </Field>
              </FieldGrid>
            )}
          </AsyncPanel>
          <p className="note">
            The service also answers <span className="mono-small">/v1/healthz</span> and{" "}
            <span className="mono-small">/v1/readyz</span> without a session.{" "}
            <a href={`${API_ORIGIN}/v1/healthz`} rel="noreferrer">
              Check health
            </a>
            .
          </p>
        </Panel>
      </SiteSection>

      <SiteSection
        title="What this page does not claim"
        lead="Three limits, because a security page that only lists strengths is an advertisement."
      >
        <div className="site-grid">
          {LIMITS.map((limit) => (
            <SiteItem key={limit.title} title={limit.title}>
              <p>{limit.body}</p>
            </SiteItem>
          ))}
        </div>
      </SiteSection>
    </>
  );
}
