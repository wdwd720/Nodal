/**
 * The read-only panels of the operator console. Each draws exactly what one
 * API read returned -- the console's overview, the capability table, the
 * liquidation queue, the simulated chains -- in one of the four honest
 * states, and none of them computes a figure. Statuses other subsystems
 * store (a stable asset's, a bridge route's, a reconciliation record's) are
 * shown as their tables hold them.
 */
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { useAdvancedCapabilities, type AdvancedCapabilityList } from "../../../api/advanced/core.ts";
import { useMarginQueue, type MarginQueue } from "../../../api/advanced/margin.ts";
import {
  useOpsChains,
  type OpsChains,
  type OpsOverview,
  type OpsReconciliationRecord,
} from "../../../api/advanced/operator.ts";
import { Button } from "../../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { AsOf, Field, FieldGrid, Identifier, Panel, Pill } from "../../../components/Layout.tsx";
import { accountStatusTone, caseStatusTone } from "../../../lib/margin.ts";
import {
  alertTone,
  bridgeStatusTone,
  formatAge,
  formatDuration,
  oracleOrder,
  reconciliationKindMeaning,
  reconciliationKindTone,
  reconciliationStatusTone,
  stableStatusTone,
} from "../../../lib/operator.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref } from "../area.ts";
import { ScrollTable } from "../shared.tsx";

/** The overview query, as every panel that draws a part of it receives it. */
export interface OverviewQuery {
  readonly isPending: boolean;
  readonly isError: boolean;
  readonly error: unknown;
  readonly data: OpsOverview | undefined;
  readonly refetch: () => unknown;
}

/** When the overview was read, and in which environment. */
export function OverviewMeta(props: { readonly overview: OpsOverview }): ReactNode {
  return (
    <div className="meta-row">
      <AsOf at={props.overview.generated_at} label="Overview read at" />
      <span className="mono-small">environment {props.overview.environment}</span>
      <Pill tone="warn">value domain {props.overview.value_domain}</Pill>
    </div>
  );
}

/* --------------------------------------------------------------------------
 * Circuit breakers
 * ------------------------------------------------------------------------ */

export function CircuitBreakersPanel(props: { readonly overview: OverviewQuery }): ReactNode {
  return (
    <Panel
      title="Circuit breakers"
      description="Every kill switch that is active now, every stable asset's status (PAUSED includes a pause by the peg breaker) with its latest peg alert, and every bridge route's status."
    >
      <AsyncPanel query={props.overview} loadingLabel="Loading the circuit breakers…">
        {(overview: OpsOverview) => (
          <>
            <h3>Active kill switches</h3>
            {overview.kill_switches.length === 0 ? (
              <EmptyState title="No kill switch is active" body="The backend reports no active kill switch." />
            ) : (
              <ScrollTable caption="Active kill switches" headers={["Kind", "Scope", "Severity", "Reason", "Activated"]}>
                {overview.kill_switches.map((sw) => (
                  <tr key={`${sw.kind}-${sw.scope_id}`} data-kill-switch={sw.kind}>
                    <th scope="row">
                      <code className="mono-small">{sw.kind}</code>
                    </th>
                    <td>
                      <code className="mono-small">{sw.scope_id === "" ? "global" : sw.scope_id}</code>
                    </td>
                    <td>
                      <Pill tone="bad">{sw.severity}</Pill>
                    </td>
                    <td className="cell-prose">{sw.reason ?? <span className="absent">none recorded</span>}</td>
                    <td>
                      {sw.activated_at === undefined ? <span className="absent">not recorded</span> : formatInstant(sw.activated_at)}
                      {sw.activated_by !== undefined && <span className="cell-note">by {sw.activated_by}</span>}
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            )}
            <h3>Stable assets</h3>
            {overview.stable_assets.length === 0 ? (
              <EmptyState title="No stable asset is registered" body="The backend reports no platform stable asset in this deployment." />
            ) : (
              <ScrollTable caption="Stable assets and their breakers" headers={["Stable asset", "Chain", "Status", "Latest peg alert"]}>
                {overview.stable_assets.map((asset) => (
                  <tr key={asset.id} data-stable-asset={asset.id}>
                    <th scope="row">
                      {asset.name}
                      <span className="cell-note">
                        <Identifier value={asset.asset_id} />
                      </span>
                    </th>
                    <td>
                      <code className="mono-small">{asset.chain_id}</code>
                    </td>
                    <td>
                      <Pill tone={stableStatusTone(asset.status)}>{asset.status}</Pill>
                    </td>
                    <td>
                      {asset.latest_alert === undefined ? (
                        <span className="absent">no alert recorded</span>
                      ) : (
                        <>
                          <Pill tone={alertTone(asset.latest_alert.severity, asset.latest_alert.breaker_tripped)}>
                            {asset.latest_alert.severity} {asset.latest_alert.kind}
                          </Pill>
                          <span className="cell-note">
                            {asset.latest_alert.breaker_tripped ? "tripped the breaker" : "did not trip the breaker"} ·{" "}
                            {formatInstant(asset.latest_alert.observed_at)}
                          </span>
                        </>
                      )}
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            )}
            <h3>Bridge routes</h3>
            {overview.bridge_routes.length === 0 ? (
              <EmptyState title="No bridge route is registered" body="The backend reports no bridge route in this deployment." />
            ) : (
              <ScrollTable caption="Bridge routes" headers={["Route", "From", "To", "Status", "Open reconciliation"]}>
                {overview.bridge_routes.map((route) => (
                  <tr key={route.id} data-bridge-route={route.id}>
                    <th scope="row">{route.name}</th>
                    <td>
                      <code className="mono-small">{route.source_chain}</code>
                    </td>
                    <td>
                      <code className="mono-small">{route.destination_chain}</code>
                    </td>
                    <td>
                      <Pill tone={bridgeStatusTone(route.status)}>{route.status}</Pill>
                    </td>
                    <td>
                      <RecordCell record={route.open_reconciliation} none="none open" />
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            )}
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function RecordCell(props: { readonly record: OpsReconciliationRecord | undefined; readonly none: string }): ReactNode {
  const { record } = props;
  if (record === undefined) return <span className="absent">{props.none}</span>;
  return (
    <>
      <Pill tone={reconciliationStatusTone(record.status)}>{record.status}</Pill>
      <span className="cell-note">
        {record.material ? "material" : "not material"} · {record.blocks_new_risk ? "blocks new risk" : "does not block new risk"} ·
        opened {formatInstant(record.opened_at)}
        {record.resolved_at !== undefined && <> · resolved {formatInstant(record.resolved_at)}</>}
      </span>
      <span className="cell-note">
        <Identifier value={record.id} />
      </span>
    </>
  );
}

/* --------------------------------------------------------------------------
 * Reconciliation and reserve
 * ------------------------------------------------------------------------ */

export function ReconciliationPanel(props: { readonly overview: OverviewQuery }): ReactNode {
  return (
    <Panel
      title="Reconciliation mismatches"
      description="Unresolved reconciliation records of each advanced kind: how many, how many are material or block new risk, and how long the oldest has been open."
    >
      <AsyncPanel query={props.overview} loadingLabel="Loading the reconciliation records…">
        {(overview: OpsOverview) =>
          overview.reconciliation.length === 0 ? (
            <EmptyState title="No reconciliation kinds reported" body="The backend's overview listed no reconciliation kind." />
          ) : (
            <ScrollTable
              caption="Unresolved reconciliation records by kind"
              headers={["Kind", "Unresolved", "Material", "Blocks new risk", "Oldest open"]}
            >
              {overview.reconciliation.map((entry) => (
                <tr key={entry.kind} data-reconciliation-kind={entry.kind} data-unresolved={String(entry.unresolved)}>
                  <th scope="row">
                    <Pill tone={reconciliationKindTone(entry)}>{entry.kind}</Pill>
                    <span className="cell-note">{reconciliationKindMeaning(entry.kind)}</span>
                  </th>
                  <td>{String(entry.unresolved)}</td>
                  <td>{String(entry.material)}</td>
                  <td>{String(entry.blocks_new_risk)}</td>
                  <td>
                    {entry.oldest_opened_at === undefined ? (
                      <span className="absent">none open</span>
                    ) : (
                      <>
                        {formatInstant(entry.oldest_opened_at)}
                        {entry.oldest_age_ms !== undefined && <span className="cell-note">{formatAge(entry.oldest_age_ms)}</span>}
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </ScrollTable>
          )
        }
      </AsyncPanel>
    </Panel>
  );
}

export function ReservePanel(props: { readonly overview: OverviewQuery }): ReactNode {
  return (
    <Panel
      title="Reserve mismatch"
      description="Each stable asset's latest supply reconciliation: books, wallets, the simulated chain's supply and the verified reserve compared at one instant. A mismatch is material and blocks new risk."
    >
      <AsyncPanel query={props.overview} loadingLabel="Loading the stable assets' reconciliations…">
        {(overview: OpsOverview) =>
          overview.stable_assets.length === 0 ? (
            <EmptyState
              title="No stable asset is registered"
              body="There is no stable asset whose supply could be reconciled, so there is no reserve result to show."
            />
          ) : (
            <ScrollTable caption="Latest supply reconciliation per stable asset" headers={["Stable asset", "Latest supply reconciliation"]}>
              {overview.stable_assets.map((asset) => (
                <tr key={asset.id} data-reserve={asset.id}>
                  <th scope="row">{asset.name}</th>
                  <td>
                    <RecordCell record={asset.latest_supply_reconciliation} none="none recorded" />
                  </td>
                </tr>
              ))}
            </ScrollTable>
          )
        }
      </AsyncPanel>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Oracle freshness
 * ------------------------------------------------------------------------ */

export function OraclePanel(props: {
  readonly overview: OverviewQuery;
  /** Chooses an asset in the reference-mark form below. */
  readonly onPick: (assetId: string) => void;
}): ReactNode {
  return (
    <Panel
      title="Oracle freshness"
      description="The latest reference mark in the sandbox dollar of every SIMULATED asset someone holds, oldest first. A missing mark or one older than the maximum mark age is stale: margin values the collateral at nothing and risk-increasing intents are refused until a fresh mark arrives."
    >
      <AsyncPanel
        query={props.overview}
        loadingLabel="Loading the marks' freshness…"
        empty={{
          isEmpty: (overview: OpsOverview) => overview.oracle.length === 0,
          title: "No held SIMULATED asset needs a mark",
          body: "The backend reports no SIMULATED asset, other than the sandbox dollar, that anyone holds.",
        }}
      >
        {(overview: OpsOverview) => (
          <>
            <p className="note">
              Maximum mark age in this deployment: <span data-field="max-mark-age">{formatDuration(overview.max_mark_age_ms)}</span>.
            </p>
            <ScrollTable caption="Freshness of the latest mark per held asset" headers={["Asset", "Latest mark", "Age", "Fresh", ""]}>
              {oracleOrder(overview.oracle).map((asset) => (
                <tr key={asset.asset_id} data-oracle-asset={asset.asset_id} data-stale={String(asset.stale)}>
                  <th scope="row">
                    {asset.symbol}
                    <span className="cell-note">
                      <Identifier value={asset.asset_id} />
                    </span>
                  </th>
                  <td>
                    {asset.observed_at === undefined ? (
                      <span className="absent">no mark recorded</span>
                    ) : (
                      <>
                        <time dateTime={asset.observed_at} title={exactInstant(asset.observed_at)}>
                          {formatInstant(asset.observed_at)}
                        </time>
                        {asset.source !== undefined && <span className="cell-note">source {asset.source}</span>}
                      </>
                    )}
                  </td>
                  <td>{asset.age_ms === undefined ? <span className="absent">no age</span> : formatAge(asset.age_ms)}</td>
                  <td>{asset.stale ? <Pill tone="bad">stale</Pill> : <Pill tone="good">fresh</Pill>}</td>
                  <td>
                    <Button
                      variant="quiet"
                      onClick={() => {
                        props.onPick(asset.asset_id);
                      }}
                    >
                      Mark {asset.symbol}
                    </Button>
                  </td>
                </tr>
              ))}
            </ScrollTable>
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Capability gates and the liquidation queue (their own reads)
 * ------------------------------------------------------------------------ */

export function CapabilityGatesPanel(): ReactNode {
  const capabilities = useAdvancedCapabilities();
  return (
    <Panel
      title="Capability gates"
      description="Every advanced capability's gate and kill switch, and the simulated modes the backend allows for it now. The overview of the advanced area shows every decision with its reasons."
      actions={<Link to={advancedHref("overview")}>Every decision</Link>}
    >
      <AsyncPanel
        query={capabilities}
        loadingLabel="Loading the capability gates…"
        empty={{
          isEmpty: (list: AdvancedCapabilityList) => list.items.length === 0,
          title: "No capability reported",
          body: "The backend listed no advanced capability.",
        }}
      >
        {(list: AdvancedCapabilityList) => (
          <ScrollTable caption="Capability gates" headers={["Capability", "Gate", "Kill switch", "Modes allowed now"]}>
            {list.items.map((capability) => {
              const allowed = capability.decisions.filter((decision) => decision.allowed).map((decision) => decision.mode);
              return (
                <tr key={capability.capability} data-capability={capability.capability}>
                  <th scope="row">
                    <code className="mono-small">{capability.capability}</code>
                  </th>
                  <td>{capability.gate_active ? <Pill tone="good">ACTIVE</Pill> : <Pill tone="neutral">not active</Pill>}</td>
                  <td>{capability.kill_switched ? <Pill tone="bad">switched off</Pill> : <Pill tone="good">clear</Pill>}</td>
                  <td>{allowed.length === 0 ? <span className="absent">none</span> : allowed.join(", ")}</td>
                </tr>
              );
            })}
          </ScrollTable>
        )}
      </AsyncPanel>
    </Panel>
  );
}

export function LiquidationQueuePanel(): ReactNode {
  const queue = useMarginQueue();
  return (
    <Panel
      title="Margin liquidation queue"
      description="Margin accounts in a margin call or being liquidated under the margin service's policy. The margin operations page runs the queue and shows each account's evaluation."
      actions={<Link to={advancedHref("margin", "operator")}>Margin operations</Link>}
    >
      <AsyncPanel
        query={queue}
        loadingLabel="Loading the liquidation queue…"
        empty={{
          isEmpty: (data: MarginQueue) => data.items.length === 0,
          title: "The queue is empty",
          body: "The backend reports no margin account in a margin call or being liquidated.",
        }}
      >
        {(data: MarginQueue) => (
          <ScrollTable caption="Accounts in the liquidation queue" headers={["Account", "Status", "Open call", "Latest case"]}>
            {data.items.map((entry) => (
              <tr key={entry.account.id} data-queue-account={entry.account.account_id}>
                <th scope="row">
                  <Identifier value={entry.account.account_id} />
                </th>
                <td>
                  <Pill tone={accountStatusTone(entry.account.status)}>{entry.account.status}</Pill>
                </td>
                <td>
                  {entry.open_call === undefined ? <span className="absent">none</span> : formatInstant(entry.open_call.issued_at)}
                </td>
                <td>
                  {entry.latest_case === undefined ? (
                    <span className="absent">none</span>
                  ) : (
                    <Pill tone={caseStatusTone(entry.latest_case.status)}>{entry.latest_case.status}</Pill>
                  )}
                </td>
              </tr>
            ))}
          </ScrollTable>
        )}
      </AsyncPanel>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Venue health
 * ------------------------------------------------------------------------ */

export function VenueHealthPanel(): ReactNode {
  const chains = useOpsChains();
  return (
    <Panel
      title="Venue health"
      description="The simulated chains' tips, read from the devnet host. The paper venue fills at the latest fresh reference mark of each asset, so its health is the oracle freshness above."
    >
      <AsyncPanel
        query={chains}
        loadingLabel="Reading the simulated chains' tips…"
        errorExtra={
          <p className="explain-body">
            A deployment that runs no devnet host answers UNSUPPORTED here: there is no simulated chain to report on, and
            nothing is shown in its place.
          </p>
        }
        empty={{
          isEmpty: (data: OpsChains) => data.items.length === 0,
          title: "No simulated chain reported",
          body: "The devnet host answered with no chain.",
        }}
      >
        {(data: OpsChains) => (
          <>
            <AsOf at={data.observed_at} label={`Network ${data.network} read at`} />
            <ScrollTable
              caption="Simulated chain tips"
              headers={["Chain", "Height", "Slot", "Finalized height", "Tip final", "Tip hash", "Last block produced"]}
            >
              {data.items.map((chain) => (
                <tr key={chain.chain_id} data-chain={chain.chain_id}>
                  <th scope="row">
                    <code className="mono-small">{chain.chain_id}</code>
                  </th>
                  <td>{String(chain.height)}</td>
                  <td>{String(chain.slot)}</td>
                  <td>{String(chain.finalized_height)}</td>
                  <td>{chain.final ? "final" : "not final"}</td>
                  <td>
                    <code className="mono-small">{chain.hash}</code>
                  </td>
                  <td>
                    {chain.last_block_produced_at === undefined ? (
                      <span className="absent">no block produced yet</span>
                    ) : (
                      formatInstant(chain.last_block_produced_at)
                    )}
                  </td>
                </tr>
              ))}
            </ScrollTable>
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

/** The counts the overview reports of items waiting for a second operator. */
export function DualControlCounts(props: { readonly overview: OpsOverview }): ReactNode {
  const counts = props.overview.dual_control;
  return (
    <FieldGrid columns={3}>
      <Field label="Transfer reversals awaiting a second operator">
        <span data-field="transfer-reversals-pending">{String(counts.transfer_reversals_pending)}</span>
      </Field>
      <Field label="Margin cases in manual review">
        <span data-field="margin-cases-in-review">{String(counts.margin_cases_in_review)}</span>
      </Field>
      <Field label="Margin resolutions awaiting a second operator">
        <span data-field="margin-resolutions-pending">{String(counts.margin_resolutions_pending)}</span>
      </Field>
    </FieldGrid>
  );
}
