/**
 * AGENT AUTHORITY: what an account's owner has delegated to the account's own
 * agents, and what those agents asked for under it (ADR-0020;
 * internal/delegation behind /v1/advanced/delegation).
 *
 * The backend decides everything this section shows: a capability's scope,
 * limits, use and expiry; whether a swap the agent asked for is signed (only
 * after the transaction inspector proves it is the approved plan); whether a
 * withdrawal it proposed may leave (only to an allow-listed destination,
 * under the customer's policy, with the owner's decision except where a test
 * environment's policy approves it automatically). An agent never reaches
 * this API -- it can never sign, execute, decide or read a key -- so every
 * page here is the owner's or an operator's.
 */
import type { ReactNode } from "react";
import { Route, Routes } from "react-router-dom";

import { AdvancedNotFound } from "../shared.tsx";
import { CapabilityDetail } from "./CapabilityDetail.tsx";
import { Destinations } from "./Destinations.tsx";
import { Operator } from "./Operator.tsx";
import { Overview } from "./Overview.tsx";
import { SwapDetail } from "./SwapDetail.tsx";
import { WithdrawalDetail } from "./WithdrawalDetail.tsx";

export function DelegationSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<Overview />} />
      <Route path="capabilities/:capabilityId" element={<CapabilityDetail />} />
      <Route path="swaps/:swapId" element={<SwapDetail />} />
      <Route path="withdrawals/:withdrawalId" element={<WithdrawalDetail />} />
      <Route path="destinations" element={<Destinations />} />
      <Route path="operator" element={<Operator />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}
