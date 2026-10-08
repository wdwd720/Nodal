/**
 * The derivatives section of the advanced (sandbox) area: futures, perpetuals
 * and options in PAPER and SANDBOX mode, cleared through one clearing house
 * (internal/derivatives, ADR-0020). Routed at /advanced/derivatives/*.
 *
 *   /advanced/derivatives                     contracts and the policy
 *   /advanced/derivatives/contracts/:id       one contract, its marks, open,
 *                                             analytics, operator actions
 *   /advanced/derivatives/positions           the account's positions
 *   /advanced/derivatives/positions/:id       one position, close, exercise
 *   /advanced/derivatives/flows               the flow register and funding
 *   /advanced/derivatives/clearing-house      the operator's clearing house
 */
import type { ReactNode } from "react";
import { Route, Routes } from "react-router-dom";

import { AdvancedNotFound } from "../shared.tsx";
import { ClearingHousePage } from "./ClearingHouse.tsx";
import { ContractDetailPage } from "./ContractDetail.tsx";
import { ContractsPage } from "./Contracts.tsx";
import { FlowsPage } from "./Flows.tsx";
import { PositionDetailPage } from "./PositionDetail.tsx";
import { PositionsPage } from "./Positions.tsx";

export function DerivativesSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<ContractsPage />} />
      <Route path="contracts/:instrumentId" element={<ContractDetailPage />} />
      <Route path="positions" element={<PositionsPage />} />
      <Route path="positions/:positionId" element={<PositionDetailPage />} />
      <Route path="flows" element={<FlowsPage />} />
      <Route path="clearing-house" element={<ClearingHousePage />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}
