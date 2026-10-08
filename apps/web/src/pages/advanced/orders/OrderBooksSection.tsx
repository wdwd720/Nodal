/**
 * The order-book section of the advanced (sandbox) area: the persistent
 * internal books of internal/orders, in PAPER and SANDBOX mode (ADR-0020).
 * Routed at /advanced/order-books/*.
 *
 *   /advanced/order-books                                  the books; an operator opens one
 *   /advanced/order-books/books/:instrumentId              one book: terms, fees, crossing
 *                                                          rule, depth, the order form and
 *                                                          the operator's controls
 *   /advanced/order-books/books/:instrumentId/trades/:seq  the audit proof of one trade
 *   /advanced/order-books/orders                           the account's orders
 *   /advanced/order-books/orders/:orderId                  one order: cancel, replace
 *   /advanced/order-books/trades                           the account's trades
 */
import type { ReactNode } from "react";
import { Route, Routes } from "react-router-dom";

import { AdvancedNotFound } from "../shared.tsx";
import { BookDetailPage } from "./BookDetail.tsx";
import { BooksPage } from "./Books.tsx";
import { OrderDetailPage, OrdersPage } from "./Orders.tsx";
import { TradeProofPage, TradesPage } from "./Trades.tsx";

export function OrderBooksSection(): ReactNode {
  return (
    <Routes>
      <Route index element={<BooksPage />} />
      <Route path="books/:instrumentId" element={<BookDetailPage />} />
      <Route path="books/:instrumentId/trades/:tradeSeq" element={<TradeProofPage />} />
      <Route path="orders" element={<OrdersPage />} />
      <Route path="orders/:orderId" element={<OrderDetailPage />} />
      <Route path="trades" element={<TradesPage />} />
      <Route path="*" element={<AdvancedNotFound />} />
    </Routes>
  );
}
