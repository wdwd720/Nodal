/**
 * THE CLEARING HOUSE (operator): the derivatives section's operator page.
 * Here an operator defines a contract (DefineContract.tsx), capitalizes the
 * clearing house -- the counterparty of every derivative position -- in a
 * sandbox asset, so a one-sided book is paid from its capital, and reconciles
 * its books against the flow register (reconciliation kind CLEARING). A
 * difference opens a material mismatch record, and while that record is
 * unresolved the backend refuses new derivative risk.
 *
 * The page offers the assets contracts settle in, as the backend lists them;
 * the backend decides what may be funded and by whom.
 */
import { useId, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useDerivativeContracts,
  useFundClearingHouse,
  useReconcileClearingHouse,
  type DerivativeAsset,
  type DerivativeClearingReport,
  type DerivativeContract,
} from "../../../api/advanced/derivatives.ts";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { parseQuantityInput } from "../../../lib/money.ts";
import { useAdvancedAccount } from "../area.ts";
import { ScrollTable } from "../shared.tsx";
import { DefineContractForm } from "./DefineContract.tsx";
import { ActionKey, Amount, CommandFailure, ConfirmButton, DerivativesNav, TextField, derivativesHref, useActionKey } from "./parts.tsx";

export function ClearingHousePage(): ReactNode {
  const { canInspect } = useAdvancedAccount();
  const contracts = useDerivativeContracts(undefined);
  return (
    <Page
      title="Clearing house"
      lead="The counterparty of every derivative position: its capital, and its books against the flow register."
    >
      <SimulatedNotice />
      <DerivativesNav />
      {!canInspect && (
        <p className="note" role="status">
          This page is for operators. The backend refuses its commands to a session without sandbox:operate; nothing here
          is decided by the page.
        </p>
      )}
      <Panel
        title="Define a contract"
        description="A dated future, a perpetual or an option, defined through the derivatives service. It needs a recent step-up."
      >
        <DefineContractForm />
      </Panel>
      <AsyncPanel
        query={contracts}
        loadingLabel="Loading the assets contracts settle in…"
        empty={{
          isEmpty: (list: DerivativeContract[]) => list.length === 0,
          title: "No contracts, so no settlement asset",
          body: "The backend lists no derivative contract, so there is no asset the clearing house settles in yet.",
        }}
      >
        {(list: DerivativeContract[]) => <ClearingPanels assets={settlementAssets(list)} />}
      </AsyncPanel>
    </Page>
  );
}

/** The distinct assets the listed contracts settle in, in listing order. */
function settlementAssets(list: readonly DerivativeContract[]): DerivativeAsset[] {
  const seen = new Map<string, DerivativeAsset>();
  for (const contract of list) {
    if (!seen.has(contract.settlement_asset.id)) seen.set(contract.settlement_asset.id, contract.settlement_asset);
  }
  return [...seen.values()];
}

function ClearingPanels(props: { readonly assets: readonly DerivativeAsset[] }): ReactNode {
  const selectId = useId();
  const [assetId, setAssetId] = useState(props.assets[0]?.id ?? "");
  const asset = useMemo(() => props.assets.find((a) => a.id === assetId), [props.assets, assetId]);
  if (asset === undefined) {
    return <EmptyState title="No asset chosen" body="Choose the asset to act on." />;
  }
  return (
    <>
      <div className="filter">
        <label htmlFor={selectId}>Settlement asset</label>
        <select
          id={selectId}
          className="input"
          value={asset.id}
          onChange={(event) => {
            setAssetId(event.target.value);
          }}
        >
          {props.assets.map((a) => (
            <option key={a.id} value={a.id}>
              {a.symbol}
            </option>
          ))}
        </select>
      </div>
      <Panel
        title="Capitalize"
        description="Funds the clearing house's capital in this asset. It moves simulated value, so the backend asks for a recent strong sign-in."
      >
        <FundForm key={`fund-${asset.id}`} asset={asset} />
      </Panel>
      <Panel
        title="Reconcile"
        description="Compares the clearing house's books in this asset with the flow register. A difference opens a reconciliation record."
      >
        <ReconcileForm key={`reconcile-${asset.id}`} asset={asset} />
      </Panel>
    </>
  );
}

function FundForm(props: { readonly asset: DerivativeAsset }): ReactNode {
  const { asset } = props;
  const inputId = useId();
  const [amount, setAmount] = useState("");
  const action = useActionKey();
  const fund = useFundClearingHouse();
  const parsed = parseQuantityInput(amount, asset.decimals);
  const blocked = parsed.ok ? undefined : amount === "" ? "Enter an amount." : parsed.error;
  return (
    <>
      <div className="form">
        <TextField
          id={inputId}
          label={`Amount of ${asset.symbol}`}
          value={amount}
          problem={parsed.ok ? undefined : parsed.error}
          hint={`Exact to ${String(asset.decimals)} decimal places; it is sent as base units and never rounded.`}
          onChange={(value) => {
            setAmount(value);
            action.reset();
            fund.reset();
          }}
        />
      </div>
      <FieldGrid columns={2}>
        <Field label="Base units to be sent">{parsed.ok ? <code className="mono-small">{parsed.value}</code> : <span className="absent">none yet</span>}</Field>
        <ActionKey actionKey={action.key} />
      </FieldGrid>
      <div className="form-actions">
        <ConfirmButton
          blockedReason={blocked}
          busy={fund.isPending}
          busyLabel="Funding…"
          onConfirm={() => {
            if (!parsed.ok) return;
            fund.mutate({ assetId: asset.id, quantity: parsed.value, idempotencyKey: action.confirm() });
          }}
        >
          Confirm: capitalize the clearing house
        </ConfirmButton>
      </div>
      {fund.isError && <CommandFailure error={fund.error} />}
      {fund.isSuccess && (
        <div className="callout" role="status">
          <p>
            Posted <Amount value={fund.data.quantity} asset={fund.data.asset} /> of capital in{" "}
            {String(fund.data.journal_transaction_ids.length)} journal{" "}
            {fund.data.journal_transaction_ids.length === 1 ? "transaction" : "transactions"}.
          </p>
        </div>
      )}
    </>
  );
}

function ReconcileForm(props: { readonly asset: DerivativeAsset }): ReactNode {
  const { asset } = props;
  const action = useActionKey();
  const reconcile = useReconcileClearingHouse();
  return (
    <>
      <div className="form-actions">
        <ConfirmButton
          blockedReason={undefined}
          busy={reconcile.isPending}
          busyLabel="Reconciling…"
          onConfirm={() => {
            // A reconciliation that has answered is done; asking again is a new
            // act with a new key. A failed one is retried under the same key.
            if (reconcile.isSuccess) action.reset();
            reconcile.mutate({ assetId: asset.id, idempotencyKey: action.confirm() });
          }}
        >
          Reconcile {asset.symbol} now
        </ConfirmButton>
      </div>
      {reconcile.isError && <CommandFailure error={reconcile.error} />}
      {reconcile.isSuccess && <ReportFields report={reconcile.data} />}
    </>
  );
}

function ReportFields(props: { readonly report: DerivativeClearingReport }): ReactNode {
  const { report } = props;
  const asset = report.asset;
  return (
    <>
      <p>
        {report.matched ? (
          <Pill tone="good">books agree with the register</Pill>
        ) : (
          <Pill tone="bad">books disagree with the register</Pill>
        )}{" "}
        {report.internally_matched ? (
          <Pill tone="info">internally matched: the clearing house carries no position</Pill>
        ) : (
          <Pill tone="neutral">the clearing house carries the net open interest</Pill>
        )}
      </p>
      <FieldGrid columns={3}>
        <Field label="Holdings" note="The clearing house's holdings account in this asset.">
          <Amount value={report.holdings} asset={asset} />
        </Field>
        <Field label="Capital">
          <Amount value={report.capital} asset={asset} />
        </Field>
        <Field label="Result" note="What the clearing house has collected less what it has paid.">
          <Amount value={report.result} asset={asset} />
        </Field>
        <Field label="Register net" note="The register's amounts, from the customers' side.">
          <Amount value={report.register_net} asset={asset} />
        </Field>
        <Field label="Expected result" note="Minus the register net.">
          <Amount value={report.expected_result} asset={asset} />
        </Field>
        <Field label="Difference">
          <Amount value={report.difference} asset={asset} />
        </Field>
        <Field label="Register shortfall">
          <Amount value={report.register_shortfall} asset={asset} />
        </Field>
        <Field label="Flows in the register">{String(report.flows)}</Field>
        <Field label="Reconciliation record">
          {report.record_id === undefined ? <span className="absent">none open</span> : <Identifier value={report.record_id} />}
        </Field>
      </FieldGrid>
      {report.net_open_interest.length > 0 && (
        <ScrollTable caption="Net open interest by contract" headers={["Contract", "Net contracts"]}>
          {report.net_open_interest.map((row) => (
            <tr key={row.instrument_id}>
              <th scope="row">
                <Link to={derivativesHref("contracts", row.instrument_id)}>
                  <code className="mono-small">{row.instrument_id}</code>
                </Link>
              </th>
              <td>{row.quantity}</td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </>
  );
}
