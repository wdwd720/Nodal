/**
 * The one door to the API.
 *
 * `@controlplane/generated-client` is generated from `openapi/openapi.yaml`, so
 * a contract change becomes a type error here rather than a surprise in
 * production. This module adds nothing to it except the base URL: the app is
 * served from the same origin as the API in every environment, which is what
 * makes the session cookie a first-party cookie and lets the browser attach
 * `Sec-Fetch-Site: same-origin` to the unsafe methods the backend's CSRF check
 * requires.
 */
import { createApiClient } from "@controlplane/generated-client";

/** Same-origin base path. Never an absolute URL: cross-origin would break the session. */
export const API_BASE = "/v1";

/** Where the backend mounts the login redirect. */
export const LOGIN_PATH = `${API_BASE}/auth/login`;

export const api = createApiClient({ baseUrl: API_BASE });

/**
 * Sends the browser to the identity provider. This is a full navigation rather
 * than an XHR because the flow ends in a redirect that sets a cookie.
 */
export function startSignIn(): void {
  window.location.assign(LOGIN_PATH);
}
