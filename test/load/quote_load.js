// Quote preview load (PART 154): non-binding quotes hit the execution provider
// through the platform's rate limiter and provider health tracker. Run only
// against a fake or sandbox provider; never against live keys.
import { sleep } from "k6";
import { BASE_URL, params, postIdempotent, requireAccount } from "./lib.js";
import http from "k6/http";

export const options = {
  scenarios: {
    quotes: { executor: "constant-arrival-rate", rate: 20, timeUnit: "1s", duration: "2m", preAllocatedVUs: 20, maxVUs: 100 },
  },
  thresholds: {
    "http_req_duration{name:quote-preview}": ["p(95)<1500"],
    // 429s from the quote limiter are expected under load and are not failures.
    checks: ["rate>0.99"],
  },
};

let instrumentId = __ENV.INSTRUMENT_ID || "";

export function setup() {
  if (instrumentId) return { instrumentId };
  // params() carries the session cookie and Fetch Metadata; a bare http.get
  // here is anonymous and gets 401, which surfaced as the misleading
  // "no instruments available" rather than as an auth failure.
  const res = http.get(`${BASE_URL}/instruments`, params());
  const list = res.json();
  if (!Array.isArray(list) || list.length === 0) throw new Error("no instruments available");
  return { instrumentId: list[0].id };
}

export default function (data) {
  const acct = requireAccount();
  postIdempotent(
    "/quotes/preview",
    { account_id: acct, instrument_id: data.instrumentId, action: "ACQUIRE_NOTIONAL", notional_usd: "25.00" },
    `k6-quote-${__VU}-${__ITER}`,
    "quote-preview",
  );
  sleep(0.1);
}
