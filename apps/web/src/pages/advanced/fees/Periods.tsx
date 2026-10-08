/**
 * FEES — performance fees, a sandbox accounting that is never charged live.
 *
 * A performance-fee period of an (account, strategy) pair opens with a
 * high-water mark and chains from the previous crystallized period. Each
 * valuation adds one line that never edits an earlier one: nothing accrues
 * below the high-water mark or the hurdle, and a fall is a compensating
 * REVERSAL line. At its end a period crystallizes: the fee is charged, the
 * high-water mark moves to where the next period opens, the fee is
 * attributed to strategies, and the statement is stored with its hash. A
 * deposit raises the high-water mark by itself and a withdrawal scales it, so
 * no flow creates or destroys performance.
 *
 * Every figure is the fee ledger's, in US dollars to the cent. Operators
 * additionally open, value, crystallize, cancel and reverse periods; those
 * that charge or return a fee ask for a recent strong sign-in.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useAccruePeriod,
  useCancelPeriod,
  useCrystallizePeriod,
  useFeePeriod,
  useFeePeriods,
  useOpenPeriod,
  useRecordFlow,
  useReversePeriod,
  type AccrueRequest,
  type CancelPeriodRequest,
  type CrystallizeRequest,
  type FeeContribution,
  type FeeFlowResult,
  type FeeLineResult,
  type FeeOpenPeriodRequest,
  type FeePeriod,
  type FeePerformanceLine,
  type FeeStatement,
  type FlowRequest,
  type ReverseRequest,
} from "../../../api/advanced/fees.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { Button } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { AsOf, Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Absent, Bps, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import {
  FEE_RUN_MODES,
  LINE_KIND_MEANING,
  PERIOD_STATUS_MEANING,
  parseReasonCode,
  parseStrategyRef,
  parseUsdInput,
  parseUtcInstant,
  periodStatusTone,
} from "../../../lib/fees.ts";
import { parseText, parseUuid, parseWholeNumber } from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { Choice, CommandFlow, CommandForm, TextArea, TextInput, prepare } from "../treasury/command.tsx";
import { FeesNav, RecordIds, VerificationView, feesHref, useIsOperator } from "./parts.tsx";

/** A whole number of basis points that may be negative or exceed 100% (a return), shown exactly. */
function SignedBps(props: { readonly value: number | undefined; readonly absent: string }): ReactNode {
  if (props.value === undefined) return <Absent what={props.absent} />;
  return <Bps value={props.value} />;
}

export function FeePeriodsPage(): ReactNode {
  const account = useAdvancedAccount();
  const operator = useIsOperator();
  return (
    <Page
      title="Performance fees"
      lead="This account's performance-fee periods: the high-water mark each opened with, every valuation's line, and what each crystallization charged. A sandbox calculation, never charged live."
    >
      <SimulatedNotice />
      <FeesNav />
      {account.accountId === undefined ? <NoAccount /> : <PeriodsPanel accountId={account.accountId} />}
      {operator && <OpenPeriodPanel defaultAccount={account.accountId} />}
    </Page>
  );
}

function PeriodsPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const [strategyDraft, setStrategyDraft] = useState("");
  const [strategy, setStrategy] = useState<string | undefined>(undefined);
  const periods = useFeePeriods(props.accountId, strategy, trail.current);
  const parsedStrategy = strategyDraft.trim() === "" ? undefined : parseStrategyRef(strategyDraft);
  return (
    <Panel title="This account's periods" description="Newest first, every strategy unless one is chosen.">
      <CommandForm label="Choose a strategy">
        <TextInput id="fees-periods-strategy" label="Strategy reference (optional)" value={strategyDraft} onChange={setStrategyDraft} />
        <div className="form-actions">
          {parsedStrategy !== undefined && !parsedStrategy.ok ? (
            <Button disabledReason={parsedStrategy.error}>Show</Button>
          ) : (
            <Button
              onClick={() => {
                setStrategy(parsedStrategy?.ok === true ? parsedStrategy.value : undefined);
                setTrail(FIRST_PAGE);
              }}
            >
              Show
            </Button>
          )}
        </div>
      </CommandForm>
      <AsyncPanel
        query={periods}
        loadingLabel="Loading the performance-fee periods…"
        empty={{
          isEmpty: (page: CursorPage<FeePeriod>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No performance-fee periods",
          body: "The backend has no performance-fee period of this account.",
        }}
      >
        {(page: CursorPage<FeePeriod>) => (
          <>
            <ScrollTable
              caption="Performance-fee periods"
              headers={["Period", "Strategy", "Status", "Opened with high-water mark", "Running accrual", "Fee charged", "High-water mark after"]}
            >
              {page.items.map((period) => (
                <tr key={period.id} data-period={period.id}>
                  <th scope="row">
                    <Link to={feesHref("periods", period.id)}>
                      {formatInstant(period.terms.period_start)} – {formatInstant(period.terms.period_end)}
                    </Link>
                    <span className="cell-note">#{String(period.seq)}</span>
                  </th>
                  <td>
                    <code className="mono-small">{period.strategy_ref}</code>
                  </td>
                  <td>
                    <Pill tone={periodStatusTone(period.status)}>{period.status}</Pill>
                  </td>
                  <td>
                    <Usd value={period.terms.prior_hwm} />
                  </td>
                  <td>
                    <Usd value={period.accrued} />
                  </td>
                  <td>{period.statement === undefined ? <Absent what="not crystallized" /> : <Usd value={period.statement.fee_amount} />}</td>
                  <td>{period.statement === undefined ? <Absent what="not crystallized" /> : <Usd value={period.statement.hwm_after} />}</td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="periods"
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

/* --------------------------------------------------------------------------
 * One period
 * ------------------------------------------------------------------------ */

export function FeePeriodPage(): ReactNode {
  const { periodId = "" } = useParams();
  const account = useAdvancedAccount();
  const period = useFeePeriod(account.accountId, periodId);
  const operator = useIsOperator();
  return (
    <Page title="Performance-fee period" lead="One period: its terms and high-water mark, its flows, every valuation's line, and the statement its crystallization stored.">
      <SimulatedNotice />
      <FeesNav />
      <AsyncPanel query={period} loadingLabel="Loading the period…">
        {(data: FeePeriod) => (
          <>
            <TermsPanel period={data} />
            <FlowsPanel period={data} />
            <LinesPanel lines={data.lines} />
            {data.statement !== undefined && <StatementPanel period={data} statement={data.statement} />}
            {operator && <OperatorPanels period={data} />}
          </>
        )}
      </AsyncPanel>
    </Page>
  );
}

function TermsPanel(props: { readonly period: FeePeriod }): ReactNode {
  const { period } = props;
  const t = period.terms;
  return (
    <Panel title={`${period.strategy_ref} — period ${String(period.seq)}`} description={PERIOD_STATUS_MEANING[period.status]}>
      <p className="meta-row">
        <span data-field="period_status">
          <Pill tone={periodStatusTone(period.status)}>{period.status}</Pill>
        </span>
        <span className="mono-small">{period.mode}</span>
        <span className="mono-small">{t.terms_version}</span>
        <span className="mono-small">terms {period.terms_hash}</span>
      </p>
      {period.verification !== undefined && <VerificationView verification={period.verification} />}
      <FieldGrid columns={3}>
        <Field label="Period">
          {formatInstant(t.period_start)} – {formatInstant(t.period_end)}
        </Field>
        <Field label="Opening NAV" note="Gross of performance fees.">
          <Usd value={t.opening_nav} />
        </Field>
        <Field label="High-water mark it opened with" emphasis note="No fee accrues on value below it.">
          <span data-field="prior_hwm">
            <Usd value={t.prior_hwm} />
          </span>
        </Field>
        <Field label="Hurdle" note="Annual, applied pro rata to the time elapsed.">
          <Bps value={t.hurdle_annual_bps} />
        </Field>
        <Field label="Fee rate" note="Of what is above the high-water mark and the hurdle.">
          <Bps value={t.fee_rate_bps} />
        </Field>
        <Field label="Crystallization">{t.crystallization}</Field>
        <Field label="Running accrual" note="Provisional until the period crystallizes; nothing has been charged.">
          <span data-field="accrued">
            <Usd value={period.accrued} />
          </span>
        </Field>
        <Field label="Last valuation">{period.last_valuation_at === undefined ? <Absent what="not valued yet" /> : formatInstant(period.last_valuation_at)}</Field>
        <Field label="Returns, gross / net of the fee" note="Modified Dietz, rounded toward zero; only once crystallized.">
          <SignedBps value={period.gross_return_bps} absent="not stated" />
          <span className="cell-note">
            <SignedBps value={period.net_return_bps} absent="not stated" />
          </span>
        </Field>
      </FieldGrid>
      {period.status === "REVERSED" && (
        <p className="note" role="status">
          The crystallization was reversed {period.reversed_at === undefined ? "" : formatInstant(period.reversed_at)} with reason code{" "}
          <code className="mono-small">{period.reversal_reason_code ?? "not stated"}</code>: the fee was returned in full.
        </p>
      )}
      <RecordIds
        ids={[
          ["period", period.id],
          ["account", period.account_id],
          ["previous period", period.previous_period_id],
          ["fee asset", period.fee_asset_id],
          ["crystallization journal", period.crystallization_journal_transaction_id],
          ["reversal journal", period.reversal_journal_transaction_id],
        ]}
      />
    </Panel>
  );
}

function FlowsPanel(props: { readonly period: FeePeriod }): ReactNode {
  return (
    <Panel title="Flows" description="Deposits (positive) and withdrawals (negative), in time order. A deposit raises the high-water mark by itself; a withdrawal scales it.">
      {props.period.flows.length === 0 ? (
        <p className="note" role="status">
          No flow was recorded in this period.
        </p>
      ) : (
        <ScrollTable caption="Flows" headers={["Flow", "At", "Amount", "NAV before", "Recorded by"]}>
          {props.period.flows.map((flow) => (
            <tr key={flow.id}>
              <th scope="row">{String(flow.seq)}</th>
              <td>{formatInstant(flow.at)}</td>
              <td>
                <Usd value={flow.amount} signed />
              </td>
              <td>
                <Usd value={flow.nav_before} />
              </td>
              <td>
                {flow.actor_type} <code className="mono-small">{flow.actor_id}</code>
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </Panel>
  );
}

function LinesPanel(props: { readonly lines: readonly FeePerformanceLine[] }): ReactNode {
  return (
    <Panel
      title="Lines"
      description="One line per valuation, never edited: the high-water mark and hurdle it was measured against, what was above them, and the change in the running accrual."
    >
      {props.lines.length === 0 ? (
        <p className="note" role="status">
          The period has not been valued yet.
        </p>
      ) : (
        <ScrollTable
          caption="Performance-fee lines"
          headers={["Line", "Kind", "At", "NAV", "High-water mark", "Hurdle threshold", "Fee base", "Amount", "Accrual before → after"]}
        >
          {props.lines.map((line) => (
            <tr key={line.id} data-line-kind={line.kind}>
              <th scope="row">{String(line.seq)}</th>
              <td>
                <code className="mono-small">{line.kind}</code>
                <span className="cell-note">{LINE_KIND_MEANING[line.kind]}</span>
              </td>
              <td>{formatInstant(line.at)}</td>
              <td>
                <Usd value={line.nav} />
              </td>
              <td>
                <Usd value={line.hwm_adjusted} />
              </td>
              <td>
                <Usd value={line.hurdle_threshold} />
              </td>
              <td>
                <Usd value={line.fee_base} />
              </td>
              <td>
                <Usd value={line.amount} signed />
              </td>
              <td>
                <Usd value={line.accrual_before} /> → <Usd value={line.accrual_after} />
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
    </Panel>
  );
}

function StatementPanel(props: { readonly period: FeePeriod; readonly statement: FeeStatement }): ReactNode {
  const st = props.statement;
  return (
    <Panel title="Statement — simulated" description="The deterministic statement the crystallization stored with its hash. It can be replayed byte for byte from the terms, the flows and the valuations.">
      {props.period.crystallized_at !== undefined && <AsOf at={props.period.crystallized_at} label="Crystallized at" />}
      <p className="meta-row">
        <span className="mono-small">{st.kind}</span>
        {props.period.statement_hash !== undefined && <span className="mono-small">hash {props.period.statement_hash}</span>}
      </p>
      <FieldGrid columns={3}>
        <Field label="Opening NAV">
          <Usd value={st.opening_nav} />
        </Field>
        <Field label="Net flows">
          <Usd value={st.net_flows} signed />
        </Field>
        <Field label="Closing NAV, gross">
          <Usd value={st.closing_nav_gross} />
        </Field>
        <Field label="Gross PnL">
          <Usd value={st.gross_pnl} signed />
        </Field>
        <Field label="High-water mark before / adjusted" note="Adjusted for the flows.">
          <Usd value={st.hwm_before} />
          <span className="cell-note">
            <Usd value={st.hwm_adjusted} />
          </span>
        </Field>
        <Field label="Hurdle threshold">
          <Usd value={st.hurdle_threshold} />
          <span className="cell-note">
            <Bps value={st.hurdle_annual_bps} /> a year
          </span>
        </Field>
        <Field label="Fee base" note="Only value above both the high-water mark and the hurdle.">
          <Usd value={st.fee_base} />
        </Field>
        <Field label="Fee charged" emphasis note={`At ${String(st.fee_rate_bps)} basis points of the fee base.`}>
          <span data-field="fee_amount">
            <Usd value={st.fee_amount} />
          </span>
        </Field>
        <Field label="Closing NAV, net of the fee">
          <Usd value={st.closing_nav_net} />
        </Field>
        <Field label="High-water mark after" emphasis note="Where the next period opens.">
          <span data-field="hwm_after">
            <Usd value={st.hwm_after} />
          </span>
        </Field>
        <Field label="Accrued before this period">
          <Usd value={st.prior_accrued_fee} />
        </Field>
      </FieldGrid>
      {st.attribution.length > 0 && (
        <ScrollTable caption="Attribution to strategies" headers={["Strategy", "Gross PnL", "Bears the fee", "Fee share", "Remainder cent"]}>
          {st.attribution.map((a) => (
            <tr key={a.strategy}>
              <th scope="row">
                <code className="mono-small">{a.strategy}</code>
              </th>
              <td>
                <Usd value={a.gross_pnl} signed />
              </td>
              <td>{a.eligible ? "yes" : "no: no gain"}</td>
              <td>
                <Usd value={a.fee_share} />
              </td>
              <td>
                <Usd value={a.remainder_allocated} />
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
      {st.flows.length > 0 && (
        <ScrollTable caption="Flows and the high-water mark" headers={["At", "Amount", "NAV before → after", "High-water mark before → after"]}>
          {st.flows.map((flow) => (
            <tr key={flow.at}>
              <th scope="row">{formatInstant(flow.at)}</th>
              <td>
                <Usd value={flow.amount} signed />
              </td>
              <td>
                <Usd value={flow.nav_before} /> → <Usd value={flow.nav_after} />
              </td>
              <td>
                <Usd value={flow.hwm_before} /> → <Usd value={flow.hwm_after} />
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
      <p className="field-note">
        Rounding:{" "}
        {Object.entries(st.rounding)
          .map(([what, mode]) => `${what.replace(/_/g, " ")} ${mode}`)
          .join("; ")}
        .
      </p>
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * The operator
 * ------------------------------------------------------------------------ */

function OpenPeriodPanel(props: { readonly defaultAccount: string | undefined }): ReactNode {
  const open = useOpenPeriod();
  const [accountId, setAccountId] = useState(props.defaultAccount ?? "");
  const [strategy, setStrategy] = useState("");
  const [mode, setMode] = useState<(typeof FEE_RUN_MODES)[number]>("PAPER");
  const [feeAsset, setFeeAsset] = useState("");
  const [termsVersion, setTermsVersion] = useState("");
  const [start, setStart] = useState("");
  const [end, setEnd] = useState("");
  const [openingNav, setOpeningNav] = useState("");
  const [hurdle, setHurdle] = useState("0");
  const [rate, setRate] = useState("2000");
  const a = parseUuid(accountId, "The account id");
  const s = parseStrategyRef(strategy);
  const fa = feeAsset.trim() === "" ? undefined : parseUuid(feeAsset, "The fee asset id");
  const tv = parseText(termsVersion, "The terms version", 100);
  const ps = start.trim() === "" ? undefined : parseUtcInstant(start, "The period start");
  const pe = parseUtcInstant(end, "The period end");
  const nav = openingNav.trim() === "" ? undefined : parseUsdInput(openingNav, "The opening NAV");
  const h = parseWholeNumber(hurdle, 0, 100000000, "The hurdle");
  const r = parseWholeNumber(rate, 0, 10000, "The fee rate");
  const optional = (p: { readonly ok: boolean; readonly error?: string } | undefined): string | undefined =>
    p === undefined || p.ok ? undefined : (p.error ?? "This field is not valid.");
  const prepared = prepare<FeeOpenPeriodRequest>(
    [optional(a), optional(s), optional(fa), optional(tv), optional(ps), optional(pe), optional(nav), optional(h), optional(r)],
    () => ({
      account_id: a.ok ? a.value : "",
      strategy_ref: s.ok ? s.value : "",
      mode,
      terms_version: tv.ok ? tv.value : "",
      period_end: pe.ok ? pe.value : "",
      hurdle_annual_bps: h.ok ? h.value : 0,
      fee_rate_bps: r.ok ? r.value : 0,
      ...(fa !== undefined && fa.ok ? { fee_asset_id: fa.value } : {}),
      ...(ps !== undefined && ps.ok ? { period_start: ps.value } : {}),
      ...(nav !== undefined && nav.ok ? { opening_nav: nav.value } : {}),
    }),
  );
  return (
    <Panel
      title="Open a period"
      description="Opens the next period of an (account, strategy) pair. A pair's first period states its start and opening NAV, which become its high-water mark; every later one opens where the previous statement closed. One period of a pair is open at a time. It moves no value."
    >
      <CommandForm label="Open a period">
        <TextInput id="fees-open-account" label="Account id" value={accountId} onChange={setAccountId} />
        <TextInput id="fees-open-strategy" label="Strategy reference" value={strategy} onChange={setStrategy} />
        <Choice id="fees-open-mode" label="Mode" value={mode} options={FEE_RUN_MODES} onChange={setMode} />
        <TextInput id="fees-open-fee-asset" label="Fee asset id (optional)" value={feeAsset} onChange={setFeeAsset} note="Empty means the sandbox dollar." />
        <TextInput id="fees-open-terms" label="Terms version" value={termsVersion} onChange={setTermsVersion} />
        <TextInput id="fees-open-start" label="Period start, in UTC (the first period only)" value={start} onChange={setStart} />
        <TextInput id="fees-open-end" label="Period end, in UTC" value={end} onChange={setEnd} />
        <TextInput id="fees-open-nav" label="Opening NAV in USD (the first period only)" value={openingNav} onChange={setOpeningNav} inputMode="decimal" />
        <TextInput id="fees-open-hurdle" label="Hurdle, in basis points a year" value={hurdle} onChange={setHurdle} inputMode="numeric" />
        <TextInput id="fees-open-rate" label="Fee rate, in basis points" value={rate} onChange={setRate} inputMode="numeric" />
        <CommandFlow<FeeOpenPeriodRequest, FeePeriod>
          label="Open the period"
          prepared={prepared}
          command={open}
          refusedNote="No period was opened."
          describe={(request) => (
            <FieldGrid columns={3}>
              <Field label="Account">
                <Identifier value={request.account_id} />
              </Field>
              <Field label="Strategy">
                <code className="mono-small">{request.strategy_ref}</code>
              </Field>
              <Field label="Ends">{formatInstant(request.period_end)}</Field>
              <Field label="Opening NAV">{request.opening_nav === undefined ? "where the previous statement closed" : <Usd value={request.opening_nav} />}</Field>
              <Field label="Hurdle and fee rate">
                <Bps value={request.hurdle_annual_bps} /> <Bps value={request.fee_rate_bps} />
              </Field>
            </FieldGrid>
          )}
          renderResult={(period) => (
            <p>
              Opened <Link to={feesHref("periods", period.id)}>period {String(period.seq)}</Link> with a high-water mark of{" "}
              <Usd value={period.terms.prior_hwm} />.
            </p>
          )}
        />
      </CommandForm>
    </Panel>
  );
}

function OperatorPanels(props: { readonly period: FeePeriod }): ReactNode {
  const { period } = props;
  if (period.status === "OPEN") {
    return (
      <>
        <FlowPanel period={period} />
        <AccruePanel period={period} />
        <CrystallizePanel period={period} />
        <CancelPanel period={period} />
      </>
    );
  }
  if (period.status === "CRYSTALLIZED") return <ReversePanel period={period} />;
  return null;
}

function FlowPanel(props: { readonly period: FeePeriod }): ReactNode {
  const record = useRecordFlow();
  const [at, setAt] = useState("");
  const [amount, setAmount] = useState("");
  const [navBefore, setNavBefore] = useState("");
  const a = parseUtcInstant(at, "The instant");
  const m = parseUsdInput(amount, "The amount", true);
  const n = parseUsdInput(navBefore, "The NAV before");
  const prepared = prepare<FlowRequest>([a.ok ? undefined : a.error, m.ok ? undefined : m.error, n.ok ? undefined : n.error], () => ({
    periodId: props.period.id,
    at: a.ok ? a.value : "",
    amount: m.ok ? m.value : "",
    navBefore: n.ok ? n.value : "",
  }));
  return (
    <Panel title="Record a flow" description="A deposit (positive) or withdrawal (negative), after any recorded valuation and inside the period. It moves no value.">
      <CommandForm label="Record a flow">
        <TextInput id="fees-flow-at" label="At, in UTC" value={at} onChange={setAt} />
        <TextInput id="fees-flow-amount" label="Amount in USD (negative for a withdrawal)" value={amount} onChange={setAmount} inputMode="decimal" />
        <TextInput id="fees-flow-nav" label="Gross NAV just before, in USD" value={navBefore} onChange={setNavBefore} inputMode="decimal" />
        <CommandFlow<FlowRequest, FeeFlowResult>
          label="Record the flow"
          prepared={prepared}
          command={record}
          refusedNote="No flow was recorded."
          describe={(request) => (
            <p>
              <Usd value={request.amount} signed /> at {formatInstant(request.at)}, NAV before <Usd value={request.navBefore} />.
            </p>
          )}
          renderResult={(result) => (
            <p>
              Flow {String(result.flow.seq)} recorded: <Usd value={result.flow.amount} signed />.
            </p>
          )}
        />
      </CommandForm>
    </Panel>
  );
}

function AccruePanel(props: { readonly period: FeePeriod }): ReactNode {
  const accrue = useAccruePeriod();
  const [at, setAt] = useState("");
  const [nav, setNav] = useState("");
  const a = parseUtcInstant(at, "The instant");
  const n = parseUsdInput(nav, "The NAV");
  const prepared = prepare<AccrueRequest>([a.ok ? undefined : a.error, n.ok ? undefined : n.error], () => ({
    periodId: props.period.id,
    at: a.ok ? a.value : "",
    nav: n.ok ? n.value : "",
  }));
  return (
    <Panel
      title="Value the period"
      description="Values the open period at an instant with its gross NAV and books the change in the running accrual: nothing accrues below the high-water mark or the hurdle, and a fall is a compensating line. It moves no customer value."
    >
      <CommandForm label="Value the period">
        <TextInput id="fees-accrue-at" label="At, in UTC" value={at} onChange={setAt} />
        <TextInput id="fees-accrue-nav" label="Gross NAV in USD" value={nav} onChange={setNav} inputMode="decimal" />
        <CommandFlow<AccrueRequest, FeeLineResult>
          label="Value the period"
          prepared={prepared}
          command={accrue}
          refusedNote="No line was booked."
          describe={(request) => (
            <p>
              NAV <Usd value={request.nav} /> at {formatInstant(request.at)}.
            </p>
          )}
          renderResult={(result) => (
            <FieldGrid columns={3}>
              <Field label="Line" note={LINE_KIND_MEANING[result.line.kind]}>
                <code className="mono-small">{result.line.kind}</code>
              </Field>
              <Field label="High-water mark">
                <Usd value={result.line.hwm_adjusted} />
              </Field>
              <Field label="Fee base">
                <Usd value={result.line.fee_base} />
              </Field>
              <Field label="Change">
                <Usd value={result.line.amount} signed />
              </Field>
              <Field label="Accrual after">
                <Usd value={result.line.accrual_after} />
              </Field>
            </FieldGrid>
          )}
        />
      </CommandForm>
    </Panel>
  );
}

/** Strategy contributions, one per line: a strategy reference and its gross PnL in USD. */
function parseContributions(raw: string): { readonly ok: true; readonly value: FeeContribution[] } | { readonly ok: false; readonly error: string } {
  const out: FeeContribution[] = [];
  const lines = raw.split("\n");
  for (let i = 0; i < lines.length; i = i + 1) {
    const line = (lines[i] ?? "").trim();
    if (line === "") continue;
    const parts = line.split(/\s+/);
    const where = `Contribution line ${String(i + 1)}`;
    if (parts.length !== 2) return { ok: false, error: `${where}: write a strategy and its gross PnL in USD.` };
    const s = parseText(parts[0] ?? "", `${where}: the strategy`, 100);
    if (!s.ok) return s;
    const p = parseUsdInput(parts[1] ?? "", `${where}: the gross PnL`, true);
    if (!p.ok) return p;
    out.push({ strategy: s.value, gross_pnl: p.value });
  }
  return { ok: true, value: out };
}

function CrystallizePanel(props: { readonly period: FeePeriod }): ReactNode {
  const crystallize = useCrystallizePeriod();
  const [closing, setClosing] = useState("");
  const [contributions, setContributions] = useState("");
  const c = parseUsdInput(closing, "The closing NAV");
  const k = parseContributions(contributions);
  const prepared = prepare<CrystallizeRequest>([c.ok ? undefined : c.error, k.ok ? undefined : k.error], () => ({
    periodId: props.period.id,
    closingNav: c.ok ? c.value : "",
    contributions: k.ok ? k.value : [],
  }));
  return (
    <Panel
      title="Crystallize the period"
      description="Once the period's end has passed: values it at the end, charges the fee to the customer's wallet (reserved first), attributes it to strategies, and stores the statement. Only value above the high-water mark and the hurdle is charged. It moves simulated value, so the backend asks for a recent strong sign-in."
    >
      <CommandForm label="Crystallize the period">
        <TextInput id="fees-crystallize-nav" label="Gross closing NAV in USD" value={closing} onChange={setClosing} inputMode="decimal" />
        <TextArea
          id="fees-crystallize-contributions"
          label="Strategy contributions (optional): per line, a strategy and its gross PnL in USD; they must sum to the period's gross PnL"
          value={contributions}
          onChange={setContributions}
        />
        <CommandFlow<CrystallizeRequest, FeePeriod>
          label="Crystallize"
          variant="danger"
          prepared={prepared}
          command={crystallize}
          refusedNote="The period was not crystallized and nothing was charged."
          describe={(request) => (
            <p>
              Crystallize at a gross closing NAV of <Usd value={request.closingNav} />
              {request.contributions.length === 0 ? "." : `, attributed to ${String(request.contributions.length)} strategies.`}
            </p>
          )}
          renderResult={(period) => (
            <p>
              <Pill tone={periodStatusTone(period.status)}>{period.status}</Pill> fee charged{" "}
              {period.statement === undefined ? <Absent what="not stated" /> : <Usd value={period.statement.fee_amount} />}.
            </p>
          )}
        />
      </CommandForm>
    </Panel>
  );
}

function CancelPanel(props: { readonly period: FeePeriod }): ReactNode {
  const cancel = useCancelPeriod();
  const [reason, setReason] = useState("");
  const r = parseText(reason, "The reason", 500);
  const prepared = prepare<CancelPeriodRequest>([r.ok ? undefined : r.error], () => ({ periodId: props.period.id, reason: r.ok ? r.value : "" }));
  return (
    <Panel title="Cancel the period" description="Cancels the open period; any running accrual is reversed by a compensating line. Nothing is charged.">
      <CommandForm label="Cancel the period">
        <TextInput id="fees-cancel-reason" label="Reason" value={reason} onChange={setReason} />
        <CommandFlow<CancelPeriodRequest, FeePeriod>
          label="Cancel the period"
          prepared={prepared}
          command={cancel}
          refusedNote="The period is unchanged."
          describe={(request) => <p>Cancel: {request.reason}</p>}
          renderResult={(period) => <Pill tone={periodStatusTone(period.status)}>{period.status}</Pill>}
        />
      </CommandForm>
    </Panel>
  );
}

function ReversePanel(props: { readonly period: FeePeriod }): ReactNode {
  const reverse = useReversePeriod();
  const [code, setCode] = useState("");
  const [reason, setReason] = useState("");
  const c = parseReasonCode(code);
  const r = parseText(reason, "The reason", 500);
  const prepared = prepare<ReverseRequest>([c.ok ? undefined : c.error, r.ok ? undefined : r.error], () => ({
    id: props.period.id,
    reasonCode: c.ok ? c.value : "",
    reason: r.ok ? r.value : "",
  }));
  return (
    <Panel
      title="Reverse the crystallization"
      description="Returns exactly the fee to the customer by a compensating journal with a reason code; the statement and lines stay as they were. A period another one already continues from cannot be reversed. It moves simulated value, so the backend asks for a recent strong sign-in."
    >
      <CommandForm label="Reverse the crystallization">
        <TextInput id="fees-reverse-period-code" label="Reason code" value={code} onChange={setCode} />
        <TextInput id="fees-reverse-period-reason" label="Reason" value={reason} onChange={setReason} />
        <CommandFlow<ReverseRequest, FeePeriod>
          label="Reverse the crystallization"
          variant="danger"
          prepared={prepared}
          command={reverse}
          refusedNote="The crystallization stands."
          describe={(request) => (
            <p>
              Reverse with <code className="mono-small">{request.reasonCode}</code>: {request.reason}
            </p>
          )}
          renderResult={(period) => <Pill tone={periodStatusTone(period.status)}>{period.status}</Pill>}
        />
      </CommandForm>
    </Panel>
  );
}
