/**
 * OVERVIEW of the advanced (sandbox) area.
 *
 * One response, GET /advanced/capabilities, answers three questions: which
 * advanced capabilities exist, in which modes each may run in this deployment,
 * and — wherever one may not — every reason why. LIVE is a column like the
 * others: it is refused for every capability, and the refusal is shown with
 * its reason rather than hidden, because "not offered" and "not possible in
 * this build" are different statements.
 *
 * The decisions are the backend's. econ.Authorizer.Describe produces them with
 * the same function that refuses a request (gates.DecideMode), so this table
 * is what a request meets, not a description of what it might meet.
 */
import type { ReactNode } from "react";

import {
  useAdvancedCapabilities,
  type AdvancedCapability,
  type AdvancedCapabilityList,
  type AdvancedModeDecision,
} from "../../../api/advanced/core.ts";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Page, Panel, Pill, Table } from "../../../components/Layout.tsx";
import { Absent, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import {
  ADVANCED_MODES,
  GATE_REASONS,
  LIVE_NOT_IMPLEMENTED,
  decisionFor,
  distinctReasons,
  formatSeconds,
  summariseLive,
  type LiveSummary,
} from "../../../lib/advanced.ts";
import { ADVANCED_MODE_DESCRIPTIONS } from "../../../lib/honesty.ts";
import { useAdvancedSections } from "../area.ts";
import { Codes, ModePill, ScrollTable, SectionList, ValueDomainPill } from "../shared.tsx";

export function Overview(): ReactNode {
  const capabilities = useAdvancedCapabilities();
  const sections = useAdvancedSections();

  return (
    <Page
      title="Overview"
      lead="Every advanced capability this build knows, whether each mode may run in this deployment, and every reason when it may not. The decisions are the backend's own, made by the same function that refuses a request."
    >
      <SimulatedNotice />

      {capabilities.isSuccess ? (
        <CapabilityPanels data={capabilities.data} />
      ) : (
        <Panel title="Capabilities and modes" description="As the backend decides them for this deployment.">
          <AsyncPanel query={capabilities} loadingLabel="Asking the backend which advanced capabilities may run here…">
            {() => null}
          </AsyncPanel>
        </Panel>
      )}

      <Panel title="The four modes" description="What each mode means. Every mode is listed, LIVE included.">
        <Table caption="Advanced modes and what they mean" headers={["Mode", "What it means"]}>
          {ADVANCED_MODES.map((mode) => (
            <tr key={mode}>
              <th scope="row">
                <ModePill mode={mode} />
              </th>
              <td className="cell-prose">{ADVANCED_MODE_DESCRIPTIONS[mode]}</td>
            </tr>
          ))}
        </Table>
      </Panel>

      <Panel title="Sections of this area" description="Every section this build registers, and what each one shows.">
        <SectionList sections={sections} />
      </Panel>
    </Page>
  );
}

function CapabilityPanels(props: { readonly data: AdvancedCapabilityList }): ReactNode {
  const { data } = props;
  const summary = summariseLive(data.items);
  const reasons = distinctReasons(data.items);

  return (
    <>
      <Panel title="This deployment" description="As the backend reports it, with the decisions below counted.">
        <FieldGrid columns={4}>
          <Field label="Environment" note="The deployment the decisions below were made for.">
            <span className="num">{data.environment}</span>
          </Field>
          <Field label="Value domain" note="Every figure the advanced API reports is in this domain.">
            <ValueDomainPill value={data.value_domain} />
          </Field>
          <Field label="LIVE" note={liveNote(summary)}>
            {liveStatement(summary)}
          </Field>
          <Field
            label="Runnable here"
            note="Capabilities with at least one simulated mode the backend allows in this deployment."
          >
            <span className="num">
              {String(summary.runnable)} of {String(summary.capabilities)}
            </span>
          </Field>
        </FieldGrid>
        {summary.allowed.length > 0 && (
          <p className="banner banner-bad" role="alert">
            The backend reports LIVE as allowed for {summary.allowed.join(", ")}. This build implements no live
            path for any advanced capability, so treat that answer as a defect to report, not as permission.
          </p>
        )}
      </Panel>

      <Panel
        title="Capabilities and modes"
        description="One row per capability and one column per mode, LIVE included. A refused mode lists every reason the backend gave; all of them would have to change before it could run."
      >
        {data.items.length === 0 ? (
          <EmptyState
            title="No advanced capabilities reported"
            body="The backend returned no advanced capabilities. That is its answer, not a table of refusals."
          />
        ) : (
          <ScrollTable
            caption="Decision for every capability and mode"
            headers={["Capability", "Gate", "Kill switch", ...ADVANCED_MODES]}
          >
            {data.items.map((item) => (
              <CapabilityRow key={item.capability} item={item} />
            ))}
          </ScrollTable>
        )}
      </Panel>

      {data.items.length > 0 && (
        <Panel
          title="What a live activation would require"
          description="What stands between this build and real customer exposure, per capability, as its gate specification states it. The real-capital ceiling is the most real value a first live activation could put at risk; zero means none at all."
        >
          <ScrollTable
            caption="Live activation requirements per capability"
            headers={[
              "Capability",
              "Live implemented",
              "Evidence it would need",
              "Approvers",
              "Validity window",
              "Real-capital ceiling",
              "Jurisdictions",
              "Provider readiness",
            ]}
          >
            {data.items.map((item) => (
              <LiveRequirementsRow key={item.capability} item={item} />
            ))}
          </ScrollTable>
        </Panel>
      )}

      {data.items.length > 0 && (
        <Panel
          title="Reason codes in this answer"
          description="Every distinct code the decisions above carry, and what it means. A code this app has no words for is listed exactly as the backend sent it."
        >
          {reasons.length === 0 ? (
            <EmptyState
              title="No reason codes"
              body="Every decision above is an allowance, so the backend gave no reasons."
            />
          ) : (
            <Table caption="Reason codes and their meaning" headers={["Code", "What it means"]}>
              {reasons.map((code) => (
                <tr key={code}>
                  <th scope="row">
                    <code className="mono-small">{code}</code>
                  </th>
                  <td className="cell-prose">
                    {GATE_REASONS[code] ?? "No gloss in this app; the code is the backend's own."}
                  </td>
                </tr>
              ))}
            </Table>
          )}
        </Panel>
      )}
    </>
  );
}

function liveStatement(summary: LiveSummary): ReactNode {
  if (summary.capabilities === 0) return <Absent what="no capabilities reported" />;
  return (
    <span className="num">
      refused for {String(summary.refused)} of {String(summary.capabilities)}
    </span>
  );
}

function liveNote(summary: LiveSummary): string {
  const parts = [
    `${LIVE_NOT_IMPLEMENTED} is among the reasons in ${String(summary.notImplemented)} of them: this build has no live path to switch on.`,
  ];
  if (summary.unreported.length > 0) {
    parts.push(`No LIVE decision was reported for ${summary.unreported.join(", ")}.`);
  }
  return parts.join(" ");
}

function CapabilityRow(props: { readonly item: AdvancedCapability }): ReactNode {
  const { item } = props;
  return (
    <tr>
      <th scope="row">
        <span className="cap-name">{item.capability}</span>
        {item.high_risk && (
          <>
            {" "}
            <Pill tone="warn">high risk</Pill>
          </>
        )}
        <span className="cell-note">
          implements {item.simulated_modes.length === 0 ? "no simulated mode" : item.simulated_modes.join(", ")}
        </span>
      </th>
      <td>
        {item.gate_active ? <Pill tone="good">ACTIVE</Pill> : <Pill tone="neutral">not active</Pill>}
        <span className="cell-note">
          {item.ungated_environments.length === 0
            ? "needed in every environment"
            : `not needed in ${item.ungated_environments.join(", ")}`}
        </span>
      </td>
      <td>{item.kill_switched ? <Pill tone="bad">switched off</Pill> : <Pill tone="neutral">not switched off</Pill>}</td>
      {ADVANCED_MODES.map((mode) => (
        <ModeCell key={mode} capability={item.capability} mode={mode} decision={decisionFor(item.decisions, mode)} />
      ))}
    </tr>
  );
}

function ModeCell(props: {
  readonly capability: string;
  readonly mode: string;
  readonly decision: AdvancedModeDecision | undefined;
}): ReactNode {
  const { decision } = props;
  return (
    <td data-capability={props.capability} data-mode={props.mode}>
      {decision === undefined ? (
        <Absent what="no decision reported" />
      ) : (
        <>
          <Pill tone={decision.allowed ? "good" : "bad"}>{decision.allowed ? "Allowed" : "Refused"}</Pill>
          <Codes codes={decision.reasons} />
        </>
      )}
    </td>
  );
}

function LiveRequirementsRow(props: { readonly item: AdvancedCapability }): ReactNode {
  const { item } = props;
  return (
    <tr>
      <th scope="row">{item.capability}</th>
      <td>{item.live_implemented ? <Pill tone="warn">yes</Pill> : <Pill tone="neutral">no</Pill>}</td>
      <td>
        <Codes codes={item.live_evidence} none="none stated" />
      </td>
      <td>
        <span className="num">{String(item.min_approvers)}</span>
      </td>
      <td>
        {formatSeconds(item.max_live_validity_seconds)}
        <span className="cell-note">{String(item.max_live_validity_seconds)} seconds</span>
      </td>
      <td>
        <Usd value={item.max_live_capital} />
      </td>
      <td>
        <Codes codes={item.jurisdictions} none="none analysed, which permits none" />
      </td>
      <td className="cell-prose">{item.provider_readiness}</td>
    </tr>
  );
}
