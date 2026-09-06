// Portfolio reads (PART 154): buying power, holdings, ledger history.
// Buying power is computed on every request (never cached as truth), so this
// scenario measures the real cost of the authoritative read path.
import { sleep } from "k6";
import { get, requireAccount } from "./lib.js";

export const options = {
  scenarios: {
    portfolio: {
      executor: "ramping-vus",
      startVUs: 1,
      stages: [
        { duration: "30s", target: 25 },
        { duration: "2m", target: 25 },
        { duration: "30s", target: 0 },
      ],
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    "http_req_duration{name:buying-power}": ["p(95)<400"],
    "http_req_duration{name:holdings}": ["p(95)<400"],
    "http_req_duration{name:ledger}": ["p(95)<600"],
  },
};

export default function () {
  const acct = requireAccount();
  get(`/accounts/${acct}/buying-power?purpose=DISPLAY`, "buying-power");
  get(`/accounts/${acct}/holdings`, "holdings");
  get(`/accounts/${acct}/ledger/transactions?limit=50`, "ledger");
  sleep(1);
}
