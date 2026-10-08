/**
 * ONE STABLE ASSET: its definition, status history and peg-monitor alerts,
 * and the account's two commands on it — issue (mint) and redeem (burn).
 *
 * Both commands act only on an account the session owns. The quantity is
 * typed as a decimal of the stable asset and sent as base units by moving
 * digits; the backend computes the payment or payout from the reserve's
 * verified price and the peg and decides everything else (admission against
 * the cap and the reserve, Risk Kernel V2, the reservation, the chain). Its
 * answer is shown as the request's status: a submission whose outcome is
 * unknown is shown as unknown, with the reconciliation that will settle it.
 */
import { useId, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useIssueStable,
  useRedeemStable,
  useStableAsset,
  type IssueRequest,
  type RedeemRequest,
  type StableAsset,
  type StableAssetDetail,
  type StableIssuanceResult,
  type StableRedemptionResult,
} from "../../../api/advanced/stable.ts";
import { useAssets } from "../../../api/queries.ts";
import { LinkButton } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { BaseUnits, Bps, Qty } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { formatMillis } from "../../../lib/advanced.ts";
import { parseQuantityInput } from "../../../lib/money.ts";
import {
  ALERT_KIND_MEANING,
  ASSET_STATUS_MEANING,
  REDEMPTION_MODE_MEANING,
  REQUEST_MODE_MEANING,
  STABLE_REQUEST_MODES,
  alertTone,
  assetStatusTone,
  reserveKindLabel,
  reserveKindNote,
  type StableRequestMode,
} from "../../../lib/stable.ts";
import { formatInstant } from "../../../lib/time.ts";
import { useAdvancedAccount } from "../area.ts";
import { ScrollTable } from "../shared.tsx";
import { ActorLine, CommandFlow, PriceLine, ReplayNote, StableNav, symbolOf, type Prepared } from "./parts.tsx";
import { IssuanceFields, RedemptionFields } from "./requests.tsx";
import { reserveHref, stableHref } from "./paths.ts";

export function StableAssetPage(): ReactNode {
  const { stableAssetId } = useParams();
  const detail = useStableAsset(stableAssetId);
  return (
    <Page
      title={detail.isSuccess ? detail.data.asset.symbol : "Stable asset"}
      lead="One sandbox stable asset: its definition, status history and peg-monitor alerts, and issuing or redeeming it on your own account."
      actions={<LinkButton to={stableHref()}>All stable assets</LinkButton>}
    >
      <SimulatedNotice />
      <StableNav />
      <AsyncPanel query={detail} loadingLabel="Loading the stable asset…">
        {(data: StableAssetDetail) => <AssetBody detail={data} />}
      </AsyncPanel>
    </Page>
  );
}

function AssetBody(props: { readonly detail: StableAssetDetail }): ReactNode {
  const { asset } = props.detail;
  const registry = useAssets();
  const quote = symbolOf(registry.data, asset.peg.quote);
  return (
    <>
      <Panel title="Definition" description="As the stable asset controller registered it. Nothing here is a public brand or a real-money token.">
        <FieldGrid columns={3}>
          <Field label="Status" note={ASSET_STATUS_MEANING[asset.status]}>
            <Pill tone={assetStatusTone(asset.status)}>{asset.status}</Pill>
          </Field>
          <Field label="Target peg" note="The exact price of one whole unit, quoted in the sandbox dollar.">
            <PriceLine price={asset.peg} {...(quote === undefined ? {} : { quote })} />
          </Field>
          <Field label="Issuance cap" note="Outstanding supply never exceeds it.">
            <Qty value={asset.issuance_cap} decimals={asset.decimals} symbol={asset.symbol} />
            <span className="cell-note">
              <BaseUnits value={asset.issuance_cap} />
            </span>
          </Field>
          <Field label="Redemption mode" note={REDEMPTION_MODE_MEANING[asset.redemption_mode]}>
            <code className="mono-small">{asset.redemption_mode}</code>
          </Field>
          <Field label="Peg deviation threshold" note="A market price further from the peg than this trips the circuit breaker.">
            <Bps value={asset.peg_deviation_threshold_bps} />
          </Field>
          <Field label="Oracle source" note="The only price source the monitor and the reserve verification accept.">
            <code className="mono-small">{asset.oracle_source}</code>
            <span className="cell-note">prices older than {formatMillis(asset.max_price_age_ms)} count as stale</span>
          </Field>
          <Field label="Simulated chain and mint">
            <code className="mono-small">{asset.chain}</code>
            <Identifier value={asset.mint} label="mint" />
          </Field>
          <Field label="Mint authority" note="Only the platform issuer signer holds it; it signs only a mint its inspector proved matches the plan.">
            <code className="mono-small">{asset.mint_authority_mode}</code>
            <Identifier value={asset.mint_authority} />
          </Field>
          <Field label="Burn authority" note="The holder's custodied key signs a burn.">
            <code className="mono-small">{asset.burn_authority_mode}</code>
          </Field>
          <Field label="Reserve snapshots" note="Signed, hash-linked records of the books and the chain. Not an attestation.">
            {asset.attestable ? (
              <Link to={reserveHref(asset.id)}>Published: see the reserve and its snapshots</Link>
            ) : (
              <span className="absent">this asset does not publish snapshots</span>
            )}
          </Field>
          <Field label="Issuer facility">
            <Identifier value={asset.facility_id} />
          </Field>
          <Field label="Registered">
            {formatInstant(asset.created_at)}
            <span className="cell-note">
              by <ActorLine actor={asset.created_by} />
            </span>
          </Field>
        </FieldGrid>
      </Panel>
      <Panel title="Reserve policy" description="The eligible reserve assets and the factor each counts at.">
        <ScrollTable caption="Reserve policy" headers={["Asset", "Kind", "Eligibility factor", "Pays for an issuance"]}>
          {asset.reserves.map((reserve) => (
            <tr key={reserve.asset_id}>
              <th scope="row">
                {reserve.symbol}
                <span className="cell-note mono-small">{reserve.asset_id}</span>
              </th>
              <td>
                {reserveKindLabel(reserve.kind)}
                <span className="cell-note">{reserveKindNote(reserve.kind)}</span>
              </td>
              <td>
                <Bps value={reserve.factor_bps} />
              </td>
              <td>{reserve.accepted_for_mint ? "yes" : "no"}</td>
            </tr>
          ))}
        </ScrollTable>
        <p className="meta-row">
          <Link to={reserveHref(asset.id)}>The reserve ledger, its coverage and the snapshot chain</Link>
        </p>
      </Panel>
      <IssuePanel asset={asset} />
      <RedeemPanel asset={asset} />
      <Panel title="Status history" description="Every status change, oldest first, with who made it and why. The circuit breaker pauses; only an operator resumes.">
        {props.detail.transitions.length === 0 ? (
          <p className="absent">The backend reports no status change.</p>
        ) : (
          <ScrollTable caption="Status changes" headers={["From", "To", "By", "Reason", "When"]}>
            {props.detail.transitions.map((t, index) => (
              <tr key={`${t.occurred_at}-${String(index)}`}>
                <th scope="row">
                  <code className="mono-small">{t.from_status}</code>
                </th>
                <td>
                  <Pill tone={assetStatusTone(t.to_status)}>{t.to_status}</Pill>
                </td>
                <td>
                  <ActorLine actor={t.actor} />
                </td>
                <td className="cell-prose">{t.reason}</td>
                <td>{formatInstant(t.occurred_at)}</td>
              </tr>
            ))}
          </ScrollTable>
        )}
      </Panel>
      <Panel title="Peg-monitor alerts" description="The newest findings first, each as the monitor recorded it.">
        {props.detail.alerts.length === 0 ? (
          <p className="absent">The monitor has recorded no alert for this asset.</p>
        ) : (
          <ScrollTable caption="Peg-monitor alerts" headers={["Finding", "Severity", "Paused the asset", "Observed", "When"]}>
            {props.detail.alerts.map((alert) => (
              <tr key={alert.id}>
                <th scope="row">
                  <code className="mono-small">{alert.kind}</code>
                  <span className="cell-note">{ALERT_KIND_MEANING[alert.kind]}</span>
                </th>
                <td>
                  <Pill tone={alertTone(alert.severity)}>{alert.severity}</Pill>
                </td>
                <td>{alert.breaker_tripped ? "yes" : "no"}</td>
                <td>
                  <code className="mono-small">{JSON.stringify(alert.observed)}</code>
                </td>
                <td>{formatInstant(alert.observed_at)}</td>
              </tr>
            ))}
          </ScrollTable>
        )}
      </Panel>
    </>
  );
}

/* --------------------------------------------------------------------------
 * Commands on the session's own account
 * ------------------------------------------------------------------------ */

function OwnAccountNote(): ReactNode {
  const account = useAdvancedAccount();
  if (account.ownAccountId === undefined || !account.inspecting) return null;
  return (
    <p className="note">
      This form acts on your own account <code className="mono-small">{account.ownAccountId}</code>, never on the account
      being inspected.
    </p>
  );
}

function ModeSelect(props: { readonly value: StableRequestMode; readonly onChange: (mode: StableRequestMode) => void }): ReactNode {
  const id = useId();
  return (
    <div className="form-row">
      <label htmlFor={id}>Mode</label>
      <select
        id={id}
        className="input"
        value={props.value}
        onChange={(event) => {
          const chosen = STABLE_REQUEST_MODES.find((mode) => mode === event.target.value);
          if (chosen !== undefined) props.onChange(chosen);
        }}
      >
        {STABLE_REQUEST_MODES.map((mode) => (
          <option key={mode} value={mode}>
            {mode}
          </option>
        ))}
      </select>
      <p className="field-note">{REQUEST_MODE_MEANING[props.value]}</p>
    </div>
  );
}

function UnitsInput(props: {
  readonly label: string;
  readonly asset: StableAsset;
  readonly raw: string;
  readonly onChange: (raw: string) => void;
}): ReactNode {
  const id = useId();
  const helpId = useId();
  const parsed = parseQuantityInput(props.raw, props.asset.decimals);
  return (
    <div className="form-row">
      <label htmlFor={id}>{props.label}</label>
      <input
        id={id}
        className="input"
        inputMode="decimal"
        autoComplete="off"
        value={props.raw}
        aria-describedby={helpId}
        {...(parsed.ok || props.raw === "" ? {} : { "aria-invalid": true })}
        onChange={(event) => {
          props.onChange(event.target.value);
        }}
      />
      <p id={helpId} className="field-note">
        {props.raw === "" ? (
          `Whole units of ${props.asset.symbol}, with up to ${String(props.asset.decimals)} decimal places. The amount is sent exactly, as base units.`
        ) : parsed.ok ? (
          <>
            Will be sent as <BaseUnits value={parsed.value} />.
          </>
        ) : (
          parsed.error
        )}
      </p>
    </div>
  );
}

function IssuePanel(props: { readonly asset: StableAsset }): ReactNode {
  const { asset } = props;
  const account = useAdvancedAccount();
  const own = account.ownAccountId;
  const command = useIssueStable();
  const payments = asset.reserves.filter((reserve) => reserve.accepted_for_mint);
  const [raw, setRaw] = useState("");
  const [mode, setMode] = useState<StableRequestMode>("SANDBOX");
  const [payment, setPayment] = useState<string>(payments[0]?.asset_id ?? "");
  const paymentId = useId();
  const parsed = parseQuantityInput(raw, asset.decimals);
  const chosen = payments.find((reserve) => reserve.asset_id === payment);

  let prepared: Prepared<IssueRequest>;
  if (own === undefined) {
    prepared = { ok: false, reason: "This session owns no account; an issuance is made only to an account the session owns." };
  } else if (asset.status !== "ACTIVE") {
    prepared = { ok: false, reason: `The service issues only while a stable asset is ACTIVE; this one is ${asset.status}.` };
  } else if (chosen === undefined) {
    prepared = { ok: false, reason: "Choose the reserve asset the issuance is paid in." };
  } else if (!parsed.ok) {
    prepared = { ok: false, reason: raw === "" ? "Enter how many units to issue." : parsed.error };
  } else {
    prepared = {
      ok: true,
      request: { stableAssetId: asset.id, accountId: own, quantity: parsed.value, reserveAssetId: chosen.asset_id, mode },
    };
  }

  return (
    <Panel
      title="Issue"
      description="Mint stable units into your account's custodied wallet, paid in a dollar-equivalent reserve asset at the peg. The units post only once the mint is final on the simulated chain."
    >
      <OwnAccountNote />
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
        }}
      >
        <UnitsInput label="Units to issue" asset={asset} raw={raw} onChange={setRaw} />
        <div className="form-row">
          <label htmlFor={paymentId}>Paid in</label>
          <select
            id={paymentId}
            className="input"
            value={payment}
            onChange={(event) => {
              setPayment(event.target.value);
            }}
          >
            {payments.map((reserve) => (
              <option key={reserve.asset_id} value={reserve.asset_id}>
                {reserve.symbol}
              </option>
            ))}
          </select>
          <p className="field-note">
            The backend computes the payment from this reserve asset's verified price, its eligibility factor and the
            peg, rounded up so the issuance is never under-backed. The amount taken is shown with the result.
          </p>
        </div>
        <ModeSelect value={mode} onChange={setMode} />
      </form>
      <CommandFlow
        label="issue"
        prepared={prepared}
        command={command}
        describe={(request: IssueRequest) => (
          <p>
            Issue <Qty value={request.quantity} decimals={asset.decimals} symbol={asset.symbol} /> (
            <BaseUnits value={request.quantity} />) to account <code className="mono-small">{request.accountId}</code>,
            paid in {payments.find((reserve) => reserve.asset_id === request.reserveAssetId)?.symbol ?? request.reserveAssetId},
            in {request.mode} mode. Simulated value only.
          </p>
        )}
        renderResult={(result: StableIssuanceResult) => (
          <div role="status">
            <ReplayNote replay={result.replay} />
            {!result.settled && (
              <p className="note">
                The request is recorded and not final yet: its status below says where it stands.
              </p>
            )}
            <IssuanceFields issuance={result.issuance} />
          </div>
        )}
        refusedNote="The backend did not carry this out: no payment was taken and no unit was issued. A refused request is kept on record as a rejected economic intent."
      />
    </Panel>
  );
}

function RedeemPanel(props: { readonly asset: StableAsset }): ReactNode {
  const { asset } = props;
  const account = useAdvancedAccount();
  const own = account.ownAccountId;
  const command = useRedeemStable();
  const [raw, setRaw] = useState("");
  const [mode, setMode] = useState<StableRequestMode>("SANDBOX");
  const parsed = parseQuantityInput(raw, asset.decimals);

  let prepared: Prepared<RedeemRequest>;
  if (own === undefined) {
    prepared = { ok: false, reason: "This session owns no account; a redemption is made only from an account the session owns." };
  } else if (asset.status === "PAUSED") {
    prepared = { ok: false, reason: "The service redeems nothing while a stable asset is PAUSED." };
  } else if (!parsed.ok) {
    prepared = { ok: false, reason: raw === "" ? "Enter how many units to redeem." : parsed.error };
  } else {
    prepared = { ok: true, request: { stableAssetId: asset.id, accountId: own, quantity: parsed.value, mode } };
  }

  return (
    <Panel
      title="Redeem"
      description="Burn stable units from your account's custodied wallet and receive the policy's dollar-equivalent reserve asset at the peg, rounded down. The payout posts only once the burn is final on the simulated chain."
    >
      <OwnAccountNote />
      <form
        className="form"
        onSubmit={(event) => {
          event.preventDefault();
        }}
      >
        <UnitsInput label="Units to redeem" asset={asset} raw={raw} onChange={setRaw} />
        <ModeSelect value={mode} onChange={setMode} />
      </form>
      <CommandFlow
        label="redeem"
        prepared={prepared}
        command={command}
        describe={(request: RedeemRequest) => (
          <p>
            Redeem <Qty value={request.quantity} decimals={asset.decimals} symbol={asset.symbol} /> (
            <BaseUnits value={request.quantity} />) from account <code className="mono-small">{request.accountId}</code>, in{" "}
            {request.mode} mode. The backend computes the payout at the peg. Simulated value only.
          </p>
        )}
        renderResult={(result: StableRedemptionResult) => (
          <div role="status">
            <ReplayNote replay={result.replay} />
            {!result.settled && (
              <p className="note">
                The request is recorded and not final yet: its status below says where it stands.
              </p>
            )}
            <RedemptionFields redemption={result.redemption} />
          </div>
        )}
        refusedNote="The backend did not carry this out: no unit was burned and nothing was paid out. A refused request is kept on record as a rejected economic intent."
      />
    </Panel>
  );
}
