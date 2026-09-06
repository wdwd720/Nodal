/**
 * ADD FUNDS (PART 111): funding method, provider session, pending state,
 * completed state, funding history, and a clear disclosure of the USDC that is
 * actually credited.
 *
 * The single most important honesty rule on this screen is PART 162: a provider
 * saying a payment succeeded does not credit anything. Money appears when the
 * backend has observed settlement and posted it, which is why the status ladder
 * below is drawn from the backend's own state machine rather than from whether
 * the redirect came back.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useAssets,
  useDeposits,
  useStartDeposit,
  type Deposit,
} from "../api/queries.ts";
import { AsyncPanel, EmptyState, Explanation } from "../components/DataState.tsx";
import { Button } from "../components/Button.tsx";
import {
  Disclosure,
  Field,
  FieldGrid,
  Identifier,
  Page,
  Panel,
  Pill,
  Table,
  type Tone,
} from "../components/Layout.tsx";
import { AssetIdentity } from "../components/MintIdentity.tsx";
import { Qty } from "../components/Money.tsx";
import { PENDING_SETTLEMENT_NOTE, USDC_DISCLOSURE } from "../lib/honesty.ts";
import { parseUsdAmountInput } from "../lib/money.ts";
import { formatInstant } from "../lib/time.ts";
import { useActiveAccountId } from "../session.tsx";

/**
 * The deposit states the contract declares, split by what they mean for money.
 * "Completed" is a small set on purpose: everything else is still in flight.
 */
const PENDING_STATES = [
  "CREATED",
  "SESSION_CREATED",
  "CUSTOMER_ACTION_REQUIRED",
  "PROVIDER_PROCESSING",
  "PROVIDER_CONFIRMED",
  "SETTLEMENT_OBSERVED",
  "RECONCILED",
  "REVIEW_REQUIRED",
] as const;
const COMPLETED_STATES = ["AVAILABLE"] as const;
const FAILED_STATES = ["FAILED", "EXPIRED", "CANCELLED", "REVERSED"] as const;

function statusTone(status: string): Tone {
  if ((COMPLETED_STATES as readonly string[]).includes(status)) return "good";
  if ((FAILED_STATES as readonly string[]).includes(status)) return "bad";
  if (status === "REVIEW_REQUIRED") return "warn";
  if ((PENDING_STATES as readonly string[]).includes(status)) return "info";
  return "neutral";
}

function statusMeaning(status: string): string {
  switch (status) {
    case "CREATED":
      return "The backend has recorded the intent to fund. No provider session exists yet.";
    case "SESSION_CREATED":
      return "A provider session exists. Nothing has been paid and nothing has been credited.";
    case "CUSTOMER_ACTION_REQUIRED":
      return "The provider is waiting for you to finish the payment on its own hosted flow.";
    case "PROVIDER_PROCESSING":
      return "The provider has your payment and has not finished with it. Nothing is credited yet.";
    case "PROVIDER_CONFIRMED":
      return "The provider says it succeeded. That is the provider's claim; the backend has not yet observed the tokens arrive, so nothing is credited.";
    case "SETTLEMENT_OBSERVED":
      return "The backend has seen the transfer on chain and is confirming it.";
    case "RECONCILED":
      return "Expected and observed amounts have been compared and agree.";
    case "AVAILABLE":
      return "Posted to the ledger and usable. This is the only state in which the funds count towards buying power.";
    case "REVIEW_REQUIRED":
      return "Held for review. It will not be credited until the review is resolved.";
    case "FAILED":
      return "The deposit did not complete.";
    case "EXPIRED":
      return "The provider session lapsed before the payment finished.";
    case "CANCELLED":
      return "The deposit was cancelled.";
    case "REVERSED":
      return "The deposit was credited and then reversed.";
    default:
      return "The backend reported a state this app does not have a description for; it is shown verbatim.";
  }
}

export function AddFunds(): ReactNode {
  const accountId = useActiveAccountId();
  const deposits = useDeposits(accountId);
  const assets = useAssets();
  const startDeposit = useStartDeposit();
  const [amount, setAmount] = useState("");
  const [idempotencyKey, setIdempotencyKey] = useState(() => newIdempotencyKey());

  const parsed = parseUsdAmountInput(amount);
  const settlementAsset = (assets.data ?? []).find(
    (asset) => asset.is_stablecoin && asset.status === "ACTIVE",
  );

  if (accountId === undefined) {
    return (
      <Page title="Add funds">
        <EmptyState
          title="No account to fund"
          body="The backend returned no accounts for this session."
        />
      </Page>
    );
  }

  const submit = (): void => {
    if (!parsed.ok) return;
    startDeposit.mutate({ accountId, fiatAmount: parsed.value, idempotencyKey });
  };

  return (
    <Page
      title="Add funds"
      lead="US dollars are converted by a funding provider into USDC held for this account. Dollars are never held here."
    >
      <Panel
        title="What arrives in your account"
        description="The asset that a deposit actually credits."
      >
        {assets.isPending && <p className="loading">Loading the asset registry…</p>}
        {assets.isError && <Explanation error={assets.error} onRetry={() => void assets.refetch()} />}
        {settlementAsset !== undefined ? (
          <AssetIdentity asset={settlementAsset} />
        ) : (
          assets.isSuccess && (
            <EmptyState
              title="No settlement asset is registered"
              body="The asset registry returned no active stablecoin, so this deployment cannot say what a deposit would credit."
            />
          )
        )}
        <Disclosure title="Read this before funding">
          <p>{USDC_DISCLOSURE}</p>
          <p>{PENDING_SETTLEMENT_NOTE}</p>
        </Disclosure>
      </Panel>

      <Panel
        title="Funding method"
        description="What the v1 API accepts, stated exactly."
      >
        <FieldGrid columns={2}>
          <Field label="Accepted currency" note="The contract accepts USD and no other fiat currency.">
            USD
          </Field>
          <Field
            label="Provider"
            note="Chosen by the backend when it creates the session; the app does not choose it and does not offer a list."
          >
            decided by the backend
          </Field>
        </FieldGrid>
        <p className="note">
          The contract has an optional saved funding source, but v1 exposes no endpoint that lists
          saved sources, so this app cannot show you a choice it has no way to enumerate. Requests
          are sent without one and the backend picks the route.
        </p>

        <form
          className="form"
          onSubmit={(event) => {
            event.preventDefault();
            submit();
          }}
        >
          <div className="form-row">
            <label htmlFor="deposit-amount">Amount in US dollars</label>
            <input
              id="deposit-amount"
              name="amount"
              className="input"
              inputMode="decimal"
              autoComplete="off"
              value={amount}
              aria-describedby="deposit-amount-help"
              {...(parsed.ok || amount === "" ? {} : { "aria-invalid": true })}
              onChange={(event) => {
                setAmount(event.target.value);
              }}
            />
            <p id="deposit-amount-help" className="field-note">
              {amount === ""
                ? "Two decimal places at most. The amount is sent to the backend as text, exactly as typed."
                : parsed.ok
                  ? `Will be sent as ${parsed.value}`
                  : parsed.error}
            </p>
          </div>

          <div className="form-actions">
            {parsed.ok ? (
              <Button
                variant="primary"
                submit
                busy={startDeposit.isPending}
                busyLabel="Asking the backend for a provider session…"
              >
                Start funding
              </Button>
            ) : (
              <Button
                variant="primary"
                disabledReason={
                  amount === "" ? "Enter an amount to start a funding session." : parsed.error
                }
              >
                Start funding
              </Button>
            )}
            <Button
              variant="quiet"
              onClick={() => {
                setIdempotencyKey(newIdempotencyKey());
                setAmount("");
                startDeposit.reset();
              }}
            >
              Clear
            </Button>
          </div>
          <p className="mono-small">idempotency key {idempotencyKey}</p>
          <p className="field-note">
            The key above is created once for this attempt and reused if you retry, so a double
            submission cannot fund twice. Clearing the form starts a new one.
          </p>
        </form>

        {startDeposit.isError && (
          <Explanation error={startDeposit.error}>
            <p>
              No provider session was created and no money has moved. Nothing on this page has
              changed as a result of that attempt.
            </p>
          </Explanation>
        )}

        {startDeposit.isSuccess && <ProviderSession deposit={startDeposit.data} />}
      </Panel>

      <Panel title="Funding history" description="Every deposit the backend has recorded for this account.">
        <AsyncPanel
          query={deposits}
          loadingLabel="Loading funding history…"
          empty={{
            isEmpty: (list: Deposit[]) => list.length === 0,
            title: "No deposits recorded",
            body: "The backend has no funding records for this account. This is an empty history, not a zero balance.",
          }}
        >
          {(list: Deposit[]) => (
            <Table
              caption="Funding history"
              headers={[
                "Started",
                "Amount requested",
                "Status",
                "What that means",
                "Counts towards buying power",
                "Observed on chain",
              ]}
            >
              {list.map((deposit) => (
                <tr key={deposit.id}>
                  <td>
                    {formatInstant(deposit.created_at)}
                    <div className="mono-small">{deposit.id}</div>
                  </td>
                  <td>
                    {deposit.fiat_amount === undefined ? (
                      <span className="absent">not reported</span>
                    ) : (
                      <span className="num">
                        {deposit.fiat_amount} {deposit.fiat_currency ?? ""}
                      </span>
                    )}
                  </td>
                  <td>
                    <Pill tone={statusTone(deposit.status)}>{deposit.status}</Pill>
                  </td>
                  <td className="cell-prose">{statusMeaning(deposit.status)}</td>
                  <td>
                    {deposit.buying_power_eligible ? (
                      <Pill tone="good">yes</Pill>
                    ) : (
                      <Pill tone="warn">not yet</Pill>
                    )}
                  </td>
                  <td>
                    {deposit.observed_quantity === undefined ? (
                      <span className="absent">not observed yet</span>
                    ) : (
                      <Qty
                        value={deposit.observed_quantity}
                        decimals={settlementAsset?.decimals}
                        symbol={settlementAsset?.symbol ?? ""}
                      />
                    )}
                  </td>
                </tr>
              ))}
            </Table>
          )}
        </AsyncPanel>
        <p className="note">
          Pending states are listed here and never hidden. A deposit is spendable only in{" "}
          <strong>AVAILABLE</strong>; every other state is still in flight, including the one where
          the provider has already told us it succeeded.
        </p>
      </Panel>
    </Page>
  );
}

/** The provider session the backend created, shown as-is. */
function ProviderSession(props: { readonly deposit: Deposit }): ReactNode {
  const { deposit } = props;
  return (
    <div className="callout">
      <h3>Provider session created</h3>
      <p>
        The backend created a funding record and, where the provider supports it, a session for the
        provider's own hosted flow. Nothing has been paid and nothing has been credited.
      </p>
      <FieldGrid columns={2}>
        <Field label="Deposit">
          <Identifier value={deposit.id} />
        </Field>
        <Field label="Provider">{deposit.provider}</Field>
        <Field label="Provider session">
          <Identifier value={deposit.provider_session_id} />
        </Field>
        <Field label="State">
          <Pill tone={statusTone(deposit.status)}>{deposit.status}</Pill>
        </Field>
        <Field
          label="Client secret reference"
          note="Returned once, for the provider's embedded flow. It is not a credential for this API."
        >
          <Identifier value={deposit.client_secret_ref} />
        </Field>
        <Field label="Reversible until">
          {deposit.reversible_until === undefined ? (
            <span className="absent">not reported</span>
          ) : (
            formatInstant(deposit.reversible_until)
          )}
        </Field>
      </FieldGrid>
      <p className="note">{statusMeaning(deposit.status)}</p>
    </div>
  );
}
