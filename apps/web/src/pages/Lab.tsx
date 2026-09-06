/**
 * LAB (PART 111): historical replay, shadow, comparison, counterfactuals.
 *
 * The lab is where a strategy is examined without money at stake, so it is also
 * the screen where a dishonest interface does the most damage. Two rules hold
 * throughout:
 *
 *   - nothing on this page is described as a record of what this account
 *     earned. Every figure carries the mode it came from, and the modes that
 *     commit no capital are labelled as such wherever they appear;
 *   - the comparison counts records. It does not compute returns, hit rates or
 *     any other summary statistic, because the backend computes performance and
 *     the browser does not (PART 110) — and because a summary invented here
 *     would be exactly the kind of number a reader would take for a result.
 */
import { useMemo, type ReactNode } from "react";

import { useIntents, useOrders, type Order, type TradeIntent } from "../api/queries.ts";
import { AsyncPanel, EmptyState } from "../components/DataState.tsx";
import { LinkButton } from "../components/Button.tsx";
import {
  Disclosure,
  Identifier,
  NoEndpoint,
  Page,
  Panel,
  Pill,
  Table,
  type Tone,
} from "../components/Layout.tsx";
import { Usd } from "../components/Money.tsx";
import {
  MODE_DESCRIPTIONS,
  SIMULATED_RESULTS_NOTICE,
  modeBadge,
  modeUsesRealCapital,
} from "../lib/honesty.ts";
import { formatInstant } from "../lib/time.ts";
import { useActiveAccountId } from "../session.tsx";

const SIMULATED_MODES = ["BACKTEST", "PAPER", "SHADOW"] as const;

function modeTone(mode: string): Tone {
  return modeUsesRealCapital(mode) ? "warn" : "neutral";
}

interface ModeGroup {
  readonly mode: string;
  readonly intents: readonly TradeIntent[];
  readonly statuses: ReadonlyArray<readonly [string, number]>;
}

export function Lab(): ReactNode {
  const accountId = useActiveAccountId();
  const intents = useIntents(accountId);
  const orders = useOrders(accountId);

  const groups = useMemo<ModeGroup[]>(() => {
    const byMode = new Map<string, TradeIntent[]>();
    for (const intent of intents.data ?? []) {
      const list = byMode.get(intent.mode) ?? [];
      list.push(intent);
      byMode.set(intent.mode, list);
    }
    return [...byMode.entries()]
      .map(([mode, list]) => {
        const counts = new Map<string, number>();
        for (const intent of list) {
          counts.set(intent.status, (counts.get(intent.status) ?? 0) + 1);
        }
        return { mode, intents: list, statuses: [...counts.entries()] };
      })
      .sort((a, b) => (a.mode < b.mode ? -1 : 1));
  }, [intents.data]);

  if (accountId === undefined) {
    return (
      <Page title="Lab">
        <EmptyState title="No account" body="The backend returned no accounts for this session." />
      </Page>
    );
  }

  const simulated = groups.filter((group) => !modeUsesRealCapital(group.mode));
  const real = groups.filter((group) => modeUsesRealCapital(group.mode));

  return (
    <Page
      title="Lab"
      lead="Examining a strategy without money at stake. Nothing on this page is a record of what this account earned."
      actions={<LinkButton to="/strategy">Open the strategy builder</LinkButton>}
    >
      <p className="banner banner-warn" role="note">
        {SIMULATED_RESULTS_NOTICE}
      </p>

      <Panel
        title="Comparison"
        description="Records the backend holds, grouped by the mode that produced them."
      >
        <AsyncPanel
          query={intents}
          loadingLabel="Loading intents…"
          empty={{
            isEmpty: () => groups.length === 0,
            title: "Nothing to compare",
            body: "The backend returned no trade intents for this account in any mode, so there is nothing to compare. An empty comparison is not a flat line; it is an absence of records.",
          }}
        >
          {() => (
            <>
              <Table
                caption="Intents by mode"
                headers={["Mode", "Commits real capital", "Intents recorded", "Outcomes", "What the mode means"]}
              >
                {groups.map((group) => (
                  <tr key={group.mode}>
                    <th scope="row">
                      <Pill tone={modeTone(group.mode)}>{modeBadge(group.mode)}</Pill>
                    </th>
                    <td>{modeUsesRealCapital(group.mode) ? "yes" : "no"}</td>
                    <td className="num">{String(group.intents.length)}</td>
                    <td>
                      {group.statuses.map(([status, count]) => (
                        <span key={status} className="tally">
                          {status} <span className="num">{String(count)}</span>
                        </span>
                      ))}
                    </td>
                    <td className="cell-prose">
                      {MODE_DESCRIPTIONS[group.mode] ??
                        "The backend reported a mode this app has no description for; it is shown verbatim."}
                    </td>
                  </tr>
                ))}
              </Table>
              <p className="note">
                These are counts of records, not performance. The API returns no realised outcome per
                intent and no aggregate for a mode, so there is nothing here to turn into a return,
                and this page does not invent one.
              </p>
              {simulated.length > 0 && real.length > 0 && (
                <p className="note">
                  Both simulated and real-capital records exist on this account. They are kept in
                  separate rows above and are never added together.
                </p>
              )}
            </>
          )}
        </AsyncPanel>
      </Panel>

      <Panel title="Shadow" description="Decisions made alongside real activity but never submitted.">
        <ShadowOrPaper
          groups={groups.filter((group) => group.mode === "SHADOW")}
          emptyTitle="No shadow runs recorded"
          emptyBody="No intent on this account was created in SHADOW mode. Shadow decisions are made next to real activity and never submitted, so an absence here means none were recorded, not that none would have been profitable."
        />
      </Panel>

      <Panel title="Simulated runs" description="Backtest and paper records this account holds.">
        <ShadowOrPaper
          groups={groups.filter((group) =>
            (SIMULATED_MODES as readonly string[]).includes(group.mode) && group.mode !== "SHADOW",
          )}
          emptyTitle="No backtest or paper runs recorded"
          emptyBody="The backend returned no intents in BACKTEST or PAPER mode for this account."
        />
      </Panel>

      <Panel title="Orders by mode" description="What each mode produced downstream.">
        <AsyncPanel
          query={orders}
          loadingLabel="Loading orders…"
          empty={{
            isEmpty: (list: Order[]) => list.length === 0,
            title: "No orders",
            body: "The backend returned no orders for this account in any mode.",
          }}
        >
          {(list: Order[]) => (
            <Table
              caption="Orders by mode"
              headers={["Order", "Mode", "Status", "Side", "Created"]}
            >
              {list.map((order) => (
                <tr key={order.id}>
                  <th scope="row">
                    <Identifier value={order.id} />
                  </th>
                  <td>
                    <Pill tone={modeTone(order.mode)}>{modeBadge(order.mode)}</Pill>
                  </td>
                  <td>
                    <Pill tone="info">{order.status}</Pill>
                  </td>
                  <td>{order.side}</td>
                  <td>{formatInstant(order.created_at)}</td>
                </tr>
              ))}
            </Table>
          )}
        </AsyncPanel>
      </Panel>

      <Panel title="Historical replay" description="Running a strategy against recorded history.">
        <NoEndpoint
          what="v1 exposes no replay, backtest-run or point-in-time dataset endpoint."
          detail="A replay has to reconstruct exactly what was knowable at each instant, which is a server capability resting on point-in-time data the browser cannot see. Until the API exposes it, this app has nothing real to run and will not fake one."
        />
      </Panel>

      <Panel title="Counterfactuals" description="What would have happened had the decision differed.">
        <NoEndpoint
          what="v1 exposes no counterfactual endpoint."
          detail="A counterfactual is a claim about a world that did not happen; producing one in the browser from the rows on this page would be fabrication with a chart around it. When the backend can answer the question, this panel will ask it."
        />
      </Panel>

      <Disclosure title="How to read anything on this page">
        <p>{SIMULATED_RESULTS_NOTICE}</p>
        <p>
          A simulated result did not pay any real fee, did not move a real price, and did not compete
          with anyone for the same fill. Those three differences are usually larger than whatever a
          simulation shows.
        </p>
      </Disclosure>
    </Page>
  );
}

function ShadowOrPaper(props: {
  readonly groups: readonly ModeGroup[];
  readonly emptyTitle: string;
  readonly emptyBody: string;
}): ReactNode {
  const rows = props.groups.flatMap((group) => group.intents);
  if (rows.length === 0) {
    return <EmptyState title={props.emptyTitle} body={props.emptyBody} />;
  }
  return (
    <Table
      caption="Simulated intents"
      headers={["Intent", "Mode", "Status", "Action", "Notional", "Received"]}
    >
      {rows.map((intent) => (
        <tr key={intent.id}>
          <th scope="row">
            <Identifier value={intent.id} />
          </th>
          <td>
            <Pill tone={modeTone(intent.mode)}>{modeBadge(intent.mode)}</Pill>
          </td>
          <td>{intent.status}</td>
          <td>{intent.action}</td>
          <td>
            <Usd value={intent.notional_usd} absent="not set on this intent" />
          </td>
          <td>{formatInstant(intent.received_at)}</td>
        </tr>
      ))}
    </Table>
  );
}
