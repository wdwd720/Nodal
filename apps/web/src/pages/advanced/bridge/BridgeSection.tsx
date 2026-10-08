/**
 * CROSS-CHAIN CAPITAL (internal/bridge) over the simulated chains sbx-solana
 * and sbx-ethereum (internal/devnethost).
 *
 * A route joins a simulated asset on one simulated chain to its bridged twin
 * on another. A leg moves an account's units through a bound state machine:
 * the holder's custodied key locks them in the source chain's escrow, the lock
 * finalizes, the attester signs a release, the release finalizes on the
 * destination and the leg is reconciled against both chains — each posting
 * made only once the chain transaction it records is final. Value between
 * chains counts toward buying power only at the haircut policy's factor.
 *
 * Every figure is the backend's, in base units or exact cent strings, and
 * simulated; the chains are a simulation the books are reconciled against.
 * Every route answers UNSUPPORTED where the deployment has no sandbox chain;
 * the pages show that refusal with its code.
 */
import type { ReactNode } from "react";
import { Link, Route, Routes } from "react-router-dom";

import { useBridgeHoldings, type BridgeHoldings } from "../../../api/advanced/bridge.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { AsOf, Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Bps, Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { HOLDING_MEANING, legStatusTone } from "../../../lib/bridge.ts";
import { useAdvancedAccount } from "../area.ts";
import { AdvancedNotFound, NoAccount, ScrollTable } from "../shared.tsx";
import { BridgeChainsPage } from "./BridgeChains.tsx";
import { LegDetailPage, LegsPage } from "./BridgeLegs.tsx";
import { BridgeOperatorPage } from "./BridgeOperator.tsx";
import { BridgeRoutesPage } from "./BridgeRoutes.tsx";
import { BridgeNav } from "./parts.tsx";
import { legHref } from "./paths.ts";

/** The section: holdings by chain, routes and transfers, legs, the chains, and the operator's page. */
export function BridgeSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<BridgeHoldingsPage />} />
      <Route path="routes" element={<BridgeRoutesPage />} />
      <Route path="legs" element={<LegsPage />} />
      <Route path="legs/:legId" element={<LegDetailPage />} />
      <Route path="chains" element={<BridgeChainsPage />} />
      <Route path="operator" element={<BridgeOperatorPage />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}

function BridgeHoldingsPage(): ReactNode {
  const account = useAdvancedAccount();
  const holdings = useBridgeHoldings(account.accountId);
  return (
    <Page
      title="Bridge"
      lead="The account's simulated holdings on each chain, the value moving between chains, and how much of that value counts while it is not final."
    >
      <SimulatedNotice />
      <BridgeNav />
      {account.accountId === undefined ? (
        <Panel title="Holdings by chain">
          <NoAccount />
        </Panel>
      ) : (
        <AsyncPanel query={holdings} loadingLabel="Loading the account's holdings by chain…">
          {(data: BridgeHoldings) => <HoldingsBody data={data} />}
        </AsyncPanel>
      )}
    </Page>
  );
}

function HoldingsBody(props: { readonly data: BridgeHoldings }): ReactNode {
  const { data } = props;
  return (
    <>
      <Panel
        title="Holdings by chain"
        description="The account's WALLET balances of simulated assets on the books, grouped by the chain each asset lives on. The same simulated dollar on two chains is two assets."
      >
        <AsOf at={data.as_of} />
        <p className="meta-row">
          <span>
            Account <Identifier value={data.account_id} />
          </span>
        </p>
        {data.chains.length === 0 ? (
          <p className="absent">The account holds no simulated asset on any chain.</p>
        ) : (
          data.chains.map((group) => (
            <section key={group.chain} aria-label={`Holdings on ${group.chain}`} data-chain={group.chain}>
              <h3>
                <code className="mono-small">{group.chain}</code>
              </h3>
              <ScrollTable caption={`Holdings on ${group.chain}`} headers={["Asset", "On the books"]}>
                {group.holdings.map((holding) => (
                  <tr key={holding.asset_id} data-asset={holding.asset_id}>
                    <th scope="row">
                      {holding.symbol}
                      <span className="cell-note mono-small">{holding.asset_id}</span>
                    </th>
                    <td>
                      <Qty value={holding.quantity} decimals={holding.decimals} symbol={holding.symbol} />
                      <span className="cell-note">
                        <BaseUnits value={holding.quantity} />
                      </span>
                    </td>
                  </tr>
                ))}
              </ScrollTable>
            </section>
          ))
        )}
      </Panel>
      <Panel
        title="Value between chains"
        description="Each leg's value is counted in exactly one place, the ledger account that holds it, and value in flight counts toward buying power only at the haircut policy's factor."
      >
        <FieldGrid columns={3}>
          <Field label="Haircut policy" note="The deployment's policy for value that is not final.">
            <code className="mono-small">{data.policy.version}</code>
          </Field>
          <Field label="Factor while in flight" note="Lock final on the source, release not yet observed on the destination.">
            <Bps value={data.policy.in_flight_bps} />
          </Field>
          <Field label="Factor once a release is observed" note="Release confirmed, not final, on the destination.">
            <Bps value={data.policy.observed_bps} />
          </Field>
          <Field
            label="Pending locks"
            note="Units still in a source wallet under a reservation whose lock is not final, at the route's unit price. Already spoken for; counted once, on the wallet line."
          >
            <span data-field="pending_locks_usd">
              <Usd value={data.pending_locks_usd} />
            </span>{" "}
            <span className="mono-small">simulated</span>
          </Field>
        </FieldGrid>
        <h3>In flight</h3>
        {data.in_flight.length === 0 ? (
          <p className="absent">No value of this account is in flight between chains.</p>
        ) : (
          <ScrollTable caption="Value in flight" headers={["Asset", "Quantity", "Marked at the route's unit price", "Counts at", "Why"]}>
            {data.in_flight.map((line, index) => (
              <tr key={`${line.asset_id}-${String(line.factor_bps)}-${String(index)}`}>
                <th scope="row">
                  {line.symbol}
                  <span className="cell-note mono-small">{line.asset_id}</span>
                </th>
                <td>
                  <Qty value={line.quantity} decimals={line.decimals} symbol={line.symbol} />
                </td>
                <td>
                  <Usd value={line.marked_usd} /> <span className="mono-small">simulated</span>
                </td>
                <td>
                  <Bps value={line.factor_bps} />
                </td>
                <td className="cell-prose">{line.reason}</td>
              </tr>
            ))}
          </ScrollTable>
        )}
        <h3>Open legs</h3>
        {data.legs.length === 0 ? (
          <p className="absent">The account has no open leg.</p>
        ) : (
          <ScrollTable caption="Open legs" headers={["Leg", "State", "Where the value is held", "Quantity", "Counts at", "Why"]}>
            {data.legs.map((leg) => (
              <tr key={leg.leg_id} data-leg={leg.leg_id}>
                <th scope="row">
                  <Link to={legHref(leg.leg_id)}>{leg.symbol}</Link>
                  <span className="cell-note mono-small">{leg.leg_id}</span>
                </th>
                <td>
                  <Pill tone={legStatusTone(leg.status)}>{leg.status}</Pill>
                </td>
                <td>
                  <code className="mono-small">{leg.holding}</code>
                  <span className="cell-note">{HOLDING_MEANING[leg.holding]}</span>
                </td>
                <td>
                  <Qty value={leg.quantity} decimals={leg.decimals} symbol={leg.symbol} />
                </td>
                <td>
                  <Bps value={leg.factor_bps} />
                </td>
                <td className="cell-prose">{leg.reason}</td>
              </tr>
            ))}
          </ScrollTable>
        )}
      </Panel>
    </>
  );
}
