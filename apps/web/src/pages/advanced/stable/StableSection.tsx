/**
 * THE SANDBOX STABLE ASSET (internal/stableasset): a platform stable asset
 * that exists only as simulated value on the simulated chain sbx-solana,
 * issued by a STABLE_ISSUER facility against a fully reserved policy whose
 * peg is quoted in the sandbox dollar.
 *
 * An issuance (a mint) is paid in a dollar-equivalent reserve asset and posts
 * only once its chain transaction is final; a redemption (a burn) is its exact
 * inverse. The reserve and its coverage of issued supply are the issuer's
 * books, and signed, hash-linked reserve snapshots let anyone check them
 * against the snapshot signer's public key — a snapshot is not an
 * attestation, and says so in its body. Every figure is the backend's, in base
 * units or exact decimals, and simulated.
 *
 * Every route answers UNSUPPORTED where the deployment has no sandbox chain;
 * the pages show that refusal with its code.
 */
import type { ReactNode } from "react";
import { Link, Route, Routes } from "react-router-dom";

import {
  useStableAssets,
  useStableHoldings,
  type StableAsset,
  type StableAssetList,
  type StableHoldings,
} from "../../../api/advanced/stable.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { AsOf, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Bps, DecimalValue, Qty } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { ASSET_STATUS_MEANING, assetStatusTone, reserveKindLabel } from "../../../lib/stable.ts";
import { useAdvancedAccount } from "../area.ts";
import { AdvancedNotFound, NoAccount, ScrollTable } from "../shared.tsx";
import { StableAssetPage } from "./StableAssetPage.tsx";
import { IssuanceDetailPage, IssuancesPage, RedemptionDetailPage, RedemptionsPage } from "./StableHistory.tsx";
import { StableOperatorPage } from "./StableOperator.tsx";
import { StableReservePage } from "./StableReserve.tsx";
import { StableNav } from "./parts.tsx";
import { reserveHref, stableAssetHref } from "./paths.ts";

/** The section: assets and holdings, one asset, its reserve, the account's requests, and the operator's page. */
export function StableAssetsSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<StableOverviewPage />} />
      <Route path="issuances" element={<IssuancesPage />} />
      <Route path="issuances/:issuanceId" element={<IssuanceDetailPage />} />
      <Route path="redemptions" element={<RedemptionsPage />} />
      <Route path="redemptions/:redemptionId" element={<RedemptionDetailPage />} />
      <Route path="operator" element={<StableOperatorPage />} />
      <Route path=":stableAssetId" element={<StableAssetPage />} />
      <Route path=":stableAssetId/reserve" element={<StableReservePage />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}

function StableOverviewPage(): ReactNode {
  return (
    <Page
      title="Stable assets"
      lead="The platform's sandbox stable assets: simulated units on a simulated chain, issued against a fully reserved policy in the sandbox dollar, and what the account holds of them."
    >
      <SimulatedNotice />
      <StableNav />
      <AssetsPanel />
      <HoldingsPanel />
    </Page>
  );
}

function AssetsPanel(): ReactNode {
  const assets = useStableAssets();
  return (
    <Panel
      title="Registered stable assets"
      description="Each with its status, its peg in the sandbox dollar, its issuance cap and the reserve policy that backs it, as the backend lists them."
    >
      <AsyncPanel
        query={assets}
        loadingLabel="Loading the stable assets…"
        empty={{
          isEmpty: (data: StableAssetList) => data.items.length === 0,
          title: "No stable asset is registered",
          body: "The backend lists no stable asset in this deployment. An operator registers one; nothing is shown in its place.",
        }}
      >
        {(data: StableAssetList) => (
          <ScrollTable
            caption="Stable assets"
            headers={["Asset", "Status", "Peg", "Issuance cap", "Reserve policy", "Chain", "Pages"]}
          >
            {data.items.map((asset) => (
              <AssetRow key={asset.id} asset={asset} />
            ))}
          </ScrollTable>
        )}
      </AsyncPanel>
    </Panel>
  );
}

function AssetRow(props: { readonly asset: StableAsset }): ReactNode {
  const { asset } = props;
  return (
    <tr data-stable-asset={asset.id}>
      <th scope="row">
        <Link to={stableAssetHref(asset.id)}>{asset.symbol}</Link>
        <span className="cell-note">{asset.name}</span>
      </th>
      <td>
        <Pill tone={assetStatusTone(asset.status)} title={ASSET_STATUS_MEANING[asset.status]}>
          {asset.status}
        </Pill>
      </td>
      <td>
        <DecimalValue value={asset.peg.decimal} /> {asset.peg_currency}
        <span className="cell-note">per whole unit, quoted in the sandbox dollar</span>
      </td>
      <td>
        <Qty value={asset.issuance_cap} decimals={asset.decimals} symbol={asset.symbol} />
        <span className="cell-note">
          <BaseUnits value={asset.issuance_cap} />
        </span>
      </td>
      <td>
        <ul className="codes">
          {asset.reserves.map((reserve) => (
            <li key={reserve.asset_id}>
              {reserve.symbol}: {reserveKindLabel(reserve.kind)}, factor <Bps value={reserve.factor_bps} />
              {reserve.accepted_for_mint ? ", pays for an issuance" : ""}
            </li>
          ))}
        </ul>
      </td>
      <td>
        <code className="mono-small">{asset.chain}</code>
        <span className="cell-note">network {asset.network}</span>
      </td>
      <td>
        <Link to={reserveHref(asset.id)}>Reserve and snapshots</Link>
      </td>
    </tr>
  );
}

function HoldingsPanel(): ReactNode {
  const account = useAdvancedAccount();
  const holdings = useStableHoldings(account.accountId);
  return (
    <Panel
      title="Holdings"
      description="What the account holds of each stable asset on the books, how much of it is spoken for, and the custodied wallet's balance on the simulated chain the books are reconciled against; and what it can pay an issuance with."
    >
      {account.accountId === undefined ? (
        <NoAccount />
      ) : (
        <AsyncPanel query={holdings} loadingLabel="Loading the account's stable asset holdings…">
          {(data: StableHoldings) => <HoldingsBody data={data} />}
        </AsyncPanel>
      )}
    </Panel>
  );
}

function HoldingsBody(props: { readonly data: StableHoldings }): ReactNode {
  const { data } = props;
  return (
    <>
      <AsOf at={data.as_of} />
      <p className="meta-row">
        <span>
          Account <Identifier value={data.account_id} />
        </span>
        <span>
          Custodied wallet on the simulated chains <Identifier value={data.holder_address} />
        </span>
      </p>
      {data.items.length === 0 ? (
        <p className="absent">No stable asset is registered, so the account holds none.</p>
      ) : (
        <ScrollTable
          caption="Stable asset holdings"
          headers={["Asset", "Status", "On the books", "Reserved", "Held", "Available", "On the simulated chain"]}
        >
          {data.items.map((item) => (
            <tr key={item.stable_asset_id} data-stable-asset={item.stable_asset_id}>
              <th scope="row">
                <Link to={stableAssetHref(item.stable_asset_id)}>{item.symbol}</Link>
                <span className="cell-note mono-small">{item.chain}</span>
              </th>
              <td>
                <Pill tone={assetStatusTone(item.status)}>{item.status}</Pill>
              </td>
              <td data-field="wallet">
                <Qty value={item.wallet} decimals={item.decimals} symbol={item.symbol} />
              </td>
              <td>
                <Qty value={item.reserved} decimals={item.decimals} />
              </td>
              <td>
                <Qty value={item.held} decimals={item.decimals} />
              </td>
              <td data-field="available">
                <Qty value={item.available} decimals={item.decimals} symbol={item.symbol} />
              </td>
              <td data-field="chain_balance">
                <Qty value={item.chain_balance} decimals={item.decimals} symbol={item.symbol} />
                <span className="cell-note">a simulation the books are reconciled against, not a second balance</span>
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
      <h3>What an issuance can be paid in</h3>
      {data.payment_assets.length === 0 ? (
        <p className="absent">No registered stable asset accepts a payment asset, so nothing is listed.</p>
      ) : (
        <ScrollTable caption="Payment assets" headers={["Asset", "On the books", "Reserved", "Held", "Available"]}>
          {data.payment_assets.map((asset) => (
            <tr key={asset.asset_id} data-asset={asset.asset_id}>
              <th scope="row">
                {asset.symbol}
                <span className="cell-note mono-small">{asset.asset_id}</span>
              </th>
              <td>
                <Qty value={asset.wallet} decimals={asset.decimals} symbol={asset.symbol} />
              </td>
              <td>
                <Qty value={asset.reserved} decimals={asset.decimals} />
              </td>
              <td>
                <Qty value={asset.held} decimals={asset.decimals} />
              </td>
              <td data-field="available">
                <Qty value={asset.available} decimals={asset.decimals} symbol={asset.symbol} />
              </td>
            </tr>
          ))}
        </ScrollTable>
      )}
      <p className="field-note">
        Available is the backend's own arithmetic: what is on the books less what reservations and withdrawal holds
        speak for, never below zero. Every figure is simulated value in base units, shown as the decimal it denotes.
      </p>
    </>
  );
}
