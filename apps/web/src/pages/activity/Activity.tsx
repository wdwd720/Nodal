/**
 * ACTIVITY (goal §16, USER_JOURNEY §6).
 *
 * One feed, filtered by kind, paged by cursor, with each row linking to the
 * thing it is about.
 *
 * # The summary is the server's sentence, rendered as it was written
 *
 * `ActivityFeedItem.summary` is built on the server from a fixed template per
 * kind and never from user text. That is why this page renders it as a string
 * and does not parse it, reformat it, or extract a figure from it: the moment a
 * client starts reading a sentence for numbers, the sentence becomes a data
 * format and the template becomes a contract nobody wrote down.
 *
 * The figures come separately, in `amounts`, each with its own unit and its own
 * temperature — which is the only honest way to render a feed where a sandbox
 * rehearsal can sit directly above a real purchase.
 *
 * # Three units, three renderings, no conversions
 *
 *   CREDITS      exact base units of a Credit, at the Credit scale
 *   MONEY_MINOR  minor units of `currency` — the money side of a purchase
 *   ASSET_UNITS  exact base units of `symbol`
 *
 * Nothing here turns one into another. A row that shows both the money paid and
 * the Credits received shows two figures, because that is two facts; a single
 * converted figure would be this page inventing an exchange rate.
 *
 * ASSET_UNITS is rendered at scale zero — as exact base units with the symbol
 * beside it — because `ActivityAmount` carries no decimals for the asset. That
 * is honest rather than pretty, and it is a reported API gap: the feed should
 * carry the scale with the figure the way the portfolio does.
 *
 * # Goal §16's last line
 *
 * "Never expose provider secrets or internal sensitive evidence." Nothing on
 * this page reaches for one: the feed carries a reference type and id, and the
 * page turns those into a link where a route exists and into a copyable
 * identifier where it does not.
 */
import { useState, type ReactNode } from "react";

import {
  useMeActivity,
  useVersion,
  type ActivityAmount,
  type ActivityFeed,
  type ActivityFeedItem,
  type ActivityFeedKind,
} from "../../api/queries.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../components/DataState.tsx";
import { Figure } from "../../components/Figure.tsx";
import { IdentifierShort } from "../../components/Identifier.tsx";
import { Disclosure, Page, Panel, PanelCard } from "../../components/Layout.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { Temp, temperatureOf } from "../../components/Temperature.tsx";
import { CREDIT_DECIMALS, minorStringToUsd } from "../../lib/credits.ts";
import { EMPTY_STATES } from "../../lib/errors.ts";
import { CREDITS_DISCLOSURE, NATIVE_ASSET_RISK, SANDBOX_TIER_NOTE } from "../../lib/honesty.ts";
import { formatInstant } from "../../lib/time.ts";
import { useActiveAccountId } from "../../session.tsx";

/** Every kind the feed can carry, with the word a person would use for it. */
const KINDS: ReadonlyArray<readonly [ActivityFeedKind, string]> = [
  ["CREDIT_PURCHASE", "Credit purchases"],
  ["CREDIT_REVERSAL", "Reversals"],
  ["NATIVE_TRADE", "Trades"],
  ["NATIVE_ASSET_CREATED", "Assets created"],
  ["PAYOUT_REQUESTED", "Withdrawal requests"],
  ["PAYOUT_STATE_CHANGED", "Withdrawal updates"],
  ["ADMIN_ADJUSTMENT", "Adjustments"],
];

/** Where a reference genuinely resolves to a page. */
function routeFor(type: string, id: string): string | undefined {
  if (id === "") return undefined;
  switch (type) {
    case "native_market":
      return `/markets/${id}`;
    case "native_asset":
      return `/markets/${id}`;
    case "agent":
      return `/agents/${id}`;
    case "payout_request":
      return "/withdraw";
    case "credit_funding":
    case "credit_purchase":
      return "/buy-credits";
    case "account":
      return "/settings/account";
    default:
      return undefined;
  }
}

export function Activity(): ReactNode {
  const accountId = useActiveAccountId();
  // The feed marks a row simulated from the API's own answer, which is right
  // for a row. It cannot answer the question the reader is actually asking on
  // a rehearsal deployment — "is any of this real?" — so the tier is read here
  // and says so once, in words, above the feed.
  const version = useVersion();
  const sandbox = version.data?.sandbox_tier === true;
  const [kinds, setKinds] = useState<readonly ActivityFeedKind[]>([]);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const feed = useMeActivity({
    accountId,
    kinds,
    ...(cursor === undefined ? {} : { cursor }),
  });

  if (accountId === undefined) {
    return (
      <Page title="Activity">
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is nothing that could have happened yet."
        />
      </Page>
    );
  }

  const toggle = (kind: ActivityFeedKind): void => {
    setCursor(undefined);
    setKinds((current) =>
      current.includes(kind) ? current.filter((k) => k !== kind) : [...current, kind],
    );
  };

  return (
    <Page
      title="Activity"
      lead="Everything that has happened to this account, newest first. Each row is the server's own summary of one event."
    >
      {sandbox && <p className="field-note">{SANDBOX_TIER_NOTE}</p>}

      <Panel
        title="What to show"
        description="No filter means every kind. Choosing none is the same as choosing all, because a feed of nothing is not a filter anybody wants."
      >
        <div className="form-actions">
          {KINDS.map(([kind, label]) => (
            <Button
              key={kind}
              variant={kinds.includes(kind) ? "primary" : "secondary"}
              onClick={() => {
                toggle(kind);
              }}
            >
              {label}
            </Button>
          ))}
          {kinds.length > 0 && (
            <Button
              variant="quiet"
              onClick={() => {
                setKinds([]);
                setCursor(undefined);
              }}
            >
              Clear the filter
            </Button>
          )}
        </div>
      </Panel>

      <Panel
        title={kinds.length === 0 ? "Everything" : "Filtered"}
        description="A figure here is shown at the temperature the backend gave it, so a rehearsal never reads as a real movement."
      >
        <AsyncPanel
          query={feed}
          loadingLabel="Asking the backend what has happened…"
          skeleton={<Skeleton shape="rows" count={6} label="Your activity is loading" />}
          empty={{
            isEmpty: (page: ActivityFeed) => page.items.length === 0,
            title: kinds.length === 0 ? EMPTY_STATES.activity.title : "Nothing of those kinds",
            body:
              kinds.length === 0
                ? EMPTY_STATES.activity.body
                : "The backend returned no rows for the kinds selected. Clearing the filter shows everything it holds.",
          }}
        >
          {(page: ActivityFeed) => (
            <div className="stack">
              {page.items.map((item) => (
                <Row key={item.id} item={item} sandbox={sandbox} />
              ))}
              <div className="form-actions">
                {cursor !== undefined && (
                  <Button
                    variant="secondary"
                    onClick={() => {
                      setCursor(undefined);
                    }}
                  >
                    Back to newest
                  </Button>
                )}
                {page.nextCursor !== null && page.nextCursor !== "" ? (
                  <Button
                    variant="secondary"
                    onClick={() => {
                      setCursor(page.nextCursor ?? undefined);
                    }}
                  >
                    Show older
                  </Button>
                ) : (
                  <p className="field-note">
                    The backend returned no further page, so this is everything it holds.
                  </p>
                )}
              </div>
            </div>
          )}
        </AsyncPanel>
      </Panel>

      <Disclosure title="What Credits are">
        <p>{CREDITS_DISCLOSURE}</p>
      </Disclosure>
      <Disclosure title="What a trade in a Nodal-native asset is">
        <p>{NATIVE_ASSET_RISK}</p>
      </Disclosure>
    </Page>
  );
}

/* -------------------------------------------------------------------------- */

function Amount(props: { readonly amount: ActivityAmount }): ReactNode {
  const { amount } = props;

  if (amount.unit === "MONEY_MINOR") {
    return (
      <Temp value={amount.temperature}>
        <Figure
          kind="money"
          value={{ decimal: minorStringToUsd(amount.value) }}
          symbol={amount.currency ?? ""}
          signed
        />
      </Temp>
    );
  }

  if (amount.unit === "ASSET_UNITS") {
    return (
      <Temp value={amount.temperature}>
        <Figure
          kind="units"
          value={{ base: amount.value, scale: 0 }}
          symbol={amount.symbol ?? "units"}
          signed
        />
        {/* The feed carries no scale for the asset, so the exact base units are
            what is shown. Dividing by a scale this page guessed would be a
            figure the backend never stated. */}
        <span className="mono-small"> base units</span>
      </Temp>
    );
  }

  return (
    <Temp value={amount.temperature}>
      <Figure
        kind="units"
        value={{ base: amount.value, scale: CREDIT_DECIMALS }}
        symbol="Credits"
        signed
      />
      {amount.origin !== undefined && (
        <span className="mono-small"> {amount.origin}</span>
      )}
    </Temp>
  );
}

function Row(props: {
  readonly item: ActivityFeedItem;
  /** The deployment is a rehearsal, so every row on it is. */
  readonly sandbox: boolean;
}): ReactNode {
  const { item } = props;
  const to = routeFor(item.reference.type, item.reference.id);
  // The row's own temperature is the strongest of its amounts: a row with one
  // simulated figure in it is a simulated row, because that is the claim a
  // reader takes away from it.
  const rowTemp = props.sandbox
    ? "simulated"
    : item.simulated
    ? "simulated"
    : item.amounts.some((a) => temperatureOf(a.temperature) === "simulated")
      ? "simulated"
      : item.amounts.some((a) => temperatureOf(a.temperature) === "real")
        ? "real"
        : "economy";

  return (
    <PanelCard
      title={item.summary}
      temp={rowTemp}
      actions={
        item.status === undefined ? undefined : <StatusBadge>{item.status}</StatusBadge>
      }
    >
      <p className="field-note">
        <span className="mono-small">{item.kind}</span> · {formatInstant(item.occurred_at)}
      </p>
      {item.amounts.length > 0 && (
        <ul className="segbar-legend">
          {item.amounts.map((amount, index) => (
            <li key={`${amount.unit}-${String(index)}`}>
              <span className="eyebrow">{amount.unit.replace("_", " ").toLowerCase()}</span>
              <div>
                <Amount amount={amount} />
              </div>
            </li>
          ))}
        </ul>
      )}
      <div className="form-actions">
        {to !== undefined ? (
          <LinkButton to={to} variant="secondary">
            Open what this is about
          </LinkButton>
        ) : (
          <IdentifierShort value={item.reference.id} what={item.reference.type} />
        )}
      </div>
    </PanelCard>
  );
}
