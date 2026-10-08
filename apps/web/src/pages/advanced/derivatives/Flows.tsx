/**
 * FLOWS AND FUNDING of the account the area reads.
 *
 * The flow register is every movement between the account's derivative
 * positions and the clearing house — realized results of reducing trades,
 * variation margin, perpetual funding, option premiums, exercise and
 * assignment, final settlement — signed from the account's side (positive
 * received, negative paid), each with the journal transaction that posted it.
 * The clearing house's books are reconciled against exactly this register.
 *
 * Funding accruals are one row per position and funding interval, with the
 * rate the interval was charged at.
 */
import { useId, useState, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";

import type { CursorPage } from "../../../api/advanced/core.ts";
import {
  useDerivativeFlows,
  useDerivativeFundingAccruals,
  type DerivativeFlow,
  type DerivativeFundingAccrual,
} from "../../../api/advanced/derivatives.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Identifier, Page, Panel } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { FLOW_KINDS, FLOW_KIND_LABEL, type FlowKind } from "../../../lib/derivatives.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { AccrualsTable } from "./PositionDetail.tsx";
import { Amount, DerivativesNav, derivativesHref } from "./parts.tsx";

function parseFlowKind(raw: string | null): FlowKind | undefined {
  return FLOW_KINDS.find((kind) => kind === raw);
}

export function FlowsPage(): ReactNode {
  const { accountId } = useAdvancedAccount();
  const [params, setParams] = useSearchParams();
  const kind = parseFlowKind(params.get("kind"));
  const selectId = useId();
  return (
    <Page
      title="Flows and funding"
      lead="Every movement between this account's derivative positions and the clearing house, and the funding its perpetuals accrued."
    >
      <SimulatedNotice />
      <DerivativesNav />
      {accountId === undefined ? (
        <Panel title="Flows">
          <NoAccount />
        </Panel>
      ) : (
        <>
          <Panel
            title="Flow register"
            description="Newest first, signed from the account's side: positive received from the clearing house, negative paid to it."
            actions={
              <div className="filter">
                <label htmlFor={selectId}>Flow</label>
                <select
                  id={selectId}
                  className="input"
                  value={kind ?? ""}
                  onChange={(event) => {
                    const value = event.target.value;
                    setParams(value === "" ? {} : { kind: value });
                  }}
                >
                  <option value="">Every flow</option>
                  {FLOW_KINDS.map((k) => (
                    <option key={k} value={k}>
                      {FLOW_KIND_LABEL[k]}
                    </option>
                  ))}
                </select>
              </div>
            }
          >
            <FlowsTable key={`${accountId}-${kind ?? ""}`} accountId={accountId} kind={kind} />
          </Panel>
          <Panel title="Funding accruals" description="One row per perpetual position and funding interval, newest first.">
            <AccrualsList key={`accruals-${accountId}`} accountId={accountId} />
          </Panel>
        </>
      )}
    </Page>
  );
}

function FlowsTable(props: { readonly accountId: string; readonly kind: FlowKind | undefined }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const flows = useDerivativeFlows(props.accountId, props.kind === undefined ? {} : { kind: props.kind }, trail.current);
  return (
    <AsyncPanel
      query={flows}
      loadingLabel="Loading flows…"
      empty={{
        isEmpty: (page: CursorPage<DerivativeFlow>) => page.items.length === 0 && trail.previous.length === 0,
        title: "No flows",
        body: "The backend has registered no flow for this account. That is an empty register, not a zero.",
      }}
    >
      {(page: CursorPage<DerivativeFlow>) => (
        <>
          <ScrollTable
            caption="Flows on this page"
            headers={["When", "Flow", "Contract", "Amount", "Shortfall", "Reference", "Journal"]}
          >
            {page.items.map((flow) => (
              <tr key={flow.id}>
                <td>
                  {formatInstant(flow.occurred_at)}
                  <span className="cell-note">{exactInstant(flow.occurred_at)}</span>
                </td>
                <th scope="row">{FLOW_KIND_LABEL[flow.kind]}</th>
                <td>
                  <Link to={derivativesHref("positions", flow.position_id)}>{flow.symbol}</Link>
                </td>
                <td>
                  <Amount value={flow.amount} asset={flow.settlement_asset} />
                </td>
                <td>
                  <Amount value={flow.shortfall} asset={flow.settlement_asset} />
                </td>
                <td>
                  <code className="mono-small">{flow.reference_type}</code>
                  <span className="cell-note mono-small">{flow.reference_id}</span>
                </td>
                <td>
                  <Identifier value={flow.journal_transaction_id} />
                </td>
              </tr>
            ))}
          </ScrollTable>
          <CursorPager
            noun="flows"
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
  );
}

function AccrualsList(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const accruals = useDerivativeFundingAccruals(props.accountId, undefined, trail.current);
  return (
    <AsyncPanel
      query={accruals}
      loadingLabel="Loading funding accruals…"
      empty={{
        isEmpty: (page: CursorPage<DerivativeFundingAccrual>) => page.items.length === 0 && trail.previous.length === 0,
        title: "No funding accruals",
        body: "No funding interval has been charged to a position of this account.",
      }}
    >
      {(page: CursorPage<DerivativeFundingAccrual>) => <AccrualsTable page={page} trail={trail} onTrail={setTrail} />}
    </AsyncPanel>
  );
}
