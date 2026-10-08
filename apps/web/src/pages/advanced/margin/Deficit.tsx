/**
 * The covered deficit of the account's latest bankrupt liquidation case: what
 * the insurance fund covered, what the customer has repaid of it, what is
 * still owed, every repayment, and -- for the account's owner -- the
 * repayment command (REPAY, posted as MARGIN_REPAY to the fund that covered
 * it).
 *
 * Every figure is the margin service's, in exact base units of the account's
 * numeraire. The service decides whether a repayment may be made, refuses one
 * beyond what is still owed, and refuses one where there is no covered
 * deficit; the page shows those answers with their codes.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import {
  useMarginDeficit,
  useRepayDeficit,
  type AmountRequest,
  type MarginDeficitRepaymentResult,
  type MarginDeficitStatus,
} from "../../../api/advanced/margin.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Qty } from "../../../components/Money.tsx";
import { caseStatusTone } from "../../../lib/margin.ts";
import { parseQuantityInput } from "../../../lib/money.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { ScrollTable } from "../shared.tsx";
import { CommandFlow, caseHref, type Prepared } from "./parts.tsx";

/** True when the backend reported a deficit with something still owed on it. */
function stillOwed(status: MarginDeficitStatus): boolean {
  return status.outstanding !== undefined && !/^0+$/.test(status.outstanding);
}

/**
 * True when there is no covered deficit to show: none was covered, or the
 * account has no margin account at all (answered without its numeraire).
 */
function nothingOwed(status: MarginDeficitStatus): boolean {
  return status.deficit === undefined || status.numeraire_decimals === undefined;
}

export function DeficitPanel(props: {
  readonly accountId: string;
  readonly unit: string;
  /** True when the session owns the account and may repay. */
  readonly canAct: boolean;
}): ReactNode {
  const deficit = useMarginDeficit(props.accountId);
  return (
    <Panel
      title="Covered deficit"
      description="When a liquidation leaves loans the account cannot repay, the insurance fund covers them and the account owes the fund that deficit. Quantities are base units of the numeraire."
    >
      <AsyncPanel
        query={deficit}
        loadingLabel="Loading the account's covered deficit…"
        empty={{
          isEmpty: nothingOwed,
          title: "No covered deficit",
          body: "The backend reports no deficit the insurance fund covered for this account's latest liquidation case, so nothing is owed to the fund.",
        }}
      >
        {(status: MarginDeficitStatus) => (
          <>
            <FieldGrid columns={3}>
              <Field label="Deficit covered by the fund" emphasis>
                <span data-field="deficit">
                  <Qty value={status.deficit} decimals={status.numeraire_decimals} symbol={props.unit} />
                </span>
                <span className="cell-note">
                  <BaseUnits value={status.deficit} />
                </span>
              </Field>
              <Field label="Repaid so far">
                <span data-field="deficit-repaid">
                  <Qty value={status.repaid} decimals={status.numeraire_decimals} symbol={props.unit} />
                </span>
              </Field>
              <Field label="Still owed" note="The repayments of a case never exceed its deficit; the backend and its database both refuse more.">
                <span data-field="deficit-outstanding">
                  <Qty value={status.outstanding} decimals={status.numeraire_decimals} symbol={props.unit} />
                </span>
                <span className="cell-note">
                  <BaseUnits value={status.outstanding} />
                </span>
              </Field>
              {status.case !== undefined && (
                <Field label="Liquidation case" note={status.case.last_reason}>
                  <Link to={caseHref(status.case.id)}>
                    <code className="mono-small">{status.case.id}</code>
                  </Link>{" "}
                  <Pill tone={caseStatusTone(status.case.status)}>{status.case.status}</Pill>
                </Field>
              )}
              {status.case?.deficit_covered_at !== undefined && (
                <Field label="Covered" note={exactInstant(status.case.deficit_covered_at)}>
                  {formatInstant(status.case.deficit_covered_at)}
                </Field>
              )}
            </FieldGrid>
            {status.repayments.length === 0 ? (
              <p className="note">No repayment of this deficit is recorded.</p>
            ) : (
              <ScrollTable caption="Repayments of the covered deficit" headers={["Repaid", "Amount", "Journal", "Effect", "Intent"]}>
                {status.repayments.map((repayment) => (
                  <tr key={repayment.id} data-repayment={repayment.id}>
                    <th scope="row">{formatInstant(repayment.repaid_at)}</th>
                    <td>
                      <Qty value={repayment.amount} decimals={status.numeraire_decimals} symbol={props.unit} />
                    </td>
                    <td>
                      <Identifier value={repayment.journal_transaction_id} />
                    </td>
                    <td>
                      <Identifier value={repayment.effect_id} />
                    </td>
                    <td>
                      <Identifier value={repayment.intent_id} />
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            )}
            {props.canAct && stillOwed(status) && status.numeraire_decimals !== undefined && (
              <RepayDeficitForm accountId={props.accountId} decimals={status.numeraire_decimals} unit={props.unit} />
            )}
            {props.canAct && !stillOwed(status) && (
              <p className="note">The covered deficit is fully repaid; there is nothing left to repay.</p>
            )}
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function RepayDeficitForm(props: { readonly accountId: string; readonly decimals: number; readonly unit: string }): ReactNode {
  const repay = useRepayDeficit();
  const [amount, setAmount] = useState("");
  const parsed = parseQuantityInput(amount, props.decimals);
  const prepared: Prepared<AmountRequest> = parsed.ok
    ? { ok: true, request: { accountId: props.accountId, amount: parsed.value } }
    : { ok: false, reason: parsed.error };
  return (
    <form
      className="form"
      aria-label="Repay the covered deficit"
      onSubmit={(event) => {
        event.preventDefault();
      }}
    >
      <div className="form-row">
        <label htmlFor="margin-deficit-amount">{`Amount to repay, in ${props.unit}`}</label>
        <input
          id="margin-deficit-amount"
          className="input"
          inputMode="decimal"
          autoComplete="off"
          value={amount}
          aria-describedby="margin-deficit-amount-help"
          onChange={(event) => {
            setAmount(event.target.value);
          }}
        />
        <p id="margin-deficit-amount-help" className="field-note">
          Paid from the wallet to the insurance fund that covered the deficit, converted exactly to base units. Risk
          Kernel V2 checks the balance and the backend refuses more than is still owed.
        </p>
      </div>
      <CommandFlow<AmountRequest, MarginDeficitRepaymentResult>
        label="Repay the deficit"
        prepared={prepared}
        command={repay}
        refusedNote="Nothing was repaid."
        describe={(request) => (
          <Field label="Amount">
            <Qty value={request.amount} decimals={props.decimals} symbol={props.unit} />
          </Field>
        )}
        renderResult={(result) => (
          <p className="note" role="status" aria-label="The repayment" data-repayment-result={result.repayment_id}>
            {result.replayed
              ? "The backend found this repayment already applied and applied nothing again. "
              : "Repaid. "}
            <Qty value={result.repaid} decimals={result.numeraire_decimals} symbol={props.unit} /> to the fund, journal{" "}
            <code className="mono-small">{result.journal_transaction_id}</code>; still owed{" "}
            <Qty value={result.outstanding} decimals={result.numeraire_decimals} symbol={props.unit} />.
          </p>
        )}
      />
    </form>
  );
}
