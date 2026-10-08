/**
 * MARGIN — stress tests: every scenario the margin engine runs against the
 * account (price gaps, an unavailable oracle or venue, partial fills, an
 * extreme spread, a delayed bridge, a depeg), the liquidation each would
 * trigger, and what the insurance fund would draw. A run moves no value and
 * changes no status; it is stored with its hash, and read back by its id.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useMarginStressRun,
  useMarginStressRuns,
  useRunMarginStress,
  type MarginStressRun,
  type MarginStressRunSummary,
  type StressRequest,
} from "../../../api/advanced/margin.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { useAssets } from "../../../api/queries.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { AsOf, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Bps, Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { PLAN_DECISION_MEANING, STRESS_SCENARIO_MEANING, assetById } from "../../../lib/margin.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable, ValueDomainPill } from "../shared.tsx";
import { CommandFlow, MarginNav, stressRunHref } from "./parts.tsx";

export function MarginStressPage(): ReactNode {
  const account = useAdvancedAccount();
  const { accountId } = account;
  return (
    <Page
      title="Margin stress tests"
      lead="What each stress scenario would do to this account's margin, as the margin engine simulates it. A run moves no value."
    >
      <SimulatedNotice />
      <MarginNav />
      {accountId === undefined ? (
        <Panel title="Stress tests">
          <NoAccount />
        </Panel>
      ) : (
        <>
          <RunStress key={`run-${accountId}`} accountId={accountId} asOperator={account.inspecting && account.canInspect} />
          <StressRunsPanel key={`runs-${accountId}`} accountId={accountId} />
        </>
      )}
    </Page>
  );
}

function RunStress(props: { readonly accountId: string; readonly asOperator: boolean }): ReactNode {
  const run = useRunMarginStress();
  return (
    <Panel
      title="Run a stress test"
      description={
        props.asOperator
          ? "Runs every scenario against the account you are inspecting, as an operator, and stores the report."
          : "Runs every scenario against this account's margin account and stores the report."
      }
    >
      <CommandFlow<StressRequest, MarginStressRun>
        label="Run every stress scenario"
        prepared={{ ok: true, request: { accountId: props.accountId, asOperator: props.asOperator } }}
        command={run}
        refusedNote="No stress run was stored."
        describe={(request) => (
          <p>
            Every scenario, against account <code className="mono-small">{request.accountId}</code>.
          </p>
        )}
        renderResult={(result) => (
          <p className="note" role="status">
            Stored as <Link to={stressRunHref(result.id)}>{result.id}</Link>, report hash{" "}
            <code className="mono-small">{result.report_hash}</code>.
          </p>
        )}
      />
    </Panel>
  );
}

function StressRunsPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const runs = useMarginStressRuns(props.accountId, trail.current);
  return (
    <Panel title="Stored stress runs" description="Every stress run the backend stored for this account, newest first.">
      <AsyncPanel
        query={runs}
        loadingLabel="Loading stress runs…"
        empty={{
          isEmpty: (page: CursorPage<MarginStressRunSummary>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No stress runs",
          body: "The backend has stored no stress run for this account.",
        }}
      >
        {(page: CursorPage<MarginStressRunSummary>) => (
          <>
            <ScrollTable caption="Stress runs on this page" headers={["Run at", "Policy", "Report hash", "Run"]}>
              {page.items.map((summary) => (
                <tr key={summary.id}>
                  <td>{formatInstant(summary.run_at)}</td>
                  <th scope="row">
                    <code className="mono-small">{summary.policy_version}</code>
                  </th>
                  <td>
                    <code className="mono-small">{summary.report_hash}</code>
                  </td>
                  <td>
                    <Link to={stressRunHref(summary.id)}>
                      <code className="mono-small">{summary.id}</code>
                    </Link>
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="runs"
              trail={trail}
              nextCursor={page.nextCursor}
              onForward={(cursor) => {
                setTrail(forward(trail, cursor));
              }}
              onBack={() => {
                setTrail(back(trail));
              }}
            />
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

/** One stored stress run, every scenario's result exactly as stored. */
export function MarginStressRunPage(): ReactNode {
  const { runId } = useParams();
  const { accountId } = useAdvancedAccount();
  const run = useMarginStressRun(accountId, runId);
  return (
    <Page title="Margin stress run" lead="One stored stress run: every scenario, the liquidation it would trigger, and the insurance fund's draw.">
      <SimulatedNotice />
      <MarginNav />
      <Panel title="Stress run" {...(runId === undefined ? {} : { description: `Run ${runId}` })}>
        <AsyncPanel query={run} loadingLabel="Loading the stress run…">
          {(data: MarginStressRun) => <StressRunDetail run={data} />}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

function StressRunDetail(props: { readonly run: MarginStressRun }): ReactNode {
  const { run } = props;
  const assets = useAssets();
  const unit = assetById(assets.data ?? [], run.numeraire_asset_id)?.symbol ?? "numeraire units";
  const decimals = run.numeraire_decimals;
  return (
    <>
      <AsOf at={run.run_at} label="Run at" />
      <p className="meta-row">
        <ValueDomainPill value={run.value_domain} />
        <span className="mono-small">policy {run.policy_version}</span>
        <span className="mono-small">input {run.input_hash}</span>
        <span className="mono-small">report {run.report_hash}</span>
      </p>
      <ScrollTable
        caption="Stress scenarios"
        headers={["Scenario", "Shock", "Equity after", "Maintenance after", "Liquidation", "Outcome", "Insurance fund draw"]}
      >
        {run.results.map((result) => (
          <tr key={result.scenario} data-scenario={result.scenario}>
            <th scope="row">
              <code className="mono-small">{result.scenario}</code>
              <span className="cell-note">{STRESS_SCENARIO_MEANING[result.scenario]}</span>
            </th>
            <td>{result.shock_bps === 0 ? <span className="absent">no price shock</span> : <Bps value={result.shock_bps} />}</td>
            <td>
              <Usd value={result.equity_after_usd} />
              <span className="cell-note">
                exactly <Qty value={result.equity_after} decimals={decimals} symbol={unit} />
              </span>
            </td>
            <td>
              <Usd value={result.maintenance_after_usd} />
            </td>
            <td>
              {result.would_liquidate ? <Pill tone="bad">would liquidate</Pill> : <Pill tone="good">above maintenance</Pill>}
              {!result.executable && <span className="cell-note">the liquidation could not run (no price or venue)</span>}
              {result.steps_simulated > 0 && <span className="cell-note">{String(result.steps_simulated)} simulated steps</span>}
            </td>
            <td>
              <code className="mono-small">{result.outcome}</code>
              <span className="cell-note">{PLAN_DECISION_MEANING[result.outcome]}</span>
            </td>
            <td>
              <Usd value={result.insurance_fund_draw_usd} />
              <span className="cell-note">
                deficit <Usd value={result.deficit_usd} />
              </span>
            </td>
          </tr>
        ))}
      </ScrollTable>
    </>
  );
}
