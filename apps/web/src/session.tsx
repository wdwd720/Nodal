/**
 * Who is signed in, and which account the pages are reading.
 *
 * The principal and the account list both come from the backend on every load;
 * neither is remembered across a reload, because the only thing that decides
 * what a session may see is the session cookie the backend issued. The chosen
 * account is kept in memory alone for the same reason — a stale account id in
 * storage would be a way to show one customer another customer's heading.
 */
import { createContext, useContext, useMemo, useState, type ReactNode } from "react";

import { useAccounts, useMe, type Account, type Principal } from "./api/queries.ts";
import { isUnauthenticated } from "./api/problem.ts";

export interface SessionValue {
  readonly principal: Principal | undefined;
  readonly accounts: readonly Account[];
  readonly activeAccountId: string | undefined;
  readonly setActiveAccountId: (id: string) => void;
  readonly signedIn: boolean;
  readonly loading: boolean;
  /** Present when the principal or account list could not be read. */
  readonly error: unknown;
  readonly refetch: () => void;
}

const SessionContext = createContext<SessionValue | undefined>(undefined);

export function SessionProvider(props: { readonly children: ReactNode }): ReactNode {
  const me = useMe();
  const signedIn = me.isSuccess;
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
      loading: me.isPending || (signedIn && accounts.isPending),
      error: me.isError && !isUnauthenticated(me.error) ? me.error : accounts.error,
      refetch: () => {
        void me.refetch();
        void accounts.refetch();
      },
    };
  }, [me, accounts, chosen, signedIn]);

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
