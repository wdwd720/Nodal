/**
 * MARGIN — operator: the liquidation queue (every account in a margin call
 * or being liquidated, with its live evaluation), the commands that run it,
 * marking the book to market, funding the lending pool and the insurance
 * fund, every margin call across accounts, and the manual review queue where
 * two operators resolve a case a person owns.
 *
 * These are sandbox:operate commands; those that move simulated value need a
 * recent strong sign-in. A principal without the permission is refused by the
 * backend, and the page shows that refusal with its code — it never hides a
 * panel to pretend the question was not asked.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useLiquidateAccount,
  useMarginOperatorCalls,
  useMarginQueue,
  useMarkToMarket,
  useRunLiquidationQueue,
  useSeedFacilities,
  type BatchRequest,
  type LiquidateRequest,
  type MarginCall,
  type MarginFacilities,
  type MarginLiquidationResult,
  type MarginMarkToMarketResult,
  type MarginQueue,
  type MarginQueueRunResult,
  type SeedRequest,
} from "../../../api/advanced/margin.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { useAssets, type Asset } from "../../../api/queries.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, isCanonicalUuid, type CursorTrail } from "../../../lib/advanced.ts";
import {
  MARGIN_CALL_STATUSES,
  PLAN_DECISION_MEANING,
  accountStatusTone,
  assetById,
  caseStatusTone,
  type MarginCallStatus,
} from "../../../lib/margin.ts";
import { QUANTITY_PATTERN } from "../../../lib/money.ts";
import { CursorPager, ScrollTable } from "../shared.tsx";
import { CallRow } from "./MarginHistory.tsx";
import { CommandFlow, Failures, MarginNav, caseHref, type Prepared } from "./parts.tsx";
import { ReviewQueuePanel } from "./Reviews.tsx";

/** Batch sizes an operator may choose; absent means the service's own bound. */
const BATCH_CHOICES: readonly number[] = [1, 5, 10, 25, 50, 100];

export function MarginOperatorPage(): ReactNode {
  const assets = useAssets();
  const registry = assets.data ?? [];
  return (
    <Page
      title="Margin operations"
      lead="The liquidation queue and the operator commands of the margin book. The backend decides who may see and run them."
    >
      <SimulatedNotice />
      <MarginNav />
      <QueuePanel assets={registry} />
      <ReviewQueuePanel />
      <RunQueuePanel />
      <LiquidatePanel />
      <MarkToMarketPanel />
      <FacilitiesPanel />
      <OperatorCallsPanel />
    </Page>
  );
}

function unitOf(assets: readonly Asset[], numeraire: string): string {
  return assetById(assets, numeraire)?.symbol ?? "numeraire units";
}

function QueuePanel(props: { readonly assets: readonly Asset[] }): ReactNode {
  const queue = useMarginQueue();
  return (
    <Panel
      title="Liquidation queue"
      description="Accounts in MARGIN_CALL or LIQUIDATING under the service's policy, each with its live evaluation. A queue run takes the most under-margined first and skips a case a person owns (MANUAL_REVIEW)."
    >
      <AsyncPanel
        query={queue}
        loadingLabel="Loading the liquidation queue…"
        empty={{
          isEmpty: (data: MarginQueue) => data.items.length === 0,
          title: "The queue is empty",
          body: "The backend reports no account in a margin call or being liquidated.",
        }}
      >
        {(data: MarginQueue) => (
          <ScrollTable
            caption="Accounts in the liquidation queue"
            headers={["Account", "Status", "Equity", "Maintenance", "Liquidation buffer", "Open call", "Latest case"]}
          >
            {data.items.map((entry) => {
              const unit = unitOf(props.assets, entry.account.numeraire_asset_id);
              const ev = entry.evaluation;
              return (
                <tr key={entry.account.id} data-account={entry.account.account_id}>
                  <th scope="row">
                    <Identifier value={entry.account.account_id} />
                  </th>
                  <td>
                    <Pill tone={accountStatusTone(entry.account.status)}>{entry.account.status}</Pill>
                  </td>
                  {ev === undefined ? (
                    <td colSpan={3}>
                      <span className="absent">
                        not evaluated
                        {entry.evaluation_error !== undefined && (
                          <>
                            : <code className="mono-small">{entry.evaluation_error.code}</code> {entry.evaluation_error.detail}
                          </>
                        )}
                      </span>
                    </td>
                  ) : (
                    <>
                      <td>
                        <Usd value={ev.usd.equity} />
                      </td>
                      <td>
                        <Usd value={ev.usd.maintenance_margin} />
                      </td>
                      <td>
                        <Usd value={ev.usd.liquidation_buffer} />
                      </td>
                    </>
                  )}
                  <td>
                    {entry.open_call === undefined ? (
                      <span className="absent">none</span>
                    ) : (
                      <>
                        deficiency <Qty value={entry.open_call.deficiency} decimals={entry.numeraire_decimals} symbol={unit} />
                      </>
                    )}
                  </td>
                  <td>
                    {entry.latest_case === undefined ? (
                      <span className="absent">none</span>
                    ) : (
                      <Link to={caseHref(entry.latest_case.id)}>
                        <Pill tone={caseStatusTone(entry.latest_case.status)}>{entry.latest_case.status}</Pill>
                      </Link>
                    )}
                  </td>
                </tr>
              );
            })}
          </ScrollTable>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function BatchSelect(props: {
  readonly id: string;
  readonly label: string;
  readonly value: number | undefined;
  readonly onChange: (value: number | undefined) => void;
  readonly unbounded: string;
}): ReactNode {
  return (
    <div className="form-row">
      <label htmlFor={props.id}>{props.label}</label>
      <select
        id={props.id}
        className="input"
        value={props.value === undefined ? "" : String(props.value)}
        onChange={(event) => {
          const chosen = BATCH_CHOICES.find((choice) => String(choice) === event.target.value);
          props.onChange(chosen);
        }}
      >
        <option value="">{props.unbounded}</option>
        {BATCH_CHOICES.map((choice) => (
          <option key={choice} value={String(choice)}>
            {String(choice)}
          </option>
        ))}
      </select>
    </div>
  );
}

function LiquidationResultRows(props: { readonly results: readonly MarginLiquidationResult[] }): ReactNode {
  return (
    <ScrollTable caption="Steps taken" headers={["Account", "Decision", "Account status", "Case", "Step proceeds"]}>
      {props.results.map((result) => (
        <tr key={result.margin_account_id}>
          <th scope="row">
            <Identifier value={result.account_id} />
          </th>
          <td>
            <code className="mono-small">{result.decision}</code>
            <span className="cell-note">{result.reason}</span>
            <span className="cell-note">{PLAN_DECISION_MEANING[result.decision]}</span>
          </td>
          <td>
            <Pill tone={accountStatusTone(result.account_status)}>{result.account_status}</Pill>
          </td>
          <td>
            {result.case === undefined ? (
              <span className="absent">no case</span>
            ) : (
              <Link to={caseHref(result.case.id)}>
                <Pill tone={caseStatusTone(result.case.status)}>{result.case.status}</Pill>
              </Link>
            )}
          </td>
          <td>
            {result.step === undefined ? (
              <span className="absent">nothing executed</span>
            ) : (
              <BaseUnits value={result.step.proceeds} />
            )}
          </td>
        </tr>
      ))}
    </ScrollTable>
  );
}

function RunQueuePanel(): ReactNode {
  const run = useRunLiquidationQueue();
  const [limit, setLimit] = useState<number | undefined>(undefined);
  return (
    <Panel
      title="Run the liquidation queue"
      description="One deterministic step for each of the most under-margined accounts. It moves simulated value, so the backend asks for a recent strong sign-in."
    >
      <BatchSelect id="margin-queue-limit" label="Accounts to step" value={limit} onChange={setLimit} unbounded="The service's own bound" />
      <CommandFlow<BatchRequest, MarginQueueRunResult>
        label="Run the queue"
        variant="danger"
        prepared={{ ok: true, request: { limit } }}
        command={run}
        refusedNote="No liquidation step ran."
        describe={(request) => <p>Step up to {request.limit === undefined ? "the service's own bound of" : String(request.limit)} accounts.</p>}
        renderResult={(result) => (
          <>
            {result.results.length === 0 ? (
              <p className="note" role="status">
                The queue had nothing to step.
              </p>
            ) : (
              <LiquidationResultRows results={result.results} />
            )}
            <Failures failures={result.failures} />
          </>
        )}
      />
    </Panel>
  );
}

function LiquidatePanel(): ReactNode {
  const liquidate = useLiquidateAccount();
  const [accountId, setAccountId] = useState("");
  const candidate = accountId.trim().toLowerCase();
  const prepared: Prepared<LiquidateRequest> = isCanonicalUuid(candidate)
    ? { ok: true, request: { accountId: candidate, expectedStep: undefined } }
    : { ok: false, reason: "Enter the account id, a UUID in its canonical 36-character form." };
  return (
    <Panel
      title="Liquidate one account"
      description="At most one deterministic step of one account's liquidation, opening its case when it is eligible. It moves simulated value, so the backend asks for a recent strong sign-in."
    >
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
        }}
      >
        <div className="form-row">
          <label htmlFor="margin-liquidate-account">Account id</label>
          <input
            id="margin-liquidate-account"
            className="input"
            autoComplete="off"
            spellCheck={false}
            value={accountId}
            onChange={(event) => {
              setAccountId(event.target.value);
            }}
          />
        </div>
        <CommandFlow<LiquidateRequest, MarginLiquidationResult>
          label="Run one liquidation step"
          variant="danger"
          prepared={prepared}
          command={liquidate}
          refusedNote="No liquidation step ran."
          describe={(request) => (
            <p>
              The next step of account <code className="mono-small">{request.accountId}</code>.
            </p>
          )}
          renderResult={(result) => <LiquidationResultRows results={[result]} />}
        />
      </form>
    </Panel>
  );
}

function MarkToMarketPanel(): ReactNode {
  const mark = useMarkToMarket();
  const [limit, setLimit] = useState<number | undefined>(undefined);
  return (
    <Panel
      title="Mark to market"
      description="Evaluates open margin accounts and moves each between NORMAL, WARNING and MARGIN_CALL, issuing a margin call below maintenance. It moves no value."
    >
      <BatchSelect id="margin-mark-limit" label="Accounts to evaluate" value={limit} onChange={setLimit} unbounded="The service's own bound" />
      <CommandFlow<BatchRequest, MarginMarkToMarketResult>
        label="Mark the book to market"
        prepared={{ ok: true, request: { limit } }}
        command={mark}
        refusedNote="No account was evaluated."
        describe={(request) => <p>Evaluate up to {request.limit === undefined ? "the service's own bound of" : String(request.limit)} accounts.</p>}
        renderResult={(result) => (
          <>
            {result.evaluated.length === 0 ? (
              <p className="note" role="status">
                No open margin account was evaluated.
              </p>
            ) : (
              <ScrollTable caption="Accounts marked to market" headers={["Account", "Was", "Now", "Equity", "Margin call"]}>
                {result.evaluated.map((item) => (
                  <tr key={item.margin_account_id}>
                    <th scope="row">
                      <Identifier value={item.account_id} />
                    </th>
                    <td>
                      <Pill tone={accountStatusTone(item.previous_status)}>{item.previous_status}</Pill>
                    </td>
                    <td>
                      <Pill tone={accountStatusTone(item.status)}>{item.status}</Pill>
                    </td>
                    <td>
                      <Usd value={item.evaluation.usd.equity} />
                    </td>
                    <td>{item.call === undefined ? <span className="absent">none</span> : <code className="mono-small">{item.call.status}</code>}</td>
                  </tr>
                ))}
              </ScrollTable>
            )}
            <Failures failures={result.failures} />
          </>
        )}
      />
    </Panel>
  );
}

function FacilitiesPanel(): ReactNode {
  const seed = useSeedFacilities();
  const [pool, setPool] = useState("");
  const [fund, setFund] = useState("");
  const valid = (value: string): boolean => QUANTITY_PATTERN.test(value) && !value.startsWith("-");
  const prepared: Prepared<SeedRequest> =
    valid(pool) && valid(fund)
      ? { ok: true, request: { poolAmount: pool, fundAmount: fund } }
      : { ok: false, reason: "Enter both amounts as whole sandbox-dollar base units (digits only)." };
  return (
    <Panel
      title="Fund the facilities"
      description="Funds the lending pool and the insurance fund in sandbox dollars, exactly once per confirmed request. It moves simulated value, so the backend asks for a recent strong sign-in."
    >
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
        }}
      >
        <div className="form-row">
          <label htmlFor="margin-seed-pool">Lending pool, in base units</label>
          <input
            id="margin-seed-pool"
            className="input"
            inputMode="numeric"
            autoComplete="off"
            value={pool}
            onChange={(event) => {
              setPool(event.target.value.trim());
            }}
          />
        </div>
        <div className="form-row">
          <label htmlFor="margin-seed-fund">Insurance fund, in base units</label>
          <input
            id="margin-seed-fund"
            className="input"
            inputMode="numeric"
            autoComplete="off"
            value={fund}
            onChange={(event) => {
              setFund(event.target.value.trim());
            }}
          />
        </div>
        <CommandFlow<SeedRequest, MarginFacilities>
          label="Fund the facilities"
          variant="danger"
          prepared={prepared}
          command={seed}
          refusedNote="Neither facility was funded."
          describe={(request) => (
            <FieldGrid columns={2}>
              <Field label="Lending pool">
                <BaseUnits value={request.poolAmount} />
              </Field>
              <Field label="Insurance fund">
                <BaseUnits value={request.fundAmount} />
              </Field>
            </FieldGrid>
          )}
          renderResult={(result) => (
            <FieldGrid columns={2}>
              <Field label={`Lending pool — ${result.lending_pool.name}`} note="Held unlent, in the sandbox dollar.">
                <Qty value={result.lending_pool.available} decimals={result.asset_decimals} />
              </Field>
              <Field label={`Insurance fund — ${result.insurance_fund.name}`} note="Held undrawn, in the sandbox dollar.">
                <Qty value={result.insurance_fund.available} decimals={result.asset_decimals} />
              </Field>
            </FieldGrid>
          )}
        />
      </form>
    </Panel>
  );
}

function OperatorCallsPanel(): ReactNode {
  const [status, setStatus] = useState<MarginCallStatus | undefined>("OPEN");
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const calls = useMarginOperatorCalls(status, trail.current);
  return (
    <Panel title="Margin calls across accounts" description="Every margin call the backend recorded, newest first.">
      <div className="form-row">
        <label htmlFor="margin-calls-status">Status</label>
        <select
          id="margin-calls-status"
          className="input"
          value={status ?? ""}
          onChange={(event) => {
            setStatus(MARGIN_CALL_STATUSES.find((candidate) => candidate === event.target.value));
            setTrail(FIRST_PAGE);
          }}
        >
          <option value="">Every status</option>
          {MARGIN_CALL_STATUSES.map((candidate) => (
            <option key={candidate} value={candidate}>
              {candidate}
            </option>
          ))}
        </select>
      </div>
      <AsyncPanel
        query={calls}
        loadingLabel="Loading margin calls…"
        empty={{
          isEmpty: (page: CursorPage<MarginCall>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No margin calls",
          body: "The backend reports no margin call with this status.",
        }}
      >
        {(page: CursorPage<MarginCall>) => (
          <>
            <ScrollTable caption="Margin calls across accounts" headers={["Issued", "Status", "Deficiency", "Equity", "Maintenance", "Resolved", "Call"]}>
              {page.items.map((call) => (
                <CallRow key={call.id} call={call} showAccount />
              ))}
            </ScrollTable>
            <CursorPager
              noun="calls"
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
