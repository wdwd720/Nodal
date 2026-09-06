# Anthropic Messages API — verified integration notes

Role in this platform: **ModelProvider** (natural-language strategy compiler producing schema-constrained JSON).
Overall status: **VERIFIED_FROM_OFFICIAL_DOCS**.

## Verification record

All fetched 2026-09-05. `docs.claude.com` now redirects (301/302) to `platform.claude.com`.

| Source | URL | Result |
|---|---|---|
| Messages API reference | https://platform.claude.com/docs/en/api/messages | OK |
| API overview (headers) | https://platform.claude.com/docs/en/api/overview | OK |
| Models overview | https://platform.claude.com/docs/en/about-claude/models/overview | OK |
| Structured outputs | https://platform.claude.com/docs/en/build-with-claude/structured-outputs | OK |
| Rate limits | https://platform.claude.com/docs/en/api/rate-limits | OK |
| Errors | https://platform.claude.com/docs/en/api/errors | OK |
| Pricing | https://platform.claude.com/docs/en/about-claude/pricing | OK |
| Go SDK release | https://github.com/anthropics/anthropic-sdk-go/releases/latest ; https://pkg.go.dev/github.com/anthropics/anthropic-sdk-go?tab=versions ; https://raw.githubusercontent.com/anthropics/anthropic-sdk-go/main/README.md | OK |
| Failed | https://pkg.go.dev/github.com/anthropics/anthropic-sdk-go (page > 10 MB) | size limit |

## Model IDs (current lineup, verbatim from models overview)

| Model | Claude API ID | Context | Max output | Notes |
|---|---|---|---|---|
| Claude Fable 5.1 | `claude-fable-5-1` | 1M | 128K | thinking always on; **forced `tool_choice` (`any`/`tool`) returns 400** |
| Claude Opus 5 | `claude-opus-5` | 1M | 128K | recommended default ("start with Claude Opus 5 for most workloads") |
| Claude Sonnet 5 | `claude-sonnet-5` | 1M | 128K | |
| Claude Haiku 4.5 | `claude-haiku-4-5-20251001` (alias `claude-haiku-4-5`) | 200K | 64K | extended thinking only |

"Every Claude model ID is a pinned snapshot, including the dateless IDs used from the 4.6 generation on." Retirement: Fable 5.1 not before 2027-09-01; Opus 5 not before 2027-07-24; Sonnet 5 not before 2027-06-30; Haiku 4.5 not before 2026-10-15. Legacy still served: Fable 5, Opus 4.8/4.7/4.6/4.5, Sonnet 4.6/4.5. Capabilities can be read live from `GET /v1/models` (`max_input_tokens`, `max_tokens`, `capabilities`).

Recommendation for the compiler: `claude-opus-5` with `output_config.format` (JSON schema) — avoids the Fable 5.1 forced-tool-use restriction and the prefill removal.

## Auth and secrets

Base `https://api.anthropic.com`. Headers: `Authorization: Bearer <key or WIF token>` (or legacy `x-api-key`), `anthropic-version: 2023-06-01`, `content-type: application/json`, optional `anthropic-workspace-id` (required for multi-workspace keys), `anthropic-beta` for beta features. Keys have configurable expiration. Use a dedicated workspace with its own spend/rate limits for this service.

## Endpoints and schemas

`POST /v1/messages` (32 MB max request). Also `/v1/messages/count_tokens`, `/v1/messages/batches` (256 MB; 50% discount), `/v1/models`, `/v1/files`, `/v1/skills`.

Request fields: `model` (string, required), `messages[]` ({role: user|assistant, content}), `max_tokens` (number, required; "models may stop before reaching this maximum"), `system` (string | TextBlockParam[]), `tools[]`, `tool_choice` (auto | any | tool | none), `output_config{format, effort: low|medium|high|xhigh|max}`, `thinking` (`{type:"adaptive"}` on 4.6+; `budget_tokens` only on Haiku 4.5 and older; `{type:"disabled"}`/`enabled` rejected on Fable 5.x), `temperature` (0-1, **deprecated**), `stop_sequences[]`, `stream` (bool, SSE), `service_tier` (`auto`|`standard_only`), `metadata.user_id` (<=512 chars — put the tenant/user hash here for abuse attribution).

Response: `id` (msg_...), `type:"message"`, `role:"assistant"`, `model`, `content[]` (text | tool_use | thinking ...), `stop_reason` (`end_turn`, `max_tokens`, `stop_sequence`, `tool_use` on the reference page; `refusal` and `pause_turn` exist per the errors/streaming docs and SDK — always treat unknown values as failure), `stop_sequence`, `usage`, plus `service_tier`, `inference_geo`, `speed`.

Usage fields for cost accounting (all integers): `input_tokens` (tokens **after the last cache breakpoint** only), `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `cache_creation{ephemeral_5m_input_tokens, ephemeral_1h_input_tokens}`; server tools add `server_tool_use{...}`. Verbatim: `total_input_tokens = cache_read_input_tokens + cache_creation_input_tokens + input_tokens`. Bill = base x input_tokens + 1.25x (5m) or 2x (1h) x cache writes + 0.1x (0.025x Fable 5.1) x cache reads + output rate x output_tokens; `inference_geo:"us"` adds 1.1x; batch 0.5x.

### Structured output (GA, no beta header)

Two mechanisms:
1. **JSON output**: `output_config.format = {type:"json_schema", schema:{...}}` — response text block is JSON matching the schema.
2. **Strict tool use**: `tools[i].strict = true` with `input_schema` having `additionalProperties:false` and `required` — guarantees `tool_use.input` validates.

Supported models include `claude-fable-5-1`, `claude-opus-5`, `claude-sonnet-5`, `claude-haiku-4-5-20251001` and 4.x models. Schema subset: object/array/string/integer/number/boolean/null, `enum` (primitives only), `const`, `anyOf`/`allOf` (no `allOf`+`$ref`), local `$ref`/`$defs`, `default`, `required`, `additionalProperties:false` (mandatory), string formats `date-time, time, date, duration, email, hostname, uri, ipv4, ipv6, uuid`, array `minItems` 0|1. **Not supported**: recursive schemas, external `$ref`, `minimum/maximum/multipleOf`, `minLength/maxLength`, other array constraints. Grammar compiles on first use and is cached 24 h (invalidated by schema or tool-set changes, not by name/description). Old `output_format` field and `structured-outputs-2025-11-13` beta header accepted "for a transition period" only. Numeric-range and string-length constraints must be enforced by the caller after parsing.

## Errors, rate limits, retries

Error body: `{type:"error", error:{type, message, details?}, request_id}`. Codes: 400 `invalid_request_error` (also self-set spend limit), 401 `authentication_error`, 402 `billing_error`, 403 `permission_error`, 404 `not_found_error`, 409 `conflict_error`, 413 `request_too_large`, 429 `rate_limit_error` (rate limit **or** tier spend cap — spend cap has **no `retry-after`** and `error.details.error_code = "enforced_spend_limit_reached"`), 500 `api_error`, 504 `timeout_error`, 529 `overloaded_error`. Mid-stream SSE errors arrive as `error` events after a 200.

Headers: `request-id` (also `request_id` in error bodies — persist per call), `anthropic-organization-id`, `anthropic-workspace-id`, `retry-after` (seconds), `anthropic-ratelimit-requests-{limit,remaining,reset}`, `anthropic-ratelimit-tokens-{limit,remaining,reset}` (most restrictive limit), `anthropic-ratelimit-input-tokens-*`, `anthropic-ratelimit-output-tokens-*` (reset values RFC 3339; remaining rounded to nearest thousand). Rate limits are token-bucket, per model class, per organization; cache reads do not count toward ITPM. Standard tiers (RPM / ITPM / OTPM): Start — Opus 5 & Sonnet 5 1,000 / 2,000,000 / 400,000, Fable 5.x 1,000 / 500,000 / 100,000; Build — 5,000 / 5,000,000 / 1,000,000 (Fable 2,000 / 1,500,000 / 300,000); Scale — 10,000 / 10,000,000 / 2,000,000 (Fable 4,000 / 4,000,000 / 800,000). Monthly spend caps: Start $500, Build $1,000, Scale $200,000. Acceleration limits can 429 on sharp ramps.

**Idempotency: no idempotency-key request header is documented** on the API overview or Messages reference. Every retry is a new billable request. SDKs auto-retry 2x on 408/409/429/5xx and connection errors honouring `retry-after`; set `max_retries` deliberately and dedupe at the application layer (hash of request -> cached result).

| Endpoint | Class | Rationale |
|---|---|---|
| POST /v1/messages | UNKNOWN_EFFECT_WRITE (billing) | No side effects beyond spend; a timed-out request may still have been billed. Safe to retry functionally; cap retries for cost. |
| POST /v1/messages/batches | UNKNOWN_EFFECT_WRITE | Duplicate batches double-bill; key results by `custom_id`. |
| POST /v1/messages/count_tokens, GET /v1/models | SAFE_RETRY | |

Long requests: use streaming for anything that may exceed ~10 minutes; SDKs reject non-streaming requests expected to exceed the 10-minute timeout.

## Webhooks/streams

No webhooks. Streaming via SSE (`stream:true`); accumulate to a final `Message` with the SDK helper.

## Sandbox/test availability

No sandbox/test mode. Use a separate workspace with a low self-set spend limit (returns 400 `invalid_request_error` when hit) and `metadata.user_id` tagging. Batch API for offline evaluation at 50% cost.

## Pricing (official page, USD per MTok, fetched 2026-09-05)

| Model | Input | 5m cache write | 1h cache write | Cache read | Output |
|---|---|---|---|---|---|
| Claude Fable 5.1 | $10 | $12.50 | $20 | $0.25 | $50 |
| Claude Opus 5 | $5 | $6.25 | $10 | $0.50 | $25 |
| Claude Sonnet 5 | $2 | $2.50 | $4 | $0.20 | $10 |
| Claude Haiku 4.5 | $1 | $1.25 | $2 | $0.10 | $5 |

Batch: 50% off both directions. 1M context at standard pricing. Tool-use system prompt overhead: Opus 5 286 tokens (`auto`/`none`) / 406 (`any`/`tool`); Sonnet 5 354 / 474. Web search $10 per 1,000 searches (not needed here). Source: https://platform.claude.com/docs/en/about-claude/pricing.

## Go SDK

Module `github.com/anthropics/anthropic-sdk-go`, latest **v1.71.0 (2026-09-04)**, requires **Go 1.24+**; install `go get -u 'github.com/anthropics/anthropic-sdk-go@v1.71.0'`. Client `anthropic.NewClient(option.WithAPIKey(...))`; `client.Messages.New(ctx, anthropic.MessageNewParams{Model, MaxTokens, Messages, ...})`; streaming `client.Messages.NewStreaming` + `message.Accumulate(event)`; request id via `option.WithResponseInto(&resp)` then `resp.Header.Get("request-id")`; errors as `*anthropic.Error` (branch on `StatusCode`); strict tools `Strict: anthropic.Bool(true)` with `additionalProperties` via `InputSchema.ExtraFields`; thinking `anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}}`. Typed model constants lag launches — pass the model ID as a plain string. `option.WithMaxRetries`, `option.WithRequestTimeout` configure retry/timeout (defaults: 2 retries, 10-minute timeout).

## Worked request shape (compiler call)

```json
{
  "model": "claude-opus-5",
  "max_tokens": 16000,
  "system": [{"type": "text", "text": "<frozen compiler instructions>", "cache_control": {"type": "ephemeral"}}],
  "messages": [{"role": "user", "content": "<natural-language strategy>"}],
  "output_config": {
    "effort": "high",
    "format": {"type": "json_schema", "schema": {"type": "object", "properties": {"...": {}}, "required": ["..."], "additionalProperties": false}}
  },
  "metadata": {"user_id": "<tenant-hash>"}
}
```

Handling rules derived from the docs above:
- Check `stop_reason` before reading `content`; only `end_turn` with a parseable JSON text block is a successful compile. `max_tokens` means truncated output — do not attempt to repair JSON, re-run with a higher `max_tokens`.
- Keep `system` and the schema byte-stable so the 24-hour grammar cache and the prompt cache both hit; verify with `usage.cache_read_input_tokens > 0` on the second call.
- Persist `request-id`, `model`, the four usage counters, and `service_tier` per call for cost attribution.
- Validate the parsed JSON against the same schema locally (numeric ranges, string lengths, cross-field rules are not enforced server-side).
- On 429 with `retry-after`, sleep and retry; on 429 without `retry-after` and `error_code=enforced_spend_limit_reached`, fail fast and alert (retries will not succeed until the next month or a tier change).
- On 529 `overloaded_error` / 500 `api_error`, exponential backoff with a bounded retry count; each retry is billed if it reaches the model.

## Open questions / unverified

1. Complete `stop_reason` enum on the Messages reference page (fetched summary showed four values; `refusal`/`pause_turn` known from other official pages/SDK).
2. Whether `output_config.format` and `strict` tools can be combined with `effort:"low"` without quality loss for schema compilation — needs eval.
3. Exact Go type names for `OutputConfig`/`JSONOutputFormat` in v1.71.0 (README did not show them; confirm with `go doc`).
