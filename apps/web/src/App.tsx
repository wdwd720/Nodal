import type { ReactNode } from "react";
import { Route, Routes } from "react-router-dom";

import { AppShell } from "./components/AppShell.tsx";
import { Explanation, Loading } from "./components/DataState.tsx";
import { useSession } from "./session.tsx";
import { Activity } from "./pages/Activity.tsx";
import { AddFunds } from "./pages/AddFunds.tsx";
import { Agents } from "./pages/Agents.tsx";
import { Home } from "./pages/Home.tsx";
import { Lab } from "./pages/Lab.tsx";
import { Marketplace } from "./pages/Marketplace.tsx";
import { NativeMarkets } from "./pages/NativeMarkets.tsx";
import { NodalEconomy } from "./pages/NodalEconomy.tsx";
import { NotFound } from "./pages/NotFound.tsx";
import { Payouts } from "./pages/Payouts.tsx";
import { Portfolio } from "./pages/Portfolio.tsx";
import { Settings } from "./pages/Settings.tsx";
import { SignIn } from "./pages/SignIn.tsx";
import { StrategyBuilder } from "./pages/StrategyBuilder.tsx";
import { Trade } from "./pages/Trade.tsx";

export function App(): ReactNode {
  const session = useSession();

  if (session.loading) {
    return (
      <div className="boot">
        <Loading label="Checking this session with the backend…" />
      </div>
    );
  }

  // Only the backend's own 401 produces the sign-in screen. A failure to REACH
  // the backend is a different thing, and telling a signed-in customer they are
  // not signed in would be the application asserting something it does not
  // know. It says what actually happened and offers to ask again.
  if (session.signedOut) {
    return <SignIn />;
  }

  if (!session.signedIn) {
    return (
      <div className="boot">
        <Explanation error={session.error} onRetry={session.refetch}>
          <p>
            This is a failure to reach the backend, not a statement about your session. Nothing has
            signed you out.
          </p>
        </Explanation>
      </div>
    );
  }

  return (
    <AppShell>
      {session.error !== undefined && session.error !== null && (
        <Explanation error={session.error} onRetry={session.refetch} />
      )}
      <Routes>
        <Route path="/" element={<Home />} />
        <Route path="/add-funds" element={<AddFunds />} />
        {/* The internal economy. Three routes, deliberately not one: Credits,
            what you can buy with them, and what you can create. */}
        <Route path="/nodal-economy" element={<NodalEconomy />} />
        <Route path="/marketplace" element={<Marketplace />} />
        <Route path="/native-markets" element={<NativeMarkets />} />
        <Route path="/payouts" element={<Payouts />} />
        <Route path="/trade" element={<Trade />} />
        <Route path="/portfolio" element={<Portfolio />} />
        <Route path="/strategy" element={<StrategyBuilder />} />
        <Route path="/agents" element={<Agents />} />
        <Route path="/lab" element={<Lab />} />
        <Route path="/activity" element={<Activity />} />
        <Route path="/settings" element={<Settings />} />
        <Route path="*" element={<NotFound />} />
      </Routes>
    </AppShell>
  );
}
