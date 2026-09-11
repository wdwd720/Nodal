/**
 * Scenario F — a verified identity on the sandbox tier, and a payout that
 * reserves real Credits and moves no value anywhere.
 *
 * `docs/product/STAGING_E2E.md` defines F as `/verify` → the sandbox provider
 * (labelled) → outcome VERIFIED through the callback → `/withdraw` eligible per
 * origin → a destination from a token → quote → request → `PROVIDER_PENDING` →
 * `SETTLED` after ten seconds. What must not happen: a real provider, and a KYC
 * result written by the browser.
 *
 * # What this build can and cannot reach, and why the spec says so
 *
 * Everything up to and including the reservation is real here and is walked:
 * the rehearsal provider decides through the control the API itself exposes,
 * eligibility is recomputed per origin, a destination is tokenised, the quote
 * comes back with a fee, a net, an expiry and the provenance slices it would
 * consume, and the request reserves exactly those units out of the account's
 * spendable balance.
 *
 * The two states after that are not reachable from any route this deployment
 * serves. `payout.Service.Submit` — the method that hands a reserved request to
 * a provider — has no caller anywhere in `cmd/`: no worker runs it, no admin
 * route exposes it, and the customer API has no "submit" verb by design. So a
 * request stops at VERIFIED with the value held, and the sandbox provider's
 * ten-second settlement is never asked for. This spec asserts that it stops
 * there, which is the honest fact about the build; the PROVIDER_PENDING and
 * SETTLED assertions belong in this file the day something submits, and the
 * shape below is written so they can be added without moving anything else.
 *
 * # The value has to be earned, because the seeded value cannot leave
 *
 * Seeded Credits are PROMOTIONAL, which the sandbox payout policy refuses on
 * purpose. So customer-a buys something from the seeded catalogue and the
 * SELLER — customer-b — ends up holding a CREATOR_EARNING, which the policy
 * permits. Everything after that is driven as customer-b through the pages.
 */
import { expect, test, type Page } from "@playwright/test";

import {
  SAME_ORIGIN,
  accountIdOf,
  buySomething,
  creditBalance,
  ensureDestination,
  signInAs,
  verifyInSandbox,
} from "../withdrawal.ts";

/** Reads the eligibility answer straight from the API. */
async function eligibilityOf(page: Page, accountId: string): Promise<{
  readonly eligible: boolean;
  readonly withdrawable_now: string;
  readonly current_verification: string;
  readonly sandbox: boolean;
  readonly provider?: string;
  readonly minimum_quantity?: string;
  readonly destination_configured?: boolean;
  readonly jurisdiction_supported?: boolean;
  readonly buckets: ReadonlyArray<{
    readonly origin: string;
    readonly quantity: string;
    readonly withdrawable: string;
    readonly payout_allowed: boolean;
    readonly consumption_rank: number;
    readonly reasons: readonly string[];
  }>;
}> {
  const response = await page.request.get(`/v1/me/eligibility?account_id=${accountId}`);
  expect(response.status(), "eligibility is readable").toBe(200);
  return (await response.json()) as never;
}

test("a rehearsal verification, an eligible earning, a quote and a reservation that moves nothing", async ({
  browser,
  page,
}) => {
  test.setTimeout(120_000);

  // --- the earning -------------------------------------------------------
  // The buyer is the suite's own session. The purchase is this scenario's
  // precondition, not its subject, so it goes through the API.
  const buyerId = await accountIdOf(page);
  await buySomething(page, buyerId);

  // --- the seller's journey, through the pages ---------------------------
  const seller = await signInAs(browser, "customer-b", "Customer B");
  const sellerId = await accountIdOf(seller);

  // The suite has to be re-runnable against a database it has already touched,
  // so nothing here asserts that this account STARTS unverified: a second run
  // finds the verification the first one produced, and `verifyInSandbox` leaves
  // it alone rather than pushing a second session through.

  // 1 · Verification, decided by the rehearsal provider through the control
  // the API exposes. Nothing here writes a verification row from the browser.
  await verifyInSandbox(seller);

  const profile = await seller.request.get(`/v1/me/verification?account_id=${sellerId}`);
  const verified = (await profile.json()) as {
    readonly state: string;
    readonly level: string;
    readonly sandbox: boolean;
    readonly payout_ready: boolean;
    readonly checks: ReadonlyArray<{ readonly kind: string; readonly sandbox: boolean }>;
  };
  expect(verified.state).toBe("VERIFIED");
  // PAYOUT_KYC is what a withdrawal needs; the rehearsal provider answers every
  // sub-check including PEP, which is enough for ENHANCED. Either satisfies the
  // requirement, and asserting the exact one would be asserting how thorough a
  // rehearsal happens to be.
  expect(["PAYOUT_KYC", "ENHANCED"]).toContain(verified.level);
  expect(verified.payout_ready).toBe(true);
  // Every row it wrote is labelled a rehearsal. That label is what stops this
  // from being indistinguishable from an assessment somebody actually made.
  expect(verified.sandbox, "the profile is labelled a rehearsal").toBe(true);
  for (const check of verified.checks) {
    expect(check.sandbox, `${check.kind} is labelled a rehearsal`).toBe(true);
  }

  // 2 · A destination, registered from a sandbox handle through the page.
  //
  // It comes before the eligibility assertions because eligibility COMPOSES
  // the destination: a verified person with nowhere to send value still cannot
  // withdraw, and the API says so rather than reporting an amount that has no
  // route out. Asserting eligibility first would have been asserting against
  // half the question.
  await ensureDestination(seller, "customer-b");
  const destinations = await seller.request.get(`/v1/me/payout-destinations?account_id=${sellerId}`);
  expect(destinations.status(), await destinations.text()).toBe(200);
  const registered = (await destinations.json()) as ReadonlyArray<{
    readonly destination_id: string;
    readonly provider: string;
    readonly sandbox: boolean;
    readonly usable?: boolean;
  }>;
  const usable = registered.find((row) => row.usable === true);
  expect(usable, "the sandbox provider tokenised the handle").toBeTruthy();
  expect(usable?.provider, "the provider is the rehearsal one").toBe("sandbox_payout");
  expect(usable?.sandbox, "the destination is labelled a rehearsal").toBe(true);

  // 2b · And the provider is asked about the recipient, always (D-122).
  //
  // `country` used to be optional, and the provider was asked only when the
  // client supplied one -- so the same body with "country":"FR" came back 503
  // RECIPIENT_COUNTRY_UNSUPPORTED and the body WITHOUT the field came back 201
  // with status VERIFIED. Both probes run after the step-up above, so neither
  // refusal is a step-up wearing another code.
  const noCountry = await seller.request.post("/v1/me/payout-destinations", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": `f-nocountry-${String(Date.now())}` },
    data: { account_id: sellerId, kind: "BANK", provider_token: "sbx_tok_nocountry" },
  });
  expect(noCountry.status(), await noCountry.text()).toBe(400);
  expect(JSON.stringify(await noCountry.json())).toContain("country");

  const unsupported = await seller.request.post("/v1/me/payout-destinations", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": `f-fr-${String(Date.now())}` },
    data: {
      account_id: sellerId, kind: "BANK", provider_token: "sbx_tok_fr",
      currency: "USD", country: "FR",
    },
  });
  expect(
    unsupported.status(),
    "a destination the provider has said it cannot pay is refused",
  ).toBeGreaterThanOrEqual(400);
  expect(JSON.stringify(await unsupported.json())).toContain("RECIPIENT_COUNTRY_UNSUPPORTED");

  const after = await seller.request.get(`/v1/me/payout-destinations?account_id=${sellerId}`);
  const afterRows = (await after.json()) as ReadonlyArray<{ readonly country?: string }>;
  expect(
    afterRows.some((row) => row.country === "FR"),
    "a refused destination is not registered",
  ).toBe(false);

  const balanceBefore = await creditBalance(seller, sellerId);
  const payoutsBefore = await seller.request.get(`/v1/payouts?account_id=${sellerId}`);
  const countBefore = ((await payoutsBefore.json()) as { readonly items: readonly unknown[] }).items
    .length;

  // 3 · Eligibility, per origin, recomputed against the new level and the new
  // destination.
  const eligible = await eligibilityOf(seller, sellerId);
  expect(["PAYOUT_KYC", "ENHANCED"]).toContain(eligible.current_verification);
  expect(eligible.sandbox, "the whole answer is labelled a rehearsal").toBe(true);
  expect(eligible.provider, "the rehearsal provider is the one that would pay").toBe(
    "sandbox_payout",
  );
  expect(eligible.destination_configured, "the destination composes into the answer").toBe(true);
  expect(eligible.jurisdiction_supported, "the jurisdiction the person stated is offered").toBe(true);

  // The sale minted an earning of an origin the policy PERMITS. Which origin it
  // is depends on what the seeded catalogue sells -- a DATA product mints
  // DATA_SALE_EARNING, an agent service mints AGENT_SERVICE_EARNING -- and the
  // rule under test is that provenance decides, not which row it happens to be.
  const earned = eligible.buckets.filter(
    (bucket) => bucket.payout_allowed && BigInt(bucket.quantity) > 0n,
  );
  expect(earned.length, "the sale minted value of a permitted origin").toBeGreaterThan(0);

  // And the promotional grant still cannot leave, which is the whole point of
  // deciding per origin rather than per balance: value that was given away does
  // not become withdrawable by being spent and re-earned.
  const promotional = eligible.buckets.find((bucket) => bucket.origin === "PROMOTIONAL");
  expect(promotional?.payout_allowed, "a promotional grant never leaves").toBe(false);
  expect(BigInt(promotional?.withdrawable ?? "1")).toBe(0n);

  // # What holds value back here, and what no longer does
  //
  // Every Credit on a seeded deployment is UNFUNDED: `scripts/seedeconomy`
  // grants them PROMOTIONAL and unfunded on purpose, because nobody paid for
  // them. This spec used to explain the refusal by saying an earning "inherits
  // that" — it did not. internal/commerce and internal/nativemarket minted
  // every earning REVERSIBLE unconditionally, and the only writer that promotes
  // a lot out of REVERSIBLE reads `credit_fundings.lot_id`, which an earning has
  // never had. So the refusal was not the finality model working; it was F-230,
  // and no earning on any deployment could ever be withdrawn.
  //
  // D-124 makes an earning as final as what paid for it: it records the lots it
  // was funded from and is minted at the least final finality among them. An
  // earning funded by an UNFUNDED grant is therefore UNFUNDED and
  // payout-eligible, and one funded by a card payment inside its dispute window
  // is REVERSIBLE until `credit.Service.SettleDerived` promotes it — which is
  // what FUNDING_NOT_SETTLED has always claimed and could not deliver.
  //
  // The spec still follows whichever answer the tier gives rather than
  // asserting one it cannot produce: what the seeded catalogue sells, and
  // therefore what funds the sale, is not this scenario's subject.
  const holdsUnfunded = eligible.buckets.some((bucket) =>
    bucket.reasons.includes("FUNDING_NOT_SETTLED"),
  );
  if (!eligible.eligible) {
    expect(
      holdsUnfunded,
      "nothing is withdrawable, and the reason is a stated one rather than a silent zero",
    ).toBe(true);
    expect(eligible.withdrawable_now).toBe("0");
  }

  // 4 · A quote, through the page. It reserves nothing and writes no ledger row.
  await seller.goto("/withdraw");
  await seller.waitForLoadState("networkidle");
  const table = seller.getByRole("group", { name: /Your payout destinations/ });
  const use = table.getByRole("button", { name: "Use this" }).first();
  if ((await use.count()) > 0) await use.click();

  // The page must have read the withdrawal disclosure to the person before it
  // will let them commit to anything, and it asks for it here rather than at
  // signup. Accepting it is part of the journey, not a fixture.
  const disclosure = seller.getByRole("region", { name: "Withdrawal and Verification Disclosure" });
  if ((await disclosure.count()) > 0) {
    await expect(disclosure).toContainText("Nodal is not a money transmitter");
    await disclosure.getByRole("button", { name: "I have read this" }).click();
  }

  // The amount asked for is the whole permitted bucket, in the decimal form a
  // person types. Exact base units in, exact base units out.
  const askable = earned[0];
  await seller.getByLabel("Amount in Credits").fill(asDecimal(askable?.quantity ?? "0"));
  await seller.getByRole("button", { name: "Get a quote" }).click();

  const quoted = seller.getByText("Above the minimum");
  const refusedQuote = seller.getByText("No quote was given for this.");
  await expect(quoted.or(refusedQuote).first()).toBeVisible();

  if (await refusedQuote.isVisible()) {
    // The tier refused to price it — because the value it names cannot leave —
    // and the page renders the refusal with the backend's own code and the next
    // action rather than an error or a zero.
    const refusal = seller.locator(".refusal", { hasText: "No quote was given for this." });
    await expect(refusal.locator(".refusal-meta")).not.toBeEmpty();

    // Nothing was reserved and nothing was created by asking.
    const after = await creditBalance(seller, sellerId);
    expect(after.spendable, "asking for a price reserves nothing").toBe(balanceBefore.spendable);
    const payouts = await seller.request.get(`/v1/payouts?account_id=${sellerId}`);
    const listed = (await payouts.json()) as { readonly items: readonly unknown[] };
    expect(listed.items.length, "a refused quote creates no payout").toBe(countBefore);

    // And the commit control is refused too, with the reason beside it rather
    // than in a tooltip.
    const submit = seller.getByRole("button", { name: "Request this withdrawal" });
    await expect(submit).toBeDisabled();
    await seller.context().close();
    return;
  }

  // A quote came back. Everything a customer commits against is on the screen:
  // what leaves, the fee, the expiry, the sandbox label, and the provenance.
  await expect(seller.getByText("What leaves")).toBeVisible();
  await expect(seller.getByText("These figures come from a sandbox provider")).toBeVisible();
  await expect(seller.getByText("SANDBOX-PLACEHOLDER-NOT-A-PRICE")).toBeVisible();
  await expect(seller.getByText(/seconds left/)).toBeVisible();

  // 5 · The request. This is the only step that reserves anything.
  await seller.getByRole("button", { name: "Request this withdrawal" }).click();
  await expect(seller.getByRole("region", { name: "This request" })).toBeVisible();

  const payouts = await seller.request.get(`/v1/payouts?account_id=${sellerId}`);
  const listed = (await payouts.json()) as {
    readonly items: ReadonlyArray<{
      readonly payout_id: string;
      readonly state: string;
      readonly reserved_quantity: string;
      readonly sandbox?: boolean;
    }>;
  };
  expect(listed.items.length, "exactly one more payout request exists").toBe(countBefore + 1);
  const request = listed.items[0];
  expect(BigInt(request?.reserved_quantity ?? "0") > 0n, "value was reserved").toBe(true);

  // 6 · The Credits moved out of spendable and did NOT leave the system: the
  // gross is unchanged, because a reservation holds value rather than spending
  // it, and nothing external has been instructed.
  const balanceAfter = await creditBalance(seller, sellerId);
  expect(balanceAfter.gross, "nothing has left the system").toBe(balanceBefore.gross);
  expect(
    BigInt(balanceBefore.spendable) - BigInt(balanceAfter.spendable),
    "exactly what was reserved came out of spendable, and not a unit more",
  ).toBe(BigInt(request?.reserved_quantity ?? "0"));

  const detail = await seller.request.get(`/v1/payouts/${request?.payout_id ?? ""}`);
  const followed = (await detail.json()) as {
    readonly state: string;
    readonly sandbox?: boolean;
    readonly provenance?: ReadonlyArray<{ readonly origin: string; readonly quantity: string }>;
  };
  expect((followed.provenance ?? []).length, "the payout names what it consumed").toBeGreaterThan(0);
  for (const slice of followed.provenance ?? []) {
    expect(slice.origin, "only an origin the policy permits is consumed").not.toBe("PROMOTIONAL");
  }

  // 7 · And then the sweep takes it. `cmd/api/payoutsweep.go` (D-085) hands a
  // reserved request to the provider every fifteen seconds and asks about it
  // five seconds later; the rehearsal provider settles ten seconds after it
  // accepts. So the provider leg happens on its own, without this file touching
  // the database.
  //
  // This spec used to assert the request "stops at VERIFIED" and say that
  // nothing in this build hands a reserved request to a provider. That was true
  // when it was written and had stopped being true: the assertion passed only
  // because it read the state immediately, before the first tick (F-250). What
  // is asserted now is what the deployment actually does, observed rather than
  // assumed, with a bounded wait.
  const seen = new Set<string>([followed.state]);
  let final = followed.state;
  for (let attempt = 0; attempt < 24 && final !== "SETTLED" && final !== "FAILED"; attempt += 1) {
    const poll = await seller.request.get(`/v1/payouts/${request?.payout_id ?? ""}`);
    const body = (await poll.json()) as { readonly state: string };
    final = body.state;
    seen.add(final);
    if (final === "SETTLED" || final === "FAILED") break;
    await seller.waitForTimeout(5000);
  }
  expect(
    ["VERIFIED", "SUBMITTED", "PROVIDER_PENDING", "SETTLED"],
    `the request went as far as the rehearsal provider takes it; states seen: ${[...seen].join(" → ")}`,
  ).toContain(final);

  // And no value moved anywhere real. The payout is labelled a rehearsal on the
  // row itself rather than by asking what the provider happens to be today
  // (F-232), the provider is the rehearsal one, and the page says the state the
  // request is actually in.
  const settled = await seller.request.get(`/v1/payouts/${request?.payout_id ?? ""}`);
  const settledBody = (await settled.json()) as {
    readonly state: string;
    readonly sandbox?: boolean;
    readonly provider?: string;
  };
  expect(settledBody.sandbox, "the payout is labelled a rehearsal").toBe(true);
  if (settledBody.provider !== undefined && settledBody.provider !== "") {
    expect(settledBody.provider, "the rehearsal provider is the one it went to").toBe(
      "sandbox_payout",
    );
  }
  await seller.goto("/withdraw");
  await seller.waitForLoadState("networkidle");
  await expect(seller.getByRole("region", { name: "This request" })).toContainText(
    settledBody.state,
  );

  await seller.context().close();
});

/**
 * Exact base units as the decimal a person types into the form.
 *
 * No arithmetic touches the value: the six-decimal shift is string surgery, so
 * the amount asked for is exactly the amount the backend holds.
 */
function asDecimal(baseUnits: string): string {
  const decimals = 6;
  const padded = baseUnits.padStart(decimals + 1, "0");
  const cut = padded.length - decimals;
  return `${padded.slice(0, cut)}.${padded.slice(cut)}`;
}

test("the sandbox outcome control is refused where it does not belong", async ({ page }) => {
  // The control exists only because the API said this session has one. Asking
  // for an outcome the session was never offered is refused by the same route
  // that offers it, which is what keeps "sandbox tier only" a property of the
  // backend rather than a decision the interface makes.
  const accountId = await accountIdOf(page);
  const response = await page.request.post("/v1/me/verification/sandbox-outcome", {
    headers: { ...SAME_ORIGIN, "Idempotency-Key": `f-sandbox-${String(Date.now())}` },
    data: { account_id: accountId, outcome: "VERIFIED" },
  });
  // On this sandbox tier the route exists; what it must never do is decide
  // anything for a session that has none open.
  if (response.status() >= 400) {
    const problem = (await response.json()) as { readonly code?: string };
    expect(problem.code, "a refusal carries a stable code").toBeTruthy();
    return;
  }
  // If it did answer, it answered about a session that exists, and the outcome
  // it recorded is labelled a rehearsal.
  const session = (await response.json()) as { readonly sandbox: boolean };
  expect(session.sandbox, "a rehearsal outcome is labelled one").toBe(true);
});
