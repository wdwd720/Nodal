/**
 * The one number on the ticket that is the CUSTOMER'S and not the market's.
 *
 * A quote is a record of what the internal market said at a state version. It
 * is not a promise: the backend re-prices the order against current state when
 * it executes, and the only protection the caller has is `min_output` — the
 * least they are prepared to receive, which travels with the order and is
 * checked against a freshly computed fill.
 *
 * So `min_output` cannot come from the backend. There is no endpoint that
 * computes it, and there should not be: it is a statement of the customer's own
 * tolerance, and a default equal to the quote would be a tolerance of zero
 * dressed up as a protection — every order would be refused, because the market
 * moves between the quote and the fill by design.
 *
 * What this module does is translate a tolerance the customer CHOSE into the
 * exact base units the API demands. Two rules make that honest:
 *
 *   1. IT IS EXACT. BigInt throughout. The inputs are integer base-unit strings
 *      and the output is one, and the value is never a double at any point.
 *   2. IT ROUNDS TOWARDS THE CUSTOMER. A remainder rounds the minimum UP, which
 *      makes the protection very slightly stronger than the tolerance asked
 *      for. Rounding the other way would quietly weaken the one number the
 *      customer is actually agreeing to.
 *
 * It computes no price, no fee and no valuation. Everything on the ticket that
 * is a claim about the market comes from the quote or the fill.
 */

/** Basis points in one hundred per cent, which is what a tolerance is out of. */
const FULL = 10_000n;

export class ToleranceError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ToleranceError";
  }
}

/**
 * The tolerances the ticket offers, with what each one means for a customer.
 *
 * Three, and no free-text field. A tolerance is a risk decision made in a
 * hurry, and an open number box invites a zero — which refuses every order —
 * or a digit too many, which accepts almost any fill.
 */
export const TOLERANCES: ReadonlyArray<{
  readonly bps: number;
  readonly label: string;
  readonly note: string;
}> = [
  {
    bps: 50,
    label: "0.5%",
    note: "Tightest. On a thin market this refuses more orders than it fills.",
  },
  {
    bps: 100,
    label: "1%",
    note: "A working default for a market with depth behind it.",
  },
  {
    bps: 500,
    label: "5%",
    note: "Loosest. It will fill through a move you might not have accepted.",
  },
];

/**
 * The least the customer will accept, in exact base units.
 *
 * `expected` is the quote's expected output, exactly as the API sent it;
 * `toleranceBps` is how far below it the customer will still take the trade.
 * The result is what goes on the order as `min_output`, and it is the number
 * the confirmation step shows as "minimum received".
 */
export function minimumOutput(expected: string, toleranceBps: number): string {
  if (!/^[0-9]+$/.test(expected)) {
    throw new ToleranceError(`expected output: ${JSON.stringify(expected)} is not base units`);
  }
  if (!Number.isInteger(toleranceBps) || toleranceBps < 0 || toleranceBps > 10_000) {
    throw new ToleranceError(`tolerance: ${String(toleranceBps)} is not a basis-point count`);
  }
  const amount = BigInt(expected);
  const keep = FULL - BigInt(toleranceBps);
  const product = amount * keep;
  const floor = product / FULL;
  // Round up on any remainder: a minimum that is one base unit higher is a
  // protection the customer did not ask to be weakened.
  return String(product % FULL === 0n ? floor : floor + 1n);
}

/**
 * Whether a fill met the minimum the customer agreed to, by exact comparison.
 *
 * The ticket states this after a fill. It is a COMPARISON and not a
 * computation: how far the fill landed from the quote is the backend's
 * `slippage_bps`, which the fill already carries, and recomputing it here would
 * give the screen a second answer to a question the ledger has already decided.
 */
export function meetsMinimum(received: string, minimum: string): boolean | undefined {
  if (!/^[0-9]+$/.test(received) || !/^[0-9]+$/.test(minimum)) return undefined;
  return BigInt(received) >= BigInt(minimum);
}
