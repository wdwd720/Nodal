// Load against the Nodal-native economy (gola.md Stage 21, PART LXXIV).
//
// Two scenarios, and the point of both is a FINANCIAL invariant rather than a
// latency number:
//
//   marketplace  Many buyers at one product. Every purchase moves Credits
//                between two accounts and mints provenance for the seller, so
//                the invariant is that the buyer's balance falls by exactly
//                the price of the purchases that committed and by nothing
//                else. A purchase that half-happens is the failure this
//                watches for.
//
//   catalogue    The read surface under the same load. It exists so that a
//                latency number for the writes has something honest to be
//                compared against, and so a read path that only works when
//                nothing else is happening is visible.
//
// What this deliberately does NOT do: create native-market trades. Trading
// needs an activated market with an approved asset, which is a moderation
// decision this script must not manufacture — a load script that activates its
// own market is a load script that has switched off a control to measure
// throughput.
//
// Results are only meaningful with the commit, host and database size recorded
// beside them. A laptop number is never production capacity.
import { sleep } from "k6";
import { Counter } from "k6/metrics";

import { get, postIdempotent, requireAccount, uuid } from "./lib.js";

const bought = new Counter("internal_purchases_committed");
const replayed = new Counter("internal_purchases_replayed");
// Refusals are counted BY THE CODE THE BACKEND GAVE, never by status alone.
// A 409 is three different things here — the price moved, the same key is
// still in flight, or the same key came back with a different body — and a run
// that reports all three as "price changed" is a run that has misread its own
// results. The first draft of this script did exactly that.
const refusedPrice = new Counter("internal_purchases_refused_price_changed");
const refusedInFlight = new Counter("internal_purchases_refused_key_in_flight");
const refusedKeyReuse = new Counter("internal_purchases_refused_key_reused");
const refusedFunds = new Counter("internal_purchases_refused_insufficient");
const refusedGate = new Counter("internal_purchases_refused_capability_off");
const refusedPolicy = new Counter("internal_purchases_refused_by_policy");
const refusedOther = new Counter("internal_purchases_refused_other");
const nothingToBuy = new Counter("internal_purchases_no_catalogue");

export const options = {
  scenarios: {
    marketplace: {
      executor: "shared-iterations",
      vus: 50,
      iterations: 200,
      maxDuration: "3m",
      exec: "buy",
    },
    catalogue: {
      executor: "constant-vus",
      vus: 10,
      duration: "30s",
      exec: "browse",
    },
  },
  thresholds: {
    // NOT http_req_failed. k6 counts every non-2xx as a failure, and a refusal
    // is the expected answer here: the marketplace gate may be off, the legal
    // policy may deny, the buyer may run out of Credits. A threshold on that
    // metric would fail a run that behaved perfectly, and an operator who has
    // learned to ignore a red threshold has lost the threshold.
    //
    // What must hold is that the server never breaks and never answers a
    // refusal with something a client cannot read.
    "checks{check:no 5xx}": ["rate==1.00"],
    "checks{check:problem+json on rejection}": ["rate==1.00"],
    http_req_duration: ["p(95)<1000"],
  },
};

export function setup() {
  const accountId = requireAccount();
  const products = get("/internal-products", "list-products").json();
  const balance = get(`/credits/balance?account_id=${accountId}`, "credit-balance").json();

  if (!Array.isArray(products) && !(products && products.items)) {
    console.log("no product page returned; the marketplace scenario will report refusals only");
  }
  const items = (products && products.items) || [];
  const first = items.find((p) => p.seller_account_id !== accountId);

  console.log(
    `credits before: gross=${balance.gross} spendable=${balance.spendable} ` +
      `payout_eligible=${balance.payout_eligible} policy=${balance.policy_version}`,
  );

  return {
    accountId,
    productId: first ? first.product_id : "",
    price: first ? first.price : "0",
    grossBefore: balance.gross,
    sharedKey: uuid(),
  };
}

export function buy(data) {
  if (data.productId === "") {
    // Nothing this account may buy. Counted separately rather than as a
    // refusal, so an empty catalogue can never be read as the system saying
    // no — and so a run that exercised nothing cannot look like a fast one.
    nothingToBuy.add(1);
    sleep(0.1);
    return;
  }
  // Every tenth iteration reuses one key, so idempotent replay is exercised
  // under contention rather than only in isolation.
  const key = __ITER % 10 === 0 ? data.sharedKey : uuid();
  const res = postIdempotent(
    `/internal-products/${data.productId}/orders`,
    { account_id: data.accountId, expected_price: data.price },
    key,
    "buy-internal-product",
  );
  const code = res.status === 201 || res.status === 200 ? "" : res.json("code");
  if (res.status === 201) bought.add(1);
  else if (res.status === 200) replayed.add(1);
  else if (code === "CONFLICT") refusedPrice.add(1);
  else if (code === "IDEMPOTENCY_IN_PROGRESS") refusedInFlight.add(1);
  else if (code === "INVALID_IDEMPOTENCY_REUSE") refusedKeyReuse.add(1);
  else if (code === "INSUFFICIENT_BUYING_POWER") refusedFunds.add(1);
  else if (code === "CAPABILITY_NOT_APPROVED") refusedGate.add(1);
  else if (code === "FORBIDDEN") refusedPolicy.add(1);
  else refusedOther.add(1);
  sleep(0.05);
}

export function browse() {
  get("/internal-products", "list-products");
  get("/native-assets", "list-native-assets");
  sleep(0.2);
}

export function teardown(data) {
  // The invariant, reported rather than asserted: k6 cannot fail a run from
  // teardown, and the authoritative checks are VerifyProvenance and
  // VerifyEarnings in the integration suite. What this gives an operator is
  // the arithmetic to eyeball — Credits out should equal purchases committed
  // times the price, and nothing else.
  const after = get(`/credits/balance?account_id=${data.accountId}`, "credit-balance").json();
  const orders = get(`/internal-orders?account_id=${data.accountId}`, "list-orders").json();
  const count = ((orders && orders.items) || []).length;
  console.log(
    `credits gross before=${data.grossBefore} after=${after.gross}; ` +
      `orders now=${count}; price=${data.price}. ` +
      `Expect (before - after) to equal price times the purchases this run committed.`,
  );
}
