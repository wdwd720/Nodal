/**
 * The transfers section of the advanced (sandbox) area against the real API:
 * customer-to-customer transfers of simulated value and the sandbox faucet.
 *
 * Nothing is stubbed except where a test says so to lose an answer on
 * purpose. Every assertion compares what a page shows with what the backend
 * answered, including where it answers no. A send's outcome is the
 * backend's: where the sender's identity is verified it COMPLETES; where it
 * is not (the local seed verifies nobody), Risk Kernel V2 refuses it and the
 * refusal is RECORDED as a REJECTED transfer. Both are legitimate answers and
 * each is asserted as what it is -- the test never decides which one is due.
 *
 * Customer-a is the session the suite signed in with. Customer-b and the
 * operator sign in here through the same development identity provider, in
 * contexts of their own.
 */
import { randomBytes } from "node:crypto";

import { expect, test, type Browser, type BrowserContext, type Locator, type Page, type Response } from "@playwright/test";

/** The accessible name of the notice every page of the area opens with. */
const NOTICE = "Simulated value — sandbox only";
const SECTION = "/advanced/transfers";
const SEND_PATH = "/v1/advanced/transfers";

interface Problem {
  readonly code: string;
  readonly detail?: string;
  readonly fields?: Record<string, unknown>;
}

interface Transfer {
  readonly id: string;
  readonly status: string;
  readonly sender_account_id: string;
  readonly recipient_account_id?: string;
  readonly memo?: string;
  readonly risk_decision_id?: string;
}

interface Allowance {
  readonly asset_symbol: string;
  readonly asset_decimals: number;
  readonly used: string;
  readonly remaining: string;
  readonly value_domain: string;
}

function hex(bytes: number): string {
  return randomBytes(bytes).toString("hex");
}

/** Waits until a page has its answer on screen (see advanced-core.spec.ts). */
async function settle(page: Page): Promise<void> {
  await expect(page.locator("h1")).toHaveCount(1);
  await page.waitForLoadState("networkidle");
  await expect(page.locator(".loading")).toHaveCount(0);
}

/** The account the application reads for a session: the first the backend lists. */
async function ownAccount(page: Page): Promise<string> {
  const response = await page.request.get("/v1/accounts");
  expect(response.ok()).toBeTruthy();
  const accounts = (await response.json()) as Array<{ id: string }>;
  const id = accounts[0]?.id;
  expect(id, "the signed-in principal owns an account").toBeTruthy();
  return id as string;
}

/** Signs a development identity in, in a fresh context, through the real OIDC flow. */
async function signIn(browser: Browser, identity: string, mfa: boolean): Promise<{ context: BrowserContext; page: Page }> {
  const context = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await context.newPage();
  await page.goto("/v1/auth/login");
  await expect(page.getByRole("heading", { name: "Choose an identity" })).toBeVisible();
  const row = page.locator("li", { has: page.locator("code", { hasText: new RegExp(`^${identity}$`) }) });
  await row.getByRole("link", { name: mfa ? "sign in with MFA" : "sign in", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Home", level: 1 })).toBeVisible();
  return { context, page };
}

/** The response to the POST an action makes, observed as the page received it. */
async function answerTo(page: Page, path: string, act: () => Promise<void>): Promise<Response> {
  const [response] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === path && r.request().method() === "POST"),
    act(),
  ]);
  return response;
}

/** A refusal is on screen with the backend's own code. */
async function expectCode(scope: Locator, code: string): Promise<void> {
  await expect(scope.locator(".explain-meta").first()).toContainText(`code ${code}`);
}

/** Requests sandbox dollars through the page, so the sandbox dollar exists and the sender holds some. */
async function fundThroughTheFaucet(page: Page, amount: string): Promise<void> {
  await page.goto(SECTION);
  await settle(page);
  const faucet = page.getByRole("region", { name: "Sandbox faucet", exact: true });
  const form = faucet.getByRole("form", { name: "Request sandbox dollars" });
  await form.getByRole("textbox").fill(amount);
  const response = await answerTo(page, "/v1/advanced/transfers/faucet/grants", () =>
    form.getByRole("button", { name: "Request sandbox dollars", exact: true }).click(),
  );
  expect(response.status(), "the faucet runs in this deployment").toBe(201);
  await expect(faucet.getByRole("status", { name: "The faucet's answer" })).toBeVisible();
}

/** Who customer-a pays: customer-b's fresh handle, or its account id when it registered one on an earlier run. */
interface Recipient {
  readonly handle?: string;
  readonly accountId: string;
}

async function prepareRecipient(browser: Browser): Promise<{ recipient: Recipient; context: BrowserContext; page: Page }> {
  const b = await signIn(browser, "customer-b", false);
  const accountId = await ownAccount(b.page);
  await b.page.goto(SECTION);
  await settle(b.page);
  const panel = b.page.getByRole("region", { name: "Your handle", exact: true });
  const handle = `b-${hex(6)}`;
  await panel.getByLabel("Handle", { exact: true }).fill(`@${handle.toUpperCase()}`);
  const response = await answerTo(b.page, "/v1/advanced/transfers/handles", () =>
    panel.getByRole("button", { name: "Register this handle", exact: true }).click(),
  );
  if (response.status() === 201) {
    // Normalized by the backend: the leading @ dropped, lower-cased.
    await expect(panel.locator(`[data-registered-handle="${handle}"]`)).toBeVisible();
    return { recipient: { handle, accountId }, ...b };
  }
  // The account registered a handle on an earlier run, and a handle is never
  // reassigned or read back: the backend says CONFLICT, and the account id is
  // the other way to name it.
  const problem = (await response.json()) as Problem;
  expect(problem.code).toBe("CONFLICT");
  await expectCode(panel, "CONFLICT");
  return { recipient: { accountId }, ...b };
}

/** Fills the send form and moves to the review step. */
async function reviewSend(send: Locator, recipient: Recipient, amount: string, memo: string): Promise<void> {
  if (recipient.handle !== undefined) {
    await send.getByLabel("Recipient handle").fill(`@${recipient.handle}`);
  } else {
    await send.getByRole("radio", { name: /account id/ }).check();
    await send.getByLabel("Recipient account id").fill(recipient.accountId);
  }
  await send.getByLabel(/^Amount in /).fill(amount);
  await send.getByLabel("Memo (optional)").fill(memo);
  await send.getByRole("button", { name: "Review the transfer", exact: true }).click();
  await expect(send.getByRole("group", { name: "Review the transfer" })).toBeVisible();
}

/** The transfer id a send's answer names: the transfer itself, or the REJECTED one a recorded refusal names. */
function transferIdOf(body: unknown): string | undefined {
  const record = body as { id?: unknown; fields?: { transfer_id?: unknown } };
  if (typeof record.id === "string") return record.id;
  const named = record.fields?.transfer_id;
  return typeof named === "string" ? named : undefined;
}

test.describe("the transfers section", () => {
  test("renders under the simulated-value notice and is reachable from the area's navigation", async ({ page }) => {
    await page.goto("/advanced/overview");
    await page
      .getByRole("navigation", { name: "Advanced area", exact: true })
      .getByRole("link", { name: "Transfers", exact: true })
      .click();
    await expect(page.getByRole("heading", { level: 1, name: "Transfers", exact: true })).toBeVisible();
    const notice = page.getByRole("note", { name: NOTICE, exact: true });
    await expect(notice).toBeVisible();
    await expect(notice).toContainText("no economic substance");
    await settle(page);
    await expect(page.locator(".malformed")).toHaveCount(0);
    await expect(page.getByText("This response could not be trusted")).toHaveCount(0);
  });

  test("the faucet grants sandbox dollars, and the allowance falls by exactly the grant", async ({ page }) => {
    const id = await ownAccount(page);
    const before = await page.request.get(`/v1/advanced/transfers/faucet/allowance?account_id=${id}`);
    await page.goto(SECTION);
    await settle(page);
    const faucet = page.getByRole("region", { name: "Sandbox faucet", exact: true });

    if (!before.ok()) {
      // A deployment where the faucet does not run says so with the backend's code, and offers no form.
      const problem = (await before.json()) as Problem;
      await expectCode(faucet, problem.code);
      await expect(faucet.getByRole("form", { name: "Request sandbox dollars" })).toHaveCount(0);
      return;
    }
    const allowance = (await before.json()) as Allowance;
    expect(allowance.value_domain).toBe("SIMULATED");
    await expect(faucet.getByText(`${allowance.remaining} base units`, { exact: true })).toBeVisible();

    const form = faucet.getByRole("form", { name: "Request sandbox dollars" });
    await form.getByLabel(`Amount in ${allowance.asset_symbol}`).fill("25");
    const response = await answerTo(page, "/v1/advanced/transfers/faucet/grants", () =>
      form.getByRole("button", { name: "Request sandbox dollars", exact: true }).click(),
    );
    expect(response.status()).toBe(201);
    const grant = (await response.json()) as { quantity: string; value_domain: string };
    expect(grant.value_domain).toBe("SIMULATED");
    const unit = 10n ** BigInt(allowance.asset_decimals);
    expect(BigInt(grant.quantity)).toBe(25n * unit);
    const answer = faucet.getByRole("status", { name: "The faucet's answer" });
    await expect(answer.locator('[data-field="granted-quantity"]')).toHaveText(`25 ${allowance.asset_symbol}`);
    await expect(form.locator("[data-idempotency-key]")).toHaveCount(1);

    const after = (await (await page.request.get(`/v1/advanced/transfers/faucet/allowance?account_id=${id}`)).json()) as Allowance;
    expect(BigInt(after.used) - BigInt(allowance.used)).toBe(25n * unit);
    expect(BigInt(allowance.remaining) - BigInt(after.remaining)).toBe(25n * unit);
    await settle(page);
    await expect(faucet.getByText(`${after.remaining} base units`, { exact: true })).toBeVisible();
  });

  test("refusals carry the backend's code: an unknown handle, and an amount finer than the asset", async ({ page }) => {
    await fundThroughTheFaucet(page, "1");
    const send = page.getByRole("region", { name: "Send", exact: true });

    await reviewSend(send, { handle: `nobody-${hex(6)}`, accountId: "" }, "1", "e2e unknown handle");
    let response = await answerTo(page, SEND_PATH, () =>
      send.getByRole("button", { name: "Confirm and send", exact: true }).click(),
    );
    expect(response.status()).toBe(404);
    await expectCode(send, "NOT_FOUND");
    await expect(send.getByText("Nothing was recorded for this attempt and nothing moved.")).toBeVisible();

    // Seven decimal places of a six-decimal asset: refused, never rounded.
    await send.getByRole("button", { name: "Change the transfer", exact: true }).click();
    await send.getByLabel(/^Amount in /).fill("1.0000001");
    await send.getByRole("button", { name: "Review the transfer", exact: true }).click();
    response = await answerTo(page, SEND_PATH, () =>
      send.getByRole("button", { name: "Confirm and send", exact: true }).click(),
    );
    expect(response.status()).toBe(422);
    expect(((await response.json()) as Problem).code).toBe("PRECISION_LOSS");
    await expectCode(send, "PRECISION_LOSS");
  });

  test("a send is decided once: a lost answer is retried with the same key, and the API replays it", async ({
    page,
    browser,
  }) => {
    const b = await prepareRecipient(browser);
    const alice = await ownAccount(page);
    await fundThroughTheFaucet(page, "5");
    const send = page.getByRole("region", { name: "Send", exact: true });
    const memo = `e2e ${hex(6)}`;
    await reviewSend(send, b.recipient, "2", memo);

    // The first answer is lost on the way back, as a dropped connection would
    // lose it. The page is told PROVIDER_UNAVAILABLE and offers a retry.
    const sent: Array<{ key: string; body: string }> = [];
    page.on("request", (request) => {
      if (request.method() === "POST" && new URL(request.url()).pathname === SEND_PATH) {
        sent.push({ key: request.headers()["idempotency-key"] ?? "", body: request.postData() ?? "" });
      }
    });
    let lost = false;
    const sendRoute = (url: URL): boolean => url.pathname === SEND_PATH;
    await page.route(
      sendRoute,
      async (route) => {
        if (route.request().method() !== "POST" || lost) {
          await route.continue();
          return;
        }
        lost = true;
        await route.fulfill({
          status: 503,
          contentType: "application/problem+json",
          body: JSON.stringify({
            type: "about:blank",
            title: "unavailable",
            status: 503,
            code: "PROVIDER_UNAVAILABLE",
            detail: "this answer was dropped by the test",
          }),
        });
      },
    );
    await send.getByRole("button", { name: "Confirm and send", exact: true }).click();
    await expectCode(send, "PROVIDER_UNAVAILABLE");
    const key = await send.locator("[data-idempotency-key]").first().getAttribute("data-idempotency-key");
    expect(key, "the key was created at confirmation and is shown").toBeTruthy();

    const response = await answerTo(page, SEND_PATH, () =>
      send.getByRole("button", { name: "Try again with the same key", exact: true }).click(),
    );
    await page.unroute(sendRoute);
    expect(sent.length, "the page sent the attempt twice").toBe(2);
    expect(sent[1]?.key, "the retry reused the confirmation's key").toBe(sent[0]?.key);
    expect(sent[1]?.key).toBe(key);
    expect(sent[1]?.body, "the retry is the same request").toBe(sent[0]?.body);

    const first: unknown = await response.json();
    const transferId = transferIdOf(first);
    expect(transferId, "the answer names the transfer it recorded").toBeTruthy();
    if (response.status() === 201) {
      await expect(send.getByRole("status", { name: "The backend's answer" })).toContainText(transferId as string);
    } else {
      // A refusal the backend reached after accepting the request: recorded as
      // a REJECTED transfer, shown with its code and linked.
      const problem = first as Problem;
      expect(problem.fields?.["rejection_code"], "a recorded refusal names its rejection code").toBeTruthy();
      await expectCode(send, problem.code);
      await expect(send.locator(`[data-recorded-refusal="${transferId as string}"]`)).toBeVisible();
    }

    // The API itself replays the attempt: the same key and the same bytes are
    // the same answer, never a second transfer.
    const replay = await page.request.post(SEND_PATH, {
      headers: {
        "Content-Type": "application/json",
        "Idempotency-Key": sent[1]?.key ?? "",
        "Sec-Fetch-Site": "same-origin",
      },
      data: sent[1]?.body ?? "",
    });
    expect(transferIdOf(await replay.json()), "a replay names the same transfer").toBe(transferId);
    expect(replay.status()).toBe(response.status() === 201 ? 200 : response.status());

    const listed = (await (await page.request.get(`/v1/advanced/transfers?account_id=${alice}&limit=200`)).json()) as {
      items: Transfer[];
      value_domain: string;
    };
    expect(listed.value_domain).toBe("SIMULATED");
    const mine = listed.items.filter((item) => item.memo === memo);
    expect(mine.map((item) => item.id), "exactly one transfer for the attempt").toEqual([transferId]);
    const transfer = mine[0] as Transfer;
    if (b.recipient.handle !== undefined) {
      expect(transfer.recipient_account_id, "a handle is never resolved for the sender").toBeUndefined();
    }

    // The sender's history shows it with the status the backend gave it.
    await page.goto(SECTION);
    await settle(page);
    const row = page.locator(`tr[data-transfer-id="${transfer.id}"]`);
    await expect(row).toContainText(transfer.status);
    await expect(row).toContainText("Sent");

    // The recipient sees what reached it, and nothing of an attempt that did not.
    const received = await b.page.request.get(`/v1/advanced/transfers/${transfer.id}`);
    await b.page.goto(`${SECTION}/${transfer.id}`);
    await settle(b.page);
    if (transfer.status === "COMPLETED") {
      expect(received.status()).toBe(200);
      const seen = (await received.json()) as Transfer;
      expect(seen.recipient_account_id).toBe(b.recipient.accountId);
      expect(seen.risk_decision_id, "the sender's decision record is the sender's").toBeUndefined();
      await expect(b.page.locator('[data-field="transfer-status"]')).toContainText("COMPLETED");
      await expect(b.page.getByText("You received it", { exact: true })).toBeVisible();
    } else {
      expect(received.status(), "a refused attempt is the sender's record").toBe(404);
      await expectCode(b.page.locator("main"), "NOT_FOUND");
      const bList = (await (await b.page.request.get(`/v1/advanced/transfers?account_id=${b.recipient.accountId}&limit=200`)).json()) as {
        items: Transfer[];
      };
      expect(bList.items.some((item) => item.id === transfer.id)).toBe(false);
    }
    await b.context.close();
  });

  test("an operator is offered reversal only of a COMPLETED transfer, and the backend decides it", async ({
    page,
    browser,
  }) => {
    const b = await prepareRecipient(browser);
    await b.context.close();
    await fundThroughTheFaucet(page, "3");
    const send = page.getByRole("region", { name: "Send", exact: true });
    await reviewSend(send, b.recipient, "1", `e2e reversal ${hex(6)}`);
    const response = await answerTo(page, SEND_PATH, () =>
      send.getByRole("button", { name: "Confirm and send", exact: true }).click(),
    );
    const transferId = transferIdOf(await response.json()) as string;
    expect(transferId).toBeTruthy();

    const operator = await signIn(browser, "admin", true);
    await operator.page.goto(`${SECTION}/${transferId}`);
    await settle(operator.page);
    const status = ((await (await operator.page.request.get(`/v1/advanced/transfers/${transferId}`)).json()) as Transfer).status;
    await expect(operator.page.locator('[data-field="transfer-status"]')).toContainText(status);
    const correction = operator.page.getByRole("region", { name: "Operator correction", exact: true });

    if (status === "COMPLETED") {
      await correction.getByLabel("Reason code", { exact: true }).fill("DISPUTE_UPHELD");
      await correction.getByLabel("Reason", { exact: true }).fill("end-to-end check of the operator correction");
      const reversal = await answerTo(operator.page, `${SEND_PATH}/${transferId}/reverse`, () =>
        correction.getByRole("button", { name: "Reverse this transfer", exact: true }).click(),
      );
      if (reversal.ok()) {
        await expect(correction.locator(`[data-reversed="${transferId}"]`)).toContainText("REVERSED");
      } else {
        // The backend may refuse (a recent strong sign-in, the operator's
        // permissions, funds no longer held); the refusal is on screen with its code.
        await expectCode(correction, ((await reversal.json()) as Problem).code);
      }
    } else {
      // Nothing moved, so nothing can be reversed: the action says why instead
      // of being offered, and the backend refuses it all the same.
      const button = correction.getByRole("button", { name: "Reverse this transfer", exact: true });
      await expect(button).toBeDisabled();
      await expect(correction).toContainText("nothing to reverse");
      const refused = await operator.page.request.post(`${SEND_PATH}/${transferId}/reverse`, {
        headers: {
          "Content-Type": "application/json",
          "Idempotency-Key": `e2e-reversal-${hex(8)}`,
          "Sec-Fetch-Site": "same-origin",
        },
        data: JSON.stringify({ reason_code: "DISPUTE_UPHELD", reason: "end-to-end check of a refused correction" }),
      });
      expect(refused.status()).toBe(409);
      expect(((await refused.json()) as Problem).code).toBe("INVALID_STATE_TRANSITION");
    }
    await operator.context.close();
  });

  test("no dead controls, and no money-shaped figure the API did not send", async ({ page }) => {
    const bodies: unknown[] = [];
    const listener = (response: Response): void => {
      if (new URL(response.url()).pathname.startsWith("/v1/advanced/")) {
        bodies.push(response.json().catch(() => undefined));
      }
    };
    page.on("response", listener);
    await page.goto(SECTION);
    await settle(page);
    page.off("response", listener);

    const buttons = page.locator("main button");
    const count = await buttons.count();
    for (let i = 0; i < count; i = i + 1) {
      const button = buttons.nth(i);
      if (!(await button.isDisabled())) continue;
      const describedBy = await button.getAttribute("aria-describedby");
      expect(describedBy, "a disabled button explains itself").toBeTruthy();
      const reason = page.locator(`[id="${(describedBy as string).replace(/"/g, '\\"')}"]`);
      await expect(reason).toBeVisible();
    }
    // The transfers section states amounts in base-unit quantities; a dollar
    // figure on it could only be one the page invented.
    const text = await page.locator("main").innerText();
    expect(text).not.toMatch(/\$\s?[0-9]/);
    expect((await Promise.all(bodies)).length).toBeGreaterThan(0);
  });
});
