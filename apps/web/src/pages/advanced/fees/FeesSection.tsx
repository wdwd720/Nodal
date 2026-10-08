/**
 * The fees section of the advanced area (internal/feeledger): the published
 * fee schedules with their history and an exact quote calculator, this
 * account's quote disclosures, fee bookings and performance-fee periods, the
 * house trades against it, and — for operators — publishing, recording,
 * reversing, the spread audit and the performance-fee lifecycle. Every page
 * renders <SimulatedNotice /> first.
 */
import type { ReactNode } from "react";
import { Route, Routes } from "react-router-dom";

import { AdvancedNotFound } from "../shared.tsx";
import { FeeBookingPage, FeeBookingsPage } from "./Bookings.tsx";
import { HouseTradesPage } from "./HouseTrades.tsx";
import { FeePeriodPage, FeePeriodsPage } from "./Periods.tsx";
import { FeeQuotePage, FeeQuotesPage } from "./Quotes.tsx";
import { FeeSchedulePage, FeeSchedulesPage } from "./Schedules.tsx";
import { SpreadAuditPage } from "./SpreadAudit.tsx";

export function FeesSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<FeeSchedulesPage />} />
      <Route path="schedule" element={<FeeSchedulePage />} />
      <Route path="quotes" element={<FeeQuotesPage />} />
      <Route path="quotes/:disclosureId" element={<FeeQuotePage />} />
      <Route path="bookings" element={<FeeBookingsPage />} />
      <Route path="bookings/:bookingId" element={<FeeBookingPage />} />
      <Route path="periods" element={<FeePeriodsPage />} />
      <Route path="periods/:periodId" element={<FeePeriodPage />} />
      <Route path="house-trades" element={<HouseTradesPage />} />
      <Route path="spread-audit" element={<SpreadAuditPage />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}
