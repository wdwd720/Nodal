// Reservation contention through the API (PART 154, complementing the PART 23
// package-level torture test): many VUs submit PAPER intents against one
// account at once. The invariant under test is financial, not latency:
// the sum of accepted notionals never exceeds the account's buying power,
// and duplicate idempotency keys never create duplicate intents.
import { sleep } from "k6";
import { Counter } from "k6/metrics";
import { get, postIdempotent, requireAccount, uuid } from "./lib.js";
import http from "k6/http";
import { BASE_URL, params } from "./lib.js";

const accepted = new Counter("intents_accepted");
const rejectedInsufficient = new Counter("intents_rejected_insufficient");
const replayed = new Counter("intents_replayed");

export const options = {
  scenarios: {
    burst: { executor: "shared-iterations", vus: 100, iterations: 400, maxDuration: "3m" },
  },
  thresholds: { http_req_failed: ["rate<0.01"] },
};

export function setup() {
  const acct = requireAccount();
  const bp = get(`/accounts/${acct}/buying-power?purpose=TRADE`, "buying-power").json();
  // params() carries the session cookie and Fetch Metadata; a bare http.get
  // here is anonymous and gets 401, which surfaced as the misleading
  // "no instruments available" rather than as an auth failure.
  const res = http.get(`${BASE_URL}/instruments`, params());
  const list = res.json();
  return { accountId: acct, buyingPower: bp.buying_power, instrumentId: list[0].id, sharedKey: uuid() };
}

export default function (data) {
  // Every 10th iteration reuses one shared key to exercise idempotent replay.
  const key = __ITER % 10 === 0 ? data.sharedKey : uuid();
  const res = postIdempotent(
    "/intents",
    { account_id: data.accountId, instrument_id: data.instrumentId, action: "ACQUIRE_NOTIONAL", notional_usd: "50.00", mode: "PAPER" },
    key,
    "submit-intent",
  );
  if (res.status === 202) accepted.add(1);
  else if (res.status === 200) replayed.add(1);
  else if (res.status === 422 && res.json("code") === "INSUFFICIENT_BUYING_POWER") rejectedInsufficient.add(1);
  sleep(0.05);
}

export function teardown(data) {
  // Post-condition: reserved never exceeds the pre-run buying power. Reported,
  // not asserted, because k6 cannot fail a run from teardown; the reconciliation
  // verifiers are the authoritative check.
  const bp = get(`/accounts/${data.accountId}/buying-power?purpose=TRADE`, "buying-power").json();
  console.log(`buying power before=${data.buyingPower} after=${bp.buying_power} reserved=${bp.reserved}`);
}
