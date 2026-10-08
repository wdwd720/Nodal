/**
 * Pieces the equities pages share. None of them computes a figure: a price
 * is the decimal string the backend sent, an amount is base units drawn at
 * the decimals of the asset the backend named, a date is the New York trade
 * or settlement date exactly as sent, and a refusal is drawn with its code —
 * and, when the backend recorded the refused order, a link to it.
 *
 * The command machinery (one confirmed request and its idempotency key, the
 * confirm button, the inputs) is the order-book section's, shared rather
 * than copied.
 */
import type { ReactNode } from "react";
import { Link, NavLink } from "react-router-dom";
import { ApiProblem } from "@controlplane/generated-client";

import type { EquitySessionState } from "../../../api/advanced/equities.ts";
import { explain } from "../../../api/problem.ts";
import { Button } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { Field, FieldGrid, type Tone } from "../../../components/Layout.tsx";
import { SESSION_LABEL, refusalField } from "../../../lib/equities.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref } from "../area.ts";

export {
  CheckField,
  ChoiceField,
  CommandControls,
  KeyLine,
  Recorded,
  StatusPill,
  TextField,
  Units,
  useConfirmation,
  type Confirmation,
} from "../orders/parts.tsx";

/** Where the section lives under /advanced. */
export const EQUITIES_PATH = "equities";

export function equitiesHref(...segments: readonly string[]): string {
  return advancedHref(EQUITIES_PATH, ...segments);
}

/** The section's own pages. */
export function EquitiesNav(): ReactNode {
  const links: ReadonlyArray<readonly [string, string, boolean]> = [
    [equitiesHref(), "Listings", true],
    [equitiesHref("orders"), "My orders", false],
    [equitiesHref("fills"), "Fills", false],
    [equitiesHref("settlements"), "Settlements", false],
    [equitiesHref("corporate-actions"), "Corporate actions", false],
    [equitiesHref("calendar"), "Calendar", false],
  ];
  return (
    <nav className="subnav" aria-label="Equities pages">
      <ul>
        {links.map(([to, label, end]) => (
          <li key={to}>
            <NavLink
              to={to}
              end={end}
              className={({ isActive }) => (isActive ? "subnav-link subnav-link-active" : "subnav-link")}
            >
              {label}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}

export function equityOrderTone(status: string): Tone {
  switch (status) {
    case "FILLED":
      return "good";
    case "OPEN":
    case "ACCEPTED":
      return "info";
    case "REJECTED":
      return "bad";
    default:
      return "neutral";
  }
}

export function settlementTone(state: string): Tone {
  return state === "SETTLED" ? "good" : "warn";
}

/** The session in force, as the backend computed it for the instant it names. */
export function SessionFields(props: { readonly session: EquitySessionState }): ReactNode {
  const { session } = props;
  return (
    <FieldGrid columns={3}>
      <Field label="Session" note={SESSION_LABEL[session.session]}>
        {session.session}
        {session.holiday_name !== undefined && <span className="cell-note">holiday: {session.holiday_name}</span>}
      </Field>
      <Field label="Trade date" note={`${session.calendar} calendar, ${session.timezone}.`}>
        {session.trade_date}
        <span className="cell-note">{session.trading_day ? "a trading day" : "not a trading day"}</span>
      </Field>
      <Field label="A trade today settles on" note="T+1: one trading day after the trade date.">
        {session.settlement_date ?? <span className="absent">no trading today</span>}
      </Field>
      <Field label="Next trading day">{session.next_trading_day}</Field>
      <Field label="As of">
        {formatInstant(session.as_of)} <span className="mono-small">({exactInstant(session.as_of)})</span>
      </Field>
    </FieldGrid>
  );
}

/**
 * A command the backend did not carry out: its refusal with the code and the
 * request id, the order and intent it recorded when it recorded them, and a
 * retry under the same key only when trying again unchanged could pass.
 */
export function EquityCommandFailure(props: {
  readonly error: unknown;
  readonly onRetry: () => void;
  readonly onStartOver: () => void;
}): ReactNode {
  const detail = explain(props.error);
  const fields = props.error instanceof ApiProblem ? props.error.fields : undefined;
  const orderId = refusalField(fields, "order_id");
  const intentId = refusalField(fields, "intent_id");
  const rejection = refusalField(fields, "rejection_code");
  return (
    <Explanation error={props.error}>
      {rejection !== undefined && (
        <p>
          Rejection: <code className="mono-small">{rejection}</code>
        </p>
      )}
      {orderId !== undefined && (
        <p>
          The backend recorded the refused order, which stays readable:{" "}
          <Link to={equitiesHref("orders", orderId)}>
            <code className="mono-small">{orderId}</code>
          </Link>
          .
        </p>
      )}
      {intentId !== undefined && (
        <p>
          Its economic intent:{" "}
          <Link to={advancedHref("activity", "intents", intentId)}>
            <code className="mono-small">{intentId}</code>
          </Link>
          .
        </p>
      )}
      <div className="form-actions">
        {detail.retryable ? (
          <Button onClick={props.onRetry}>Retry with the same key</Button>
        ) : (
          <Button disabledReason="The backend gave a definite answer; the same key would only replay it.">
            Retry with the same key
          </Button>
        )}
        <Button variant="quiet" onClick={props.onStartOver}>
          Start over
        </Button>
      </div>
    </Explanation>
  );
}

/** The failure renderer the equities commands pass to CommandControls. */
export function renderEquityFailure(error: unknown, onRetry: () => void, onStartOver: () => void): ReactNode {
  return <EquityCommandFailure error={error} onRetry={onRetry} onStartOver={onStartOver} />;
}
