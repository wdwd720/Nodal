# Runbook: model malfunction (LLM provider or model behaviour)

Severity: SEV2 when CANARY/LIMITED/LIVE agents are affected; SEV3 otherwise (PART 158: the deterministic/manual product stays operational) · Owner: RISK (policy), OPERATIONS (containment) · Related: [stale-market-data.md](./stale-market-data.md), [admin-compromise.md](./admin-compromise.md), `docs/architecture/AGENT_RUNTIME.md` §6–7, `docs/api/providers/anthropic.md`

## Trigger

- `policy_violations` counter rising (agent output attempting a forbidden effect, schema violation with a forbidden field, prompt-injection corpus hits).
- `agent_pauses` with `reason_code = MODEL_UNAVAILABLE` (auto-pause after `config.Agents.ModelFailurePauseAfter` consecutive failures) or `BUDGET_EXHAUSTED`.
- `model_cost` histogram / daily `model_calls.cost_usd_minor` sum far above baseline (looping, retries: the provider has no idempotency key, every retry is billable; a 429 can be the spend cap `enforced_spend_limit_reached`).
- `model_calls.parse_result` in `INVALID_JSON | SCHEMA_VIOLATION | TOO_LARGE` at an elevated rate; `error_code` bursts; a model id retired or changed by the provider.
- Qualitative: a model producing systematically wrong predictions (calibration collapse in `calibration_snapshots`), or rationale text that reveals it is following injected instructions.
- PENDING: `internal/agent`, `internal/model`, `internal/strategy`, `cmd/agent-worker`; the tables (`model_calls`, `agent_runs`, `agent_pauses`, `predictions`, `calibration_snapshots`) and the `MODEL_DISABLE` switch exist.

## Blast radius

- Money safety is unaffected by construction: the model proposes, deterministic systems authorise (POLICY_AUTHORITY.md). A model cannot sign, transfer, withdraw, change risk or capital, reach admin APIs or the gate subsystem (closed agent permission set, depguard rules, `CREATE_TRADE_INTENT` is the only money-adjacent effect and it still passes eligibility, risk `PRE_TRADE`/`FINAL`, reservation, compiler and inspector).
- Affected: strategies with `CALL_MODEL` in their effect set; their runs `SKIPPED{MODEL_UNAVAILABLE}` if the model is required, or continue with the model output absent if optional. No synthetic output, no heuristic fallback (PART 177).
- Not affected: manual trading, deterministic strategies without a model dependency, funding, reconciliation, ledger, everything else.
- Cost: unbounded charges are prevented by persisted per-day budgets (`model_calls` sums) checked before dialing.

## Immediate actions (first 10 minutes)

1. Measure (read-only):
   ```sql
   SELECT provider, model_id, purpose, parse_result, error_code, success, count(*), sum(cost_usd_minor)
     FROM model_calls WHERE request_at > now() - interval '1 hour' GROUP BY 1,2,3,4,5,6 ORDER BY 7 DESC;
   SELECT reason_code, count(*) FROM agent_pauses WHERE resumed_at IS NULL GROUP BY 1;
   SELECT a.id, a.mode FROM agents a JOIN agent_pauses p ON p.agent_id = a.id AND p.resumed_at IS NULL WHERE a.mode IN ('CANARY','LIMITED','LIVE');
   ```
2. If the model is producing *wrong or hostile* output (not merely unavailable): `POST /admin/kill-switches {"kind":"MODEL_DISABLE","scope_id":"<model_id>" or "*","action":"activate","reason":"<INC-id>: ..."}` (STANDARD; `kill:activate`; PENDING `cmd/api`). The ToolBroker refuses `CALL_MODEL` under `MODEL_DISABLE`; required-model runs skip; nothing else changes.
3. If the model is merely *unavailable*, do nothing: auto-pause and skip semantics already hold. Confirm no synthetic outputs exist: `SELECT count(*) FROM model_calls WHERE success = false AND structured_output IS NOT NULL;` must be 0.
4. If any LIVE/LIMITED agent shows `policy_violations` or an intent was created from a run whose model output failed schema validation (impossible by design; check `agent_runs` → `trade_intents` join with `model_calls.parse_result <> 'OK'`), pause that agent (`AGENT_PAUSE(<agent_id>)`) and treat it as a SEV1 authority-boundary defect.
5. Announce which models/agents are disabled or paused and that manual trading is unaffected.

## Diagnosis

- Provider status and changes: model id retirement, structured-output (`output_config.format`) behaviour, tier spend cap (`docs/api/providers/anthropic.md`). Cost accounting must sum `input_tokens + cache_creation_input_tokens + cache_read_input_tokens`.
- Prompt-injection: the three fixed segments (`SYSTEM POLICY / TOOL RESULTS / UNTRUSTED CONTENT`) mean content cannot add a tool, effect or destination; review `prompt_ref`/`response_ref` archives (retention class `MODEL_IO`) for the run in question; check the `EFFECT_FORBIDDEN` rejections in `compile_attempts`.
- Loop detection: `agent_runs` per agent per minute vs `IR` limits; `tool_invocations` cost per day vs envelope `max_data_spend_usd_minor`.
- Calibration: `calibration_snapshots` and `prediction_outcomes` for the model/version; a genuine model regression is a promotion/demotion decision for RISK, not an incident control.
- Is the "malfunction" actually stale data? `agent_runs.evidence` freshness per dependency; see [stale-market-data.md](./stale-market-data.md).

## Containment and recovery

1. Keep `MODEL_DISABLE` until the provider incident is resolved or the model version is replaced by configuration (model ids are configuration, never code).
2. Agents auto-paused for `MODEL_UNAVAILABLE` are resumed by their owner or an operator (`agent_pauses.resumed_by_*`) once model calls succeed again in a dry run; agents paused for `RISK_VIOLATION`/`SECURITY` need RISK sign-off and, for a promotion-stage agent, the `AGENT_PROMOTE`/demotion path (dual control).
3. Release `MODEL_DISABLE`: STANDARD severity, `kill:release` (break-glass) + step-up; record the provider incident reference in the release reason.
4. If a schema/effect defect is found in the compiler or ToolBroker, it is a release blocker: the corpus test (STRATEGY_IR.md §11) with the new case must pass before any LIVE agent resumes.
5. Budget overrun: no financial repair; adjust envelope budgets through `ENVELOPE_AUTHORITY_CHANGE` (dual control) if policy should change.

## What NOT to do

- Never add a heuristic or "safe default" output path so strategies keep trading through a model outage (PART 177).
- Never give the model retry budget by raising limits in an env var; budgets are persisted policy.
- Never disable the schema validation or the effect system to "unblock" an agent.
- Never treat model rationale as evidence of anything financial; the ledger and reconciliation are the only truth.
- Never activate `GLOBAL_NEW_RISK_KILL` for a model outage; the blast radius is agents with a model dependency only.

## Verification / exit criteria

- `model_calls` success rate and `parse_result = 'OK'` rate back to baseline for 30 minutes; spend within budget.
- No open `agent_pauses` with `MODEL_UNAVAILABLE` except deliberately kept ones; `MODEL_DISABLE` released with evidence.
- No `policy_violations` in the window after recovery; zero intents created from failed-schema runs.
- If a hostile-output case was found: corpus test extended and green.

## Post-incident

- Archive the affected runs (`agent_runs`, `model_calls`, prompt/response archive refs, hashes) and the pause/resume audit rows.
- Security event review: prompt-injection attempt ⇒ record as a security finding (PENDING: a `security_events` kind for it).
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-177-1, R-197-1, R-053-12; add the model-outage chaos test (`test/chaos/llm_outage_test.go`) to BLOCKERS if absent.
