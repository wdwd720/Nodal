/**
 * STABLE ASSETS — operator: registering a stable asset, pausing and resuming
 * one, running its peg monitor and its supply reconciliation, publishing a
 * reserve snapshot, paying out its queued redemptions, and settling every
 * submission whose outcome is unknown from the simulated chain.
 *
 * These are sandbox:operate commands; registering, pausing, resuming and
 * anything that moves simulated value need a recent strong sign-in. A
 * principal without the permission is refused by the backend, and the page
 * shows that refusal with its code — it never hides a panel to pretend the
 * question was not asked.
 */
import { useId, useState, type ReactNode } from "react";

import {
  usePauseStableAsset,
  useRegisterStableAsset,
  useReconcileStableSupply,
  useResolveUnknownStable,
  useResumeStableAsset,
  useRunRedemptionQueue,
  useRunStableMonitor,
  useStableAssets,
  useTakeStableSnapshot,
  type AssetActionRequest,
  type QueueRequest,
  type ReasonRequest,
  type RegisterReserve,
  type RegisterRequest,
  type ResolveRequest,
  type StableAsset,
  type StableAssetDetail,
  type StableAssetList,
  type StableMonitorReport,
  type StableSettleReport,
  type StableSnapshotResult,
  type StableSupplyReconciliation,
} from "../../../api/advanced/stable.ts";
import { useAssets } from "../../../api/queries.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Bps, DecimalValue, Qty } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { parseQuantityInput } from "../../../lib/money.ts";
import {
  ALERT_KIND_MEANING,
  DOLLAR_EQUIVALENT_RESERVE,
  REDEMPTION_MODE_MEANING,
  STABLE_REDEMPTION_MODES,
  TOKENIZED_RESERVE,
  assetStatusTone,
  parsePositiveDecimal,
  parseReason,
  parseRegistryName,
  parseWholeNumber,
  reserveKindLabel,
  verificationSummary,
  type StableRedemptionMode,
} from "../../../lib/stable.ts";
import { formatInstant } from "../../../lib/time.ts";
import { ScrollTable } from "../shared.tsx";
import { CommandFlow, PriceLine, StableNav, simulatedAssets, type Prepared } from "./parts.tsx";

/** The sandbox dollar's symbol, which the registry's default reserve choice prefers. */
const SANDBOX_DOLLAR_SYMBOL = "SBX-USD";

export function StableOperatorPage(): ReactNode {
  return (
    <Page
      title="Stable asset operations"
      lead="Registering and pausing stable assets, the peg monitor, supply reconciliation, reserve snapshots, the redemption queue and unknown submissions. The backend decides who may run them."
    >
      <SimulatedNotice />
      <StableNav />
      <p className="note">
        Every command here is an operator's (sandbox:operate). Registering, pausing, resuming, paying out the redemption
        queue and settling unknown submissions also need a recent strong sign-in. A session without the permission is
        refused by the backend, and the refusal is shown with its code.
      </p>
      <AssetActionsPanel />
      <ResolveUnknownPanel />
      <RegisterPanel />
    </Page>
  );
}

/* --------------------------------------------------------------------------
 * Actions on one stable asset
 * ------------------------------------------------------------------------ */

function AssetActionsPanel(): ReactNode {
  const assets = useStableAssets();
  return (
    <Panel title="Act on a stable asset" description="Choose a registered stable asset, then the action.">
      <AsyncPanel
        query={assets}
        loadingLabel="Loading the stable assets…"
        empty={{
          isEmpty: (data: StableAssetList) => data.items.length === 0,
          title: "No stable asset is registered",
          body: "Register one below first; there is nothing to act on yet.",
        }}
      >
        {(data: StableAssetList) => <AssetActions assets={data.items} />}
      </AsyncPanel>
    </Panel>
  );
}

function AssetActions(props: { readonly assets: readonly StableAsset[] }): ReactNode {
  const [chosenId, setChosenId] = useState<string>(props.assets[0]?.id ?? "");
  const selectId = useId();
  const chosen = props.assets.find((asset) => asset.id === chosenId) ?? props.assets[0];
  if (chosen === undefined) return null;
  return (
    <>
      <div className="form-row">
        <label htmlFor={selectId}>Stable asset</label>
        <select
          id={selectId}
          className="input"
          value={chosen.id}
          onChange={(event) => {
            setChosenId(event.target.value);
          }}
        >
          {props.assets.map((asset) => (
            <option key={asset.id} value={asset.id}>
              {asset.symbol} ({asset.status})
            </option>
          ))}
        </select>
        <p className="field-note">
          <Pill tone={assetStatusTone(chosen.status)}>{chosen.status}</Pill> <code className="mono-small">{chosen.id}</code>
        </p>
      </div>
      <StatusChange asset={chosen} />
      <MonitorRun asset={chosen} />
      <SupplyReconcile asset={chosen} />
      <SnapshotTake asset={chosen} />
      <QueueRun asset={chosen} />
    </>
  );
}

function StatusChange(props: { readonly asset: StableAsset }): ReactNode {
  const { asset } = props;
  const pause = usePauseStableAsset();
  const resume = useResumeStableAsset();
  const [raw, setRaw] = useState("");
  const reasonId = useId();
  const reason = parseReason(raw);
  const prepared: Prepared<ReasonRequest> = reason.ok
    ? { ok: true, request: { stableAssetId: asset.id, reason: reason.value } }
    : { ok: false, reason: reason.error };
  const result = (detail: StableAssetDetail): ReactNode => (
    <p role="status">
      {detail.asset.symbol} is now <Pill tone={assetStatusTone(detail.asset.status)}>{detail.asset.status}</Pill>
    </p>
  );
  return (
    <section aria-label="Pause or resume">
      <h3>Pause or resume</h3>
      <div className="form-row">
        <label htmlFor={reasonId}>Reason</label>
        <input
          id={reasonId}
          className="input"
          autoComplete="off"
          value={raw}
          onChange={(event) => {
            setRaw(event.target.value);
          }}
        />
        <p className="field-note">Recorded with the status change. The circuit breaker pauses on its own; only a person resumes.</p>
      </div>
      <CommandFlow
        label={`pause ${asset.symbol}`}
        prepared={prepared}
        command={pause}
        variant="danger"
        describe={(request: ReasonRequest) => (
          <p>
            Pause {asset.symbol} (<code className="mono-small">{request.stableAssetId}</code>): stop issuance and redemption,
            because “{request.reason}”.
          </p>
        )}
        renderResult={result}
        refusedNote="The backend did not change the asset's status."
      />
      <CommandFlow
        label={`resume ${asset.symbol}`}
        prepared={prepared}
        command={resume}
        describe={(request: ReasonRequest) => (
          <p>
            Resume {asset.symbol} (<code className="mono-small">{request.stableAssetId}</code>), because “{request.reason}”.
          </p>
        )}
        renderResult={result}
        refusedNote="The backend did not change the asset's status."
      />
    </section>
  );
}

function MonitorRun(props: { readonly asset: StableAsset }): ReactNode {
  const { asset } = props;
  const command = useRunStableMonitor();
  return (
    <section aria-label="Peg monitor">
      <h3>Peg monitor</h3>
      <p className="field-note">
        Compares the market price with the peg, the eligible reserve with issued supply, and the books with the simulated
        chain. A deviation above the threshold, coverage under full, or a supply mismatch pauses the asset. Moves no value.
      </p>
      <CommandFlow
        label={`run the peg monitor for ${asset.symbol}`}
        prepared={{ ok: true, request: { stableAssetId: asset.id } }}
        command={command}
        describe={(request: AssetActionRequest) => (
          <p>
            Run the peg monitor for {asset.symbol} (<code className="mono-small">{request.stableAssetId}</code>).
          </p>
        )}
        renderResult={(report: StableMonitorReport) => <MonitorReportView report={report} asset={asset} />}
        refusedNote="The backend did not run the monitor; nothing was recorded."
      />
    </section>
  );
}

function MonitorReportView(props: { readonly report: StableMonitorReport; readonly asset: StableAsset }): ReactNode {
  const { report, asset } = props;
  return (
    <div role="status">
      <FieldGrid columns={3}>
        <Field label="Observed">{formatInstant(report.observed_at)}</Field>
        <Field label="Market price" note="The asset's own price from its oracle source.">
          {report.price_stale ? <span className="absent">no fresh price</span> : <PriceLine price={report.market_price} />}
        </Field>
        <Field label="Deviation from the peg" note="Rounded up by the backend, so a deviation is never understated.">
          <Bps value={report.deviation_bps} />
        </Field>
        <Field label="Findings">
          {report.alerts.length === 0 ? (
            <span className="absent">none</span>
          ) : (
            <ul className="codes">
              {report.alerts.map((alert) => (
                <li key={alert}>
                  <code className="mono-small">{alert}</code> {ALERT_KIND_MEANING[alert]}
                </li>
              ))}
            </ul>
          )}
        </Field>
        <Field label="Status after the run">
          <Pill tone={assetStatusTone(report.status)}>{report.status}</Pill>
          {report.breaker_tripped && <span className="cell-note">this run paused the asset</span>}
        </Field>
        <Field label="Coverage">
          <Bps value={report.reserve.coverage_bps} /> {report.reserve.covered ? "covered" : "not covered"}
        </Field>
      </FieldGrid>
      <SupplyView supply={report.supply} asset={asset} />
    </div>
  );
}

function SupplyReconcile(props: { readonly asset: StableAsset }): ReactNode {
  const { asset } = props;
  const command = useReconcileStableSupply();
  return (
    <section aria-label="Supply reconciliation">
      <h3>Supply reconciliation</h3>
      <p className="field-note">
        Compares the issuer's outstanding supply with every holding of the asset on the books and with the simulated chain's
        supply, and the eligible reserve with supply at the peg. A mismatch opens a record that blocks new risk and pauses
        the asset. Moves no value.
      </p>
      <CommandFlow
        label={`reconcile ${asset.symbol}`}
        prepared={{ ok: true, request: { stableAssetId: asset.id } }}
        command={command}
        describe={(request: AssetActionRequest) => (
          <p>
            Reconcile the supply of {asset.symbol} (<code className="mono-small">{request.stableAssetId}</code>).
          </p>
        )}
        renderResult={(supply: StableSupplyReconciliation) => <SupplyView supply={supply} asset={asset} />}
        refusedNote="The backend did not reconcile; nothing was recorded."
      />
    </section>
  );
}

function SupplyView(props: { readonly supply: StableSupplyReconciliation; readonly asset: StableAsset }): ReactNode {
  const { supply, asset } = props;
  const units = (value: string): ReactNode => (
    <>
      <Qty value={value} decimals={asset.decimals} symbol={asset.symbol} />
      <span className="cell-note">
        <BaseUnits value={value} />
      </span>
    </>
  );
  return (
    <div role="status" data-field="supply-reconciliation">
      <p>
        {supply.matched ? <Pill tone="good">books, holdings, chain and reserve agree</Pill> : <Pill tone="bad">mismatch</Pill>}
        {supply.breaker_tripped && <span className="cell-note">this run paused the asset</span>}
      </p>
      <FieldGrid columns={3}>
        <Field label="Outstanding on the books" note="The issuer's STABLE_OUTSTANDING.">
          {units(supply.ledger_outstanding)}
        </Field>
        <Field label="In customer wallets" note="Every customer WALLET balance of the asset.">
          {units(supply.customer_wallet_sum)}
        </Field>
        <Field label="Held elsewhere" note="Every other holding of the asset on the books: a bridge escrow, collateral, a house inventory, collected fees.">
          {units(supply.held_elsewhere)}
        </Field>
        <Field label="Holdings agree with outstanding" note="Customer wallets plus held elsewhere equal outstanding.">
          {supply.wallets_match ? <Pill tone="good">yes</Pill> : <Pill tone="bad">no</Pill>}
        </Field>
        <Field label="Supply on the simulated chain" note="The devnet mint's supply.">
          {units(supply.chain_supply)}
        </Field>
        <Field label="Expected on the chain" note="Outstanding, plus mints that landed and are not posted, less burns that landed and are not posted.">
          {units(supply.expected_chain_supply)}
        </Field>
        <Field label="Landed, not yet posted" note="Mints / burns already in the chain state but not yet on the books.">
          <span className="num">{supply.landed_unposted_mints}</span> / <span className="num">{supply.landed_unposted_burns}</span>
        </Field>
        <Field label="Chain agrees" note="The chain's supply equals what is expected.">
          {supply.supply_matches ? <Pill tone="good">yes</Pill> : <Pill tone="bad">no</Pill>}
        </Field>
        <Field label="Reserve coverage">
          <Bps value={supply.coverage_bps} /> {supply.covered ? "covered" : "not covered"}
          <span className="cell-note">
            eligible <DecimalValue value={supply.eligible_reserve} /> against <DecimalValue value={supply.supply_value} /> at the peg
          </span>
        </Field>
        <Field label="Reconciliation record">
          {supply.record_id === undefined ? <span className="absent">none recorded</span> : <Identifier value={supply.record_id} />}
        </Field>
      </FieldGrid>
    </div>
  );
}

function SnapshotTake(props: { readonly asset: StableAsset }): ReactNode {
  const { asset } = props;
  const command = useTakeStableSnapshot();
  return (
    <section aria-label="Reserve snapshot">
      <h3>Reserve snapshot</h3>
      <p className="field-note">
        Records the next signed, hash-linked snapshot of the books and the simulated chain. It is not an attestation. Moves
        no value.
      </p>
      {asset.attestable ? (
        <CommandFlow
          label={`publish a snapshot of ${asset.symbol}`}
          prepared={{ ok: true, request: { stableAssetId: asset.id } }}
          command={command}
          describe={(request: AssetActionRequest) => (
            <p>
              Publish the next reserve snapshot of {asset.symbol} (<code className="mono-small">{request.stableAssetId}</code>).
            </p>
          )}
          renderResult={(result: StableSnapshotResult) => (
            <div role="status">
              <p>
                Snapshot {String(result.snapshot.seq)} recorded,{" "}
                {result.snapshot.verified ? <Pill tone="good">verified</Pill> : <Pill tone="bad">unverified</Pill>}
              </p>
              <p className="field-note">{verificationSummary(result.verification)}</p>
              <Identifier value={result.snapshot.hash} label="hash" />
            </div>
          )}
          refusedNote="The backend did not record a snapshot."
        />
      ) : (
        <p className="absent">{asset.symbol} does not publish reserve snapshots.</p>
      )}
    </section>
  );
}

function LimitInput(props: { readonly raw: string; readonly onChange: (raw: string) => void }): ReactNode {
  const id = useId();
  return (
    <div className="form-row">
      <label htmlFor={id}>At most this many requests</label>
      <input
        id={id}
        className="input"
        inputMode="numeric"
        autoComplete="off"
        value={props.raw}
        onChange={(event) => {
          props.onChange(event.target.value);
        }}
      />
      <p className="field-note">Leave it empty for the service's own bound of one hundred.</p>
    </div>
  );
}

function limitOf(raw: string): { readonly ok: true; readonly limit: number | undefined } | { readonly ok: false; readonly error: string } {
  if (raw.trim() === "") return { ok: true, limit: undefined };
  const parsed = parseWholeNumber(raw, 1, 500, "The limit");
  return parsed.ok ? { ok: true, limit: parsed.value } : { ok: false, error: parsed.error };
}

const UPKEEP_STEP: Readonly<Record<StableSettleReport["failures"][number]["operation"], string>> = {
  "stableasset.settle_mint": "Settle an issuance",
  "stableasset.settle_burn": "Settle a redemption",
  "stableasset.pay_redemption": "Pay out a redemption",
};

function SettleReportView(props: { readonly report: StableSettleReport }): ReactNode {
  const { report } = props;
  return (
    <>
      <FieldGrid columns={3}>
        <Field label="Examined">{String(report.examined)}</Field>
        <Field label="Issued">{String(report.issued)}</Field>
        <Field label="Redeemed">{String(report.redeemed)}</Field>
        <Field label="Queued for payout">{String(report.queued)}</Field>
        <Field label="Failed" note="Requests this pass settled as FAILED.">
          {String(report.failed)}
        </Field>
        <Field label="Still waiting" note="Their transaction is not final yet.">
          {String(report.waiting)}
        </Field>
      </FieldGrid>
      {report.failures.length > 0 && (
        <>
          <p className="field-note">
            The pass could not act on the requests below. Each was left as it was; every other request was handled, and a later pass
            retries each one once its backoff has passed.
          </p>
          <ScrollTable caption="Requests this pass could not act on" headers={["Request", "Step", "Attempts", "Retried after", "Reason"]}>
            {report.failures.map((f) => (
              <tr key={`${f.operation}:${f.request_id}`}>
                <th scope="row">
                  <Identifier value={f.request_id} />
                </th>
                <td>{UPKEEP_STEP[f.operation]}</td>
                <td>{String(f.attempts)}</td>
                <td>{formatInstant(f.retry_after)}</td>
                <td>
                  <code>{f.error_code}</code> {f.detail}
                </td>
              </tr>
            ))}
          </ScrollTable>
        </>
      )}
    </>
  );
}

function QueueRun(props: { readonly asset: StableAsset }): ReactNode {
  const { asset } = props;
  const command = useRunRedemptionQueue();
  const [raw, setRaw] = useState("");
  const limit = limitOf(raw);
  const prepared: Prepared<QueueRequest> = limit.ok
    ? { ok: true, request: { stableAssetId: asset.id, limit: limit.limit } }
    : { ok: false, reason: limit.error };
  return (
    <section aria-label="Redemption queue">
      <h3>Redemption queue</h3>
      <p className="field-note">{REDEMPTION_MODE_MEANING[asset.redemption_mode]} Paying out moves simulated value.</p>
      <LimitInput raw={raw} onChange={setRaw} />
      <CommandFlow
        label={`pay out queued redemptions of ${asset.symbol}`}
        prepared={prepared}
        command={command}
        describe={(request: QueueRequest) => (
          <p>
            Pay out the queued redemptions of {asset.symbol}, oldest first
            {request.limit === undefined ? "" : `, at most ${String(request.limit)}`}.
          </p>
        )}
        renderResult={(report: StableSettleReport) => (
          <div role="status">
            <SettleReportView report={report} />
          </div>
        )}
        refusedNote="The backend paid nothing out."
      />
    </section>
  );
}

/* --------------------------------------------------------------------------
 * Unknown submissions, across every stable asset
 * ------------------------------------------------------------------------ */

function ResolveUnknownPanel(): ReactNode {
  const command = useResolveUnknownStable();
  const [raw, setRaw] = useState("");
  const limit = limitOf(raw);
  const prepared: Prepared<ResolveRequest> = limit.ok ? { ok: true, request: { limit: limit.limit } } : { ok: false, reason: limit.error };
  return (
    <Panel
      title="Settle unknown submissions"
      description="Issuances and redemptions whose submission outcome is unknown are settled from the simulated chain's status of their own transaction: final posts, failed or expired releases, anything else waits. Nothing is ever resubmitted. In SANDBOX mode the platform produces the blocks, so a landed transaction finalizes only once blocks are produced (the bridge section's chain page)."
    >
      <LimitInput raw={raw} onChange={setRaw} />
      <CommandFlow
        label="settle unknown submissions"
        prepared={prepared}
        command={command}
        describe={(request: ResolveRequest) => (
          <p>
            Settle every stable asset submission whose outcome is unknown, from the chain
            {request.limit === undefined ? "" : `, at most ${String(request.limit)}`}.
          </p>
        )}
        renderResult={(report: StableSettleReport) => (
          <div role="status">
            <SettleReportView report={report} />
          </div>
        )}
        refusedNote="The backend settled nothing."
      />
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * Registering a stable asset
 * ------------------------------------------------------------------------ */

function TextRow(props: {
  readonly label: string;
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly note: string;
  readonly numeric?: boolean;
}): ReactNode {
  const id = useId();
  return (
    <div className="form-row">
      <label htmlFor={id}>{props.label}</label>
      <input
        id={id}
        className="input"
        autoComplete="off"
        {...(props.numeric === true ? { inputMode: "decimal" as const } : {})}
        value={props.value}
        onChange={(event) => {
          props.onChange(event.target.value);
        }}
      />
      <p className="field-note">{props.note}</p>
    </div>
  );
}

function RegisterPanel(): ReactNode {
  const registry = useAssets();
  const simulated = simulatedAssets(registry.data);
  const command = useRegisterStableAsset();
  const preferred = simulated.find((asset) => asset.symbol === SANDBOX_DOLLAR_SYMBOL)?.id ?? simulated[0]?.id ?? "";

  const [name, setName] = useState("");
  const [symbol, setSymbol] = useState("");
  const [pegCurrency, setPegCurrency] = useState("USD");
  const [decimalsRaw, setDecimalsRaw] = useState("6");
  const [capRaw, setCapRaw] = useState("");
  const [redemptionMode, setRedemptionMode] = useState<StableRedemptionMode>("INSTANT");
  const [pegRaw, setPegRaw] = useState("");
  const [oracle, setOracle] = useState("");
  const [ageRaw, setAgeRaw] = useState("3600");
  const [thresholdRaw, setThresholdRaw] = useState("50");
  const [attestable, setAttestable] = useState(true);
  const [dollarReserve, setDollarReserve] = useState<string>("");
  const [dollarFactorRaw, setDollarFactorRaw] = useState("10000");
  const [tokenizedReserve, setTokenizedReserve] = useState<string>("");
  const [tokenizedFactorRaw, setTokenizedFactorRaw] = useState("9000");
  const modeId = useId();
  const dollarId = useId();
  const tokenizedId = useId();
  const attestId = useId();

  const dollar = dollarReserve === "" ? preferred : dollarReserve;
  const decimals = parseWholeNumber(decimalsRaw, 0, 18, "Decimals");
  const cap = decimals.ok ? parseQuantityInput(capRaw, decimals.value) : undefined;
  const nameParsed = parseRegistryName(name, "The name");
  const peg = parsePositiveDecimal(pegRaw, "The peg");
  const age = parseWholeNumber(ageRaw, 1, 86400, "The maximum price age");
  const threshold = parseWholeNumber(thresholdRaw, 1, 10000, "The deviation threshold");
  const dollarFactor = parseWholeNumber(dollarFactorRaw, 1, 10000, "The eligibility factor");
  const tokenizedFactor = parseWholeNumber(tokenizedFactorRaw, 0, 10000, "The eligibility factor");

  let prepared: Prepared<RegisterRequest>;
  if (!nameParsed.ok) prepared = { ok: false, reason: nameParsed.error };
  else if (symbol.trim() === "") prepared = { ok: false, reason: "Enter the symbol the asset is listed under." };
  else if (pegCurrency.trim() === "") prepared = { ok: false, reason: "Enter the currency the peg is displayed in." };
  else if (!decimals.ok) prepared = { ok: false, reason: decimals.error };
  else if (cap === undefined || !cap.ok) prepared = { ok: false, reason: cap === undefined || capRaw === "" ? "Enter the issuance cap." : cap.error };
  else if (!peg.ok) prepared = { ok: false, reason: peg.error };
  else if (oracle.trim() === "") prepared = { ok: false, reason: "Name the oracle source the monitor accepts prices from." };
  else if (!age.ok) prepared = { ok: false, reason: age.error };
  else if (!threshold.ok) prepared = { ok: false, reason: threshold.error };
  else if (dollar === "") prepared = { ok: false, reason: "Choose the dollar-equivalent reserve asset that pays for an issuance." };
  else if (!dollarFactor.ok) prepared = { ok: false, reason: dollarFactor.error };
  else if (tokenizedReserve !== "" && !tokenizedFactor.ok) prepared = { ok: false, reason: tokenizedFactor.error };
  else if (tokenizedReserve !== "" && tokenizedReserve === dollar)
    prepared = { ok: false, reason: "A reserve asset can appear in the policy only once." };
  else {
    const reserves: RegisterReserve[] = [
      { assetId: dollar, kind: DOLLAR_EQUIVALENT_RESERVE, factorBps: dollarFactor.value, acceptedForMint: true },
    ];
    if (tokenizedReserve !== "" && tokenizedFactor.ok) {
      reserves.push({ assetId: tokenizedReserve, kind: TOKENIZED_RESERVE, factorBps: tokenizedFactor.value, acceptedForMint: false });
    }
    prepared = {
      ok: true,
      request: {
        name: nameParsed.value,
        symbol: symbol.trim(),
        pegCurrency: pegCurrency.trim(),
        decimals: decimals.value,
        issuanceCap: cap.value,
        redemptionMode,
        peg: peg.value,
        oracleSource: oracle.trim(),
        maxPriceAgeMs: age.value * 1000,
        pegDeviationThresholdBps: threshold.value,
        attestable,
        reserves,
      },
    };
  }

  const symbolFor = (id: string): string => simulated.find((asset) => asset.id === id)?.symbol ?? id;

  return (
    <Panel
      title="Register a stable asset"
      description="A SIMULATED asset on the simulated chain sbx-solana, minted only by the platform issuer signer, fully reserved. The peg is quoted in the sandbox dollar; that is not a choice."
    >
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
        }}
      >
        <TextRow label="Name" value={name} onChange={setName} note="1 to 63 lowercase letters, digits and hyphens. Generic: never a public brand." />
        <TextRow label="Symbol" value={symbol} onChange={setSymbol} note="The symbol the simulated asset is listed under." />
        <TextRow label="Peg currency" value={pegCurrency} onChange={setPegCurrency} note="The currency the peg is displayed in." />
        <TextRow label="Decimals" value={decimalsRaw} onChange={setDecimalsRaw} numeric note="Base units per whole unit, as a power of ten: 0 to 18." />
        <TextRow
          label="Issuance cap"
          value={capRaw}
          onChange={setCapRaw}
          numeric
          note="Whole units outstanding supply may never exceed, written in digits; sent exactly as base units."
        />
        <TextRow
          label="Peg"
          value={pegRaw}
          onChange={setPegRaw}
          numeric
          note="The exact price of one whole unit in the sandbox dollar, written as a decimal in digits; sent exactly as typed."
        />
        <TextRow label="Oracle source" value={oracle} onChange={setOracle} note="The only price source the monitor and the reserve verification accept." />
        <TextRow label="Maximum price age, in seconds" value={ageRaw} onChange={setAgeRaw} numeric note="An older price counts as stale." />
        <TextRow
          label="Peg deviation threshold, in basis points"
          value={thresholdRaw}
          onChange={setThresholdRaw}
          numeric
          note="A market price further from the peg than this pauses the asset."
        />
        <div className="form-row">
          <label htmlFor={modeId}>Redemption mode</label>
          <select
            id={modeId}
            className="input"
            value={redemptionMode}
            onChange={(event) => {
              const chosen = STABLE_REDEMPTION_MODES.find((mode) => mode === event.target.value);
              if (chosen !== undefined) setRedemptionMode(chosen);
            }}
          >
            {STABLE_REDEMPTION_MODES.map((mode) => (
              <option key={mode} value={mode}>
                {mode}
              </option>
            ))}
          </select>
          <p className="field-note">{REDEMPTION_MODE_MEANING[redemptionMode]}</p>
        </div>
        <label className="checkbox" htmlFor={attestId}>
          <input
            id={attestId}
            type="checkbox"
            checked={attestable}
            onChange={(event) => {
              setAttestable(event.target.checked);
            }}
          />
          <span>Publish signed reserve snapshots (not an attestation: a signed record anyone can verify)</span>
        </label>
        <div className="form-row">
          <label htmlFor={dollarId}>Dollar-equivalent reserve (pays for issuance)</label>
          <select
            id={dollarId}
            className="input"
            value={dollar}
            onChange={(event) => {
              setDollarReserve(event.target.value);
            }}
          >
            {simulated.map((asset) => (
              <option key={asset.id} value={asset.id}>
                {asset.symbol}
              </option>
            ))}
          </select>
          <p className="field-note">Counted at the lower of its verified price and par; the sandbox dollar itself counts at par.</p>
        </div>
        <TextRow
          label="Its eligibility factor, in basis points"
          value={dollarFactorRaw}
          onChange={setDollarFactorRaw}
          numeric
          note="Ten thousand counts it in full."
        />
        <div className="form-row">
          <label htmlFor={tokenizedId}>Tokenized reserve (optional)</label>
          <select
            id={tokenizedId}
            className="input"
            value={tokenizedReserve}
            onChange={(event) => {
              setTokenizedReserve(event.target.value);
            }}
          >
            <option value="">none</option>
            {simulated.map((asset) => (
              <option key={asset.id} value={asset.id}>
                {asset.symbol}
              </option>
            ))}
          </select>
          <p className="field-note">Over-collateralisation, counted at its verified price and factor; it never pays for an issuance.</p>
        </div>
        {tokenizedReserve !== "" && (
          <TextRow
            label="Its eligibility factor, in basis points"
            value={tokenizedFactorRaw}
            onChange={setTokenizedFactorRaw}
            numeric
            note="Below ten thousand counts it at a haircut."
          />
        )}
      </form>
      <CommandFlow
        label="register"
        prepared={prepared}
        command={command}
        describe={(request: RegisterRequest) => (
          <div>
            <p>
              Register <strong>{request.symbol}</strong> named <code className="mono-small">{request.name}</code>, with{" "}
              {String(request.decimals)} decimals, an issuance cap of <BaseUnits value={request.issuanceCap} />, a peg of{" "}
              {request.peg} in the sandbox dollar ({request.pegCurrency}), redemption {request.redemptionMode}, oracle{" "}
              <code className="mono-small">{request.oracleSource}</code>, prices fresh for{" "}
              {String(request.maxPriceAgeMs)} ms, a deviation threshold of <Bps value={request.pegDeviationThresholdBps} />,
              {request.attestable ? " publishing snapshots" : " publishing no snapshots"}.
            </p>
            <ul className="codes">
              {request.reserves.map((reserve) => (
                <li key={reserve.assetId}>
                  {symbolFor(reserve.assetId)}: {reserveKindLabel(reserve.kind)}, factor{" "}
                  <Bps value={reserve.factorBps} />
                  {reserve.acceptedForMint ? ", pays for an issuance" : ""}
                </li>
              ))}
            </ul>
          </div>
        )}
        renderResult={(detail: StableAssetDetail) => (
          <p role="status">
            {detail.asset.symbol} is registered: <Identifier value={detail.asset.id} />{" "}
            <Pill tone={assetStatusTone(detail.asset.status)}>{detail.asset.status}</Pill>
            <span className="cell-note">
              cap <Qty value={detail.asset.issuance_cap} decimals={detail.asset.decimals} symbol={detail.asset.symbol} />
            </span>
          </p>
        )}
        refusedNote="The backend registered nothing."
      />
    </Panel>
  );
}
