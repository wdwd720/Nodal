/**
 * The one door to the API.
 *
 * `@controlplane/generated-client` is generated from `openapi/openapi.yaml`, so
 * a contract change becomes a type error here rather than a surprise in
 * production. This module adds nothing to it except the origin.
 *
 * In development and in the browser tests the app is served from the API's
 * own origin through a proxy, so the origin is empty and every path is
 * relative. In the deployed tier the app is a static site on its own
 * hostname and the API is on another, under the same registrable domain: the
 * origin is baked in at build time (VITE_API_ORIGIN), the generated client
 * sends credentials, the session cookie travels because the two hosts are
 * same-site, and the API's CORS/CSRF allowlist names this app's origin.
 * Nothing about the session is held by the app in either case.
 */
import { createApiClient } from "@controlplane/generated-client";

/**
 * The API's origin, or empty for same-origin. Read once at build time; a
 * value with a path, a trailing slash or a non-https scheme is a deployment
 * mistake and is refused rather than repaired, because a wrong origin here is
 * every request going to the wrong place.
 */
function apiOrigin(): string {
  const raw = import.meta.env["VITE_API_ORIGIN"];
  if (raw === undefined || raw === "") return "";
  if (!/^https:\/\/[a-z0-9.-]+$/i.test(raw)) {
    throw new Error(`VITE_API_ORIGIN must be an https origin with no path: ${raw}`);
  }
  return raw;
}

export const API_ORIGIN = apiOrigin();

/** The API's base URL: absolute when the app is hosted apart from the API. */
export const API_BASE = `${API_ORIGIN}/v1`;

/** Where the backend mounts the login redirect. */
export const LOGIN_PATH = `${API_BASE}/auth/login`;

export const api = createApiClient({ baseUrl: API_BASE });

/**
 * Asks the API whether it is ready to serve: true only for a 200 from
 * /v1/readyz, which the API answers only once its database is reachable.
 * Used by the boot screen while a spun-down instance starts. This is the one
 * request outside the generated client, because readiness is not part of the
 * contract and must work before anything else does.
 */
export async function probeReady(): Promise<boolean> {
  try {
    const res = await fetch(`${API_BASE}/readyz`, { credentials: "omit", cache: "no-store" });
    return res.status === 200;
  } catch {
    return false;
  }
}

/**
 * Sends the browser to the identity provider. This is a full navigation rather
 * than an XHR because the flow ends in a redirect that sets a cookie.
 */
export function startSignIn(): void {
  window.location.assign(LOGIN_PATH);
}
