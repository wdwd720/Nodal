/**
 * ACTIVITY (PART 111): the lifecycle timeline —
 * data event → prediction → intent → eligibility → risk → execution → fill →
 * reconciliation.
 *
 * The ladder is drawn in full for every correlated chain, including the stages
 * that have no row. A stage the backend never recorded is shown as "not
 * recorded", never skipped: an intent that reached execution without a risk
 * decision on file is a fact worth seeing, and a timeline that silently omits
 * empty stages hides exactly that.
 */
import { useMemo, useState, type ReactNode } from "react";

import { useActivity, type ActivityItem, type ActivityPage } from "../api/queries.ts";
import { AsyncPanel, EmptyState } from "../components/DataState.tsx";
import { Button, DownloadLink } from "../components/Button.tsx";
import { Identifier, Page, Panel, Pill, Table, type Tone } from "../components/Layout.tsx";
import { AUXILIARY_ACTIVITY_KINDS, LIFECYCLE_STAGES } from "../lib/honesty.ts";
import { formatInstant } from "../lib/time.ts";
import { useActiveAccountId } from "../session.tsx";
import { API_BASE } from "../api/client.ts";

interface Chain {
  readonly key: string;
  readonly correlationId: string | undefined;
  readonly items: readonly ActivityItem[];
  readonly startedAt: string;
}

function kindTone(kind: string): Tone {
  switch (kind) {
    case "FILL":
    case "RECONCILIATION":
      return "good";
    case "RISK":
    case "ELIGIBILITY":
      return "info";
    case "SECURITY":
      return "warn";
    default:
      return "neutral";
  }
}

export function Activity(): ReactNode {
  const accountId = useActiveAccountId();
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [history, setHistory] = useState<readonly string[]>([]);
  const activity = useActivity(accountId, cursor);

  const chains = useMemo<Chain[]>(() => {
    const items = activity.data?.items ?? [];
    const byCorrelation = new Map<string, ActivityItem[]>();
    for (const item of items) {
      const key = item.correlation_id ?? `standalone:${item.id}`;
      const list = byCorrelation.get(key) ?? [];
      list.push(item);
      byCorrelation.set(key, list);
    }
    return [...byCorrelation.entries()]
      .map(([key, list]) => {
        const ordered = [...list].sort((a, b) => (a.occurred_at < b.occurred_at ? -1 : 1));
        return {
          key,
          correlationId: ordered[0]?.correlation_id,
          items: ordered,
          startedAt: ordered[0]?.occurred_at ?? "",
        };
      })
      .sort((a, b) => (a.startedAt > b.startedAt ? -1 : 1));
  }, [activity.data]);

  if (accountId === undefined) {
    return (
      <Page title="Activity">
        <EmptyState title="No account" body="The backend returned no accounts for this session." />
      </Page>
    );
  }

  return (
    <Page
      title="Activity"
      lead="Every recorded step, in the order the platform records them, grouped by the correlation id that ties one chain of causation together."
      actions={
        <DownloadLink
          href={`${API_BASE}/accounts/${accountId}/export?format=json`}
          fileName={`nodal-activity-${accountId}.json`}
        >
          Export as JSON
        </DownloadLink>
      }
    >
      <Panel title="The lifecycle" description="What each stage means, whether or not it appears below.">
        <Table caption="Lifecycle stages" headers={["Stage", "What it records"]}>
          {LIFECYCLE_STAGES.map((stage) => (
            <tr key={stage.kind}>
              <th scope="row">
                <Pill tone={kindTone(stage.kind)}>{stage.label}</Pill>
              </th>
              <td className="cell-prose">{stage.description}</td>
            </tr>
          ))}
        </Table>
      </Panel>

      <Panel title="Timeline" description="Newest chain first.">
        <AsyncPanel
          query={activity}
          loadingLabel="Loading the activity timeline…"
          empty={{
            isEmpty: (page: ActivityPage) => page.items.length === 0,
            title: "No activity recorded",
            body: "The backend returned no activity for this account on this page. That is an empty history, not an error.",
          }}
        >
          {(page: ActivityPage) => (
            <>
              {chains.map((chain) => (
                <ChainView key={chain.key} chain={chain} />
              ))}

              <div className="form-actions">
                {history.length > 0 ? (
                  <Button
                    variant="secondary"
                    onClick={() => {
                      const previous = history.at(-1);
                      setHistory(history.slice(0, history.length - 1));
                      setCursor(previous === "" ? undefined : previous);
                    }}
                  >
                    Previous page
                  </Button>
                ) : (
                  <Button variant="secondary" disabledReason="This is the first page.">
                    Previous page
                  </Button>
                )}
                {page.nextCursor !== null ? (
                  <Button
                    variant="secondary"
                    onClick={() => {
                      setHistory([...history, cursor ?? ""]);
                      setCursor(page.nextCursor ?? undefined);
                    }}
                  >
                    Next page
                  </Button>
                ) : (
                  <Button
                    variant="secondary"
                    disabledReason="The backend returned no cursor, so this is the last page it has."
                  >
                    Next page
                  </Button>
                )}
              </div>
            </>
          )}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

function ChainView(props: { readonly chain: Chain }): ReactNode {
  const { chain } = props;
  const byKind = new Map<string, ActivityItem[]>();
  for (const item of chain.items) {
    const list = byKind.get(item.kind) ?? [];
    list.push(item);
    byKind.set(item.kind, list);
  }
  const auxiliary = chain.items.filter(
    (item) => !LIFECYCLE_STAGES.some((stage) => stage.kind === item.kind),
  );

  return (
    <article className="chain">
      <header className="chain-head">
        <h3>
          {chain.correlationId === undefined ? (
            <span>Standalone event</span>
          ) : (
            <>
              Chain <Identifier value={chain.correlationId} />
            </>
          )}
        </h3>
        <span className="chain-time">{formatInstant(chain.startedAt)}</span>
      </header>

      <ol className="timeline">
        {LIFECYCLE_STAGES.map((stage) => {
          const rows = byKind.get(stage.kind) ?? [];
          return (
            <li key={stage.kind} className={rows.length > 0 ? "step step-present" : "step step-absent"}>
              <span className="step-marker" aria-hidden="true" />
              <div className="step-body">
                <p className="step-label">
                  {stage.label}
                  {rows.length === 0 && <span className="absent"> — not recorded</span>}
                </p>
                {rows.map((item) => (
                  <div className="step-item" key={item.id}>
                    <p className="step-summary">{item.summary}</p>
                    <p className="step-meta mono-small">
                      {formatInstant(item.occurred_at)} · {item.id}
                    </p>
                    {item.references !== undefined && Object.keys(item.references).length > 0 && (
                      <ul className="step-refs">
                        {Object.entries(item.references).map(([name, value]) => (
                          <li key={name}>
                            <span className="mono-small">
                              {name}: {value}
                            </span>
                          </li>
                        ))}
                      </ul>
                    )}
                    {item.detail !== undefined && Object.keys(item.detail).length > 0 && (
                      <details className="raw">
                        <summary>Detail the backend attached</summary>
                        <pre>{JSON.stringify(item.detail, null, 2)}</pre>
                      </details>
                    )}
                  </div>
                ))}
              </div>
            </li>
          );
        })}
      </ol>

      {auxiliary.length > 0 && (
        <>
          <h4>Alongside the trading lifecycle</h4>
          <Table caption="Other recorded events" headers={["Kind", "Summary", "When"]}>
            {auxiliary.map((item) => (
              <tr key={item.id}>
                <th scope="row">
                  <Pill tone={kindTone(item.kind)}>
                    {AUXILIARY_ACTIVITY_KINDS[item.kind] ?? item.kind}
                  </Pill>
                </th>
                <td>{item.summary}</td>
                <td>{formatInstant(item.occurred_at)}</td>
              </tr>
            ))}
          </Table>
        </>
      )}
    </article>
  );
}
