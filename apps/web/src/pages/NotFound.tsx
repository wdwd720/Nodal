import type { ReactNode } from "react";

import { LinkButton } from "../components/Button.tsx";
import { NAV_ITEMS } from "../components/AppShell.tsx";
import { Page, Panel } from "../components/Layout.tsx";

export function NotFound(): ReactNode {
  return (
    <Page title="No such page" lead="This address does not match any screen in the application.">
      <Panel title="Where you can go" description="Every section of the app.">
        <ul className="link-list">
          {NAV_ITEMS.map((item) => (
            <li key={item.to}>
              <LinkButton to={item.to}>{item.label}</LinkButton>
            </li>
          ))}
        </ul>
      </Panel>
    </Page>
  );
}
