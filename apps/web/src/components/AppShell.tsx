/**
 * The frame every page sits in.
 *
 * Two shapes, chosen in JavaScript rather than hidden with CSS, so that only
 * one navigation exists in the document at a time. A duplicate landmark that is
 * merely invisible is still a duplicate landmark to a screen reader, and a link
 * that is merely invisible is still a link to a keyboard.
 *
 *   768px and above — a left rail carrying every section, grouped so the three
 *   kinds of value stay apart, with the masthead holding the two actions a
 *   customer starts from.
 *
 *   below 768px — a bottom bar with the five destinations a thumb reaches for
 *   at 44px each, and the full section list one press away in a sheet.
 *
 * Accessibility here is structural rather than decorative: a skip link ahead of
 * everything, one `<nav>` with `aria-current` on the active link, a single
 * `<main>` that can be focused, and a footer risk statement that is part of the
 * document rather than a dismissible banner.
 *
 * The navigation is grouped so that Real Capital, the Nodal Economy and
 * Simulated Capital are adjacent but never merged into one "balance"
 * destination. A single entry point would be the first step toward a single
 * total, and a single total across those three is true of nothing.
 *
 * WHAT IS DELIBERATELY NOT HERE: a search field and a notification bell. There
 * is no endpoint behind either one in this API, and a control that does nothing
 * is the same defect as a button that does nothing — the rule this codebase
 * enforces with a type. They belong in the shell the day they have something to
 * do.
 */
import { useEffect, useState, type ReactNode } from "react";
import { NavLink } from "react-router-dom";

import { RISK_FOOTER } from "../lib/honesty.ts";
import { useSession } from "../session.tsx";
import { useVersion } from "../api/queries.ts";
import { BrandLockup } from "./Brand.tsx";
import { IconButton, LinkButton } from "./Button.tsx";
import { Sheet } from "./Dialog.tsx";
import { StreamBadge, useEventStream } from "./StreamStatus.tsx";
import { ToastProvider } from "./Toast.tsx";

export interface NavItem {
  readonly to: string;
  readonly label: string;
}

export interface NavGroup {
  readonly label: string;
  readonly items: readonly NavItem[];
}

/**
 * The sections, grouped by what kind of value they are about.
 *
 * The group names are the design doing the same work the disclosures do: a
 * customer reading down this rail is told, before they click anything, that
 * Credits and dollars and replays live in three different places.
 */
export const NAV_GROUPS: readonly NavGroup[] = [
  {
    label: "Overview",
    items: [
      { to: "/", label: "Home" },
      { to: "/activity", label: "Activity" },
    ],
  },
  {
    label: "Real capital",
    items: [
      { to: "/add-funds", label: "Add funds" },
      { to: "/trade", label: "Trade" },
      { to: "/portfolio", label: "Portfolio" },
    ],
  },
  {
    label: "Nodal Economy",
    items: [
      { to: "/nodal-economy", label: "Nodal Economy" },
      { to: "/marketplace", label: "Marketplace" },
      { to: "/native-markets", label: "Native Markets" },
      { to: "/create-asset", label: "Create asset" },
      { to: "/payouts", label: "Payouts" },
    ],
  },
  {
    label: "Agents and simulation",
    items: [
      { to: "/strategy", label: "Strategy builder" },
      { to: "/agents", label: "Agents" },
      { to: "/lab", label: "Lab" },
    ],
  },
  {
    label: "Account",
    items: [{ to: "/settings", label: "Settings and security" }],
  },
];

/** Every section, flattened. The 404 page lists these. */
export const NAV_ITEMS: readonly NavItem[] = NAV_GROUPS.flatMap((group) => group.items);

/** The five a thumb reaches for. Every one is a real route. */
const BOTTOM_ITEMS: readonly NavItem[] = [
  { to: "/", label: "Home" },
  { to: "/trade", label: "Trade" },
  { to: "/portfolio", label: "Portfolio" },
  { to: "/activity", label: "Activity" },
  { to: "/settings", label: "Settings" },
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
  return (
    <nav className="nav" aria-label="Sections">
      {NAV_GROUPS.map((group) => {
        const headingId = `nav-group-${group.label.replace(/\s+/g, "-").toLowerCase()}`;
        return (
          <div className="nav-group" key={group.label}>
            <p className="eyebrow" id={headingId}>
              {group.label}
            </p>
            <ul aria-labelledby={headingId}>
              {group.items.map((item) => (
                <li key={item.to}>
                  <NavLink
                    to={item.to}
                    end={item.to === "/"}
                    className={navClass}
                    {...(props.onNavigate === undefined ? {} : { onClick: props.onNavigate })}
                  >
                    {item.label}
                  </NavLink>
                </li>
              ))}
            </ul>
          </div>
        );
      })}
    </nav>
  );
}

export function AppShell(props: { readonly children: ReactNode }): ReactNode {
  const session = useSession();
  const version = useVersion();
  const stream = useEventStream(session.signedIn);
  const wide = useMediaQuery(WIDE);
  const [menuOpen, setMenuOpen] = useState(false);

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
              <LinkButton to="/add-funds" variant="primary">
                Add funds
              </LinkButton>
              <LinkButton to="/payouts">Withdraw</LinkButton>
            </div>
          </header>

          <main id="main" tabIndex={-1}>
            {props.children}
          </main>

          <footer className="footer">
            <p>{RISK_FOOTER}</p>
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
                {BOTTOM_ITEMS.map((item) => (
                  <li key={item.to}>
                    <NavLink to={item.to} end={item.to === "/"}>
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
      </div>
    </ToastProvider>
  );
}
