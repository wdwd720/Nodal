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

const proxy = {
  "/v1": { target: apiTarget, changeOrigin: false },
  // The development identity-provider picker is mounted outside /v1.
  "/auth": { target: apiTarget, changeOrigin: false },
} as const;

export default defineConfig({
  plugins: [react()],
  server: {
    host: "127.0.0.1",
    port: 5273,
    strictPort: true,
    proxy,
  },
  preview: {
    host: "127.0.0.1",
    port: 5273,
    strictPort: true,
    proxy,
  },
  build: {
    target: "es2022",
    sourcemap: true,
  },
});
