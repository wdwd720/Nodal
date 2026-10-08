/**
 * Pieces the stable asset pages share (and the bridge pages borrow). None of
 * them computes a figure: every amount is a string the stable asset
 * controller computed, rendered through the Money components, and every
 * command goes through CommandFlow, which creates its idempotency key when
 * the user confirms and reuses it for every retry.
 */
import { useState, type ReactNode } from "react";
import { NavLink } from "react-router-dom";
import { newIdempotencyKey } from "@controlplane/generated-client";
import type { UseMutationResult } from "@tanstack/react-query";

import type { StablePrice, StableReconciliation } from "../../../api/advanced/stable.ts";
import type { Asset } from "../../../api/queries.ts";
import { Button, type ButtonVariant } from "../../../components/Button.tsx";
import { Explanation } from "../../../components/DataState.tsx";
import { Identifier, Pill } from "../../../components/Layout.tsx";
import { Absent, DecimalValue } from "../../../components/Money.tsx";
import {
  RECONCILIATION_STATE_MEANING,
  confirmAttempt,
  outcomeLabel,
  outcomeTone,
  reconciliationTone,
  type ConfirmedAttempt,
  type RequestOutcome,
} from "../../../lib/stable.ts";
import { EFFECT_STATUS_MEANING, effectStatusTone } from "../../../lib/advanced.ts";
import { formatInstant } from "../../../lib/time.ts";
import { stableHref } from "./paths.ts";

/* --------------------------------------------------------------------------
 * Navigation
 * ------------------------------------------------------------------------ */

const STABLE_PAGES: ReadonlyArray<{ readonly path: string; readonly label: string }> = [
  { path: "", label: "Stable assets" },
  { path: "issuances", label: "Issuances" },
  { path: "redemptions", label: "Redemptions" },
  { path: "operator", label: "Operator" },
];

/** The section's own pages. */
export function StableNav(): ReactNode {
  return (
    <nav className="subnav" aria-label="Stable asset pages">
      <ul>
        {STABLE_PAGES.map((page) => (
          <li key={page.path}>
            <NavLink
              end
              to={page.path === "" ? stableHref() : stableHref(page.path)}
              className={({ isActive }) => (isActive ? "subnav-link subnav-link-active" : "subnav-link")}
            >
              {page.label}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}

/* --------------------------------------------------------------------------
 * Assets and prices
 * ------------------------------------------------------------------------ */

/** The registry's symbol for an asset id, when the registry lists it. */
export function symbolOf(assets: readonly Asset[] | undefined, id: string): string | undefined {
  const wanted = id.toLowerCase();
  return assets?.find((asset) => asset.id.toLowerCase() === wanted)?.symbol;
}

/** The registry's simulated assets, in symbol order. Which of them may serve is the backend's decision. */
export function simulatedAssets(assets: readonly Asset[] | undefined): Asset[] {
  return (assets ?? [])
    .filter((asset) => asset.kind === "SIMULATED" && asset.status === "ACTIVE")
    .sort((a, b) => (a.symbol < b.symbol ? -1 : a.symbol > b.symbol ? 1 : 0));
}

/**
 * A price of one whole unit exactly as the backend stated it: the decimal, the
 * quote asset (by symbol when known), its source and when it was observed.
 */
export function PriceLine(props: { readonly price: StablePrice | undefined; readonly quote?: string; readonly absent?: string }): ReactNode {
  if (props.price === undefined) return <Absent what={props.absent ?? "no fresh price"} />;
  const { price } = props;
  return (
    <span>
      <DecimalValue value={price.decimal} />{" "}
      {props.quote === undefined ? <code className="mono-small">{price.quote}</code> : props.quote}
      <span className="cell-note">
        source {price.source}, observed {formatInstant(price.observed_at)}
      </span>
    </span>
  );
}

/* --------------------------------------------------------------------------
 * Outcomes and reconciliation
 * ------------------------------------------------------------------------ */

/** What a request's status says about its outcome; UNKNOWN is drawn as unknown, never as success or failure. */
export function OutcomePill(props: { readonly outcome: RequestOutcome; readonly done: string }): ReactNode {
  return <Pill tone={outcomeTone(props.outcome)}>{outcomeLabel(props.outcome, props.done)}</Pill>;
}

/** A request's reconciliation state and its economic effect, as the backend reports them. */
export function ReconciliationLine(props: { readonly reconciliation: StableReconciliation }): ReactNode {
  const r = props.reconciliation;
  return (
    <div data-field="reconciliation">
      <p>
        <Pill tone={reconciliationTone(r.state)}>reconciliation {r.state}</Pill>{" "}
        <Pill tone={effectStatusTone(r.effect_status)}>effect {r.effect_status}</Pill>
      </p>
      <p className="field-note">{RECONCILIATION_STATE_MEANING[r.state]}</p>
      <p className="field-note">{EFFECT_STATUS_MEANING[r.effect_status]}</p>
      {r.unknown_since !== undefined && <p className="cell-note">outcome unknown since {formatInstant(r.unknown_since)}</p>}
      {r.resolved_at !== undefined && <p className="cell-note">settled from the chain at {formatInstant(r.resolved_at)}</p>}
    </div>
  );
}

/** Who recorded something, exactly as the backend named them. */
export function ActorLine(props: { readonly actor: { readonly actor_type: string; readonly actor_id: string } }): ReactNode {
  return (
    <span>
      {props.actor.actor_type} <code className="mono-small">{props.actor.actor_id}</code>
    </span>
  );
}

/** A transaction id on a simulated chain. */
export function ChainTx(props: { readonly chain: string; readonly txId: string | undefined }): ReactNode {
  if (props.txId === undefined || props.txId === "") return <Absent what="no transaction yet" />;
  return (
    <span>
      <span className="mono-small">{props.chain}</span> <Identifier value={props.txId} />
    </span>
  );
}

/* --------------------------------------------------------------------------
 * Commands
 * ------------------------------------------------------------------------ */

/** What a form currently describes: a request it could send, or why it cannot. */
export type Prepared<R> = { readonly ok: true; readonly request: R } | { readonly ok: false; readonly reason: string };

/** A mutation of one confirmed attempt; the attempt carries its idempotency key. */
export type Command<R, T> = UseMutationResult<T, unknown, ConfirmedAttempt<R>>;

/**
 * Review, confirm, and the outcome of one command.
 *
 * The idempotency key is created when the user confirms and not before; the
 * attempt keeps it, and "Try again" sends the same request with the same key,
 * so a retry can never apply twice — the backend answers it as the request it
 * already saw. A changed request is a new attempt with a new key.
 */
export function CommandFlow<R, T>(props: {
  readonly label: string;
  readonly prepared: Prepared<R>;
  readonly describe: (request: R) => ReactNode;
  readonly command: Command<R, T>;
  readonly renderResult: (result: T) => ReactNode;
  /** What a refusal leaves unchanged, said plainly. */
  readonly refusedNote: string;
  readonly variant?: ButtonVariant;
}): ReactNode {
  const [review, setReview] = useState<R | undefined>(undefined);
  const [attempt, setAttempt] = useState<ConfirmedAttempt<R> | undefined>(undefined);
  const { command } = props;
  const sent = attempt !== undefined && (command.isPending || command.isError || command.isSuccess);

  const confirm = (request: R): void => {
    const next = confirmAttempt(attempt, request, newIdempotencyKey);
    setAttempt(next);
    setReview(undefined);
    command.mutate(next);
  };

  const finish = (): void => {
    setAttempt(undefined);
    setReview(undefined);
    command.reset();
  };

  if (sent && attempt !== undefined) {
    return (
      <div className="command">
        <div className="command-request">{props.describe(attempt.request)}</div>
        <p className="mono-small">idempotency key {attempt.idempotencyKey}</p>
        {command.isPending && <p className="loading">Sending to the backend…</p>}
        {command.isError && (
          <>
            <Explanation error={command.error}>
              <p>{props.refusedNote}</p>
            </Explanation>
            <div className="form-actions">
              <Button
                onClick={() => {
                  command.mutate(attempt);
                }}
              >
                Try again with the same key
              </Button>
              <Button variant="quiet" onClick={finish}>
                Change the request
              </Button>
            </div>
            <p className="field-note">
              Trying again sends the same request with the same key, so it cannot apply twice; a refusal the backend
              recorded for that key is answered the same way. Changing the request starts a new attempt with a new key.
            </p>
          </>
        )}
        {command.isSuccess && (
          <>
            {props.renderResult(command.data)}
            <div className="form-actions">
              <Button variant="quiet" onClick={finish}>
                Done
              </Button>
            </div>
          </>
        )}
      </div>
    );
  }

  if (review !== undefined) {
    return (
      <div className="command">
        <p className="command-title">Review what will be sent</p>
        <div className="command-request">{props.describe(review)}</div>
        <div className="form-actions">
          <Button
            variant={props.variant ?? "primary"}
            onClick={() => {
              confirm(review);
            }}
          >
            Confirm: {props.label}
          </Button>
          <Button
            variant="quiet"
            onClick={() => {
              setReview(undefined);
            }}
          >
            Change
          </Button>
        </div>
        <p className="field-note">
          Confirming creates this request's idempotency key; it is kept for any retry of the same request.
        </p>
      </div>
    );
  }

  return (
    <div className="form-actions">
      {props.prepared.ok ? (
        <Button
          variant="secondary"
          onClick={() => {
            if (props.prepared.ok) setReview(props.prepared.request);
          }}
        >
          Review: {props.label}
        </Button>
      ) : (
        <Button variant="secondary" disabledReason={props.prepared.reason}>
          Review: {props.label}
        </Button>
      )}
    </div>
  );
}

/** A replayed answer, said plainly so nobody mistakes it for a second action. */
export function ReplayNote(props: { readonly replay: boolean }): ReactNode {
  if (!props.replay) return null;
  return (
    <p className="callout" role="status">
      The backend had already recorded this request under the same idempotency key; this is that request, and nothing
      was submitted twice.
    </p>
  );
}
