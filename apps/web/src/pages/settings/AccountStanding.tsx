/**
 * ACCOUNT (goal §37, USER_JOURNEY §9).
 *
 * Where this account stands, why it stands there, and how to leave.
 *
 * # A restriction is shown with its reason, in the customer's words
 *
 * `AccountRestriction.message` is written for the person it applies to. The
 * free text an operator wrote on a status change is deliberately not in this
 * response — it was written for other operators — so this page has nothing to
 * hide and nothing to paraphrase. It shows the code and the message, and it
 * does not invent a remedy for a restriction it was not told the remedy for.
 *
 * # Closing is a request with a cooling-off period, never a deletion
 *
 * The API is explicit that nothing is deleted: financial and audit records are
 * retained because they have to be. The page says that BEFORE the button, not
 * in a confirmation afterwards, because somebody who would not have asked had
 * they known needs to know before they ask.
 *
 * # The asymmetry is the point
 *
 * Requesting closure needs a recent strong authentication; cancelling one does
 * not, and the API refuses to require it. Stopping a dangerous request must
 * never be harder than starting it. So the request path renders the whole
 * step-up round trip — the reason, the control, and a return path back to this
 * page — while the cancel path is one button that works immediately.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import {
  useCancelAccountClosure,
  useCloseAccount,
  useMyAccount,
  type MyAccount,
} from "../../api/queries.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { AsyncPanel, EmptyState } from "../../components/DataState.tsx";
import { DataTable, type Column } from "../../components/DataTable.tsx";
import { Field, FieldGrid, FormField } from "../../components/Field.tsx";
import { Identifier, IdentifierShort, Page, Panel } from "../../components/Layout.tsx";
import { Refusal } from "../../components/Refusal.tsx";
import { Refused } from "../../components/Refused.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { useSurvivesSignIn } from "../../lib/survives-sign-in.ts";
import { formatInstant } from "../../lib/time.ts";
import { useSession } from "../../session.tsx";

type AccountRow = MyAccount["accounts"][number];
type Restriction = MyAccount["restrictions"][number];

const STATUS_TONE: Readonly<Record<string, "good" | "warn" | "bad" | "neutral">> = {
  ACTIVE: "good",
  RESTRICTED: "warn",
  FROZEN: "bad",
  CLOSED: "bad",
  SUSPENDED: "bad",
};

export function AccountStanding(): ReactNode {
  const session = useSession();
  const account = useMyAccount(session.signedIn);

  return (
    <Page
      title="Account"
      lead="Where this account stands, any restriction on it and why, and how to ask for it to be closed."
      actions={
        <LinkButton to="/settings" variant="secondary">
          Back to settings
        </LinkButton>
      }
    >
      <AsyncPanel
        query={account}
        loadingLabel="Asking the backend where this account stands…"
        skeleton={
          <Panel title="Standing">
            <Skeleton shape="text" count={4} label="Your account standing is loading" />
          </Panel>
        }
      >
        {(data: MyAccount) => <Standing data={data} />}
      </AsyncPanel>
    </Page>
  );
}

/* -------------------------------------------------------------------------- */

function Standing(props: { readonly data: MyAccount }): ReactNode {
  const { data } = props;

  const accountColumns: ReadonlyArray<Column<AccountRow>> = [
    { key: "id", header: "Account", cell: (row) => <IdentifierShort value={row.id} what="account id" /> },
    { key: "kind", header: "Kind", cell: (row) => <span>{row.kind}</span> },
    {
      key: "status",
      header: "Status",
      cell: (row) => (
        <StatusBadge tone={STATUS_TONE[row.status] ?? "neutral"}>{row.status}</StatusBadge>
      ),
    },
    {
      key: "reason",
      header: "Reason",
      cell: (row) => <span>{row.status_reason ?? "none recorded"}</span>,
    },
    {
      key: "created",
      header: "Opened",
      cell: (row) => <span>{formatInstant(row.created_at)}</span>,
    },
  ];

  return (
    <>
      <Panel
        title="Standing"
        description="The status the backend holds for you, and for each account under you."
      >
        <FieldGrid columns={2}>
          <Field label="Your status">
            <StatusBadge tone={STATUS_TONE[data.user_status] ?? "neutral"}>
              {data.user_status}
            </StatusBadge>
          </Field>
          <Field label="Your identifier" note="Quote this to support.">
            <Identifier value={data.user_id} label="user" />
          </Field>
        </FieldGrid>
        <DataTable<AccountRow>
          caption="Every account under this user, with its status and the reason recorded for it"
          rows={data.accounts}
          rowKey={(row) => row.id}
          columns={accountColumns}
          empty={{
            title: "No accounts",
            body: "The backend holds no account under this user. Nothing financial is possible until one exists.",
          }}
        />
      </Panel>

      <Restrictions restrictions={data.restrictions} />
      <Closure data={data} />
    </>
  );
}

function Restrictions(props: { readonly restrictions: readonly Restriction[] }): ReactNode {
  return (
    <Panel
      title="Restrictions"
      description="What is stopped, and the reason as it was written for you."
    >
      {props.restrictions.length === 0 ? (
        <EmptyState
          title="Nothing is restricted"
          body="The backend reported no restriction on this account. Everything the deployment offers is open to you."
        />
      ) : (
        <div className="stack">
          {props.restrictions.map((restriction) => (
            <Refusal
              key={`${restriction.code}-${restriction.account_id ?? "user"}`}
              what="A restriction is in force on this account"
              rule={restriction.message}
              code={restriction.code}
              remedy="This page shows what the backend recorded. Support can say what would lift it; nothing on this page can."
            >
              {restriction.account_id !== undefined && (
                <p className="refusal-body">
                  It applies to account <IdentifierShort value={restriction.account_id} what="account id" />.
                </p>
              )}
            </Refusal>
          ))}
        </div>
      )}
    </Panel>
  );
}

/* -------------------------------------------------------------------------- */

function Closure(props: { readonly data: MyAccount }): ReactNode {
  const { data } = props;
  const close = useCloseAccount();
  const cancel = useCancelAccountClosure();
  const reason = useSurvivesSignIn<string>("account.close.reason", "");
  const key = useSurvivesSignIn<string>("account.close.key", "");
  const [asking, setAsking] = useState(false);

  const open = data.closure_request;
  const pending = open !== undefined && open.state === "PENDING";

  if (pending && open !== undefined) {
    return (
      <Panel
        title="Closure requested"
        description="A request is open. Cancelling it needs nothing more than this button."
      >
        <FieldGrid columns={3}>
          <Field label="Requested">
            <span>{formatInstant(open.requested_at)}</span>
          </Field>
          <Field
            label="Cooling-off ends"
            note="Until this passes, nobody can effect the request — including an operator."
          >
            <span>{formatInstant(open.cooling_off_until)}</span>
          </Field>
          <Field label="State">
            <StatusBadge tone="warn">{open.state}</StatusBadge>
          </Field>
        </FieldGrid>
        {cancel.isError && (
          <Refused
            what="Cancelling your closure request"
            error={cancel.error}
            onRetry={() => {
              cancel.reset();
            }}
          />
        )}
        <p className="field-note">
          Your account still works normally while a request is open. Nothing has been deleted and
          nothing will be: financial and audit records are retained whatever happens to the account.
        </p>
        <div className="form-actions">
          <Button
            variant="primary"
            busy={cancel.isPending}
            busyLabel="Cancelling…"
            onClick={() => {
              cancel.mutate();
            }}
          >
            Cancel the closure request
          </Button>
        </div>
      </Panel>
    );
  }

  return (
    <Panel
      title="Closing your account"
      description="A request with a cooling-off period, decided by an operator. Never an immediate deletion."
    >
      <FieldGrid columns={2}>
        <Field
          label="Cooling-off period"
          note="How long a new request waits before it can be effected."
        >
          <span>{String(data.cooling_off_days)} days</span>
        </Field>
        <Field label="Previous request">
          <span>
            {open === undefined
              ? "none"
              : `${open.state} on ${formatInstant(open.decided_at ?? open.requested_at)}`}
          </span>
        </Field>
      </FieldGrid>

      <p className="field-note">
        Read this before you ask. Closing is a request, not a deletion: your financial records, your
        ledger entries and your audit trail are retained because they have to be. The request waits
        out the cooling-off period above, during which you can cancel it without being asked to
        confirm who you are, and is then effected by an operator.
      </p>

      {close.isError && (
        <Refused
          what="Requesting closure"
          error={close.error}
          onRetry={() => {
            close.reset();
          }}
        >
          <p className="refusal-body">
            Your reason is kept. Coming back from a stronger sign-in returns to this page with it,
            and the request is retried as the same request rather than a second one.
          </p>
        </Refused>
      )}

      {asking ? (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            const existing = key.value === "" ? newIdempotencyKey() : key.value;
            key.set(existing);
            close.mutate(
              { reason: reason.value, idempotencyKey: existing },
              {
                onSuccess: () => {
                  key.set("");
                  reason.clear();
                  setAsking(false);
                },
              },
            );
          }}
        >
          <FormField
            label="Why, if you want to say"
            hint="Optional, and up to 500 characters. A person leaving does not owe an explanation."
          >
            {(field) => (
              <textarea
                className="input"
                rows={3}
                maxLength={500}
                value={reason.value}
                onChange={(event) => {
                  reason.set(event.target.value);
                }}
                {...field}
              />
            )}
          </FormField>
          <div className="form-actions">
            <Button variant="danger" submit busy={close.isPending} busyLabel="Sending the request…">
              Request closure
            </Button>
            <Button
              variant="quiet"
              onClick={() => {
                setAsking(false);
                close.reset();
              }}
            >
              Keep my account
            </Button>
          </div>
          <p className="field-note">
            This asks for a stronger sign-in. If the backend asks for one, this page says so and
            offers the round trip; what you typed comes back with you.
          </p>
        </form>
      ) : (
        <div className="form-actions">
          <Button
            variant="secondary"
            onClick={() => {
              setAsking(true);
            }}
          >
            Request account closure
          </Button>
        </div>
      )}
    </Panel>
  );
}
