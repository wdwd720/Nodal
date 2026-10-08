/**
 * Reference analytics of an option: a pricing model's price, Greeks and
 * implied volatility at the contract's latest mark, computed by the backend
 * from a volatility, a rate and a dividend yield the reader chooses.
 *
 * Every value is a reference value of the model, and the panel says so twice:
 * in the backend's own label, shown verbatim, and in this interface's words.
 * They are not a quote, not a settlement price and not advice, and nothing is
 * ever posted from them. Each one is the exact decimal string the backend
 * sent, with the inputs it was computed from beside it.
 */
import { useId, useState, type ReactNode } from "react";

import {
  useDerivativeAnalytics,
  type AnalyticsInputs,
  type DerivativeContract,
  type DerivativeOptionAnalytics,
} from "../../../api/advanced/derivatives.ts";
import { Button } from "../../../components/Button.tsx";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { AsOf, Disclosure, Field, FieldGrid } from "../../../components/Layout.tsx";
import { Bps, DecimalValue } from "../../../components/Money.tsx";
import { REFERENCE_VALUES_NOTE, parseBpsInput } from "../../../lib/derivatives.ts";
import { MarkFields, TextField } from "./parts.tsx";

/** The model's bounds, as the backend states them: volatility in (0, 1000%], rates within ±100%. */
const VOL_BPS = { min: 1, max: 100_000 } as const;
const RATE_BPS = { min: -10_000, max: 10_000 } as const;

export function AnalyticsPanel(props: { readonly contract: DerivativeContract }): ReactNode {
  const ids = { vol: useId(), rate: useId(), dividend: useId() };
  const [vol, setVol] = useState("8000");
  const [rate, setRate] = useState("0");
  const [dividend, setDividend] = useState("0");
  const [inputs, setInputs] = useState<AnalyticsInputs | undefined>(undefined);
  const analytics = useDerivativeAnalytics(props.contract.id, inputs);

  const volParse = parseBpsInput(vol, VOL_BPS.min, VOL_BPS.max);
  const rateParse = parseBpsInput(rate, RATE_BPS.min, RATE_BPS.max);
  const dividendParse = parseBpsInput(dividend, RATE_BPS.min, RATE_BPS.max);
  const ready = volParse.ok && rateParse.ok && dividendParse.ok;

  return (
    <>
      <Disclosure title="Reference values, not a quote">
        <p>{REFERENCE_VALUES_NOTE}</p>
      </Disclosure>
      <div className="form">
        <TextField
          id={ids.vol}
          label="Volatility (bps a year)"
          value={vol}
          problem={volParse.ok ? undefined : volParse.error}
          hint="8000 is 80% a year."
          onChange={setVol}
        />
        <TextField
          id={ids.rate}
          label="Rate (bps, continuously compounded)"
          value={rate}
          problem={rateParse.ok ? undefined : rateParse.error}
          onChange={setRate}
        />
        <TextField
          id={ids.dividend}
          label="Dividend yield (bps)"
          value={dividend}
          problem={dividendParse.ok ? undefined : dividendParse.error}
          onChange={setDividend}
        />
      </div>
      <div className="form-actions">
        {ready ? (
          <Button
            onClick={() => {
              setInputs({ volBps: volParse.value, rateBps: rateParse.value, dividendYieldBps: dividendParse.value });
            }}
          >
            Compute reference values
          </Button>
        ) : (
          <Button disabledReason="Every input is a whole number of basis points within the model's bounds.">
            Compute reference values
          </Button>
        )}
      </div>
      {inputs !== undefined && (
        <AsyncPanel query={analytics} loadingLabel="Computing reference values…">
          {(data: DerivativeOptionAnalytics) => <AnalyticsFigures data={data} />}
        </AsyncPanel>
      )}
    </>
  );
}

function AnalyticsFigures(props: { readonly data: DerivativeOptionAnalytics }): ReactNode {
  const { data } = props;
  return (
    <>
      <p className="note">
        The backend labels these values: <code className="mono-small">{data.label}</code>
      </p>
      <AsOf at={data.as_of} label="Computed as of" />
      <h3>Inputs the model used</h3>
      <FieldGrid columns={3}>
        <Field label="Model">{data.model}</Field>
        <Field label="Right and style">
          {data.right} · {data.style}
        </Field>
        <Field label="Regime" note="How the model treated this input set.">
          {data.regime}
        </Field>
        <Field label="Spot (the mark's index)">
          <DecimalValue value={data.spot} />
        </Field>
        <Field label="Strike">
          <DecimalValue value={data.strike} />
        </Field>
        <Field label="Time to expiry (years)">
          <DecimalValue value={data.time_years} />
        </Field>
        <Field label="Volatility">
          <DecimalValue value={data.vol} />
          <span className="cell-note">
            <Bps value={data.inputs.vol_bps} />
          </span>
        </Field>
        <Field label="Rate">
          <Bps value={data.inputs.rate_bps} />
        </Field>
        <Field label="Dividend yield">
          <Bps value={data.inputs.dividend_yield_bps} />
        </Field>
      </FieldGrid>
      <h3>The mark priced at</h3>
      <MarkFields mark={data.mark} />
      <h3>Reference values</h3>
      <FieldGrid columns={3}>
        <Field label="Model price" note="Per unit of the underlying. A reference value, not a price anyone will trade at.">
          <DecimalValue value={data.price} />
        </Field>
        <Field label="Intrinsic value" note="Exact, at the spot above.">
          <DecimalValue value={data.intrinsic} />
        </Field>
        <Field label="Time value" note="The mark less the intrinsic value, exact.">
          {data.time_value === undefined ? <span className="absent">no mark to compare</span> : <DecimalValue value={data.time_value} />}
        </Field>
        <Field label="Delta">
          <DecimalValue value={data.delta} />
        </Field>
        <Field label="Gamma">
          <DecimalValue value={data.gamma} />
        </Field>
        <Field label="Theta a year">
          <DecimalValue value={data.theta_per_year} />
        </Field>
        <Field label="Theta a day">
          <DecimalValue value={data.theta_per_day} />
        </Field>
        <Field label="Vega">
          <DecimalValue value={data.vega} />
        </Field>
        <Field label="Vega per volatility point">
          <DecimalValue value={data.vega_per_vol_point} />
        </Field>
        <Field label="Implied volatility" note="Solved from the mark, when a solution exists.">
          {data.implied_vol !== undefined ? (
            <DecimalValue value={data.implied_vol} />
          ) : (
            <span className="absent">
              not solved{data.implied_vol_reason === undefined ? "" : ` (${data.implied_vol_reason})`}
            </span>
          )}
        </Field>
      </FieldGrid>
    </>
  );
}
