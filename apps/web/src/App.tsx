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
import { NotFound } from "./pages/NotFound.tsx";
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

  if (!session.signedIn) {
    return <SignIn />;
  }

  return (
    <AppShell>
      {session.error !== undefined && session.error !== null && (
        <Explanation error={session.error} onRetry={session.refetch} />
      )}
      <Routes>
        <Route path="/" element={<Home />} />
        <Route path="/add-funds" element={<AddFunds />} />
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
