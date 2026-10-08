/**
 * FEES — the operator's spread audit.
 *
 * Every charged spread, oldest first, with the disclosure it charged, the
 * schedule version that priced it, the prices it moved between and the
 * journal that carried it; and the totals of one quote asset — what was
 * booked and what was reversed, shown side by side and never subtracted here.
 * A customer is refused by the backend (sandbox:operate), and the page shows
 * the refusal.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { useSpreadAudit, useSpreadTotals, type FeeSpreadAudit, type FeeSpreadTotals, type SpreadAuditFilter } from "../../../api/advanced/fees.ts";
import { useAssets } from "../../../api/queries.ts";
import { Button } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Bps } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { bookingStatusTone, parseUtcInstant } from "../../../lib/fees.ts";
import { parseUuid } from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { ScrollTable } from "../shared.tsx";
import { CommandForm, TextInput, Toggle } from "../treasury/command.tsx";
import { FeesNav, PriceValue, RegistryAmount, feesHref } from "./parts.tsx";

export function SpreadAuditPage(): ReactNode {
  return (
    <Page title="Spread audit" lead="Every unit of spread charged, traceable to its disclosure, its schedule version and its journal, and the totals of a quote asset.">
      <SimulatedNotice />
      <FeesNav />
      <AuditPanel />
      <TotalsPanel />
    </Page>
  );
}

function optionalUuid(raw: string, what: string): { readonly ok: true; readonly value: string | undefined } | { readonly ok: false; readonly error: string } {
  if (raw.trim() === "") return { ok: true, value: undefined };
  return parseUuid(raw, what);
}

function optionalInstant(raw: string, what: string): { readonly ok: true; readonly value: string | undefined } | { readonly ok: false; readonly error: string } {
  if (raw.trim() === "") return { ok: true, value: undefined };
  return parseUtcInstant(raw, what);
}

function AuditPanel(): ReactNode {
  const assets = useAssets();
  const [quoteAsset, setQuoteAsset] = useState("");
  const [account, setAccount] = useState("");
  const [since, setSince] = useState("");
  const [until, setUntil] = useState("");
  const [includeReversed, setIncludeReversed] = useState(false);
  const [filter, setFilter] = useState<SpreadAuditFilter | undefined>(undefined);
  const audit = useSpreadAudit(filter);
  const q = optionalUuid(quoteAsset, "The quote asset id");
  const a = optionalUuid(account, "The account id");
  const s = optionalInstant(since, "Since");
  const u = optionalInstant(until, "Until");
  const problem = [q, a, s, u].map((p) => (p.ok ? undefined : p.error)).find((p) => p !== undefined);
  return (
    <Panel title="Charged spreads" description="Oldest first, at most two hundred lines. Every line names the disclosure it charged, which re-proves it.">
      <CommandForm label="Filter the spread audit">
        <TextInput id="fees-audit-quote" label="Quote asset id (optional)" value={quoteAsset} onChange={setQuoteAsset} />
        <TextInput id="fees-audit-account" label="Account id (optional)" value={account} onChange={setAccount} />
        <TextInput id="fees-audit-since" label="Booked since, in UTC (optional)" value={since} onChange={setSince} />
        <TextInput id="fees-audit-until" label="Booked before, in UTC (optional)" value={until} onChange={setUntil} />
        <Toggle id="fees-audit-reversed" label="Include reversed charges" checked={includeReversed} onChange={setIncludeReversed} />
        <div className="form-actions">
          {problem !== undefined ? (
            <Button disabledReason={problem}>Show the charged spreads</Button>
          ) : (
            <Button
              onClick={() => {
                setFilter({
                  quoteAssetId: q.ok ? q.value : undefined,
                  accountId: a.ok ? a.value : undefined,
                  since: s.ok ? s.value : undefined,
                  until: u.ok ? u.value : undefined,
                  includeReversed,
                });
              }}
            >
              Show the charged spreads
            </Button>
          )}
        </div>
      </CommandForm>
      {filter !== undefined && (
        <AsyncPanel
          query={audit}
          loadingLabel="Loading the spread audit…"
          empty={{
            isEmpty: (data: FeeSpreadAudit) => data.items.length === 0,
            title: "No spread charged",
            body: "The backend reports no charged spread this filter selects.",
          }}
        >
          {(data: FeeSpreadAudit) => (
            <ScrollTable
              caption="Charged spreads"
              headers={["Booked", "Payer", "Schedule", "Side and quantity", "Reference → execution", "Spread", "Status", "Journal"]}
            >
              {data.items.map((line) => (
                <tr key={line.booking_id} data-booking={line.booking_id}>
                  <th scope="row">
                    <Link to={feesHref("bookings", line.booking_id)}>{formatInstant(line.booked_at)}</Link>
                  </th>
                  <td>
                    <Identifier value={line.account_id} />
                  </td>
                  <td>
                    <code className="mono-small">{line.schedule_key}</code> v{String(line.schedule_version)}
                  </td>
                  <td>
                    {line.side} <code className="mono-small">{line.base_quantity}</code>
                  </td>
                  <td>
                    <PriceValue mantissa={line.reference_price.mantissa} scale={line.reference_price.scale} /> →{" "}
                    <PriceValue mantissa={line.execution_price.mantissa} scale={line.execution_price.scale} />
                  </td>
                  <td>
                    <RegistryAmount assets={assets.data ?? []} id={line.quote_asset_id} value={line.spread_amount} /> <Bps value={line.spread_bps} />
                  </td>
                  <td>
                    <Pill tone={bookingStatusTone(line.status)}>{line.status}</Pill>
                    <span className="cell-note">{line.booking_mode}</span>
                  </td>
                  <td>
                    <Identifier value={line.journal_transaction_id} />
                    {line.reversal_journal_transaction_id !== undefined && <Identifier label="reversal" value={line.reversal_journal_transaction_id} />}
                  </td>
                </tr>
              ))}
            </ScrollTable>
          )}
        </AsyncPanel>
      )}
    </Panel>
  );
}

function TotalsPanel(): ReactNode {
  const assets = useAssets();
  const [draft, setDraft] = useState("");
  const [asked, setAsked] = useState<string | undefined>(undefined);
  const totals = useSpreadTotals(asked);
  const parsed = parseUuid(draft, "The quote asset id");
  return (
    <Panel
      title="Spread totals"
      description="The spread booked and the spread reversed in one quote asset. Booked less reversed is what the platform's spread revenue holds from these bookings."
    >
      <CommandForm label="Read the spread totals">
        <TextInput id="fees-totals-quote" label="Quote asset id" value={draft} onChange={setDraft} />
        <div className="form-actions">
          {parsed.ok ? (
            <Button
              onClick={() => {
                setAsked(parsed.value);
              }}
            >
              Show the totals
            </Button>
          ) : (
            <Button disabledReason={parsed.error}>Show the totals</Button>
          )}
        </div>
      </CommandForm>
      {asked !== undefined && (
        <AsyncPanel query={totals} loadingLabel="Loading the spread totals…">
          {(data: FeeSpreadTotals) => (
            <FieldGrid columns={2}>
              <Field label="Booked — simulated">
                <span data-field="spread_booked">
                  <RegistryAmount assets={assets.data ?? []} id={data.quote_asset_id} value={data.booked} />
                </span>
              </Field>
              <Field label="Reversed">
                <span data-field="spread_reversed">
                  <RegistryAmount assets={assets.data ?? []} id={data.quote_asset_id} value={data.reversed} />
                </span>
              </Field>
            </FieldGrid>
          )}
        </AsyncPanel>
      )}
    </Panel>
  );
}
