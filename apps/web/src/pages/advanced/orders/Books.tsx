/**
 * ORDER BOOKS: the internal books this deployment runs, oldest first, each
 * with its status, mode, pair, tick and lot, and the fee schedules it
 * discloses. An operator opens books here; nothing opens one on its own.
 */
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";

import { useBooks, type Book } from "../../../api/advanced/orders.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Page, Panel } from "../../../components/Layout.tsx";
import { Bps } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { BOOK_STATUS_MEANING, INTERNAL_BOOK_NOTE } from "../../../lib/orders.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, ModePill, ScrollTable } from "../shared.tsx";
import { OpenBookForm } from "./forms.tsx";
import { OrderBooksNav, PriceOf, StatusPill, Units, bookStatusTone, booksHref } from "./parts.tsx";

export function BooksPage(): ReactNode {
  const { canInspect } = useAdvancedAccount();
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const books = useBooks(trail.current);
  return (
    <Page
      title="Order books"
      lead="The internal books this deployment runs: limit and market orders between accounts, matched by price and time, with every trade posted to the ledger in simulated assets."
    >
      <SimulatedNotice />
      <OrderBooksNav />
      <p className="note">{INTERNAL_BOOK_NOTE}</p>
      <Panel title="Books" description="Oldest first. The fees are the percentage each schedule charges on a trade's notional.">
        <AsyncPanel
          query={books}
          loadingLabel="Loading books…"
          empty={{
            isEmpty: (page) => page.items.length === 0 && trail.previous.length === 0,
            title: "No order books",
            body: "The backend lists no internal book in this deployment. An operator opens one; until then there is nothing to trade on.",
          }}
        >
          {(page) => (
            <>
              <ScrollTable caption="Order books" headers={["Pair", "Status", "Mode", "Tick", "Lot", "Taker fee", "Maker fee", "Trades"]}>
                {page.items.map((book) => (
                  <BookRow key={book.id} book={book} />
                ))}
              </ScrollTable>
              <CursorPager
                noun="books"
                trail={trail}
                nextCursor={page.nextCursor}
                onForward={(cursor) => {
                  setTrail(forward(trail, cursor));
                }}
                onBack={() => {
                  setTrail(back(trail));
                }}
              />
            </>
          )}
        </AsyncPanel>
      </Panel>
      {canInspect && (
        <Panel
          title="Open a book"
          description="Operator only: the backend refuses anyone without sandbox:operate and a recent step-up, whatever this page shows."
        >
          <OpenBookForm />
        </Panel>
      )}
    </Page>
  );
}

function BookRow(props: { readonly book: Book }): ReactNode {
  const { book } = props;
  return (
    <tr>
      <th scope="row">
        <Link to={booksHref("books", book.instrument_id)}>
          {book.base_symbol}/{book.quote_symbol}
        </Link>
        <span className="cell-note">{book.instrument_kind}</span>
      </th>
      <td>
        <StatusPill status={book.status} tone={bookStatusTone(book.status)} />
        <span className="cell-note">{BOOK_STATUS_MEANING[book.status]}</span>
      </td>
      <td>
        <ModePill mode={book.mode} />
      </td>
      <td>
        <PriceOf value={book.tick_price} quote={book.quote_symbol} />
      </td>
      <td>
        <Units value={book.lot_size} decimals={book.base_decimals} symbol={book.base_symbol} />
      </td>
      <td>
        <Bps value={book.taker_fee.percentage_fee_bps} />
      </td>
      <td>
        <Bps value={book.maker_fee.percentage_fee_bps} />
      </td>
      <td>{String(book.last_trade_seq)}</td>
    </tr>
  );
}
