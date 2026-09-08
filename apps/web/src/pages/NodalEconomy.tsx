/**
 * NODAL ECONOMY (gola.md PARTS XII–XXI, LII).
 *
 * This page exists to make one rule visible: **Nodal Economy, Simulated Capital
 * and Real Capital are three different things and this interface never adds
 * them together.**
 *
 * The rule is structural rather than stylistic. There is no query hook that
 * produces a combined figure, so no component here can render one by accident;
 * what this page does is name the other two pots and say where they live, so
 * that a customer who sees a Credit balance is never left to assume it is
 * money or that it is the whole picture.
 *
 * The second rule is about the breakdown. A Credit balance is never one number:
 * what may be paid out — if anything — depends on where each unit came from,
 * so `gross` and `payout_eligible` are shown as the different figures they are,
 * with the reasons the remainder is not eligible.
 */
import type { ReactNode } from "react";

import { useCreditBalance, type CreditBalance } from "../api/queries.ts";
import { AsyncPanel, EmptyState } from "../components/DataState.tsx";
import { LinkButton } from "../components/Button.tsx";
import { Disclosure, Field, FieldGrid, Page, Panel, Pill, Table } from "../components/Layout.tsx";
import { BaseUnits, Qty } from "../components/Money.tsx";
import {
  CREDITS_DISCLOSURE,
  PROVENANCE_NOTE,
  THREE_POTS_NOTE,
} from "../lib/honesty.ts";
import { useActiveAccountId } from "../session.tsx";

/**
 * Credits are held at six decimal places. The scale is a property of the asset
 * and the API does not repeat it on the balance response, so it is stated here
 * once rather than guessed per figure.
 */
const CREDIT_DECIMALS = 6;

/** Human labels for the provenance origins, so a table is not a wall of enums. */
const ORIGIN_LABELS: Readonly<Record<string, string>> = {
  PURCHASED: "Bought with money",
  PROMOTIONAL: "Given by Nodal",
  REFUND: "Refunded",
  CREATOR_EARNING: "Earned by creating",
  DATA_SALE_EARNING: "Earned by selling data",
  AGENT_SERVICE_EARNING: "Earned by running an agent for someone",
  MARKET_CREATOR_EARNING: "Earned as a market creator",
  MARKET_TRADING_PROCEEDS: "Made trading an internal market",
  COMPETITION_REWARD: "Won in a competition",
  ADMIN_ADJUSTMENT: "Adjusted by an operator",
  PROVIDER_SETTLEMENT: "Settled by a provider",
};

/** Human labels for funding finality. */
const FINALITY_LABELS: Readonly<Record<string, string>> = {
  UNFUNDED: "No money behind it",
  REVERSIBLE: "Money behind it can still be reversed",
  SETTLED: "Money behind it has settled",
  DISPUTED: "Money behind it is being disputed",
  REVERSED: "Money behind it was taken back",
};

function label(map: Readonly<Record<string, string>>, key: string): string {
  return map[key] ?? key;
}

function breakdownRows(
  map: Record<string, string> | undefined,
  labels: Readonly<Record<string, string>>,
): ReadonlyArray<readonly [string, string]> {
  if (map === undefined) return [];
  return Object.entries(map)
    .filter(([, amount]) => amount !== "0")
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([key, amount]) => [label(labels, key), amount] as const);
}

export function NodalEconomy(): ReactNode {
  const accountId = useActiveAccountId();
  const credits = useCreditBalance(accountId);

  if (accountId === undefined) {
    return (
      <Page title="Nodal Economy">
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is no balance to show."
        />
      </Page>
    );
  }

  return (
    <Page
      title="Nodal Economy"
      lead="Credits and what they can do. This is one of three separate pots, and it is never added to the others."
      actions={<LinkButton to="/payouts">Payouts</LinkButton>}
    >
      <ThreePots />

      <Panel
        title="Credits"
        description="Every figure is computed by the backend from the individual lots this account holds."
      >
        <AsyncPanel
          query={credits}
          loadingLabel="Asking the backend for this account's Credit lots…"
          empty={{
            isEmpty: (b: CreditBalance) => b.gross === "0",
            title: "No Credits recorded",
            body: "The backend holds no Credit lots for this account. That is an answer, not a loading state.",
          }}
        >
          {(balance: CreditBalance) => (
            <>
            <FieldGrid columns={3}>
              <Field
                label="Total held"
                note="Every remaining unit, whatever its origin and whatever its state."
                emphasis
              >
                <Qty value={balance.gross} decimals={CREDIT_DECIMALS} symbol="Credits" />
              </Field>
              <Field label="Usable inside Nodal" note="What can fund internal activity right now.">
                <Qty value={balance.spendable} decimals={CREDIT_DECIMALS} symbol="Credits" />
              </Field>
              <Field
                label="Payout eligible"
                note="What the named policy version permits to leave the system right now. A different number from the total, on purpose."
                emphasis
              >
                <Qty value={balance.payout_eligible} decimals={CREDIT_DECIMALS} symbol="Credits" />
              </Field>
              <Field label="Frozen" note="Held while the money behind it is disputed.">
                <Qty value={balance.frozen} decimals={CREDIT_DECIMALS} symbol="Credits" />
              </Field>
              <Field label="Not payout eligible" note="The remainder, with the reasons below.">
                <Qty value={balance.ineligible} decimals={CREDIT_DECIMALS} symbol="Credits" />
              </Field>
              <Field label="Policy" note="The version of the payout policy these figures were computed under.">
                <span className="mono-small">{balance.policy_version}</span>
              </Field>
            </FieldGrid>

            <p className="field-note">
              Exact base units, as the backend holds them: <BaseUnits value={balance.gross} /> held,{" "}
              <BaseUnits value={balance.payout_eligible} /> eligible.
            </p>

            {Array.isArray(balance.ineligible_reasons) && balance.ineligible_reasons.length > 0 && (
              <Panel
                title="Why the rest cannot be paid out"
                description="The backend's own reasons, not an interpretation of them."
              >
                <ul className="reasons">
                  {balance.ineligible_reasons.map((reason) => (
                    <li key={reason}>{reason}</li>
                  ))}
                </ul>
              </Panel>
            )}

            <Origins balance={balance} />
              <Disclosure title="What Credits are">
                <p>{CREDITS_DISCLOSURE}</p>
                <p>{PROVENANCE_NOTE}</p>
              </Disclosure>
            </>
          )}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

/**
 * The three pots, named and separated. The other two link out rather than
 * showing a figure here: a number for real capital on this page would be the
 * first step toward a total.
 */
function ThreePots(): ReactNode {
  return (
    <Panel
      title="Three kinds of value"
      description="Shown separately because they are not the same thing and cannot be added."
    >
      <FieldGrid columns={3}>
        <Field label="Nodal Economy" note="Credits, native assets and what you have earned inside Nodal.">
          <Pill tone="info">This page</Pill>
        </Field>
        <Field label="Simulated Capital" note="A record of what would have happened. No capital moved.">
          <LinkButton to="/lab">Open the Lab</LinkButton>
        </Field>
        <Field label="Real Capital" note="Money and settlement assets held for this account.">
          <LinkButton to="/portfolio">Open the portfolio</LinkButton>
        </Field>
      </FieldGrid>
      <Disclosure title="Why these are never added together">
        <p>{THREE_POTS_NOTE}</p>
      </Disclosure>
    </Panel>
  );
}

function Origins(props: { readonly balance: CreditBalance }): ReactNode {
  const byOrigin = breakdownRows(
    props.balance.by_origin as Record<string, string> | undefined,
    ORIGIN_LABELS,
  );
  const byFinality = breakdownRows(
    props.balance.by_finality as Record<string, string> | undefined,
    FINALITY_LABELS,
  );

  if (byOrigin.length === 0 && byFinality.length === 0) {
    return null;
  }

  return (
    <Panel
      title="Where these Credits came from"
      description="Payout eligibility is decided per origin, not on the total, which is why this breakdown exists."
    >
      {byOrigin.length > 0 && (
        <Table caption="Credits by origin" headers={["Origin", "Amount"]}>
          {byOrigin.map(([name, amount]) => (
            <tr key={name}>
              <td>{name}</td>
              <td>
                <Qty value={amount} decimals={CREDIT_DECIMALS} symbol="Credits" />
              </td>
            </tr>
          ))}
        </Table>
      )}
      {byFinality.length > 0 && (
        <Table caption="Credits by the state of the money behind them" headers={["State", "Amount"]}>
          {byFinality.map(([name, amount]) => (
            <tr key={name}>
              <td>{name}</td>
              <td>
                <Qty value={amount} decimals={CREDIT_DECIMALS} symbol="Credits" />
              </td>
            </tr>
          ))}
        </Table>
      )}
    </Panel>
  );
}
