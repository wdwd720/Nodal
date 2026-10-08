/**
 * Pieces the pages of the advanced area share. Subsystem sections are welcome
 * to use them; none of them renders a figure of its own.
 */
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { Button } from "../../components/Button.tsx";
import { EmptyState } from "../../components/DataState.tsx";
import { Page, Panel, Pill } from "../../components/Layout.tsx";
import { SimulatedNotice } from "../../components/SimulatedNotice.tsx";
import { modeTone, pageNumber, type CursorTrail } from "../../lib/advanced.ts";
import { advancedHref, useAdvancedAccount, useAdvancedSections } from "./area.ts";
import type { AdvancedSection } from "./registry.tsx";

/**
 * A wide table. It scrolls inside its own box rather than widening the page,
 * and the box is a named, focusable region so that a keyboard user can scroll
 * it as well as a pointer user can (WCAG 2.1.1); the global :focus-visible
 * rule draws its outline.
 */
export function ScrollTable(props: {
  readonly caption: string;
  readonly headers: readonly string[];
  readonly children: ReactNode;
}): ReactNode {
  return (
    <div className="table-scroll" role="region" aria-label={props.caption} tabIndex={0}>
      <table>
        <caption className="visually-hidden">{props.caption}</caption>
        <thead>
          <tr>
            {props.headers.map((header) => (
              <th key={header} scope="col">
                {header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{props.children}</tbody>
      </table>
    </div>
  );
}

/** Stable machine-readable codes, exactly as the backend sent them. */
export function Codes(props: { readonly codes: readonly string[]; readonly none?: string }): ReactNode {
  if (props.codes.length === 0) {
    return props.none === undefined ? null : <span className="absent">{props.none}</span>;
  }
  return (
    <ul className="codes">
      {props.codes.map((code, index) => (
        <li key={`${code}-${String(index)}`}>
          <code className="mono-small">{code}</code>
        </li>
      ))}
    </ul>
  );
}

/** An advanced mode. LIVE is drawn as a refusal wherever it appears. */
export function ModePill(props: { readonly mode: string }): ReactNode {
  return <Pill tone={modeTone(props.mode)}>{props.mode}</Pill>;
}

/** The value domain a response reported, as the backend said it. */
export function ValueDomainPill(props: { readonly value: string }): ReactNode {
  return <Pill tone="warn">value domain {props.value}</Pill>;
}

/** For an account-scoped page when there is no account to read. */
export function NoAccount(): ReactNode {
  const account = useAdvancedAccount();
  return (
    <EmptyState
      title="No account to read"
      body={
        account.canInspect
          ? "This session owns no account, so there is nothing of its own to show. As an operator you can ask to read an account by its id above; the backend decides whether you may."
          : "The backend returned no accounts for this session, so there is no account to read. Nothing is shown in place of an account's figures."
      }
    />
  );
}

/**
 * Paging for a newest-first list. The backend's cursors only go forward, so
 * "newer" walks back along the trail of cursors that led here, and "older"
 * exists only when the backend returned a cursor.
 */
export function CursorPager(props: {
  readonly noun: string;
  readonly trail: CursorTrail;
  readonly nextCursor: string | null;
  readonly onForward: (cursor: string) => void;
  readonly onBack: () => void;
}): ReactNode {
  const { nextCursor } = props;
  return (
    <div className="form-actions pager" role="group" aria-label={`Pages of ${props.noun}`}>
      {props.trail.previous.length > 0 ? (
        <Button onClick={props.onBack}>Newer {props.noun}</Button>
      ) : (
        <Button disabledReason="This is the first page; there is nothing newer to go back to.">
          Newer {props.noun}
        </Button>
      )}
      <span className="pager-position">Page {String(pageNumber(props.trail))}</span>
      {nextCursor !== null ? (
        <Button
          onClick={() => {
            props.onForward(nextCursor);
          }}
        >
          Older {props.noun}
        </Button>
      ) : (
        <Button disabledReason="The backend returned no cursor, so there is nothing older on this list.">
          Older {props.noun}
        </Button>
      )}
    </div>
  );
}

/** Every section of the area, linked, with what each one shows. */
export function SectionList(props: { readonly sections: readonly AdvancedSection[] }): ReactNode {
  return (
    <ul className="section-list">
      {props.sections.map((section) => (
        <li key={section.path}>
          <Link to={advancedHref(section.path)}>{section.title}</Link>
          <p className="section-description">{section.description}</p>
        </li>
      ))}
    </ul>
  );
}

/** An address inside /advanced that matches no section or page. */
export function AdvancedNotFound(): ReactNode {
  const sections = useAdvancedSections();
  return (
    <Page title="No such advanced page" lead="This address does not match any page of the advanced area.">
      <SimulatedNotice />
      <Panel title="Sections of the advanced area" description="Every section this build registers.">
        <SectionList sections={sections} />
      </Panel>
    </Page>
  );
}
