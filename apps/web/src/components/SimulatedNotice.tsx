/**
 * The standing notice of the advanced (sandbox) area.
 *
 * Every page of the area renders it first, under its heading and before any
 * figure: the reader learns what kind of value they are looking at before
 * they see an amount of it. It is part of the page rather than a dismissible
 * banner for the same reason the risk statement is part of the footer, and
 * `honesty.test.ts` refuses a page of the area that does not render it.
 */
import type { ReactNode } from "react";

import { SANDBOX_NOTICE, SANDBOX_NOTICE_TITLE } from "../lib/honesty.ts";

export function SimulatedNotice(): ReactNode {
  return (
    <div className="banner banner-warn sim-notice" role="note" aria-label={SANDBOX_NOTICE_TITLE}>
      <p className="sim-notice-title">{SANDBOX_NOTICE_TITLE}</p>
      <p className="sim-notice-body">{SANDBOX_NOTICE}</p>
    </div>
  );
}
