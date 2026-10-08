/**
 * FEES — quote disclosures.
 *
 * A quote disclosure is the stored record of what an order was priced at
 * under the schedule in force: the reference and execution prices, the
 * spread, every explicit fee, the network estimate, the totals and the
 * effective all-in price, with its hash. The account's disclosures are
 * listed here; one disclosure read by its id is re-proved by the backend from
 * the record against its schedule, and the verification is shown with it.
 *
 * Operators additionally record a quote, bound to its payer when they name
 * one; recording moves no value.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useFeeQuote,
  useFeeQuotes,
  useRecordQuote,
  type FeeQuoteRecord,
  type FeeQuoteRecordRequest,
} from "../../../api/advanced/fees.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { useAssets } from "../../../api/queries.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import {
  FEE_RUN_MODES,
  FEE_SIDES,
  QUOTE_SUBSYSTEMS,
  parseScheduleKey,
  parseUsdInput,
  parseUtcInstant,
  type FeeSide,
} from "../../../lib/fees.ts";
import { parseBaseUnits, parseDecimalPrice, parseText, parseUuid, parseWholeNumber } from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, NoAccount, ScrollTable } from "../shared.tsx";
import { Choice, CommandFlow, CommandForm, TextInput, Toggle, prepare } from "../treasury/command.tsx";
import { ChargesView, DisclosureView, FeesNav, RecordIds, UnitAmount, VerificationView, feesHref, useIsOperator } from "./parts.tsx";

export function FeeQuotesPage(): ReactNode {
  const account = useAdvancedAccount();
  const operator = useIsOperator();
  return (
    <Page title="Quote disclosures" lead="Every quote disclosed to this account: what an order was priced at, component by component, under which schedule version.">
      <SimulatedNotice />
      <FeesNav />
      {account.accountId === undefined ? <NoAccount /> : <QuotesPanel accountId={account.accountId} />}
      {operator && <RecordQuotePanel />}
    </Page>
  );
}

function QuotesPanel(props: { readonly accountId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const quotes = useFeeQuotes(props.accountId, trail.current);
  return (
    <Panel title="This account's quote disclosures" description="Newest first. Each one is bound to this account as its payer: no other account can be charged it.">
      <AsyncPanel
        query={quotes}
        loadingLabel="Loading the quote disclosures…"
        empty={{
          isEmpty: (page: CursorPage<FeeQuoteRecord>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No quote disclosures",
          body: "The backend has no quote disclosed to this account.",
        }}
      >
        {(page: CursorPage<FeeQuoteRecord>) => (
          <>
            <ScrollTable caption="Quote disclosures" headers={["Quoted", "Schedule", "Side", "Spread", "Explicit fees", "Total", "Valid until"]}>
              {page.items.map((quote) => {
                const unit = { decimals: quote.disclosure.input.quote_decimals, symbol: undefined };
                const total = quote.side === "BUY" ? quote.disclosure.total_debit : quote.disclosure.total_credit;
                return (
                  <tr key={quote.id} data-quote={quote.id}>
                    <th scope="row">
                      <Link to={feesHref("quotes", quote.id)}>{formatInstant(quote.quoted_at)}</Link>
                    </th>
                    <td>
                      <code className="mono-small">{quote.schedule_key}</code> v{String(quote.schedule_version)}
                    </td>
                    <td>{quote.side}</td>
                    <td>
                      <UnitAmount value={quote.charges.spread} unit={unit} />
                    </td>
                    <td>
                      <UnitAmount value={quote.disclosure.fees.total} unit={unit} />
                    </td>
                    <td>
                      <UnitAmount value={total} unit={unit} />
                    </td>
                    <td>{formatInstant(quote.valid_until)}</td>
                  </tr>
                );
              })}
            </ScrollTable>
            <CursorPager
              noun="disclosures"
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

/** One quote disclosure, re-proved from the record by the backend. */
export function FeeQuotePage(): ReactNode {
  const { disclosureId = "" } = useParams();
  const account = useAdvancedAccount();
  const quote = useFeeQuote(account.accountId, disclosureId);
  const assets = useAssets();
  return (
    <Page title="Quote disclosure" lead="One stored disclosure, re-proved against the schedule version that priced it.">
      <SimulatedNotice />
      <FeesNav />
      <AsyncPanel query={quote} loadingLabel="Loading the disclosure…">
        {(record: FeeQuoteRecord) => (
          <>
            <Panel title="The disclosure" description="Re-proved from the stored record alone: the schedule decoded and proven canonical, the disclosure decoded, hashed as recorded, and recomputed from the schedule.">
              {record.verification === undefined ? (
                <p className="note" role="status">
                  The backend sent no verification with this record.
                </p>
              ) : (
                <VerificationView verification={record.verification} />
              )}
              <FieldGrid columns={3}>
                <Field label="Schedule">
                  <code className="mono-small">{record.schedule_key}</code> v{String(record.schedule_version)}
                </Field>
                <Field label="Subsystem">{record.subsystem}</Field>
                <Field label="Payer" note="Only this account can be charged the quote.">
                  {record.account_id === undefined ? <span className="absent">not bound to a payer</span> : <Identifier value={record.account_id} />}
                </Field>
                <Field label="Quoted">{formatInstant(record.quoted_at)}</Field>
                <Field label="Valid until" note="A booking after this instant is refused.">
                  {formatInstant(record.valid_until)}
                </Field>
                <Field label="Disclosure hash">
                  <span className="mono-small">{record.disclosure_hash}</span>
                </Field>
              </FieldGrid>
              <RecordIds ids={[["disclosure", record.id], ["schedule", record.schedule_id], ["base asset", record.base_asset_id]]} />
            </Panel>
            <Panel title="Priced, line by line — simulated" description="Exactly as the fee engine itemized it.">
              <DisclosureView disclosure={record.disclosure} />
              <h3>Charged, component by component</h3>
              <ChargesView
                charges={record.charges}
                unit={{ decimals: record.disclosure.input.quote_decimals, symbol: undefined }}
                assets={assets.data ?? []}
              />
            </Panel>
          </>
        )}
      </AsyncPanel>
    </Page>
  );
}

function RecordQuotePanel(): ReactNode {
  const record = useRecordQuote();
  const [scheduleKey, setScheduleKey] = useState("");
  const [subsystem, setSubsystem] = useState<(typeof QUOTE_SUBSYSTEMS)[number]>("ORDERS");
  const [mode, setMode] = useState<(typeof FEE_RUN_MODES)[number]>("PAPER");
  const [payer, setPayer] = useState("");
  const [baseAsset, setBaseAsset] = useState("");
  const [side, setSide] = useState<FeeSide>("BUY");
  const [quantity, setQuantity] = useState("");
  const [decimals, setDecimals] = useState("6");
  const [price, setPrice] = useState("");
  const [source, setSource] = useState("");
  const [volume, setVolume] = useState("");
  const [subscription, setSubscription] = useState(false);
  const [strategy, setStrategy] = useState(false);
  const [at, setAt] = useState("");

  const k = parseScheduleKey(scheduleKey);
  const pa = payer.trim() === "" ? undefined : parseUuid(payer, "The payer account id");
  const ba = baseAsset.trim() === "" ? undefined : parseUuid(baseAsset, "The base asset id");
  const q = parseBaseUnits(quantity, "The quantity");
  const d = parseWholeNumber(decimals, 0, 38, "The base asset's decimals");
  const p = parseDecimalPrice(price, "The reference price");
  const so = source.trim() === "" ? undefined : parseText(source, "The price source", 100);
  const v = volume.trim() === "" ? undefined : parseUsdInput(volume, "The trailing volume");
  const a = at.trim() === "" ? undefined : parseUtcInstant(at, "The instant");
  const optional = (parsed: { readonly ok: boolean; readonly error?: string } | undefined): string | undefined =>
    parsed === undefined || parsed.ok ? undefined : (parsed.error ?? "This field is not valid.");
  const prepared = prepare<FeeQuoteRecordRequest>(
    [k.ok ? undefined : k.error, q.ok ? undefined : q.error, d.ok ? undefined : d.error, p.ok ? undefined : p.error, optional(pa), optional(ba), optional(so), optional(v), optional(a)],
    () => ({
      schedule_key: k.ok ? k.value : "",
      subsystem,
      mode,
      side,
      base_quantity: q.ok ? q.value : "",
      base_decimals: d.ok ? d.value : 0,
      reference_price: p.ok ? p.value : "",
      has_subscription: subscription,
      strategy_originated: strategy,
      ...(pa !== undefined && pa.ok ? { account_id: pa.value } : {}),
      ...(ba !== undefined && ba.ok ? { base_asset_id: ba.value } : {}),
      ...(so !== undefined && so.ok ? { source: so.value } : {}),
      ...(v !== undefined && v.ok ? { trailing_volume_usd: v.value } : {}),
      ...(a !== undefined && a.ok ? { at: a.value } : {}),
    }),
  );
  return (
    <Panel
      title="Record a quote"
      description="Prices an order under the schedule in force, verifies the quote and its disclosure against the schedule, and stores the disclosure. Naming a payer binds it: only that account can then be charged it. Recording moves no value."
    >
      <CommandForm label="Record a quote">
        <TextInput id="fees-record-key" label="Schedule key" value={scheduleKey} onChange={setScheduleKey} />
        <Choice id="fees-record-subsystem" label="Subsystem" value={subsystem} options={QUOTE_SUBSYSTEMS} onChange={setSubsystem} />
        <Choice id="fees-record-mode" label="Mode" value={mode} options={FEE_RUN_MODES} onChange={setMode} />
        <TextInput id="fees-record-payer" label="Payer account id (optional)" value={payer} onChange={setPayer} />
        <TextInput id="fees-record-base-asset" label="Base asset id (optional)" value={baseAsset} onChange={setBaseAsset} />
        <Choice id="fees-record-side" label="Side" value={side} options={FEE_SIDES} onChange={setSide} />
        <TextInput id="fees-record-quantity" label="Quantity, in base units" value={quantity} onChange={setQuantity} inputMode="numeric" />
        <TextInput id="fees-record-decimals" label="Decimals of the asset priced" value={decimals} onChange={setDecimals} inputMode="numeric" />
        <TextInput id="fees-record-price" label="Reference price of one whole unit, in the schedule's quote asset" value={price} onChange={setPrice} inputMode="decimal" />
        <TextInput id="fees-record-source" label="Where the reference price came from (optional)" value={source} onChange={setSource} />
        <TextInput id="fees-record-volume" label="Trailing volume in USD (optional)" value={volume} onChange={setVolume} inputMode="decimal" />
        <Toggle id="fees-record-subscription" label="Has a subscription" checked={subscription} onChange={setSubscription} />
        <Toggle id="fees-record-strategy" label="Strategy-originated order" checked={strategy} onChange={setStrategy} />
        <TextInput id="fees-record-at" label="Price at an instant in UTC (optional)" value={at} onChange={setAt} note="Empty means now." />
        <CommandFlow<FeeQuoteRecordRequest, FeeQuoteRecord>
          label="Record the quote"
          prepared={prepared}
          command={record}
          refusedNote="No quote was recorded."
          describe={(request) => (
            <FieldGrid columns={3}>
              <Field label="Schedule">
                <code className="mono-small">{request.schedule_key}</code>
              </Field>
              <Field label="Order">
                {request.side} <code className="mono-small">{request.base_quantity}</code> at {request.reference_price}
              </Field>
              <Field label="Payer">{request.account_id === undefined ? "not bound" : <Identifier value={request.account_id} />}</Field>
              <Field label="Subsystem and mode">
                {request.subsystem} · {request.mode}
              </Field>
            </FieldGrid>
          )}
          renderResult={(quote) => (
            <p>
              Recorded <Link to={feesHref("quotes", quote.id)}>disclosure {quote.id}</Link> under {quote.schedule_key} v
              {String(quote.schedule_version)}, valid until {formatInstant(quote.valid_until)}.
            </p>
          )}
        />
      </CommandForm>
    </Panel>
  );
}
