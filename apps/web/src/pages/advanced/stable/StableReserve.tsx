/**
 * RESERVE TRANSPARENCY for one stable asset: the issuer's reserve ledger and
 * its coverage of issued supply at the peg, the collateral composition the
 * latest snapshot states, and the chain of signed reserve snapshots with the
 * backend's verification of it against the snapshot signer's public key.
 *
 * Every figure is the backend's exact string. A snapshot's verified or
 * unverified badge is the backend's verification result, never a check this
 * page makes or guesses at; the canonical body, its hash and its signature are
 * shown so anyone can check them independently. A snapshot is a signed record
 * of simulated books and a simulated chain. It is not an attestation and not
 * an audit, and its body says so.
 */
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useStableAsset,
  useStableReserve,
  useStableSnapshots,
  type StableReserve,
  type StableReserveHolding,
  type StableSnapshot,
  type StableSnapshotChain,
} from "../../../api/advanced/stable.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { AsOf, Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Bps, DecimalValue, Qty } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import {
  SNAPSHOT_FAILURE_MEANING,
  formatBpsText,
  reserveKindLabel,
  snapshotsShownNote,
  verificationSummary,
} from "../../../lib/stable.ts";
import { formatInstant } from "../../../lib/time.ts";
import { ScrollTable } from "../shared.tsx";
import { PriceLine, StableNav } from "./parts.tsx";
import { stableAssetHref } from "./paths.ts";

export function StableReservePage(): ReactNode {
  const { stableAssetId } = useParams();
  const asset = useStableAsset(stableAssetId);
  const reserve = useStableReserve(stableAssetId);
  const snapshots = useStableSnapshots(stableAssetId);
  const title = asset.isSuccess ? `${asset.data.asset.symbol} reserve` : "Stable asset reserve";
  return (
    <Page
      title={title}
      lead="The issuer's reserve, its coverage of issued supply at the peg, and the signed snapshot chain anyone can verify against the snapshot signer's public key."
      actions={stableAssetId === undefined ? undefined : <LinkButton to={stableAssetHref(stableAssetId)}>The stable asset</LinkButton>}
    >
      <SimulatedNotice />
      <StableNav />
      <Panel
        title="Reserve ledger"
        description="Issued supply and the reserve are the issuer facility's ledger balances; pending figures come from request states and are never booked. Each reserve asset counts at its verified price and eligibility factor; one with no fresh price counts zero."
      >
        <AsyncPanel query={reserve} loadingLabel="Loading the reserve ledger…">
          {(data: StableReserve) => <ReserveBody reserve={data} />}
        </AsyncPanel>
      </Panel>
      <Panel
        title="Snapshot chain"
        description="Machine-verifiable records of the books and the simulated chain at one instant, each signed and linked to the one before it. A snapshot is not an attestation and not an audit."
      >
        <AsyncPanel query={snapshots} loadingLabel="Loading the snapshot chain…">
          {(data: StableSnapshotChain) => <SnapshotChainBody chain={data} />}
        </AsyncPanel>
      </Panel>
    </Page>
  );
}

function HoldingsLines(props: { readonly caption: string; readonly lines: readonly StableReserveHolding[]; readonly none: string }): ReactNode {
  if (props.lines.length === 0) return <p className="absent">{props.none}</p>;
  return (
    <ScrollTable caption={props.caption} headers={["Asset", "Kind", "Quantity", "Verified price", "Factor", "Eligible value"]}>
      {props.lines.map((line) => (
        <tr key={`${line.asset_id}-${line.kind}`} data-asset={line.asset_id}>
          <th scope="row">
            {line.symbol}
            <span className="cell-note mono-small">{line.asset_id}</span>
          </th>
          <td>{reserveKindLabel(line.kind)}</td>
          <td>
            <Qty value={line.quantity} decimals={line.decimals} symbol={line.symbol} />
            <span className="cell-note">
              <BaseUnits value={line.quantity} />
            </span>
          </td>
          <td>
            <PriceLine price={line.price} absent="no fresh price: counts zero" />
          </td>
          <td>
            <Bps value={line.factor_bps} />
          </td>
          <td data-field="eligible">
            <DecimalValue value={line.eligible} />
          </td>
        </tr>
      ))}
    </ScrollTable>
  );
}

function ReserveBody(props: { readonly reserve: StableReserve }): ReactNode {
  const r = props.reserve;
  return (
    <>
      <AsOf at={r.as_of} label="Valued at" />
      <FieldGrid columns={3}>
        <Field label="Coverage" emphasis note="The eligible reserve over issued supply at the peg, rounded down; full coverage when nothing is issued.">
          <span data-field="coverage_bps">
            <Bps value={r.coverage_bps} />
          </span>{" "}
          {r.covered ? <Pill tone="good">covered</Pill> : <Pill tone="bad">not covered</Pill>}
        </Field>
        <Field label="Issued supply" note="The issuer's STABLE_OUTSTANDING on the books.">
          <span data-field="issued_supply">
            <Qty value={r.issued_supply} decimals={r.decimals} symbol={r.symbol} />
          </span>
          <span className="cell-note">
            <BaseUnits value={r.issued_supply} />
          </span>
        </Field>
        <Field label="Target peg">
          <PriceLine price={r.peg} />
        </Field>
        <Field label="Supply at the peg" note="Issued supply times the peg, in whole units of the peg's quote asset, exact.">
          <span data-field="supply_value">
            <DecimalValue value={r.supply_value} />
          </span>
        </Field>
        <Field label="Eligible reserve" note="Both reserve lines at their verified prices and factors, exact.">
          <span data-field="eligible_reserve">
            <DecimalValue value={r.eligible_reserve} />
          </span>
        </Field>
        <Field label="Unverified reserve assets" note="Reserve assets holding a balance with no fresh price; each counts zero.">
          {r.unverified_reserve_asset_ids.length === 0 ? (
            <span className="absent">none</span>
          ) : (
            <ul className="codes">
              {r.unverified_reserve_asset_ids.map((id) => (
                <li key={id}>
                  <code className="mono-small">{id}</code>
                </li>
              ))}
            </ul>
          )}
        </Field>
        <Field label="Pending issuance" note="Supply reserved by issuances not yet final: derived from request states, never booked.">
          <Qty value={r.pending_mint} decimals={r.decimals} symbol={r.symbol} />
        </Field>
        <Field label="Pending redemption" note="Units of redemptions not yet final on the simulated chain.">
          <Qty value={r.pending_burn} decimals={r.decimals} symbol={r.symbol} />
        </Field>
      </FieldGrid>
      <h3>Dollar-equivalent reserve</h3>
      <HoldingsLines caption="Dollar-equivalent reserve" lines={r.cash_equivalent_reserve} none="No dollar-equivalent reserve asset in the policy." />
      <h3>Tokenized reserve</h3>
      <HoldingsLines caption="Tokenized reserve" lines={r.tokenized_reserve} none="No tokenized reserve asset in the policy." />
      <h3>Receivable</h3>
      <HoldingsLines
        caption="Receivable from pending issuances"
        lines={r.receivable}
        none="No issuance is pending, so no payment is held for one."
      />
      <h3>Payouts not yet posted</h3>
      <HoldingsLines
        caption="Payouts of pending redemptions"
        lines={r.pending_redemption}
        none="No redemption payout is pending."
      />
    </>
  );
}

function SnapshotChainBody(props: { readonly chain: StableSnapshotChain }): ReactNode {
  const { chain } = props;
  const v = chain.verification;
  const latest = chain.items.length > 0 ? chain.items[chain.items.length - 1] : undefined;
  return (
    <>
      <div className={v.verified ? "banner banner-good" : "banner banner-bad"} role="status" data-field="verification">
        <p>
          {v.verified ? <Pill tone="good">chain verified</Pill> : <Pill tone="bad">chain does not verify</Pill>}{" "}
          {verificationSummary(v)}
        </p>
        {v.reason !== undefined && <p className="field-note">{SNAPSHOT_FAILURE_MEANING[v.reason]}</p>}
      </div>
      <FieldGrid columns={2}>
        <Field label="Snapshot signer's public key" note="The Ed25519 key the backend verified the chain against.">
          <Identifier value={v.public_key} />
        </Field>
        <Field label="What a snapshot is" note="As every snapshot body states it.">
          <span className="cell-prose">{v.disclaimer}</span>
        </Field>
      </FieldGrid>
      {latest !== undefined && <Composition snapshot={latest} />}
      {chain.items.length === 0 ? (
        <p className="absent">No snapshot has been published for this asset.</p>
      ) : (
        <>
          <p className="field-note">{snapshotsShownNote(chain.total, chain.items.length)}</p>
          {chain.items.map((snapshot) => (
            <SnapshotCard key={snapshot.seq} snapshot={snapshot} />
          ))}
        </>
      )}
    </>
  );
}

/** The collateral composition the latest snapshot states: each reserve kind's share of the eligible reserve. */
function Composition(props: { readonly snapshot: StableSnapshot }): ReactNode {
  const body = props.snapshot.body;
  return (
    <>
      <h3>Collateral composition</h3>
      <p className="field-note">
        As snapshot {body.seq} states it, taken {formatInstant(body.taken_at)}. Shares are of the eligible reserve, rounded
        down by the backend.
      </p>
      <ScrollTable caption="Collateral composition" headers={["Reserve kind", "Eligible value", "Share"]}>
        {body.composition.map((line) => (
          <tr key={line.kind}>
            <th scope="row">{reserveKindLabel(line.kind)}</th>
            <td>
              <DecimalValue value={line.eligible} />
            </td>
            <td>
              {formatBpsText(line.share_bps)} <span className="mono-small">({line.share_bps} bps)</span>
            </td>
          </tr>
        ))}
      </ScrollTable>
    </>
  );
}

function SnapshotCard(props: { readonly snapshot: StableSnapshot }): ReactNode {
  const s = props.snapshot;
  const body = s.body;
  return (
    <section className="callout" aria-label={`Snapshot ${String(s.seq)}`} data-snapshot={String(s.seq)}>
      <h3>
        Snapshot {String(s.seq)}{" "}
        {s.verified ? (
          <Pill tone="good" title="The backend verified this snapshot and every one before it against the snapshot signer's public key.">
            verified
          </Pill>
        ) : (
          <Pill tone="bad" title="This snapshot, or one before it, did not verify against the snapshot signer's public key.">
            unverified
          </Pill>
        )}
      </h3>
      <FieldGrid columns={3}>
        <Field label="Taken">{formatInstant(s.taken_at)}</Field>
        <Field label="Supply on the books" note="The issuer's outstanding supply, in base units.">
          <span className="num">{body.ledger_supply}</span>
        </Field>
        <Field label="Supply on the simulated chain" note="The devnet mint's supply, in base units.">
          <span className="num">{body.chain_supply}</span>
        </Field>
        <Field label="Supply at the peg">
          <DecimalValue value={body.supply_value} />
        </Field>
        <Field label="Eligible reserve">
          <DecimalValue value={body.eligible_reserve} />
        </Field>
        <Field label="Coverage">
          {formatBpsText(body.coverage_bps)} <span className="mono-small">({body.coverage_bps} bps)</span>
        </Field>
        <Field label="Pending issuance and redemption" note="In base units, as the snapshot states them.">
          <span className="num">{body.pending_mint}</span> / <span className="num">{body.pending_burn}</span>
        </Field>
        <Field label="Hash" note="SHA-256 of the canonical body below.">
          <Identifier value={s.hash} />
        </Field>
        <Field label="Previous hash" note="Links this snapshot to the one before it.">
          {s.prev_hash === "" ? <span className="absent">none: the first snapshot</span> : <Identifier value={s.prev_hash} />}
        </Field>
        <Field label="Signature" note="Ed25519, over the domain-separated hash.">
          <Identifier value={s.signature} />
        </Field>
        <Field label="Signed with">
          <Identifier value={s.signer_public_key} />
        </Field>
        <Field label="Evidence">
          <Identifier value={s.evidence_ref} />
        </Field>
      </FieldGrid>
      <details>
        <summary>The canonical body, exactly as hashed and signed</summary>
        <p className="mono-small" data-field="canonical_body">
          {s.canonical_body}
        </p>
      </details>
      <p className="meta-row">
        <span className="mono-small">version {body.version}</span>
        <Link to={stableAssetHref(body.stable_asset_id)}>The stable asset</Link>
      </p>
    </section>
  );
}
