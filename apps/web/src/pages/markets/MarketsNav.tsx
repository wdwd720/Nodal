/**
 * The two things `/markets` means.
 *
 * D-077 puts internal markets and the internal product marketplace under one
 * address, and they are genuinely two views of one idea: things other people
 * made, priced in Credits. So they get one navigation rather than two entries
 * in the rail that look unrelated.
 *
 * It is a `<nav>` of links and NOT a tablist, even though it is drawn like
 * one. The WAI-ARIA tab pattern describes panels inside one document; these are
 * two documents with two addresses, and a tab that changes the URL is a link
 * wearing a tab's clothes — a screen-reader user is told to expect a panel and
 * gets a navigation instead. That argument is why the application never grew a
 * tablist, and why the `Tabs` primitive was deleted unused (D-113); these links
 * borrow its appearance from the stylesheet and nothing else.
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
