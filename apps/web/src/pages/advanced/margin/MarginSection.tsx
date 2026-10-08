/**
 * The margin section of the advanced area (internal/margin): the account's
 * margin account, its calls and cases, its stress tests, and the operator's
 * liquidation queue. Every page renders <SimulatedNotice /> first.
 */
import type { ReactNode } from "react";
import { Route, Routes } from "react-router-dom";

import { AdvancedNotFound } from "../shared.tsx";
import { MarginAccountPage } from "./MarginAccountPage.tsx";
import { MarginCasePage, MarginHistoryPage } from "./MarginHistory.tsx";
import { MarginOperatorPage } from "./MarginOperator.tsx";
import { MarginStressPage, MarginStressRunPage } from "./MarginStress.tsx";

export function MarginSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<MarginAccountPage />} />
      <Route path="history" element={<MarginHistoryPage />} />
      <Route path="cases/:caseId" element={<MarginCasePage />} />
      <Route path="stress" element={<MarginStressPage />} />
      <Route path="stress/:runId" element={<MarginStressRunPage />} />
      <Route path="operator" element={<MarginOperatorPage />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}
