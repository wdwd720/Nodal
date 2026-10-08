/**
 * ECONOMIC ACTIVITY: the three registers every advanced operation writes, for
 * the account the area is reading.
 *
 *   - Economic intents: what was asked for, in which mode, under which
 *     capability, and where it ended. Content-hashed when recorded.
 *   - Economic effects: the exactly-once register. One row per logical
 *     operation, with the journal transaction that applied it and the
 *     evidence recorded beside it; an UNKNOWN effect is never retried blindly.
 *   - Risk Kernel V2 decisions: every verdict with its stable reason codes and
 *     the hashes that make it reproducible.
 *
 * The lists are newest first and are read exactly as the backend pages them.
 * Rows are never filtered here: what the backend returns for this principal
 * and this account is what is drawn.
 */
import { useState, type ReactNode } from "react";
import { Link, Route, Routes, useParams } from "react-router-dom";

import {
  RISK_DECISION_LIMIT,
  useAdvancedRiskDecisions,
  useEconomicEffects,
  useEconomicIntent,
  useEconomicIntents,
  type AdvancedRiskDecision,
  type CursorPage,
  type EconomicEffect,
  type EconomicIntent,
} from "../../../api/advanced/core.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill, Table } from "../../../components/Layout.tsx";
import { BaseUnits, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import {
  EFFECT_STATUSES,
  EFFECT_STATUS_MEANING,
  FIRST_PAGE,
  INTENT_STATUS_MEANING,
  back,
  effectStatusTone,
  forward,
  intentStatusTone,
  verdictTone,
  type CursorTrail,
} from "../../../lib/advanced.ts";
import { exactInstant, formatInstant } from "../../../lib/time.ts";
import { advancedHref, useAdvancedAccount } from "../area.ts";
import { AdvancedNotFound, Codes, CursorPager, ModePill, NoAccount, ScrollTable } from "../shared.tsx";

/** The section: the three registers, and one intent by id. */
export function EconomicActivitySection(): ReactNode {
  return (
    <Routes>
      <Route index element={<EconomicActivity />} />
      <Route path="intents/:intentId" element={<IntentDetail />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}

function intentHref(intentId: string): string {
  return advancedHref("activity", "intents", intentId);
}

function EconomicActivity(): ReactNode {
  const { accountId } = useAdvancedAccount();
  return (
    <Page
      title="Economic activity"
      lead="The intents, exactly-once effects and Risk Kernel V2 decisions the backend has recorded for this account, newest first."
    >
      <SimulatedNotice />
      {accountId === undefined ? (
        <Panel title="Economic intents">
          <NoAccount />
        </Panel>
      ) : (
        <>
          {/* Keyed by account: switching account starts each list again at its first page. */}
          <IntentsPanel key={`intents-${accountId}`} accountId={accountId} />
          <EffectsPanel key={`effects-${accountId}`} accountId={accountId} />
          <RiskDecisionsPanel key={`decisions-${accountId}`} accountId={accountId} />
          <StatusGlossary />
        </>
      )}
    </Page>
  );
}

function IntentsPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const intents = useEconomicIntents(props.accountId, trail.current);
  return (
    <Panel
      title="Economic intents"
      description="Typed requests to change exposure or move simulated value, as recorded. A rejected intent carries the code it was rejected with."
    >
      <AsyncPanel
        query={intents}
        loadingLabel="Loading economic intents…"
        empty={{
          isEmpty: (page: CursorPage<EconomicIntent>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No economic intents",
          body: "The backend has recorded no economic intents for this account. That is an empty register, not an error.",
        }}
      >
        {(page: CursorPage<EconomicIntent>) => (
          <>
            <ScrollTable
              caption="Intents on this page"
              headers={["Recorded", "Kind", "Mode", "Capability", "Status", "Amount", "Actor", "Intent"]}
            >
              {page.items.map((intent) => (
                <tr key={intent.id}>
                  <td>{formatInstant(intent.created_at)}</td>
                  <th scope="row">{intent.kind}</th>
                  <td>
                    <ModePill mode={intent.mode} />
                  </td>
                  <td>
                    <code className="mono-small">{intent.capability}</code>
                  </td>
                  <td>
                    <Pill tone={intentStatusTone(intent.status)}>{intent.status}</Pill>
                    {intent.rejection_code !== undefined && <Codes codes={[intent.rejection_code]} />}
                  </td>
                  <td>
                    <IntentAmount intent={intent} />
                  </td>
                  <td>
                    {intent.actor_type}
                    {intent.agent_id !== undefined && <span className="cell-note">agent {intent.agent_id}</span>}
                  </td>
                  <td>
                    <Link to={intentHref(intent.id)}>
                      <code className="mono-small">{intent.id}</code>
                    </Link>
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="intents"
              trail={trail}
              nextCursor={page.nextCursor}
              onForward={(cursor) => {
                setTrail(forward(trail, cursor));
              }}
              onBack={() => {
                setTrail(back(trail));
              }}
            />
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

/** The amount an intent states: a notional, a quantity, or neither — said as such. */
function IntentAmount(props: { readonly intent: EconomicIntent }): ReactNode {
  const { intent } = props;
  if (intent.notional_usd !== undefined) return <Usd value={intent.notional_usd} />;
  if (intent.quantity !== undefined) {
    return (
      <>
        <BaseUnits value={intent.quantity} />
        {intent.asset_id !== undefined && <span className="cell-note">of asset {intent.asset_id}</span>}
      </>
    );
  }
  return <span className="absent">no amount stated</span>;
}

function EffectsPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const effects = useEconomicEffects(props.accountId, trail.current);
  return (
    <Panel
      title="Economic effects"
      description="The exactly-once register: one row per logical operation, keyed so a replay finds the first outcome instead of applying a second. An effect that posts to the ledger names its journal transaction."
    >
      <AsyncPanel
        query={effects}
        loadingLabel="Loading economic effects…"
        empty={{
          isEmpty: (page: CursorPage<EconomicEffect>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No economic effects",
          body: "The backend has recorded no economic effects for this account. Nothing has been applied, so there is nothing to show.",
        }}
      >
        {(page: CursorPage<EconomicEffect>) => (
          <>
            <ScrollTable
              caption="Effects on this page"
              headers={["Effect", "Status", "Ledger", "Evidence", "External reference", "Intent", "Recorded"]}
            >
              {page.items.map((effect) => (
                <tr key={effect.id}>
                  <th scope="row">
                    {effect.effect_kind} <Pill tone="neutral">{effect.subsystem}</Pill>
                    <span className="cell-note">
                      key <code className="mono-small">{effect.effect_key}</code>
                    </span>
                    <span className="cell-note">
                      id <code className="mono-small">{effect.id}</code>
                    </span>
                  </th>
                  <td>
                    <Pill tone={effectStatusTone(effect.status)}>{effect.status}</Pill>
                    {effect.failure_code !== undefined && <Codes codes={[effect.failure_code]} />}
                  </td>
                  <td>
                    <EffectLedger effect={effect} />
                  </td>
                  <td>
                    {effect.evidence_ref === undefined ? (
                      <span className="absent">no evidence recorded</span>
                    ) : (
                      <>
                        <Identifier value={effect.evidence_ref} />
                        <span className="cell-note">
                          sha256 <Identifier value={effect.evidence_sha256} />
                        </span>
                      </>
                    )}
                  </td>
                  <td>
                    {effect.external_ref === undefined ? (
                      <span className="absent">none</span>
                    ) : (
                      <Identifier value={effect.external_ref} />
                    )}
                  </td>
                  <td>
                    {effect.intent_id === undefined ? (
                      <span className="absent">no intent</span>
                    ) : (
                      <Link to={intentHref(effect.intent_id)}>
                        <code className="mono-small">{effect.intent_id}</code>
                      </Link>
                    )}
                  </td>
                  <td>
                    {formatInstant(effect.created_at)}
                    <span className="cell-note">updated {formatInstant(effect.updated_at)}</span>
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="effects"
              trail={trail}
              nextCursor={page.nextCursor}
              onForward={(cursor) => {
                setTrail(forward(trail, cursor));
              }}
              onBack={() => {
                setTrail(back(trail));
              }}
            />
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function EffectLedger(props: { readonly effect: EconomicEffect }): ReactNode {
  const { effect } = props;
  if (!effect.posts_ledger) return <span className="absent">does not post to the ledger</span>;
  return (
    <>
      {effect.journal_transaction_id === undefined ? (
        <span className="absent">posts to the ledger; no journal transaction yet</span>
      ) : (
        <Identifier label="journal" value={effect.journal_transaction_id} />
      )}
      {effect.compensation_journal_transaction_id !== undefined && (
        <span className="cell-note">
          <Identifier label="compensated by" value={effect.compensation_journal_transaction_id} />
        </span>
      )}
    </>
  );
}

function RiskDecisionsPanel(props: { readonly accountId: string }): ReactNode {
  const decisions = useAdvancedRiskDecisions(props.accountId);
  return (
    <Panel
      title="Risk Kernel V2 decisions"
      description={`Every verdict with its stable reason codes, and the hashes that make it reproducible: the same input and policy always give the same decision hash. The newest ${String(RISK_DECISION_LIMIT)} are shown; the API offers no cursor for older ones.`}
    >
      <AsyncPanel
        query={decisions}
        loadingLabel="Loading risk decisions…"
        empty={{
          isEmpty: (list: AdvancedRiskDecision[]) => list.length === 0,
          title: "No decisions recorded",
          body: "Risk Kernel V2 has recorded no decision about this account's intents. That is an empty register, not an approval.",
        }}
      >
        {(list: AdvancedRiskDecision[]) => (
          <>
            <ScrollTable
              caption="Risk decisions, newest first"
              headers={["Evaluated", "Verdict", "Reason codes", "Intent", "Stage", "Policy", "Evaluator", "Hashes"]}
            >
              {list.map((decision) => (
                <tr key={decision.id}>
                  <td>
                    {formatInstant(decision.evaluated_at)}
                    <span className="cell-note">
                      id <code className="mono-small">{decision.id}</code>
                    </span>
                  </td>
                  <th scope="row">
                    <Pill tone={verdictTone(decision.verdict)}>{decision.verdict}</Pill>
                  </th>
                  <td>
                    <Codes codes={decision.reason_codes} none="none" />
                  </td>
                  <td>
                    {decision.intent_kind}
                    {decision.intent_id !== undefined && (
                      <span className="cell-note">
                        <Link to={intentHref(decision.intent_id)}>
                          <code className="mono-small">{decision.intent_id}</code>
                        </Link>
                      </span>
                    )}
                  </td>
                  <td>{decision.stage}</td>
                  <td>
                    {decision.policy_version}
                    <span className="cell-note">
                      hash <code className="mono-small">{decision.policy_hash}</code>
                    </span>
                  </td>
                  <td>{decision.evaluator_version}</td>
                  <td>
                    <span className="cell-note">
                      input <code className="mono-small">{decision.input_hash}</code>
                    </span>
                    <span className="cell-note">
                      decision <code className="mono-small">{decision.decision_hash}</code>
                    </span>
                  </td>
                </tr>
              ))}
            </ScrollTable>
            {list.length === RISK_DECISION_LIMIT && (
              <p className="note">
                The backend returned exactly {String(RISK_DECISION_LIMIT)} decisions, the most this page asks for.
                Older ones may exist; the API offers no cursor to reach them.
              </p>
            )}
          </>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function StatusGlossary(): ReactNode {
  return (
    <Panel title="What the statuses mean" description="Effect statuses are a closed set the database enforces; intent statuses are listed as this build defines them.">
      <Table caption="Effect statuses and their meaning" headers={["Effect status", "What it means"]}>
        {EFFECT_STATUSES.map((status) => (
          <tr key={status}>
            <th scope="row">
              <Pill tone={effectStatusTone(status)}>{status}</Pill>
            </th>
            <td className="cell-prose">{EFFECT_STATUS_MEANING[status]}</td>
          </tr>
        ))}
      </Table>
      <Table caption="Intent statuses and their meaning" headers={["Intent status", "What it means"]}>
        {Object.entries(INTENT_STATUS_MEANING).map(([status, meaning]) => (
          <tr key={status}>
            <th scope="row">
              <Pill tone={intentStatusTone(status)}>{status}</Pill>
            </th>
            <td className="cell-prose">{meaning}</td>
          </tr>
        ))}
      </Table>
    </Panel>
  );
}

/** One economic intent, exactly as the backend recorded it. */
function IntentDetail(): ReactNode {
  const { intentId } = useParams();
  const { accountId } = useAdvancedAccount();
  const intent = useEconomicIntent(accountId, intentId);
  return (
    <Page
      title="Economic intent"
      lead="One intent, exactly as the backend recorded it."
      actions={<LinkButton to={advancedHref("activity")}>Back to economic activity</LinkButton>}
    >
      <SimulatedNotice />
      <Panel title="Intent" {...(intentId === undefined ? {} : { description: `Requested by id ${intentId}.` })}>
        <AsyncPanel
          query={intent}
          loadingLabel="Loading the intent…"
          errorExtra={
            <p className="explain-body">
              The backend answers NOT_FOUND both for an id it has never recorded and for an intent of an account
              this session may not read, so the answer neither confirms nor denies that the intent exists.
            </p>
          }
        >
          {(found: EconomicIntent) => <IntentFields intent={found} />}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

function IntentFields(props: { readonly intent: EconomicIntent }): ReactNode {
  const { intent } = props;
  return (
    <>
      <FieldGrid columns={3}>
        <Field label="Intent">
          <Identifier value={intent.id} />
        </Field>
        <Field label="Account">
          <Identifier value={intent.account_id} />
        </Field>
        <Field label="Kind">{intent.kind}</Field>
        <Field label="Mode">
          <ModePill mode={intent.mode} />
        </Field>
        <Field label="Capability">
          <code className="mono-small">{intent.capability}</code>
        </Field>
        <Field label="Status" {...noteFor(INTENT_STATUS_MEANING[intent.status])}>
          <Pill tone={intentStatusTone(intent.status)}>{intent.status}</Pill>
          {intent.rejection_code !== undefined && <Codes codes={[intent.rejection_code]} />}
        </Field>
        <Field label="Amount">
          <IntentAmount intent={intent} />
        </Field>
        <Field label="Actor">
          {intent.actor_type}
          {intent.agent_id !== undefined && <span className="cell-note">agent {intent.agent_id}</span>}
        </Field>
        <Field label="Strategy">
          {intent.strategy_ref === undefined ? <span className="absent">none</span> : <Identifier value={intent.strategy_ref} />}
        </Field>
        <Field label="Instrument">
          {intent.instrument_id === undefined ? (
            <span className="absent">none</span>
          ) : (
            <Link to={advancedHref("instruments", intent.instrument_id)}>
              <code className="mono-small">{intent.instrument_id}</code>
            </Link>
          )}
        </Field>
        <Field label="Content hash" note="Covers the intent as recorded; changing any of it would change this hash.">
          <Identifier value={intent.content_hash} />
        </Field>
        <Field label="Recorded" note={exactInstant(intent.created_at)}>
          {formatInstant(intent.created_at)}
        </Field>
        <Field label="Last changed" note={exactInstant(intent.updated_at)}>
          {formatInstant(intent.updated_at)}
        </Field>
        <Field
          label="Ended"
          {...noteFor(intent.terminal_at === undefined ? undefined : exactInstant(intent.terminal_at))}
        >
          {intent.terminal_at === undefined ? <span className="absent">not ended</span> : formatInstant(intent.terminal_at)}
        </Field>
      </FieldGrid>
      <h3>Terms, as recorded</h3>
      {/* Focusable because it scrolls: a keyboard user must be able to reach all of it. */}
      <pre className="raw-block" role="region" aria-label="Intent terms as recorded" tabIndex={0}>
        {JSON.stringify(intent.terms, null, 2)}
      </pre>
    </>
  );
}

/** A Field note only when there is one: an absent note is omitted, not passed as undefined. */
function noteFor(note: string | undefined): { readonly note?: string } {
  return note === undefined ? {} : { note };
}
