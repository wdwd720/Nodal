/**
 * HOME (PART 111): portfolio value, buying power, available now, reserved,
 * pending, P&L, underlying assets, agent status.
 *
 * Every figure here is one the backend computed and stamped with an `as of`.
 * Two things are deliberately *not* computed in the browser:
 *
 *   - buying power, which the backend recomputes on every call and which this
 *     page therefore refetches rather than deriving from balances;
 *   - a portfolio-level P&L total, which no v1 endpoint returns. Summing the
 *     per-holding figures here would be the client re-creating financial math
 *     the backend owns (PART 110), so the page shows the components the API
 *     does return and says plainly that the total is not one of them.
 */
import type { ReactNode } from "react";

import {
  useBuyingPower,
  useHoldings,
  useIntents,
  type BuyingPower,
  type HoldingsResponse,
} from "../api/queries.ts";
import { AsyncPanel, EmptyState } from "../components/DataState.tsx";
import { LinkButton } from "../components/Button.tsx";
import {
  AsOf,
  Disclosure,
  Field,
  FieldGrid,
  NoEndpoint,
  Page,
  Panel,
  Pill,
  Table,
} from "../components/Layout.tsx";
import { Qty, Usd } from "../components/Money.tsx";
import {
  PENDING_SETTLEMENT_NOTE,
  THREE_POTS_NOTE,
  USDC_DISCLOSURE,
  USD_VALUATION_NOTE,
} from "../lib/honesty.ts";
import { useActiveAccountId, useSession } from "../session.tsx";

export function Home(): ReactNode {
  const session = useSession();
  const accountId = useActiveAccountId();
  const buyingPower = useBuyingPower(accountId, "DISPLAY");
  const holdings = useHoldings(accountId);
  const intents = useIntents(accountId);

  if (accountId === undefined) {
    return (
      <Page title="Home">
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is no balance to show. That is the backend's answer, not a loading state."
        />
      </Page>
    );
  }

  const agentIntents = (intents.data ?? []).filter((intent) => intent.actor_type === "AGENT");

  return (
    <Page
      title="Home"
      lead="Everything on this page was computed by the backend and is shown with the moment it was computed."
      actions={<LinkButton to="/add-funds" variant="primary">Add funds</LinkButton>}
    >
      <Panel
        title="Three kinds of value"
        description="Nodal keeps three separate pots. This page shows real capital; the other two live on their own pages, and none of the three is ever added to another."
      >
        <FieldGrid columns={3}>
          <Field
            label="Real Capital"
            note="Money and settlement assets held for this account. The figures below are these."
          >
            <Pill tone="info">This page</Pill>
          </Field>
          <Field label="Nodal Economy" note="Credits, what you have earned inside Nodal, and what you can buy with them.">
            <LinkButton to="/nodal-economy">Open</LinkButton>
          </Field>
          <Field label="Simulated Capital" note="A record of what would have happened. No capital moved.">
            <LinkButton to="/lab">Open the Lab</LinkButton>
          </Field>
        </FieldGrid>
        <Disclosure title="Why these are never added together">
          <p>{THREE_POTS_NOTE}</p>
        </Disclosure>
      </Panel>

      <Panel
        title="Balances"
        description="Real capital, recomputed by the backend on every request. Never cached as truth."
      >
        <AsyncPanel
          query={buyingPower}
          loadingLabel="Asking the backend to compute buying power…"
        >
          {(data: BuyingPower) => <Balances data={data} />}
        </AsyncPanel>
      </Panel>

      <Panel
        title="Underlying assets"
        description="What the USD figures above are a valuation of."
      >
        <AsyncPanel
          query={buyingPower}
          loadingLabel="Loading underlying balances…"
          empty={{
            isEmpty: (data: BuyingPower) => data.underlying_balances.length === 0,
            title: "No underlying balances",
            body: "The backend reported no asset balances for this account. Nothing is being shown in their place.",
          }}
        >
          {(data: BuyingPower) => (
            <Table
              caption="Underlying asset balances"
              headers={["Asset", "Quantity", "USD valuation", "Price reference", "Status"]}
            >
              {data.underlying_balances.map((balance) => (
                <tr key={balance.asset}>
                  <th scope="row">
                    {balance.symbol}
                    <div className="mono-small">{balance.asset}</div>
                  </th>
                  <td>
                    <Qty value={balance.quantity} decimals={balance.decimals} symbol={balance.symbol} />
                  </td>
                  <td>
                    <Usd value={balance.usd_value} />
                  </td>
                  <td className="mono-small">{balance.price_ref}</td>
                  <td>
                    <Pill tone={balance.status === "NORMAL" ? "good" : "warn"}>{balance.status}</Pill>
                  </td>
                </tr>
              ))}
            </Table>
          )}
        </AsyncPanel>
        <Disclosure title="What you are actually holding">
          <p>{USDC_DISCLOSURE}</p>
          <p>{USD_VALUATION_NOTE}</p>
        </Disclosure>
      </Panel>

      <Panel
        title="Profit and loss"
        description="Per holding, exactly as the backend reports it."
      >
        <AsyncPanel
          query={holdings}
          loadingLabel="Loading holdings…"
          empty={{
            isEmpty: (data: HoldingsResponse) => data.holdings.length === 0,
            title: "No holdings",
            body: "The backend returned no holdings for this account, so there is no profit or loss to report.",
          }}
        >
          {(data: HoldingsResponse) => (
            <>
              <Table
                caption="Profit and loss by holding"
                headers={["Asset", "USD mark", "Cost basis", "Unrealized", "Realized"]}
              >
                {data.holdings.map((holding) => (
                  <tr key={holding.asset}>
                    <th scope="row">{holding.symbol}</th>
                    <td>
                      <Usd value={holding.usd_mark} />
                    </td>
                    <td>
                      <Usd value={holding.cost_basis_usd} />
                    </td>
                    <td>
                      <Usd value={holding.unrealized_pnl_usd} signed />
                    </td>
                    <td>
                      <Usd value={holding.realized_pnl_usd} absent="not reported for this holding" />
                    </td>
                  </tr>
                ))}
              </Table>
              <AsOf at={data.as_of} />
              <p className="note">
                The v1 API returns profit and loss per holding and exposes no account-level total.
                Adding these up here would be the browser re-deriving a financial figure the backend
                owns, so the components are shown and the total is not invented.
              </p>
            </>
          )}
        </AsyncPanel>
      </Panel>

      <Panel title="Agent status" description="What the v1 API can say about autonomous activity.">
        <NoEndpoint
          what="There is no agent registry, capital-envelope or agent-state endpoint in the v1 contract."
          detail="What can be said truthfully is what the trade-intent endpoint reports: how many intents on this account were created by an agent rather than by you."
        />
        <FieldGrid columns={2}>
          <Field
            label="Intents created by an agent"
            note="Counted from the intents the backend returned for this account, where actor_type is AGENT."
          >
            {intents.isPending ? "loading…" : String(agentIntents.length)}
          </Field>
          <Field label="Intents created by you" note="Same source, where actor_type is USER.">
            {intents.isPending
              ? "loading…"
              : String((intents.data ?? []).filter((i) => i.actor_type === "USER").length)}
          </Field>
        </FieldGrid>
      </Panel>

      <Panel title="Account" description="The account these figures belong to.">
        <FieldGrid columns={3}>
          <Field label="Account">
            <code className="mono-small">{accountId}</code>
          </Field>
          <Field label="Kind">{session.accounts.find((a) => a.id === accountId)?.kind ?? "unknown"}</Field>
          <Field label="Status">
            {(() => {
              const account = session.accounts.find((a) => a.id === accountId);
              if (account === undefined) return "unknown";
              return (
                <Pill tone={account.status === "ACTIVE" ? "good" : "warn"}>{account.status}</Pill>
              );
            })()}
          </Field>
        </FieldGrid>
      </Panel>
    </Page>
  );
}

function Balances(props: { readonly data: BuyingPower }): ReactNode {
  const { data } = props;
  return (
    <>
      <FieldGrid columns={3}>
        <Field
          label="Portfolio value"
          emphasis
          note="Total valuation of the underlying assets, computed by the backend."
        >
          <Usd value={data.portfolio_value} />
        </Field>
        <Field
          label="Buying power"
          emphasis
          note="What the backend will let you commit right now, after haircuts and restrictions."
        >
          <Usd value={data.buying_power} />
        </Field>
        <Field label="Available now" note="Settled and immediately usable.">
          <Usd value={data.available_now} />
        </Field>
        <Field label="Reserved" note="Committed to intents or orders that have not finished.">
          <Usd value={data.reserved} />
        </Field>
        <Field label="Pending" note="Deposited but not yet settled and available.">
          <Usd value={data.pending} />
        </Field>
        <Field
          label="Withdrawable"
          note="Subject to its own restrictions, which are listed below when they apply."
        >
          <Usd value={data.withdrawable} />
        </Field>
      </FieldGrid>

      <AsOf at={data.as_of} />
      <p className="mono-small">policy {data.policy_version}</p>

      {data.haircuts.length > 0 && (
        <>
          <h3>Haircuts applied</h3>
          <Table caption="Haircuts" headers={["Asset", "Factor", "Reason"]}>
            {data.haircuts.map((haircut) => (
              <tr key={`${haircut.asset}-${haircut.reason}`}>
                <th scope="row" className="mono-small">
                  {haircut.asset}
                </th>
                <td>{String(haircut.factor_bps)} bps</td>
                <td>{haircut.reason}</td>
              </tr>
            ))}
          </Table>
        </>
      )}

      {data.restrictions.length > 0 ? (
        <>
          <h3>Restrictions the backend is applying</h3>
          <ul className="restrictions">
            {data.restrictions.map((restriction) => (
              <li key={`${restriction.code}-${restriction.scope}`}>
                <Pill tone={restriction.blocking ? "bad" : "warn"}>
                  {restriction.blocking ? "blocking" : "advisory"}
                </Pill>{" "}
                <span className="mono-small">{restriction.code}</span> · {restriction.scope} ·{" "}
                {restriction.detail}
              </li>
            ))}
          </ul>
        </>
      ) : (
        <p className="note">The backend reported no restrictions on this account at this instant.</p>
      )}

      <Disclosure title="Pending settlement">
        <p>{PENDING_SETTLEMENT_NOTE}</p>
      </Disclosure>
    </>
  );
}
