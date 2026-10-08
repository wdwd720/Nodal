/**
 * What every section of the advanced area can rely on, provided by
 * AdvancedArea: which account the account-scoped sections read, and the list
 * of sections itself.
 *
 * Both are React context rather than imports so that a section can list its
 * siblings (the overview does) without importing the registry that imports it.
 */
import { createContext, useContext } from "react";

import type { AdvancedSection } from "./registry.tsx";

/** Where the area lives in the application's routes. */
export const ADVANCED_BASE = "/advanced";

/** The address of a section, or of a page inside one. */
export function advancedHref(...segments: readonly string[]): string {
  return [ADVANCED_BASE, ...segments].join("/");
}

export interface AdvancedAccount {
  /**
   * The account the account-scoped sections read: the one an operator is
   * inspecting, otherwise the session's active account — the same account
   * every other page of the application reads. Undefined when there is none.
   */
  readonly accountId: string | undefined;
  /** The session's own active account. Writes may only ever act on an account the principal owns. */
  readonly ownAccountId: string | undefined;
  /** True while an operator is reading an account by id. */
  readonly inspecting: boolean;
  /**
   * True when the principal is an operator, who may ask to read any account.
   * Asking is all the interface does: whether the account may be read is the
   * backend's decision, and its answer — rows or a refusal — is what is shown.
   */
  readonly canInspect: boolean;
}

export const AdvancedAccountContext = createContext<AdvancedAccount | undefined>(undefined);

/** The account the advanced pages read. Only valid inside AdvancedArea. */
export function useAdvancedAccount(): AdvancedAccount {
  const value = useContext(AdvancedAccountContext);
  if (value === undefined) {
    throw new Error("useAdvancedAccount must be used inside AdvancedArea");
  }
  return value;
}

export const AdvancedSectionsContext = createContext<readonly AdvancedSection[]>([]);

/** Every section of the area, in sub-navigation order. */
export function useAdvancedSections(): readonly AdvancedSection[] {
  return useContext(AdvancedSectionsContext);
}
