/**
 * Pieces the margin pages share. None of them computes a figure: every amount
 * is a string the margin engine computed, rendered through the Money
 * components, and every command goes through CommandFlow, which creates its
 * idempotency key when the user confirms and reuses it for every retry.
 */
import { useState, type ReactNode } from "react";
import { NavLink } from "react-router-dom";
import { newIdempotencyKey } from "@controlplane/generated-client";

import type { Asset } from "../../../api/queries.ts";
import type {
  MarginCall,
  MarginCase,
  MarginCommand,
  MarginEvaluation,
  MarginLiquidationStep,
  MarginPrice,
} from "../../../api/advanced/margin.ts";
import { Button, type ButtonVariant } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { AsOf, Field, FieldGrid, Identifier, Pill } from "../../../components/Layout.tsx";
import { Absent, BaseUnits, Bps, Qty } from "../../../components/Money.tsx";
import {
  CALL_STATUS_MEANING,
  CASE_STATUS_MEANING,
  HAIRCUT_COMPONENTS,
  assetById,
  callStatusTone,
  caseStatusTone,
  confirmAttempt,
  formatLeverage,
  formatMarginPrice,
  type ConfirmedAttempt,
} from "../../../lib/margin.ts";
import { formatInstant } from "../../../lib/time.ts";
import { advancedHref } from "../area.ts";
import { ScrollTable } from "../shared.tsx";

/* --------------------------------------------------------------------------
 * Navigation
 * ------------------------------------------------------------------------ */

const MARGIN_PAGES: ReadonlyArray<{ readonly path: string; readonly label: string }> = [
  { path: "", label: "Margin account" },
  { path: "history", label: "Calls and cases" },
  { path: "stress", label: "Stress tests" },
  { path: "operator", label: "Operator" },
];

/** The section's own pages. */
export function MarginNav(): ReactNode {
  return (
    <nav className="subnav" aria-label="Margin pages">
      <ul>
        {MARGIN_PAGES.map((page) => (
          <li key={page.path}>
            <NavLink
              end
              to={page.path === "" ? advancedHref("margin") : advancedHref("margin", page.path)}
              className={({ isActive }) => (isActive ? "subnav-link subnav-link-active" : "subnav-link")}
            >
              {page.label}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}

export function caseHref(caseId: string): string {
  return advancedHref("margin", "cases", caseId);
}

export function stressRunHref(runId: string): string {
  return advancedHref("margin", "stress", runId);
}

/* --------------------------------------------------------------------------
 * Assets and prices
 * ------------------------------------------------------------------------ */

/** An asset by its registry symbol, or by its id when the registry does not list it. */
export function AssetName(props: { readonly assets: readonly Asset[]; readonly id: string }): ReactNode {
  const asset = assetById(props.assets, props.id);
  if (asset === undefined) return <Identifier value={props.id} />;
  return (
    <span title={props.id}>
      {asset.symbol} <span className="mono-small">{props.id}</span>
    </span>
  );
}

/** Base units of an asset, formatted with its registry decimals, or as raw base units when they are unknown. */
export function AssetQty(props: {
  readonly assets: readonly Asset[];
  readonly id: string;
  readonly value: string | undefined;
}): ReactNode {
  const asset = assetById(props.assets, props.id);
  if (asset === undefined) return <BaseUnits value={props.value} />;
  return <Qty value={props.value} decimals={asset.decimals} symbol={asset.symbol} />;
}

/** A price of one whole unit in the numeraire, exactly as the engine stated it. */
export function Price(props: { readonly price: MarginPrice | undefined; readonly unit: string }): ReactNode {
  if (props.price === undefined) return <Absent what="no mark" />;
  const { price } = props;
  return (
    <span className="num">
      {formatMarginPrice(price.mantissa, price.scale)} {props.unit}
      {price.observed_at !== undefined && <span className="cell-note">observed {formatInstant(price.observed_at)}</span>}
    </span>
  );
}

/* --------------------------------------------------------------------------
 * The evaluation's figures
 * ------------------------------------------------------------------------ */

/** Effective leverage against the mode's cap, as the engine states them in basis points. */
export function Leverage(props: { readonly evaluation: MarginEvaluation }): ReactNode {
  const ev = props.evaluation;
  return (
    <Field
      label="Effective leverage"
      note="Gross exposure over equity, rounded up by the engine, against the cap of this account's margin mode."
    >
      <span data-field="effective_leverage">
        {ev.leverage_unbounded ? (
          <Pill tone="bad">unbounded — exposure with no positive equity</Pill>
        ) : (
          <span className="num">{formatLeverage(ev.effective_leverage_bps)}</span>
        )}
      </span>
      <span className="cell-note">
        cap {formatLeverage(ev.leverage_cap_bps)} <Bps value={ev.effective_leverage_bps} />
      </span>
    </Field>
  );
}

/** When the evaluation was made, and how fresh the marks behind it were. */
export function EvaluationMeta(props: { readonly evaluation: MarginEvaluation }): ReactNode {
  const ev = props.evaluation;
  return (
    <>
      <AsOf at={ev.at} label="Evaluated at" />
      <p className="meta-row">
        <span className="mono-small">policy {ev.policy_version}</span>
        <span className="mono-small">evaluation {ev.hash}</span>
        {ev.prices_fresh ? (
          <Pill tone="good">every mark fresh</Pill>
        ) : (
          <Pill tone="warn" title="A stale, missing or future mark makes that collateral worth nothing to the engine.">
            {ev.missing_marks ? "a mark is missing" : "a mark is stale"}
          </Pill>
        )}
      </p>
    </>
  );
}

/** Collateral by asset, with the haircut component by component. */
export function HoldingsTable(props: {
  readonly evaluation: MarginEvaluation;
  readonly assets: readonly Asset[];
  readonly unit: string;
}): ReactNode {
  const ev = props.evaluation;
  const decimals = ev.numeraire_decimals;
  return (
    <ScrollTable
      caption="Collateral by asset"
      headers={["Asset", "Quantity", "Mark", "Market value", "Haircut", "Collateral value", "Initial / maintenance"]}
    >
      {ev.holdings.map((holding) => (
        <tr key={holding.asset_id} data-asset={holding.asset_id}>
          <th scope="row">
            <AssetName assets={props.assets} id={holding.asset_id} />
            {holding.numeraire && <span className="cell-note">the numeraire: valued at par, never haircut</span>}
          </th>
          <td>
            <AssetQty assets={props.assets} id={holding.asset_id} value={holding.quantity} />
          </td>
          <td>
            {holding.numeraire ? (
              <span className="absent">par</span>
            ) : (
              <>
                <Price price={holding.mark} unit={props.unit} />
                {!holding.fresh && <Pill tone="warn">stale or missing</Pill>}
              </>
            )}
          </td>
          <td>
            <Qty value={holding.market_value} decimals={decimals} symbol={props.unit} />
          </td>
          <td>
            <Bps value={holding.haircut.total_bps} />
            {holding.haircut.zero_value && <span className="cell-note">worth nothing to the engine</span>}
            <ul className="codes">
              {HAIRCUT_COMPONENTS.map(([field, label]) =>
                holding.haircut[field] === 0 ? null : (
                  <li key={field}>
                    {label} <Bps value={holding.haircut[field]} />
                  </li>
                ),
              )}
            </ul>
          </td>
          <td data-field={`collateral_value:${holding.asset_id}`}>
            <Qty value={holding.collateral_value} decimals={decimals} symbol={props.unit} />
          </td>
          <td>
            <Qty value={holding.initial_margin} decimals={decimals} symbol={props.unit} />
            <span className="cell-note">
              <Qty value={holding.maintenance_margin} decimals={decimals} symbol={props.unit} />
            </span>
          </td>
        </tr>
      ))}
    </ScrollTable>
  );
}

/* --------------------------------------------------------------------------
 * Calls, cases and steps
 * ------------------------------------------------------------------------ */

export function CallSummary(props: { readonly call: MarginCall; readonly unit: string }): ReactNode {
  const { call } = props;
  const decimals = call.numeraire_decimals;
  return (
    <FieldGrid columns={3}>
      <Field label="Status" note={CALL_STATUS_MEANING[call.status]}>
        <Pill tone={callStatusTone(call.status)}>{call.status}</Pill>
      </Field>
      <Field label="Issued" note="When the engine found equity below maintenance.">
        {formatInstant(call.issued_at)}
      </Field>
      <Field label="Deficiency" note="Maintenance margin less equity, when the call was issued.">
        <Qty value={call.deficiency} decimals={decimals} symbol={props.unit} />
      </Field>
      <Field label="Equity when issued" note="In base units of the numeraire.">
        <Qty value={call.equity} decimals={decimals} symbol={props.unit} />
      </Field>
      <Field label="Maintenance when issued" note="The requirement the equity fell below.">
        <Qty value={call.maintenance_margin} decimals={decimals} symbol={props.unit} />
      </Field>
      <Field label="Resolved" note="A call is resolved when it is met or a liquidation case takes it over.">
        {call.resolved_at === undefined ? <span className="absent">not resolved</span> : formatInstant(call.resolved_at)}
      </Field>
    </FieldGrid>
  );
}

export function CaseSummary(props: { readonly kase: MarginCase; readonly unit: string }): ReactNode {
  const { kase } = props;
  return (
    <FieldGrid columns={3}>
      <Field label="Status" note={CASE_STATUS_MEANING[kase.status]}>
        <Pill tone={caseStatusTone(kase.status)}>{kase.status}</Pill>
      </Field>
      <Field label="Steps" note="Steps executed of the step budget; a case that cannot progress goes to a person.">
        <span className="num">
          {String(kase.steps_executed)} of {String(kase.max_steps)}
        </span>
      </Field>
      <Field label="Target" note="What a partial liquidation restores.">
        <code className="mono-small">{kase.target}</code>
      </Field>
      <Field label="Last reason" note="The engine's own words for where the case stands.">
        {kase.last_reason}
      </Field>
      <Field label="Deficit" note="What remained owed once nothing was left; only a bankrupt case has one.">
        {kase.deficit === undefined ? (
          <span className="absent">none recorded</span>
        ) : (
          <Qty value={kase.deficit} decimals={kase.numeraire_decimals} symbol={props.unit} />
        )}
      </Field>
      <Field label="Insurance fund coverage" note="When the insurance fund covered the deficit, and the journal that did it.">
        {kase.deficit_covered_at === undefined ? (
          <span className="absent">not covered</span>
        ) : (
          <>
            {formatInstant(kase.deficit_covered_at)}
            <Identifier value={kase.deficit_coverage_journal_transaction_id} />
          </>
        )}
      </Field>
    </FieldGrid>
  );
}

export function StepsTable(props: {
  readonly steps: readonly MarginLiquidationStep[];
  readonly decimals: number;
  readonly unit: string;
  readonly assets: readonly Asset[];
}): ReactNode {
  const { decimals, unit } = props;
  return (
    <ScrollTable
      caption="Liquidation steps"
      headers={["Step", "Action", "Asset and quantity", "Mark / execution", "Proceeds", "Fee", "Repaid", "Excess", "Equity before / after"]}
    >
      {props.steps.map((step) => (
        <tr key={step.id}>
          <th scope="row">{String(step.step_no)}</th>
          <td>
            <code className="mono-small">{step.action}</code>
            <span className="cell-note">{formatInstant(step.executed_at)}</span>
          </td>
          <td>
            <AssetName assets={props.assets} id={step.asset_id} />
            <span className="cell-note">
              <AssetQty assets={props.assets} id={step.asset_id} value={step.quantity} />
            </span>
          </td>
          <td>
            <Price price={step.mark} unit={unit} />
            <span className="cell-note">
              <Price price={step.execution_price} unit={unit} /> slippage <Bps value={step.slippage_bps} /> of cap{" "}
              <Bps value={step.slippage_cap_bps} />
            </span>
          </td>
          <td>
            <Qty value={step.proceeds} decimals={decimals} symbol={unit} />
          </td>
          <td>
            <Qty value={step.fee} decimals={decimals} symbol={unit} />
          </td>
          <td>
            <Qty value={step.repaid} decimals={decimals} symbol={unit} />
          </td>
          <td>
            <Qty value={step.excess} decimals={decimals} symbol={unit} />
          </td>
          <td>
            <Qty value={step.equity_before} decimals={decimals} symbol={unit} />
            <span className="cell-note">
              <Qty value={step.equity_after} decimals={decimals} symbol={unit} />
            </span>
          </td>
        </tr>
      ))}
    </ScrollTable>
  );
}

/* --------------------------------------------------------------------------
 * Commands
 * ------------------------------------------------------------------------ */

/** What a form currently describes: a request it could send, or why it cannot. */
export type Prepared<R> = { readonly ok: true; readonly request: R } | { readonly ok: false; readonly reason: string };

/**
 * Review, confirm, and the outcome of one command.
 *
 * The idempotency key is created when the user confirms and not before; the
 * attempt keeps it, and "Try again" sends the same request with the same key,
 * so a retry can never apply twice — the backend answers it as the request it
 * already saw. A changed request is a new attempt with a new key.
 */
export function CommandFlow<R, T>(props: {
  readonly label: string;
  readonly prepared: Prepared<R>;
  readonly describe: (request: R) => ReactNode;
  readonly command: MarginCommand<R, T>;
  readonly renderResult: (result: T) => ReactNode;
  /** What a refusal leaves unchanged, said plainly. */
  readonly refusedNote: string;
  readonly variant?: ButtonVariant;
}): ReactNode {
  const [review, setReview] = useState<R | undefined>(undefined);
  const [attempt, setAttempt] = useState<ConfirmedAttempt<R> | undefined>(undefined);
  const { command } = props;
  const sent = attempt !== undefined && (command.isPending || command.isError || command.isSuccess);

  const confirm = (request: R): void => {
    const next = confirmAttempt(attempt, request, newIdempotencyKey);
    setAttempt(next);
    setReview(undefined);
    command.mutate(next);
  };

  const finish = (): void => {
    setAttempt(undefined);
    setReview(undefined);
    command.reset();
  };

  if (sent && attempt !== undefined) {
    return (
      <div className="command">
        <div className="command-request">{props.describe(attempt.request)}</div>
        <p className="mono-small">idempotency key {attempt.idempotencyKey}</p>
        {command.isPending && <p className="loading">Sending to the backend…</p>}
        {command.isError && (
          <>
            <Explanation error={command.error}>
              <p>{props.refusedNote}</p>
            </Explanation>
            <div className="form-actions">
              <Button
                onClick={() => {
                  command.mutate(attempt);
                }}
              >
                Try again with the same key
              </Button>
              <Button variant="quiet" onClick={finish}>
                Change the request
              </Button>
            </div>
            <p className="field-note">
              Trying again sends the same request with the same key, so it cannot apply twice; a refusal the backend
              recorded for that key is answered the same way. Changing the request starts a new attempt with a new key.
            </p>
          </>
        )}
        {command.isSuccess && (
          <>
            {props.renderResult(command.data)}
            <div className="form-actions">
              <Button variant="quiet" onClick={finish}>
                Done
              </Button>
            </div>
          </>
        )}
      </div>
    );
  }

  if (review !== undefined) {
    return (
      <div className="command">
        <p className="command-title">Review what will be sent</p>
        <div className="command-request">{props.describe(review)}</div>
        <div className="form-actions">
          <Button
            variant={props.variant ?? "primary"}
            onClick={() => {
              confirm(review);
            }}
          >
            Confirm: {props.label}
          </Button>
          <Button
            variant="quiet"
            onClick={() => {
              setReview(undefined);
            }}
          >
            Change
          </Button>
        </div>
        <p className="field-note">
          Confirming creates this request's idempotency key; it is kept for any retry of the same request.
        </p>
      </div>
    );
  }

  return (
    <div className="form-actions">
      {props.prepared.ok ? (
        <Button
          variant="secondary"
          onClick={() => {
            if (props.prepared.ok) setReview(props.prepared.request);
          }}
        >
          Review: {props.label}
        </Button>
      ) : (
        <Button variant="secondary" disabledReason={props.prepared.reason}>
          Review: {props.label}
        </Button>
      )}
    </div>
  );
}

/** A refusal of a whole result list, item by item, with the backend's codes. */
export function Failures(props: {
  readonly failures: ReadonlyArray<{ readonly code: string; readonly detail: string; readonly account_id?: string }>;
}): ReactNode {
  if (props.failures.length === 0) return null;
  return (
    <ScrollTable caption="Accounts the backend could not process" headers={["Account", "Code", "Detail"]}>
      {props.failures.map((failure, index) => (
        <tr key={`${failure.code}-${String(index)}`}>
          <th scope="row">
            {failure.account_id === undefined ? <span className="absent">not named</span> : <Identifier value={failure.account_id} />}
          </th>
          <td>
            <code className="mono-small">{failure.code}</code>
          </td>
          <td className="cell-prose">{failure.detail}</td>
        </tr>
      ))}
    </ScrollTable>
  );
}
