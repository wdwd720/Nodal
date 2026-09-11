import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

/**
 * The web app is served from the same origin as the API in every environment,
 * so the session cookie is a first-party cookie and no CORS relaxation is ever
 * needed. In development the dev server reproduces that by proxying the two
 * API-owned prefixes to the local api binary; the browser only ever sees one
 * origin, exactly as it will behind the production reverse proxy.
 */
const apiTarget = process.env["CP_WEB_API_TARGET"] ?? "http://127.0.0.1:18099";

/**
 * The port this app is served on, overridable so that two checkouts can run
 * their browser suites at the same time.
 *
 * `strictPort` is deliberate: a dev server that silently moves to the next free
 * port is a dev server the end-to-end suite connects to the wrong one of. The
 * port therefore has to be chosen explicitly, and `CP_WEB_PORT` is how.
 */
const DEFAULT_WEB_PORT = 5273;
const webPort = (() => {
  const raw = process.env["CP_WEB_PORT"];
  if (raw === undefined || raw === "") return DEFAULT_WEB_PORT;
  const parsed = Number.parseInt(raw, 10);
  if (!Number.isInteger(parsed) || parsed <= 0) {
    throw new Error(`CP_WEB_PORT must be a positive integer: ${raw}`);
  }
  return parsed;
})();

const proxy = {
  "/v1": { target: apiTarget, changeOrigin: false },
  // The development identity-provider picker is mounted outside /v1.
  "/auth": { target: apiTarget, changeOrigin: false },
} as const;

export default defineConfig({
  plugins: [react()],
  server: {
    host: "127.0.0.1",
    port: webPort,
    strictPort: true,
    proxy,
  },
  preview: {
    host: "127.0.0.1",
    port: webPort,
    strictPort: true,
    proxy,
  },
  build: {
    target: "es2022",
    /**
     * "hidden", not true.
     *
     * The maps are still WRITTEN — a stack trace from a production error can be
     * symbolicated by whoever holds `dist/` — but no `//# sourceMappingURL=`
     * comment is emitted, so a browser never fetches one and the CDN never
     * serves the app's readable source to a visitor. `true` published the whole
     * frontend, comments included, beside the bundle.
     */
    sourcemap: "hidden",
  },
});
