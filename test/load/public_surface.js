// public_surface.js — load over the endpoints reachable without a session.
//
// This measures the parts of the stack every request pays for regardless of who
// is calling: routing, the middleware chain (request id, correlation id, secure
// headers, access log, rate limiting, panic recovery), the problem+json error
// contract, and — for readyz — a real database round trip.
//
// It was written when no authenticated scenario could run at all, because the
// dev identity provider's advertised login URL had no handler mounted. That is
// fixed and the authenticated scenarios now run, but this one is still worth
// keeping: it isolates the per-request overhead every caller pays from the cost
// of the handler behind it, which is the number you want when judging whether a
// latency regression came from the middleware or the query.
//
// It is deliberately NOT a substitute for the authenticated scenarios. Nothing
// here touches a ledger, a quote, a reservation or a stream.
//
//   BASE_URL=http://127.0.0.1:18099/v1 k6 run test/load/public_surface.js
//
// Environment:
//   BASE_URL   API origin including /v1 (default http://127.0.0.1:8080/v1)
//   VUS        concurrent virtual users (default 20)
//   DURATION   test duration (default 30s)

import http from "k6/http";
import { check, group } from "k6";
import { Rate, Trend } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://127.0.0.1:8080/v1";
const VUS = Number(__ENV.VUS || 20);
const DURATION = __ENV.DURATION || "30s";

// Tracked separately so a slow database shows up as readiness latency rather
// than being averaged away against the static liveness probe.
const readyLatency = new Trend("ready_latency", true);
const healthLatency = new Trend("health_latency", true);
const problemShapeOK = new Rate("problem_json_shape_ok");

export const options = {
  scenarios: {
    public_surface: {
      executor: "constant-vus",
      vus: VUS,
      duration: DURATION,
    },
  },
  thresholds: {
    // Liveness is a static answer; anything slow here is the middleware chain.
    "health_latency": ["p(95)<50"],
    // Readiness makes a database round trip, so it is allowed more room.
    "ready_latency": ["p(95)<250"],
    // The error contract must hold under load, not just at rest.
    "problem_json_shape_ok": ["rate==1.0"],
    // NOT http_req_failed: one of the four requests per iteration is a
    // deliberate anonymous call that MUST return 401, and k6 counts any non-2xx
    // as a failed request. Thresholding on it would demand that the
    // authorization check stop working. The 2xx endpoints are asserted by their
    // own checks and latency thresholds above.
    "checks": ["rate==1.0"],
  },
};

export default function () {
  group("liveness", () => {
    const res = http.get(`${BASE_URL}/healthz`, { tags: { name: "healthz" } });
    healthLatency.add(res.timings.duration);
    check(res, { "healthz is 200": (r) => r.status === 200 });
  });

  group("readiness", () => {
    const res = http.get(`${BASE_URL}/readyz`, { tags: { name: "readyz" } });
    readyLatency.add(res.timings.duration);
    check(res, { "readyz is 200": (r) => r.status === 200 });
  });

  group("version", () => {
    const res = http.get(`${BASE_URL}/version`, { tags: { name: "version" } });
    check(res, {
      "version is 200": (r) => r.status === 200,
      "version names the environment": (r) => {
        try {
          return typeof r.json("environment") === "string";
        } catch (_) {
          return false;
        }
      },
      // A build must never advertise a secret or a DSN in its version banner.
      "version leaks no secret": (r) =>
        !/password|secret|sk_live|postgres:\/\//i.test(r.body || ""),
    });
  });

  group("unauthenticated is refused in problem+json", () => {
    const res = http.get(`${BASE_URL}/accounts`, { tags: { name: "accounts_anon" } });
    const ok = check(res, {
      "anonymous read is 401": (r) => r.status === 401,
      "content-type is problem+json": (r) =>
        (r.headers["Content-Type"] || "").includes("application/problem+json"),
      "carries a machine-readable code": (r) => {
        try {
          return r.json("code") === "UNAUTHENTICATED";
        } catch (_) {
          return false;
        }
      },
      "carries a request id": (r) => {
        try {
          return String(r.json("request_id") || "").length > 0;
        } catch (_) {
          return false;
        }
      },
      // The whole point of the contract: a refusal must not describe internals.
      "leaks no internals": (r) =>
        !/postgres:\/\/|SELECT |sqlstate|password/i.test(r.body || ""),
    });
    problemShapeOK.add(ok);
  });
}
