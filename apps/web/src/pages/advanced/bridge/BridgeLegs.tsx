/**
 * The account's bridge legs, newest first, and one leg with its progress
 * through the state machine, the lock and every release or refund attempt as
 * the simulated chains report them now, and every state it passed through. A
 * leg read by its id is answered only to someone who may read its account;
 * anyone else gets NOT_FOUND, which the page shows with its code.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useBridgeLeg,
  useBridgeLegs,
  type BridgeLeg,
  type BridgeLegDetail,
} from "../../../api/advanced/bridge.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Qty } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { legStatusTone } from "../../../lib/bridge.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { ChainTxLine, LegFields, LegOutcomePill, LegProgress, BridgeNav } from "./parts.tsx";
import { bridgeHref, legHref } from "./paths.ts";

export function LegsPage(): ReactNode {
  const account = useAdvancedAccount();
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const page = useBridgeLegs(account.accountId, trail.current);
  return (
    <Page title="Bridge legs" lead="Every movement of the account's units between the simulated chains, newest first.">
      <SimulatedNotice />
      <BridgeNav />
      <Panel title="Legs" description="Each with its state and outcome as the bridge service recorded them. A leg with a submission whose outcome is unknown is shown as unknown.">
        {account.accountId === undefined ? (
          <NoAccount />
        ) : (
          <AsyncPanel
            query={page}
            loadingLabel="Loading the bridge legs…"
            empty={{
              isEmpty: (data: CursorPage<BridgeLeg>) => data.items.length === 0 && trail.previous.length === 0,
              title: "No bridge legs",
              body: "The backend reports no bridge leg for this account.",
            }}
          >
            {(data: CursorPage<BridgeLeg>) => (
              <>
                <ScrollTable caption="Bridge legs" headers={["Leg", "Outcome", "State", "Units", "From", "To", "Recorded"]}>
                  {data.items.map((leg) => (
                    <tr key={leg.id} data-leg={leg.id}>
                      <th scope="row">
                        <Link to={legHref(leg.id)}>{leg.source_symbol}</Link>
                        <span className="cell-note mono-small">{leg.id}</span>
                      </th>
                      <td>
                        <LegOutcomePill leg={leg} />
                      </td>
                      <td>
                        <Pill tone={legStatusTone(leg.status)}>{leg.status}</Pill>
                        {leg.failure_code !== undefined && <span className="cell-note mono-small">{leg.failure_code}</span>}
                      </td>
                      <td>
                        <Qty value={leg.quantity} decimals={leg.decimals} symbol={leg.source_symbol} />
                      </td>
                      <td>
                        <code className="mono-small">{leg.source_chain}</code>
                      </td>
                      <td>
                        <code className="mono-small">{leg.destination_chain}</code> {leg.destination_symbol}
                      </td>
                      <td>{formatInstant(leg.created_at)}</td>
                    </tr>
                  ))}
                </ScrollTable>
                <CursorPager
                  noun="legs"
                  trail={trail}
                  nextCursor={data.nextCursor}
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
        )}
      </Panel>
    </Page>
  );
}

export function LegDetailPage(): ReactNode {
  const { legId } = useParams();
  const account = useAdvancedAccount();
  const leg = useBridgeLeg(account.accountId, legId);
  return (
    <Page
      title="Bridge leg"
      lead="One movement of units between the simulated chains: where it stands, its transactions on both chains, and every state it passed through."
      actions={<LinkButton to={bridgeHref("legs")}>All legs</LinkButton>}
    >
      <SimulatedNotice />
      <BridgeNav />
      <AsyncPanel query={leg} loadingLabel="Loading the leg…">
        {(data: BridgeLegDetail) => <LegDetailBody detail={data} />}
      </AsyncPanel>
    </Page>
  );
}

function LegDetailBody(props: { readonly detail: BridgeLegDetail }): ReactNode {
  const { detail } = props;
  const visited = detail.transitions.map((t) => t.to_status);
  return (
    <>
      <Panel title="Progress" description="The states a leg takes when nothing goes wrong, and which this one passed through, as its own transitions record.">
        <LegProgress status={detail.leg.status} visited={visited} />
      </Panel>
      <Panel title="The leg">
        <LegFields leg={detail.leg} />
      </Panel>
      <Panel
        title="On the chains"
        description="What each simulated chain says of the leg's transactions at the instant of this read. A transaction is never resubmitted; an unknown outcome is read back by its id."
      >
        <h3>Lock on the source chain</h3>
        {detail.lock === undefined ? (
          <p className="absent">The lock has not been signed yet.</p>
        ) : (
          <p data-field="lock">
            <ChainTxLine tx={detail.lock} />
          </p>
        )}
        <h3>Release and refund attempts</h3>
        {detail.attempts.length === 0 ? (
          <p className="absent">No release or refund has been relayed for this leg.</p>
        ) : (
          <ScrollTable caption="Release and refund attempts" headers={["Attempt", "Kind", "On the chain", "Recorded"]}>
            {detail.attempts.map((attempt) => (
              <tr key={`${attempt.kind}-${String(attempt.number)}`}>
                <th scope="row">{String(attempt.number)}</th>
                <td>
                  <code className="mono-small">{attempt.kind}</code>
                </td>
                <td>
                  <ChainTxLine tx={attempt.on_chain} />
                  <span className="cell-note">blockhash height {String(attempt.blockhash_height)}</span>
                </td>
                <td>{formatInstant(attempt.created_at)}</td>
              </tr>
            ))}
          </ScrollTable>
        )}
      </Panel>
      <Panel title="Every state it passed through" description="The leg's bound transitions, oldest first, with who made each one and why.">
        <ScrollTable caption="Leg transitions" headers={["From", "To", "By", "Reason", "When"]}>
          {detail.transitions.map((t, index) => (
            <tr key={`${t.to_status}-${String(index)}`}>
              <th scope="row">
                <code className="mono-small">{t.from_status}</code>
              </th>
              <td>
                <Pill tone={legStatusTone(t.to_status)}>{t.to_status}</Pill>
              </td>
              <td>
                {t.actor_type} <code className="mono-small">{t.actor_id}</code>
              </td>
              <td className="cell-prose">{t.reason}</td>
              <td>{formatInstant(t.occurred_at)}</td>
            </tr>
          ))}
        </ScrollTable>
      </Panel>
    </>
  );
}
