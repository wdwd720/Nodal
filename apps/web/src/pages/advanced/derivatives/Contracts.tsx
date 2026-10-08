/**
 * DERIVATIVE CONTRACTS: the futures, perpetuals and options of the canonical
 * instrument model, each with the terms the backend trades it on, its latest
 * mark and, once it has one, its final settlement — and the versioned policy
 * every derivative decision records (margin rates, the funding cap, the
 * disclosed paper slippage).
 *
 * Every figure is the backend's. A contract that has never been marked says
 * so; nothing stands in for a mark.
 */
import { useId, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";

import {
  CONTRACT_LIST_LIMIT,
  useDerivativeContracts,
  useDerivativePolicy,
  type DerivativeContract,
  type DerivativePolicy,
} from "../../../api/advanced/derivatives.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Bps } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { formatMillis } from "../../../lib/advanced.ts";
import {
  DERIVATIVE_KINDS,
  KIND_LABEL,
  PAPER_FILL_NOTE,
  formatRateE8,
  type DerivativeKind,
} from "../../../lib/derivatives.ts";
import { formatInstant } from "../../../lib/time.ts";
import { ScrollTable } from "../shared.tsx";
import { DerivativesNav, Price, derivativesHref } from "./parts.tsx";

function parseKind(raw: string | null): { readonly kind: DerivativeKind | undefined; readonly ignored: string | undefined } {
  if (raw === null || raw === "") return { kind: undefined, ignored: undefined };
  const kind = DERIVATIVE_KINDS.find((candidate) => candidate === raw);
  return kind === undefined ? { kind: undefined, ignored: raw } : { kind, ignored: undefined };
}

export function ContractsPage(): ReactNode {
  const [params, setParams] = useSearchParams();
  const filter = parseKind(params.get("kind"));
  const contracts = useDerivativeContracts(filter.kind);
  const policy = useDerivativePolicy();
  const selectId = useId();

  return (
    <Page
      title="Derivatives"
      lead="Dated futures, perpetual futures and options in PAPER and SANDBOX mode, cleared through one clearing house."
    >
      <SimulatedNotice />
      <DerivativesNav />
      <Panel
        title="Contracts"
        description="As the backend lists them, with the terms it trades each on. Choosing a kind asks the backend for that kind."
        actions={
          <div className="filter">
            <label htmlFor={selectId}>Contract kind</label>
            <select
              id={selectId}
              className="input"
              value={filter.kind ?? ""}
              onChange={(event) => {
                const value = event.target.value;
                setParams(value === "" ? {} : { kind: value });
              }}
            >
              <option value="">All kinds</option>
              {DERIVATIVE_KINDS.map((kind) => (
                <option key={kind} value={kind}>
                  {KIND_LABEL[kind]}
                </option>
              ))}
            </select>
          </div>
        }
      >
        {filter.ignored !== undefined && (
          <p className="note" role="status">
            This address asked for the kind “{filter.ignored}”, which is not a derivative kind, so every kind is listed.
          </p>
        )}
        <AsyncPanel
          query={contracts}
          loadingLabel="Loading contracts…"
          empty={{
            isEmpty: (list: DerivativeContract[]) => list.length === 0,
            title: "No derivative contracts",
            body: "The backend lists no derivative contract for this request. An operator lists them; none is invented here.",
          }}
        >
          {(list: DerivativeContract[]) => (
            <>
              <ScrollTable
                caption="Derivative contracts"
                headers={["Contract", "Kind", "Status", "Latest mark", "Expiry or funding", "Settled in"]}
              >
                {list.map((contract) => (
                  <tr key={contract.id}>
                    <th scope="row">
                      <Link to={derivativesHref("contracts", contract.id)}>{contract.terms.symbol}</Link>
                      <span className="cell-note">{contract.canonical_name}</span>
                    </th>
                    <td>
                      <Pill tone="neutral">{KIND_LABEL[contract.kind]}</Pill>
                      {contract.terms.option_right !== undefined && (
                        <span className="cell-note">
                          {contract.terms.option_right} · {contract.terms.exercise_style ?? "style not stated"} · strike{" "}
                          <Price mantissa={contract.terms.strike_mantissa} scale={contract.terms.price_scale} />
                        </span>
                      )}
                    </td>
                    <td>
                      {contract.status}
                      {contract.settled && <Pill tone="info">settled</Pill>}
                    </td>
                    <td>
                      {contract.latest_mark === undefined ? (
                        <span className="absent">never marked</span>
                      ) : (
                        <>
                          <Price mantissa={contract.latest_mark.mark_mantissa} scale={contract.latest_mark.price_scale} />
                          <span className="cell-note">{formatInstant(contract.latest_mark.observed_at)}</span>
                        </>
                      )}
                    </td>
                    <td>
                      {contract.terms.expiry !== undefined
                        ? `expires ${formatInstant(contract.terms.expiry)}`
                        : contract.terms.funding_interval_ms !== undefined
                          ? `funding every ${formatMillis(contract.terms.funding_interval_ms)}`
                          : "no expiry stated"}
                    </td>
                    <td>{contract.settlement_asset.symbol}</td>
                  </tr>
                ))}
              </ScrollTable>
              <p className="note">
                {list.length === CONTRACT_LIST_LIMIT
                  ? `The backend returned ${String(list.length)} contracts, the most it sends in one answer. More may exist.`
                  : `${String(list.length)} ${list.length === 1 ? "contract" : "contracts"} returned by the backend.`}
              </p>
            </>
          )}
        </AsyncPanel>
      </Panel>

      <Panel
        title="Policy"
        description="The versioned margin, funding and paper-fill policy. Every derivative decision records its version and hash."
      >
        <AsyncPanel query={policy} loadingLabel="Loading the policy…">
          {(p: DerivativePolicy) => <PolicyFields policy={p} />}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

function PolicyFields(props: { readonly policy: DerivativePolicy }): ReactNode {
  const { policy } = props;
  return (
    <>
      <FieldGrid columns={3}>
        <Field label="Version">
          <code className="mono-small">{policy.version}</code>
        </Field>
        <Field label="Hash">
          <code className="mono-small">{policy.hash}</code>
        </Field>
        <Field label="Disclosed paper slippage" note={PAPER_FILL_NOTE}>
          <Bps value={policy.paper_slippage_bps} />
        </Field>
        <Field label="Variation margin interval">{formatMillis(policy.variation_margin_interval_ms)}</Field>
        <Field label="Funding cap" note="The funding rate is clamped to plus or minus this, per interval.">
          {formatRateE8(policy.max_funding_rate_e8)}
        </Field>
        <Field label="Funding buffer" note="Intervals at the cap a perpetual's initial margin carries on top of its rate.">
          {String(policy.funding_buffer_intervals)} intervals
        </Field>
        <Field label="Written option margin floor">
          <Bps value={policy.option_min_margin_bps} />
        </Field>
        <Field label="Written option maintenance share">
          <Bps value={policy.option_maintenance_ratio_bps} />
        </Field>
        <Field label="Expiry gap" note="The jump a contract's initial margin must survive in its last window before expiry.">
          <Bps value={policy.expiry_gap_shock_bps} /> within {formatMillis(policy.expiry_gap_window_ms)} of expiry
        </Field>
      </FieldGrid>
      <ScrollTable caption="Linear margin rates by the underlying's risk class" headers={["Risk class", "Initial", "Maintenance"]}>
        {policy.linear.map((rates) => (
          <tr key={rates.risk_class}>
            <th scope="row">{rates.risk_class}</th>
            <td>
              <Bps value={rates.initial_bps} />
            </td>
            <td>
              <Bps value={rates.maintenance_bps} />
            </td>
          </tr>
        ))}
      </ScrollTable>
      <p className="note">
        A risk class not listed cannot be traded: there is no default rate. A written option is margined at its worst loss over
        the scenario grid of {String(policy.option_shocks_bps.length)} underlying moves.
      </p>
    </>
  );
}
