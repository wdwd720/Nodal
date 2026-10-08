/**
 * Pieces the strategy marketplace pages share. None of them renders a figure
 * of its own: amounts arrive as exact strings from the API and are only
 * formatted here.
 */
import { useState, type ReactNode } from "react";
import { newIdempotencyKey } from "@controlplane/generated-client";

import { Pill } from "../../../components/Layout.tsx";
import { Bps, DecimalValue } from "../../../components/Money.tsx";
import {
  MODERATION_MEANING,
  STATUS_MEANING,
  moderationTone,
  outcomeTone,
  statusTone,
  terminationTone,
  type StrategyModeration,
  type StrategyStatus,
} from "../../../lib/marketplace.ts";
import { advancedHref } from "../area.ts";

/** Where the section lives, and the pages inside it. */
export const STRATEGIES = "strategies";

export const strategyHref = {
  home: (): string => advancedHref(STRATEGIES),
  create: (): string => advancedHref(STRATEGIES, "new"),
  listing: (listingId: string): string => advancedHref(STRATEGIES, "listings", listingId),
  newVersion: (listingId: string): string => advancedHref(STRATEGIES, "listings", listingId, "versions", "new"),
  fork: (versionId: string): string => advancedHref(STRATEGIES, "versions", versionId, "fork"),
  installs: (): string => advancedHref(STRATEGIES, "installs"),
  runs: (): string => advancedHref(STRATEGIES, "runs"),
  review: (): string => advancedHref(STRATEGIES, "review"),
  intent: (intentId: string): string => advancedHref("activity", "intents", intentId),
};

/** A version's lifecycle status and moderation state, as the backend reported them. */
export function VersionState(props: { readonly status: string; readonly moderation: string }): ReactNode {
  const status = props.status as StrategyStatus;
  const moderation = props.moderation as StrategyModeration;
  return (
    <>
      <Pill tone={statusTone(props.status)} {...titleOf(STATUS_MEANING[status])}>
        {props.status}
      </Pill>{" "}
      <Pill tone={moderationTone(props.moderation)} {...titleOf(MODERATION_MEANING[moderation])}>
        {props.moderation}
      </Pill>
    </>
  );
}

function titleOf(meaning: string | undefined): { readonly title?: string } {
  return meaning === undefined ? {} : { title: meaning };
}

export function OutcomePill(props: { readonly outcome: string }): ReactNode {
  return <Pill tone={outcomeTone(props.outcome)}>{props.outcome}</Pill>;
}

export function TerminationPill(props: { readonly termination: string }): ReactNode {
  return <Pill tone={terminationTone(props.termination)}>{props.termination}</Pill>;
}

/** An exact decimal amount of simulated dollars. */
export function SimulatedAmount(props: { readonly value: string | undefined }): ReactNode {
  return (
    <>
      <DecimalValue value={props.value} /> <span className="cell-note">simulated dollars</span>
    </>
  );
}

/** Basis points as the backend computed them; a return may be negative. */
export function BasisPoints(props: { readonly value: number | undefined }): ReactNode {
  return <Bps value={props.value} />;
}

/**
 * One confirmed action. The request is built, with its idempotency key, once,
 * when the person confirms; every retry sends exactly that request again, and
 * editing what is being confirmed discards it so the next confirmation is a
 * new action with a new key.
 */
export function usePrepared<T>(): {
  readonly prepared: T | undefined;
  readonly prepare: (build: (idempotencyKey: string) => T) => T;
  readonly clear: () => void;
} {
  const [prepared, setPrepared] = useState<T | undefined>(undefined);
  return {
    prepared,
    prepare: (build) => {
      const next = build(newIdempotencyKey());
      setPrepared(next);
      return next;
    },
    clear: () => {
      setPrepared(undefined);
    },
  };
}
