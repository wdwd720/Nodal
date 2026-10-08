/** Where the stable asset section lives under /advanced, and the address of each of its pages. */
import { advancedHref } from "../area.ts";

export const STABLE_PATH = "stable-assets";

export function stableHref(...segments: readonly string[]): string {
  return advancedHref(STABLE_PATH, ...segments);
}

export function stableAssetHref(stableAssetId: string): string {
  return stableHref(stableAssetId);
}

export function reserveHref(stableAssetId: string): string {
  return stableHref(stableAssetId, "reserve");
}

export function issuanceHref(issuanceId: string): string {
  return stableHref("issuances", issuanceId);
}

export function redemptionHref(redemptionId: string): string {
  return stableHref("redemptions", redemptionId);
}
