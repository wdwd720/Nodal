/** Where the bridge section lives under /advanced, and the address of each of its pages. */
import { advancedHref } from "../area.ts";

export const BRIDGE_PATH = "bridge";

export function bridgeHref(...segments: readonly string[]): string {
  return advancedHref(BRIDGE_PATH, ...segments);
}

export function legHref(legId: string): string {
  return bridgeHref("legs", legId);
}
