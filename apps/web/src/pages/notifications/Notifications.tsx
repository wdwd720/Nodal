/**
 * NOTIFICATIONS (goal §36, USER_JOURNEY §2 and §9).
 *
 * A notification centre is where a product is most tempted to lie by omission:
 * it is the one screen a person scans instead of reading, so a figure here
 * would be trusted without being checked. The API does not allow it — the
 * contract says a notification's `data` carries identifiers and state names and
 * never a balance, and canonical figures come from REST. This page honours that
 * by rendering the server's own title and body and adding no figure of its own.
 *
 * Three things that are not decoration:
 *
 *   - THE SANDBOX FLAG IS PER NOTIFICATION and is required by the contract, so
 *     a sandbox notification is labelled individually rather than the page
 *     taking a deployment-wide guess. A row that describes nothing of real
 *     value says so on the row.
 *
 *   - A PREFERENCE THAT CANNOT BE CHANGED IS SHOWN AS FIXED, not as a switch
 *     that quietly does nothing. The contract's `enforced: false` means the
 *     kind cannot be switched off — a new sign-in, an account restriction, a
 *     reversed purchase, a failed payout, a system message — and the stored
 *     answer is kept and ignored. A control that appears to work and does not
 *     is worse than one that states its reason.
 *
 *   - IN-APP IS THE ONLY CHANNEL, and the page says so once, plainly. There is
 *     no e-mail, SMS or push provider configured anywhere in this system, and a
 *     settings screen that implied otherwise would be promising delivery
 *     nothing can perform.
 *
 * The shell's unread badge and this page read the same query (`useUnreadCount`
 * in `src/api/queries.ts`), so marking one notification read moves both at
 * once. Two independent counts on one screen is how a bell comes to disagree
 * with the list under it.
 */
import { useState, type ReactNode } from "react";

import {
  useMarkAllNotificationsRead,
  useMarkNotificationRead,
  useNotificationPreferences,
  useNotifications,
  useUpdateNotificationPreferences,
  type Notification,
  type NotificationList,
  type NotificationPreference,
} from "../../api/queries.ts";
import { Button, LinkButton } from "../../components/Button.tsx";
import { AsyncPanel } from "../../components/DataState.tsx";
import { IdentifierShort } from "../../components/Identifier.tsx";
import { Disclosure, Page, Panel, PanelCard } from "../../components/Layout.tsx";
import { Refused } from "../../components/Refused.tsx";
import { Skeleton } from "../../components/Skeleton.tsx";
import { StatusBadge, type Tone } from "../../components/StatusBadge.tsx";
import { useToast } from "../../components/Toast.tsx";
import { EMPTY_STATES } from "../../lib/errors.ts";
import { formatInstant } from "../../lib/time.ts";

/** The one sentence §36 and the contract both require this page to say. */
const CHANNEL_SENTENCE =
  "In-app is the only delivery channel that exists. No e-mail, SMS or push provider is configured " +
  "anywhere in this system, so a preference here changes what appears on this page and nothing else.";

const SEVERITY_TONE: Readonly<Record<string, Tone>> = {
  INFO: "info",
  WARN: "warn",
  CRITICAL: "bad",
};

/**
 * Where a notification's object actually has a page.
 *
 * Only the types that genuinely resolve to a route are here. A link that lands
 * on a 404 is worse than an identifier somebody can copy, so the rest render as
 * identifiers and the activity feed is what stitches them to their objects.
 */
function routeFor(resourceType: string | undefined, resourceId: string | undefined): string | undefined {
  if (resourceType === undefined || resourceId === undefined || resourceId === "") return undefined;
  switch (resourceType) {
    case "native_market":
      return `/markets/${resourceId}`;
    case "agent":
      return `/agents/${resourceId}`;
    case "account":
      return "/settings/account";
    case "session":
      return "/settings/security";
    default:
      return undefined;
  }
}

export function Notifications(): ReactNode {
  const [unread, setUnread] = useState(false);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const list = useNotifications({ unread, ...(cursor === undefined ? {} : { cursor }) });
  const markAll = useMarkAllNotificationsRead();
  const toast = useToast();

  return (
    <Page
      title="Notifications"
      lead="What happened to this account, newest first. Figures are never in a notification; the pages they point at hold those."
      actions={
        <>
          <Button
            variant={unread ? "secondary" : "primary"}
            onClick={() => {
              setUnread(false);
              setCursor(undefined);
            }}
          >
            All
          </Button>
          <Button
            variant={unread ? "primary" : "secondary"}
            onClick={() => {
              setUnread(true);
              setCursor(undefined);
            }}
          >
            Unread only
          </Button>
        </>
      }
    >
      <Panel
        title={unread ? "Unread" : "Everything"}
        description="Each row is the server's own summary, rendered as it was written."
        actions={
          <Button
            variant="quiet"
            busy={markAll.isPending}
            busyLabel="Marking…"
            onClick={() => {
              markAll.mutate(undefined, {
                onSuccess: (updated) => {
                  toast.notify(
                    updated === 0
                      ? "There was nothing unread to mark."
                      : `Marked ${String(updated)} notifications read.`,
                  );
                },
              });
            }}
          >
            Mark all read
          </Button>
        }
      >
        {markAll.isError && (
          <Refused
            what="Marking every notification read"
            error={markAll.error}
            onRetry={() => {
              markAll.reset();
            }}
          />
        )}
        <AsyncPanel
          query={list}
          loadingLabel="Asking the backend for notifications…"
          skeleton={<Skeleton shape="rows" count={5} label="Notifications are loading" />}
          empty={{
            isEmpty: (page: NotificationList) => page.items.length === 0,
            title: unread ? "Nothing unread" : EMPTY_STATES.notifications.title,
            body: unread
              ? "Everything the backend has sent this account has been read."
              : EMPTY_STATES.notifications.body,
          }}
        >
          {(page: NotificationList) => (
            <div className="stack">
              {page.items.map((item) => (
                <Row key={item.id} notification={item} />
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

      <Preferences />

      <Disclosure title="How Nodal contacts you">
        <p>{CHANNEL_SENTENCE}</p>
      </Disclosure>
    </Page>
  );
}

/* -------------------------------------------------------------------------- */

function Row(props: { readonly notification: Notification }): ReactNode {
  const item = props.notification;
  const mark = useMarkNotificationRead();
  const read = item.read_at !== null && item.read_at !== undefined;
  const to = routeFor(item.resource_type, item.resource_id);

  return (
    <PanelCard
      title={item.title}
      description={item.body}
      {...(item.sandbox ? ({ temp: "simulated" } as const) : {})}
      actions={
        <>
          <StatusBadge tone={SEVERITY_TONE[item.severity] ?? "neutral"}>{item.severity}</StatusBadge>
          {!read && <StatusBadge tone="info">Unread</StatusBadge>}
        </>
      }
    >
      <p className="field-note">
        <span className="mono-small">{item.kind}</span> · {formatInstant(item.occurred_at)}
      </p>
      {mark.isError && (
        <Refused
          what="Marking this notification read"
          error={mark.error}
          onRetry={() => {
            mark.reset();
          }}
        />
      )}
      <div className="form-actions">
        {to !== undefined ? (
          <LinkButton to={to} variant="secondary">
            Open what this is about
          </LinkButton>
        ) : (
          item.resource_id !== undefined &&
          item.resource_id !== "" && (
            <IdentifierShort value={item.resource_id} what={item.resource_type ?? "reference"} />
          )
        )}
        {!read && (
          <Button
            variant="quiet"
            busy={mark.isPending}
            busyLabel="Marking…"
            onClick={() => {
              mark.mutate(item.id);
            }}
          >
            Mark read
          </Button>
        )}
      </div>
    </PanelCard>
  );
}

/* -------------------------------------------------------------------------- */

function Preferences(): ReactNode {
  const prefs = useNotificationPreferences();
  const update = useUpdateNotificationPreferences();
  const toast = useToast();

  return (
    <Panel
      title="What you are notified about"
      description="One answer per kind. The kinds that cannot be switched off say so rather than offering a control that does nothing."
    >
      {update.isError && (
        <Refused
          what="Saving this preference"
          error={update.error}
          onRetry={() => {
            update.reset();
          }}
        />
      )}
      <AsyncPanel
        query={prefs}
        loadingLabel="Asking the backend for notification preferences…"
        skeleton={<Skeleton shape="rows" count={6} label="Preferences are loading" />}
        empty={{
          isEmpty: (items: NotificationPreference[]) => items.length === 0,
          title: "No preferences to set",
          body: "The backend returned no notification kinds for this account, so there is nothing to switch.",
        }}
      >
        {(items: NotificationPreference[]) => (
          <div className="table-scroll" role="group" aria-label="Notification preferences" tabIndex={0}>
            <table>
              <caption className="visually-hidden">
                Every notification kind, whether it is switched on, and whether it can be switched off
              </caption>
              <thead>
                <tr>
                  <th scope="col">Kind</th>
                  <th scope="col">Channel</th>
                  <th scope="col">Send it</th>
                </tr>
              </thead>
              <tbody>
                {items.map((pref) => (
                  <tr key={pref.kind}>
                    <th scope="row">
                      <span className="mono-small">{pref.kind}</span>
                    </th>
                    <td>{pref.channel}</td>
                    <td>
                      {pref.enforced ? (
                        <input
                          type="checkbox"
                          checked={pref.enabled}
                          aria-label={`Send ${pref.kind} notifications`}
                          disabled={update.isPending}
                          onChange={(event) => {
                            const enabled = event.target.checked;
                            update.mutate([{ kind: pref.kind, enabled }], {
                              onSuccess: () => {
                                toast.notify("Preference saved.");
                              },
                            });
                          }}
                        />
                      ) : (
                        <span className="absent" title="This kind cannot be switched off.">
                          always sent
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </AsyncPanel>
      <p className="field-note">{CHANNEL_SENTENCE}</p>
      <p className="field-note">
        A kind marked <em>always sent</em> cannot be switched off: a new sign-in, an account
        restriction, a reversed purchase, a failed payout and a system message reach you whatever
        this page says.
      </p>
    </Panel>
  );
}

