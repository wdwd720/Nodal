/**
 * SANDBOX RUNS of the account the area is reading: the runs of its
 * paper-follows, and of simulations of its own listings.
 *
 * Every run is recorded whatever happened: why it ended (decided by the host
 * from what it observed, never from what the strategy printed), the digests
 * that make it reproducible, and every violation -- a record that was not an
 * intent or a log, a proposal the strategy may not make, or one beyond its
 * manifest. Any violation in a run refuses every intent from it.
 */
import { useState, type ReactNode } from "react";

import { useStrategyRuns, type StrategySandboxRun } from "../../../api/advanced/marketplace.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Identifier, Page, Panel, Pill, Table } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { RUN_TERMINATIONS, TERMINATION_MEANING, terminationTone } from "../../../lib/marketplace.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { TerminationPill, strategyHref } from "./parts.tsx";

export function RunsPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  return (
    <Page
      title="Sandbox runs"
      lead="Every time this account's strategies ran in the sandbox: why each run ended, its digests, and every violation the host recorded."
      actions={<LinkButton to={strategyHref.home()}>Back to strategies</LinkButton>}
    >
      <SimulatedNotice />
      {accountId === undefined ? (
        <Panel title="Runs">
          <NoAccount />
        </Panel>
      ) : (
        <RunsPanel key={`runs-${accountId}`} accountId={accountId} />
      )}
      <Panel
        title="Why a run ends"
        description="Decided by the host from what it observed: a refused memory growth, a full output buffer, an expired deadline, the exit status."
      >
        <Table caption="Run terminations and their meaning" headers={["Termination", "What it means"]}>
          {RUN_TERMINATIONS.map((termination) => (
            <tr key={termination}>
              <th scope="row">
                <Pill tone={terminationTone(termination)}>{termination}</Pill>
              </th>
              <td className="cell-prose">{TERMINATION_MEANING[termination]}</td>
            </tr>
          ))}
        </Table>
      </Panel>
    </Page>
  );
}

function RunsPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const runs = useStrategyRuns(props.accountId, trail.current);
  return (
    <Panel title="Runs" description="Newest first, as the backend lists them.">
      <AsyncPanel
        query={runs}
        loadingLabel="Loading sandbox runs…"
        empty={{
          isEmpty: (page: CursorPage<StrategySandboxRun>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No sandbox runs",
          body: "No strategy of this account has run in the sandbox.",
        }}
      >
        {(page: CursorPage<StrategySandboxRun>) => (
          <>
            <ScrollTable
              caption="Sandbox runs on this page"
              headers={["Run", "Source", "Ended", "Proposals", "Violations", "Resources", "Digests", "Recorded"]}
            >
              {page.items.map((run) => (
                <tr key={run.id}>
                  <th scope="row">
                    <code className="mono-small">{run.id}</code>
                    <span className="cell-note">
                      <code className="mono-small">{run.strategy_ref}</code>
                    </span>
                  </th>
                  <td>
                    <Pill tone="neutral">{run.source}</Pill>
                    {run.follow_run_id !== undefined && <span className="cell-note">follow {run.follow_run_id}</span>}
                    {run.evaluation_id !== undefined && <span className="cell-note">evaluation {run.evaluation_id}</span>}
                  </td>
                  <td>
                    <TerminationPill termination={run.termination} />
                    <span className="cell-note">exit {String(run.exit_code)}</span>
                    <span className="cell-note">
                      {run.artifact_verified ? "artifact verified" : "artifact NOT verified"}
                    </span>
                  </td>
                  <td>
                    {String(run.proposals_count)} proposed
                    <span className="cell-note">{String(run.accepted_count)} within the manifest</span>
                  </td>
                  <td>
                    {run.violations.length === 0 ? (
                      <span className="absent">none</span>
                    ) : (
                      <ul className="codes">
                        {run.violations.map((v, index) => (
                          <li key={`${v.code}-${String(index)}`}>
                            <code className="mono-small">{v.code}</code>
                            <span className="cell-note">
                              {v.record_seq === 0 ? "the whole output" : `record ${String(v.record_seq)}`}: {v.detail}
                            </span>
                          </li>
                        ))}
                      </ul>
                    )}
                  </td>
                  <td>
                    {String(run.wall_ms)} ms
                    <span className="cell-note">{String(run.output_bytes)} bytes of output</span>
                    <span className="cell-note">limits {run.limits_version}</span>
                  </td>
                  <td>
                    <span className="cell-note">
                      run <Identifier value={run.run_digest} />
                    </span>
                    <span className="cell-note">
                      artifact <Identifier value={run.artifact_digest} />
                    </span>
                    <span className="cell-note">
                      input <Identifier value={run.input_digest} />
                    </span>
                  </td>
                  <td>{formatInstant(run.created_at)}</td>
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
