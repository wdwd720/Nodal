/**
 * Tabs, following the WAI-ARIA pattern rather than approximating it.
 *
 * A roving tabindex, so one keypress leaves the tablist rather than walking
 * through every tab; arrow keys move between tabs and wrap; Home and End jump
 * to the ends; each panel is labelled by its own tab.
 *
 * What tabs are NOT for here: hiding a disclosure, a refusal, or a figure a
 * customer needs in order to decide. Anything the rules require to be visible
 * is visible, not behind a tab that starts unselected.
 */
import { useCallback, useId, useRef, useState, type ReactNode } from "react";

import { TabButton } from "./Button.tsx";
import { rovingTab } from "../lib/table.ts";

export interface Tab {
  readonly id: string;
  readonly label: string;
  readonly panel: ReactNode;
}

export function Tabs(props: {
  /** What this set of tabs selects between. The tablist's accessible name. */
  readonly label: string;
  readonly tabs: readonly Tab[];
  readonly initial?: string;
}): ReactNode {
  const base = useId();
  const first = props.tabs[0];
  const [selected, setSelected] = useState<string>(props.initial ?? first?.id ?? "");
  const listRef = useRef<HTMLDivElement | null>(null);

  const index = props.tabs.findIndex((tab) => tab.id === selected);
  const current = index < 0 ? 0 : index;

  const onKeyDown = useCallback(
    (key: string) => {
      const next = rovingTab(key, current, props.tabs.length);
      if (next === undefined) return;
      const target = props.tabs[next];
      if (target === undefined) return;
      setSelected(target.id);
      const button = listRef.current?.querySelectorAll("button")[next];
      if (button instanceof HTMLElement) button.focus();
    },
    [current, props.tabs],
  );

  const active = props.tabs[current];

  return (
    <div>
      <div className="tabs" role="tablist" aria-label={props.label} ref={listRef}>
        {props.tabs.map((tab) => (
          <TabButton
            key={tab.id}
            id={`${base}-${tab.id}-tab`}
            controls={`${base}-${tab.id}-panel`}
            selected={tab.id === active?.id}
            onSelect={() => {
              setSelected(tab.id);
            }}
            onKeyDown={onKeyDown}
          >
            {tab.label}
          </TabButton>
        ))}
      </div>
      {active !== undefined && (
        <div
          className="tab-panel"
          role="tabpanel"
          id={`${base}-${active.id}-panel`}
          aria-labelledby={`${base}-${active.id}-tab`}
          tabIndex={0}
        >
          {active.panel}
        </div>
      )}
    </div>
  );
}
