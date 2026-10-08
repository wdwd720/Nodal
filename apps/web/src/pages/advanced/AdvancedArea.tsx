/**
 * The advanced (sandbox) area, mounted at /advanced/* (ADR-0020).
 *
 * Everything under this route is value domain SIMULATED and runs in PAPER,
 * SANDBOX or DEVNET mode only. The area is drawn from the section registry:
 * a sub-navigation link and a nested route per entry, and `/advanced` itself
 * opens the overview.
 *
 * Account-scoped sections read the account the rest of the application reads
 * (the session's active account). An operator may additionally ask to read an
 * account by id; the interface only asks — whether the account may be read is
 * the backend's decision (account:read_any), and its answer, rows or a
 * refusal, is exactly what the sections show. The choice lives in memory
 * alone, like the session's own account choice, and ends when the area is
 * left.
 */
import { useId, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Navigate, NavLink, Route, Routes } from "react-router-dom";

import { Button } from "../../components/Button.tsx";
import { isCanonicalUuid } from "../../lib/advanced.ts";
import { useSession } from "../../session.tsx";
import {
  AdvancedAccountContext,
  AdvancedSectionsContext,
  advancedHref,
  type AdvancedAccount,
} from "./area.ts";
import { ADVANCED_DEFAULT_SECTION, ADVANCED_SECTIONS } from "./registry.tsx";
import { AdvancedNotFound } from "./shared.tsx";

export function AdvancedArea(): ReactNode {
  const session = useSession();
  const [inspected, setInspected] = useState<string | undefined>(undefined);
  const operator = session.principal?.actor_type === "OPERATOR";

  const account = useMemo<AdvancedAccount>(
    () => ({
      accountId: inspected ?? session.activeAccountId,
      ownAccountId: session.activeAccountId,
      inspecting: inspected !== undefined,
      canInspect: operator,
    }),
    [inspected, session.activeAccountId, operator],
  );

  return (
    <AdvancedSectionsContext.Provider value={ADVANCED_SECTIONS}>
      <AdvancedAccountContext.Provider value={account}>
        <div className="advanced-area">
          {/* Not "… sections": Playwright matches accessible names by substring,
              and the main navigation is located by the name "Sections". */}
          <nav className="subnav" aria-label="Advanced area">
            <ul>
              {ADVANCED_SECTIONS.map((section) => (
                <li key={section.path}>
                  <NavLink
                    to={advancedHref(section.path)}
                    className={({ isActive }) => (isActive ? "subnav-link subnav-link-active" : "subnav-link")}
                  >
                    {section.title}
                  </NavLink>
                </li>
              ))}
            </ul>
          </nav>

          <AccountScope account={account} onInspect={setInspected} />

          <Routes>
            <Route index element={<Navigate to={advancedHref(ADVANCED_DEFAULT_SECTION)} replace />} />
            {ADVANCED_SECTIONS.map((section) => (
              <Route key={section.path} path={`${section.path}/*`} element={section.element} />
            ))}
            <Route path="*" element={<AdvancedNotFound />} />
          </Routes>
        </div>
      </AdvancedAccountContext.Provider>
    </AdvancedSectionsContext.Provider>
  );
}

/** Which account the account-scoped sections read, and, for an operator, a way to read another. */
function AccountScope(props: {
  readonly account: AdvancedAccount;
  readonly onInspect: (accountId: string | undefined) => void;
}): ReactNode {
  const { account } = props;
  const [draft, setDraft] = useState("");
  const [problem, setProblem] = useState<string | undefined>(undefined);
  const inputId = useId();
  const problemId = useId();

  const submit = (event: FormEvent<HTMLFormElement>): void => {
    event.preventDefault();
    const candidate = draft.trim();
    if (!isCanonicalUuid(candidate)) {
      setProblem("An account id is a UUID in its canonical 36-character form.");
      return;
    }
    setProblem(undefined);
    props.onInspect(candidate.toLowerCase());
  };

  return (
    <section className="account-scope" aria-label="Account the advanced sections read">
      <p className="account-scope-line">
        {account.accountId === undefined ? (
          "Account-scoped sections have no account to read: the backend returned none for this session."
        ) : (
          <>
            Account-scoped sections read <code className="mono-small">{account.accountId}</code>
            {account.inspecting
              ? " — an account you asked to inspect; the backend decides whether it may be read."
              : " — the account the rest of the application reads."}
          </>
        )}
      </p>
      {account.canInspect && (
        <form className="inspect-form" onSubmit={submit} noValidate>
          <div className="filter">
            <label htmlFor={inputId}>Inspect an account by id</label>
            <input
              id={inputId}
              className="input"
              value={draft}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={problem !== undefined}
              {...(problem === undefined ? {} : { "aria-describedby": problemId })}
              onChange={(event) => {
                setDraft(event.target.value);
              }}
            />
          </div>
          <Button submit>Inspect</Button>
          {account.inspecting && (
            <Button
              onClick={() => {
                setDraft("");
                setProblem(undefined);
                props.onInspect(undefined);
              }}
            >
              {account.ownAccountId === undefined ? "Stop inspecting" : "Back to your account"}
            </Button>
          )}
          {problem !== undefined && (
            <p className="field-error" id={problemId} role="alert">
              {problem}
            </p>
          )}
        </form>
      )}
    </section>
  );
}
