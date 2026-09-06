/**
 * What a token actually is.
 *
 * A symbol is not an identity: "USDC" is a label anyone can put on a mint. The
 * thing that decides what a customer owns is the chain plus the mint address
 * plus the decimals, so every place an asset appears, those appear with it.
 * PART 111 asks the trade screen for "exact mint identity/details"; this is
 * that, and it is reused everywhere else for the same reason.
 */
import type { ReactNode } from "react";

import type { Asset } from "../api/queries.ts";
import { USDC_DISCLOSURE } from "../lib/honesty.ts";
import { Pill } from "./Layout.tsx";

function statusTone(status: string): "good" | "warn" | "bad" | "neutral" {
  switch (status) {
    case "ACTIVE":
      return "good";
    case "CLOSE_ONLY":
    case "RESTRICTED":
    case "DELISTING":
      return "warn";
    case "HALTED":
    case "DELISTED":
      return "bad";
    default:
      return "neutral";
  }
}

export function AssetIdentity(props: { readonly asset: Asset; readonly compact?: boolean }): ReactNode {
  const { asset } = props;
  return (
    <div className={props.compact === true ? "mint mint-compact" : "mint"}>
      <div className="mint-head">
        <span className="mint-symbol">{asset.symbol}</span>
        <span className="mint-name">{asset.name}</span>
        <Pill tone={statusTone(asset.status)}>{asset.status}</Pill>
        {asset.is_stablecoin && (
          <Pill tone="info" title={USDC_DISCLOSURE}>
            stablecoin{asset.peg_currency === undefined ? "" : ` pegged to ${asset.peg_currency}`}
          </Pill>
        )}
      </div>
      <dl className="mint-facts">
        <div>
          <dt>Chain</dt>
          <dd>{asset.chain}</dd>
        </div>
        <div>
          <dt>Mint address</dt>
          <dd>
            <code className="mono-small">{asset.mint_address}</code>
          </dd>
        </div>
        <div>
          <dt>Token program</dt>
          <dd>{asset.kind}</dd>
        </div>
        <div>
          <dt>Decimals</dt>
          <dd>{String(asset.decimals)}</dd>
        </div>
        <div>
          <dt>Risk class</dt>
          <dd>{asset.risk_class}</dd>
        </div>
        {asset.token_extensions !== undefined && asset.token_extensions.length > 0 && (
          <div>
            <dt>Token extensions</dt>
            <dd>{asset.token_extensions.join(", ")}</dd>
          </div>
        )}
      </dl>
    </div>
  );
}

/** A one-line form for tables: symbol plus the mint that decides identity. */
export function AssetLine(props: { readonly asset: Asset | undefined; readonly fallbackId?: string }): ReactNode {
  if (props.asset === undefined) {
    return (
      <span className="absent">
        asset {props.fallbackId ?? "unknown"} was not in the asset registry response
      </span>
    );
  }
  return (
    <span className="asset-line">
      <strong>{props.asset.symbol}</strong>{" "}
      <code className="mono-small">
        {props.asset.chain}:{props.asset.mint_address}
      </code>
    </span>
  );
}
