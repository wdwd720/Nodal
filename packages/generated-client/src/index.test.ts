import assert from "node:assert/strict";
import { test } from "node:test";

import { ApiProblem, createApiClient, idempotent, newIdempotencyKey } from "./index.ts";

test("problem+json responses become ApiProblem with the backend code", async () => {
  const fetchImpl: typeof fetch = async () =>
    new Response(
      JSON.stringify({
        type: "urn:problem:insufficient_buying_power",
        title: "Insufficient buying power",
        status: 422,
        code: "INSUFFICIENT_BUYING_POWER",
        fields: { available: "10.00", requested: "500.00" },
        request_id: "req-1",
      }),
      { status: 422, headers: { "content-type": "application/problem+json" } },
    );
  const client = createApiClient({ baseUrl: "https://api.test/v1", fetch: fetchImpl });
  await assert.rejects(
    () => client.GET("/accounts/{accountId}/buying-power", { params: { path: { accountId: "0192" } } }),
    (err: unknown) => {
      assert.ok(err instanceof ApiProblem);
      assert.equal(err.code, "INSUFFICIENT_BUYING_POWER");
      assert.equal(err.status, 422);
      assert.equal(err.requestId, "req-1");
      assert.ok(err.isBusinessRejection);
      return true;
    },
  );
});

test("requests carry a request id and the idempotency header when supplied", async () => {
  let seen: Headers | undefined;
  const fetchImpl: typeof fetch = async (input) => {
    seen = new Request(input).headers;
    return new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
  };
  const client = createApiClient({ baseUrl: "https://api.test/v1", fetch: fetchImpl, requestId: () => "fixed-id" });
  const key = newIdempotencyKey();
  await client.POST("/intents", {
    ...idempotent(key),
    body: { account_id: "a", instrument_id: "i", action: "ACQUIRE_NOTIONAL", notional_usd: "10.00", mode: "PAPER" },
  });
  assert.ok(seen);
  assert.equal(seen.get("X-Request-Id"), "fixed-id");
  assert.equal(seen.get("Idempotency-Key"), key);
});

test("idempotent() rejects keys outside 8–128 characters and merges params", () => {
  assert.throws(() => idempotent("short"));
  const p = idempotent("long-enough-key", { path: { intentId: "x" } });
  assert.equal(p.params.header["Idempotency-Key"], "long-enough-key");
  assert.equal(p.params.path.intentId, "x");
});
