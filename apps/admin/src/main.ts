/**
 * Bootstrap.
 *
 * Three things must be true before any operator surface renders, and each
 * failure is reported as itself rather than as a blank page:
 *
 *   1. the generated authority document loads and is the version this console
 *      understands — without it the console would be guessing at permissions;
 *   2. there is a session — otherwise the operator is sent to sign in;
 *   3. the session is an OPERATOR. A customer session reaching this app is not
 *      an error to hide: it is told plainly that this is the operator console.
 */
import { ApiProblem, getMe } from "./api.ts";
import { mount } from "./app.ts";
import { fetchAuthority } from "./authority.ts";
import { append, clear, el, notice } from "./dom.ts";
import { isOperator, toSession } from "./session.ts";

const root = document.getElementById("root");

function fatal(...body: Array<HTMLElement | string>): void {
  if (!root) return;
  clear(root);
  const box = el("div", { class: "fatal" });
  append(box, ...body);
  append(root, box);
}

function signInLink(label: string): HTMLElement {
  return el("a", { class: "button primary", href: "/v1/auth/login" }, label);
}

async function start(): Promise<void> {
  if (!root) return;

  let authority;
  try {
    authority = await fetchAuthority();
  } catch (err) {
    fatal(
      notice(
        "error",
        `The authority document could not be loaded, so this console cannot tell what you are permitted to do and will not guess. ${
          err instanceof Error ? err.message : String(err)
        }`,
      ),
      el("p", { class: "muted" }, "Rebuild with `node build.mjs`; it copies src/generated/ into dist/."),
    );
    return;
  }

  let me;
  try {
    me = await getMe();
  } catch (err) {
    if (err instanceof ApiProblem && err.status === 401) {
      fatal(notice("info", "You are not signed in."), signInLink("Sign in"));
      return;
    }
    fatal(
      notice("error", `Could not read your identity: ${err instanceof Error ? err.message : String(err)}`),
      signInLink("Try signing in"),
    );
    return;
  }

  const session = toSession(me);
  if (!isOperator(session)) {
    fatal(
      notice(
        "warn",
        `This is the operator console. Your session is a ${session.principal.actorType} principal, which has no operator surface here.`,
      ),
      el("p", { class: "muted" }, "Customer functions live in the customer application, not in this one."),
    );
    return;
  }

  mount({ authority, session, now: () => new Date(), root });
}

void start();
