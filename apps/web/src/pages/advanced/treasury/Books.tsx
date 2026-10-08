/**
 * TREASURY — the house books.
 *
 * The house book trades only simulated house capital, never a customer's:
 * its value lives in accounts of its own, and every order and fill passes the
 * book's signing policy, its risk limits and Risk Kernel V2 first. This page
 * lists the registered house books and registers a new one.
 *
 * The section is the operator's (sandbox:operate). A customer is refused by
 * the backend, and the page shows that refusal with its code rather than
 * hiding a panel to pretend the question was not asked.
 */
import { useState, type ReactNode } from "react";

import { useRegisterBook, useTreasuryBooks, type TreasuryBook, type TreasuryBookDetail, type TreasuryRegisterRequest } from "../../../api/advanced/treasury.ts";
import type { CursorPage } from "../../../api/advanced/core.ts";
import { AsyncPanel } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel, Pill } from "../../../components/Layout.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, forward, type CursorTrail } from "../../../lib/advanced.ts";
import {
  CLASS_MEANING,
  STRATEGY_MEANING,
  TREASURY_STRATEGIES,
  bookStatusTone,
  parseText,
  parseUuid,
  type TreasuryStrategy,
} from "../../../lib/treasury.ts";
import { formatInstant } from "../../../lib/time.ts";
import { CursorPager, ScrollTable } from "../shared.tsx";
import { Choice, CommandFlow, CommandForm, TextInput, Toggle, prepare } from "./command.tsx";
import { AssetName, BookLink, TreasuryNav } from "./parts.tsx";

export function TreasuryBooksPage(): ReactNode {
  return (
    <Page
      title="Treasury"
      lead="The house books: principal and treasury books that trade only simulated house capital, each under its own signing policy and risk limits."
    >
      <SimulatedNotice />
      <TreasuryNav />
      <BooksPanel />
      <RegisterPanel />
    </Page>
  );
}

function BooksPanel(): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const books = useTreasuryBooks(trail.current);
  return (
    <Panel
      title="House books"
      description="Every registered house book, newest first. A house book holds only simulated value, in accounts no customer shares."
    >
      <AsyncPanel
        query={books}
        loadingLabel="Loading the house books…"
        empty={{
          isEmpty: (page: CursorPage<TreasuryBook>) => page.items.length === 0 && trail.previous.length === 0,
          title: "No house books",
          body: "The backend reports no registered house book.",
        }}
      >
        {(page: CursorPage<TreasuryBook>) => (
          <>
            <ScrollTable caption="House books" headers={["Name", "Strategy", "Class", "Status", "Valuation asset", "Version", "Registered"]}>
              {page.items.map((book) => (
                <tr key={book.house_account_id} data-book={book.house_account_id}>
                  <th scope="row">
                    <BookLink bookId={book.house_account_id} name={book.name} />
                    <span className="cell-note mono-small">{book.house_account_id}</span>
                  </th>
                  <td>
                    <code className="mono-small">{book.strategy}</code>
                    <span className="cell-note">{STRATEGY_MEANING[book.strategy]}</span>
                  </td>
                  <td>
                    <code className="mono-small">{book.class}</code>
                  </td>
                  <td>
                    <Pill tone={bookStatusTone(book.status)}>{book.status}</Pill>
                  </td>
                  <td>
                    <AssetName asset={book.valuation_asset} />
                  </td>
                  <td>{String(book.version)}</td>
                  <td>
                    {formatInstant(book.registered_at)}
                    <span className="cell-note">
                      {book.registered_by_actor_type} <code className="mono-small">{book.registered_by_actor_id}</code>
                    </span>
                  </td>
                </tr>
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
  );
}

function RegisterPanel(): ReactNode {
  const register = useRegisterBook();
  const [name, setName] = useState("");
  const [strategy, setStrategy] = useState<TreasuryStrategy>("MARKET_MAKING");
  const [serviceActor, setServiceActor] = useState("");
  const [operatorsAllowed, setOperatorsAllowed] = useState(false);
  const [valuationAsset, setValuationAsset] = useState("");
  const [reason, setReason] = useState("");

  const n = parseText(name, "The name", 200);
  const s = parseText(serviceActor, "The house service id", 100);
  const r = parseText(reason, "The reason", 500);
  const v = valuationAsset.trim() === "" ? undefined : parseUuid(valuationAsset, "The valuation asset id");
  const prepared = prepare<TreasuryRegisterRequest>(
    [n.ok ? undefined : n.error, s.ok ? undefined : s.error, r.ok ? undefined : r.error, v === undefined || v.ok ? undefined : v.error],
    () => ({
      name: n.ok ? n.value : "",
      strategy,
      service_actor_id: s.ok ? s.value : "",
      operators_allowed: operatorsAllowed,
      reason: r.ok ? r.value : "",
      ...(v !== undefined && v.ok ? { valuation_asset_id: v.value } : {}),
    }),
  );

  return (
    <Panel
      title="Register a house book"
      description="Creates a house book with its class, valuation asset and first signing policy. Only an operator registers one, with a recent strong sign-in; the valuation asset must be a simulated, US-dollar-pegged asset."
    >
      <CommandForm label="Register a house book">
        <TextInput id="treasury-register-name" label="Name" value={name} onChange={setName} />
        <Choice
          id="treasury-register-strategy"
          label="Strategy"
          value={strategy}
          options={TREASURY_STRATEGIES}
          onChange={setStrategy}
          describe={(value) => STRATEGY_MEANING[value]}
        />
        <TextInput
          id="treasury-register-service"
          label="House service id"
          value={serviceActor}
          onChange={setServiceActor}
          note="The one service the signing policy lets act for the book. A sandbox house book holds no keys."
        />
        <Toggle
          id="treasury-register-operators"
          label="Operators may place orders and fills"
          checked={operatorsAllowed}
          onChange={setOperatorsAllowed}
          note="Operators may always cancel and administer; trading through this page needs this to be allowed."
        />
        <TextInput
          id="treasury-register-valuation"
          label="Valuation asset id (optional)"
          value={valuationAsset}
          onChange={setValuationAsset}
          note="Empty means the sandbox dollar."
        />
        <TextInput id="treasury-register-reason" label="Reason" value={reason} onChange={setReason} />
        <CommandFlow<TreasuryRegisterRequest, TreasuryBookDetail>
          label="Register the house book"
          prepared={prepared}
          command={register}
          refusedNote="No house book was registered."
          describe={(request) => (
            <FieldGrid columns={3}>
              <Field label="Name">{request.name}</Field>
              <Field label="Strategy">
                <code className="mono-small">{request.strategy}</code>
              </Field>
              <Field label="House service">
                <code className="mono-small">{request.service_actor_id}</code>
              </Field>
              <Field label="Operators may trade">{request.operators_allowed ? "yes" : "no"}</Field>
              <Field label="Valuation asset">
                {request.valuation_asset_id === undefined ? "the sandbox dollar" : <Identifier value={request.valuation_asset_id} />}
              </Field>
              <Field label="Reason">{request.reason}</Field>
            </FieldGrid>
          )}
          renderResult={(book) => (
            <FieldGrid columns={3}>
              <Field label="House book">
                <BookLink bookId={book.house_account_id} name={book.name} />
              </Field>
              <Field label="Class" note={CLASS_MEANING[book.class]}>
                <code className="mono-small">{book.class}</code>
              </Field>
              <Field label="Signing policy">v{String(book.signing_policy.version)}</Field>
            </FieldGrid>
          )}
        />
      </CommandForm>
    </Panel>
  );
}
