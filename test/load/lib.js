// Shared helpers for the k6 load scripts (PART 154). Every scenario targets the
// versioned REST API through the same session cookie a browser would hold.
//
// Environment:
//   BASE_URL   API origin including /v1 (default http://127.0.0.1:8080/v1)
//   SESSION    value of the session cookie for a LOCAL dev identity
//   ACCOUNT_ID account the session may act on
//
// Results are only meaningful when documented with the exact commit, host and
// database size they were measured on; never report numbers from a laptop as
// production capacity.
import http from "k6/http";
import { check } from "k6";

export const BASE_URL = __ENV.BASE_URL || "http://127.0.0.1:8080/v1";
export const ACCOUNT_ID = __ENV.ACCOUNT_ID || "";
const SESSION = __ENV.SESSION || "";
const COOKIE_NAME = __ENV.COOKIE_NAME || "__Host-cp_session";

export function params(extra = {}) {
  const headers = Object.assign(
    {
      Accept: "application/json, application/problem+json",
      // __VU/__ITER exist only in the VU stage; setup() and teardown() run
      // without them and referencing them there throws ReferenceError.
      "X-Request-Id": `k6-${typeof __VU === "undefined" ? "s" : __VU}-${typeof __ITER === "undefined" ? "s" : __ITER}`,
      // CSRF here is Fetch Metadata, not a token: internal/auth/httpmw/csrf.go
      // admits a request whose Sec-Fetch-Site is "same-origin" or "none", or
      // whose Origin is allow-listed. A browser sets this header itself and k6
      // does not, so without it every mutating request is refused 403
      // "cross-site request refused" and the scenario measures the CSRF guard
      // instead of the endpoint. Sending it is accurate emulation of the
      // same-origin caller these scenarios represent, not a bypass — the guard
      // still rejects anything that claims a cross-site origin.
      "Sec-Fetch-Site": "same-origin",
    },
    extra.headers || {},
  );
  const p = Object.assign({}, extra, { headers });
  if (SESSION) {
    p.cookies = Object.assign({}, extra.cookies || {}, { [COOKIE_NAME]: SESSION });
  }
  return p;
}

export function get(path, tag) {
  const res = http.get(`${BASE_URL}${path}`, params({ tags: { name: tag || path } }));
  check(res, {
    "status is 2xx": (r) => r.status >= 200 && r.status < 300,
    "no 5xx": (r) => r.status < 500,
  });
  return res;
}

export function postIdempotent(path, body, key, tag) {
  const res = http.post(
    `${BASE_URL}${path}`,
    JSON.stringify(body),
    params({ headers: { "Content-Type": "application/json", "Idempotency-Key": key }, tags: { name: tag || path } }),
  );
  check(res, {
    "no 5xx": (r) => r.status < 500,
    "problem+json on rejection": (r) => r.status < 400 || (r.headers["Content-Type"] || "").startsWith("application/problem+json"),
  });
  return res;
}

export function uuid() {
  // RFC 4122 v4 from k6's PRNG; only used as an idempotency key.
  return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    return (c === "x" ? r : (r & 0x3) | 0x8).toString(16);
  });
}

export function requireAccount() {
  if (!ACCOUNT_ID) {
    throw new Error("ACCOUNT_ID is required for this scenario");
  }
  return ACCOUNT_ID;
}
