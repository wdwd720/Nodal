/** Where the prediction markets section lives under /advanced, and the address of one market. */
import { advancedHref } from "../area.ts";

export const PREDMARKET_PATH = "prediction-markets";

export function marketsHref(): string {
  return advancedHref(PREDMARKET_PATH);
}

export function marketHref(marketId: string): string {
  return advancedHref(PREDMARKET_PATH, marketId);
}
