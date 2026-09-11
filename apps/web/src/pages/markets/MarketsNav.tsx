/**
 * The two things `/markets` means.
 *
 * D-077 puts internal markets and the internal product marketplace under one
 * address, and they are genuinely two views of one idea: things other people
 * made, priced in Credits. So they get one navigation rather than two entries
 * in the rail that look unrelated.
 *
 * It is a `<nav>` of links and NOT the `Tabs` primitive, even though it is
 * drawn like one. The WAI-ARIA tab pattern describes panels inside one
 * document; these are two documents with two addresses, and a tab that changes
 * the URL is a link wearing a tab's clothes — a screen-reader user is told to
 * expect a panel and gets a navigation instead.
 */
import type { ReactNode } from "react";
import { NavLink } from "react-router-dom";

function tabClass({ isActive }: { isActive: boolean }): string {
  return isActive ? "tab tab-current" : "tab";
}

export function MarketsNav(): ReactNode {
  return (
    <nav className="tabs" aria-label="Markets">
      <NavLink to="/markets" end className={tabClass}>
        Internal markets
      </NavLink>
      <NavLink to="/markets/products" className={tabClass}>
        Products
      </NavLink>
    </nav>
  );
}
