/**
 * SECURITY (goal §37, USER_JOURNEY §9).
 *
 * Three questions, answered from three routes that all read and none of which
 * create anything: how strong is this sign-in, what sessions exist, and what
 * has happened to this account.
 *
 * # MFA is reported, not offered
 *
 * `SecuritySummary.mfa_present` says whether the identity provider asserted a
 * strong method for THIS session. Nodal does not enrol factors — the identity
 * provider owns that, Nodal never sees a password and never sees a factor — so
 * there is no "turn on MFA" control here that would pretend otherwise. What
 * there is, is the truth about the current session and where to go to change
 * it, which is the provider.
 *
 * # Revoking a session is a real, immediate action
 *
 * It is offered for every session including the current one, because a person
 * who has decided to end a session is usually right. The current one is marked
 * so nobody ends it by accident.
 *
 * # The audit trail is the customer's own
 *
 * `GET /v1/me/audit` is stitched from the security and account trails and can
 * reach nobody else's. It names actor CATEGORIES rather than people: an
 * operator's identity is never exposed to a customer, and a row that said
 * "changed by Dana" would be exposing one.
 */
import { useState, type ReactNode } from "react";

import {
  useMeAudit,
  useRevokeSession,
  useSecuritySummary,
  useSessions,
  type AuditPage,
  type MeAuditEntry,
  type SecuritySummary,
  type SessionSummary,
} from "../../api/queries.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { AsyncPanel } from "../../components/DataState.tsx";
import { DataTable, type Column } from "../../components/DataTable.tsx";
import { Field, FieldGrid } from "../../components/Field.tsx";
import { Figure } from "../../components/Figure.tsx";
import { IdentifierShort } from "../../components/Identifier.tsx";
import { Page, Panel } from "../../components/Layout.tsx";
import { Refused } from "../../components/Refused.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { StatusBadge } from "../../components/StatusBadge.tsx";
import { beginSignIn, useSession } from "../../session.tsx";
import { formatInstant } from "../../lib/time.ts";

export function Security(): ReactNode {
  const session = useSession();
  const summary = useSecuritySummary(session.signedIn);
  const sessions = useSessions(session.signedIn);
  const revoke = useRevokeSession();
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const audit = useMeAudit(cursor);

  const sessionColumns: ReadonlyArray<Column<SessionSummary>> = [
    {
      key: "id",
      header: "Session",
      cell: (row) => <IdentifierShort value={row.id} what="session id" />,
    },
    {
      key: "device",
      header: "Device",
      cell: (row) => <span>{row.device_label ?? row.user_agent ?? "not reported"}</span>,
    },
    { key: "ip", header: "Address", cell: (row) => <span>{row.ip ?? "not reported"}</span> },
    {
      key: "seen",
      header: "Last seen",
      cell: (row) => <span>{formatInstant(row.last_seen_at)}</span>,
    },
    {
      key: "expires",
      header: "Expires",
      cell: (row) => <span>{formatInstant(row.expires_at)}</span>,
    },
    {
      key: "state",
      header: "State",
      cell: (row) =>
        row.revoked_at !== undefined ? (
          <StatusBadge tone="bad">Revoked</StatusBadge>
        ) : row.current === true ? (
          <StatusBadge tone="good">This session</StatusBadge>
        ) : (
          <StatusBadge>Active</StatusBadge>
        ),
    },
    {
      key: "revoke",
      header: "End it",
      cell: (row) =>
        row.revoked_at !== undefined ? (
          <span className="absent">already revoked</span>
        ) : (
          <Button
            variant="danger"
            busy={revoke.isPending && revoke.variables === row.id}
            busyLabel="Ending…"
            onClick={() => {
              revoke.mutate(row.id);
            }}
          >
            {row.current === true ? "End this session" : "End session"}
          </Button>
        ),
    },
  ];

  const auditColumns: ReadonlyArray<Column<MeAuditEntry>> = [
    { key: "when", header: "When", cell: (row) => <span>{formatInstant(row.occurred_at)}</span> },
    {
      key: "source",
      header: "Trail",
      cell: (row) => <StatusBadge>{row.source}</StatusBadge>,
    },
    {
      key: "action",
      header: "What happened",
      cell: (row) => <span className="mono-small">{row.action}</span>,
    },
    {
      key: "actor",
      header: "Who",
      cell: (row) => <span>{row.actor_type ?? "not reported"}</span>,
    },
    { key: "ip", header: "Address", cell: (row) => <span>{row.ip ?? "not reported"}</span> },
  ];

  return (
    <Page
      title="Security"
      lead="How this session was authenticated, every session that exists, and everything that has happened to your account."
      actions={
        <LinkButton to="/settings" variant="secondary">
          Back to settings
        </LinkButton>
      }
    >
      <Panel
        title="This sign-in"
        description="Derived from the session store and the claims of the current session. Nothing here is a secret and nothing here is created by asking."
      >
        <AsyncPanel
          query={summary}
          loadingLabel="Asking the backend about this session…"
          skeleton={<Skeleton shape="text" count={4} label="The security summary is loading" />}
        >
          {(data: SecuritySummary) => (
            <div className="stack">
              <FieldGrid columns={3}>
                <Field label="Active sessions">
                  <Figure kind="count" count={data.active_sessions} />
                </Field>
                <Field
                  label="Strong authentication"
                  note="Asserted by the identity provider for this session. Nodal never sees a password or a factor."
                >
                  <StatusBadge tone={data.mfa_present ? "good" : "warn"}>
                    {data.mfa_present ? "Asserted" : "Not asserted"}
                  </StatusBadge>
                </Field>
                <Field label="Methods the provider named">
                  <span className="mono-small">
                    {data.amr.length === 0 ? "none reported" : data.amr.join(", ")}
                  </span>
                </Field>
                <Field label="Last sign-in">
                  <span>{formatInstant(data.last_login_at)}</span>
                </Field>
                <Field label="Last strong confirmation">
                  <span>
                    {data.last_step_up_at === undefined
                      ? "never on this account"
                      : formatInstant(data.last_step_up_at)}
                  </span>
                </Field>
                <Field
                  label="Strong confirmation valid until"
                  note="Sensitive actions ask again once this passes."
                >
                  <span>
                    {data.step_up_valid_until === undefined
                      ? "not currently valid"
                      : formatInstant(data.step_up_valid_until)}
                  </span>
                </Field>
              </FieldGrid>
              <p className="field-note">
                A strong confirmation counts for {String(data.step_up_max_age_seconds)} seconds. After
                that, closing an account or moving value asks for it again.
              </p>
              <div className="form-actions">
                <Button
                  variant="secondary"
                  onClick={() => {
                    beginSignIn({ returnTo: "/settings/security", stepUp: true });
                  }}
                >
                  Confirm it&rsquo;s you now
                </Button>
              </div>
              <p className="field-note">
                Enrolling or changing a factor happens with the identity provider, not here. Nodal
                holds no credential of yours to change.
              </p>
            </div>
          )}
        </AsyncPanel>
      </Panel>

      <Panel
        title="Sessions"
        description="Every session the backend holds for you. Ending one takes effect immediately."
      >
        {revoke.isError && (
          <Refused
            what="Ending that session"
            error={revoke.error}
            onRetry={() => {
              revoke.reset();
            }}
          />
        )}
        <AsyncPanel
          query={sessions}
          loadingLabel="Asking the backend for your sessions…"
          skeleton={<Skeleton shape="rows" count={3} label="Your sessions are loading" />}
          empty={{
            isEmpty: (rows: SessionSummary[]) => rows.length === 0,
            title: "No sessions",
            body: "The backend returned no sessions for you, which is unusual while you are reading this page. Reloading asks again.",
          }}
        >
          {(rows: SessionSummary[]) => (
            <DataTable<SessionSummary>
              caption="Sessions on this account, when each was last seen, and a control to end it"
              rows={rows}
              rowKey={(row) => row.id}
              columns={sessionColumns}
            />
          )}
        </AsyncPanel>
      </Panel>

      <Panel
        title="Your own history"
        description="The sign-in trail and what has been done to your accounts. It reaches nobody else's record and names actor categories rather than people."
      >
        <AsyncPanel
          query={audit}
          loadingLabel="Asking the backend for your history…"
          skeleton={<Skeleton shape="rows" count={6} label="Your history is loading" />}
          empty={{
            isEmpty: (page: AuditPage) => page.items.length === 0,
            title: "Nothing recorded yet",
            body: "Sign-ins, session changes and anything done to your accounts appear here as they happen.",
          }}
        >
          {(page: AuditPage) => (
            <div className="stack">
              <DataTable<MeAuditEntry>
                caption="Your own security and account history, newest first"
                rows={page.items}
                rowKey={(row) => row.id}
                columns={auditColumns}
                tall
              />
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
    </Page>
  );
}
