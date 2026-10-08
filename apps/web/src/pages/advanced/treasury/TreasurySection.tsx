/**
 * The treasury section of the advanced area (internal/treasury): the house
 * books, their policies, positions, PnL and limits in use, their activity,
 * the operator's commands on them, and the disclosed house-versus-customer
 * crossing rules. Every page renders <SimulatedNotice /> first.
 */
import type { ReactNode } from "react";
import { Route, Routes } from "react-router-dom";

import { AdvancedNotFound } from "../shared.tsx";
import { BookActivityPage } from "./BookActivity.tsx";
import { BookOperatePage } from "./BookOperate.tsx";
import { BookPage } from "./BookPage.tsx";
import { TreasuryBooksPage } from "./Books.tsx";
import { CrossingPage } from "./Crossing.tsx";

export function TreasurySection(): ReactNode {
  return (
    <Routes>
      <Route index element={<TreasuryBooksPage />} />
      <Route path="crossing" element={<CrossingPage />} />
      <Route path="books/:bookId" element={<BookPage />} />
      <Route path="books/:bookId/activity" element={<BookActivityPage />} />
      <Route path="books/:bookId/operate" element={<BookOperatePage />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}
