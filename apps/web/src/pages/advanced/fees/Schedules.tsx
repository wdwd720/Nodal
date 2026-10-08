/**
 * FEES — the published fee schedules, public to any signed-in customer.
 *
 * Every fee and spread a customer can be charged comes from a schedule
 * published under a key, in immutable consecutive versions, each effective
 * from an instant that is never in the past. This section lists every key
 * with the version in force, shows a key's whole history component by
 * component, and prices an order exactly under the schedule in force: the
 * calculator asks the fee engine, which returns its verified disclosure, and
 * the page shows that disclosure line by line — it never computes a fee.
 *
 * Operators additionally publish the next version of a key, with a recent
 * strong sign-in.
 */
import { useState, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";

import {
  useFeeScheduleHistory,
  useFeeSchedules,
  usePublishSchedule,
  useQuotePreview,
  type FeePublishRequest,
  type FeeQuoteInput,
  type FeeQuotePreview,
  type FeeScheduleHistory,
  type FeeScheduleSummary,
  type FeeScheduleVersion,
  type TreasuryAssetRef,
} from "../../../api/advanced/fees.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { useAssets } from "../../../api/queries.ts";
import { Button } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { Bps, Qty, Usd } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import {
  CUSTOMER_FAVOURABLE_ROUNDING,
  FEE_SIDES,
  ROUNDING_FIELDS,
  ROUNDING_MODES,
  parseScheduleKey,
  parseTierLines,
  parseUsdInput,
  parseUtcInstant,
  scheduleQuery,
  type FeeSide,
  type RoundingField,
} from "../../../lib/fees.ts";
import { parseBaseUnits, parseDecimalPrice, parseText, parseUuid, parseWholeNumber } from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { CursorPager, ScrollTable } from "../shared.tsx";
import { Choice, CommandFlow, CommandForm, TextArea, TextInput, Toggle, prepare } from "../treasury/command.tsx";
import { ChargesView, DisclosureView, FeesNav, feesHref, useIsOperator } from "./parts.tsx";

export function FeeSchedulesPage(): ReactNode {
  const operator = useIsOperator();
  return (
    <Page
      title="Fees"
      lead="Every fee and spread is published in a versioned schedule that any customer can read, and every charge can be traced back to the disclosure and the schedule version that priced it."
    >
      <SimulatedNotice />
      <FeesNav />
      <SchedulesPanel />
      {operator && <PublishPanel />}
    </Page>
  );
}

function scheduleHref(key: string): string {
  return `${feesHref("schedule")}${scheduleQuery(key)}`;
}

function SchedulesPanel(): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const schedules = useFeeSchedules(trail.current);
  return (
    <Panel
      title="Published schedules"
      description="Every schedule key with the version in force now. A version never changes once published; a new one takes over at its effective instant."
    >
      <AsyncPanel
        query={schedules}
        loadingLabel="Loading the published schedules…"
        empty={{
          isEmpty: (page: CursorPage<FeeScheduleSummary>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No schedules published",
          body: "The backend reports no published fee schedule.",
        }}
      >
        {(page: CursorPage<FeeScheduleSummary>) => (
          <>
            <ScrollTable caption="Published schedules" headers={["Schedule", "In force", "Spread", "Percentage fee", "Effective from", "Versions"]}>
              {page.items.map((summary) => (
                <tr key={summary.key} data-schedule={summary.key}>
                  <th scope="row">
                    <Link to={scheduleHref(summary.key)}>
                      <code className="mono-small">{summary.key}</code>
                    </Link>
                  </th>
                  {summary.effective === undefined ? (
                    <td colSpan={4} className="absent">
                      none in force yet: every version takes effect later
                    </td>
                  ) : (
                    <>
                      <td>
                        v{String(summary.effective.version)} <span className="cell-note">{summary.effective.schedule.label}</span>
                      </td>
                      <td>
                        <Bps value={summary.effective.schedule.spread_bps} />
                      </td>
                      <td>
                        <Bps value={summary.effective.schedule.percentage_fee_bps} />
                      </td>
                      <td>{formatInstant(summary.effective.effective_from)}</td>
                    </>
                  )}
                  <td>
                    {String(summary.version_count)}
                    <span className="cell-note">latest v{String(summary.latest_version)}</span>
                  </td>
                </tr>
              ))}
            </ScrollTable>
            <CursorPager
              noun="schedules"
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
 * One schedule key: its history and the calculator
 * ------------------------------------------------------------------------ */

export function FeeSchedulePage(): ReactNode {
  const [params] = useSearchParams();
  const key = params.get("key") ?? "";
  const history = useFeeScheduleHistory(key === "" ? undefined : key);
  return (
    <Page title="Fee schedule" lead="One schedule key: every version it has had, component by component, and an exact quote under the version in force.">
      <SimulatedNotice />
      <FeesNav />
      {key === "" ? (
        <p className="note" role="status">
          No schedule key was given. Choose one from the published schedules.
        </p>
      ) : (
        <AsyncPanel query={history} loadingLabel="Loading the schedule's history…">
          {(data: FeeScheduleHistory) => <ScheduleHistory history={data} />}
        </AsyncPanel>
      )}
    </Page>
  );
}

function ScheduleHistory(props: { readonly history: FeeScheduleHistory }): ReactNode {
  const { history } = props;
  const inForce = history.versions.find((v) => v.in_force);
  return (
    <>
      <Panel
        title={history.key}
        description={
          history.effective_version === undefined
            ? "No version of this schedule is in force yet."
            : `Version ${String(history.effective_version)} prices orders now.`
        }
      >
        {[...history.versions].reverse().map((version) => (
          <VersionView key={version.id} version={version} />
        ))}
      </Panel>
      <CalculatorPanel scheduleKey={history.key} quoteAsset={inForce?.quote_asset} />
    </>
  );
}

function VersionView(props: { readonly version: FeeScheduleVersion }): ReactNode {
  const { version } = props;
  const s = version.schedule;
  const quote = version.quote_asset;
  const amount = (value: string): ReactNode => <Qty value={value} decimals={quote.decimals} symbol={quote.symbol} />;
  return (
    <section className="callout" aria-label={`Version ${String(version.version)}`} data-version={String(version.version)}>
      <h3>
        Version {String(version.version)} — {s.label}{" "}
        {version.in_force ? <Pill tone="good">in force</Pill> : <Pill tone="neutral">not in force now</Pill>}
      </h3>
      <p className="meta-row">
        <span className="mono-small">effective {formatInstant(version.effective_from)}</span>
        <span className="mono-small">published {formatInstant(version.published_at)}</span>
        <span className="mono-small">
          by {version.actor_type} {version.actor_id}
        </span>
        <span className="mono-small">hash {version.schedule_hash}</span>
      </p>
      <p className="field-note">Reason: {version.reason}</p>
      <FieldGrid columns={3}>
        <Field label="Quote asset" note={`${String(s.quote_decimals)} decimals.`}>
          {quote.symbol} <span className="mono-small">{quote.asset_id}</span>
        </Field>
        <Field label="Spread" note="Added to the reference price of a buy, taken from a sale.">
          <Bps value={s.spread_bps} />
        </Field>
        <Field label="Fixed fee" note="Once per order.">
          {amount(s.fixed_fee)}
        </Field>
        <Field label="Percentage fee">
          <Bps value={s.percentage_fee_bps} />
        </Field>
        <Field label="Venue pass-through" note="Recovers the venue's fee; not platform revenue.">
          <Bps value={s.venue_pass_through_bps} />
        </Field>
        <Field label="Strategy fee" note="Strategy-originated orders only.">
          <Bps value={s.strategy_fee_bps} />
        </Field>
        <Field label="Subscription discount" note="Of the percentage fee only.">
          <Bps value={s.subscription_discount_bps} />
        </Field>
        <Field label="Network fee estimate" note={s.network_fee_asset_id === undefined ? "In the quote asset." : `In asset ${s.network_fee_asset_id}.`}>
          <code className="mono-small">{s.network_fee_amount} base units</code>
        </Field>
        <Field label="Slippage reserve" note="Held against adverse execution; never charged.">
          <Bps value={s.slippage_reserve_bps} />
        </Field>
        <Field label="Minimum fee" note="Zero means no minimum.">
          {amount(s.min_fee)}
        </Field>
        <Field label="Fee caps" note="Zero means no cap. A cap always wins over the minimum.">
          <Bps value={s.max_total_fee_bps} />
          <span className="cell-note">{amount(s.max_total_fee)}</span>
        </Field>
      </FieldGrid>
      {s.tiers.length > 0 && (
        <ScrollTable caption={`Volume tiers of version ${String(version.version)}`} headers={["From trailing volume", "Percentage fee", "Spread"]}>
          {s.tiers.map((tier) => (
            <tr key={tier.min_trailing_volume_usd}>
              <th scope="row">
                <Usd value={tier.min_trailing_volume_usd} />
              </th>
              <td>{tier.overrides_percentage_fee ? <Bps value={tier.percentage_fee_bps} /> : <span className="absent">base rate</span>}</td>
              <td>{tier.overrides_spread ? <Bps value={tier.spread_bps} /> : <span className="absent">base rate</span>}</td>
            </tr>
          ))}
        </ScrollTable>
      )}
      <p className="field-note">
        Rounding: {ROUNDING_FIELDS.map((field) => `${field.replace(/_/g, " ")} ${s.rounding[field]}`).join("; ")}.{" "}
        {s.rounding.allow_platform_favourable ? "Platform-favourable rounding is explicitly allowed." : "No platform-favourable rounding is allowed."}
      </p>
    </section>
  );
}

function CalculatorPanel(props: { readonly scheduleKey: string; readonly quoteAsset: TreasuryAssetRef | undefined }): ReactNode {
  const assets = useAssets();
  const [side, setSide] = useState<FeeSide>("BUY");
  const [quantity, setQuantity] = useState("");
  const [decimals, setDecimals] = useState("6");
  const [price, setPrice] = useState("");
  const [volume, setVolume] = useState("");
  const [subscription, setSubscription] = useState(false);
  const [strategy, setStrategy] = useState(false);
  const [at, setAt] = useState("");
  const [asked, setAsked] = useState<FeeQuoteInput | undefined>(undefined);
  const preview = useQuotePreview(asked);

  const q = parseBaseUnits(quantity, "The quantity");
  const d = parseWholeNumber(decimals, 0, 38, "The base asset's decimals");
  const p = parseDecimalPrice(price, "The reference price");
  const v = volume.trim() === "" ? undefined : parseUsdInput(volume, "The trailing volume");
  const a = at.trim() === "" ? undefined : parseUtcInstant(at, "The instant");
  const problems = [
    q.ok ? undefined : q.error,
    d.ok ? undefined : d.error,
    p.ok ? undefined : p.error,
    v === undefined || v.ok ? undefined : v.error,
    a === undefined || a.ok ? undefined : a.error,
  ];
  const problem = problems.find((x) => x !== undefined);
  const input: FeeQuoteInput | undefined =
    problem === undefined && q.ok && d.ok && p.ok
      ? {
          schedule_key: props.scheduleKey,
          side,
          base_quantity: q.value,
          base_decimals: d.value,
          reference_price: p.value,
          has_subscription: subscription,
          strategy_originated: strategy,
          ...(v !== undefined && v.ok ? { trailing_volume_usd: v.value } : {}),
          ...(a !== undefined && a.ok ? { at: a.value } : {}),
        }
      : undefined;

  return (
    <Panel
      title="Exact quote calculator"
      description="Asks the fee engine to price an order under this schedule's version in force and shows the verified disclosure it would make. Nothing is recorded, reserved or charged, and the page computes nothing: every figure below is the engine's."
    >
      <CommandForm label="Price an order">
        <Choice id="fees-calc-side" label="Side" value={side} options={FEE_SIDES} onChange={setSide} />
        <TextInput id="fees-calc-quantity" label="Quantity, in base units of the asset priced" value={quantity} onChange={setQuantity} inputMode="numeric" />
        <TextInput id="fees-calc-decimals" label="Decimals of the asset priced" value={decimals} onChange={setDecimals} inputMode="numeric" />
        <TextInput
          id="fees-calc-price"
          label={`Reference price of one whole unit${props.quoteAsset === undefined ? "" : `, in ${props.quoteAsset.symbol}`}`}
          value={price}
          onChange={setPrice}
          inputMode="decimal"
        />
        <TextInput
          id="fees-calc-volume"
          label="Trailing volume in USD (optional)"
          value={volume}
          onChange={setVolume}
          inputMode="decimal"
          note="Decides the volume tier, when the schedule has tiers."
        />
        <Toggle id="fees-calc-subscription" label="Has a subscription" checked={subscription} onChange={setSubscription} />
        <Toggle id="fees-calc-strategy" label="Strategy-originated order" checked={strategy} onChange={setStrategy} />
        <TextInput id="fees-calc-at" label="Price at an instant in UTC (optional)" value={at} onChange={setAt} note="Empty means now." />
        <div className="form-actions">
          {input === undefined ? (
            <Button disabledReason={problem ?? "Fill in the order to price."}>Price it under the schedule</Button>
          ) : (
            <Button
              variant="primary"
              onClick={() => {
                setAsked(input);
              }}
            >
              Price it under the schedule
            </Button>
          )}
        </div>
      </CommandForm>
      {asked !== undefined && (
        <AsyncPanel query={preview} loadingLabel="Asking the fee engine…">
          {(result: FeeQuotePreview) => (
            <div role="status" aria-label="The fee engine's quote">
              <p className="meta-row">
                <span className="mono-small">
                  priced under {result.schedule.key} v{String(result.schedule.version)} ({result.schedule.label})
                </span>
                <span className="mono-small">disclosure {result.disclosure_hash}</span>
                <Pill tone="info">{result.recorded ? "recorded" : "not recorded: a preview"}</Pill>
              </p>
              <DisclosureView disclosure={result.disclosure} quote={props.quoteAsset} />
              <h3>Charged, component by component</h3>
              <ChargesView
                charges={result.charges}
                unit={{ decimals: result.disclosure.input.quote_decimals, symbol: props.quoteAsset?.symbol }}
                assets={assets.data ?? []}
              />
            </div>
          )}
        </AsyncPanel>
      )}
    </Panel>
  );
}

/* --------------------------------------------------------------------------
 * The operator: publishing a version
 * ------------------------------------------------------------------------ */

function BpsInput(props: { readonly id: string; readonly label: string; readonly value: string; readonly onChange: (v: string) => void }): ReactNode {
  return <TextInput id={props.id} label={`${props.label}, in basis points`} value={props.value} onChange={props.onChange} inputMode="numeric" />;
}

function PublishPanel(): ReactNode {
  const publish = usePublishSchedule();
  const [key, setKey] = useState("");
  const [label, setLabel] = useState("");
  const [quoteAsset, setQuoteAsset] = useState("");
  const [quoteDecimals, setQuoteDecimals] = useState("6");
  const [spread, setSpread] = useState("0");
  const [fixed, setFixed] = useState("0");
  const [pct, setPct] = useState("0");
  const [venue, setVenue] = useState("0");
  const [networkAsset, setNetworkAsset] = useState("");
  const [networkAmount, setNetworkAmount] = useState("0");
  const [reserve, setReserve] = useState("0");
  const [discount, setDiscount] = useState("0");
  const [strategyFee, setStrategyFee] = useState("0");
  const [capBps, setCapBps] = useState("0");
  const [capAbs, setCapAbs] = useState("0");
  const [minFee, setMinFee] = useState("0");
  const [rounding, setRounding] = useState<Record<RoundingField, string>>({ ...CUSTOMER_FAVOURABLE_ROUNDING });
  const [platformFavourable, setPlatformFavourable] = useState(false);
  const [tiers, setTiers] = useState("");
  const [effective, setEffective] = useState("");
  const [reason, setReason] = useState("");

  const k = parseScheduleKey(key);
  const l = parseText(label, "The label", 100);
  const qa = parseUuid(quoteAsset, "The quote asset id");
  const qd = parseWholeNumber(quoteDecimals, 0, 38, "The quote decimals");
  const bps = (raw: string, what: string, max = 10000): ReturnType<typeof parseWholeNumber> => parseWholeNumber(raw, 0, max, what);
  const sp = bps(spread, "The spread", 9999);
  const fx = parseBaseUnits(fixed, "The fixed fee", true);
  const pc = bps(pct, "The percentage fee");
  const ve = bps(venue, "The venue pass-through");
  const na = networkAsset.trim() === "" ? undefined : parseUuid(networkAsset, "The network fee asset id");
  const nm = parseBaseUnits(networkAmount, "The network fee estimate", true);
  const rs = bps(reserve, "The slippage reserve");
  const ds = bps(discount, "The subscription discount");
  const st = bps(strategyFee, "The strategy fee");
  const cb = bps(capBps, "The fee cap");
  const ca = parseBaseUnits(capAbs, "The absolute fee cap", true);
  const mf = parseBaseUnits(minFee, "The minimum fee", true);
  const tr = parseTierLines(tiers);
  const ef = effective.trim() === "" ? undefined : parseUtcInstant(effective, "The effective instant");
  const r = parseText(reason, "The reason", 500);
  const checks = [k, l, qa, qd, sp, fx, pc, ve, nm, rs, ds, st, cb, ca, mf, tr, r].map((c) => (c.ok ? undefined : c.error));
  const prepared = prepare<FeePublishRequest>(
    [...checks, na === undefined || na.ok ? undefined : na.error, ef === undefined || ef.ok ? undefined : ef.error],
    () => ({
      key: k.ok ? k.value : "",
      reason: r.ok ? r.value : "",
      ...(ef !== undefined && ef.ok ? { effective_from: ef.value } : {}),
      schedule: {
        label: l.ok ? l.value : "",
        quote_asset_id: qa.ok ? qa.value : "",
        quote_decimals: qd.ok ? qd.value : 0,
        spread_bps: sp.ok ? sp.value : 0,
        fixed_fee: fx.ok ? fx.value : "0",
        percentage_fee_bps: pc.ok ? pc.value : 0,
        venue_pass_through_bps: ve.ok ? ve.value : 0,
        network_fee_amount: nm.ok ? nm.value : "0",
        ...(na !== undefined && na.ok ? { network_fee_asset_id: na.value } : {}),
        slippage_reserve_bps: rs.ok ? rs.value : 0,
        subscription_discount_bps: ds.ok ? ds.value : 0,
        tiers: tr.ok ? tr.value : [],
        strategy_fee_bps: st.ok ? st.value : 0,
        max_total_fee_bps: cb.ok ? cb.value : 0,
        max_total_fee: ca.ok ? ca.value : "0",
        min_fee: mf.ok ? mf.value : "0",
        rounding: {
          buy_notional: rounding.buy_notional,
          sell_notional: rounding.sell_notional,
          spread: rounding.spread,
          percentage_fee: rounding.percentage_fee,
          venue_pass_through: rounding.venue_pass_through,
          strategy_fee: rounding.strategy_fee,
          fee_cap: rounding.fee_cap,
          slippage_reserve: rounding.slippage_reserve,
          allow_platform_favourable: platformFavourable,
        },
      },
    }),
  );

  return (
    <Panel
      title="Publish a schedule version"
      description="Records the next immutable version of a schedule key, effective now or later and never in the past. Publishing the same label with the same content again returns the stored version. It changes what customers are charged, so the backend asks for a recent strong sign-in."
    >
      <CommandForm label="Publish a schedule version">
        <TextInput id="fees-publish-key" label="Schedule key" value={key} onChange={setKey} />
        <TextInput id="fees-publish-label" label="Version label" value={label} onChange={setLabel} note="Recorded on every quote the version prices." />
        <TextInput id="fees-publish-quote-asset" label="Quote asset id" value={quoteAsset} onChange={setQuoteAsset} />
        <TextInput id="fees-publish-quote-decimals" label="Quote asset decimals" value={quoteDecimals} onChange={setQuoteDecimals} inputMode="numeric" />
        <BpsInput id="fees-publish-spread" label="Spread" value={spread} onChange={setSpread} />
        <TextInput id="fees-publish-fixed" label="Fixed fee, in base units of the quote asset" value={fixed} onChange={setFixed} inputMode="numeric" />
        <BpsInput id="fees-publish-pct" label="Percentage fee" value={pct} onChange={setPct} />
        <BpsInput id="fees-publish-venue" label="Venue pass-through" value={venue} onChange={setVenue} />
        <TextInput id="fees-publish-network-asset" label="Network fee asset id (optional)" value={networkAsset} onChange={setNetworkAsset} note="Empty means the quote asset." />
        <TextInput id="fees-publish-network-amount" label="Network fee estimate, in base units" value={networkAmount} onChange={setNetworkAmount} inputMode="numeric" />
        <BpsInput id="fees-publish-reserve" label="Slippage reserve" value={reserve} onChange={setReserve} />
        <BpsInput id="fees-publish-discount" label="Subscription discount" value={discount} onChange={setDiscount} />
        <BpsInput id="fees-publish-strategy" label="Strategy fee" value={strategyFee} onChange={setStrategyFee} />
        <BpsInput id="fees-publish-cap-bps" label="Fee cap" value={capBps} onChange={setCapBps} />
        <TextInput id="fees-publish-cap-abs" label="Absolute fee cap, in base units (zero for none)" value={capAbs} onChange={setCapAbs} inputMode="numeric" />
        <TextInput id="fees-publish-min" label="Minimum fee, in base units (zero for none)" value={minFee} onChange={setMinFee} inputMode="numeric" />
        {ROUNDING_FIELDS.map((field) => (
          <Choice
            key={field}
            id={`fees-publish-rounding-${field}`}
            label={`Rounding: ${field.replace(/_/g, " ")}`}
            value={rounding[field] as (typeof ROUNDING_MODES)[number]}
            options={ROUNDING_MODES}
            onChange={(mode) => {
              setRounding({ ...rounding, [field]: mode });
            }}
          />
        ))}
        <Toggle
          id="fees-publish-platform-favourable"
          label="Explicitly allow a rounding that can favour the platform"
          checked={platformFavourable}
          onChange={setPlatformFavourable}
          note="Recorded in the schedule's hash and in every disclosure it prices."
        />
        <TextArea
          id="fees-publish-tiers"
          label="Volume tiers (optional): per line, the trailing volume in USD, the percentage fee and the spread in basis points, '-' to keep the base rate"
          value={tiers}
          onChange={setTiers}
        />
        <TextInput id="fees-publish-effective" label="Effective from, in UTC (optional)" value={effective} onChange={setEffective} note="Empty means now; never in the past." />
        <TextInput id="fees-publish-reason" label="Reason" value={reason} onChange={setReason} />
        <CommandFlow<FeePublishRequest, FeeScheduleVersion>
          label="Publish the version"
          variant="danger"
          prepared={prepared}
          command={publish}
          refusedNote="No version was published."
          describe={(request) => (
            <FieldGrid columns={3}>
              <Field label="Key">
                <code className="mono-small">{request.key}</code>
              </Field>
              <Field label="Label">{request.schedule.label}</Field>
              <Field label="Effective from">{request.effective_from === undefined ? "now" : formatInstant(request.effective_from)}</Field>
              <Field label="Spread">
                <Bps value={request.schedule.spread_bps} />
              </Field>
              <Field label="Percentage fee">
                <Bps value={request.schedule.percentage_fee_bps} />
              </Field>
              <Field label="Fixed fee">
                <code className="mono-small">{request.schedule.fixed_fee} base units</code>
              </Field>
              <Field label="Tiers">{String(request.schedule.tiers.length)}</Field>
              <Field label="Reason">{request.reason}</Field>
            </FieldGrid>
          )}
          renderResult={(version) => (
            <p>
              Published <Link to={scheduleHref(version.key)}>{version.key}</Link> version {String(version.version)}, effective{" "}
              {formatInstant(version.effective_from)}.
            </p>
          )}
        />
      </CommandForm>
    </Panel>
  );
}
