/**
 * The equities section of the advanced (sandbox) area: equities and ETFs of
 * internal/equities in PAPER and SANDBOX mode, filled by a deterministic
 * paper venue and settled T+1 (ADR-0020). Routed at /advanced/equities/*.
 *
 *   /advanced/equities                              listings and the session;
 *                                                   an operator lists one
 *   /advanced/equities/listings/:instrumentId       one listing, its order form
 *                                                   and corporate actions
 *   /advanced/equities/orders                       the account's orders
 *   /advanced/equities/orders/:orderId              one order, its fill, cancel
 *   /advanced/equities/fills                        the account's fills
 *   /advanced/equities/settlements                  the account's T+1 settlements
 *   /advanced/equities/corporate-actions            every action, the account's postings
 *   /advanced/equities/corporate-actions/:actionId  one action; withdraw, apply
 *   /advanced/equities/calendar                     the calendar; holidays and
 *                                                   the operator's worker runs
 */
import type { ReactNode } from "react";
import { Route, Routes } from "react-router-dom";

import { AdvancedNotFound } from "../shared.tsx";
import { CalendarPage } from "./Calendar.tsx";
import { CorporateActionDetailPage, CorporateActionsPage } from "./CorporateActions.tsx";
import { FillsPage } from "./Fills.tsx";
import { ListingDetailPage, ListingsPage } from "./Listings.tsx";
import { EquityOrderDetailPage, EquityOrdersPage } from "./Orders.tsx";
import { SettlementsPage } from "./Settlements.tsx";

export function EquitiesSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<ListingsPage />} />
      <Route path="listings/:instrumentId" element={<ListingDetailPage />} />
      <Route path="orders" element={<EquityOrdersPage />} />
      <Route path="orders/:orderId" element={<EquityOrderDetailPage />} />
      <Route path="fills" element={<FillsPage />} />
      <Route path="settlements" element={<SettlementsPage />} />
      <Route path="corporate-actions" element={<CorporateActionsPage />} />
      <Route path="corporate-actions/:actionId" element={<CorporateActionDetailPage />} />
      <Route path="calendar" element={<CalendarPage />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}
