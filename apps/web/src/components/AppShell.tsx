/**
 * The frame every page sits in.
 *
 * Accessibility here is structural rather than decorative (PART 113): a skip
 * link ahead of the navigation, one `<nav>` with `aria-current` on the active
 * link, a single `<main>` that receives focus target, and a footer risk
 * statement that is part of the document rather than a dismissible banner.
 */
import type { ReactNode } from "react";
import { NavLink } from "react-router-dom";

import { RISK_FOOTER } from "../lib/honesty.ts";
import { useSession } from "../session.tsx";
import { useVersion } from "../api/queries.ts";
import { StreamBadge, useEventStream } from "./StreamStatus.tsx";

export const NAV_ITEMS: ReadonlyArray<{ readonly to: string; readonly label: string }> = [
  { to: "/", label: "Home" },
  { to: "/add-funds", label: "Add funds" },
  { to: "/trade", label: "Trade" },
  { to: "/portfolio", label: "Portfolio" },
  { to: "/strategy", label: "Strategy builder" },
  { to: "/agents", label: "Agents" },
  { to: "/lab", label: "Lab" },
  { to: "/activity", label: "Activity" },
  { to: "/settings", label: "Settings and security" },
];

export function AppShell(props: { readonly children: ReactNode }): ReactNode {
  const session = useSession();
  const version = useVersion();
  const stream = useEventStream(session.signedIn);

  return (
    <div className="shell">
      <a className="skip-link" href="#main">
        Skip to main content
      </a>
      <header className="masthead">
        <div className="brand">
          <span className="brand-mark" aria-hidden="true" />
          <span className="brand-name">Nodal</span>
        </div>
        <div className="masthead-meta">
          <StreamBadge status={stream} />
          {version.data !== undefined && (
            <span className="mono-small env-tag">
              {version.data.environment} · build {version.data.build_version}
            </span>
          )}
        </div>
      </header>

      <nav className="nav" aria-label="Sections">
        <ul>
          {NAV_ITEMS.map((item) => (
            <li key={item.to}>
              <NavLink
                to={item.to}
                end={item.to === "/"}
                className={({ isActive }) => (isActive ? "nav-link nav-link-active" : "nav-link")}
              >
                {item.label}
              </NavLink>
            </li>
          ))}
        </ul>
      </nav>

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
    </div>
  );
}
