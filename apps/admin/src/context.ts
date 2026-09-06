/**
 * What every view is handed: the authority document, the session, a clock and
 * the two things a view must never invent for itself — how to report a failure
 * and how to ask for a fresh render.
 */
import type { AuthorityIndex } from "./authority.ts";
import type { Session } from "./session.ts";

export interface ViewContext {
  readonly authority: AuthorityIndex;
  readonly session: Session;
  /** Injected so views never read the wall clock directly. */
  readonly now: () => Date;
  /** Re-renders the current route. */
  readonly refresh: () => void;
  /** Navigates to a route. */
  readonly navigate: (route: string) => void;
  /** Reports a completed command or a failure to the operator. */
  readonly report: (message: HTMLElement) => void;
}

/** One route of the console. */
export interface Route {
  /** The URL fragment, without the leading '#'. */
  readonly path: string;
  readonly title: string;
  /** The surface id in the authority document that gates visibility. */
  readonly surface: string;
  readonly render: (ctx: ViewContext, root: HTMLElement) => Promise<void>;
  /** A one-line description shown under the heading. */
  readonly description: string;
}
