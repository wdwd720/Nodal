import { defineConfig, devices } from "@playwright/test";

/**
 * Critical-path end-to-end tests (STAGE 14 exit criteria).
 *
 * The suite runs against a production build served by `vite preview`, which
 * proxies `/v1` and `/auth` to a real `api` process talking to a real Postgres.
 * Nothing is stubbed: the assertions below are about what the backend actually
 * answers, including the places where it answers "no".
 *
 * Browser: `channel: "chrome"` uses the Chrome already installed on this host.
 * Playwright's own Chromium build could not be fetched here (its CDN times out),
 * and running against a real browser is closer to what a customer uses than
 * running against nothing.
 */
const WEB_PORT = 5273;
const API_URL = process.env["CP_WEB_API_TARGET"] ?? "http://127.0.0.1:18099";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env["CI"],
  retries: process.env["CI"] ? 1 : 0,
  reporter: [["list"]],
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: `http://127.0.0.1:${String(WEB_PORT)}`,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    channel: "chrome",
  },
  projects: [
    { name: "setup", testMatch: /auth\.setup\.ts/ },
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"], channel: "chrome", storageState: ".playwright/state.json" },
      dependencies: ["setup"],
    },
  ],
  webServer: [
    {
      // The API must already be running with a seeded database; see README.
      command: "node -e \"process.exit(0)\"",
      url: `${API_URL}/v1/healthz`,
      reuseExistingServer: true,
      timeout: 15_000,
    },
    {
      command: "pnpm run build && pnpm run preview",
      url: `http://127.0.0.1:${String(WEB_PORT)}/`,
      reuseExistingServer: !process.env["CI"],
      timeout: 180_000,
      stdout: "pipe",
    },
  ],
});
