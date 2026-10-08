/**
 * EQUITIES: the listed equities and ETFs, each with its terms and the
 * freshest mark the price store holds for it, under the session in force on
 * the XNYS calendar; and ONE LISTING, with its terms, its corporate actions
 * and the order form. An operator lists equities here.
 */
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import {
  useCorporateActions,
  useEquityListing,
  useEquityListings,
  type EquityListing,
  type EquitySessionState,
} from "../../../api/advanced/equities.ts";
import { AsyncPanel, EmptyState } from "../../../components/DataState.tsx";
import { Field, FieldGrid, Identifier, Page, Panel } from "../../../components/Layout.tsx";
import { DecimalValue } from "../../../components/Money.tsx";
import { SimulatedNotice } from "../../../components/SimulatedNotice.tsx";
import { FIRST_PAGE, back, formatMillis, forward, type CursorTrail } from "../../../lib/advanced.ts";
import { PAPER_VENUE_NOTE, T_PLUS_ONE_NOTE } from "../../../lib/equities.ts";
import { useAdvancedAccount } from "../area.ts";
import { CursorPager, ScrollTable, ValueDomainPill } from "../shared.tsx";
import { ActionRows } from "./CorporateActions.tsx";
import { AnnounceActionForm, ListEquityForm, OperatorOnly, SubmitEquityOrderForm } from "./forms.tsx";
import { EquitiesNav, Recorded, SessionFields, Units, equitiesHref } from "./parts.tsx";

export function ListingsPage(): ReactNode {
  const { canInspect } = useAdvancedAccount();
  const listings = useEquityListings();
  return (
    <Page
      title="Equities"
      lead="Equities and ETFs in PAPER and SANDBOX mode: simulated shares, filled by a deterministic paper venue at the latest mark and settled T+1."
    >
      <SimulatedNotice />
      <EquitiesNav />
      <AsyncPanel query={listings} loadingLabel="Loading listings…">
        {(list) => (
          <>
            <Panel title="Market session" description="The XNYS calendar as the backend reads it now.">
              <SessionFields session={list.session} />
            </Panel>
            <Panel title="Listings" description={PAPER_VENUE_NOTE}>
              {list.items.length === 0 ? (
                <EmptyState
                  title="No listings"
                  body="The backend lists no equity or ETF in this deployment. An operator lists one; until then there is nothing to trade."
                />
              ) : (
                <ScrollTable caption="Listings" headers={["Symbol", "Kind", "Status", "Mark", "Tick", "Lot", "Settles in"]}>
                  {list.items.map((listing) => (
                    <ListingRow key={listing.instrument_id} listing={listing} />
                  ))}
                </ScrollTable>
              )}
            </Panel>
          </>
        )}
      </AsyncPanel>
      {canInspect && (
        <Panel title="List an equity" description="Creates the instrument and its simulated share asset.">
          <OperatorOnly>
            <ListEquityForm />
          </OperatorOnly>
        </Panel>
      )}
    </Page>
  );
}

function ListingRow(props: { readonly listing: EquityListing }): ReactNode {
  const { listing } = props;
  return (
    <tr>
      <th scope="row">
        <Link to={equitiesHref("listings", listing.instrument_id)}>{listing.symbol}</Link>
        <span className="cell-note">
          {listing.name} · {listing.exchange_mic}
        </span>
      </th>
      <td>{listing.kind}</td>
      <td>{listing.status}</td>
      <td>
        {listing.mark === undefined ? (
          <span className="absent">no fresh mark</span>
        ) : (
          <>
            <DecimalValue value={listing.mark.price} />
            <span className="cell-note">
              {listing.settlement_symbol} · <Recorded at={listing.mark.observed_at} />
            </span>
          </>
        )}
      </td>
      <td>
        <DecimalValue value={listing.tick_price} />
      </td>
      <td>
        <Units value={listing.lot_size} decimals={listing.share_decimals} symbol={listing.symbol} />
      </td>
      <td>{listing.settlement_symbol}</td>
    </tr>
  );
}

export function ListingDetailPage(): ReactNode {
  const { instrumentId } = useParams();
  const listing = useEquityListing(instrumentId);
  const listings = useEquityListings();
  const session = listings.data?.session;
  return (
    <Page
      title={listing.data === undefined ? "Equity listing" : listing.data.symbol}
      lead="One listing: its terms, its freshest mark, its corporate actions and the order form."
    >
      <SimulatedNotice />
      <EquitiesNav />
      <AsyncPanel query={listing} loadingLabel="Loading the listing…">
        {(l) => <ListingPanels listing={l} session={session} />}
      </AsyncPanel>
    </Page>
  );
}

function ListingPanels(props: { readonly listing: EquityListing; readonly session: EquitySessionState | undefined }): ReactNode {
  const { listing } = props;
  const { canInspect } = useAdvancedAccount();
  return (
    <>
      <Panel title="Terms" description={PAPER_VENUE_NOTE}>
        <FieldGrid columns={3}>
          <Field label="Name">
            {listing.name}
            <span className="cell-note">
              {listing.kind} on {listing.exchange_mic}
            </span>
          </Field>
          <Field label="Status">{listing.status}</Field>
          <Field label="Value domain">
            <ValueDomainPill value={listing.value_domain} />
          </Field>
          <Field label="Mark" note={`A mark older than ${formatMillis(listing.oracle_max_age_ms)} is stale and prices nothing.`}>
            {listing.mark === undefined ? (
              <span className="absent">no fresh mark: market orders and valuations are refused until one is recorded</span>
            ) : (
              <>
                <DecimalValue value={listing.mark.price} /> <span className="cell-note">{listing.settlement_symbol} per share</span>
                <span className="cell-note">
                  {listing.mark.source} · <Recorded at={listing.mark.observed_at} />
                </span>
              </>
            )}
          </Field>
          <Field label="Tick" note={`Prices carry ${String(listing.price_scale)} decimal places.`}>
            <DecimalValue value={listing.tick_price} />
          </Field>
          <Field label="Lot">
            <Units value={listing.lot_size} decimals={listing.share_decimals} symbol={listing.symbol} />
          </Field>
          <Field label="Minimum quantity">
            <Units value={listing.min_quantity} decimals={listing.share_decimals} symbol={listing.symbol} />
          </Field>
          <Field label="Shares" note="A simulated asset that exists only in this deployment.">
            {listing.share_symbol} <span className="cell-note">{String(listing.share_decimals)} decimal places</span>
          </Field>
          <Field label="Settles in" note={T_PLUS_ONE_NOTE}>
            {listing.settlement_symbol}
          </Field>
          <Field label="Venue">{listing.venue}</Field>
          <Field label="Calendar">{listing.trading_calendar}</Field>
          <Field label="Terms version">
            {String(listing.terms_version)}
            <span className="cell-note">
              <Identifier value={listing.terms_hash} />
            </span>
          </Field>
        </FieldGrid>
      </Panel>
      {props.session !== undefined && (
        <Panel title="Market session" description="The XNYS calendar as the backend reads it now.">
          <SessionFields session={props.session} />
        </Panel>
      )}
      <Panel title="Place an order" description="From this session's own account, at the paper venue.">
        <SubmitEquityOrderForm listing={listing} session={props.session} />
      </Panel>
      <Panel title="Corporate actions" description="Every action announced for this listing, newest effective date first.">
        <ListingActions instrumentId={listing.instrument_id} />
      </Panel>
      {canInspect && (
        <Panel title="Announce a corporate action" description="For this listing.">
          <OperatorOnly>
            <AnnounceActionForm instrumentId={listing.instrument_id} />
          </OperatorOnly>
        </Panel>
      )}
    </>
  );
}

function ListingActions(props: { readonly instrumentId: string }): ReactNode {
  const [trail, setTrail] = useState<CursorTrail>(FIRST_PAGE);
  const actions = useCorporateActions(props.instrumentId, trail.current);
  return (
    <AsyncPanel
      query={actions}
      loadingLabel="Loading corporate actions…"
      empty={{
        isEmpty: (page) => page.items.length === 0 && trail.previous.length === 0,
        title: "No corporate actions",
        body: "The backend lists no corporate action for this listing.",
      }}
    >
      {(page) => (
        <>
          <ActionRows actions={page.items} />
          <CursorPager
            noun="actions"
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
  );
}
