/**
 * Rendering money.
 *
 * These components exist so no page ever calls a formatter directly and has to
 * remember what to do when the value is missing or malformed. A missing figure
 * renders as a stated absence; a malformed one renders as a stated fault. In
 * neither case does anything that looks like an amount appear on screen.
 */
import type { ReactNode } from "react";

import {
  MoneyFormatError,
  formatBps,
  formatBpsRaw,
  formatDecimalString,
  formatQuantity,
  formatUsd,
  usdSign,
} from "../lib/money.ts";

/** What to show when the backend simply did not send the field. */
export function Absent(props: { readonly what?: string }): ReactNode {
  return (
    <span className="absent" title="The backend did not return a value for this field.">
      {props.what ?? "not reported"}
    </span>
  );
}

function Malformed(props: { readonly detail: string }): ReactNode {
  return (
    <span className="malformed" role="status">
      unreadable value <span className="malformed-detail">({props.detail})</span>
    </span>
  );
}

function guard(render: () => ReactNode): ReactNode {
  try {
    return render();
  } catch (error) {
    if (error instanceof MoneyFormatError) {
      return <Malformed detail={error.message} />;
    }
    throw error;
  }
}

export function Usd(props: {
  readonly value: string | undefined;
  /** Prefix a gain with "+" and colour by sign. For P&L only. */
  readonly signed?: boolean;
  readonly absent?: string;
}): ReactNode {
  if (props.value === undefined) {
    return <Absent {...(props.absent === undefined ? {} : { what: props.absent })} />;
  }
  return guard(() => {
    const text = formatUsd(props.value as string, { signed: props.signed === true });
    if (props.signed !== true) {
      return <span className="num">{text}</span>;
    }
    const sign = usdSign(props.value as string);
    const tone = sign > 0 ? "num gain" : sign < 0 ? "num loss" : "num";
    return <span className={tone}>{text}</span>;
  });
}

export function Qty(props: {
  readonly value: string | undefined;
  readonly decimals: number | undefined;
  readonly symbol?: string;
  readonly absent?: string;
}): ReactNode {
  if (props.value === undefined || props.decimals === undefined) {
    return <Absent {...(props.absent === undefined ? {} : { what: props.absent })} />;
  }
  return guard(() => (
    <span className="num">
      {formatQuantity(props.value as string, props.decimals as number)}
      {props.symbol === undefined ? "" : ` ${props.symbol}`}
    </span>
  ));
}

/** Base units exactly as the backend holds them, for reconciliation. */
export function BaseUnits(props: { readonly value: string | undefined }): ReactNode {
  if (props.value === undefined) return <Absent />;
  return <span className="mono-small">{props.value} base units</span>;
}

export function Bps(props: { readonly value: number | undefined; readonly absent?: string }): ReactNode {
  if (props.value === undefined) {
    return <Absent {...(props.absent === undefined ? {} : { what: props.absent })} />;
  }
  return guard(() => (
    <span className="num">
      {formatBps(props.value as number)}{" "}
      <span className="mono-small">({formatBpsRaw(props.value as number)})</span>
    </span>
  ));
}

export function DecimalValue(props: { readonly value: string | undefined }): ReactNode {
  if (props.value === undefined) return <Absent />;
  return guard(() => <span className="num">{formatDecimalString(props.value as string)}</span>);
}
