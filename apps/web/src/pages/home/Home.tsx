/**
 * HOME (goal §8, USER_JOURNEY §3).
 *
 * The modules are in the order §3 sets, and the order is the argument: Credits
 * first, because that is what the product runs on; then what is owned, then
 * what is acting on the owner's behalf, then what is moving, then what has
 * happened. A dashboard that leads with a headline total teaches somebody to
 * read one number and stop, and the one number that would sit there is the one
 * that is true of nothing.
 *
 * # What this page never does
 *
 * It never adds a Credit figure to anything, and it never shows a placeholder
 * where a figure has not arrived — a `Skeleton` says "a balance is coming",
 * a zero says "you have nothing", and only one of those is honest while a
 * request is in flight. Buying power, portfolio value and Credit balances all
 * come from the backend already computed; nothing on this page derives one.
 *
 * # Internal Credits and payout-eligible value are separated structurally
 *
 * Goal §8 asks for the distinction and §46 forbids treating "available",
 * "spendable", "withdrawable" and "settled" as synonyms. So they are two
 * different blocks answering two different questions — what may be SPENT
 * inside Nodal, and what a payout policy would CONSIDER — rather than two
 * labels on one row, and the payout block says in words that it is a quantity
 * of Credits and not an amount of US dollars.
 *
 * # The modules not here yet
 *
 * Portfolio value, holdings, market movers and recent activity are §3 modules
 * whose routes (`GET /v1/me/portfolio`, the market collection read, and
 * `GET /v1/me/activity`) are not on this API. Their places are marked below and
 * render NOTHING — not a panel with dashes in it, not an illustration, not a
 * number. A module that is not there is absent; a module drawn from figures
 * that do not exist is a lie with a loading state.
 */
import type { ReactNode } from "react";

import { useAgents, useCreditBalance, type Agent, type CreditBalance } from "../../api/queries.ts";
import { LinkButton } from "../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../components/DataState.tsx";
import { DataTable, type Column } from "../../components/DataTable.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Figure } from "../../components/Figure.tsx";
import { Disclosure, Page, Panel } from "../../components/Layout.tsx";
import { SegmentedBar } from "../../components/SegmentedBar.tsx";
import { Skeleton, SkeletonField } from "../../components/Skeleton.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { CREDIT_DECIMALS } from "../../lib/credits.ts";
import { EMPTY_STATES } from "../../lib/errors.ts";
import { CREDITS_DISCLOSURE, PROVENANCE_NOTE } from "../../lib/honesty.ts";
import { useActiveAccountId } from "../../session.tsx";

/**
 * The sentence goal §8 asks for, in the one place it belongs: beside the
 * payout-eligible figure, not in a footnote at the bottom of the page.
 */
const PAYOUT_ELIGIBLE_SENTENCE =
  "Payout-eligible value is a quantity of Credits, not an amount of US dollars. It is the part of " +
  "this balance a payout policy would consider if an approved payout path were active; it is not " +
  "an amount held for you anywhere, it is not a bank deposit, and it is not insured.";

/** The four actions goal §8 names. Withdraw is here for everybody, always. */
function PrimaryActions(): ReactNode {
  return (
    <>
      <LinkButton to="/buy-credits" variant="primary">
        Buy Credits
      </LinkButton>
      <LinkButton to="/markets" variant="secondary">
        Trade
      </LinkButton>
      <LinkButton to="/agents/new" variant="secondary">
        Create Agent
      </LinkButton>
      {/* Never hidden from an unverified customer (goal §8): the page it opens
          explains what verification is for, which is the whole point. */}
      <LinkButton to="/withdraw" variant="secondary">
        Withdraw
      </LinkButton>
    </>
  );
}

export function Home(): ReactNode {
  const accountId = useActiveAccountId();
  const credits = useCreditBalance(accountId);
  const agents = useAgents(accountId);

  if (accountId === undefined) {
    return (
      <Page title="Home" actions={<PrimaryActions />}>
        <EmptyState
          title="This principal owns no account"
          body="The backend returned an empty account list for this session, so there is no balance to show. That is the backend's answer, not a loading state."
        />
      </Page>
    );
  }

  return (
    <Page
      title="Home"
      lead="Every figure here was computed by the backend. Nothing on this page adds two kinds of value together."
      actions={<PrimaryActions />}
    >
      {/* 1. Credits. */}
      <Panel
        title="Credits"
        description="What this account holds inside Nodal, and what may be done with each part of it."
        temp="economy"
      >
        <AsyncPanel
          query={credits}
          loadingLabel="Asking the backend for the Credit balance…"
          skeleton={
            <FieldGrid columns={3}>
              <SkeletonField label="Total Credits" />
              <SkeletonField label="Spendable" />
              <SkeletonField label="Frozen" />
            </FieldGrid>
          }
        >
          {(balance: CreditBalance) => <Credits balance={balance} />}
        </AsyncPanel>
      </Panel>

      {/* 2. Portfolio value and today's change — USER_JOURNEY §3, module 2.
          Needs `GET /v1/me/portfolio`, which this API does not serve. The
          module lands with that route; until then this renders nothing, on
          purpose. */}

      {/* 3. Holdings — USER_JOURNEY §3, module 3. Same route, same rule. */}

      {/* 4. Active agents. */}
      <Panel
        title="Agents"
        description="What is acting on this account's behalf, and the authority each one was granted."
        temp="economy"
      >
        <AsyncPanel
          query={agents}
          loadingLabel="Asking the backend for this account's agents…"
          skeleton={<Skeleton shape="rows" count={3} label="The agent list is loading" />}
          empty={{
            isEmpty: (list: Agent[]) => list.length === 0,
            title: EMPTY_STATES.agents.title,
            body: EMPTY_STATES.agents.body,
          }}
        >
          {(list: Agent[]) => <Agents list={list} />}
        </AsyncPanel>
      </Panel>

      {/* 5. Markets — USER_JOURNEY §3, module 5. The movers come from the
          market COLLECTION route, which this API does not serve: only the
          single-market read exists. Renders nothing until the list lands, and
          it will carry the user-created-asset risk statement when it does. */}

      {/* 6. Recent activity — USER_JOURNEY §3, module 6. Needs
          `GET /v1/me/activity?limit=5`. Renders nothing until it does. */}

      <Disclosure title="What Credits are">
        <p>{CREDITS_DISCLOSURE}</p>
        <p>{PROVENANCE_NOTE}</p>
      </Disclosure>
    </Page>
  );
}

/* -------------------------------------------------------------------------- */

function Credits(props: { readonly balance: CreditBalance }): ReactNode {
  const { balance } = props;
  const value = (base: string): { readonly base: string; readonly scale: number } => ({
    base,
    scale: CREDIT_DECIMALS,
  });

  return (
    <div className="stack">
      <FieldGrid columns={3}>
        <Field label="Total Credits" note="Everything this account holds, whatever may be done with it." emphasis>
          <Figure kind="units" value={value(balance.gross)} symbol="Credits" big />
        </Field>
        <Field label="Spendable" note="Usable inside Nodal right now.">
          <Figure kind="units" value={value(balance.spendable)} symbol="Credits" />
        </Field>
        <Field
          label="Frozen"
          note="Held by a restriction or an open dispute. Not spendable until that clears."
        >
          <Figure kind="units" value={value(balance.frozen)} symbol="Credits" />
        </Field>
      </FieldGrid>

      <SegmentedBar
        caption="How this balance is held"
        scale={CREDIT_DECIMALS}
        symbol="Credits"
        total={balance.gross}
        segments={[
          {
            key: "spendable",
            label: "Spendable",
            baseUnits: balance.spendable,
            explanation: "Usable inside Nodal right now.",
            texture: "solid",
          },
          {
            key: "frozen",
            label: "Frozen",
            baseUnits: balance.frozen,
            explanation: "Held by a restriction or an open dispute.",
            texture: "hatch",
          },
        ]}
      />

      {/* A second axis, deliberately its own block: "may be spent" and "may be
          paid out" are different questions with different answers, and §46
          forbids the interface treating them as one. */}
      <div className="stack">
        <h3>Payout-eligible value</h3>
        <FieldGrid columns={2}>
          <Field label="Payout-eligible" note="What a payout policy would consider.">
            <Figure kind="units" value={value(balance.payout_eligible)} symbol="Credits" />
          </Field>
          <Field label="Not payout-eligible" note="Everything else in this balance.">
            <Figure kind="units" value={value(balance.ineligible)} symbol="Credits" />
          </Field>
        </FieldGrid>
        <p className="field-note">{PAYOUT_ELIGIBLE_SENTENCE}</p>
        <p className="field-note">{EMPTY_STATES.verification.body}</p>
        {balance.ineligible_reasons !== undefined && balance.ineligible_reasons.length > 0 && (
          <ul className="explain-fields">
            {balance.ineligible_reasons.map((reason) => (
              <li key={reason}>
                <span className="mono-small">{reason}</span>
              </li>
            ))}
          </ul>
        )}
      </div>

      <Origins balance={balance} />

      <p className="field-note">
        Policy <span className="mono-small">{balance.policy_version}</span>. The response carries no
        snapshot instant, so none is shown; this figure is refetched on every visit and again
        whenever the event stream reports the balance stale.
      </p>
    </div>
  );
}

/** Where the Credits came from. Origin is what decides eligibility, not the total. */
function Origins(props: { readonly balance: CreditBalance }): ReactNode {
  const byOrigin = props.balance.by_origin;
  if (byOrigin === undefined) return null;
  const rows = Object.entries(byOrigin);
  if (rows.length === 0) return null;

  return (
    <div className="stack">
      <h3>Where these Credits came from</h3>
      <DataTable<readonly [string, string]>
        caption="Credits held, by the origin that created them"
        rows={rows}
        rowKey={(row) => row[0]}
        columns={[
          { key: "origin", header: "Origin", cell: (row) => <span>{row[0]}</span> },
          {
            key: "quantity",
            header: "Credits",
            numeric: true,
            cell: (row) => (
              <Figure
                kind="units"
                value={{ base: row[1], scale: CREDIT_DECIMALS }}
                symbol="Credits"
              />
            ),
          },
        ]}
      />
      <p className="field-note">{PROVENANCE_NOTE}</p>
    </div>
  );
}

/* -------------------------------------------------------------------------- */

/** What the product's own word for an agent's state means, in a badge tone. */
function statusTone(status: string): "good" | "warn" | "bad" | "neutral" {
  if (status === "ENABLED") return "good";
  if (status === "PAUSED") return "warn";
  if (status === "FAILED" || status === "DISABLED") return "bad";
  return "neutral";
}

function Agents(props: { readonly list: readonly Agent[] }): ReactNode {
  const columns: ReadonlyArray<Column<Agent>> = [
    {
      key: "name",
      header: "Agent",
      cell: (agent) => <span>{agent.name}</span>,
    },
    {
      key: "status",
      header: "Status",
      cell: (agent) => (
        <StatusBadge tone={statusTone(agent.status)}>{agent.status}</StatusBadge>
      ),
    },
    {
      key: "authority",
      header: "Authority",
      cell: (agent) => (
        <span title={agent.authority.summary}>
          {agent.authority.level} · {agent.authority.name}
        </span>
      ),
    },
    {
      key: "budget",
      header: "Budget used",
      numeric: true,
      cell: (agent) => (
        <Figure
          kind="units"
          value={{ base: agent.budget.used_credits, scale: CREDIT_DECIMALS }}
          symbol="Credits"
        />
      ),
    },
    {
      key: "granted",
      header: "Budget granted",
      numeric: true,
      cell: (agent) => (
        <Figure
          kind="units"
          value={{ base: agent.budget.granted_credits, scale: CREDIT_DECIMALS }}
          symbol="Credits"
        />
      ),
    },
    {
      key: "runtime",
      header: "Runtime",
      cell: (agent) => <span title={agent.runtime.detail}>{agent.runtime.evaluator}</span>,
    },
  ];

  return (
    <div className="stack">
      <DataTable<Agent>
        caption="Agents on this account, with the authority granted to each and the budget it has used"
        rows={props.list}
        rowKey={(agent) => agent.id}
        columns={columns}
      />
      <p className="field-note">
        An agent being ENABLED is the owner granting it the right to be evaluated. Whether anything
        is evaluating it is the runtime column, which is a separate fact.
      </p>
      <div className="form-actions">
        <LinkButton to="/agents" variant="secondary">
          Open agents
        </LinkButton>
      </div>
    </div>
  );
}
