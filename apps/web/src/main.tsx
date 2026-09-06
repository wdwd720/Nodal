import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";

import { App } from "./App.tsx";
import { SessionProvider } from "./session.tsx";
import { isUnauthenticated } from "./api/problem.ts";
import "./styles.css";

/**
 * Retry policy: a business rejection is a decision, not a hiccup, so it is
 * never retried — the customer is shown what the backend said. Only transport
 * and server faults are retried, and never more than twice, because a financial
 * interface that silently hammers an endpoint hides a real outage.
 */
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15_000,
      refetchOnWindowFocus: true,
      retry: (failureCount, error) => {
        if (isUnauthenticated(error)) return false;
        return failureCount < 2;
      },
    },
    mutations: { retry: false },
  },
});

const container = document.getElementById("root");
if (container === null) {
  throw new Error("index.html is missing #root");
}

createRoot(container).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <SessionProvider>
          <App />
        </SessionProvider>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
