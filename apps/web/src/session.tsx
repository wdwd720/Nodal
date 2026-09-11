/**
 * Who is signed in, which account the pages are reading, and how the app leaves
 * for the identity provider and comes back.
 *
 * The principal and the account list both come from the backend on every load;
 * neither is remembered across a reload, because the only thing that decides
 * what a session may see is the session cookie the backend issued. The chosen
 * account is kept in memory alone for the same reason — a stale account id in
 * storage would be a way to show one customer another customer's heading.
 *
 * The one thing this module does keep in the tab is where the customer was
 * going, and it keeps it because signing in is a full-page navigation that
 * destroys every value in the tab (see `lib/survives-sign-in.ts`).
 */
import { createContext, useContext, useMemo, useState, type ReactNode } from "react";

import { LOGIN_PATH } from "./api/client.ts";
import { useAccounts, useMe, type Account, type Principal } from "./api/queries.ts";
import { isUnauthenticated } from "./api/problem.ts";
import { isLocalPath, markSignInStarted, rememberReturnPath } from "./lib/survives-sign-in.ts";

export interface SessionValue {
  readonly principal: Principal | undefined;
  readonly accounts: readonly Account[];
  readonly activeAccountId: string | undefined;
  readonly setActiveAccountId: (id: string) => void;
  readonly signedIn: boolean;
  /**
   * True only when the BACKEND said this session is not signed in (401).
   *
   * It is separate from `!signedIn` on purpose. "I could not ask" and "the
   * answer was no" are different facts, and only the second one justifies
   * telling a customer they are signed out — which is a claim about their
   * session, not about the network.
   */
  readonly signedOut: boolean;
  readonly loading: boolean;
  /** Present when the principal or account list could not be read. */
  readonly error: unknown;
  readonly refetch: () => void;
}

const SessionContext = createContext<SessionValue | undefined>(undefined);

export function SessionProvider(props: { readonly children: ReactNode }): ReactNode {
  const me = useMe();
  const signedIn = me.isSuccess;
  const signedOut = me.isError && isUnauthenticated(me.error);
  const accounts = useAccounts(signedIn);
  const [chosen, setChosen] = useState<string | undefined>(undefined);

  const value = useMemo<SessionValue>(() => {
    const list = accounts.data ?? [];
    const active = chosen !== undefined && list.some((a) => a.id === chosen) ? chosen : list[0]?.id;
    return {
      principal: me.data,
      accounts: list,
      activeAccountId: active,
      setActiveAccountId: setChosen,
      signedIn,
      signedOut,
      loading: me.isPending || (signedIn && accounts.isPending),
      error: me.isError && !isUnauthenticated(me.error) ? me.error : accounts.error,
      refetch: () => {
        void me.refetch();
        void accounts.refetch();
      },
    };
  }, [me, accounts, chosen, signedIn, signedOut]);

  return <SessionContext.Provider value={value}>{props.children}</SessionContext.Provider>;
}

export function useSession(): SessionValue {
  const value = useContext(SessionContext);
  if (value === undefined) {
    throw new Error("useSession must be used inside SessionProvider");
  }
  return value;
}

/**
 * The account the page is reading. Pages call this rather than reaching for
 * `accounts[0]` so that "no account yet" is a state they must handle.
 */
export function useActiveAccountId(): string | undefined {
  return useSession().activeAccountId;
}

/**
 * Leaves for the identity provider.
 *
 * This is a full navigation rather than a request, because the flow ends in a
 * redirect that sets a cookie — an XHR could not receive it. Everything the app
 * wants to survive the trip is written to the tab first:
 *
 *   - the return path, validated as a local path, so the app can forward the
 *     customer back to what they were doing. The API's login endpoint takes no
 *     `return_to` parameter today, so the browser holds it;
 *   - a marker saying a sign-in is in flight, so the callback landing on `/`
 *     shows "signing you in" rather than flashing the public landing page at
 *     somebody who has just signed in.
 *
 * `stepUp` asks the provider for a stronger authentication. It is the same
 * endpoint with `?step_up=true`, which is what `STEP_UP_REQUIRED` needs.
 */
export function beginSignIn(options?: {
  readonly returnTo?: string;
  readonly stepUp?: boolean;
}): void {
  const returnTo = options?.returnTo;
  if (returnTo !== undefined && isLocalPath(returnTo)) {
    rememberReturnPath(returnTo);
  }
  markSignInStarted();
  const target = options?.stepUp === true ? `${LOGIN_PATH}?step_up=true` : LOGIN_PATH;
  window.location.assign(target);
}

/**
 * The path to send a signed-out visitor to, carrying where they were going.
 *
 * `?return=` is read back by the sign-in page and by nothing else, and it is
 * re-validated there, so a hand-edited value cannot become a redirect to
 * another origin.
 */
export function signInPathFor(location: { readonly pathname: string; readonly search: string }): string {
  const target = `${location.pathname}${location.search}`;
  const safe = isLocalPath(target) ? target : "/home";
  return `/sign-in?return=${encodeURIComponent(safe)}`;
}
