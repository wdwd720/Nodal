/**
 * The frame every signed-in page sits in.
 *
 * Two shapes, chosen in JavaScript rather than hidden with CSS, so that only
 * one navigation exists in the document at a time. A duplicate landmark that is
 * merely invisible is still a duplicate landmark to a screen reader, and a link
 * that is merely invisible is still a link to a keyboard.
 *
 *   768px and above — a left rail with the five destinations, and a masthead
 *   holding the stream badge, the primary actions and the account menu.
 *
 *   below 768px — a bottom bar with the same five destinations at 44px each,
 *   and the account menu in a sheet.
 *
 * # The five destinations, and why the rail is no longer grouped
 *
 * The previous shell grouped the rail into Real capital / Nodal Economy /
 * Simulated capital, because the product then held three kinds of value that
 * must never be summed and the grouping was the design doing the same work the
 * disclosures do. D-077 removed the hosted rail: this product has one kind of
 * value, Credits, and the simulation surfaces went with the pages that fed
 * them. Keeping the group headings would now be a structure that describes a
 * product that no longer exists — three labels over one pot — so the rail is
 * five destinations and no headings. The temperatures still do their work on
 * every panel; they simply have nothing left to keep apart in the navigation.
 *
 * # Controls that are not here
 *
 * The rule this codebase enforces with a type is that a control which does
 * nothing is a defect. So:
 *
 *   - **Search** is absent: the markets search does not exist yet.
 *   - **Notifications** is here now that `GET /v1/me/notifications/unread-count`
 *     exists. The bell shows the count and nothing else: a notification carries
 *     identifiers and state names, never a balance, and the page it opens is
 *     another branch's — so until that page lands the bell is a read-out rather
 *     than a link, which is the honest shape for a control with nowhere to go.
 *   - **Buy Credits** and **Withdraw** are declared below and rendered only
 *     when their pages exist. `USER_JOURNEY.md` §2 requires them to be always
 *     visible — and they will be — but a primary action that navigates to a
 *     404 is worse than one that has not arrived. Turning each on is one word:
 *     `present: false` becomes `present: true` in `PRIMARY_ACTIONS`.
 *
 * The same is true of every entry in `DESTINATIONS` and `ACCOUNT_LINKS`.
 */
import { useEffect, useState, type ReactNode } from "react";
import { Link, NavLink } from "react-router-dom";

import { useSignOut, useUnreadCount } from "../api/queries.ts";
import { RISK_FOOTER } from "../lib/honesty.ts";
import { useSession } from "../session.tsx";
import { useVersion } from "../api/queries.ts";
import { BrandLockup } from "./Brand.tsx";
import { Button, IconButton, LinkButton } from "./Button.tsx";
import { StatusBadge } from "./StatusBadge.tsx";
import { Dialog, Sheet } from "./Dialog.tsx";
import { Field, FieldGrid } from "./Field.tsx";
import { StreamBadge, useEventStream } from "./StreamStatus.tsx";
import { ToastProvider } from "./Toast.tsx";

export interface NavItem {
  readonly to: string;
  readonly label: string;
  /**
   * Whether a page answers this route in this build.
   *
   * It is a property of the shell rather than a lookup against the router
   * because the router is assembled from several branches: the shell has to be
   * able to say "not yet" about a page it does not own without importing it.
   */
  readonly present: boolean;
}

/**
 * D-077's five destinations, in order. Every one of them has a page.
 */
export const DESTINATIONS: readonly NavItem[] = [
  { to: "/home", label: "Home", present: true },
  { to: "/markets", label: "Markets", present: true },
  { to: "/agents", label: "Agents", present: true },
  { to: "/portfolio", label: "Portfolio", present: true },
  { to: "/activity", label: "Activity", present: true },
];

/**
 * The two actions a customer starts from.
 *
 * Both are owned by other branches. Flip `present` to true as each lands.
 */
export const PRIMARY_ACTIONS: readonly NavItem[] = [
  { to: "/buy-credits", label: "Buy Credits", present: true },
  { to: "/withdraw", label: "Withdraw", present: false },
];

/** What the account menu offers. Sign out is a real action and is always there. */
export const ACCOUNT_LINKS: readonly NavItem[] = [
  { to: "/settings", label: "Settings", present: true },
  { to: "/settings/security", label: "Security", present: true },
  { to: "/notifications", label: "Notifications", present: true },
];

/** Sections reachable from the rail that are not one of the five. */
export const SECONDARY_LINKS: readonly NavItem[] = [
  { to: "/markets/products", label: "Products", present: true },
  { to: "/create-asset", label: "Create asset", present: true },
];

function available(items: readonly NavItem[]): readonly NavItem[] {
  return items.filter((item) => item.present);
}

/**
 * Every section a customer can reach. The 404 page lists these, so it can only
 * ever offer routes that exist.
 */
export const NAV_ITEMS: readonly NavItem[] = [
  ...available(DESTINATIONS),
  ...available(SECONDARY_LINKS),
  ...available(PRIMARY_ACTIONS),
  ...available(ACCOUNT_LINKS),
];

const WIDE = "(min-width: 768px)";

/** Tracks a media query. The shell is one shape or the other, never both. */
function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState<boolean>(() => window.matchMedia(query).matches);

  useEffect(() => {
    const list = window.matchMedia(query);
    const onChange = (): void => {
      setMatches(list.matches);
    };
    onChange();
    list.addEventListener("change", onChange);
    return () => {
      list.removeEventListener("change", onChange);
    };
  }, [query]);

  return matches;
}

function navClass({ isActive }: { isActive: boolean }): string {
  return isActive ? "nav-link nav-link-active" : "nav-link";
}

function Sections(props: { readonly onNavigate?: () => void }): ReactNode {
  const secondary = available(SECONDARY_LINKS);
  return (
    <nav className="nav" aria-label="Sections">
      <ul>
        {available(DESTINATIONS).map((item) => (
          <li key={item.to}>
            <NavLink
              to={item.to}
              end={item.to === "/markets"}
              className={navClass}
              {...(props.onNavigate === undefined ? {} : { onClick: props.onNavigate })}
            >
              {item.label}
            </NavLink>
          </li>
        ))}
      </ul>
      {secondary.length > 0 && (
        <div className="nav-group">
          <p className="eyebrow" id="nav-group-more">
            Also
          </p>
          <ul aria-labelledby="nav-group-more">
            {secondary.map((item) => (
              <li key={item.to}>
                <NavLink
                  to={item.to}
                  className={navClass}
                  {...(props.onNavigate === undefined ? {} : { onClick: props.onNavigate })}
                >
                  {item.label}
                </NavLink>
              </li>
            ))}
          </ul>
        </div>
      )}
    </nav>
  );
}

/**
 * The account menu.
 *
 * It is a `Dialog`, which becomes a bottom sheet under 768px, rather than a
 * bespoke popover: the native `<dialog>` traps focus, closes on Escape, makes
 * the rest of the document inert and restores focus on close, and none of those
 * is worth reimplementing badly for a menu with three items.
 */
function AccountMenu(props: { readonly open: boolean; readonly onClose: () => void }): ReactNode {
  const session = useSession();
  const signOut = useSignOut();
  const links = available(ACCOUNT_LINKS);

  return (
    <Dialog open={props.open} title="Account" onClose={props.onClose}>
      {session.principal !== undefined && (
        <FieldGrid columns={2}>
          <Field label="Signed in as" note="The subject the identity provider asserted.">
            <span className="mono-small">{session.principal.subject_id}</span>
          </Field>
          <Field label="Roles" note="Asserted by the operator directory, never by a token claim.">
            <span className="mono-small">{session.principal.roles.join(", ")}</span>
          </Field>
        </FieldGrid>
      )}
      <ul className="link-list">
        {links.map((item) => (
          <li key={item.to}>
            <LinkButton to={item.to}>{item.label}</LinkButton>
          </li>
        ))}
      </ul>
      <div className="form-actions">
        <Button
          variant="danger"
          busy={signOut.isPending}
          busyLabel="Signing out…"
          onClick={() => {
            signOut.mutate(undefined, {
              // The backend revokes the session and clears the cookie; a full
              // reload is what makes the app ask again from nothing, rather
              // than keeping a cache that belongs to a session that is gone.
              onSettled: () => {
                window.location.assign("/");
              },
            });
          }}
        >
          Sign out
        </Button>
      </div>
    </Dialog>
  );
}


/**
 * How many notifications are unread.
 *
 * A count is not money, so it may be rendered as a plain number. Everything a
 * notification is ABOUT is refetched from its own resource before it is shown
 * as a figure, which is the rule the whole realtime layer is built on.
 *
 * While the count is loading it renders nothing at all rather than a zero: "no
 * unread notifications" and "I have not asked yet" are different facts, and a
 * zero that turns into a seven is the small dishonesty this codebase spends its
 * effort refusing.
 */
function NotificationBell(props: { readonly enabled: boolean }): ReactNode {
  const unread = useUnreadCount(props.enabled);
  const page = ACCOUNT_LINKS.find((item) => item.to === "/notifications");
  const count = unread.data;

  if (count === undefined) return null;

  const label =
    count === 0
      ? "Notifications: none unread"
      : `Notifications: ${String(count)} unread`;

  // A link once the page exists; until then a labelled read-out, because a
  // control that navigates nowhere is the defect this file exists to prevent.
  if (page?.present === true) {
    return (
      <LinkButton to={page.to} variant="quiet">
        {count === 0 ? "Notifications" : `Notifications (${String(count)})`}
      </LinkButton>
    );
  }

  return (
    <StatusBadge tone={count === 0 ? "neutral" : "info"} title="The notifications page is not part of this build yet.">
      {label}
    </StatusBadge>
  );
}

/**
 * The standing sandbox statement.
 *
 * `GET /v1/version` says whether this deployment is a sandbox tier; the client
 * never infers it from the environment name, because a build that guessed would
 * label the wrong deployment — and the only thing worse than an unlabelled
 * rehearsal is a real deployment labelled as one. An absent flag means the API
 * did not say, and that is not "sandbox" either.
 *
 * It is part of the document rather than a dismissible banner. A rehearsal a
 * customer can dismiss is a rehearsal they will forget they are in.
 */
function SandboxLine(props: { readonly sandbox: boolean | undefined }): ReactNode {
  if (props.sandbox !== true) return null;
  return (
    <p className="sandbox-line" role="note">
      <span className="sandbox-word">Sandbox</span>
      <span>
        Credits, verification and payouts here are rehearsals; nothing moves real value.
      </span>
    </p>
  );
}

export function AppShell(props: { readonly children: ReactNode }): ReactNode {
  const session = useSession();
  const version = useVersion();
  const stream = useEventStream(session.signedIn);
  const wide = useMediaQuery(WIDE);
  const [menuOpen, setMenuOpen] = useState(false);
  const [accountOpen, setAccountOpen] = useState(false);
  const actions = available(PRIMARY_ACTIONS);

  return (
    <ToastProvider>
      <div className="shell">
        <a className="skip-link" href="#main">
          Skip to main content
        </a>

        {wide && (
          <div className="sidebar">
            <BrandLockup />
            <Sections />
          </div>
        )}

        <div className="app-body">
          <SandboxLine sandbox={version.data?.sandbox_tier} />
          <header className="masthead">
            {wide ? (
              <div className="masthead-meta">
                <StreamBadge status={stream} />
                {version.data !== undefined && (
                  <span className="mono-small env-tag">
                    {version.data.environment} · build {version.data.build_version}
                  </span>
                )}
              </div>
            ) : (
              <div className="masthead-meta">
                <IconButton
                  label="Open the section list"
                  expanded={menuOpen}
                  onClick={() => {
                    setMenuOpen(true);
                  }}
                >
                  <span aria-hidden="true">☰</span>
                </IconButton>
                <BrandLockup compact />
              </div>
            )}
            <div className="masthead-meta">
              {!wide && <StreamBadge status={stream} />}
              <NotificationBell enabled={session.signedIn} />
              {actions.map((action, index) => (
                <LinkButton
                  key={action.to}
                  to={action.to}
                  variant={index === 0 ? "primary" : "secondary"}
                >
                  {action.label}
                </LinkButton>
              ))}
              <IconButton
                label="Open the account menu"
                expanded={accountOpen}
                onClick={() => {
                  setAccountOpen(true);
                }}
              >
                <span aria-hidden="true">Account</span>
              </IconButton>
            </div>
          </header>

          <main id="main" tabIndex={-1}>
            {props.children}
          </main>

          <footer className="footer">
            <p>{RISK_FOOTER}</p>
            <p>
              <Link to="/terms">Terms</Link> · <Link to="/privacy">Privacy</Link> ·{" "}
              <Link to="/risk">Risk disclosure</Link>
            </p>
            {session.principal !== undefined && (
              <p className="mono-small">
                signed in as {session.principal.subject_id} · {session.principal.actor_type} ·{" "}
                {session.principal.roles.join(", ")}
              </p>
            )}
          </footer>

          {!wide && (
            <nav className="bottom-nav" aria-label="Primary">
              <ul>
                {available(DESTINATIONS).map((item) => (
                  <li key={item.to}>
                    <NavLink to={item.to} end={item.to === "/markets"}>
                      {item.label}
                    </NavLink>
                  </li>
                ))}
              </ul>
            </nav>
          )}
        </div>

        {!wide && (
          <Sheet
            open={menuOpen}
            title="Sections"
            onClose={() => {
              setMenuOpen(false);
            }}
          >
            <Sections
              onNavigate={() => {
                setMenuOpen(false);
              }}
            />
          </Sheet>
        )}

        <AccountMenu
          open={accountOpen}
          onClose={() => {
            setAccountOpen(false);
          }}
        />
      </div>
    </ToastProvider>
  );
}
