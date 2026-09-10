/**
 * Dense, sortable, accessible.
 *
 * A market table is the one place in this product where density earns its
 * keep — a reader is comparing rows, and rows they cannot see at once they
 * cannot compare. Everything else about the table is a rule about not lying:
 *
 *   - a `<caption>`, visually hidden, saying what the table is AND naming any
 *     column that is a risk measure, because "impact" in a header does not tell
 *     a screen-reader user that the column is the one that can cost them;
 *   - `scope="col"` on every header, so a cell is read with its column;
 *   - `aria-sort` on the sorted column, alongside a glyph, so the sort is both
 *     announced and visible;
 *   - horizontal overflow scrolls INSIDE this container. The page body never
 *     scrolls sideways, at any width;
 *   - the scroll container is focusable and named, because a region a mouse can
 *     scroll and a keyboard cannot is unreachable;
 *   - row height comes from the temperature, so density follows the kind of
 *     value rather than the whim of the screen;
 *   - full keyboard operation: arrows move a row, Home and End jump, Enter
 *     opens. Rows are only focusable when there is something to open — a focus
 *     stop that does nothing is a dead control with extra steps.
 *
 * The five states are the caller's to choose between: pass `rows` for present,
 * `empty` for empty, and render `Skeleton`, `Explanation` or `Refusal` in place
 * of the table for the other three. A table that renders its own error state
 * cannot be told apart from one that has no rows.
 */
import { useCallback, useRef, useState, type KeyboardEvent, type ReactNode } from "react";

import { applySort, nextSort, rovingIndex, type SortState } from "../lib/table.ts";
import { SortButton } from "./Button.tsx";
import { EmptyState } from "./DataState.tsx";

export interface Column<T> {
  /** Stable key. Also the sort identity. */
  readonly key: string;
  readonly header: string;
  /** Right-align the column. Figures line up; words do not need to. */
  readonly numeric?: boolean;
  /**
   * True when this column is a risk measure — price impact, slippage,
   * concentration, exposure. Named in the caption so it is not a surprise.
   */
  readonly riskMeasure?: boolean;
  /** Supplying a comparator makes the column sortable. */
  readonly compare?: (a: T, b: T) => number;
  readonly cell: (row: T) => ReactNode;
}

export interface DataTableProps<T> {
  /** What this table is. Visually hidden, always present. */
  readonly caption: string;
  readonly columns: ReadonlyArray<Column<T>>;
  readonly rows: readonly T[];
  readonly rowKey: (row: T) => string;
  /** Opening a row is a real action; supplying it makes rows focusable. */
  readonly onOpenRow?: (row: T) => void;
  /**
   * Bound the height so the header actually sticks. A sticky header only
   * sticks against something that scrolls, so a long table opts in and a
   * six-row table does not pretend to.
   */
  readonly tall?: boolean;
  readonly empty?: { readonly title: string; readonly body: string };
}

export function DataTable<T>(props: DataTableProps<T>): ReactNode {
  const [sortKey, setSortKey] = useState<string | undefined>(undefined);
  const [sortState, setSortState] = useState<SortState>("none");
  const [focused, setFocused] = useState(0);
  const bodyRef = useRef<HTMLTableSectionElement | null>(null);

  const sortedColumn = props.columns.find((column) => column.key === sortKey);
  const rows = applySort(props.rows, sortState, sortedColumn?.compare);

  const toggle = useCallback(
    (key: string) => {
      setSortKey((previous) => (previous === key ? previous : key));
      setSortState((previous) => (sortKey === key ? nextSort(previous) : "ascending"));
    },
    [sortKey],
  );

  const interactive = props.onOpenRow !== undefined;
  const { onOpenRow } = props;

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLTableSectionElement>): void => {
      if (!interactive) return;
      if (event.key === "Enter" || event.key === " ") {
        const row = rows[focused];
        if (row !== undefined && onOpenRow !== undefined) {
          event.preventDefault();
          onOpenRow(row);
        }
        return;
      }
      const next = rovingIndex(event.key, focused, rows.length);
      if (next === undefined) return;
      event.preventDefault();
      setFocused(next);
      const element = bodyRef.current?.querySelectorAll("tr")[next];
      if (element instanceof HTMLElement) element.focus();
    },
    [focused, interactive, onOpenRow, rows],
  );

  const riskColumns = props.columns.filter((column) => column.riskMeasure === true);
  const caption =
    riskColumns.length === 0
      ? props.caption
      : `${props.caption}. Risk measures in this table: ${riskColumns
          .map((column) => column.header)
          .join(", ")}.`;

  if (rows.length === 0 && props.empty !== undefined) {
    return <EmptyState title={props.empty.title} body={props.empty.body} />;
  }

  return (
    <div
      className={props.tall === true ? "table-scroll table-scroll-tall" : "table-scroll"}
      role="group"
      aria-label={props.caption}
      tabIndex={0}
    >
      <table>
        <caption className="visually-hidden">{caption}</caption>
        <thead>
          <tr>
            {props.columns.map((column) => {
              const sorted = column.key === sortKey ? sortState : "none";
              return (
                <th
                  key={column.key}
                  scope="col"
                  className={column.numeric === true ? "cell-num" : undefined}
                  {...(column.compare === undefined ? {} : { "aria-sort": sorted })}
                >
                  {column.compare === undefined ? (
                    column.header
                  ) : (
                    <SortButton
                      sort={sorted}
                      onClick={() => {
                        toggle(column.key);
                      }}
                    >
                      {column.header}
                    </SortButton>
                  )}
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody ref={bodyRef} onKeyDown={onKeyDown}>
          {rows.map((row, index) => (
            <tr
              key={props.rowKey(row)}
              {...(interactive
                ? {
                    tabIndex: index === focused ? 0 : -1,
                    onFocus: () => {
                      setFocused(index);
                    },
                    onClick: () => {
                      if (onOpenRow !== undefined) onOpenRow(row);
                    },
                  }
                : {})}
            >
              {props.columns.map((column) => (
                <td
                  key={column.key}
                  className={column.numeric === true ? "cell-num" : undefined}
                >
                  {column.cell(row)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
