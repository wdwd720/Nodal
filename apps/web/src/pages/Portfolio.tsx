/**
 * PORTFOLIO (PART 111): holdings, units, USD mark, basis, realized and
 * unrealized P&L, and where the underlying is actually held.
 *
 * "Units" and "USD mark" are two different claims and are shown as two columns:
 * the units are what the ledger says you hold, exactly, in the asset's own base
 * units; the mark is a valuation the backend computed from a named price
 * reference. Conflating them is how an interface ends up implying a customer
 * holds dollars.
 */
import { useMemo, type ReactNode } from "react";

import {
  useAssets,
  useHoldings,
  useLedger,
  type Asset,
  type HoldingsResponse,
  type JournalTransaction,
} from "../api/queries.ts";
import { AsyncPanel, EmptyState } from "../components/DataState.tsx";
import { DownloadLink } from "../components/Button.tsx";
import {
  AsOf,
  Disclosure,
  Field,
  FieldGrid,
  Identifier,
  Page,
  Panel,
  Pill,
  Table,
} from "../components/Layout.tsx";
import { AssetLine } from "../components/MintIdentity.tsx";
import { BaseUnits, Qty, Usd } from "../components/Money.tsx";
import { USDC_DISCLOSURE, USD_VALUATION_NOTE } from "../lib/honesty.ts";
import { formatInstant } from "../lib/time.ts";
import { useActiveAccountId } from "../session.tsx";
import { API_BASE } from "../api/client.ts";

export function Portfolio(): ReactNode {
  const accountId = useActiveAccountId();
  const holdings = useHoldings(accountId);
  const assets = useAssets();
  const ledger = useLedger(accountId);

  const assetById = useMemo(() => {
    const map = new Map<string, Asset>();
    for (const asset of assets.data ?? []) map.set(asset.id, asset);
    return map;
  }, [assets.data]);

  if (accountId === undefined) {
    return (
      <Page title="Portfolio">
        <EmptyState title="No account" body="The backend returned no accounts for this session." />
      </Page>
    );
  }

  return (
    <Page
      title="Portfolio"
      lead="What this account holds, valued by the backend, with the exact units alongside every valuation."
      actions={
        <DownloadLink
          href={`${API_BASE}/accounts/${accountId}/export?format=csv`}
          fileName={`nodal-${accountId}.csv`}
        >
          Export as CSV
        </DownloadLink>
      }
    >
      <Panel title="Holdings" description="One row per asset the ledger records for this account.">
        <AsyncPanel
          query={holdings}
          loadingLabel="Loading holdings…"
          empty={{
            isEmpty: (data: HoldingsResponse) => data.holdings.length === 0,
            title: "No holdings",
            body: "The backend returned no holdings for this account. Nothing is being displayed in their place.",
          }}
        >
          {(data: HoldingsResponse) => (
            <>
              <Table
                caption="Holdings"
                headers={[
                  "Asset",
                  "Units held",
                  "USD mark",
                  "Cost basis",
                  "Unrealized P&L",
                  "Realized P&L",
                  "Price reference",
                ]}
              >
                {data.holdings.map((holding) => {
                  const asset = assetById.get(holding.asset);
                  return (
                    <tr key={holding.asset}>
                      <th scope="row">
                        <AssetLine asset={asset} fallbackId={holding.asset} />
                      </th>
                      <td>
                        <Qty
                          value={holding.quantity}
                          decimals={holding.decimals}
                          symbol={holding.symbol}
                        />
                        <div>
                          <BaseUnits value={holding.quantity} />
                        </div>
                      </td>
                      <td>
                        <Usd value={holding.usd_mark} />
                      </td>
                      <td>
                        <Usd value={holding.cost_basis_usd} />
                      </td>
                      <td>
                        <Usd value={holding.unrealized_pnl_usd} signed />
                      </td>
                      <td>
                        <Usd
                          value={holding.realized_pnl_usd}
                          signed
                          absent="not reported for this holding"
                        />
                      </td>
                      <td className="mono-small">{holding.price_ref ?? "not reported"}</td>
                    </tr>
                  );
                })}
              </Table>
              <AsOf at={data.as_of} />

              <h3>Where the underlying is held</h3>
              <Table caption="Custody" headers={["Asset", "Chain", "Mint", "Custody location"]}>
                {data.holdings.map((holding) => (
                  <tr key={`custody-${holding.asset}`}>
                    <th scope="row">{holding.symbol}</th>
                    <td>{holding.chain ?? assetById.get(holding.asset)?.chain ?? "not reported"}</td>
                    <td>
                      <code className="mono-small">
                        {holding.mint_address ?? assetById.get(holding.asset)?.mint_address ?? "not reported"}
                      </code>
                    </td>
                    <td>
                      {holding.location === undefined || holding.location === "" ? (
                        <span className="absent">
                          the backend returned no custody location for this holding
                        </span>
                      ) : (
                        <Identifier value={holding.location} />
                      )}
                    </td>
                  </tr>
                ))}
              </Table>
              <p className="note">
                A custody location is the address the tokens actually sit at. Where the backend does
                not report one, this says so rather than showing a plausible-looking placeholder.
              </p>
            </>
          )}
        </AsyncPanel>

        <Disclosure title="What these valuations are">
          <p>{USDC_DISCLOSURE}</p>
          <p>{USD_VALUATION_NOTE}</p>
        </Disclosure>
      </Panel>

      <Panel
        title="Ledger history"
        description="The append-only postings behind the holdings above."
      >
        <AsyncPanel
          query={ledger}
          loadingLabel="Loading journal transactions…"
          empty={{
            isEmpty: (list: JournalTransaction[]) => list.length === 0,
            title: "No journal transactions",
            body: "The backend has posted nothing to this account's ledger yet.",
          }}
        >
          {(list: JournalTransaction[]) => (
            <>
              {list.map((transaction) => (
                <div className="ledger-entry" key={transaction.id}>
                  <div className="ledger-head">
                    <Pill tone="neutral">{transaction.kind}</Pill>
                    <span>{formatInstant(transaction.posted_at)}</span>
                    <Identifier value={transaction.id} />
                  </div>
                  {transaction.description !== undefined && (
                    <p className="ledger-description">{transaction.description}</p>
                  )}
                  <Table
                    caption={`Entries for ${transaction.id}`}
                    headers={["Seq", "Account", "Asset", "Side", "Quantity", "USD value"]}
                  >
                    {transaction.entries.map((entry) => {
                      const asset = assetById.get(entry.asset);
                      return (
                        <tr key={`${transaction.id}-${String(entry.seq)}`}>
                          <td>{String(entry.seq)}</td>
                          <th scope="row">{entry.account_code}</th>
                          <td>
                            <AssetLine asset={asset} fallbackId={entry.asset} />
                          </td>
                          <td>
                            <Pill tone={entry.side === "DEBIT" ? "info" : "neutral"}>{entry.side}</Pill>
                          </td>
                          <td>
                            <Qty value={entry.quantity} decimals={asset?.decimals} />
                            <div>
                              <BaseUnits value={entry.quantity} />
                            </div>
                          </td>
                          <td>
                            <Usd value={entry.usd_value} absent="not valued" />
                          </td>
                        </tr>
                      );
                    })}
                  </Table>
                  <FieldGrid columns={3}>
                    <Field label="Effective at">{formatInstant(transaction.effective_at)}</Field>
                    <Field label="Reference">
                      {transaction.reference === undefined ? (
                        <span className="absent">not reported</span>
                      ) : (
                        <span className="mono-small">
                          {transaction.reference.type} · {transaction.reference.id}
                        </span>
                      )}
                    </Field>
                    <Field label="Content hash" note="Covers the posting; changing it would change this hash.">
                      <Identifier value={transaction.content_hash} />
                    </Field>
                  </FieldGrid>
                </div>
              ))}
            </>
          )}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}
